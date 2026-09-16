package digest

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// readHostStatusBeside 从设置文件的位置推出宿主状态文件的位置 —— 两者同目录
// （都是应用私有目录下的 <dataDir>/questionbook/），所以不必让调用方再传一个路径进来。
func readHostStatusBeside(configPath string) HostStatus {
	return readHostStatus(filepath.Join(filepath.Dir(configPath), hostStatusName))
}

// HostStatus 是**宿主**（Java 那一侧）回写的状态。
//
// 为什么要有它：有两件事 Go 侧无从知道 —— 系统里的通知权限有没有被关、精确闹钟的
// 「闹钟和提醒」权限有没有给。那两件事只有 Java 知道（`areNotificationsEnabled()`、
// `canScheduleExactAlarms()`），所以由它每次 arm / 发完之后写进一个文件，这里只读。
//
// **读不到、读坏了都不算错。** 这份东西是诊断，缺了它设置页少显示一行而已；
// 让它报错会把「提醒设置」整页顶成红色，而用户其实什么都没做错。
type HostStatus struct {
	// Known 为 false 表示宿主还没写过这个文件（补丁还没打上，或者装上之后
	// 宿主那一侧一次都没跑过）。此时下面几项没有意义，界面也不该拿它们说事。
	Known bool

	// CanScheduleExact 是「闹钟和提醒」权限。false 不等于发不出来 —— 宿主会降级到
	// 不精确的窗口闹钟，只是可能被系统省电策略推到维护窗口。
	CanScheduleExact bool
	// NotificationsEnabled 是系统里的通知权限。false 就是**真的发不出来**。
	NotificationsEnabled bool

	LastArmAtMS  int64
	LastFireAtMS int64
	// LastResult 取值由宿主那边定：posted / skipped_disabled / skipped_no_due /
	// skipped_stale / skipped_no_permission / failed。空串 = 还没发过。
	LastResult string
}

// hostStatusName 是宿主回写的那个文件名。
//
// 它**必须**与宿主那边的常量一致（`DigestScheduler.java`）。写死在这里而不是做成配置：
// 两边是两个进程里跑的两段代码，中间没有类型能帮它们对齐，所以这条契约只能靠一句话
// 和一处注释互相指着。改名要同时改两处。
const hostStatusName = "digest-host.json"

// readHostStatus 读宿主回写的状态。任何读不通的情形都返回零值（Known = false），不报错。
func readHostStatus(path string) HostStatus {
	raw, err := os.ReadFile(path)
	if err != nil {
		// 文件不在是常态（补丁没打、或者还没跑过）。别的读错误同样当「不知道」。
		if !errors.Is(err, fs.ErrNotExist) {
			return HostStatus{}
		}
		return HostStatus{}
	}

	var wire struct {
		CanScheduleExact     bool   `json:"can_schedule_exact"`
		NotificationsEnabled bool   `json:"notifications_enabled"`
		LastArmAtMS          int64  `json:"last_arm_at_ms"`
		LastFireAtMS         int64  `json:"last_fire_at_ms"`
		LastResult           string `json:"last_result"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		// 半截文件（宿主正在写）或字段对不上：当作不知道，而不是报错。
		return HostStatus{}
	}

	return HostStatus{
		Known:                true,
		CanScheduleExact:     wire.CanScheduleExact,
		NotificationsEnabled: wire.NotificationsEnabled,
		LastArmAtMS:          wire.LastArmAtMS,
		LastFireAtMS:         wire.LastFireAtMS,
		LastResult:           wire.LastResult,
	}
}

// Note 用一句人话说明「这条提醒到底能不能到」，说不着就返回空串。
//
// 措辞放在 Go 侧而不是界面里：它要同时看设置（enabled）与宿主报回来的两三个布尔量，
// 是一段判断而不是一段文案。前端只负责把非空的它显示出来。
func (h HostStatus) Note(enabled bool) string {
	if !enabled {
		// 用户自己关的，界面上那个开关已经说明了一切，不用再说一遍。
		return ""
	}
	if !h.Known {
		// 宿主还没报过。别把「补丁没打上」说成用户的错。
		return ""
	}

	if !h.NotificationsEnabled {
		return "系统里的通知权限被关掉了，这条汇总发不出来 —— 去系统设置里给「错题本」打开通知。"
	}
	if !h.CanScheduleExact {
		return "系统没给「闹钟和提醒」权限，汇总可能会晚一些（由系统的省电策略决定什么时候发）。"
	}
	switch h.LastResult {
	case "skipped_stale":
		return "上一次提醒到得太晚，已经跳过了 —— 系统把它推到了宽限之外。"
	case "failed":
		return "上一次提醒没发成。"
	case "skipped_no_permission":
		// 与上面 NotificationsEnabled 说的是一件事，但那是 arm 时的快照、这是发的那一刻的
		// 实况，两者不一致时以这条为准。
		return "上一次提醒因为通知权限没发出来。"
	}
	return ""
}
