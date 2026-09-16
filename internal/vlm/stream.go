package vlm

import "context"

// 这一份是「让回答一个字一个字地到界面上」那条路。
//
// 契约是对着官方文档核过的（api-docs.deepseek.com 的 create-chat-completion 一页，
// 以及 guides/thinking_mode_api_example_streaming）。要点四条，实现按它写：
//
//   - 请求里加 stream: true，回应就从一整条 JSON 变成 text/event-stream。
//   - 每一片是一行 `data: {...}`，事件之间空行分隔，结束标记是单独一行 `data: [DONE]`。
//     （SSE 的行格式是通用的那套：前缀 `data: `、以 `:` 开头的注释行、空行分隔事件。）
//   - **思考模式下 reasoning 也一起流**：delta.reasoning_content 与 delta.content 都在
//     choices[0].delta 上，推理那一段先来、正文随后。官方那份流式样例就是拿这两个字段
//     各攒一份缓冲、并且刻意不合并它们 —— 这里照做（Reply 上两个字段照旧分开）。
//   - 工具调用也在 delta 里，但**是碎片**：第一片带 id / type / function.name，
//     后面几片只给 function.arguments 的一段。所以拼的时候要按下标攒，见 absorb。
//
// 为什么它不做成 Provider 上的第三个方法：这两条路是二选一 —— 流不动就回落（ChatStreaming），
// 而「这家 provider 一定会流」不是任何服务方能给的保证。
type Streamer interface {
	// ChatStream 发一次对话，边收边把分片交给 onDelta，返回**拼好的整条**。
	//
	// onDelta 可能是 nil（调用方只想要那条整的，不想被逐片打扰）。
	// 分片按到达先后一片一次地调，顺序与它们拼起来的那个顺序一致。
	ChatStream(ctx context.Context, req Request, onDelta Sink) (Reply, error)
}

// Chatter 是「能发一次对话」这一面。Provider 与 agent 那边的 Model 都满足它 ——
// ChatStreaming 要的只是这一面，于是两条路都能用它，回落那条规矩也就只有一个出处。
type Chatter interface {
	Chat(ctx context.Context, req Request) (Reply, error)
}

// Sink 收下一片增量，把它播到该去的地方（界面上、或者测试里的一个切片）。
type Sink func(Delta)

// Delta 是流式回答里的一片增量。
//
// 两个字段都可能为空：服务方偶尔会发只有 role 或者只有 finish_reason 的空片。
// 一片里两路都有是**合法**的（类型上允许），所以谁收下它，谁负责按路归位。
type Delta struct {
	// Text 是这一片新增的回答正文（线上是 delta.content）。
	Text string
	// Reasoning 是这一片新增的思考过程（线上是 delta.reasoning_content）。
	Reasoning string
}

// NewSink 把「这一次调用的 streamID」与「把一片播出去的那一步」绑在一起。
//
// streamID 为空（或者根本没有播的出口）时返回 nil —— 调用方据此**压根不走流式**：
// 没有 id，前端认不出哪些分片是自己的（见 Discussion.svelte 里按 stream_id 筛的那一段）。
func NewSink(streamID string, emit func(streamID string, d Delta)) Sink {
	if streamID == "" || emit == nil {
		return nil
	}
	return func(d Delta) { emit(streamID, d) }
}

// ChatStreaming 发一次对话：能流就流，流不动就**静默回落**到非流式。
//
// 回落这条规矩只有这一个出处（讨论与 agent 两条路都走它）。为什么要有它：
// 用户不该因为流式坏了就发不出问题 —— 流式是锦上添花，回答问题才是那件事本身。
// 于是这里的错一律咽下，交给 Chat 再试一次；Chat 也失败，那个错才冒上去。
//
// 代价如实写下：流到一半断掉时，这一次会**再发一份完整请求**（多花一次钱）。
// 已经流出去的那半截还留在界面上 —— 调用方在拿到返回值（或错误）时要用它整条替换掉，
// 不然屏幕上会是一段半截话（见 Discussion.svelte / AgentChat.svelte 的 live 那一块）。
func ChatStreaming(ctx context.Context, p Chatter, req Request, onDelta Sink) (Reply, error) {
	if st, ok := p.(Streamer); ok && onDelta != nil {
		if reply, err := st.ChatStream(ctx, req, onDelta); err == nil {
			return reply, nil
		}
	}
	return p.Chat(ctx, req)
}
