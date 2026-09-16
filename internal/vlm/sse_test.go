package vlm

import (
	"strings"
	"testing"
)

// 这一份只碰两件事：**把一串 SSE 解析成 Reply**（readStream），以及**报文里那个 stream 开关**
// （chatBody）。两者都是纯函数，喂一个 strings.Reader 或一个 Request 进去就够 ——
// **一次网络都不打**，连一个监听端口都不开。
//
// 为什么它是白盒（package vlm）：要验的正是「拼装」那一段本身，而它在对外那一面里看不见。
// 真实 provider 与真机那一侧的事（头有没有带对、地址拼得对不对）不在这一份里，也验不了
// （见 sse_test 上面那条注释与汇报里「没验到的」那一段）。

// sseBody 是一整段逼真的流式回应：含注脚行、event: 行、只有 role 的空片、
// 正文与思考过程交错的片、按 index 分片到达的工具调用、以及结尾的 [DONE]。
// 行尾用 \r\n：服务方与中间那些代理都可能这么发，Scanner 得照收。
const sseBody = ": keep-alive\r\n" +
	"\r\n" +
	"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\r\n" +
	"\r\n" +
	"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"先看它像什么。\"}}]}\r\n" +
	"\r\n" +
	"event: ping\r\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\"先配方，\"}}]}\r\n" +
	"\r\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\"再求根。\",\"reasoning_content\":\"两边同时加一个数。\"}}]}\r\n" +
	"\r\n" +
	"data: {\"usage\":{\"total_tokens\":9}}\r\n" +
	"\r\n" +
	"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\"," +
	"\"function\":{\"name\":\"list_tags\",\"arguments\":\"{\\\"a\\\"\"}}]}}]}\r\n" +
	"\r\n" +
	"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\":1}\"}}]}}]}\r\n" +
	"\r\n" +
	"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\r\n" +
	"\r\n" +
	"data: [DONE]\r\n"

// 正文与思考过程各攒一份，**分别**交出去一片一片地播 —— 播出去的那些拼起来，
// 必须与返回的整条一字不差（这是这一票对界面许下的那句话）。
func TestReadStreamReassemblesTheWhole(t *testing.T) {
	var got []Delta
	reply, err := readStream(strings.NewReader(sseBody), func(d Delta) { got = append(got, d) })
	if err != nil {
		t.Fatalf("readStream: %v", err)
	}

	if reply.Text != "先配方，再求根。" {
		t.Errorf("正文 = %q，想要 %q", reply.Text, "先配方，再求根。")
	}
	// 思考过程**不并进正文**：它是另一路（Reply 上两个字段照旧分开）。
	if reply.Reasoning != "先看它像什么。两边同时加一个数。" {
		t.Errorf("思考过程 = %q", reply.Reasoning)
	}

	// 只有真有内容的那几片才播出去：role 片、usage 片、finish_reason 片都是空片。
	want := []Delta{
		{Reasoning: "先看它像什么。"},
		{Text: "先配方，"},
		{Text: "再求根。", Reasoning: "两边同时加一个数。"},
	}
	if len(got) != len(want) {
		t.Fatalf("播了 %d 片，想要 %d 片：%+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 片 = %+v，想要 %+v", i, got[i], want[i])
		}
	}

	// 顺序与拼接：这一条是票据点名的验收项。
	var texts, reasons strings.Builder
	for _, d := range got {
		texts.WriteString(d.Text)
		reasons.WriteString(d.Reasoning)
	}
	if texts.String() != reply.Text {
		t.Errorf("分片拼出来的正文 = %q，返回的整条 = %q", texts.String(), reply.Text)
	}
	if reasons.String() != reply.Reasoning {
		t.Errorf("分片拼出来的思考过程 = %q，返回的整条 = %q", reasons.String(), reply.Reasoning)
	}

	// 工具调用是碎片：两片按下标拼回一次调用，函数名与 id 只在第一片里。
	if len(reply.ToolCalls) != 1 {
		t.Fatalf("工具调用 = %+v，想要一次", reply.ToolCalls)
	}
	tc := reply.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "list_tags" || tc.Arguments != `{"a":1}` {
		t.Errorf("拼回来的工具调用 = %+v，想要 call_1 / list_tags / {\"a\":1}", tc)
	}
}

// onDelta 传 nil 也得能跑完（调用方只想要整条、不想被逐片打扰）。
func TestReadStreamWithNilSink(t *testing.T) {
	reply, err := readStream(strings.NewReader(sseBody), nil)
	if err != nil {
		t.Fatalf("readStream: %v", err)
	}
	if reply.Text != "先配方，再求根。" {
		t.Errorf("正文 = %q", reply.Text)
	}
}

// 没读到 [DONE] 就当这次流式失败 —— 半截话不能当成整条答案落库（见 readStream 的注释）。
func TestReadStreamRequiresTheDoneMarker(t *testing.T) {
	cut := strings.TrimSuffix(sseBody, "data: [DONE]\r\n")
	reply, err := readStream(strings.NewReader(cut), nil)
	if err == nil {
		t.Fatal("想要一个错，拿到 nil：少了结束标记就得以失败告终（回落那条路才走得通）")
	}
	// 已经收到的那半截一个字都不往外交 —— 调用方要的是整条，拼半截是它自己的事。
	if reply.Text != "" || len(reply.ToolCalls) != 0 {
		t.Errorf("失败时返回了半截：%+v", reply)
	}
}

// 分片不是合法 JSON：报错（同样交给回落那条路），不要把坏数据咽下去。
func TestReadStreamRejectsABrokenChunk(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"好的一半\"}}]}\n\n" +
		"data: {这不是 JSON\n\n" +
		"data: [DONE]\n"
	if _, err := readStream(strings.NewReader(body), nil); err == nil {
		t.Fatal("想要一个错，拿到 nil")
	}
}

// sseData 只认 data 行：注脚行、event / id / retry 都不是增量，空行只是分隔。
func TestSSEDataTakesOnlyDataLines(t *testing.T) {
	cases := []struct {
		line string
		ok   bool
		want string
	}{
		{`data: {"a":1}`, true, `{"a":1}`},
		// 冒号后那一个空格是**可选**的装饰：两种写法都得认。
		{`data:{"a":1}`, true, `{"a":1}`},
		{`data: [DONE]`, true, "[DONE]"},
		{"data:", true, ""},
		{": keep-alive", false, ""},
		{"event: ping", false, ""},
		{"", false, ""},
		{"{\"a\":1}", false, ""},
	}
	for _, c := range cases {
		got, ok := sseData(c.line)
		if ok != c.ok || got != c.want {
			t.Errorf("sseData(%q) = (%q, %v)，想要 (%q, %v)", c.line, got, ok, c.want, c.ok)
		}
	}
}

// 报文那一侧：stream:true 只在流式那条路上出现（其余字段两条路一样）。
func TestChatBodyCarriesTheStreamFlagOnlyWhenAsked(t *testing.T) {
	d := newDeepSeek(Config{BaseURL: "https://x.invalid", APIKey: "k"})
	req := Request{Model: "m", Messages: []Message{{Role: RoleUser, Text: "你好"}}}

	streamed, err := d.chatBody(req, true)
	if err != nil {
		t.Fatalf("chatBody(stream=true): %v", err)
	}
	if !strings.Contains(string(streamed), `"stream":true`) {
		t.Errorf("流式报文里没有 stream:true：%s", streamed)
	}

	plain, err := d.chatBody(req, false)
	if err != nil {
		t.Fatalf("chatBody(stream=false): %v", err)
	}
	// omitempty：非流式那条路上这个字段**根本不该出现**（发一个 stream:false 也行，
	// 但那等于多告诉服务方一件它不需要知道的事）。
	if strings.Contains(string(plain), "stream") {
		t.Errorf("非流式报文里出现了 stream：%s", plain)
	}
}

// 缺模型名时两条路都得当场报 ErrNotConfigured，而不是让服务方替我们挑一个默认模型（ADR-0005）。
func TestChatBodyWithoutAModelName(t *testing.T) {
	d := newDeepSeek(Config{BaseURL: "https://x.invalid", APIKey: "k"})
	for _, stream := range []bool{false, true} {
		_, err := d.chatBody(Request{Messages: []Message{{Role: RoleUser, Text: "你好"}}}, stream)
		if err == nil || !strings.Contains(err.Error(), "模型名") {
			t.Errorf("stream=%v 时 err = %v，想要一句「没给模型名」", stream, err)
		}
	}
}

// 没有 Wails 应用（测试、或者将来别的宿主）时播一片出去**什么也不该发生** ——
// 更不能 panic：拿回答那条路一个字都不受影响（见 events.go）。
func TestEmitDeltaWithoutAnAppIsSilent(t *testing.T) {
	EmitDelta("", Delta{Text: "这一片没有号"})
	EmitDelta("s-1", Delta{Text: "这一片有号，但没有应用"})
	EmitDelta("s-1", Delta{})
}
