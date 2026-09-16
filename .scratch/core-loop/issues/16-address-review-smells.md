# 16: 收掉代码审查报出的重复与冗余（机会性）

**What to build:** 代码审查的规范轴报出的一批**判断性**气味。它们**都不影响正确性**，所以这张票是机会性的 —— 顺手做，不必专门开一轮。

- **Duplicated Code**：clamp 三份 —— `Capture.svelte`、`CropBox.svelte`、`internal/capture/rectify.go` 各一份
- **Duplicated Code**：`build/android/Taskfile.yml` 里挑设备那段 `tr -d '\r' | tail -n +2 | while read -r SERIAL STATE _REST` 在 deploy-emulator 与 deploy-device **逐字重复**；`[ -f "{{.ADB}}" ] || command -v adb` 连注释重复 3 次
- **Speculative Generality**：`library.Store` 的 `MemoryPath` 与随之而来的 `SetMaxOpenConns(1)` 特例，只被它自己那条测试用到
- **Primitive Obsession**：hash 在 capture 有类型（`Hash`），在 library 是裸 string，测试另编 `"sha256:…"` 前缀
- **Mysterious Name**：`capture.Service.Rectify` 与包级 `Rectify` 同名，却多带一层落盘副作用，名字盖住了它

**明确不做**：`library.Service` 那几个单行转发方法**不是待办** —— 它们被「唯一测试缝」这条决策定死了（见 spec），砍掉反而拆了自己的测试面。

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [ ] 上述每一条，要么处理，要么在票里写一句**为什么留着**
- [ ] 不改任何外部行为；全量测试保持通过
- [ ] 若某条改动会牵出 spec 里已定的决策（比如服务划分），停下来记进票而不是顺手改
