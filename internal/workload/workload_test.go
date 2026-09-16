package workload_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"questionbook/internal/capture"
	"questionbook/internal/library"
	"questionbook/internal/review"
	"questionbook/internal/tags"
	"questionbook/internal/vlm"
	"questionbook/internal/workload"
)

// 全应用只有一条测试缝：服务层（spec）。这里从头到尾只调 workload.Service 的方法，
// 不断言它内部调了谁几次，也不启动 Wails。
//
// **一次网络都不打**：接在这条缝后面的是真的 vlm.Service，但它的 provider 换成了
// vlm.Fake —— 于是配置、模型名的选法、报文怎么拼走的都是真货，只有「跟服务方说话」
// 那一步是假的。这也正是 vlm.Fake 待在非 _test.go 里的原因（它的注释里点名了
// 「每日推荐做几道」）。

// zone / base 与 review 那边的测试同一个约定：时区钉死 UTC+8，时间基准钉死 2026-09-16 10:00。
//
// 「今天到期」是日期相关的判定，而「最近几天」是一个时间窗 —— 两处都得靠假时钟钉住，
// 否则同一份断言在不同时区的机器上会给出不同的结果。
var (
	zone = time.FixedZone("UTC+8", 8*60*60)
	base = time.Date(2026, 9, 16, 10, 0, 0, 0, zone)
)

// clock 是一个可以拨的假时钟。
//
// 订阅它的两个服务（review 与 workload）用的是同一个：「队列是谁算的」与「窗口从哪一刻
// 起算」必须是同一个「现在」，否则断言出来的就不是同一件事。
type clock struct{ t time.Time }

func (c *clock) Now() time.Time   { return c.t }
func (c *clock) Set(at time.Time) { c.t = at.In(zone) }

// testConfig 是一份能过 Validate 的配置。
//
// 模型名是**编的**，而且刻意不像任何一个真的模型名：代码里不许出现硬编码的模型 ID
// （ADR-0005），测试里也不该出现。这里要验的是「文本那一路确实传到了 provider 那一层」。
func testConfig() vlm.Config {
	return vlm.Config{
		BaseURL:     "https://vlm.invalid/v1",
		APIKey:      "test-credential-not-a-real-one",
		VisionModel: "vision-model-from-config",
		TextModel:   "text-model-from-config",
	}
}

// harness 是临时目录里搭出来的一整套，形状与 main.go 的接线一致：
// 库、标签服务、复习服务、VLM 服务、推荐服务，服务之间共用同一条库连接与同一个时钟。
type harness struct {
	svc    *workload.Service
	review *review.Service
	lib    *library.Store
	fake   *vlm.Fake // nil 表示这次没注入假 provider（走的是按配置现建那条真路）
	clock  *clock
}

// newHarness 搭一套：配置用 testConfig()，provider 是假的，模型依次回 replies。
func newHarness(t *testing.T, replies ...string) *harness {
	t.Helper()
	return newHarnessCfg(t, testConfig(), vlm.NewFake(replies...))
}

// newHarnessCfg 用给定的一份配置与一个 provider 搭一套。
//
// provider 传 nil 表示**不注入**：那时 vlm 那一侧按配置现建 provider —— 配置是空的
// 就会得到 ErrNotConfigured，那正是「还没配 VLM」这条路。
func newHarnessCfg(t *testing.T, cfg vlm.Config, provider vlm.Provider) *harness {
	t.Helper()

	root := t.TempDir()
	cards, err := capture.NewStore(filepath.Join(root, "cards"))
	if err != nil {
		t.Fatalf("开题图目录失败: %v", err)
	}
	lib, err := library.Open(filepath.Join(root, "library.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	t.Cleanup(func() { lib.Close() })

	cfgPath := filepath.Join(root, "vlm.json")
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}

	clk := &clock{t: base}
	libSvc := library.NewService(lib, cards)
	tagSvc := tags.NewService(lib)

	var (
		opts []vlm.Option
		fake *vlm.Fake
	)
	if provider != nil {
		opts = append(opts, vlm.WithProvider(provider))
	}
	if f, ok := provider.(*vlm.Fake); ok {
		fake = f
	}
	asker := vlm.NewService(libSvc, tagSvc, cfgPath, opts...)

	// 复习服务只有一份，与 main.go 一样：推荐要基于的正是复习页看到的那条队列。
	reviewSvc := review.NewService(lib, filepath.Join(t.TempDir(), "review.json"), review.WithNow(clk.Now))

	return &harness{
		svc:    workload.NewService(reviewSvc, lib, asker, workload.WithNow(clk.Now)),
		review: reviewSvc,
		lib:    lib,
		fake:   fake,
		clock:  clk,
	}
}

// addQuestion 落一道错题，创建时间显式给 —— 没复习过的题的到期时刻就是它的创建时间，
// 不钉住创建时间就断言不了「积压了几天」。
func (h *harness) addQuestion(t *testing.T, name string, at time.Time) library.Question {
	t.Helper()

	q, err := h.lib.AddQuestion(library.Question{QuestionHash: "sha256:" + name, CreatedAt: at})
	if err != nil {
		t.Fatalf("AddQuestion(%q): %v", name, err)
	}
	return q
}

// gradeAt 把时钟拨到某个时刻，评一道题，返回 FSRS 算出来的结果。
func (h *harness) gradeAt(t *testing.T, id int64, r review.Rating, at time.Time) review.ReviewResult {
	t.Helper()

	h.clock.Set(at)
	res, err := h.review.Grade(id, r)
	if err != nil {
		t.Fatalf("Grade(%d, %v): %v", id, r, err)
	}
	return res
}

// queue 取当前队列，失败即终止测试。
func (h *harness) queue(t *testing.T) []review.QueueItem {
	t.Helper()

	items, err := h.review.Queue()
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	return items
}

// snap 把队列拍成一张「id + 到期时刻」的快照，用来断言两次取到的队列逐字相同。
func snap(items []review.QueueItem) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, fmt.Sprintf("%d@%d", item.Question.ID, item.DueAt.UnixMilli()))
	}
	return out
}

// queuedIDs 把队列摊成 id，方便按序列断言。
func queuedIDs(items []review.QueueItem) []int64 {
	out := make([]int64, 0, len(items))
	for _, item := range items {
		out = append(out, item.Question.ID)
	}
	return out
}

// countRows 数一张表里有多少行，用来断言「推荐一个字节都没写」。
func countRows(t *testing.T, lib *library.Store, query string) int {
	t.Helper()

	var n int
	if err := lib.DB().QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// 推荐要看着「还剩多少道」与「最近表现」给出一个数 —— 而且喂给模型的必须是真数。
//
// 这条测试同时钉住两件事：返回的那一条，与**发出去的那句话**。
// 后者是这一层的全部输入，它错了前者的对错就无从谈起。
func TestRecommendUsesDueCountAndRecentPerformance(t *testing.T) {
	h := newHarness(t, `{"suggest":2,"reason":"  最近忘得有点多  "}`)

	// 八道题都是三天前拍的。
	ids := make([]int64, 0, 8)
	for i := range 8 {
		ids = append(ids, h.addQuestion(t, fmt.Sprintf("题%d", i), base.AddDate(0, 0, -3)).ID)
	}
	// 在「现在」评掉三道：它们会被推到明天之后，因此不在今天的队列里；
	// 但它们是「最近表现」里唯一的数据。
	h.gradeAt(t, ids[0], review.Again, base)
	h.gradeAt(t, ids[1], review.Hard, base)
	h.gradeAt(t, ids[2], review.Good, base)
	h.clock.Set(base)

	if got, want := queuedIDs(h.queue(t)), ids[3:]; !slices.Equal(got, want) {
		t.Fatalf("队列 = %v，想要 %v —— 评过的三道不该回来", got, want)
	}

	rec, err := h.svc.Recommend()
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if rec.Suggest != 2 || rec.Due != 5 || rec.Reason != "最近忘得有点多" {
		t.Errorf("推荐 = %+v，想要 Suggest 2 / Due 5 / Reason「最近忘得有点多」", rec)
	}

	// 模型看到的那句话，逐条对：到期量、新题量、积压深度、最近的表现。
	if n := h.fake.ChatCount(); n != 1 {
		t.Fatalf("问了模型 %d 次，想要 1 次", n)
	}
	req := h.fake.Chats[0]
	body := req.Messages[0].Text
	for _, want := range []string{
		"队列里现在还有：5 道",
		"其中从没复习过的：5 道",
		"已经逾期 3 天",
		// 三次复习都落在窗口里，都在今天这一天。
		"最近 7 天：复习 3 次，涉及 3 道题，其中 1 天有复习",
		"最近 7 天的评级：Again 1 / Hard 1 / Good 1 / Easy 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("喂给模型的这段话里没有 %q：\n%s", want, body)
		}
	}

	// 走的是**文本**模型那一路（推荐不带图），而且要求了 JSON。
	if req.Model != "text-model-from-config" {
		t.Errorf("这次用的模型是 %q，想要配置里的文本模型", req.Model)
	}
	if !req.JSON {
		t.Error("没有要求 JSON 回答")
	}
	// 推荐这条路没有图可传：一条 user 消息，里面一张图都没有。
	if len(req.Messages) != 1 || len(req.Messages[0].Images) != 0 {
		t.Errorf("这次请求带了图：%+v", req.Messages)
	}
	// 提示词里那句「必须在 1 到队列长度之间」是接口契约的一部分，代码靠它才敢夹。
	if !strings.Contains(req.System, "1 到") {
		t.Errorf("系统提示词里没有那条上下界约束：\n%s", req.System)
	}
}

// 最近一次都没复习过时，这件事要说出口 —— 它是「今天少做点」最硬的依据。
// 同时钉住另一头：没积压时不提「逾期」。
func TestRecommendSaysWhenNothingWasReviewed(t *testing.T) {
	h := newHarness(t, `{"suggest":1,"reason":""}`)

	// 今天早上刚拍的两道：逾期 0 天，也从没复习过。
	h.addQuestion(t, "题一", base.Add(-time.Hour))
	h.addQuestion(t, "题二", base.Add(-time.Hour))

	if _, err := h.svc.Recommend(); err != nil {
		t.Fatalf("Recommend: %v", err)
	}

	body := h.fake.Chats[0].Messages[0].Text
	if !strings.Contains(body, "最近 7 天：一次都没复习过") {
		t.Errorf("没复习过这件事没说出来：\n%s", body)
	}
	if strings.Contains(body, "逾期") {
		t.Errorf("今天才到期的题被说成了逾期：\n%s", body)
	}
	if !strings.Contains(body, "其中从没复习过的：2 道") {
		t.Errorf("新题的道数不对：\n%s", body)
	}
}

// 模型给的数越界时夹回 [1, 队列长度]：方向清楚就留着用，别为一个越界数把整条推荐丢掉。
func TestRecommendClampsOutOfRange(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		want  int
	}{
		{"给多了夹到队列长度", `{"suggest":500,"reason":""}`, 3},
		{"给 0 也至少 1", `{"suggest":0,"reason":""}`, 1},
		{"负数同样至少 1", `{"suggest":-4,"reason":""}`, 1},
		{"小数四舍五入", `{"suggest":2.4,"reason":"先做两道"}`, 2},
		// 数组被引号包起来是模型常干的事，json.Number 照单全收 —— 这是白捡的容错。
		{"数字被加了引号也认", `{"suggest":"2","reason":""}`, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, c.reply)
			for i := range 3 {
				h.addQuestion(t, fmt.Sprintf("题%d", i), base.Add(-time.Hour))
			}

			rec, err := h.svc.Recommend()
			if err != nil {
				t.Fatalf("Recommend: %v", err)
			}
			if rec.Suggest != c.want {
				t.Errorf("Suggest = %d，想要 %d", rec.Suggest, c.want)
			}
			if rec.Due != 3 {
				t.Errorf("Due = %d，想要 3", rec.Due)
			}
		})
	}
}

// 答得不能用是**明确失败**，不是一个凑合的数：界面上要能据此静默退化。
//
// 顺带钉住两条：没有 JSON 的散文、根本没有那个键。
func TestRecommendRejectsUnusableAnswer(t *testing.T) {
	cases := []struct {
		name  string
		reply string
	}{
		{"一段散文", "今天先做 20 道吧。"},
		{"没有 suggest 这个键", `{"reason":"我算不出来"}`},
		{"键在但值是个词", `{"suggest":"不少","reason":""}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, c.reply)
			h.addQuestion(t, "题一", base.Add(-time.Hour))

			_, err := h.svc.Recommend()
			if !errors.Is(err, workload.ErrUnusableAnswer) {
				t.Fatalf("返回 %v，想要 ErrUnusableAnswer", err)
			}
			// 拿不到推荐不影响队列：它照旧是那一道。
			if got := len(h.queue(t)); got != 1 {
				t.Errorf("拿不到推荐时队列里有 %d 道", got)
			}
		})
	}
}

// 没配 VLM 是一个**正常状态**：拍摄、入库、复习都照常，只有这一条推荐没有。
func TestRecommendWithoutConfig(t *testing.T) {
	// 写下去的那份配置里什么都没有（零值 Config）—— Validate 会明确报「还没配置」。
	h := newHarnessCfg(t, vlm.Config{}, nil)
	h.addQuestion(t, "题一", base.Add(-time.Hour))

	_, err := h.svc.Recommend()
	if !errors.Is(err, vlm.ErrNotConfigured) {
		t.Fatalf("返回 %v，想要 vlm.ErrNotConfigured", err)
	}
	if got := len(h.queue(t)); got != 1 {
		t.Errorf("没配 VLM 时队列里有 %d 道", got)
	}
}

// 调不通（断网、401、超时）同样是静默退化：错照报，队列不受影响。
func TestRecommendWhenProviderFails(t *testing.T) {
	h := newHarness(t)
	h.fake.ChatErr = errors.New("网络断了")
	h.addQuestion(t, "题一", base.Add(-time.Hour))

	_, err := h.svc.Recommend()
	if err == nil || !strings.Contains(err.Error(), "网络断了") {
		t.Fatalf("返回 %v，想要 provider 报上来的那个错", err)
	}
	if got := len(h.queue(t)); got != 1 {
		t.Errorf("调不通时队列里有 %d 道", got)
	}
}

// 队列空的时候不去问模型：没有可推荐的东西，发一次请求既是白花钱，
// 也会让「今天没有要复习的题」那一屏莫名其妙地等一会儿。
func TestRecommendOnEmptyQueueSkipsTheModel(t *testing.T) {
	h := newHarness(t, `{"suggest":3,"reason":"随便给一个"}`)

	rec, err := h.svc.Recommend()
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if rec.Suggest != 0 || rec.Due != 0 {
		t.Errorf("空队列上的推荐 = %+v，想要 Suggest 0 / Due 0", rec)
	}
	if n := h.fake.ChatCount(); n != 0 {
		t.Errorf("空队列也问了模型 %d 次", n)
	}
}

// 没有配文本模型时回落到视觉模型，而不是发一个空模型名出去（服务方一律 400）。
func TestRecommendFallsBackToVisionModel(t *testing.T) {
	cfg := testConfig()
	cfg.TextModel = ""
	h := newHarnessCfg(t, cfg, vlm.NewFake(`{"suggest":1,"reason":""}`))
	h.addQuestion(t, "题一", base.Add(-time.Hour))

	if _, err := h.svc.Recommend(); err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if got := h.fake.Chats[0].Model; got != "vision-model-from-config" {
		t.Errorf("这次用的模型是 %q，想要回落到配置里的视觉模型", got)
	}
}

// 这一票最要紧的一条：**不采纳推荐时，一切回落到纯 FSRS**。
//
// 推荐是加在 FSRS 之上的一道闸，闸要摘得掉 —— 而它之所以摘得掉，是因为它在代码上
// 什么都没改：队列逐字不变，复习状态与复习记录一行没多，评级算出来的下次到期与
// 「从没听说过推荐」的那一道逐字相同。
func TestNotTakingTheRecommendationFallsBackToPlainFSRS(t *testing.T) {
	h := newHarness(t, `{"suggest":1,"reason":"先做一道试试"}`)

	// 两道**完全一样**的题（同一时刻创建、都还没复习过）：一道在推荐之前评，
	// 一道在推荐之后评。两者之间唯一的差别就是中间发生过一次推荐。
	control := h.addQuestion(t, "对照", base.AddDate(0, 0, -3))
	subject := h.addQuestion(t, "本体", base.AddDate(0, 0, -3))
	h.addQuestion(t, "陪跑", base.AddDate(0, 0, -3))

	resControl := h.gradeAt(t, control.ID, review.Good, base)

	// 推荐之前库里的样子：那次对照评级留下的两行，就是此刻全部的内容。
	logsBefore := countRows(t, h.lib, `SELECT count(*) FROM review_logs`)
	statesBefore := countRows(t, h.lib, `SELECT count(*) FROM review_states`)

	before := snap(h.queue(t))
	rec, err := h.svc.Recommend()
	if err != nil {
		t.Fatalf("Recommend: %v", err)
	}
	if rec.Suggest != 1 {
		t.Fatalf("建议 = %d 道，想要 1 道", rec.Suggest)
	}

	// 用户**不采纳**：他接着按原来的方式做题，也就是什么都没发生。
	after := snap(h.queue(t))
	if !slices.Equal(before, after) {
		t.Errorf("推荐改动过队列：\n之前 %v\n之后 %v", before, after)
	}
	if !slices.Contains(queuedIDs(h.queue(t)), subject.ID) {
		t.Error("待评的那道题从队列里消失了 —— 闸不该挪动任何一道题")
	}

	// 一个字节都没写：状态与记录都还是推荐之前那个数。
	if n := countRows(t, h.lib, `SELECT count(*) FROM review_states`); n != statesBefore {
		t.Errorf("推荐之后复习状态有 %d 行，之前是 %d 行", n, statesBefore)
	}
	if n := countRows(t, h.lib, `SELECT count(*) FROM review_logs`); n != logsBefore {
		t.Errorf("推荐之后复习记录有 %d 行，之前是 %d 行", n, logsBefore)
	}

	// 评同一档，两者的下次到期逐字相同 —— 推荐在手时算出来的仍然是纯 FSRS 的那个数。
	resSubject := h.gradeAt(t, subject.ID, review.Good, base)
	if !resSubject.DueAt.Equal(resControl.DueAt) || resSubject.ScheduledDays != resControl.ScheduledDays {
		t.Errorf("推荐在手时评 Good 推到 %s（%d 天），没有推荐时是 %s（%d 天）",
			resSubject.DueAt, resSubject.ScheduledDays, resControl.DueAt, resControl.ScheduledDays)
	}
}
