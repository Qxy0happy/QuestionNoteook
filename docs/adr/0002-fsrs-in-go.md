# FSRS 用官方 Go 库，跑在后端

复习调度用官方 `github.com/open-spaced-repetition/go-fsrs/v4`，运行在 Go 侧，而非前端的 `ts-fsrs`。

前端的官方 TS 实现本更省事（它一直紧跟当前算法版本），但会让调度状态机落在 JS 侧。选 Go 库的代价是必须核实版本：**官方 Go 的稳定线曾长期停在 FSRS-6 之前**，只有 v4 线才是 FSRS-6。落笔时 `v4.0.0` 已发布，权重向量为 21 元素（`weights.go` 注释明确写 FSRS v6）。
