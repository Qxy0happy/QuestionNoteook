# 16: 收掉代码审查报出的重复与冗余（机会性）

**What to build:** 代码审查的规范轴报出的一批**判断性**气味。它们**都不影响正确性**，所以这张票是机会性的 —— 顺手做，不必专门开一轮。

**Blocked by:** None (can start immediately)

**Status:** done —— 两条处理、两条留着（理由写进了代码注释）、一条**票据已过时**

- [x] 上述每一条，要么处理，要么在票里写一句**为什么留着**
- [x] 不改任何外部行为；全量测试保持通过
- [x] 若某条改动会牵出 spec 里已定的决策（比如服务划分），停下来记进票而不是顺手改

## Comments

逐条结果。**理由都写在代码注释里**（本票原先不许改 `.scratch/`，所以现在补记到这里）。

### 1. Duplicated Code：clamp 三份 → TS 那两份**处理了**

`CropBox.svelte` 的 `<script module>` 里导出 `clamp`（跟 `Box` 一起），实例段那份删掉；
`Capture.svelte` 改成 `import CropBox, { clamp, type Box }`。

顺带纠了个名：`Capture.svelte` 原来那四个调用点传的是 `Math.round(...)`，本来就是 number，
叫 `clampInt` 是假的。

**Go 那第三份留着**（`internal/capture/rectify.go`）：跨语言不可能共享；它只服务双线性采样的
下标夹取（越界外推那两行），为三行函数开共享包、或把夹取摊到调用处都比这三行贵；
而且 `Int` 这个名字在这里是**准的**（参数真是像素下标）。

### 2. Duplicated Code：Taskfile → **处理了**（YAML 锚）

`&adb-present` 锚在 `ensure-emulator`（首次出现处），`run:device` / `deploy-device` 写成
`- *adb-present`；`&deploy-to-device` 锚在 `run:device` 的 cmds，`deploy-device` 写成
`- *deploy-to-device`。两个任务各少 27 行，重复的注释只留正本一处。

别名整条命令做替换，**没有把一个 cmd 拆成两个** —— go-task 的 cmds 是否共享同一个 shell 会话
没有把握，拆开万一不共享就会把 `$DEVICE` 弄空，那是真会坏设备的改动。

⚠️ **票据描述与代码不符（已确认）**：挑设备那段**不在** `deploy-emulator` 里，而是
`run:device` 与 `deploy-device` 逐字重复（已用哈希确认两份完全一致）；`deploy-emulator`
走 `ensure-emulator`、不带 `-s`。

### 3. Speculative Generality：`MemoryPath` + `SetMaxOpenConns(1)` → **留着**

票里没写理由就留了，补上：删它必须连 `TestOpenMemoryPath` 一起删（它是全仓唯一调用方），
那是「**拿少一条覆盖换少五行代码**」。而且 `SetMaxOpenConns(1)` 不是可顺手删的装饰 ——
内存库跟着连接走，池里再开一条就等于换了个空库，这两处只能同生共死。

真要删的话：`store.go` 去掉那个 const 与两个分支（`MkdirAll` 改无条件，对真实路径行为不变），
`store_test.go` 删 `TestOpenMemoryPath`。

### 4. Primitive Obsession：hash 裸 string → **留着**

具名类型落在这条边界上要同时改 4 处，都在别的票/别人的文件里：`library/question.go`、
`library/service.go`、`internal/capture/store.go`、`main.go` 的适配壳。

实质理由（写进了 `library/store.go` 的包注释）：两端看到的本来就是文本（SQLite TEXT 列、
前端收到的字符串），再立第三个 hash 类型换不来安全，只多一圈转换。

### 5. Mysterious Name：`capture.Service.Rectify` → **票据已过时，无需处理**

那个方法**已经不存在了**。票 15 把「拉正 + 落盘 + 建错题」合成了一趟 `Service.Capture`，
名字说的就是它做的事；`capture.Rectify` 只剩包级那个纯函数。全仓 grep 确认没有
`func (s *Service) Rectify`。

### 6. 后来补的一条：手抄的 DROP 清单（同一类问题，第 5 条不做之后长出来的）

三个包里的 `TestUpgradeFromOlderSchema` 要**扮演**一个老库，各自手抄了一份「把之后每条迁移建的表删掉」
的清单。每加一条迁移，就要记得回来补 N 行 —— 这件事被**漏掉过四次**（迁移 3、4、5 各一次）。
漏了的后果不是报错本身，而是「扮演的老库」根本没扮演对：那个年代不该有的表还在里面，
于是那条测试**静默地**在测一个假场景。

改法：`migration` 结构加一个 `tables` 字段声明「这条迁移建了哪些表」，
`library.TablesIntroducedAfter(version)` 汇总，三处测试改成调它。
清单不是「又一份靠自觉维护的名单」—— `internal/library/migrations_test.go` 双向盯着它
（声明了却建不出来会红；建了却没声明也会红，靠数 `CREATE TABLE` 的出现次数，不靠解析 SQL 名字）。

顺带撤掉了一个因它而生的绕法：迁移 5 原先带 `IF NOT EXISTS`（为了绕过清单漏了它），
那是真的弱化 —— 与前面四条一样改回裸 `CREATE TABLE` 了。

### 顺手看见、没做的（不在票里）

`build/android/Taskfile.yml` 里 `assemble:apk` / `assemble:apk:release` / `assemble:aab` /
`assemble:aab:release` 四个任务把 java 的 precondition 抄了 4 遍、`uninstall/install/am start`
三连抄了 4 遍 —— 与本票收掉的是同一种重复。锚的机制已在同一文件里验证可用，**要收可以顺手收**，
但票里没列，没自己扩范围。

### 验证

`go build` / `go vet` / `go test -count=1 ./...`（改到的包）全过；`gofmt -l` 对改动的 Go 文件干净；
`svelte-check` 0 错 0 警（接线后我又跑了一次全仓，155 文件 0 错）。

Taskfile **没有执行**（按约束），改用解析器验证：go-task v3.53.1 同时链了 `gopkg.in/yaml.v3`
与 `go.yaml.in/yaml/v3`，用这两个分别把改前（`git show HEAD:`）与改后解析并逐任务比对 ——
24 个任务、13 个变量，解码结果 **0 项不同**。

**未验证**：go-task 运行时接受 YAML 别名是从「解析器一致」推的，没跑过 `task`；
前端只过了类型检查。
