package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// 前端构建产物（vite 产出到 frontend/dist）嵌进二进制，由资源服务器进程内提供。
// 安卓上这走的是 WebViewAssetLoader，不起本地端口。
//
//go:embed all:frontend/dist
var assets embed.FS

func main() {
	app := application.New(application.Options{
		Name:        "错题本",
		Description: "考研错题拍照整理与 FSRS 复习",
		// Services 留空：业务服务在后续 ticket 里逐个加进来。
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
