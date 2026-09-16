package vlm_test

import (
	"errors"
	"image"
	"image/color"
	"image/draw"
	"path/filepath"
	"strings"
	"testing"

	"questionbook/internal/capture"
	"questionbook/internal/library"
	"questionbook/internal/tags"
	"questionbook/internal/vlm"
)

// 全应用只有一条测试缝：服务层（spec）。这里从头到尾只调 vlm.Service 的方法，
// 不断言它内部调了谁几次，也不启动 Wails。
//
// **一次网络都不打**（票据的硬要求）：网络那一侧整个换成 vlm.Fake，
// 真实 provider（DeepSeek）的代码写好了但不在这里测 —— 真要验它得有 key 与真机，
// 那是用户那边的事。

// testConfig 是一份能过 Validate 的配置。
//
// 模型名是**编的**，而且刻意不像任何一个真的模型名：代码里不许出现硬编码的模型 ID，
// 测试里也不该出现 —— 那正是 provider 抽象要挡住的东西。这里要验的是
// 「配置里的名字确实原样传到了 provider 那一层」。
func testConfig() vlm.Config {
	return vlm.Config{
		BaseURL:     "https://vlm.invalid/v1",
		APIKey:      "test-credential-not-a-real-one",
		VisionModel: "vision-model-from-config",
		TextModel:   "text-model-from-config",
	}
}

// harness 是临时目录里搭出来的一整套：库 + 题图目录 + 标签服务 + VLM 服务，
// 形状与 main.go 的接线一致（服务之间共用同一条库连接，VLM 再接一层 provider）。
type harness struct {
	svc     *vlm.Service
	lib     *library.Service
	tags    *tags.Service
	cards   *capture.Store
	fake    *vlm.Fake
	cfgPath string
}

// newHarness 搭一套，provider 用假实现，回答取 replies（用完最后一条就一直重复它）。
func newHarness(t *testing.T, replies ...string) *harness {
	t.Helper()
	root := t.TempDir()

	cards, err := capture.NewStore(filepath.Join(root, "cards"))
	if err != nil {
		t.Fatalf("开题图目录失败: %v", err)
	}
	db, err := library.Open(filepath.Join(root, "library.db"))
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
	return &harness{
		svc:     vlm.NewService(libSvc, tagSvc, cfgPath, vlm.WithProvider(fake)),
		lib:     libSvc,
		tags:    tagSvc,
		cards:   cards,
		fake:    fake,
		cfgPath: cfgPath,
	}
}

// addQuestion 落一道带题图的错题，返回它。题图得先真落在盘上 ——
// 读题图那条路要能读到一个真文件。
func (h *harness) addQuestion(t *testing.T, shade uint8) library.Question {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	draw.Draw(img, img.Bounds(),
		image.NewUniform(color.RGBA{R: shade, G: 0x40, B: 0x80, A: 0xff}),
		image.Point{}, draw.Src)
	hash, err := h.cards.Save(img)
	if err != nil {
		t.Fatalf("落题图失败: %v", err)
	}

	q, err := h.lib.Add(hash.String(), "")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	return q
}

// tagsOf 取一道题挂着的标签。
func (h *harness) tagsOf(t *testing.T, questionID int64) []tags.Tag {
	t.Helper()

	ts, err := h.tags.TagsOfQuestion(questionID)
	if err != nil {
		t.Fatalf("TagsOfQuestion(%d): %v", questionID, err)
	}
	return ts
}

// allTags 取全部标签。
func (h *harness) allTags(t *testing.T) []tags.Tag {
	t.Helper()

	ts, err := h.tags.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return ts
}

// names 把一组标签压成「层级:名字」的集合，断言起来比对着结构体字段清楚。
func names(ts []tags.Tag) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Level.String()+":"+t.Name)
	}
	return out
}

// hasName 报告一组标签里有没有某个层级的某个名字。
func hasName(ts []tags.Tag, level tags.Level, name string) bool {
	for _, t := range ts {
		if t.Level == level && t.Name == name {
			return true
		}
	}
	return false
}

// ── 建议：只读 ──

// SuggestTags 一个字节都不该写库 —— 票据要求建议先给用户改，保存之后才生效。
func TestSuggestTagsWritesNothing(t *testing.T) {
	h := newHarness(t, `{"tags":[{"subject":"数学","chapter":"高等数学","point":"中值定理"}]}`)
	q := h.addQuestion(t, 0x10)

	before := len(h.allTags(t))

	res, err := h.svc.SuggestTags(q.ID)
	if err != nil {
		t.Fatalf("SuggestTags: %v", err)
	}

	if len(res.Tags) != 1 {
		t.Fatalf("建议条数 = %d，想要 1：%+v", len(res.Tags), res.Tags)
	}
	if res.Problem != "" {
		t.Errorf("Problem = %q，想要空", res.Problem)
	}

	// 没建标签。
	if after := len(h.allTags(t)); after != before {
		t.Errorf("标签数从 %d 变成了 %d，识别不该建标签", before, after)
	}
	// 没挂标签。
	if got := h.tagsOf(t, q.ID); len(got) != 0 {
		t.Errorf("题上多了标签 %v，识别不该挂标签", names(got))
	}
}

// 一条建议原样传回来：三层都在、顺序不变。
func TestSuggestTagsReturnsThreeLevels(t *testing.T) {
	h := newHarness(t, `{"tags":[{"subject":"数学","chapter":"高等数学","point":"中值定理"}]}`)
	q := h.addQuestion(t, 0x11)

	res, err := h.svc.SuggestTags(q.ID)
	if err != nil {
		t.Fatalf("SuggestTags: %v", err)
	}

	got := res.Tags[0]
	if got.Subject != "数学" || got.Chapter != "高等数学" || got.Point != "中值定理" {
		t.Errorf("建议 = %+v，想要 数学 / 高等数学 / 中值定理", got)
	}
	if res.Raw == "" {
		t.Error("Raw 是空的：模型的原话要跟着一起回来，票据 01 调提示词全靠它")
	}
}

// 模型偶尔会拿代码块包一层、或者前面寒暄一句。这不该让整次识别失败。
func TestSuggestTagsSurvivesCodeFence(t *testing.T) {
	h := newHarness(t, "好的，这是标签：\n```json\n{\"tags\":[{\"subject\":\"英语\",\"chapter\":\"阅读\",\"point\":\"主旨题\"}]}\n```\n希望有用！")
	q := h.addQuestion(t, 0x12)

	res, err := h.svc.SuggestTags(q.ID)
	if err != nil {
		t.Fatalf("SuggestTags: %v", err)
	}
	if len(res.Tags) != 1 || res.Tags[0].Subject != "英语" {
		t.Errorf("建议 = %+v，想要一条 英语 / 阅读 / 主旨题", res.Tags)
	}
}

// 模型答得不成话：调用本身成功（error 为 nil），但结果不能用，
// 而且**原话要交出来** —— 那是调提示词时唯一的线索。
func TestSuggestTagsReportsUnusableReply(t *testing.T) {
	h := newHarness(t, "这道题我建议归到数学里。")
	q := h.addQuestion(t, 0x13)

	res, err := h.svc.SuggestTags(q.ID)
	if err != nil {
		t.Fatalf("SuggestTags 不该报错（调用是成功的）: %v", err)
	}
	if res.Problem == "" {
		t.Error("Problem 是空的：模型没按格式回，界面得知道")
	}
	if len(res.Tags) != 0 {
		t.Errorf("Tags = %+v，想要空", res.Tags)
	}
	if !strings.Contains(res.Raw, "建议归到数学") {
		t.Errorf("Raw = %q，想要模型的原话", res.Raw)
	}
}

// 模型答了、但一条可用的都没给：也算 Problem，不是解析失败。
func TestSuggestTagsReportsEmptySuggestion(t *testing.T) {
	h := newHarness(t, `{"tags":[]}`)
	q := h.addQuestion(t, 0x14)

	res, err := h.svc.SuggestTags(q.ID)
	if err != nil {
		t.Fatalf("SuggestTags: %v", err)
	}
	if res.Problem == "" {
		t.Error("Problem 是空的：模型一条都没给，界面得知道")
	}
	if len(res.Tags) != 0 {
		t.Errorf("Tags = %+v，想要空", res.Tags)
	}
}

// 没有学科的建议整条丢掉 —— 没有学科就不知道该挂到哪棵树下面。
func TestSuggestTagsDropsEntriesWithoutSubject(t *testing.T) {
	h := newHarness(t, `{"tags":[{"subject":"","chapter":"高等数学","point":"中值定理"},{"subject":"数学","chapter":"","point":""}]}`)
	q := h.addQuestion(t, 0x15)

	res, err := h.svc.SuggestTags(q.ID)
	if err != nil {
		t.Fatalf("SuggestTags: %v", err)
	}
	if len(res.Tags) != 1 {
		t.Fatalf("建议条数 = %d，想要 1：%+v", len(res.Tags), res.Tags)
	}
	if res.Tags[0].Subject != "数学" || res.Tags[0].Chapter != "" {
		t.Errorf("留下的那条 = %+v，想要只有学科的那条", res.Tags[0])
	}
	if res.Problem != "" {
		t.Errorf("Problem = %q，想要空（还有一条能用）", res.Problem)
	}
}

// 模型超量了就替它裁到上限：没有上限时它会一口气列七八条。
func TestSuggestTagsCapsAtLimit(t *testing.T) {
	h := newHarness(t, `{"tags":[
		{"subject":"数学","chapter":"","point":"a"},
		{"subject":"数学","chapter":"","point":"b"},
		{"subject":"数学","chapter":"","point":"c"},
		{"subject":"数学","chapter":"","point":"d"},
		{"subject":"数学","chapter":"","point":"e"}]}`)
	q := h.addQuestion(t, 0x16)

	res, err := h.svc.SuggestTags(q.ID)
	if err != nil {
		t.Fatalf("SuggestTags: %v", err)
	}
	if len(res.Tags) != 3 {
		t.Errorf("建议条数 = %d，想要 3（上限）", len(res.Tags))
	}
}

// ── 传图：同一张图不反复上传 ──

// 第一次识别把图交上去换引用，之后的请求用引用（票据：避免同一张图反复上传）。
func TestSuggestTagsUploadsOnceThenReuses(t *testing.T) {
	h := newHarness(t,
		`{"tags":[{"subject":"数学","chapter":"","point":""}]}`,
		`{"tags":[{"subject":"数学","chapter":"","point":""}]}`,
	)
	q := h.addQuestion(t, 0x20)

	for i := 0; i < 2; i++ {
		if _, err := h.svc.SuggestTags(q.ID); err != nil {
			t.Fatalf("第 %d 次 SuggestTags: %v", i+1, err)
		}
	}

	if got := h.fake.UploadCount(); got != 1 {
		t.Errorf("上传了 %d 次，想要 1 次：同一张图不该反复上传", got)
	}
	if len(h.fake.Chats) != 2 {
		t.Fatalf("对话了 %d 次，想要 2 次", len(h.fake.Chats))
	}
	// 两次都带着同一个引用，而不是内联的字节。
	for i, req := range h.fake.Chats {
		if len(req.Messages) != 1 || len(req.Messages[0].Images) != 1 {
			t.Fatalf("第 %d 次请求的消息形状不对: %+v", i+1, req.Messages)
		}
		img := req.Messages[0].Images[0]
		if img.FileID == "" {
			t.Errorf("第 %d 次请求没有带上传引用", i+1)
		}
		if req.Model != testConfig().VisionModel {
			t.Errorf("第 %d 次请求的模型 = %q，想要配置里那个 %q", i+1, req.Model, testConfig().VisionModel)
		}
	}
}

// 缓存是落盘的：换一个 Service（相当于重启一次应用）也不该重传。
func TestSuggestTagsReusesUploadAcrossRestart(t *testing.T) {
	h := newHarness(t, `{"tags":[{"subject":"数学","chapter":"","point":""}]}`)
	q := h.addQuestion(t, 0x21)

	if _, err := h.svc.SuggestTags(q.ID); err != nil {
		t.Fatalf("SuggestTags: %v", err)
	}

	// 同一个配置文件、同一个 provider，但换一个新的 Service 实例。
	restarted := vlm.NewService(h.lib, h.tags, h.cfgPath, vlm.WithProvider(h.fake))
	if _, err := restarted.SuggestTags(q.ID); err != nil {
		t.Fatalf("重启后 SuggestTags: %v", err)
	}

	if got := h.fake.UploadCount(); got != 1 {
		t.Errorf("上传了 %d 次，想要 1 次：缓存该落盘，重启不该重传", got)
	}
}

// 服务方没有文件接口（或这次没传上去）时回落到内联，而不是整件事失败。
func TestSuggestTagsFallsBackToInlineImage(t *testing.T) {
	h := newHarness(t, `{"tags":[{"subject":"数学","chapter":"","point":""}]}`)
	h.fake.UploadErr = vlm.ErrNoUpload
	q := h.addQuestion(t, 0x22)

	res, err := h.svc.SuggestTags(q.ID)
	if err != nil {
		t.Fatalf("SuggestTags: %v", err)
	}
	if len(res.Tags) != 1 {
		t.Errorf("建议 = %+v，想要一条：上传这条路不通不该让识别失败", res.Tags)
	}

	img := h.fake.Chats[0].Messages[0].Images[0]
	if img.FileID != "" {
		t.Errorf("FileID = %q，想要空（回落到内联）", img.FileID)
	}
	if len(img.Data) == 0 {
		t.Error("内联的字节是空的：回落之后图还是得送到")
	}
}

// 换了 key，旧的引用一律作废（服务方的 file_id 是绑在 key 上的），得重传一次。
func TestSuggestTagsReuploadsAfterKeyChange(t *testing.T) {
	h := newHarness(t, `{"tags":[{"subject":"数学","chapter":"","point":""}]}`)
	q := h.addQuestion(t, 0x23)

	if _, err := h.svc.SuggestTags(q.ID); err != nil {
		t.Fatalf("第一次 SuggestTags: %v", err)
	}
	if err := h.svc.SetConfig(vlm.Config{APIKey: "another-credential-entirely"}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if _, err := h.svc.SuggestTags(q.ID); err != nil {
		t.Fatalf("换 key 后 SuggestTags: %v", err)
	}

	if got := h.fake.UploadCount(); got != 2 {
		t.Errorf("上传了 %d 次，想要 2 次：换了 key 之后旧引用作废，得重传", got)
	}
}

// provider 报错要原样冒上来，不能被吞掉。
func TestSuggestTagsPropagatesProviderError(t *testing.T) {
	h := newHarness(t, `{"tags":[]}`)
	h.fake.ChatErr = errors.New("网络不通")
	q := h.addQuestion(t, 0x24)

	if _, err := h.svc.SuggestTags(q.ID); err == nil {
		t.Fatal("想要一个错，拿到 nil")
	}
}

// 题不存在时明确报错，而不是拿一个空 hash 去拼路径。
func TestSuggestTagsUnknownQuestion(t *testing.T) {
	h := newHarness(t, `{"tags":[]}`)

	if _, err := h.svc.SuggestTags(9999); err == nil {
		t.Fatal("想要一个错，拿到 nil")
	}
}

// 没配 VLM 时只有一个动作失败：识别报 ErrNotConfigured。
// 这一条不注入 provider —— 那才是真实接线的形状。
func TestSuggestTagsWithoutConfig(t *testing.T) {
	h := newHarness(t)
	q := h.addQuestion(t, 0x25)

	// 造一个配置文件不存在的服务（与 main.go 的接线形状一样，只是没配过）。
	unconfigured := vlm.NewService(h.lib, h.tags, filepath.Join(t.TempDir(), "vlm.json"))
	_, err := unconfigured.SuggestTags(q.ID)
	if !errors.Is(err, vlm.ErrNotConfigured) {
		t.Fatalf("err = %v，想要 ErrNotConfigured", err)
	}
}

// ── 保存：建树、挂题 ──

// 保存才生效：三层标签现建出来，再整份挂到这道题上。
func TestSaveTagsBuildsTreeAndAttaches(t *testing.T) {
	h := newHarness(t)
	q := h.addQuestion(t, 0x30)

	got, err := h.svc.SaveTags(q.ID, []vlm.ProposedTag{
		{Subject: "数学", Chapter: "高等数学", Point: "中值定理"},
	})
	if err != nil {
		t.Fatalf("SaveTags: %v", err)
	}

	// 挂上去的是整条路径：学科 + 章节 + 知识点。
	for _, want := range []struct {
		level tags.Level
		name  string
	}{
		{tags.LevelSubject, "数学"},
		{tags.LevelChapter, "高等数学"},
		{tags.LevelPoint, "中值定理"},
	} {
		if !hasName(got, want.level, want.name) {
			t.Errorf("题上没有 %s:%s，实际挂的是 %v", want.level, want.name, names(got))
		}
	}

	// 再读一次，确认真的落库了（而不是只活在返回值里）。
	if !hasName(h.tagsOf(t, q.ID), tags.LevelPoint, "中值定理") {
		t.Error("重新读回来的标签里没有那条知识点")
	}
}

// 已经有的标签照用，不新建：用户反复用同一批标签时词表不该长出一堆同名节点。
func TestSaveTagsReusesExistingTags(t *testing.T) {
	h := newHarness(t)
	q := h.addQuestion(t, 0x31)

	first, err := h.svc.SaveTags(q.ID, []vlm.ProposedTag{{Subject: "数学", Chapter: "高等数学", Point: "中值定理"}})
	if err != nil {
		t.Fatalf("第一次 SaveTags: %v", err)
	}
	// 「数学」是迁移里预置的学科，本来就在。
	lv1AfterFirst := 0
	for _, tag := range h.allTags(t) {
		if tag.Level == tags.LevelSubject {
			lv1AfterFirst++
		}
	}

	q2 := h.addQuestion(t, 0x32)
	second, err := h.svc.SaveTags(q2.ID, []vlm.ProposedTag{{Subject: "数学", Chapter: "高等数学", Point: "中值定理"}})
	if err != nil {
		t.Fatalf("第二次 SaveTags: %v", err)
	}

	// 两次落到的标签 id 应当是同一批。
	firstIDs := map[int64]bool{}
	for _, tag := range first {
		firstIDs[tag.ID] = true
	}
	for _, tag := range second {
		if !firstIDs[tag.ID] {
			t.Errorf("第二次用的是另一个标签（id=%d, %s），同名同父该复用", tag.ID, tag.Name)
		}
	}

	lv1AfterSecond := 0
	for _, tag := range h.allTags(t) {
		if tag.Level == tags.LevelSubject {
			lv1AfterSecond++
		}
	}
	if lv1AfterSecond != lv1AfterFirst {
		t.Errorf("顶层学科数从 %d 变成了 %d，同名不该再建一个", lv1AfterFirst, lv1AfterSecond)
	}
}

// 同一批建议重复交上来，题上的标签不该翻倍（整体替换，不是追加）。
func TestSaveTagsIsIdempotent(t *testing.T) {
	h := newHarness(t)
	q := h.addQuestion(t, 0x33)
	proposed := []vlm.ProposedTag{{Subject: "英语", Chapter: "阅读", Point: "主旨题"}}

	first, err := h.svc.SaveTags(q.ID, proposed)
	if err != nil {
		t.Fatalf("第一次 SaveTags: %v", err)
	}
	second, err := h.svc.SaveTags(q.ID, proposed)
	if err != nil {
		t.Fatalf("第二次 SaveTags: %v", err)
	}

	if len(first) != len(second) {
		t.Errorf("标签数从 %d 变成了 %d，重复保存不该翻倍", len(first), len(second))
	}
	if len(h.tagsOf(t, q.ID)) != len(second) {
		t.Errorf("重新读回来的标签数与返回值对不上：%v vs %v", names(h.tagsOf(t, q.ID)), names(second))
	}
}

// 空集 = 把标签全摘掉（界面上「一条都不留」就是这个意思）。
func TestSaveTagsWithEmptyListClearsTags(t *testing.T) {
	h := newHarness(t)
	q := h.addQuestion(t, 0x34)

	if _, err := h.svc.SaveTags(q.ID, []vlm.ProposedTag{{Subject: "政治", Chapter: "马原", Point: "辩证法"}}); err != nil {
		t.Fatalf("SaveTags: %v", err)
	}
	got, err := h.svc.SaveTags(q.ID, nil)
	if err != nil {
		t.Fatalf("清空 SaveTags: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("标签 = %v，想要空", names(got))
	}
}

// 没有学科的那条建议整条丢掉：没学科就不知道往哪棵树上挂。
// 只有学科（章节留空）是合法的：先搭骨架再往里填。
func TestSaveTagsHandlesShortPaths(t *testing.T) {
	h := newHarness(t)
	q := h.addQuestion(t, 0x35)

	got, err := h.svc.SaveTags(q.ID, []vlm.ProposedTag{
		{Subject: "", Chapter: "无处安放", Point: ""},
		{Subject: "专业课", Chapter: "", Point: ""},
	})
	if err != nil {
		t.Fatalf("SaveTags: %v", err)
	}

	if hasName(got, tags.LevelChapter, "无处安放") {
		t.Error("没有学科的那条不该被建出来")
	}
	if !hasName(got, tags.LevelSubject, "专业课") {
		t.Errorf("只有学科的那条该保留，实际挂的是 %v", names(got))
	}
}

// 模型把层级写错（章节与学科同名）时照建不误：那是模型的锅，
// 悄悄替它改名或丢掉只会让用户看不到模型到底给了什么。
func TestSaveTagsKeepsModelWording(t *testing.T) {
	h := newHarness(t)
	q := h.addQuestion(t, 0x36)

	got, err := h.svc.SaveTags(q.ID, []vlm.ProposedTag{{Subject: "数学", Chapter: "数学", Point: ""}})
	if err != nil {
		t.Fatalf("SaveTags: %v", err)
	}

	// 数学（学科，预置的那个）+ 数学（它下面的章节）。
	var chapters int
	for _, tag := range got {
		if tag.Level == tags.LevelChapter && tag.Name == "数学" {
			chapters++
		}
	}
	if chapters != 1 {
		t.Errorf("题上「数学」这一层的章节有 %d 个，想要 1 个：%v", chapters, names(got))
	}
}

// 题不存在时写操作明确报错，并且一个标签都不建（先验后建）。
func TestSaveTagsUnknownQuestion(t *testing.T) {
	h := newHarness(t)
	before := len(h.allTags(t))

	if _, err := h.svc.SaveTags(9999, []vlm.ProposedTag{{Subject: "新学科", Chapter: "新章节", Point: ""}}); err == nil {
		t.Fatal("想要一个错，拿到 nil")
	}
	if after := len(h.allTags(t)); after != before {
		t.Errorf("标签数从 %d 变成了 %d：题不存在时不该建任何标签", before, after)
	}
}

// ── 配置 ──

// 从没配过时，配置视图说得清「还没配」而不是崩掉。
func TestConfigViewWithoutFile(t *testing.T) {
	h := newHarness(t)
	unconfigured := vlm.NewService(h.lib, h.tags, filepath.Join(t.TempDir(), "vlm.json"))

	v := unconfigured.Config()
	if v.Configured {
		t.Error("Configured = true，想要 false")
	}
	if v.Problem == "" {
		t.Error("Problem 是空的：界面得知道缺什么")
	}
	if v.Path == "" {
		t.Error("Path 是空的：界面要把它显示出来")
	}
}

// 配好了的视图里带着模型名与地址，而凭据只有「有没有」与「多长」。
func TestConfigViewHidesCredential(t *testing.T) {
	h := newHarness(t)
	cfg := testConfig()

	v := h.svc.Config()
	if !v.Configured {
		t.Fatalf("Configured = false，Problem = %q", v.Problem)
	}
	if v.BaseURL != cfg.BaseURL || v.VisionModel != cfg.VisionModel || v.TextModel != cfg.TextModel {
		t.Errorf("视图 = %+v，想要带上配置里的地址与模型名", v)
	}
	if !v.APIKeySet || v.APIKeyLength != len(cfg.APIKey) {
		t.Errorf("凭据的存在性/长度 = %v/%d，想要 true/%d", v.APIKeySet, v.APIKeyLength, len(cfg.APIKey))
	}
}

// 补丁里的空串表示「不动这一项」：界面拿不到凭据，改别的项时没法把 key 带回来。
func TestSetConfigKeepsUnsetFields(t *testing.T) {
	h := newHarness(t)
	cfg := testConfig()

	if err := h.svc.SetConfig(vlm.Config{VisionModel: "another-vision-model"}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	v := h.svc.Config()
	if v.VisionModel != "another-vision-model" {
		t.Errorf("VisionModel = %q，想要改后的那个", v.VisionModel)
	}
	if v.BaseURL != cfg.BaseURL || !v.APIKeySet || v.APIKeyLength != len(cfg.APIKey) {
		t.Errorf("没传的项被清掉了：%+v", v)
	}
}

// 缺项的配置当场退回，不写盘 —— 一份存下去也用不了的配置不该躺在盘上冒充「配好了」。
func TestSetConfigRejectsIncomplete(t *testing.T) {
	h := newHarness(t)

	bad := vlm.NewService(h.lib, h.tags, filepath.Join(t.TempDir(), "vlm.json"))
	if err := bad.SetConfig(vlm.Config{BaseURL: "https://vlm.invalid/v1"}); !errors.Is(err, vlm.ErrNotConfigured) {
		t.Fatalf("err = %v，想要 ErrNotConfigured", err)
	}
	if bad.Config().Configured {
		t.Error("缺项的配置被当成配好了")
	}
}
