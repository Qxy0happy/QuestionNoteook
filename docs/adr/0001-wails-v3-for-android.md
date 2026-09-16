# 用 Wails v3 构建安卓应用

目标平台是安卓，但选型是 Wails v3（Go 后端 + Web 前端），而非原生 Kotlin 或 Flutter。驱动因素是语言所有权：本项目 100% 的 Go 由 Claude 编写，而用户读不了 Kotlin 和 Dart。

代价：Wails v3 的安卓支持在官方文档中标为 **experimental**，API 与构建流程都可能变动。
