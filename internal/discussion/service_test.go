package discussion_test

import (
	"errors"
	"image"
	"image/color"
	"image/draw"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"questionbook/internal/capture"
	"questionbook/internal/discussion"
	"questionbook/internal/library"
	"questionbook/internal/tags"
	"questionbook/internal/vlm"
)

// 全应用只有一条测试缝：服务层（spec）。这里从头到尾只调 discussion.Service 的方法，
// 不断言它内部调了谁几次，也不启动 Wails。
//
// **一次网络都不打**：接在讨论后面的是真的 vlm.Service，但它的 provider 换成了 vlm.Fake ——
// 于是配置、题图的读法、上传缓存这三样走的都是真货，只有「跟服务方说话」那一步是假的。
// 这也正是 vlm.Fake 待在非 _test.go 里的原因（它自己的注释里点名了「就题讨论」）。

// base 是测试的时间基准。
//
// 时钟钉死是刻意的：「用户那句与模型那句写在同一毫秒里」是排序要靠自增主键兜底的那一格，
// 用真实时钟就没法让它必然发生，那条兜底也就测不到。
var base = time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)

// clock 是一个不走的假时钟。
type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

// testConfig 是一份能过 Validate 的配置。
//
// 模型名是**编的**，而且刻意不像任何一个真的模型名：代码里不许出现硬编码的模型 ID
// （ADR-0005），测试里也不该出现。这里要验的是「配置里的名字确实原样传到了 provider 那一层」。
func testConfig() vlm.Config {
	return vlm.Config{
		BaseURL:     "https://vlm.invalid/v1",
		APIKey:      "test-credential-not-a-real-one",
		VisionModel: "vision-model-from-config",
		TextModel:   "text-model-from-config",
	}
}

// harness 是临时目录里搭出来的一整套，形状与 main.go 的接线一致：
// 库、题图目录、标签服务、VLM 服务（假 provider）、讨论服务，服务之间共用同一条库连接。
type harness struct {
	svc    *discussion.Service
	lib    *library.Store
	libSvc *library.Service
	tags   *tags.Service
	cards  *capture.Store
	fake   *vlm.Fake
	clock  *clock
	cfg    string

	// streamID 是这一套里问出去时带的流式号（h.ask 用它）。
	//
	// **默认给一个非空值**：于是这份测试跑的整条路都是**流式**那条 —— 假 provider 把
	// 这一轮该说的话当成一片播出去（见 vlm.Fake.ChatStream），返回值与非流式一字不差，
	// 所以下面那些断言一个字都不用改。想跑非流式就把它置空，两条路写进库里的东西是否
	// 一样由 TestStreamedAndPlainAskPersistTheSameThing 对着比。
	streamID string
}

// newHarness 搭一套，模型的回答取 replies（用完最后一条就一直重复它）。
func newHarness(t *testing.T, replies ...string) *harness {
	t.Helper()
	return newHarnessAt(t, filepath.Join(t.TempDir(), "library.db"), replies...)
}

// newHarnessAt 在指定的库文件上搭一套。
//
// 单独留一个入口是给「老库升上来」用的：那个库要先按老模式建好、关掉，再从这里重开 ——
// 不能每次都用一个新的临时目录。
func newHarnessAt(t *testing.T, dbPath string, replies ...string) *harness {
	t.Helper()
	root := filepath.Dir(dbPath)

	cards, err := capture.NewStore(filepath.Join(root, "cards"))
	if err != nil {
		t.Fatalf("开题图目录失败: %v", err)
	}
	db, err := library.Open(dbPath)
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	libSvc := library.NewService(db, cards)
	tagSvc := tags.NewService(db)

	cfgPath := filepath.Join(root, "vlm.json")
	if err := testConfig().Save(cfgPath); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}

	fake := vlm.NewFake(replies...)
	// 讨论那条接缝就是 vlm.Service.Ask，接法与 main.go 里一模一样，只是 provider 是假的。
	asker := vlm.NewService(libSvc, tagSvc, cfgPath, vlm.WithProvider(fake))

	clk := &clock{t: base}
	return &harness{
		svc:    discussion.NewService(db, asker, discussion.WithNow(clk.Now)),
		lib:    db,
		libSvc: libSvc,
		tags:   tagSvc,
		cards:  cards,
		fake:   fake,
		clock:  clk,
		cfg:    cfgPath,
		// 见 harness.streamID：默认就走流式那条路。
		streamID: "test-stream",
	}
}

// shaded 造一张纯色的小图，用来当题图 / 答案图。
func shaded(shade uint8) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	draw.Draw(img, img.Bounds(),
		image.NewUniform(color.RGBA{R: shade, G: 0x40, B: 0x80, A: 0xff}),
		image.Point{}, draw.Src)
	return img
}

// addQuestion 落一道带题图的错题。题图得真落在盘上 —— 每轮讨论都要把它读出来交给模型。
func (h *harness) addQuestion(t *testing.T, shade uint8) library.Question {
	t.Helper()

	hash, err := h.cards.Save(shaded(shade))
	if err != nil {
		t.Fatalf("落题图失败: %v", err)
	}
	q, err := h.lib.AddQuestion(library.Question{QuestionHash: hash.String()})
	if err != nil {
		t.Fatalf("AddQuestion: %v", err)
	}
	return q
}

// attachAnswer 给一道题补一张答案图（直接写 hash：补拍那条路要的是整条采集链路，
// 这里只是搭个场景，与 review 的测试直接用 AddQuestion 建题同一个道理）。
func (h *harness) attachAnswer(t *testing.T, id int64, shade uint8) string {
	t.Helper()

	hash, err := h.cards.Save(shaded(shade))
	if err != nil {
		t.Fatalf("落答案图失败: %v", err)
	}
	if _, err := h.lib.SetAnswerHash(id, hash.String()); err != nil {
		t.Fatalf("SetAnswerHash: %v", err)
	}
	return hash.String()
}

// history 取一道题的讨论记录，失败即终止测试。
func (h *harness) history(t *testing.T, id int64) []discussion.Message {
	t.Helper()

	msgs, err := h.svc.History(id)
	if err != nil {
		t.Fatalf("History(%d): %v", id, err)
	}
	return msgs
}

// ask 问一句，失败即终止测试。
func (h *harness) ask(t *testing.T, id int64, text string) discussion.Turn {
	t.Helper()

	turn, err := h.svc.Ask(id, text, h.streamID)
	if err != nil {
		t.Fatalf("Ask(%d, %q): %v", id, text, err)
	}
	return turn
}

// texts 把一段记录摊成「角色:正文」，断言起来比对着结构体字段清楚。
func texts(msgs []discussion.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, string(m.Role)+":"+m.Text)
	}
	return out
}

// ── 预设追问 ──

// 票据点名的三个必须在，而且至少三个。
func TestPresetsIncludeTheThreeFromTheTicket(t *testing.T) {
	h := newHarness(t)

	got := h.svc.Presets()
	if len(got) < 3 {
		t.Fatalf("预设追问只有 %d 个，想要至少 3 个：%v", len(got), got)
	}
	for _, want := range []string{"这步为什么", "还有别的解法吗", "我错在哪"} {
		if !slices.Contains(got, want) {
			t.Errorf("预设追问里没有 %q，实际是 %v", want, got)
		}
	}
}

// 拿到的是一份副本：界面上改不动服务里的那一份。
func TestPresetsReturnsACopy(t *testing.T) {
	h := newHarness(t)

	got := h.svc.Presets()
	got[0] = "改过的"

	if again := h.svc.Presets(); again[0] == "改过的" {
		t.Error("预设追问被改掉了：该返回副本")
	}
}

// ── 问一句 ──

// 一轮问答落两条：用户那句与模型那句，角色与正文都对得上。
func TestAskRecordsBothSides(t *testing.T) {
	h := newHarness(t, "这一步用了拉格朗日中值定理。")
	q := h.addQuestion(t, 0x10)

	const asked = "这步为什么"
	turn := h.ask(t, q.ID, asked)

	if turn.Question.Role != discussion.RoleUser || turn.Question.Text != asked {
		t.Errorf("用户那条 = %+v，想要 user / %q", turn.Question, asked)
	}
	if turn.Reply.Role != discussion.RoleAssistant || turn.Reply.Text != "这一步用了拉格朗日中值定理。" {
		t.Errorf("模型那条 = %+v，想要 assistant 与它的回答", turn.Reply)
	}
	if turn.Question.ID == 0 || turn.Reply.ID == 0 {
		t.Errorf("返回值里的 id 是 0：%+v", turn)
	}
	if !turn.Question.CreatedAt.Equal(base) {
		t.Errorf("时刻 = %s，想要 %s", turn.Question.CreatedAt, base)
	}

	// 再读一次，确认真的落库了（而不是只活在返回值里）。
	got := texts(h.history(t, q.ID))
	want := []string{"user:" + asked, "assistant:这一步用了拉格朗日中值定理。"}
	if !slices.Equal(got, want) {
		t.Errorf("回看这道题 = %v，想要 %v", got, want)
	}
}

// 用户那句**先落库**：模型调用失败时，他打的字不能跟着丢（票据 33）。
func TestAskKeepsTheQuestionWhenTheModelFails(t *testing.T) {
	h := newHarness(t, "这一句用不上")
	h.fake.ChatErr = errors.New("网络不通")
	q := h.addQuestion(t, 0x11)

	if _, err := h.svc.Ask(q.ID, "我错在哪", h.streamID); err == nil {
		t.Fatal("想要一个错，拿到 nil")
	}

	got := texts(h.history(t, q.ID))
	if !slices.Equal(got, []string{"user:我错在哪"}) {
		t.Errorf("失败之后回看这道题 = %v，想要留下用户那句、没有模型那句", got)
	}
}

// 没配 VLM 时用户那句照样留下 —— 落库与「能不能调模型」无关。
func TestAskWithoutConfig(t *testing.T) {
	h := newHarness(t)
	q := h.addQuestion(t, 0x12)

	// 与 main.go 同一个形状，只是没配过：配置文件不在。
	unconfigured := vlm.NewService(h.libSvc, h.tags, filepath.Join(t.TempDir(), "vlm.json"))
	svc := discussion.NewService(h.lib, unconfigured)

	if _, err := svc.Ask(q.ID, "这步为什么", ""); !errors.Is(err, vlm.ErrNotConfigured) {
		t.Fatalf("err = %v，想要 ErrNotConfigured", err)
	}
	if got := texts(h.history(t, q.ID)); !slices.Equal(got, []string{"user:这步为什么"}) {
		t.Errorf("回看这道题 = %v，想要留下用户那句", got)
	}
}

// 模型答了、但答的是空的：不算调用失败，但也不留一条空的回答。
func TestAskRejectsEmptyReply(t *testing.T) {
	h := newHarness(t, "   \n  ")
	q := h.addQuestion(t, 0x13)

	if _, err := h.svc.Ask(q.ID, "这步为什么", h.streamID); !errors.Is(err, discussion.ErrEmptyReply) {
		t.Fatalf("err = %v，想要 ErrEmptyReply", err)
	}
	if got := texts(h.history(t, q.ID)); !slices.Equal(got, []string{"user:这步为什么"}) {
		t.Errorf("回看这道题 = %v，想要只留下用户那句", got)
	}
}

// 空话发不出去，也不留记录，更不会去打扰模型。
func TestAskRejectsEmptyText(t *testing.T) {
	h := newHarness(t, "不该被问到")
	q := h.addQuestion(t, 0x14)

	for _, blank := range []string{"", "   ", "\n\t"} {
		if _, err := h.svc.Ask(q.ID, blank, h.streamID); !errors.Is(err, discussion.ErrEmptyMessage) {
			t.Errorf("Ask(%q) 返回 %v，想要 ErrEmptyMessage", blank, err)
		}
	}

	if got := h.history(t, q.ID); len(got) != 0 {
		t.Errorf("空话留下了 %v，想要什么都没有", texts(got))
	}
	if n := h.fake.ChatCount(); n != 0 {
		t.Errorf("空话也让模型跑了 %d 次", n)
	}
}

// 题不存在时明确报错，什么都不落。
func TestAskUnknownQuestion(t *testing.T) {
	h := newHarness(t, "不该被问到")

	if _, err := h.svc.Ask(9999, "这步为什么", h.streamID); !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("err = %v，想要 library.ErrNotFound", err)
	}
	if n := h.fake.ChatCount(); n != 0 {
		t.Errorf("题不存在也让模型跑了 %d 次", n)
	}
}

// ── 回看 ──

// 记录挂在错题上：两道题的讨论不会串。
func TestHistoryIsPerQuestion(t *testing.T) {
	h := newHarness(t, "答一", "答二")
	a := h.addQuestion(t, 0x20)
	b := h.addQuestion(t, 0x21)

	h.ask(t, a.ID, "题目 A 的问题")
	h.ask(t, b.ID, "题目 B 的问题")

	if got, want := texts(h.history(t, a.ID)), []string{"user:题目 A 的问题", "assistant:答一"}; !slices.Equal(got, want) {
		t.Errorf("A 的讨论 = %v，想要 %v", got, want)
	}
	if got, want := texts(h.history(t, b.ID)), []string{"user:题目 B 的问题", "assistant:答二"}; !slices.Equal(got, want) {
		t.Errorf("B 的讨论 = %v，想要 %v", got, want)
	}
}

// 同一毫秒里写下的几条，先后由自增主键定 —— 时间戳在这一格上说明不了任何事。
func TestHistoryOrdersSameMillisecondByID(t *testing.T) {
	h := newHarness(t, "第一次回答", "第二次回答")
	q := h.addQuestion(t, 0x22)

	h.ask(t, q.ID, "第一问")
	h.ask(t, q.ID, "第二问")

	got := h.history(t, q.ID)
	want := []string{"user:第一问", "assistant:第一次回答", "user:第二问", "assistant:第二次回答"}
	if !slices.Equal(texts(got), want) {
		t.Fatalf("回看这道题 = %v，想要 %v", texts(got), want)
	}

	// 四条的时刻一模一样 —— 这正是要验的那一格：顺序只可能来自 id。
	for i, m := range got {
		if !m.CreatedAt.Equal(base) {
			t.Errorf("第 %d 条的时刻 = %s，想要 %s（时钟是钉死的）", i, m.CreatedAt, base)
		}
		if i > 0 && m.ID <= got[i-1].ID {
			t.Errorf("第 %d 条的 id = %d，不比前一条 %d 大：id 该是递增的", i, m.ID, got[i-1].ID)
		}
	}
}

// 没有讨论过的题回空切片而不是 nil：前端要拿到 []，不是 null。
// 题不存在也一样 —— 这是个读，拿一个刚被删掉的 id 来问是正常的。
func TestHistoryIsEmptyNotNil(t *testing.T) {
	h := newHarness(t)
	q := h.addQuestion(t, 0x23)

	for _, id := range []int64{q.ID, 9999} {
		got := h.history(t, id)
		if got == nil {
			t.Errorf("History(%d) 是 nil，前端要拿到 [] 不是 null", id)
		}
		if len(got) != 0 {
			t.Errorf("History(%d) = %v，想要空", id, texts(got))
		}
	}
}

// ── 交给模型的那份对话 ──

// 每一轮都带**整段**对话（模型得记得自己说过什么），而且题图每次都跟着去。
func TestAskSendsTheWholeConversationWithTheQuestionImage(t *testing.T) {
	h := newHarness(t, "答一", "答二")
	q := h.addQuestion(t, 0x30)

	h.ask(t, q.ID, "第一问")
	h.ask(t, q.ID, "第二问")

	if len(h.fake.Chats) != 2 {
		t.Fatalf("对话了 %d 次，想要 2 次", len(h.fake.Chats))
	}
	for i, req := range h.fake.Chats {
		if req.Model != testConfig().VisionModel {
			t.Errorf("第 %d 次的模型 = %q，想要配置里那个 %q", i+1, req.Model, testConfig().VisionModel)
		}
		if req.System == "" {
			t.Errorf("第 %d 次没有系统提示词", i+1)
		}
	}

	second := h.fake.Chats[1]
	want := []string{"user:第一问", "assistant:答一", "user:第二问"}
	got := make([]string, 0, len(second.Messages))
	for _, m := range second.Messages {
		got = append(got, string(m.Role)+":"+m.Text)
	}
	if !slices.Equal(got, want) {
		t.Errorf("第二轮发出去的对话 = %v，想要 %v", got, want)
	}

	// 图挂在最后一条 user 消息上（这次要问的那句），一次请求里只带一份。
	for i, req := range second.Messages[:len(second.Messages)-1] {
		if len(req.Images) != 0 {
			t.Errorf("第 %d 条消息上挂了 %d 张图，图该只挂在最后一条 user 消息上", i, len(req.Images))
		}
	}
	last := second.Messages[len(second.Messages)-1]
	if len(last.Images) != 1 || last.Images[0].Hash != q.QuestionHash {
		t.Fatalf("最后一条消息上的图 = %+v，想要题图那一张", last.Images)
	}
	// 图送到了，而且是走引用（上传缓存那一格）—— 内联的字节也可以，这里不挑传法，
	// 但两份缓存必须共用：见下面 UploadCount 的断言。
	if last.Images[0].FileID == "" && len(last.Images[0].Data) == 0 {
		t.Error("题图既没有引用也没有字节：图根本没送到")
	}
}

// 同一张题图只上传一次 —— 讨论内部不重传，与打标签那条路也共用同一份缓存。
// 这是票据的硬要求，也是 vlm 那条接缝开在那边（而不是在讨论包里）的全部理由。
func TestDiscussionSharesTheUploadCache(t *testing.T) {
	h := newHarness(t, "答一", "答二", `{"tags":[{"subject":"数学","chapter":"","point":""}]}`)
	q := h.addQuestion(t, 0x31)

	h.ask(t, q.ID, "第一问")
	h.ask(t, q.ID, "第二问")
	if got := h.fake.UploadCount(); got != 1 {
		t.Fatalf("两轮讨论上传了 %d 次，想要 1 次：同一张图不该反复上传", got)
	}

	// 打标签那条路读的是同一张题图（同一份缓存），它不该再传一遍。
	if _, err := vlm.NewService(h.libSvc, h.tags, h.cfg, vlm.WithProvider(h.fake)).SuggestTags(q.ID); err != nil {
		t.Fatalf("SuggestTags: %v", err)
	}
	if got := h.fake.UploadCount(); got != 1 {
		t.Errorf("打标签之后上传数变成 %d，想要还是 1：两条路该共用同一份缓存", got)
	}
}

// 有答案图就一起带上 —— 「我错在哪」没有答案图基本答不了。
func TestAskAlsoSendsTheAnswerImage(t *testing.T) {
	h := newHarness(t, "答一")
	q := h.addQuestion(t, 0x32)
	answerHash := h.attachAnswer(t, q.ID, 0x33)

	h.ask(t, q.ID, "我错在哪")

	last := h.fake.Chats[0].Messages[0]
	if len(last.Images) != 2 {
		t.Fatalf("带上了 %d 张图，想要题图 + 答案图", len(last.Images))
	}
	if last.Images[0].Hash != q.QuestionHash || last.Images[1].Hash != answerHash {
		t.Errorf("带上的图 = %q / %q，想要 %q / %q",
			last.Images[0].Hash, last.Images[1].Hash, q.QuestionHash, answerHash)
	}
	if got := h.fake.UploadCount(); got != 2 {
		t.Errorf("上传了 %d 次，想要 2 次（两张不同的图各一次）", got)
	}
}

// ── 与错题同生共死 ──

// 删掉错题，它的讨论一并带走 —— 不留指向不存在错题的行。
// （外键 cascade 做的，dsn 里开着 foreign_keys，这条测试钉住那件事。）
func TestDeleteQuestionTakesDiscussion(t *testing.T) {
	h := newHarness(t, "答一")
	q := h.addQuestion(t, 0x40)
	h.ask(t, q.ID, "这步为什么")

	if _, err := h.lib.DeleteQuestion(q.ID); err != nil {
		t.Fatalf("DeleteQuestion: %v", err)
	}

	var n int
	if err := h.lib.DB().QueryRow(
		`SELECT count(*) FROM discussions WHERE question_id = ?`, q.ID,
	).Scan(&n); err != nil {
		t.Fatalf("数讨论记录的残留: %v", err)
	}
	if n != 0 {
		t.Errorf("删题之后 discussions 里还剩 %d 行", n)
	}
}

// 老库（票据 09 时期的形状：模式版本停在 3）升上来：讨论表建出来，
// 原来的错题一条不少，而且立刻就能聊。
func TestUpgradeFromOlderSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "library.db")

	old, err := library.Open(dbPath)
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	// 老库里就有的题（没有题图文件也不要紧：这一条只验它还在、新表能用）。
	older, err := old.AddQuestion(library.Question{QuestionHash: "sha256:老库里就有的题"})
	if err != nil {
		t.Fatalf("AddQuestion: %v", err)
	}

	// 把库退回票据 09 的形状：讨论那两张表还没建、版本号停在 3。
	// 这是测试在**扮演**一个老库文件，不是绕过服务层的实现细节。
	// 要删的表从迁移表推出来，不手抄（见 tags 那边同一条规矩的说明）。
	// 这条原先只删了 discussions 自己 —— 于是迁移 5 的表留在库里，重开时撞车。
	for _, tbl := range library.TablesIntroducedAfter(3) {
		if _, err := old.DB().Exec(`DROP TABLE ` + tbl); err != nil {
			t.Fatalf("退回老模式 (DROP TABLE %s): %v", tbl, err)
		}
	}
	if _, err := old.DB().Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatalf("退回老模式 (PRAGMA user_version = 3): %v", err)
	}
	if err := old.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	h := newHarnessAt(t, dbPath, "升级之后也能聊")

	// 老题还在。
	if _, err := h.lib.GetQuestion(older.ID); err != nil {
		t.Fatalf("升级之后老题不见了: %v", err)
	}
	// 新表能用：新题问一句，两个方向都落下来。
	q := h.addQuestion(t, 0x50)
	h.ask(t, q.ID, "这步为什么")
	if got, want := texts(h.history(t, q.ID)), []string{"user:这步为什么", "assistant:升级之后也能聊"}; !slices.Equal(got, want) {
		t.Errorf("升级之后回看这道题 = %v，想要 %v", got, want)
	}
}
