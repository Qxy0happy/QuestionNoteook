# 14: VLM 推荐每日复习量

**What to build:** 复习页显示今天建议做几道。推荐来自 VLM，它能看到「今日到期多少」与「最近的表现」。

这一层**在 FSRS 之上**再加一道闸：FSRS 自己用 `RequestRetention` 控制到期量，它只回答「今天最多做几道」。

**Blocked by:** 08（复习）、09（VLM 客户端）

**Status:** ready-for-agent

- [ ] 推荐基于今日到期数量 + 最近表现
- [ ] 显示在复习页
- [ ] 用户可以不采纳
- [ ] 它**不改变** FSRS 自己算出的到期时间
- [ ] 测试用假 provider；断言推荐值被拒绝时队列行为回落到纯 FSRS
