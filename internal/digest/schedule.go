package digest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// scheduleFileName 是排程文件的落点，与设置文件（digest.json）同目录。
const scheduleFileName = "digest-schedule.json"

// schedulePathFor 由设置文件的路径推出排程文件的路径。
//
// 接线时只给一个路径（设置那份），排程放在它旁边 —— 与 vlm 由 configPath 推出上传缓存
// 路径是同一个做法（那边是 vlm-files.json）。
func schedulePathFor(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), scheduleFileName)
}

// scheduleFile 是落给**宿主**的那份 JSON 的形状，slotFile 是它的一条。
//
// 它刻意不是 Schedule 本身，理由有三条：
//
//   - 读者是 Java，不是 Go。脚手架里 minSdk 是 21，`java.time` 用不了（要 26 或脱糖），
//     RFC3339 字符串得拿 SimpleDateFormat 去啃；epoch 毫秒是一句 `getLong` 比大小。
//   - 字段名要**稳定**：宿主补丁不在这个仓库的构建里，它读的是磁盘上这份文件，
//     所以形状得显式钉住，不能跟着 Go 结构体改名走。
//   - 日期必须是能直接与宿主本地日历比对的 "2026-09-16"，不能是别的写法。
//
// 于是「Go 侧的类型」与「磁盘上的形状」是两份东西，中间由 writeSchedule 连一次。
// 多出来的那一次转换是**故意**的：它是这条接缝上唯一需要对齐的地方，
// 有它才看得出契约在哪（见 internal/digest/service_test.go 里那份照着契约写的测试）。
type scheduleFile struct {
	// Enabled 为 false 时 slots 一定是空的（见 Schedule.Enabled）。
	Enabled bool `json:"enabled"`

	Hour   int `json:"hour"`
	Minute int `json:"minute"`

	// GeneratedAtMS 是这份排程算出来的时刻（epoch 毫秒）。宿主据此知道它有多旧。
	GeneratedAtMS int64 `json:"generated_at_ms"`

	// Note 是给**人**看的一句话（「已关闭每日提醒」之类）。宿主不必懂它。
	Note string `json:"note,omitempty"`

	// Slots 一天一条，从最近的一次算起。空切片落成 `[]`，不是 `null` ——
	// 宿主是 Java，少写一个 null 检查就少一次 NPE。
	Slots []slotFile `json:"slots"`
}

// slotFile 是排程里的一条。字段与读音见 scheduleFile 的注释。
type slotFile struct {
	FireAtMS int64  `json:"fire_at_ms"`
	Date     string `json:"date"`
	Count    int    `json:"count"`
	Title    string `json:"title"`
	Body     string `json:"body"`
}

// file 把一份排程转成落盘的形状。
func (s Schedule) file(note string) scheduleFile {
	slots := make([]slotFile, 0, len(s.Slots)) // 空切片而不是 nil
	for _, slot := range s.Slots {
		slots = append(slots, slotFile{
			FireAtMS: slot.FireAt.UnixMilli(),
			Date:     slot.Date,
			Count:    slot.Count,
			Title:    slot.Title,
			Body:     slot.Body,
		})
	}
	return scheduleFile{
		Enabled:       s.Enabled,
		Hour:          s.Hour,
		Minute:        s.Minute,
		GeneratedAtMS: s.GeneratedAt.UnixMilli(),
		Note:          note,
		Slots:         slots,
	}
}

// writeSchedule 把排程原子地写到 path。
//
// 原子性在这件事上比在设置上还重要：宿主是**另一个进程里的另一段代码**，
// 它在闹钟响的那一刻读这个文件 —— 读到一个写了一半的 JSON，它会当成「没有可发的」，
// 于是那天的通知就没了，而且没人知道为什么。所以照旧是临时文件 + 改名。
//
// 权限 0600，与设置文件、vlm 配置一致（同目录同权限，见 Config.Save）。
func writeSchedule(path string, s Schedule, note string) error {
	raw, err := json.MarshalIndent(s.file(note), "", "  ")
	if err != nil {
		return fmt.Errorf("每日提醒: 编码排程: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("每日提醒: 建排程目录: %w", err)
	}

	f, err := os.CreateTemp(dir, "digest-schedule-*.json")
	if err != nil {
		return fmt.Errorf("每日提醒: 建临时文件: %w", err)
	}
	tmp := f.Name()
	renamed := false
	defer func() {
		f.Close()
		if !renamed {
			os.Remove(tmp)
		}
	}()

	if _, err := f.Write(raw); err != nil {
		return fmt.Errorf("每日提醒: 写排程: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("每日提醒: 收尾临时文件: %w", err)
	}
	// Windows 上改名之前必须先放手；安卓上这一步同样是原子的。
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("每日提醒: 落盘排程 %s: %w", path, err)
	}
	renamed = true
	return nil
}
