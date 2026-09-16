package export

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Bundle 是一个**已经打好、等人把它搬出去**的导出包。
type Bundle struct {
	// Path 是包在应用私有目录里的落点：宿主读它，搬完把源文件删掉。
	Path string
	// Name 是建议的显示名，宿主拿它当「下载」目录里的文件名。
	Name string

	// 清点数据（题数 / 图数 / 字节数），界面拿它说清「导出了什么、多大」。
	Result
}

// stagePrefix 是暂存包的命名前缀。clearStaged 靠它认出上一次的残留。
const stagePrefix = "错题本-"

// WithNow 换掉取当前时刻的函数。只给测试用 —— 文件名里的日期要能钉死。
func WithNow(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// Stage 打一个导出包放在应用私有目录里，返回它的落点与建议名。
//
// **为什么停在私有目录，而不是直接落到系统的「下载」目录**：Go 与 WebView 都碰不到
// Android 的存储 API（scoped storage 下直接写 /sdcard/Download/ 会被拒），搬出去那一步
// 只能由 Java 宿主做（`window.wails.copyToDownloads`，见 WailsJSBridge.java）。
// 所以这一层只负责「有一个已经打好的包」，并把它的路径交出去。
//
// **要么一个完整的包、要么什么都不留**：先写临时文件，成功了再改名。半截的 zip 顶着
// 备份的名字留在盘上比没有包更坏 —— zip 没收尾就没有中央目录，既打不开也认不出它是坏的。
func (s *Service) Stage() (Bundle, error) {
	if err := os.MkdirAll(s.tempDir, 0o700); err != nil {
		return Bundle{}, fmt.Errorf("建暂存目录: %w", err)
	}
	// 上一次次没搬走的（拷贝失败、或者用户没给权限）先清掉，免得越积越多。
	s.clearStaged()

	name := stagePrefix + s.now().Format("2006-01-02") + ".zip"
	final := filepath.Join(s.tempDir, name)

	tmp, err := os.CreateTemp(s.tempDir, "stage-*.zip")
	if err != nil {
		return Bundle{}, fmt.Errorf("建临时包: %w", err)
	}

	res, err := s.WriteBundle(tmp)
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return Bundle{}, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return Bundle{}, fmt.Errorf("关临时包: %w", err)
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		os.Remove(tmp.Name())
		return Bundle{}, fmt.Errorf("把包改名为 %s: %w", final, err)
	}

	return Bundle{Path: final, Name: name, Result: res}, nil
}

// clearStaged 删掉暂存目录里上一次留下的包。
//
// 删不掉就算了：它是垃圾，不是错误 —— 为了清垃圾把一次导出弄失败，本末倒置。
func (s *Service) clearStaged() {
	old, err := filepath.Glob(filepath.Join(s.tempDir, stagePrefix+"*.zip"))
	if err != nil {
		return
	}
	for _, p := range old {
		_ = os.Remove(p)
	}
}
