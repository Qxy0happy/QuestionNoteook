package main

import (
	"embed"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/wailsapp/wails/v3/pkg/application"

	"questionbook/internal/capture"
	"questionbook/internal/library"
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

	// 卡片图与库文件都在这个目录下。两者都是「库外」的东西：库里只记 hash（ADR-0004）。
	cardStore, err := capture.NewStore(filepath.Join(root, "cards"))
	if err != nil {
		log.Fatalf("打不开卡片图目录: %v", err)
	}

	db, err := library.Open(filepath.Join(root, "library.db"))
	if err != nil {
		log.Fatalf("打不开错题库: %v", err)
	}
	defer db.Close()

	app := application.New(application.Options{
		Name:        "错题本",
		Description: "考研错题拍照整理与 FSRS 复习",
		Services: []application.Service{
			application.NewService(capture.NewService(cardStore)),
			application.NewService(library.NewService(db)),
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

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
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
