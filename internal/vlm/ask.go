package vlm

import (
	"context"
	"strings"
)

// AskText 是「问模型一句**不带图**的话」这条接缝 —— 推荐今日复习量走它。
//
// 为什么与 Ask 分开、而不是给它加一个「这次不带图」的开关：两条路要的东西不一样。
// 讨论必然带图，于是要读题图与答案图、要过那份 hash → file_id 的上传缓存；这条路手上
// 只有几个数，没有图可传，那套东西一样都用不上。合在一起就得在 Ask 里到处问「这次有图吗」。
//
// 走的是配置里的**文本模型**那一路（Config.modelFor）：视觉模型带 Exp 后缀、官方声明可能
// 被替换，而推荐复习量这种纯文本的活不该跟着它一起冒险 —— 这正是 Config.TextModel 留着
// 的原因（它的注释里点名了「推荐今天做几道」）。文本模型没配时回落到视觉模型，见 textModel。
//
// 回答要求是 JSON 对象：这条路眼下唯一的用处是「要一个数」，而散文里挑数是挑不出来的。
// 将来有别的纯文本活要自由文本（比如 agent 那边的整理），再加旋钮，现在不猜。
//
// 没配 VLM 返回 ErrNotConfigured；网络与凭据的问题由 provider 原样报上来。
func (s *Service) AskText(system, user string) (Reply, error) {
	provider, cfg, err := s.current()
	if err != nil {
		return Reply{}, err
	}

	return provider.Chat(context.Background(), Request{
		Model:  textModel(cfg),
		System: system,
		// 图片只能放在 user 消息里（服务方的硬约束），这条路上没有图，但正文仍然要在那儿。
		Messages: []Message{{Role: RoleUser, Text: user}},
		JSON:     true,
	})
}

// textModel 挑这次用哪个模型名；文本那一路没配就落到视觉那一路。
//
// 「没配模型名」在这里是合法状态（TextModel 是可选的，Validate 不要求它），
// 但一个空串发出去服务方一律 400 —— 所以必须兜住。兜住的那一个是**配置里写着的**名字，
// 不是写死的模型 ID：ADR-0005 禁的是后者。
//
// 回落到视觉模型不算「拿它冒险」：冒险的是图片那条路（Exp 后缀、可能被替换），
// 而这次请求里没有图，它答的是一道纯文本的题。
func textModel(cfg Config) string {
	if m := strings.TrimSpace(cfg.modelFor(false)); m != "" {
		return m
	}
	return cfg.modelFor(true)
}
