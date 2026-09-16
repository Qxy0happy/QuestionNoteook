package vlm

import "github.com/wailsapp/wails/v3/pkg/application"

// 这一份是「把一片增量播到界面上」这一步 —— 本包里唯一碰 Wails 的地方。
//
// 为什么开在这里：产生分片的是 provider（ChatStream），而 provider 只有本包够得着；
// 于是播出去的那一步也只能开在本包。业务逻辑一行都不依赖 Wails ——
// Service.emit 与 agent.Service.emit 都是**字段**，测试里换掉它，整条流式路径照样跑，
// 一条事件都不发。
//
// 事件怎么从 Go 出去（查证结论，wails v3.0.0-beta.22）：
//
//   - Wails 把 app 存成一个包级全局，`application.Get()` 在任何地方都能拿到它
//     （pkg/application/application.go 里 `globalApplication` 与 `func Get()`）。
//     拿到之后 `app.Event.Emit(name, data...)` 就是广播一条自定义事件。
//     所以**不需要**把 app 传进服务、也不需要在接线那一层加参数。
//   - 另一条路是 v3 新加的 Streams（`app.HandleStream` + 前端的 `JSONStream`）：它更像
//     WebSocket、有顺序与背压保证，但注册 handler 必须发生在**拿到 app 之后、跑起来之前**
//     （也就是 main.go 那一层，这一票不动那儿），而且它是为双向长连接设计的。
//     这里要的是「一次调用期间播一串事件」，事件这条路更贴合，也不必改接线。
//
// 代价如实写下：本包因此 import 了 application 这个 UI 框架包。换来的就是上面那条
// 「服务里不用一直揣着一个 app 指针」。测试里 `application.Get()` 返回 nil
// （没有 application.New 过），这时静默不发 —— 见 emit。
const (
	// StreamEvent 是流式增量的事件名。前端按它订阅（Discussion.svelte / AgentChat.svelte）。
	StreamEvent = "vlm:delta"

	// KindText / KindReasoning 是载荷里 kind 的两个取值：这一片是回答正文，还是思考过程。
	// 界面据此把分片归到两块不同的地方（回答那一块、折起来的思考过程那一块）。
	KindText      = "text"
	KindReasoning = "reasoning"
)

// StreamDelta 是一条增量事件的载荷。
//
// 字段名刻意是**扁平的蛇形名**：Wails 把 data 原样 JSON 化交给前端，
// 前端就是拿这三个字段（stream_id / kind / text）把一片归位的。
type StreamDelta struct {
	// StreamID 是这次调用的标识，由**前端**生成、原样传进来（见 Service.Ask 的 streamID）。
	// 一次调用期间播出去的分片全带同一个 id —— 前端按它筛掉不是自己那一次的。
	StreamID string `json:"stream_id"`

	// Kind 是这一片走哪一路：KindText 或 KindReasoning。
	Kind string `json:"kind"`

	// Text 是这一片新增的字。
	Text string `json:"text"`
}

// EmitDelta 把一片增量播给界面。它是 Service / agent 里那个 emit 字段的默认实现。
//
// 一片里两路都可能有：**分开播**，前端只要按 kind 归位就行，不必自己切。
// 两边都空的一片不发 —— 服务方偶尔会发只有 role、或者只有 finish_reason 的空片。
func EmitDelta(streamID string, d Delta) {
	if streamID == "" {
		return
	}
	if d.Reasoning != "" {
		emitDelta(streamID, KindReasoning, d.Reasoning)
	}
	if d.Text != "" {
		emitDelta(streamID, KindText, d.Text)
	}
}

// emitDelta 是真正发那条事件的地方。
func emitDelta(streamID, kind, text string) {
	app := application.Get()
	if app == nil || app.Event == nil {
		// 没有 Wails 应用（测试、或者将来别的宿主）：静默不发。
		// 这不是故障 —— 拿回答那条路（非流式）一个字都不受影响。
		return
	}
	app.Event.Emit(StreamEvent, StreamDelta{StreamID: streamID, Kind: kind, Text: text})
}
