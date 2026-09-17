# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Agent skills

### Issue tracker

Issues and specs live as markdown under `.scratch/`. See `docs/agents/issue-tracker.md`.

### Triage labels

The five canonical roles, label strings equal to their names. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `CONTEXT.md` + `docs/adr/` at the repo root. See `docs/agents/domain.md`.

## Commands

工具版本：Go 由 `go.mod` 钉（`go 1.26`）；包管理器 pnpm 12.4.2 + node 24（`PACKAGE_MANAGER` 可覆盖，本机没有把 npm 暴露到 PATH —— Taskfile 的默认值是 pnpm）。

`wails3` CLI 在 CI 与 `build/Taskfile.yml` 里都钉 **v3.0.0-beta.22**，而本机装的是 **v3.0.0-beta.23**（`wails3 version` 可验）。重新生成绑定前留意这处不一致：绑定的 CI 检查是拿 beta.22 重生成后 `git diff --exit-code`，本机生成的那份若与它不同，红的是 CI。

### Go（唯一有自动化测试的一层）

```sh
go build ./...                                  # 见下面那条 embed 前提
go vet ./...
go test -race -count=1 ./...                    # 与 CI 同一条
go test -run TestProposeTouchesNothingButPendingChanges ./internal/agent/
```

`main.go` 上有 `//go:embed all:frontend/dist`，干净克隆里那个目录不存在（构建产物、在 `.gitignore` 里），于是 `go build ./...` 连编译都过不去。先 `pnpm run build`，或放个占位：`mkdir -p frontend/dist && printf '<!doctype html>\n' > frontend/dist/index.html`。

### 前端（`frontend/`）

```sh
pnpm install --frozen-lockfile
pnpm exec svelte-check --threshold warning    # 与 CI 同一个门槛：有 warning 就红
pnpm run build                                # 产出 frontend/dist
```

`package.json` 里的 `pnpm run check` **不带** `--threshold warning`，别拿它当 CI 的替身。

### 绑定（改了 Go 服务方法之后必须重跑）

前端调的每个 Go 方法都靠 `frontend/bindings/` 那批生成文件。忘了重新生成，前端照样能编译、却在真机上调用失败 —— 本地没有任何东西会红，所以排在 CI 里。

```sh
wails3 generate bindings -f '' -clean=true -ts -i    # 仓库根；等价于 wails3 task common:generate:bindings
```

（Windows 上 shell 必须是 bash：PowerShell 会把空的 `-f ''` 整个丢掉，后面那个 `-clean=true` 就被当成 `-f` 的值。）

### 跑起来

```sh
wails3 task dev                                 # 桌面开发模式（= wails3 dev -config ./build/config.yml -port 9245）
wails3 task build && wails3 task run            # 桌面打包 / 运行
```

安卓是唯一出货路径，本地靠 Taskfile 兜着（CI 里那条 `android` 作业是它的替身）：

```sh
wails3 task android:run:device                  # 编 arm64 .so + APK，装真机并启动，产物 bin/questionbook.apk
wails3 task android:deploy-device               # 同上，但走 release 包
wails3 task android:build ARCH=arm64
wails3 task android:assemble:apk                # 只跑 Java 那一半
wails3 task android:device:list                 # 看连着的设备
wails3 task android:logs                        # logcat 过滤到本应用
```

`run:device` 用 `adb install -r`（覆盖安装、**保留应用数据**）。别改成 uninstall + install：图片与库全在私有目录，卸载就是全丢且不可恢复。

### CI

`.github/workflows/ci.yml` 四个作业：Go（build/vet/`-race` 测试）、前端（svelte-check + build）、绑定（重新生成后 `git diff --exit-code`）、Android（真编 `.so` + APK 并留 artifact）。前三个跑在 ubuntu，绑定的那个跑在 **windows**（wails3 CLI 在 Linux 上要先有 gtk4 / webkitgtk-6.0 才编得出来）。

## Architecture

**形态**：Wails v3 应用 —— Go 后端 + Svelte 5 前端，目标平台是安卓（ADR-0001）。前端构建产物由 `//go:embed all:frontend/dist` 嵌进二进制、由资源服务器进程内提供（安卓上走 WebViewAssetLoader，不起本地端口）。

**唯一测试缝是 Go 的 service 层。** `application.NewService(&X{})` 里的 `X` 就是个普通 struct，测试直接 `new` 出来调方法、断言返回值与落盘/落库结果，不用跑起 Wails。前端是薄视图、不做自动化测试，所以逻辑刻意不往前端放。一处有边界的例外：不含外部依赖的纯函数可以单测（目前只有 `main_test.go` 的 `packageFromCmdline`，因为安卓那条分支在桌面上永远跑不到）；新增这类例外要写明理由。

**服务图在 `main.go` 一处接完。** 每个 `internal/*` 是一个包，对应一个服务；构造顺序有依赖（`discussion` 依赖 `vlm.Service` 本身，因为「同一张图只传一次」的上传缓存、取配置、读两张图都在 vlm 那一层）。两条要守住的接线约束：

- **依赖方向只有一条：采集 → 题库。** 题库要「拉正 + 按 hash 落盘」，但 `capture.Quad` / `capture.Hash` 不能在题库里出现，所以适配层 `cardFiles` 只能待在 `main.go`。
- **SQLite 只开一处。** `library.Open` 是唯一的打开点；tags / review / discussion / workload / digest / agent 都通过 `db.DB()` 借**同一条**连接，不开第二条。

**数据**：SQLite（`modernc.org/sqlite`，纯 Go、无 cgo —— 交叉编译到安卓要靠它）；图片**不在库里**，按内容 hash 命名落在应用私有目录，库里只记 hash（ADR-0004），于是「同一道题拍两次」天然去重、导出必须是 zip 包。图片的读也归 `library.Service`，文件层由外部注入。

**迁移在 `internal/library/migrations.go`，追加式。** 加表/加列 = 往 `migrations` 末尾追加一条、version 取当前最大 +1；**已经发布出去的那几条永远不要改**（老库只跑比自己版本号大的那些）。这张表也建 tags / review / discussion / pending_changes 的表 —— 全库只有这一套迁移机制。

**库外的配置**：应用私有目录（`dataDir()`，安卓上从 `/proc/self/cmdline` 取包名拼出 `/data/data/<pkg>/files/questionbook`）下的若干 JSON —— `vlm.json`（端点 / 凭据 / 模型名 / 提示词）、`review.json`（间隔模糊、考试日期、保留率）、`digest.json`。凭据**只进不出**：界面拿到的是 `ConfigView`（有没有、多长），绝不打印它的值。

**agent 的安全属性（ADR-0003）**：agent 读得到一切，写**只能**经待批准层。三层保证叠在一起 —— 类型层（`store.ReadStore` 拿的是只有 `Query`/`QueryRow` 的 `Querier`，不是 `*sql.DB`，所以它「不是被禁止写，是根本没有能调的东西」）、实例层（`apply.New` 全应用只构造一次）、以及 `internal/agent/safety_test.go` 那几把锁（反射遍历字段 + 源码扫描 import 与写 SQL）。**那份 `allow*` 白名单是刻意手写的字面量**：给这一层加方法就该让它变红，红了先问「这是不是一条绕开待批准层的路」，想清楚再把名字加进去。

**Go ↔ Java 两份文件契约**（跨进程，中间没有类型能对齐，只靠常量和注释互相指着）：`digest-schedule.json`（Go 写、宿主读，字段形状刻意不是 Go 结构体本身，因为读者是 minSdk 21 的 Java）与 `digest-host.json`（宿主写、Go 读，报通知权限与精确闹钟权限）。改任一侧的字段名要同时改 `build/android/app/src/main/java/com/wails/app/DigestScheduler.java`。

**时区**：安卓上 Go 的 `time.Local` 就是 UTC（拿不到系统时区）。所有「今天」都走 `internal/devtz.Now()`，时区名由前端启动时从 `Intl.DateTimeFormat().resolvedOptions().timeZone` 报进来；zoneinfo 用 `time/tzdata` 嵌进二进制。新增任何按本地日切的逻辑都要用 `devtz.Now`，否则队列按 UTC 换日、汇总钟点差一个偏移。

**VLM**：云端、OpenAI 兼容端点（ADR-0005）。模型名**不得硬编码** —— 只能从 `vlm.json` 来，空着就报 `ErrNotConfigured`，不许偷偷用一个写死的名字跑起来。provider 抽象是可替换的，纯文本模型是非图片流量的回落。

**脚手架补丁会丢**：`build/android/**`（Taskfile 的 Windows 分支、`gradlew.bat`、AGP/Gradle 版本与镜像、applicationId 与显示名、`MainActivity.java` 里补的 `WebChromeClient` —— 出厂脚手架整个包里一个都没有，于是 `getUserMedia` 一律被拒）与 `main.go` 的 `application.Mobile.SetKeyboardWatch(true)`（安卓 15 + targetSdk 35 起系统强制边到边，WebView 自己感知不到输入法高度，键盘高度只能由宿主报）都是我们加的。**升级 Wails 或重新 init 会覆盖它们**，清单写在 `.scratch/core-loop/spec.md` 的「脚手架补丁」。
