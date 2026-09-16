# 09: VLM 客户端 + 主动打标签

**What to build:** 一层**可替换**的云端 VLM 客户端；对一道错题主动触发打标签，VLM 给出分层标签，用户编辑后保存。

**Blocked by:** 07（标签：三层树）

**Status:** ready-for-agent

- [ ] provider 是一层抽象：base_url 与模型名都来自配置，代码里**不出现硬编码的模型 ID**
- [ ] 保留纯文本模型作为非图片流量的回落
- [ ] 图片按服务方推荐的方式传（避免同一张图反复上传），并显式请求高保真模式
- [ ] 打标签是**主动触发**，不自动跑
- [ ] 返回的标签先呈现给用户编辑，保存后才生效
- [ ] 测试用假的 provider 实现，不打真实网络
- [ ] 先看 01 的结论，决定手写照片这条路是否成立

## VLM 契约

写这张票时查证过的，**实现前请对着官方文档复核一遍**：

- 端点 `https://api.deepseek.com/chat/completions`，**OpenAI 兼容**（同时支持 Chat Completions / Messages / Responses 三种格式）
- 视觉模型带 `Exp` 后缀，官方明说可能被修订或替换 —— 这就是 provider 抽象是硬要求、不是"以后换着方便"的原因
- 三种传图方式：base64 内联（单图 ≤ 32 MiB）／外部 URL／**Files API**（免费，单文件 ≤ 64 MiB，可复用）。同一张题图会在「打标签」和「讨论」里反复用到，**Files API 明显更省**
- 计费：单图最多按 **384 输入 token** 计
- `detail` 参数：`low`（降采样到 512×512）／`high`·`original`（保留原尺寸）／`auto`。**密集文档必须用 high/original**，否则会被降采样
- ⚠️ **图片只能放在 user 消息里** —— 放进 system 或 assistant 会直接 400
- ⚠️ 写票时两个来源对模型 ID 说法不一（`deepseek-v4-flash-vision-exp` vs `deepseek-flash`）。**以官方文档为准，别信这条转述**
