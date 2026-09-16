package main

import (
	"embed"
	"log"
	"os"
	"path/filepath"

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
// ⚠️ 未在真机上验证：安卓沙箱里的落点是不是这里。桌面端 os.UserConfigDir() 没有疑问，
// 但安卓上这个变量依赖 $HOME / $XDG_CONFIG_HOME，而 Go 是作为 libwails.so 被 Java 宿主拉起来的，
// 那套环境变量未必有。票据 02 收尾时要在真机上确认落点，必要时改从安卓宿主拿路径。
func dataDir() (string, error) {
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
