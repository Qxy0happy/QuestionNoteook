package vlm

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"questionbook/internal/library"
)

// Ask 是「就一道错题跟模型说一句」这条接缝，也是讨论那一侧唯一的入口。
//
// 为什么它开在这儿而不是开在讨论包里：这边已经握有那三样东西 —— 配置与 provider 的选法、
// 题图的读法、以及 hash → file_id 的**上传缓存**。讨论每一轮都要把题图（与答案图，如果有）
// 送上去，缓存不共用就等于每轮重传一遍（票据的硬要求）；而重做一遍取配置、拼报文、
// 回落内联那套，只是把同一个东西抄成两份，两边迟早会走偏。
//
// history 是**整段对话**，最后一条必须是要问出口的那句（用户消息）。这一层不动它的顺序与
// 内容，只做两件事：把这一轮的图挂到最后一条 user 消息上，然后发出去。
//
// 图挂在**最后**一条 user 消息上，不挂在第一条：一次请求里只带一份，不随轮数翻倍；
// 它又紧挨着这次的问题，将来做历史截断时也不会因为「开头那条被丢掉」而把图弄丢。
// 代价是每一轮都重发一次图（服务方那边一张图最多算 1024 token），换来的是模型每一轮
// 都真的看着这张图在答。图走的是与打标签**同一份**缓存：同一张题图只上传一次。
//
// 题不存在返回 library.ErrNotFound；没配 VLM 返回 ErrNotConfigured；网络与凭据的问题
// 由 provider 原样报上来。
func (s *Service) Ask(questionID int64, history []Message) (Reply, error) {
	provider, cfg, err := s.current()
	if err != nil {
		return Reply{}, err
	}
	if err := checkHistory(history); err != nil {
		return Reply{}, err
	}

	q, err := s.questions.Get(questionID)
	if err != nil {
		return Reply{}, err
	}
	images, err := s.discussImages(provider, cfg, q)
	if err != nil {
		return Reply{}, err
	}

	return provider.Chat(context.Background(), Request{
		// 讨论必然带图，所以走视觉那一路（见 Config.modelFor）。
		Model:  cfg.modelFor(true),
		System: discussPrompt,
		// JSON 不打开：讨论要的是一段人话，不是能解析的结构。
		Messages: attachImages(history, images),
	})
}

// discussPrompt 是讨论用的系统提示词。
//
// ⚠️ **暂定**，与 prompt.go 里那份打标签提示词同一个处境：措辞还没有在真实题目上试过。
// 定的只有接下来该守的几条（讲思路、别只给结论、拿不准就直说）—— 那几条是这类问答
// 反复出问题的地方，剩下的等真跑过再调。改措辞只改这一个常量。
//
// 与 TagPrompt 不同，它没有配置里的覆盖口子：那一处留口子是因为打标签的质量还没有结论
// （票据 01），而讨论这一条路径连一次真实的问答都还没跑过，先不给一个没人调过的旋钮。
const discussPrompt = `你在帮一个考研学生搞懂他的错题。

他会就一道错题向你提问，随他的消息会附上这道题的题图；如果他拍过答案图，也会一并附上。

回答的要求：
1. 用中文，讲**思路**，不要只丢一个结论或者最终答案。
2. 他问的是某一步时，就着那一步讲，别把整道题从头推一遍。
3. 拿不准的地方直说拿不准，不要编一段看起来对的推导。
4. 他要是问「还有别的解法吗」，给的办法要在这道题上真的能用，不要泛泛而谈。
5. 篇幅跟着问题走：一句能说清就一句话。`

// ErrNoQuestion 表示这次没有要问的话：历史是空的，或者最后一条不是用户说的。
//
// 它挡的是调用方拼错了消息 —— 模型那边收到一段以自己发言结尾的对话，会接着往下编，
// 而那不是用户要的。
var ErrNoQuestion = errors.New("VLM: 这次没有要问的话")

// checkHistory 确认这段历史里确实有一句等着回答的话。
func checkHistory(history []Message) error {
	if len(history) == 0 || history[len(history)-1].Role != RoleUser {
		return ErrNoQuestion
	}
	return nil
}

// discussImages 把这道题的图读出来、交上去换引用，返回这一轮要带的那几张。
//
// 题图必有；答案图有就一起带 —— 「我错在哪」这类问题没有答案图基本答不了。
//
// 答案图的 hash 记着、文件却不在时**整个失败**，不静默只发题图：那是数据出了问题
// （文件被误删），而少一张图照样能问出话来 —— 于是模型会给一个没看过答案的解释，
// 用户却看不出少了什么。宁可当场报出来。
func (s *Service) discussImages(p Provider, cfg Config, q library.Question) ([]Image, error) {
	qb64, err := s.questions.QuestionImage(q.QuestionHash)
	if err != nil {
		return nil, err
	}
	qimg, err := imageFrom(q.QuestionHash, qb64)
	if err != nil {
		return nil, err
	}
	images := []Image{s.resolveImage(p, cfg, qimg)}

	if !q.HasAnswer() {
		return images, nil
	}
	ab64, err := s.questions.AnswerImage(q.AnswerHash)
	if err != nil {
		return nil, err
	}
	aimg, err := imageFrom(q.AnswerHash, ab64)
	if err != nil {
		return nil, err
	}
	return append(images, s.resolveImage(p, cfg, aimg)), nil
}

// imageFrom 把题库读回来的一份 PNG base64 包成要交给模型的那张图。
//
// service.go 的 loadImage 做的是同一件事，那边只服务打标签、钉死在题图上。这里另写一遍
// 而不是去改它的签名：那是一条已经发布出去的路（票据 09），为省八行去动它不划算。
func imageFrom(hash, b64 string) (Image, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return Image{}, fmt.Errorf("VLM: 图 %s 不是合法的 base64: %w", hash, err)
	}
	return Image{Hash: hash, Data: raw, MIME: "image/png"}, nil
}

// attachImages 复制一份消息，把这一轮的图挂到最后一条 user 消息上。
//
// 复制而不是就地改：history 是调用方的切片，就地改等于悄悄动了别人的数据。
// 顺带把调用方可能塞在图上的东西清掉 —— 图归这一层挂，只有它知道题图与答案图在哪、
// 也只有它走得到那份上传缓存。最后一条是 user 由 checkHistory 保证。
func attachImages(history []Message, images []Image) []Message {
	out := make([]Message, len(history))
	copy(out, history)
	for i := range out {
		out[i].Images = nil
	}
	out[len(out)-1].Images = images
	return out
}
