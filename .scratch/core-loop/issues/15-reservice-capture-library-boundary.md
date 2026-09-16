# 15: 重划采集与题库的边界

**What to build:** 让「拍一道题」在服务层成为**一次调用**，并把图片的**读**归到它该在的地方。

两条来自代码审查的规格缺口，同源 —— 都是服务划分的问题：

1. spec 的服务划分写着「**采集**……把结果落成一张题图，**返回新错题的标识**」，但实现里 `capture.Service.Rectify` 只返回图片 hash，建错题是前端**另起一次** `library.Add`（`Capture.svelte` 的 confirm）。两次 IPC 之间失败就留下一张**库未引用的题图**。
2. spec 把「**读取题图 / 答案图**」划给**题库**，但实现把图片的读与写整个留在采集（`capture.Service.Card`），`internal/library` 只碰数据库行。

**Blocked by:** None (can start immediately) —— 它依赖的前端与 Go 两侧都已落地

**Status:** ready-for-human —— 代码已落地并跑过 `go test`，剩下的是接线、重新生成 bindings、真机复测（见 Comments）

- [x] 「拉正 + 按内容 hash 落盘 + 建错题」在服务层成为**一次调用**，返回新**错题**的标识
- [x] 两次 IPC 之间的孤儿窗口消失；若无法完全消除，在票里写明还剩多大的窗口与为什么
- [x] 读取题图 / 答案图的能力归到**题库**（`capture.Store` 的读路径可由题库持有，或作为一个被注入的依赖）
- [x] 服务测试跟着调整：断言**一次调用之后库里就有那道错题**
- [x] 前端 `confirm` 相应简化（`dfc337e` 那次为可重入做的顺序调整，届时可以重新讨论）
- [ ] 接线：`main.go` 与 `Library.svelte` 那一处读图调用（两处都是本票划出去的文件，见 Comments）
- [ ] 真机上复测一遍：拍摄 → 入库 → 题库列表

## Comments

### 落地成了什么样

依赖方向定死为**采集 → 题库**（采集要把拍下的一道题落成错题），所以图片的读不能反过来
由采集持有。做法是题库在构造时收一个**注入的文件层**，两边都不 import 对方：

- `capture.NewService(store, lib)`，`lib` 是 `*library.Service`
- `capture.Service.Capture(frameBase64, quad) (library.Question, error)` —— 取代只回 hash 的
  `Rectify`。拉正、按内容 hash 落盘、建错题在这一趟里做完，回来的是**新错题**本身。
- `library.NewService(store, images)`，`images` 是 `library.ImageStore`：只有
  `LoadByHash(hash string) (image.Image, error)` 一个方法。`*capture.Store` 结构上就满足它
  （为此加了一层薄壳方法），接线时直接传 `cardStore`。
- `library.Service.QuestionImage(hash)` / `AnswerImage(hash)` —— 新的读路径，回 PNG 的 base64。
  空 hash 明确报错（没拍答案图的错题 `AnswerHash` 就是空串），不会拿它去拼一个叫 `.png` 的路径。
- `capture.Service.Card` **删掉了** —— 读归题库，采集那边不再有读路径。

### 还剩多大的孤儿窗口

不是零，剩「**题图已落盘、写库失败**」这一小段：从 `store.Save` 返回到 `AddQuestion` 的
INSERT 提交之间，只隔着一次本地 SQLite 插入。

为什么不干脆消掉 —— 两条路都更糟：反过来先写库再落盘，失败会留下一条**指向不存在图片的记录**
（题库页上一道点开是空的错题），比一个看不见的孤儿文件难解释；写库失败后再删文件也不行，
内容 hash 是去重键，同一个 hash 很可能还被**别的**错题引用着（ADR-0004 允许两道题共用一张图）。
彻底消掉得靠引用计数式回收（扫一遍没人引用的图），那是另一张票的事。

比改之前小了一个量级：以前整个第二次 IPC 都是窗口（用户切页、应用被切后台、任何一次失败都算），
现在只剩一次本地插入。

### 留给人工的两处接线（都是本票划出去的文件）

1. `main.go`：
   ```go
   lib := library.NewService(db, cardStore)
   ...
   application.NewService(capture.NewService(cardStore, lib)),
   application.NewService(lib),
   ```
   已按这个形状**临时**接过一次线跑验证，随后还原（`git status` 里 `main.go` 无改动）。
2. `Library.svelte` 第 70 行：`Capture.Card(q.QuestionHash)` → `Library.QuestionImage(q.QuestionHash)`，
   `Capture` 那个 import 随之删掉。**在这一行改掉之前，重新生成 bindings 后 `svelte-check` 会红** ——
   现在不红只是因为 bindings 还是旧的（旧的还导着 `Card`）。

### 验证记录

`go build ./...` / `go vet ./...` / `go test ./...` 在临时接线后全过；`gofmt -l` 对本次改动的
文件干净（`build/ios/` 下那两个脚手架文件本来就报，没碰它们）。
`pnpm exec svelte-check`：**临时**给 bindings 补了 `Capture` / `QuestionImage` / `AnswerImage`
三个桩之后 0 error —— 也就是说前端那一处改动本身是类型正确的，现在报的两条纯粹是因为
bindings 还没重新生成。桩已还原（`git status` 里 `frontend/bindings` 无改动）。

**未验证**：真机上的 拍摄 → 入库 → 题库列表；以及那个残留窗口在真实失败（磁盘写满 / 库被锁）下的表现。

### 2026-09-16 接线与真机复测（人工补完）

接线做了两处：`main.go`（`library.NewService(db, cardStore)` 先建，再 `capture.NewService(cardStore, libraryService)`）与 `Library.svelte`（`Capture.Card` → `Library.QuestionImage`，`Capture` 那个 import 删掉）。

**真机复测通过，而且是硬证据**：连按两次「确认」之后，设备上 `cards/` 里恰好两个文件，
库里的两条错题 hash 与它们**逐一对应**：

```
共 2 道错题
  id=1 题图=fb81d682415c2fcd…7513c6
  id=2 题图=45962c9004cc5842…0313e
```

即 `capture.Capture(...)` 一次调用确实做完了「拉正 + 按内容 hash 落盘 + 建错题」。
`wails3 build` 生成的跨包返回类型是 `library$0.Question`，生成器处理正确
（子 agent 曾把这条标为「只读过生成器代码、没跑过」）。

**顺带发现**：`run:device` 每次都先 `uninstall` 再 `install`，所以**每次部署都会清掉应用数据**。
查库前要意识到这一点，否则会以为数据凭空消失了。

**未验证**：保留的那段孤儿窗口（题图已落盘、写库失败）在真实失败下的表现。
