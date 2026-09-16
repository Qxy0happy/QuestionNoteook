package discussion_test

import (
	"slices"
	"testing"

	"questionbook/internal/discussion"
	"questionbook/internal/vlm"
)

// 这一份是「把思考过程带回来给界面看」那一条。
//
// 要守的是一件不大但容易做错的事：思考过程**只在产生它的那一次调用的返回值里**，
// 不落库 —— 所以下面每条断言都是成对的：返回值上看得见，回库一读就没有。
// 只断言前一半的话，「顺手存一下」这种改法照样能过，而库里那份会悄悄涨一倍。

// reasonings 把一段记录的思考过程摊出来（按顺序），空的也照摊 ——
// 「谁有谁没有」正是这几条测试要看的东西。
func reasonings(msgs []discussion.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Reasoning)
	}
	return out
}

// 一轮问答：模型那条带着思考过程回来，而它不进库。
func TestAskReturnsReasoningButDoesNotStoreIt(t *testing.T) {
	h := newHarness(t)
	q := h.addQuestion(t, 0x70)
	// 前后带空白：贴到界面上的是收拾干净的那一份（trim 在 Service 那一层做）。
	h.fake.Turns = []vlm.Reply{{Text: "这一步用了拉格朗日中值定理。", Reasoning: "\n  先把 f 在两端点上看一眼。\n"}}

	turn := h.ask(t, q.ID, "这步为什么")

	if got, want := turn.Reply.Reasoning, "先把 f 在两端点上看一眼。"; got != want {
		t.Errorf("这一轮回答的思考过程 = %q，想要 %q", got, want)
	}
	// 用户那条没有思考过程，这是模型才有的东西。
	if turn.Question.Reasoning != "" {
		t.Errorf("用户那条带着思考过程 %q —— 它只可能长在模型的回答上", turn.Question.Reasoning)
	}

	// 回库再读：一个都不剩。（这一条同时钉住「换个时间打开这道题就看不到上一轮是怎么想的」
	// 那件事是**有意的**，不是漏存 —— 见 Message.Reasoning 上那段说明。）
	got := reasonings(h.history(t, q.ID))
	if !slices.Equal(got, []string{"", ""}) {
		t.Errorf("回看这道题时每条记录的思考过程 = %q，想要两条都是空的（它不落库）", got)
	}
	// 而回答本身照旧在 —— 上面那句「空」不是因为它把整条记录弄丢了。
	if lines := texts(h.history(t, q.ID)); !slices.Equal(lines, []string{"user:这步为什么", "assistant:这一步用了拉格朗日中值定理。"}) {
		t.Errorf("回看这道题 = %v，想要用户那句与模型那句都在", lines)
	}
}

// 重新生成：思考过程贴在这一轮**新生成的那条回答**上（历史里的最后一条）。
func TestRegenerateCarriesReasoningOnTheNewAnswer(t *testing.T) {
	h := newHarness(t, "第一次回答")
	q := h.addQuestion(t, 0x71)

	turn := h.ask(t, q.ID, "这步为什么")
	if turn.Reply.Reasoning != "" {
		t.Fatalf("第一次回答就带着思考过程 %q —— 这条测试要的是「重说那一轮才有」", turn.Reply.Reasoning)
	}

	h.fake.Turns = []vlm.Reply{{Text: "第二次回答", Reasoning: "换一条路子：先配方再求导。"}}
	got, err := h.svc.Regenerate(q.ID, turn.Reply.ID, h.streamID)
	if err != nil {
		t.Fatalf("Regenerate: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("重说之后有 %d 条记录，想要 2 条：%v", len(got), texts(got))
	}
	if got[len(got)-1].Reasoning != "换一条路子：先配方再求导。" {
		t.Errorf("最后一条的思考过程 = %q，想要这一次重说的那一段", got[len(got)-1].Reasoning)
	}
	if got[0].Reasoning != "" {
		t.Errorf("用户那条带着思考过程 %q", got[0].Reasoning)
	}
	// 库里那份照旧是空的。
	if after := reasonings(h.history(t, q.ID)); !slices.Equal(after, []string{"", ""}) {
		t.Errorf("回看这道题时每条记录的思考过程 = %q，想要两条都是空的", after)
	}
}

// 编辑并重发：同上，贴在这一轮生成的那条回答上。
func TestEditAndResendCarriesReasoningOnTheNewAnswer(t *testing.T) {
	h := newHarness(t, "第一次回答", "第二次回答")
	q := h.addQuestion(t, 0x72)

	turn := h.ask(t, q.ID, "这步为什么")

	// 把提问改一个字再重发。返回的整段里只有新生成的那条回答带思考过程。
	// Turns 是**另起一份**（优先于 Replies，而且从头数起），所以这里只摆这一次要用的那条。
	h.fake.Turns = []vlm.Reply{{Text: "第二次回答", Reasoning: "他改的是问法，答案其实不用变。"}}
	got, err := h.svc.EditAndResend(q.ID, turn.Question.ID, "这一步为什么", h.streamID)
	if err != nil {
		t.Fatalf("EditAndResend: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("重发之后有 %d 条记录，想要 2 条：%v", len(got), texts(got))
	}
	if got[len(got)-1].Reasoning != "他改的是问法，答案其实不用变。" {
		t.Errorf("最后一条的思考过程 = %q，想要这一次重发的那一段", got[len(got)-1].Reasoning)
	}
	if got[0].Reasoning != "" {
		t.Errorf("用户那条带着思考过程 %q", got[0].Reasoning)
	}
	if after := reasonings(h.history(t, q.ID)); !slices.Equal(after, []string{"", ""}) {
		t.Errorf("回看这道题时每条记录的思考过程 = %q，想要两条都是空的", after)
	}
}
