package discussion_test

import (
	"slices"
	"testing"

	"questionbook/internal/vlm"
)

// 流式与非流式**最终写进库里的东西必须一模一样** —— 这一票里最硬的一条要求。
//
// 怎么摆：搭**两套**一模一样的 harness，问同一句话。一套带着流式号（走流式那条路，
// 回答被切成几片播出去），一套把号置空（压根不走流式）。然后对着两边的记录逐条比。
//
// 为什么比库里那两份、而不是比返回值：库里那两份才是用户明天打开这道题会看到的东西。
// 流式那条路上界面多显示了一截（正在流的那一份），而它**一个字都不该进库** ——
// 流的那些片是给眼睛看的，落库用的是整条（见 vlm.Service.chat 的注释）。
func TestStreamedAndPlainAskPersistTheSameThing(t *testing.T) {
	const (
		whole   = "先配方，再求根。"
		thought = "把根号里那一整段看成一个整体。"
		asked   = "这步为什么"
	)

	// 流式那套：正文切成两片，中间还夹着两片思考过程 —— 现实的形状就是这样。
	streamed := newHarness(t, whole)
	// 两层切片：外层一条 = **一次**流式调用（讨论问一句只有一次），里层是这一次要播的那几片。
	streamed.fake.Streams = [][]vlm.Delta{{
		{Reasoning: thought},
		{Text: "先配方，"},
		{Text: "再求根。"},
	}}
	// 非流式那套：号不给，这一整套从头到尾走的是老那条路（等整条回来再返回）。
	plain := newHarness(t, whole)
	plain.streamID = ""

	qs := streamed.addQuestion(t, 0x22)
	qp := plain.addQuestion(t, 0x22)

	gotStreamed := streamed.ask(t, qs.ID, asked)
	gotPlain := plain.ask(t, qp.ID, asked)

	// 先确认摆对了：一套真走了流式，另一套一次都没碰过流式那条路。
	if n := streamed.fake.StreamCount(); n != 1 {
		t.Fatalf("流式那套试了 %d 次流式，想要 1 次", n)
	}
	if n := plain.fake.StreamCount(); n != 0 {
		t.Fatalf("非流式那套碰了 %d 次流式，想要一次都没有", n)
	}

	// 返回的整条回答：两条路一字不差（流式那条返回的就是分片拼出来的那一份）。
	if gotStreamed.Reply.Text != gotPlain.Reply.Text {
		t.Errorf("两条路返回的回答不同：\n流式 %q\n非流式 %q", gotStreamed.Reply.Text, gotPlain.Reply.Text)
	}
	// 思考过程只在这一轮的返回值里有，而且只有流式那条路拿得到 —— 这不算两条路的差异：
	// 它压根不是库里的东西（下一条断言），界面用的是「整条 = 正文 + 思考过程」里前面那半截。
	if gotStreamed.Reply.Reasoning != thought {
		t.Errorf("流式那套返回的思考过程 = %q，想要 %q", gotStreamed.Reply.Reasoning, thought)
	}

	// 库里：逐条一样，而且模型那句是**完整的那条回答**，不是拼到一半的那一截。
	fromStream := texts(streamed.history(t, qs.ID))
	fromPlain := texts(plain.history(t, qp.ID))
	if !slices.Equal(fromStream, fromPlain) {
		t.Errorf("两条路落库的记录不同：\n流式  %v\n非流式 %v", fromStream, fromPlain)
	}
	want := []string{"user:" + asked, "assistant:" + whole}
	if !slices.Equal(fromStream, want) {
		t.Errorf("落库的记录 = %v，想要 %v", fromStream, want)
	}

	// 老规矩再确认一遍：用户那句先落库（所以模型就算失败了它也还在），
	// 思考过程两条路都**不落库**（读回来的一律是空串）。
	for _, m := range append(streamed.history(t, qs.ID), plain.history(t, qp.ID)...) {
		if m.Reasoning != "" {
			t.Errorf("库里那条 %q 带着思考过程：%q", m.Text, m.Reasoning)
		}
	}
}
