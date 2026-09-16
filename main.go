package main

import (
	"embed"
	"errors"
	"fmt"
	"image"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/wailsapp/wails/v3/pkg/application"

	"questionbook/internal/agent"
	agentapply "questionbook/internal/agent/apply"
	agentstore "questionbook/internal/agent/store"
	"questionbook/internal/capture"
	"questionbook/internal/devtz"
	"questionbook/internal/digest"
	"questionbook/internal/discussion"
	"questionbook/internal/export"
	"questionbook/internal/library"
	"questionbook/internal/review"
	"questionbook/internal/tags"
	"questionbook/internal/vlm"
	"questionbook/internal/workload"
)

// 前端构建产物（vite 产出到 frontend/dist）嵌进二进制，由资源服务器进程内提供。
// 安卓上这走的是 WebViewAssetLoader，不起本地端口。
//
//go:embed all:frontend/dist
var assets embed.FS

func main() {
	root, err := dataDir()
	if err != nil {
		log.Fatalf("拿不到应用数据目录: %v", err)
	}

	// 题图与库文件都在这个目录下。两者都是「库外」的东西：库里只记 hash（ADR-0004）。
	cardStore, err := capture.NewStore(filepath.Join(root, "cards"))
	if err != nil {
		log.Fatalf("打不开题图目录: %v", err)
	}

	db, err := library.Open(filepath.Join(root, "library.db"))
	if err != nil {
		log.Fatalf("打不开错题库: %v", err)
	}
	defer db.Close()

	// 依赖方向只有一条：采集 → 题库。题库要能读题图、也要能在补拍答案图时拉正并落盘，
	// 所以把两个能力都注进去，而不是让两边互相 import。
	libraryService := library.NewService(db, cardStore, library.WithImageFiles(cardFiles{store: cardStore}))
	// 标签与错题共用同一个连接（tags.NewService 内部走 store.DB()），复习也一样
	// （review.NewService 吃的是 Store 而不是 Service，自己借那条连接）。都不再开第二条。
	tagsService := tags.NewService(db)
	// review 与 digest 的「今天」都从 now.Location() 推出来。**安卓上 Go 的 time.Local 是 UTC**
	// （拿不到系统时区），所以这两处必须用 devtz.Now —— 否则「今日到期」按 UTC 日切、
	// 每日汇总的钟点也会差一个偏移。见 internal/devtz 的包注释。
	reviewService := review.NewService(db, review.WithNow(devtz.Now))

	// VLM 的端点与凭据不进库：它们是配置，不是错题的数据模型（ADR-0004 已经把「库外的东西」
	// 这个模式立好了）。路径与 library.db、cards/ 并列在同一个应用私有目录下。
	vlmService := vlm.NewService(libraryService, tagsService, filepath.Join(root, "vlm.json"))

	// 讨论要「带图多轮」，而取配置、选 provider、读题图与答案图、以及**同一张图只传一次**的
	// 那份上传缓存全在 vlm 那一层。所以讨论依赖的是 vlm.Service 本身（它结构上就满足
	// discussion.Asker），不是把 provider 再注一遍 —— 那等于把上面那些抄成两份。
	// 必须在 vlmService 之后构造。
	discussionService := discussion.NewService(db, vlmService)

	// 复习量（票 14）：它只读队列与复习记录、问一次模型，然后给一个数。里面**一条写语句
	// 都没有**，所以「不采纳就回落到纯 FSRS」不是一句承诺，而是「什么都没发生」。
	workloadService := workload.NewService(reviewService, db, vlmService)

	// 每日汇总（票 12）：Go 侧只负责算出未来若干天各自的到期数与该发什么。真正到点发那条
	// 通知的是 Java 宿主 —— 它会读 digest-schedule.json，并且**自己再核一遍**才发。
	digestService := digest.NewService(db, filepath.Join(root, "digest.json"), digest.WithNow(devtz.Now))

	// agent（票 11）：读的那一侧拿的是只有 Query/QueryRow 的 Querier（不是 *sql.DB），
	// 提议那一侧只能往待批准表里写。执行者 apply.New **全应用只在这里构造一次** ——
	// 「只有一条写路径」在实例层面的保证就落在这一行上；类型层面的保证由
	// internal/agent 的反射锁与源码扫描钉着（见那边的 safety_test.go）。
	readStore := agentstore.NewReadStore(db.DB(), db, tagsService)
	pend := agent.NewPending(agentstore.NewPendingStore(db.DB()), agentapply.New(tagsService))
	// 配置文件与 VLM 用的是**同一份**：agent 也要模型，而模型名只能从配置来（ADR-0005）。
	agentService := agent.NewService(readStore, pend, filepath.Join(root, "vlm.json"))

	exportService := export.NewService(db, cardFiles{store: cardStore}, export.WithTempDir(root))

	app := application.New(application.Options{
		Name:        "错题本",
		Description: "考研错题拍照整理与 FSRS 复习",
		Services: []application.Service{
			application.NewService(capture.NewService(cardStore, libraryService)),
			application.NewService(libraryService),
			application.NewService(tagsService),
			application.NewService(reviewService),
			application.NewService(vlmService),
			application.NewService(discussionService),
			application.NewService(workloadService),
			application.NewService(digestService),
			application.NewService(agentService),
			// 同一个对象既当 agentService 的提议出口，也自己绑给前端（待批准清单那一面）。
			application.NewService(pend),
			// 导出只露 Stage 那一面 —— 理由见 bundleStager 的注释。
			application.NewService(&bundleStager{svc: exportService}),
			// 前端启动时用它把设备时区报进来（见 deviceTime 的注释）。
			application.NewService(&deviceTime{}),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "错题本",
		Width:            1000,
		Height:           618,
		BackgroundColour: application.NewRGB(6, 7, 15),
		URL:              "/",
	})

	// 打开宿主的软键盘监听。
	//
	// 前端**必须**靠它才知道键盘有多高：安卓 15 + targetSdk 35 之后系统强制边到边，
	// manifest 里的 adjustResize 对输入法已经不生效（窗口不再被压短），而 WebView 自己
	// 也没有 OSK 感知（visualViewport / dvh / interactive-widget 全都不动，crbug 40287394）。
	// 于是唯一可信的键盘高度就是宿主报的 common:keyboard 事件，而它默认是关着的。
	//
	// 这条开关属于接线，不属于任何一个业务包 —— 桌面端以及没有这块能力的平台是空实现，
	// 照调不会有事。（早先它被临时塞在 internal/discussion 里，那是分层不对，已挪到这儿。）
	application.Mobile.SetKeyboardWatch(true)

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}

// cardFiles 是采集与题库之间那层薄壳：题库要「拉正 + 按内容 hash 落盘」和「按 hash 删文件」，
// 但采集侧的具名类型（capture.Quad / capture.Hash）不能在题库里出现 —— 依赖只能有一个方向
// （采集 → 题库），所以这层适配只能待在 main 里。
type cardFiles struct{ store *capture.Store }

func (f cardFiles) RectifyAndSave(frame image.Image, quad library.Quad) (string, error) {
	card, err := capture.Rectify(frame, capture.Quad{
		{X: quad[0].X, Y: quad[0].Y},
		{X: quad[1].X, Y: quad[1].Y},
		{X: quad[2].X, Y: quad[2].Y},
		{X: quad[3].X, Y: quad[3].Y},
	})
	if err != nil {
		return "", err
	}
	hash, err := f.store.Save(card)
	if err != nil {
		return "", err
	}
	return hash.String(), nil
}

// deviceTime 是给前端的一个小服务：把**设备时区**告诉 Go。
//
// 为什么需要这么一条：Go 在安卓上拿不到系统时区（time.Local 就是 UTC），而所有「今天」
// 都建在它上面 —— 复习队列的今日到期、每日汇总的发车时刻。WebView 那边知道得清清楚楚
// （Intl.DateTimeFormat().resolvedOptions().timeZone），所以由前端启动时告诉 Go 一次。
// 见 internal/devtz 的包注释。
type deviceTime struct{}

// Set 记住设备时区（IANA 名，如 "Asia/Shanghai"），返回记下的那个。
func (deviceTime) Set(zoneID string) (string, error) { return devtz.Set(zoneID) }

// Get 报告当前记着的时区名；空串表示还没被设过（此时用的是 time.Local，安卓上即 UTC）。
func (deviceTime) Get() string { return devtz.ID() }

// bundleStager 是给前端的导出那一面。
//
// 为什么不直接把 *export.Service 注册上去：它还有 WriteBundle(io.Writer)，而 Wails 要拿
// 导出的方法生成 TS 绑定 —— 一个 io.Writer 形参在 TS 那边没有对应的东西。所以这层只把
// 界面用得上的 Stage 露出去；打包本身照旧在 export.Service 里，这层纯粹是绑定边界。
type bundleStager struct{ svc *export.Service }

func (b bundleStager) Stage() (export.Bundle, error) { return b.svc.Stage() }

// PathByHash 是导出要的那一面：它按 hash 找**盘上的文件**（不像题库那样读成 image.Image），
// 因为导出是把原样字节搬进 zip，不该先解码再编码一遍。
func (f cardFiles) PathByHash(hash string) string {
	return f.store.Path(capture.Hash(hash))
}

func (f cardFiles) RemoveByHash(hash string) error {
	err := os.Remove(f.store.Path(capture.Hash(hash)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil // 本来就不在，删除的目的已经达到
	}
	return err
}

// dataDir 返回应用私有目录，不存在就建。
//
// 桌面端 os.UserConfigDir() 就够了，安卓端不行 —— 见 androidDataDir。
func dataDir() (string, error) {
	if runtime.GOOS == "android" {
		if dir, err := androidDataDir(); err == nil {
			return dir, nil
		}
	}

	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "questionbook")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// androidDataDir 拼出安卓上的应用私有目录。
//
// **不能靠环境变量。** 应用进程未必有 $HOME（Go 的 os.UserConfigDir() 因此会失败），
// 而 Go 是作为 libwails.so 被 Java 宿主拉起来的，Wails 也不提供这个路径 ——
// 它的 pkg/application 里一整套 *_android.go 没有任何路径助手，
// WailsBridge.java 的那些 native 方法里也没有一个把路径传给 Go。
// （实测：装到真机上启动即 EXIT_SELF —— 就是 dataDir 失败触发的 log.Fatal。）
//
// 办法是问进程自己要包名：安卓上 /proc/self/cmdline 存的就是包名，
// 而应用私有 files 目录的规范位置是 /data/data/<包名>/files（与 getFilesDir() 同一个）。
func androidDataDir() (string, error) {
	raw, err := os.ReadFile("/proc/self/cmdline")
	if err != nil {
		return "", fmt.Errorf("读 /proc/self/cmdline 失败: %w", err)
	}
	pkg := packageFromCmdline(raw)
	if pkg == "" {
		return "", fmt.Errorf("/proc/self/cmdline 里没有包名: %q", string(raw))
	}

	dir := filepath.Join("/data/data", pkg, "files", "questionbook")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("建 %s 失败: %w", dir, err)
	}
	return dir, nil
}

// packageFromCmdline 从 /proc/self/cmdline 的原始内容里取出包名。
//
// cmdline 是 NUL 分隔的，第一段是程序名（安卓上即包名）；
// 末尾通常还跟着一个多余的 NUL，所以不能直接当成一个字符串用。
func packageFromCmdline(raw []byte) string {
	s := string(raw)
	if i := strings.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
