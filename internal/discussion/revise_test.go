package discussion_test

import (
	"errors"
	"slices"
	"testing"

	"questionbook/internal/discussion"
	"questionbook/internal/library"
)

// 这一份是「重新生成」与「编辑并重发」两条。
//
// 它们各自要守的东西不一样，测试也分成两摊：
//
//   - 重新生成：**先问、后换**。所以失败路径要验的是「旧回答还在」——
//     先删后问的实现会在这一格上红。
//   - 编辑并重发：**先落库、后问**。所以失败路径要验的是「用户改的那句还在」——
//     与 TestAskKeepsTheQuestionWhenTheModelFails 同一条不变量，只是入口不同。
//
// 还有一条共有的：两条都不能碰**别的题**的讨论。记录是按 id 找的，
// 少一个 question_id 条件就会顺手删到别处 —— 那是完全看不出来的那种坏。

// ── 重新生成 ──

// 重说一遍之后，历史形状是「那句提问 + 新的回答」，旧的回答不在了。
func TestRegenerateReplacesTheAnswer(t *testing.T) {
	h := newHarness(t, "第一次回答", "第二次回答")
	q := h.addQuestion(t, 0x60)

	turn := h.ask(t, q.ID, "这步为什么")
	oldReply := turn.Reply.ID

	got, err := h.svc.Regenerate(q.ID, oldReply)
	if err != nil {
		t.Fatalf("Regenerate: %v", err)
	}

	want := []string{"user:这步为什么", "assistant:第二次回答"}
	if !slices.Equal(texts(got), want) {
		t.Errorf("重说之后的讨论 = %v，想要 %v", texts(got), want)
	}

	// 返回值与库里那份是同一件事（不是只活在返回值里）。
	if !slices.Equal(texts(h.history(t, q.ID)), want) {
		t.Errorf("回看这道题 = %v，想要 %v", texts(h.history(t, q.ID)), want)
	}

	// 旧那条**真的没了**，不是还在库里只是没显示：直接数那张表。
	if n := h.countRows(t, q.ID, oldReply); n != 0 {
		t.Errorf("旧回答（id=%d）在库里还剩 %d 行，想要 0", oldReply, n)
	}
	// 新那条是**另起一行**（AUTOINCREMENT：id 不回收）—— 排序靠它兜底。
	newReply := got[len(got)-1]
	if newReply.ID <= oldReply {
		t.Errorf("新回答的 id = %d，不比旧回答 %d 大：id 该是递增的", newReply.ID, oldReply)
	}
	if !newReply.CreatedAt.Equal(base) {
		t.Errorf("新回答的时刻 = %s，想要钉死的那一档 %s", newReply.CreatedAt, base)
	}
}

// 重说时发出去的是「砍到那句提问为止」的上下文：**旧回答不跟着去** ——
// 带着去的话模型只是照抄自己上一遍，那不叫重新生成。
func TestRegenerateAsksWithTheConversationUpToTheQuestion(t *testing.T) {
	h := newHarness(t, "答一", "重说的答二")
	q := h.addQuestion(t, 0x61)

	h.ask(t, q.ID, "第一问")
	second := h.ask(t, q.ID, "第二问")

	if _, err := h.svc.Regenerate(q.ID, second.Reply.ID); err != nil {
		t.Fatalf("Regenerate: %v", err)
	}

	if len(h.fake.Chats) != 3 {
		t.Fatalf("模型被叫了 %d 次，想要 3 次（两轮问答 + 一次重说）", len(h.fake.Chats))
	}
	got := make([]string, 0, 4)
	for _, m := range h.fake.Chats[2].Messages {
		got = append(got, string(m.Role)+":"+m.Text)
	}
	want := []string{"user:第一问", "assistant:答一", "user:第二问"}
	if !slices.Equal(got, want) {
		t.Errorf("重说时发出去的对话 = %v，想要 %v（旧回答不该在里面）", got, want)
	}
	// 图照旧挂在最后一条用户消息上 —— 重说也得看得见题。
	last := h.fake.Chats[2].Messages[len(got)-1]
	if len(last.Images) != 1 || last.Images[0].Hash != q.QuestionHash {
		t.Errorf("重说时最后一条消息上的图 = %+v，想要题图那一张", last.Images)
	}
}

// 模型没答上来时**旧回答必须还在**：这一条钉住「先问、后换」那个顺序。
// 先删后问的实现到这里会红（用户什么都没换到，还倒赔一条回答）。
func TestRegenerateKeepsTheAnswerWhenTheModelFails(t *testing.T) {
	h := newHarness(t, "第一次回答")
	q := h.addQuestion(t, 0x62)

	turn := h.ask(t, q.ID, "这步为什么")
	h.fake.ChatErr = errors.New("网络不通")

	if _, err := h.svc.Regenerate(q.ID, turn.Reply.ID); err == nil {
		t.Fatal("想要一个错，拿到 nil")
	}

	want := []string{"user:这步为什么", "assistant:第一次回答"}
	if got := texts(h.history(t, q.ID)); !slices.Equal(got, want) {
		t.Errorf("失败之后回看这道题 = %v，想要原样不动 %v", got, want)
	}
	if n := h.countRows(t, q.ID, turn.Reply.ID); n != 1 {
		t.Errorf("旧回答（id=%d）在库里有 %d 行，想要 1：失败不该把一条好好的回答删掉", turn.Reply.ID, n)
	}
}

// 只认最后一条回答。中间那条、用户自己那条、别的题的、根本不存在的 —— 都不动库、不叫模型。
func TestRegenerateRejectsAnythingButTheLastAnswer(t *testing.T) {
	h := newHarness(t, "答一", "答二")
	q := h.addQuestion(t, 0x63)
	other := h.addQuestion(t, 0x64)

	first := h.ask(t, q.ID, "第一问")
	second := h.ask(t, q.ID, "第二问")
	elsewhere := h.ask(t, other.ID, "别的题的问题")

	before := texts(h.history(t, q.ID))
	calls := h.fake.ChatCount()

	cases := []struct {
		name string
		id   int64
		want error
	}{
		// 它后面还有一轮 —— 重说它就得顺手丢掉那一轮，而那是用户没要求过的删除。
		{"中间那条回答", first.Reply.ID, discussion.ErrNotRegeneratable},
		// 用户自己说的那句：没有「重说」这回事。
		{"用户那条", second.Question.ID, discussion.ErrNotRegeneratable},
		// 别的题的回答：这里是 id 与题对不上的那一关。
		{"别的题的回答", elsewhere.Reply.ID, discussion.ErrNoSuchMessage},
		{"不存在的 id", 9999, discussion.ErrNoSuchMessage},
	}
	for _, c := range cases {
		if _, err := h.svc.Regenerate(q.ID, c.id); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v，想要 %v", c.name, err, c.want)
		}
	}

	if got := texts(h.history(t, q.ID)); !slices.Equal(got, before) {
		t.Errorf("被拒之后这道题的讨论变成 %v，想要原样不动 %v", got, before)
	}
	// 别的题也一个字没动。
	if n := h.countRows(t, other.ID, elsewhere.Reply.ID); n != 1 {
		t.Errorf("别的题的回答（id=%d）在库里有 %d 行，想要 1：一条按 id 就删的记录会删到别的题上去",
			elsewhere.Reply.ID, n)
	}
	if got := h.fake.ChatCount(); got != calls {
		t.Errorf("被拒的这几次也叫了 %d 次模型（之前 %d 次）：位置不对就不该去问", got, calls)
	}
}

// 没讨论过的题：没有那条记录可重说。题不存在则按写操作的规矩报 ErrNotFound。
func TestRegenerateOnEmptyDiscussion(t *testing.T) {
	h := newHarness(t, "不该被问到")
	q := h.addQuestion(t, 0x65)

	if _, err := h.svc.Regenerate(q.ID, 1); !errors.Is(err, discussion.ErrNoSuchMessage) {
		t.Errorf("err = %v，想要 ErrNoSuchMessage", err)
	}
	if _, err := h.svc.Regenerate(9999, 1); !errors.Is(err, library.ErrNotFound) {
		t.Errorf("题不存在时 err = %v，想要 library.ErrNotFound", err)
	}
	if n := h.fake.ChatCount(); n != 0 {
		t.Errorf("这两种情况也叫了 %d 次模型", n)
	}
}

// ── 编辑并重发 ──

// 改掉一句之后：旧的那句不在了、新的在，它后面那一截也一并作废。
func TestEditAndResendReplacesTheMessageAndDropsTheTail(t *testing.T) {
	h := newHarness(t, "答一", "答二", "改过之后的新回答")
	q := h.addQuestion(t, 0x66)

	first := h.ask(t, q.ID, "第一问")
	h.ask(t, q.ID, "第二问")

	got, err := h.svc.EditAndResend(q.ID, first.Question.ID, "改过的第一问")
	if err != nil {
		t.Fatalf("EditAndResend: %v", err)
	}

	want := []string{"user:改过的第一问", "assistant:改过之后的新回答"}
	if !slices.Equal(texts(got), want) {
		t.Errorf("改过之后的讨论 = %v，想要 %v", texts(got), want)
	}
	if !slices.Equal(texts(h.history(t, q.ID)), want) {
		t.Errorf("回看这道题 = %v，想要 %v", texts(h.history(t, q.ID)), want)
	}

	// 旧的那句与它后面那一整截都不在库里了：这道题只剩两行。
	if n := h.rowsOfQuestion(t, q.ID); n != 2 {
		t.Errorf("这道题在库里有 %d 行，想要 2 行（改过的那句 + 新的回答）", n)
	}
	if n := h.countRows(t, q.ID, first.Question.ID); n != 0 {
		t.Errorf("旧的那句（id=%d）还在库里 %d 行，想要 0", first.Question.ID, n)
	}

	// 时间戳是钉死的（每条都一样），顺序只可能来自 id —— 所以 id 必须是递增的，
	// 而新那一行**不能**回收旧行的 id（AUTOINCREMENT 管这件事，这里钉住它）。
	for i, m := range got {
		if i > 0 && m.ID <= got[i-1].ID {
			t.Errorf("第 %d 条的 id = %d，不比前一条 %d 大：顺序全靠它", i, m.ID, got[i-1].ID)
		}
	}
	if got[0].ID <= first.Question.ID {
		t.Errorf("改过那句的 id = %d，不比原来那句 %d 大：id 不回收", got[0].ID, first.Question.ID)
	}

	// 发出去的是改过的那段对话（不含任何旧字）。
	sent := h.fake.Chats[len(h.fake.Chats)-1]
	went := make([]string, 0, len(sent.Messages))
	for _, m := range sent.Messages {
		went = append(went, string(m.Role)+":"+m.Text)
	}
	if !slices.Equal(went, []string{"user:改过的第一问"}) {
		t.Errorf("发出去的对话 = %v，想要只有改过的那一句", went)
	}
}

// **这一条是不变量本身**：模型失败时，用户刚改的那句字还在库里。
//
// 换掉 = 先落库（一个事务里删掉旧的、写下新的），再去问模型 —— 与 Ask 同一条顺序。
// 先问后写的实现会在这一格上红：改完字没了，用户连原来那句都回不去。
func TestEditAndResendKeepsTheNewTextWhenTheModelFails(t *testing.T) {
	h := newHarness(t, "答一", "答二")
	q := h.addQuestion(t, 0x67)

	first := h.ask(t, q.ID, "第一问")
	h.ask(t, q.ID, "第二问")

	h.fake.ChatErr = errors.New("网络不通")
	if _, err := h.svc.EditAndResend(q.ID, first.Question.ID, "改过的第一问"); err == nil {
		t.Fatal("想要一个错，拿到 nil")
	}

	// 后半个对话（第二问与它的回答）作废 —— 那是这个动作本身要丢的东西。
	// 但改过的那句**在**，而且就是它。
	want := []string{"user:改过的第一问"}
	if got := texts(h.history(t, q.ID)); !slices.Equal(got, want) {
		t.Errorf("失败之后回看这道题 = %v，想要 %v", got, want)
	}
	// 直接读那一行的正文：不是「界面上看着像」，是库里存的就是这句。
	var stored string
	if err := h.lib.DB().QueryRow(
		`SELECT text FROM discussions WHERE question_id = ? AND role = 'user'`, q.ID,
	).Scan(&stored); err != nil {
		t.Fatalf("读回改过的那句: %v", err)
	}
	if stored != "改过的第一问" {
		t.Errorf("库里存的是 %q，想要 %q", stored, "改过的第一问")
	}
}

// 只能改用户自己那句；空话不落库、也不动库；id 与题对不上要拦下。
func TestEditAndResendRejectsBadTargets(t *testing.T) {
	h := newHarness(t, "答一")
	q := h.addQuestion(t, 0x68)
	other := h.addQuestion(t, 0x69)

	turn := h.ask(t, q.ID, "这步为什么")
	elsewhere := h.ask(t, other.ID, "别的题的问题")

	before := texts(h.history(t, q.ID))
	calls := h.fake.ChatCount()

	cases := []struct {
		name string
		id   int64
		text string
		want error
	}{
		{"模型那条", turn.Reply.ID, "想改模型的话", discussion.ErrNotUserMessage},
		{"别的题那句", elsewhere.Question.ID, "想改别的题", discussion.ErrNoSuchMessage},
		{"不存在的 id", 9999, "想改不存在的", discussion.ErrNoSuchMessage},
		// 空话先被拦住，库一个字都不动 —— 连「删掉旧的」那一步都不该发生。
		{"空话", turn.Question.ID, "   ", discussion.ErrEmptyMessage},
	}
	for _, c := range cases {
		if _, err := h.svc.EditAndResend(q.ID, c.id, c.text); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v，想要 %v", c.name, err, c.want)
		}
	}

	if got := texts(h.history(t, q.ID)); !slices.Equal(got, before) {
		t.Errorf("被拒之后这道题的讨论变成 %v，想要原样不动 %v", got, before)
	}
	if n := h.countRows(t, other.ID, elsewhere.Question.ID); n != 1 {
		t.Errorf("别的题那句（id=%d）在库里有 %d 行，想要 1", elsewhere.Question.ID, n)
	}
	if got := h.fake.ChatCount(); got != calls {
		t.Errorf("被拒的这几次也叫了 %d 次模型（之前 %d 次）", got, calls)
	}

	// 题不存在：写操作的规矩是 ErrNotFound，而不是一个悄悄成功的空操作。
	if _, err := h.svc.EditAndResend(9999, 1, "随便"); !errors.Is(err, library.ErrNotFound) {
		t.Errorf("题不存在时 err = %v，想要 library.ErrNotFound", err)
	}
}

// 改完能接着往下聊：改过的那条是新的历史，不是一段接不上的孤岛。
func TestEditAndResendCanBeFollowedByAnotherAsk(t *testing.T) {
	h := newHarness(t, "答一", "改过之后的新回答", "接着聊的回答")
	q := h.addQuestion(t, 0x6a)

	first := h.ask(t, q.ID, "第一问")
	if _, err := h.svc.EditAndResend(q.ID, first.Question.ID, "改过的第一问"); err != nil {
		t.Fatalf("EditAndResend: %v", err)
	}
	h.ask(t, q.ID, "第二问")

	want := []string{
		"user:改过的第一问",
		"assistant:改过之后的新回答",
		"user:第二问",
		"assistant:接着聊的回答",
	}
	if got := texts(h.history(t, q.ID)); !slices.Equal(got, want) {
		t.Errorf("接着聊之后 = %v，想要 %v", got, want)
	}
	// 第二次问发出去的上下文也得是改过之后的那一段。
	sent := h.fake.Chats[len(h.fake.Chats)-1]
	if len(sent.Messages) != 3 || sent.Messages[0].Text != "改过的第一问" {
		t.Errorf("接着聊时发出去的对话 = %+v，想要以改过的那句开头", sent.Messages)
	}
}

// ── 测试用的小工具 ──

// countRows 数某一条记录（按 id）在库里还有几行。
func (h *harness) countRows(t *testing.T, questionID, id int64) int {
	t.Helper()

	var n int
	if err := h.lib.DB().QueryRow(
		`SELECT count(*) FROM discussions WHERE question_id = ? AND id = ?`, questionID, id,
	).Scan(&n); err != nil {
		t.Fatalf("数记录 %d: %v", id, err)
	}
	return n
}

// rowsOfQuestion 数一道题的讨论在库里共有几行。
func (h *harness) rowsOfQuestion(t *testing.T, questionID int64) int {
	t.Helper()

	var n int
	if err := h.lib.DB().QueryRow(
		`SELECT count(*) FROM discussions WHERE question_id = ?`, questionID,
	).Scan(&n); err != nil {
		t.Fatalf("数这道题的记录: %v", err)
	}
	return n
}
