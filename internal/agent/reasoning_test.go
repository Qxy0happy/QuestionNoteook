package agent_test

import (
	"testing"

	"questionbook/internal/vlm"
)

// 这一份是「把思考过程带回来给界面看」在 agent 这一侧的那一半。
//
// 与讨论那边不同的只有一处：agent 是工具调用循环，一次提问要跟模型往返好几轮，
// 而**每一轮的 assistant 消息都自带一段 reasoning_content**（要工具的那几轮也有）。
// 所以这里要钉的是**拼法**：按轮次先后接起来、中间空一行，没有思考的那一轮不留空行。
//
// 它同样不落库 —— agent 这几轮的消息本来就没进过库（见 Answer.Reasoning）。

// 要工具的那几轮与最后那轮拼在一起；中间没吐思考的那一轮不留空行。
func TestReasoningMergesAcrossToolRounds(t *testing.T) {
	h := newHarness(t)
	h.seed(t)
	h.fake.Turns = []vlm.Reply{
		// 前后带空白：每一段贴上来之前都收拾过（trim 在 converse 里做）。
		{ToolCalls: []vlm.ToolCall{call("c1", "list_tags", "{}")}, Reasoning: "  先看看标签树长什么样。\n"},
		// 中间这一轮模型没吐思考过程 —— 拼出来不该多一段空行。
		{ToolCalls: []vlm.ToolCall{call("c2", "list_questions", "{}")}},
		{Text: "你数学那块挂了两道题。", Reasoning: "看完了，可以照着这两样答。"},
	}

	a := h.ask(t, "我数学哪块最弱")

	if a.Rounds != 3 {
		t.Fatalf("Rounds = %d，想要 3（两轮工具 + 一轮回答）", a.Rounds)
	}
	want := "先看看标签树长什么样。\n\n看完了，可以照着这两样答。"
	if a.Reasoning != want {
		t.Errorf("思考过程 = %q\n            想要 %q", a.Reasoning, want)
	}
}

// 单轮就收的提问：那一段照旧带回来。
func TestReasoningComesBackFromASingleRound(t *testing.T) {
	h := newHarness(t)
	h.seed(t)
	h.fake.Turns = []vlm.Reply{{Text: "你有 2 道错题。", Reasoning: "数一遍就够了。"}}

	a := h.ask(t, "我有几道错题")

	if a.Reasoning != "数一遍就够了。" {
		t.Errorf("思考过程 = %q，想要那一段", a.Reasoning)
	}
}

// 模型没开思考模式（或者这一轮没吐）时是空串，不是一串空行。
func TestReasoningIsEmptyWhenTheModelDidNotThink(t *testing.T) {
	h := newHarness(t)
	h.seed(t)
	h.fake.Turns = []vlm.Reply{say("你有 2 道错题。")}

	if a := h.ask(t, "我有几道错题"); a.Reasoning != "" {
		t.Errorf("思考过程 = %q，想要空串", a.Reasoning)
	}
}
