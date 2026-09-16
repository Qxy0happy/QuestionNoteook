package agent_test

import (
	"errors"
	"strings"
	"testing"

	"questionbook/internal/agent"
	"questionbook/internal/vlm"
)

// 这一份看的是 agent 这一侧的流式：一次提问往往来回好几轮（先查数据、再说话），
// 而**每一轮**的增量都播到同一个号上 —— 工具调用循环对用户来说就是同一轮，
// 他不会知道里面到底往返了几次。
//
// 「一次网络都不打」照旧：模型那一层是 vlm.Fake；「播出一片」那一步换成往切片里写
// （真实的播法是往 Wails 发事件，测试里没有应用，也不该起一个）。

// recorded 记下播出去的一片：带的是哪个号、那一片是什么。
type recorded struct {
	streamID string
	d        vlm.Delta
}

// recorder 造一个收片的函数与它写进去的那个切片。
func recorder() (func(string, vlm.Delta), *[]recorded) {
	var sent []recorded
	return func(id string, d vlm.Delta) {
		sent = append(sent, recorded{streamID: id, d: d})
	}, &sent
}

// 每一轮的增量都在同一个号上，拼起来等于最终那条回答。
//
// 第一轮是「要查数据」那一轮：它没有说话，所以只有一片思考过程（空正文）——
// 那一片也得播出去并带上同一个号，否则界面上会先冒出一段话、思考过程要等下一轮才出现。
func TestAskStreamsEveryRoundUnderOneStreamID(t *testing.T) {
	emit, sent := recorder()
	h := newHarness(t, agent.WithEmitter(emit))
	h.seed(t)
	h.fake.Turns = []vlm.Reply{
		{ToolCalls: []vlm.ToolCall{call("c1", "list_tags", "{}")}, Reasoning: "先看看标签树长什么样。"},
		say("数学那边有两道到期，先把它们做了。"),
	}

	a := h.ask(t, "我数学哪块最弱")

	if a.Rounds != 2 {
		t.Fatalf("往返了 %d 轮，想要 2 轮", a.Rounds)
	}
	if a.Text != "数学那边有两道到期，先把它们做了。" {
		t.Errorf("回答 = %q", a.Text)
	}
	// 两轮都走了流式：回落那条路一次都没走（走没走看 StreamCount，成没成看 ChatCount）。
	if n := h.fake.StreamCount(); n != 2 {
		t.Errorf("流式被尝试了 %d 次，想要 2 次（每一轮一次）", n)
	}
	if n := h.fake.ChatCount(); n != 2 {
		t.Errorf("与模型往返了 %d 次，想要 2 次", n)
	}

	if len(*sent) != 2 {
		t.Fatalf("播了 %d 片，想要 2 片（每轮一片）：%+v", len(*sent), *sent)
	}
	for i, one := range *sent {
		if one.streamID != h.streamID {
			t.Errorf("第 %d 片带的号是 %q，想要 %q", i, one.streamID, h.streamID)
		}
	}

	// 拼起来 == 最终那条回答：界面上正流着的那一份，与最后落定的那一份得对得上。
	var texts, reasons strings.Builder
	for _, one := range *sent {
		texts.WriteString(one.d.Text)
		reasons.WriteString(one.d.Reasoning)
	}
	if texts.String() != a.Text {
		t.Errorf("分片拼出来的正文 = %q，最终那条回答 = %q", texts.String(), a.Text)
	}
	if reasons.String() != a.Reasoning {
		t.Errorf("分片拼出来的思考过程 = %q，Answer.Reasoning = %q", reasons.String(), a.Reasoning)
	}
}

// 流式坏了**静默回落**：回答照旧拿得到 —— 用户不该因为流式出问题就问不出话。
// 代价如实记下：每一轮都会多花一份完整请求（流式那几次的钱白花了）。
func TestAskFallsBackSilentlyWhenStreamingFails(t *testing.T) {
	emit, sent := recorder()
	h := newHarness(t, agent.WithEmitter(emit))
	h.seed(t)
	h.fake.Turns = []vlm.Reply{
		calls(call("c1", "list_tags", "{}")),
		say("数学那边有两道到期，先把它们做了。"),
	}
	h.fake.StreamErr = errors.New("流到一半断了")

	a := h.ask(t, "我数学哪块最弱")

	if a.Text != "数学那边有两道到期，先把它们做了。" {
		t.Errorf("回答 = %q，想要回落那条路拿到的那一句", a.Text)
	}
	if a.Rounds != 2 {
		t.Errorf("往返了 %d 轮，想要 2 轮", a.Rounds)
	}
	// 两轮都是「先试流式、失败了、回落一次」。
	if n := h.fake.StreamCount(); n != 2 {
		t.Errorf("流式被尝试了 %d 次，想要 2 次", n)
	}
	if n := h.fake.ChatCount(); n != 2 {
		t.Errorf("与模型往返了 %d 次，想要 2 次", n)
	}
	if len(*sent) != 0 {
		t.Errorf("失败的那两次流式仍然播了 %d 片：%+v", len(*sent), *sent)
	}
}

// 界面没给号时连流都不试：没有号，认不出哪些分片是自己的（见 vlm.NewSink）。
func TestAskWithoutAStreamIDNeverTriesStreaming(t *testing.T) {
	emit, sent := recorder()
	h := newHarness(t, agent.WithEmitter(emit))
	h.seed(t)
	h.fake.Turns = []vlm.Reply{say("今天没有到期的。")}
	h.streamID = ""

	a := h.ask(t, "今天有几道到期")

	if a.Text != "今天没有到期的。" {
		t.Errorf("回答 = %q", a.Text)
	}
	if n := h.fake.StreamCount(); n != 0 {
		t.Errorf("没有号却试了 %d 次流式", n)
	}
	if len(*sent) != 0 {
		t.Errorf("没有号却播了 %d 片：%+v", len(*sent), *sent)
	}
}
