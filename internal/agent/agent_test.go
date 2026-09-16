package agent_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"questionbook/internal/agent"
	"questionbook/internal/agent/apply"
	"questionbook/internal/agent/store"
	"questionbook/internal/capture"
	"questionbook/internal/library"
	"questionbook/internal/review"
	"questionbook/internal/tags"
	"questionbook/internal/vlm"
)

// 全应用只有一条测试缝：服务层（spec）。这里从头到尾只调 agent.Service 与 agent.Pending
// 的方法，不断言它内部调了谁几次，也不启动 Wails。
//
// **一次网络都不打**：模型那一层是 vlm.Fake。这一点在这张票上不是顺手，而是硬要求 ——
// 工具循环会连着发好几次请求，真打网络既不稳定又烧钱。

// base 是测试的时间基准。时钟钉死是刻意的：「今天到期几道」「决定时刻」这类断言
// 用真实时钟没法写；而且基准取 UTC，读出来的日期就不会跟着跑测试的机器所在的时区变。
var base = time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)

// clock 是一个不走的假时钟。
type clock struct{ t time.Time }

func (c *clock) Now() time.Time { return c.t }

// testConfig 是一份能过 Validate 的配置。
//
// 模型名是**编的**，而且刻意不像任何一个真的模型名：代码里不许出现硬编码的模型 ID
// （ADR-0005），测试里也不该出现。这里要验的是「配置里的名字确实原样传到了模型那一层」。
func testConfig() vlm.Config {
	return vlm.Config{
		BaseURL:     "https://vlm.invalid/v1",
		APIKey:      "test-credential-not-a-real-one",
		VisionModel: "vision-model-from-config",
		TextModel:   "text-model-from-config",
	}
}

// harness 是临时目录里搭出来的一整套，形状与 main.go 的接线一致：
// 库、题图目录、标签服务、复习服务、只读存储、待批准存储、执行者、待批准服务、agent 服务。
// 服务之间共用同一条库连接。
type harness struct {
	svc     *agent.Service
	pending *agent.Pending
	lib     *library.Store
	tags    *tags.Service
	review  *review.Service
	read    *store.ReadStore
	fake    *vlm.Fake
	clock   *clock

	// streamID 是这一套里问出去时带的流式号（h.ask 用它）。
	//
	// **默认给一个非空值**：于是这一整份测试跑的都是**流式**那条路 —— 假 provider 把
	// 每一轮该说的话当成一片播出来（见 vlm.Fake.ChatStream），返回值与非流式一字不差，
	// 所以下面那些断言一个字都不用改。想跑非流式就把它置空（幂等那条路验过一次）。
	streamID string
}

func newHarness(t *testing.T, opts ...agent.Option) *harness {
	t.Helper()

	root := t.TempDir()
	// 题图目录照建，虽然 agent 这条路上没有一张图（工具调用的报文里不带图）。
	// 为的是与 main.go 同一个形状：将来谁把某个服务接错了地方，这里先红。
	if _, err := capture.NewStore(filepath.Join(root, "cards")); err != nil {
		t.Fatalf("开题图目录失败: %v", err)
	}
	lib, err := library.Open(filepath.Join(root, "library.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	t.Cleanup(func() { lib.Close() })

	clk := &clock{t: base}
	tagSvc := tags.NewService(lib)
	reviewSvc := review.NewService(lib, review.WithNow(clk.Now))

	cfgPath := filepath.Join(root, "vlm.json")
	if err := testConfig().Save(cfgPath); err != nil {
		t.Fatalf("写配置失败: %v", err)
	}

	fake := vlm.NewFake()

	// 接线与 main.go 一致，只有两处是测试的：
	//   - 模型换成假的（真接线是 provider 按配置现建）
	//   - 时钟钉死（真接线是 time.Now）
	//
	// 注意 readStore 拿到的是 lib.DB()（宽的那一个），而它自己只把它当 store.Querier 用
	// —— 收窄就发生在这一次赋值上，之后那一层里再也没有 Exec 可调。
	readStore := store.NewReadStore(lib.DB(), lib, tagSvc, store.WithNow(clk.Now))
	pendStore := store.NewPendingStore(lib.DB())
	pend := agent.NewPending(pendStore, apply.New(tagSvc), agent.WithNow(clk.Now))

	all := append([]agent.Option{agent.WithModel(fake)}, opts...)

	return &harness{
		svc:     agent.NewService(readStore, pend, cfgPath, all...),
		pending: pend,
		lib:     lib,
		tags:    tagSvc,
		review:  reviewSvc,
		read:    readStore,
		fake:    fake,
		clock:   clk,
		// 见 harness.streamID：默认就走流式那条路。
		streamID: "test-stream",
	}
}

// seed 造一个像样的世界：一棵标签树、两道错题、两次复习。返回那些 id。
//
// 时间戳全部钉在 base 上：题目创建时刻直接决定「今天到期几道」，用真实时钟写不出稳定断言。
type seed struct {
	math, prob, formula, english int64
	questionA, questionB         int64
}

func (h *harness) seed(t *testing.T) seed {
	t.Helper()

	// 预置的四个学科在迁移 2 里就建好了，这里只往下挂两层。
	all, err := h.tags.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var s seed
	s.math = tagID(t, all, 0, "数学")
	s.english = tagID(t, all, 0, "英语")

	prob, err := h.tags.Create(s.math, "概率论")
	if err != nil {
		t.Fatalf("Create 概率论: %v", err)
	}
	s.prob = prob.ID
	formula, err := h.tags.Create(s.prob, "全概率公式")
	if err != nil {
		t.Fatalf("Create 全概率公式: %v", err)
	}
	s.formula = formula.ID

	s.questionA = h.addQuestion(t, "sha256:题A")
	s.questionB = h.addQuestion(t, "sha256:题B")

	// 题 A 挂整条路径（三条都挂是手工打标签与 SaveTags 都有的形状），题 B 只挂学科。
	if err := h.tags.SetQuestionTags(s.questionA, []int64{s.math, s.prob, s.formula}); err != nil {
		t.Fatalf("SetQuestionTags A: %v", err)
	}
	if err := h.tags.SetQuestionTags(s.questionB, []int64{s.english}); err != nil {
		t.Fatalf("SetQuestionTags B: %v", err)
	}

	// 题 A 复习两次：Good 之后又 Again（「重来」）。这两个数是汇总里最要紧的。
	h.grade(t, s.questionA, review.Good)
	h.grade(t, s.questionA, review.Again)

	return s
}

func (h *harness) addQuestion(t *testing.T, hash string) int64 {
	t.Helper()
	q, err := h.lib.AddQuestion(library.Question{QuestionHash: hash, CreatedAt: base})
	if err != nil {
		t.Fatalf("AddQuestion: %v", err)
	}
	return q.ID
}

func (h *harness) grade(t *testing.T, id int64, rating review.Rating) {
	t.Helper()
	if _, err := h.review.Grade(id, rating); err != nil {
		t.Fatalf("Grade(%d, %v): %v", id, rating, err)
	}
}

func tagID(t *testing.T, all []tags.Tag, parent int64, name string) int64 {
	t.Helper()
	for _, one := range all {
		if one.ParentID == parent && one.Name == name {
			return one.ID
		}
	}
	t.Fatalf("标签树里没有「%s」（父=%d）", name, parent)
	return 0
}

// ── 造给模型看的那些回答 ──

// calls 造一轮「模型要求调用这几个工具」的回答。
func calls(cs ...vlm.ToolCall) vlm.Reply { return vlm.Reply{ToolCalls: cs} }

// call 造一次工具调用。
func call(id, name, args string) vlm.ToolCall {
	return vlm.ToolCall{ID: id, Name: name, Arguments: args}
}

// say 造一轮「模型说了句话」的回答。
func say(text string) vlm.Reply { return vlm.Reply{Text: text} }

// ask 问一句，失败即终止测试。
func (h *harness) ask(t *testing.T, question string) agent.Answer {
	t.Helper()
	a, err := h.svc.Ask(question, h.streamID)
	if err != nil {
		t.Fatalf("Ask(%q): %v", question, err)
	}
	return a
}

// toolResults 取出第 n 次请求之后模型看到的那几条工具结果（按顺序）。
func (h *harness) toolResults(t *testing.T, chat int) []string {
	t.Helper()
	if chat >= len(h.fake.Chats) {
		t.Fatalf("只跟模型往返了 %d 次，要不了第 %d 次", len(h.fake.Chats), chat)
	}
	var out []string
	for _, m := range h.fake.Chats[chat].Messages {
		if m.Role == vlm.RoleTool {
			out = append(out, m.Text)
		}
	}
	return out
}

// ── 正式数据的快照 ──

// domainTables 是「正式数据」那五张表。agent 这一层一个字节都不该动它们。
var domainTables = []string{"questions", "tags", "question_tags", "review_states", "review_logs"}

// dumpAll 把这几张表整份读出来，逐行拼成文本并排序 —— 拿来断言「一模一样」。
//
// 逐行读而不是数行数：数行数看不见「某一行的某个字段被改了」。
func dumpAll(t *testing.T, db *sql.DB, tables []string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, table := range tables {
		out[table] = dumpTable(t, db, table)
	}
	return out
}

func dumpTable(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	// 表名来自测试里写死的清单，不是外部输入。
	rows, err := db.Query(`SELECT * FROM ` + table)
	if err != nil {
		t.Fatalf("读 %s: %v", table, err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("读 %s 的列: %v", table, err)
	}
	out := []string{}
	for rows.Next() {
		cells := make([]any, len(cols))
		for i := range cells {
			cells[i] = new(any)
		}
		if err := rows.Scan(cells...); err != nil {
			t.Fatalf("读 %s 的行: %v", table, err)
		}
		parts := make([]string, len(cols))
		for i, c := range cells {
			switch v := (*(c.(*any))).(type) {
			case []byte:
				parts[i] = string(v)
			case nil:
				parts[i] = "<null>"
			default:
				parts[i] = fmt.Sprint(v)
			}
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读 %s: %v", table, err)
	}
	slices.Sort(out)
	return out
}

// assertUnchanged 断言这几张表逐行没变。失败时把变了的表整份打出来 —— 不然只看到一句
// 「不一样」，还得自己去翻哪一行。
func assertUnchanged(t *testing.T, what string, before, after map[string][]string) {
	t.Helper()
	for _, table := range domainTables {
		if !slices.Equal(before[table], after[table]) {
			t.Errorf("%s：%s 表变了", what, table)
			t.Errorf("  之前 %d 行：%v", len(before[table]), before[table])
			t.Errorf("  之后 %d 行：%v", len(after[table]), after[table])
		}
	}
}

func pendingRows(t *testing.T, db *sql.DB) []string {
	t.Helper()
	return dumpTable(t, db, "pending_changes")
}

// ── 一轮提问的形状 ──

// 模型不用工具、直接回答：一轮就收，提议是空切片不是 nil。
func TestAskAnswersWithoutTools(t *testing.T) {
	h := newHarness(t)
	h.seed(t)
	h.fake.Turns = []vlm.Reply{say("你有 2 道错题。")}

	a := h.ask(t, "我有几道错题")

	if a.Text != "你有 2 道错题。" {
		t.Errorf("Text = %q", a.Text)
	}
	if a.Rounds != 1 {
		t.Errorf("Rounds = %d，想要 1", a.Rounds)
	}
	if a.Problem != "" {
		t.Errorf("Problem = %q，想要空", a.Problem)
	}
	if a.Proposals == nil {
		t.Error("Proposals 是 nil，前端要拿到 [] 不是 null")
	}
	if n := len(h.fake.Chats); n != 1 {
		t.Errorf("跟模型往返了 %d 次，想要 1", n)
	}
}

// 模型先要数据、再回答：两轮，而且第二轮的报文里必须带得回「谁要的哪一次数据」。
//
// 这一条验的是官方那套循环规矩（见 vlm 包注释）：先 append 那条带 tool_calls 的
// assistant 消息，再为每一次调用 append 一条带 tool_call_id 的 tool 消息。
// 顺序错了、或者少了 assistant 那条，服务方那边就无从归属。
func TestAskRunsTheToolLoop(t *testing.T) {
	h := newHarness(t)
	h.seed(t)
	h.fake.Turns = []vlm.Reply{
		calls(
			call("c1", "list_tags", "{}"),
			call("c2", "list_questions", `{"limit":2}`),
		),
		say("你数学那块挂了两道题。"),
	}

	a := h.ask(t, "我数学哪块最弱")

	if a.Text != "你数学那块挂了两道题。" || a.Rounds != 2 {
		t.Errorf("回答 = %q / Rounds = %d，想要那句回答与 2 轮", a.Text, a.Rounds)
	}
	if len(h.fake.Chats) != 2 {
		t.Fatalf("往返了 %d 次，想要 2 次", len(h.fake.Chats))
	}

	// 每一轮都把工具摆出来（除了收尾那一轮，见回合用尽那条测试）。
	names := make([]string, 0, 5)
	for _, tool := range h.fake.Chats[0].Tools {
		names = append(names, tool.Name)
	}
	want := []string{"list_tags", "list_questions", "review_stats", "review_history", "propose_tag_change"}
	if !slices.Equal(names, want) {
		t.Errorf("摆出来的工具 = %v，想要 %v", names, want)
	}

	// 模型名来自配置，而且是**文本模型**那一档（agent 这条路上没有图）。
	if got := h.fake.Chats[0].Model; got != testConfig().TextModel {
		t.Errorf("模型 = %q，想要配置里的 %q", got, testConfig().TextModel)
	}
	if h.fake.Chats[0].System == "" {
		t.Error("没有系统提示词")
	}

	// 第二轮的报文：用户那句 → assistant（带两次调用）→ 两条工具结果。
	got := make([]string, 0, 4)
	for _, m := range h.fake.Chats[1].Messages {
		got = append(got, fmt.Sprintf("%s:%d", m.Role, len(m.ToolCalls)))
	}
	wantMsgs := []string{"user:0", "assistant:2", "tool:0", "tool:0"}
	if !slices.Equal(got, wantMsgs) {
		t.Errorf("第二轮发出去的报文 = %v，想要 %v", got, wantMsgs)
	}

	// 两条工具结果各自挂在对应的那次调用上，顺序也一样。
	tools := h.fake.Chats[1].Messages
	if tools[2].ToolCallID != "c1" || tools[3].ToolCallID != "c2" {
		t.Errorf("工具结果的归属 = %q / %q，想要 c1 / c2", tools[2].ToolCallID, tools[3].ToolCallID)
	}
	if tools[2].Text == "" || tools[3].Text == "" {
		t.Error("工具结果里有空的：模型什么也看不到")
	}
}

// 四个只读工具确实把库里的东西读出来了 —— 「agent 能通过读数据回答全局性问题」这条
// 验收项看的就这一步。
func TestToolsReturnTheRealData(t *testing.T) {
	h := newHarness(t)
	s := h.seed(t)
	h.fake.Turns = []vlm.Reply{
		calls(
			call("c1", "list_tags", "{}"),
			call("c2", "list_questions", "{}"),
			call("c3", "review_stats", "{}"),
			call("c4", "review_history", fmt.Sprintf(`{"question_id":%d}`, s.questionA)),
		),
		say("看完了。"),
	}
	h.ask(t, "我数学哪块最弱")

	res := h.toolResults(t, 1)
	if len(res) != 4 {
		t.Fatalf("工具结果有 %d 条，想要 4 条：%v", len(res), res)
	}

	// list_tags：整棵树都在，而且带路径。
	var tagsOut tagsResult
	decode(t, res[0], &tagsOut)
	if !tagsOut.has(s.formula, "数学 / 概率论 / 全概率公式") {
		t.Errorf("list_tags 里没有「数学 / 概率论 / 全概率公式」这一条：%s", res[0])
	}

	// list_questions：两道题都在，A 挂着三条标签、复习过两次、重来一次。
	var qsOut questionsResult
	decode(t, res[1], &qsOut)
	if qsOut.Total != 2 {
		t.Errorf("list_questions 的总数 = %d，想要 2：%s", qsOut.Total, res[1])
	}
	var found bool
	for _, one := range qsOut.Questions {
		if one.ID != s.questionA {
			continue
		}
		found = true
		if len(one.Tags) != 3 || !one.Reviewed || one.Reps != 2 || one.Lapses != 1 {
			t.Errorf("题 A 那一条 = %+v，想要 3 个标签 / 复习过 / reps 2 / lapses 1", one)
		}
	}
	if !found {
		t.Errorf("list_questions 里没有题 A（#%d）：%s", s.questionA, res[1])
	}

	// review_stats：整棵树，每个标签的计数含子孙；一道题都没挂的标签也在。
	var statsOut statsResult
	decode(t, res[2], &statsOut)
	byTag := map[int64]int{}
	for i, one := range statsOut.Tags {
		byTag[one.TagID] = i
	}
	// 题 A 三条标签都挂着，所以三行都该是「1 道题、复习过、reps 2、lapses 1」
	// ——**含子孙但一道题只算一次**，不是把整条路径加三遍。
	for _, id := range []int64{s.math, s.prob, s.formula} {
		i, ok := byTag[id]
		if !ok {
			t.Fatalf("review_stats 里没有标签 %d：%s", id, res[2])
		}
		one := statsOut.Tags[i]
		if one.Questions != 1 || one.Reviewed != 1 || one.Reps != 2 || one.Lapses != 1 {
			t.Errorf("标签 %d 的汇总 = %+v，想要 1 道题 / 复习过 / reps 2 / lapses 1", id, one)
		}
	}
	// 题 B 从没复习过，它是新题 —— 从被拍下来那一刻起就到期（与复习页同一个口径）。
	if i, ok := byTag[s.english]; !ok {
		t.Errorf("review_stats 里没有英语：%s", res[2])
	} else if one := statsOut.Tags[i]; one.Questions != 1 || one.Reviewed != 0 || one.Due != 1 {
		t.Errorf("英语的汇总 = %+v，想要 1 道题 / 没复习过 / 到期 1", one)
	}
	// 一个题都没挂的标签也要在（「这块我一道题都没拍」是答案的一部分）。
	if len(statsOut.Tags) < 6 {
		t.Errorf("review_stats 只有 %d 行，六个预置/新建的标签都该在", len(statsOut.Tags))
	}

	// review_history：两次复习，最近的在先（时钟钉死，所以顺序只可能来自自增 id）。
	var histOut historyResult
	decode(t, res[3], &histOut)
	if histOut.Count != 2 {
		t.Fatalf("复习记录 = %d 条，想要 2 条：%s", histOut.Count, res[3])
	}
	if histOut.Entries[0].Rating != 1 || histOut.Entries[0].RatingName != "Again" {
		t.Errorf("最近那条 = %+v，想要 Again", histOut.Entries[0])
	}
}

// 模型挑错工具、参数不是合法 JSON、引用了不存在的 id —— 三种都不该把循环打断，
// 而是把话喂回去让它重试。这是工具循环最容易写错的一处。
func TestToolErrorsGoBackToTheModel(t *testing.T) {
	h := newHarness(t)
	h.seed(t)
	h.fake.Turns = []vlm.Reply{
		calls(
			call("c1", "list_weather", "{}"),
			call("c2", "list_questions", `{"limit":`),
			call("c3", "review_history", `{"question_id":9999}`),
			call("c4", "propose_tag_change", `{"action":"rename_tag","reason":"改名","tag_id":9999,"name":"新名"}`),
		),
		say("刚才那几次都没成。"),
	}

	a := h.ask(t, "帮我看看")

	if a.Text != "刚才那几次都没成。" {
		t.Errorf("Text = %q，想要模型在收到失败之后照样答出话", a.Text)
	}
	res := h.toolResults(t, 1)
	if len(res) != 4 {
		t.Fatalf("工具结果有 %d 条，想要 4 条", len(res))
	}
	for i, got := range res {
		if !strings.Contains(got, "error") {
			t.Errorf("第 %d 条工具结果不是失败说明：%s", i+1, got)
		}
	}
	if !strings.Contains(res[0], "list_weather") {
		t.Errorf("认不出工具时该把可用的工具列出来：%s", res[0])
	}
	if !strings.Contains(res[2], "9999") {
		t.Errorf("id 不存在时该说清是哪个 id：%s", res[2])
	}

	// 一次失败都不该留下记录。
	if n, err := h.pending.Count(); err != nil || n != 0 {
		t.Errorf("待批准 %d 条（err=%v），想要 0 条：失败的提议不该留下什么", n, err)
	}
	if got := pendingRows(t, h.lib.DB()); len(got) != 0 {
		t.Errorf("pending_changes 里有 %v，想要空的", got)
	}
}

// 回合用尽：把工具收走再问最后一次，让它拿现有信息作答 —— 而不是把这一轮白扔。
func TestRoundsExhaustedStillAnswers(t *testing.T) {
	h := newHarness(t, agent.WithMaxRounds(2))
	h.seed(t)
	h.fake.Turns = []vlm.Reply{
		calls(call("c1", "list_tags", "{}")),
		calls(call("c2", "list_questions", "{}")),
		say("只看到这两样，标签树我有，错题也看到了。"),
	}

	a := h.ask(t, "我哪块最弱")

	if a.Rounds != 3 {
		t.Errorf("Rounds = %d，想要 3（两轮工具 + 一次收尾）", a.Rounds)
	}
	if !strings.Contains(a.Text, "只看到这两样") {
		t.Errorf("Text = %q，想要收尾那一轮的回答", a.Text)
	}
	if a.Problem == "" {
		t.Error("Problem 是空的：回合用尽这件事必须说出来")
	}
	if len(h.fake.Chats) != 3 {
		t.Fatalf("往返了 %d 次，想要 3 次", len(h.fake.Chats))
	}
	// 收尾那一轮**不能**再给工具：给了它就能继续要数据，上限形同虚设。
	if got := h.fake.Chats[2].Tools; len(got) != 0 {
		t.Errorf("收尾那一轮还摆着 %d 件工具，想要 0 件", len(got))
	}
	// 而它仍然带着前面几轮攒下的数据 —— 那是「再问一次」值得做的全部理由。
	if n := countRole(h.fake.Chats[2].Messages, vlm.RoleTool); n != 2 {
		t.Errorf("收尾那一轮手里只有 %d 条工具结果，想要 2 条", n)
	}
}

// 一轮提问里提议的次数有上限：撞上它就该停下，而不是无休止地往待批准列表里塞。
func TestProposalsAreCappedPerRound(t *testing.T) {
	h := newHarness(t)
	s := h.seed(t)

	cs := make([]vlm.ToolCall, 0, maxProposalsPerRoundTest+1)
	for i := 0; i <= maxProposalsPerRoundTest; i++ {
		cs = append(cs, call(fmt.Sprintf("c%d", i), "propose_tag_change",
			fmt.Sprintf(`{"action":"create_tag","reason":"补齐考点","parent_id":%d,"name":"新知识点%d"}`, s.math, i)))
	}
	h.fake.Turns = []vlm.Reply{calls(cs...), say("提完了。")}

	a := h.ask(t, "把缺的考点补上")

	if len(a.Proposals) != maxProposalsPerRoundTest {
		t.Errorf("提议了 %d 条，想要 %d 条封顶", len(a.Proposals), maxProposalsPerRoundTest)
	}
	res := h.toolResults(t, 1)
	last := res[len(res)-1]
	if !strings.Contains(last, "error") {
		t.Errorf("第 %d 条该是被拦下来的：%s", len(res), last)
	}
	if n, err := h.pending.Count(); err != nil || n != maxProposalsPerRoundTest {
		t.Errorf("待批准 %d 条（err=%v），想要 %d 条", n, err, maxProposalsPerRoundTest)
	}
}

// maxProposalsPerRoundTest 是上面那条测试用的条数。
//
// 写死在这里而不是去引用内部的 maxProposals：那个数是实现细节，而这条测试要钉的是
// 「有个上限，而且它会拦住第 N+1 条」——上限具体是几，改了实现不该让这条测试红。
const maxProposalsPerRoundTest = 20

// 空话发不出去，也不会去打扰模型。
func TestAskRejectsEmptyQuestion(t *testing.T) {
	h := newHarness(t)
	h.seed(t)

	for _, blank := range []string{"", "   ", "\n\t"} {
		if _, err := h.svc.Ask(blank, h.streamID); !errors.Is(err, agent.ErrEmptyQuestion) {
			t.Errorf("Ask(%q) 返回 %v，想要 ErrEmptyQuestion", blank, err)
		}
	}
	if n := len(h.fake.Chats); n != 0 {
		t.Errorf("空话也让模型跑了 %d 次", n)
	}
}

// 没配 VLM 时明确报错，而不是偷偷用一个写死的模型名 —— ADR-0005 的硬要求。
func TestAskWithoutConfig(t *testing.T) {
	h := newHarness(t)
	h.seed(t)

	// 与 main.go 同一个形状，只是没配过：配置文件不在，也没注入模型。
	unconfigured := agent.NewService(h.read, h.pending, filepath.Join(t.TempDir(), "vlm.json"))

	if _, err := unconfigured.Ask("我有几道错题", ""); !errors.Is(err, vlm.ErrNotConfigured) {
		t.Fatalf("err = %v，想要 ErrNotConfigured", err)
	}
}

func decode(t *testing.T, raw string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(raw), v); err != nil {
		t.Fatalf("工具结果不是预期的 JSON: %v\n%s", err, raw)
	}
}

// ── 工具返回的 JSON 长什么样 ──
//
// 这四份形状与 tools.go 里那四个 row 结构体是一份契约的两半：**只**把测试真正
// 要断言的字段列出来（多列一个就多一处会因为无关改动而红的地方），
// 字段名照 tools.go 的 json tag 抄。

type tagsResult struct {
	Count int `json:"count"`
	Tags  []struct {
		ID       int64  `json:"id"`
		ParentID int64  `json:"parent_id"`
		Name     string `json:"name"`
		Level    int    `json:"level"`
		Path     string `json:"path"`
	} `json:"tags"`
}

// has 报告某个标签是不是带着这条路径在结果里。
func (r tagsResult) has(id int64, path string) bool {
	for _, one := range r.Tags {
		if one.ID == id && one.Path == path {
			return true
		}
	}
	return false
}

type questionsResult struct {
	Total     int `json:"total"`
	Offset    int `json:"offset"`
	Count     int `json:"count"`
	Questions []struct {
		ID       int64    `json:"id"`
		Created  string   `json:"created_at"`
		Tags     []string `json:"tags"`
		Reviewed bool     `json:"reviewed"`
		Reps     int      `json:"reps"`
		Lapses   int      `json:"lapses"`
	} `json:"questions"`
}

type statsResult struct {
	Tags []struct {
		TagID     int64  `json:"tag_id"`
		Path      string `json:"path"`
		Questions int    `json:"questions"`
		Reviewed  int    `json:"reviewed"`
		Due       int    `json:"due"`
		Reps      int    `json:"reps"`
		Lapses    int    `json:"lapses"`
	} `json:"tags"`
}

type historyResult struct {
	Count   int `json:"count"`
	Entries []struct {
		Rating     int    `json:"rating"`
		RatingName string `json:"rating_name"`
		ReviewedAt string `json:"reviewed_at"`
	} `json:"entries"`
}

func countRole(msgs []vlm.Message, role vlm.Role) int {
	n := 0
	for _, m := range msgs {
		if m.Role == role {
			n++
		}
	}
	return n
}

// ── 安全属性：提议只写待批准表，一个字节的正式数据都不动 ──

// proposeAllTurns 造一轮「四种动作一次提全」的回答。
//
// 四个动作是标签那一类写操作的**全集**（Change 的四个 Action）。一次提问里全提出来，
// 是为了让下面那条断言覆盖到每一条执行路径 —— 只试一种的话，另外三种绕没绕过就没人知道。
func proposeAllTurns(s seed) []vlm.ToolCall {
	return []vlm.ToolCall{
		call("c1", "propose_tag_change",
			fmt.Sprintf(`{"action":"create_tag","reason":"这一章常考但还没建","parent_id":%d,"name":"贝叶斯公式"}`, s.math)),
		call("c2", "propose_tag_change",
			fmt.Sprintf(`{"action":"rename_tag","reason":"名字与教材对不上","tag_id":%d,"name":"概率论与数理统计"}`, s.prob)),
		call("c3", "propose_tag_change",
			fmt.Sprintf(`{"action":"delete_tag","reason":"太细了，用不上","tag_id":%d}`, s.formula)),
		call("c4", "propose_tag_change",
			fmt.Sprintf(`{"action":"tag_question","reason":"这道题其实考的是数学","question_id":%d,"tag_ids":[%d]}`, s.questionB, s.math)),
	}
}

// **安全属性的行为那一半**：让模型把四种写操作全提一遍，正式数据必须逐行一模一样。
//
// 这一条是「不存在绕过待批准层直接改数据的路径」的**正面证据**：不是「我没看到它写」，
// 而是「它把能提的全提了，正式数据逐行、逐字段、一个字节没动」。
// 与之配对的是 safety_test.go：那几条证明这条路上**没有**可以调用的写动作。
func TestProposeTouchesNothingButPendingChanges(t *testing.T) {
	h := newHarness(t)
	s := h.seed(t)

	before := dumpAll(t, h.lib.DB(), domainTables)
	h.fake.Turns = []vlm.Reply{calls(proposeAllTurns(s)...), say("我建议改这四处。")}

	a := h.ask(t, "帮我整理一下标签")
	after := dumpAll(t, h.lib.DB(), domainTables)

	assertUnchanged(t, "一轮提问提了四种改动之后", before, after)

	// 提议本身必须落下来了 —— 不然上面那句「没变」只是因为什么都没发生，
	// 那种测试是空转的。
	if len(a.Proposals) != 4 {
		t.Fatalf("提议了 %d 条，想要 4 条：%v", len(a.Proposals), a.Proposals)
	}
	if n, err := h.pending.Count(); err != nil || n != 4 {
		t.Fatalf("待批准 %d 条（err=%v），想要 4 条", n, err)
	}

	// 摘要由我们写：用户点头之前读到的是一句陈述事实的话，不是模型的劝说。
	// 逐条对一遍 —— 它同时把「引用的 id 核对过了吗」这件事验了一半。
	wantSummaries := []string{
		"在「数学」下新建章节「贝叶斯公式」",
		"把「数学 / 概率论」改名为「概率论与数理统计」",
		"删除「数学 / 概率论 / 全概率公式」（连同它下面 0 个子标签，会从 1 道错题上摘掉这些标签）",
		fmt.Sprintf("把错题 #%d 挂着的标签整体换成 1 个；新增「数学」；摘掉「英语」", s.questionB),
	}
	for i, want := range wantSummaries {
		if got := a.Proposals[i].Summary; got != want {
			t.Errorf("第 %d 条摘要 = %q\n                        想要 %q", i+1, got, want)
		}
	}
	// 每一条都要有理由，而且都是待批准。
	for _, p := range a.Proposals {
		if p.Status != agent.StatusPending {
			t.Errorf("提议 #%d 的状态 = %q，想要 pending", p.ID, p.Status)
		}
		if p.Reason == "" {
			t.Errorf("提议 #%d 没有理由", p.ID)
		}
		if p.CreatedAt.IsZero() {
			t.Errorf("提议 #%d 没有提议时刻", p.ID)
		}
	}

	// 而列表里读回来的东西与这一轮返回的是同一批。
	list, err := h.pending.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 4 {
		t.Fatalf("List 里有 %d 条，想要 4 条", len(list))
	}
	for i := range list {
		if list[i].ID != a.Proposals[i].ID {
			t.Errorf("List[%d].ID = %d，想要 %d（早的在前）", i, list[i].ID, a.Proposals[i].ID)
		}
	}
}

// 批准一条，只有这一条生效 —— 另外三条仍在原地，而「生效」这件事落到了标签树上。
func TestApproveAppliesOnlyThatOneChange(t *testing.T) {
	h := newHarness(t)
	s := h.seed(t)

	h.fake.Turns = []vlm.Reply{calls(proposeAllTurns(s)...), say("提完了。")}
	a := h.ask(t, "帮我整理一下标签")

	before := dumpAll(t, h.lib.DB(), []string{"questions", "question_tags", "review_states", "review_logs"})
	tagsBefore := dumpTable(t, h.lib.DB(), "tags")

	// 批准第一条：在「数学」下新建「贝叶斯公式」。
	got, err := h.pending.Approve(a.Proposals[0].ID)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if got.Status != agent.StatusApplied {
		t.Fatalf("批准之后的状态 = %q，想要 applied（Problem=%q）", got.Status, got.Problem)
	}
	if !got.DecidedAt.Equal(base) {
		t.Errorf("决定时刻 = %s，想要 %s", got.DecidedAt, base)
	}

	after := dumpAll(t, h.lib.DB(), []string{"questions", "question_tags", "review_states", "review_logs"})
	assertUnchanged(t, "批准一条 create_tag 之后", before, after)

	// 只有 tags 那张表动了，而且只多了一行。
	tagsAfter := dumpTable(t, h.lib.DB(), "tags")
	if len(tagsAfter) != len(tagsBefore)+1 {
		t.Errorf("tags 表 %d 行 → %d 行，想要只多一行", len(tagsBefore), len(tagsAfter))
	}
	if !h.hasTag(t, s.math, "贝叶斯公式") {
		t.Error("批准之后标签树上还是找不到「贝叶斯公式」")
	}

	// 另外三条一条都没生效：改名没改、删除没删、错题的标签没换。
	if !h.hasTag(t, 0, "概率论") {
		t.Error("「概率论」不在了 —— 那条 rename 没批，它不该被改掉")
	}
	if !h.hasTag(t, 0, "全概率公式") {
		t.Error("「全概率公式」不在了 —— 那条 delete 没批，它不该被删掉")
	}
	if names := h.questionTags(t, s.questionB); !slices.Equal(names, []string{"英语"}) {
		t.Errorf("题 B 的标签 = %v —— 那条 tag_question 没批，它不该被动过", names)
	}

	// 没批的三条还在待批准列表里，状态没变。
	list, err := h.pending.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Errorf("待批准还剩 %d 条，想要 3 条", len(list))
	}
	if n, _ := h.pending.Count(); n != 3 {
		t.Errorf("Count = %d，想要 3", n)
	}
}

// 丢掉一条不碰别的：已生效的那条不回滚，剩下的待批准不受影响，正式数据也不再动。
func TestDiscardLeavesEverythingElseAlone(t *testing.T) {
	h := newHarness(t)
	s := h.seed(t)

	h.fake.Turns = []vlm.Reply{calls(proposeAllTurns(s)...), say("提完了。")}
	a := h.ask(t, "帮我整理一下标签")

	if _, err := h.pending.Approve(a.Proposals[0].ID); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	tagsAfterApprove := dumpTable(t, h.lib.DB(), "tags")
	domainAfterApprove := dumpAll(t, h.lib.DB(), []string{"questions", "question_tags", "review_states", "review_logs"})

	// 丢掉第三条（删除「全概率公式」）—— 破坏性最大的那一条。
	got, err := h.pending.Discard(a.Proposals[2].ID)
	if err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if got.Status != agent.StatusDiscarded {
		t.Errorf("丢弃之后的状态 = %q，想要 discarded", got.Status)
	}

	assertUnchanged(t, "丢弃一条之后", domainAfterApprove,
		dumpAll(t, h.lib.DB(), []string{"questions", "question_tags", "review_states", "review_logs"}))
	if !slices.Equal(tagsAfterApprove, dumpTable(t, h.lib.DB(), "tags")) {
		t.Error("tags 表变了 —— 丢弃一条改动不该动任何已经生效的东西")
	}
	if !h.hasTag(t, 0, "全概率公式") {
		t.Error("「全概率公式」被删掉了 —— 丢掉的提议不该被执行")
	}

	// 剩下的两条原样待批准（丢掉的那条不再出现）。
	list, err := h.pending.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var ids []int64
	for _, p := range list {
		ids = append(ids, p.ID)
	}
	if !slices.Equal(ids, []int64{a.Proposals[1].ID, a.Proposals[3].ID}) {
		t.Errorf("剩下的待批准 = %v，想要 %v", ids, []int64{a.Proposals[1].ID, a.Proposals[3].ID})
	}
}

// 一条提议落库之后世界变了（标签被手工删掉了）：批准**不报 error**，而是在 Problem 里
// 说清为什么，状态停在 pending，用户还能再试或者丢掉它。
//
// 为什么这很重要：Wails 在出错时会把返回值一起丢掉。用 error 表达的话，界面既拿不到
// 原因，也拿不回那条记录 —— 用户只能看着它莫名其妙地消失。
func TestApproveReportsStaleProposalAsProblem(t *testing.T) {
	h := newHarness(t)
	s := h.seed(t)

	h.fake.Turns = []vlm.Reply{calls(proposeAllTurns(s)...), say("提完了。")}
	a := h.ask(t, "帮我整理一下标签")

	// 世界变了：用户在别处亲手把「概率论」整棵删了（那条 rename 于是无处可落）。
	renameID := a.Proposals[1].ID
	if _, err := h.tags.Delete(s.prob); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got, err := h.pending.Approve(renameID)
	if err != nil {
		t.Fatalf("Approve 返回了 error（%v）—— 「这一条现在做不了」应当是 Problem，不是 error", err)
	}
	if got.Problem == "" {
		t.Fatal("Problem 是空的：执行失败必须留下原因，否则界面上一片沉默")
	}
	if got.Status != agent.StatusPending {
		t.Errorf("状态 = %q，想要仍停在 pending（那一步确实没有发生）", got.Status)
	}
	if !got.DecidedAt.IsZero() {
		t.Errorf("DecidedAt = %s，想要零值 —— 没生效就不该有决定时刻", got.DecidedAt)
	}

	// 它还在待批准列表里，Problem 也读得回来。
	list, err := h.pending.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found *agent.PendingChange
	for i := range list {
		if list[i].ID == renameID {
			found = &list[i]
		}
	}
	if found == nil {
		t.Fatal("执行失败的提议从待批准列表里消失了")
	}
	if found.Problem == "" {
		t.Error("落库的 Problem 是空的：界面刷新一次就再也看不到原因了")
	}

	// 还能丢掉它 —— 一条做不了的提议不该把用户堵死。
	if _, err := h.pending.Discard(renameID); err != nil {
		t.Errorf("Discard 一条执行失败的提议时报错: %v", err)
	}
}

// 决定过了的再决定：批准已丢弃的、丢弃已生效的，都是 ErrNotPending 而不是悄悄成功。
// 丢弃本身幂等 —— 界面连点两下不该看到一句错。
func TestDecidedChangesAreNotDecidableAgain(t *testing.T) {
	h := newHarness(t)
	s := h.seed(t)

	h.fake.Turns = []vlm.Reply{calls(proposeAllTurns(s)...), say("提完了。")}
	a := h.ask(t, "帮我整理一下标签")

	// 只用得着其中两条，但四条都取出来：索引与「第几个动作」的对应关系是这条测试
	// 读得懂的关键（0 建、1 改名、2 删、3 换标签）。
	_, rename, del, _ := a.Proposals[0].ID, a.Proposals[1].ID, a.Proposals[2].ID, a.Proposals[3].ID

	if _, err := h.pending.Approve(rename); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if _, err := h.pending.Discard(del); err != nil {
		t.Fatalf("Discard: %v", err)
	}

	// 已丢弃的再批准：不做幂等，明确报错。
	if _, err := h.pending.Approve(del); !errors.Is(err, agent.ErrNotPending) {
		t.Errorf("批准一条已丢弃的改动返回 %v，想要 ErrNotPending", err)
	}
	// 已生效的再丢弃：同样报错（不然「改都改了」与「这条被丢掉了」会互相矛盾）。
	if _, err := h.pending.Discard(rename); !errors.Is(err, agent.ErrNotPending) {
		t.Errorf("丢弃一条已生效的改动返回 %v，想要 ErrNotPending", err)
	}
	// 丢弃幂等：再丢一次拿到的是同一条记录，不报错。
	again, err := h.pending.Discard(del)
	if err != nil {
		t.Errorf("重复丢弃报错: %v", err)
	}
	if again.Status != agent.StatusDiscarded {
		t.Errorf("重复丢弃之后的状态 = %q，想要 discarded", again.Status)
	}

	// id 不存在是 error（请求本身不成立），而不是一条空记录。
	if _, err := h.pending.Approve(9999); !errors.Is(err, agent.ErrNotFound) {
		t.Errorf("批准一个不存在的 id 返回 %v，想要 ErrNotFound", err)
	}
	if _, err := h.pending.Discard(9999); !errors.Is(err, agent.ErrNotFound) {
		t.Errorf("丢弃一个不存在的 id 返回 %v，想要 ErrNotFound", err)
	}

	// 剩下那两条（create 与 tag_question）还没被动过 —— 上面这些决定都不该牵连到它们。
	if n, _ := h.pending.Count(); n != 2 {
		t.Errorf("待批准还剩 %d 条，想要 2 条", n)
	}
	if names := h.questionTags(t, s.questionB); !slices.Equal(names, []string{"英语"}) {
		t.Errorf("题 B 的标签 = %v，想要还是「英语」", names)
	}
}

// ── 测试用的几个小工具 ──

// hasTag 报告标签树里有没有这么一个标签（parent = 0 表示不管挂在谁下面）。
func (h *harness) hasTag(t *testing.T, parent int64, name string) bool {
	t.Helper()
	all, err := h.tags.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, one := range all {
		if one.Name == name && (parent == 0 || one.ParentID == parent) {
			return true
		}
	}
	return false
}

// questionTags 返回一道错题挂着的那几条标签的**名字**。
//
// 比的是名字而不是 id：这条测试问的是「题上还挂着「英语」吗」，
// 而 id 在断言里读起来完全看不出那件事。
func (h *harness) questionTags(t *testing.T, questionID int64) []string {
	t.Helper()
	one, err := h.tags.TagsOfQuestion(questionID)
	if err != nil {
		t.Fatalf("TagsOfQuestion(%d): %v", questionID, err)
	}
	out := make([]string, 0, len(one))
	for _, tg := range one {
		out = append(out, tg.Name)
	}
	slices.Sort(out)
	return out
}
