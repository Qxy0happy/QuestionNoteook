// Package vlm 是「跟云端多模态模型说话」的那一层。
//
// 它分成两半，各自只有一个职责：
//
//   - **Provider** —— 一家模型服务怎么说话：端点、报文形状、传图的方式。换一家只换实现。
//     这是 ADR-0005 的硬要求：所选视觉模型带 Exp 后缀、官方明说可能被修订或替换，
//     所以模型名一律从配置来，**代码里一个默认模型 ID 都不许出现**。
//   - **Service** —— 「给一道错题打标签」这件事本身：取题图、拼提示词、解析回答、
//     用户确认后把标签落成一棵真的树。它只认 Provider 那个接口，于是测试里塞一个假的
//     进去就能把整条流程跑完，**一次网络都不打**（票据的硬要求）。
//
// 契约的复核结论（对着官方文档逐条核过，与写票时的转述有出入，以官方为准）：
//
//   - 模型名：现在的视觉模型就叫 deepseek-flash；deepseek-v4-flash-vision-exp 是退役的旧名，
//     服务方仍然接受、由最新的 Flash 模型来应答。两个名字都不写进代码 —— 它们属于配置。
//   - detail：只对 image_url（内联/外链）那种传法生效，取值 low / high / original / auto，
//     默认 auto（服务方当前等同于 original）。**用 file_id 传时它被忽略** ——
//     所以「走 Files API」与「显式要高保真」这两条不是同一层上的事：前者换的是传输方式，
//     后者只在内联那条路上表达得出来。见 DeepSeek.imageBlock。
//   - 计费口径：官方说是**单图最多 1024 token**（写票时记的 384 只出现在第三方转述里）。
//     这不影响实现，只影响对成本的预期。
//   - 图片只能放在 user 消息里，进 system / assistant 直接 400。
package vlm

import (
	"context"
	"errors"
)

// Role 是一条消息的角色。取值直接就是线上报文里的样子（user / assistant / system），
// 不另立一套自己的枚举再到实现里翻译一遍 —— 这一层抽象要挡的差异不在这里。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Image 是要交给模型看的一张图。
type Image struct {
	// Hash 是这张图在本地的内容 hash（ADR-0004 的名字：图片按内容 hash 命名）。
	// 它是「这张图传上去过没有」的键 —— 同一张题图会在打标签与讨论两条路径上反复用到，
	// Files API 的全部意义就是别传第二遍。
	Hash string

	// Data 是图片的原始字节。这里恒为 PNG：题图落盘就是 PNG，读回来的也是 PNG。
	Data []byte

	// MIME 是它的媒体类型，形如 image/png。
	MIME string

	// FileID 非空表示这张图已经传到服务方了，这次用引用而不是内联 base64。
	// 由调用方（Service）填：怎么拿到这个引用的（缓存命中 / 刚传的 / 传不上去）是**策略**，
	// Provider 只负责照着它决定报文长什么样。
	FileID string
}

// Message 是一条消息。
type Message struct {
	Role Role
	Text string
	// Images 只能挂在 user 消息上 —— 服务方的硬约束：放进 system 或 assistant 直接 400。
	// 类型上没拦这一条（拦了就得为每种能带图的角色各造一个类型），由 Service 拼消息时守住。
	Images []Image
}

// Request 是一次对话请求。
type Request struct {
	// Model 由调用方从配置里取好传进来：要图的活给视觉模型，纯文本的活给文本模型。
	// Provider 不自己挑模型 —— 挑模型是策略，配置在哪一层由 Service 说了算。
	Model string

	// System 是系统提示词。**图片绝不能出现在这里。**
	System string

	// Messages 是对话正文。
	Messages []Message

	// JSON 为真时要求服务方以 JSON 对象作答。打标签这条路径要的就是它：
	// 回答得能解析成结构化标签，不能是一段散文。
	JSON bool
}

// Reply 是模型的一次回答。
type Reply struct {
	Text string
}

// Provider 是一家模型服务。
//
// 刻意只有两个动作：把一张图交上去换一个可复用的引用，以及发一次对话。
// 端点、凭据、模型名都不在这里 —— 它们从配置来，具体实现（DeepSeek）构造时吃下它们。
type Provider interface {
	// Upload 把一张图交给服务方的文件接口，返回一个可跨请求复用的引用。
	// 服务方没有这个能力时返回 ErrNoUpload，调用方据此回落到内联 base64。
	Upload(ctx context.Context, img Image) (string, error)

	// Chat 发一次对话，返回模型的文本回答。
	Chat(ctx context.Context, req Request) (Reply, error)
}

var (
	// ErrNoUpload 表示这家 provider 没有文件接口这一路，图片只能内联传。
	ErrNoUpload = errors.New("VLM: 这家 provider 不支持文件上传")

	// ErrNotConfigured 表示还没配置 VLM：配置文件不在，或者必填项是空的。
	//
	// 它是一个**正常状态**而不是故障：没配置时应用照常启动、照常拍照入库，
	// 只有「让 VLM 打标签」这一个动作会失败 —— 票据要求标签功能不挡拍摄与入库。
	ErrNotConfigured = errors.New("VLM: 还没配置，无法调用模型")

	// ErrBadReply 表示模型答得不合格：回的不是能解析的 JSON。
	// 它只用来判断「这次的回答没形状」，怎么呈现给用户由 Service 决定
	// （原话会连同它一起交出去，票据 01 调提示词全靠那份原话）。
	ErrBadReply = errors.New("VLM: 模型的回答不是能用的 JSON")
)
