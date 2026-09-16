package digest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 这条是**契约测试**：宿主（Java）与这里（Go）是两个进程里跑的两段代码，中间没有类型
// 能帮它们对齐 —— 只靠一个文件名和几个 JSON 字段名。所以这里照宿主写下的形状造一份，
// 断言这边读得懂。字段名改了要两边同时改，这条会当场翻。
func TestReadHostStatusReadsWhatTheHostWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, hostStatusName)

	// 这就是 DigestScheduler.java 写出来的形状（原样抄过来的，别改这边去迁就实现）。
	raw := `{
	  "can_schedule_exact": false,
	  "notifications_enabled": true,
	  "last_arm_at_ms": 1757000000000,
	  "last_fire_at_ms": 1757003600000,
	  "last_result": "skipped_stale"
	}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("写宿主状态: %v", err)
	}

	got := readHostStatus(path)

	if !got.Known {
		t.Fatal("Known = false，但文件明明在、也能解析 —— 字段名是不是对不上了？")
	}
	if got.CanScheduleExact {
		t.Error("CanScheduleExact = true，想要 false")
	}
	if !got.NotificationsEnabled {
		t.Error("NotificationsEnabled = false，想要 true")
	}
	if got.LastArmAtMS != 1757000000000 {
		t.Errorf("LastArmAtMS = %d", got.LastArmAtMS)
	}
	if got.LastFireAtMS != 1757003600000 {
		t.Errorf("LastFireAtMS = %d", got.LastFireAtMS)
	}
	if got.LastResult != "skipped_stale" {
		t.Errorf("LastResult = %q", got.LastResult)
	}
}

// 读不到、读坏了都**不算错** —— 这份东西是诊断，缺了它设置页少显示一行而已。
// 让它报错会把「提醒设置」整页顶红，而用户什么都没做错。
func TestReadHostStatusToleratesMissingAndBroken(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name string
		body string
		// 文件不存在时 body 为空
		missing bool
	}{
		{name: "文件不在", missing: true},
		{name: "半截的 JSON（宿主正在写）", body: `{"can_schedule_exact": tr`},
		{name: "空文件", body: ``},
		{name: "不是 JSON", body: `宿主写坏了`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(c.name, "/", "_")+".json")
			if !c.missing {
				if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
					t.Fatalf("造文件: %v", err)
				}
			}

			got := readHostStatus(path)
			if got.Known {
				t.Errorf("Known = true，但这份内容读不出状态：%q", c.body)
			}
			// 而且要能安全地拿它去问一句话。
			if note := got.Note(true); note != "" {
				t.Errorf("不知道的时候不该说什么，却给出了 %q", note)
			}
		})
	}
}

// 那句人话：只在「提醒真的会受影响」的时候才说，其余一律闭嘴。
func TestHostNote(t *testing.T) {
	ok := HostStatus{Known: true, CanScheduleExact: true, NotificationsEnabled: true}

	cases := []struct {
		name    string
		host    HostStatus
		enabled bool
		want    string // "" 表示什么都不该说
		// 期望那句话里含有的关键词（want 非空时用）
		contains string
	}{
		{
			name:    "用户自己关掉了，界面上那个开关已经说明了一切",
			host:    ok,
			enabled: false,
			want:    "",
		},
		{
			name:    "宿主还没报过 —— 那不是用户的错，别说话",
			host:    HostStatus{Known: false},
			enabled: true,
			want:    "",
		},
		{
			name:     "通知权限被关了：真的发不出来，要说",
			host:     HostStatus{Known: true, CanScheduleExact: true},
			enabled:  true,
			contains: "通知权限被关掉",
		},
		{
			name:     "精确闹钟没给：发得出来，但可能晚 —— 措辞不能说成发不出来",
			host:     HostStatus{Known: true, NotificationsEnabled: true},
			enabled:  true,
			contains: "可能会晚",
		},
		{
			name:     "上一次被系统推得太晚、跳过了",
			host:     HostStatus{Known: true, CanScheduleExact: true, NotificationsEnabled: true, LastResult: "skipped_stale"},
			enabled:  true,
			contains: "推到了宽限之外",
		},
		{
			name:     "上一次真的没发成",
			host:     HostStatus{Known: true, CanScheduleExact: true, NotificationsEnabled: true, LastResult: "failed"},
			enabled:  true,
			contains: "没发成",
		},
		{
			// LastResult 为空串是**常态**，不是缺数据：宿主的约定是「arm 成功不动 last_result」，
			// 所以文件刚被 arm 建出来、还没到过点时它就是空的。别把它当异常报出来。
			name:    "一切正常（含刚 arm 过、还没到点）就不说话",
			host:    ok,
			enabled: true,
			want:    "",
		},
		{
			name:    "那天没有到期题所以没发 —— 这是正常的，不该当成问题报出来",
			host:    HostStatus{Known: true, CanScheduleExact: true, NotificationsEnabled: true, LastResult: "skipped_no_due"},
			enabled: true,
			want:    "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.host.Note(c.enabled)
			if c.want == "" && c.contains == "" {
				if got != "" {
					t.Errorf("Note = %q，想要什么都不说", got)
				}
				return
			}
			if c.contains != "" && !strings.Contains(got, c.contains) {
				t.Errorf("Note = %q，里面该含有 %q", got, c.contains)
			}
		})
	}
}

// 状态文件与设置文件同目录 —— 这条钉住「宿主写哪儿、我们读哪儿」的约定。
func TestHostStatusLivesNextToTheConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "digest.json")

	if err := os.WriteFile(filepath.Join(dir, hostStatusName),
		[]byte(`{"notifications_enabled": true, "can_schedule_exact": true}`), 0o600); err != nil {
		t.Fatalf("写宿主状态: %v", err)
	}

	if got := readHostStatusBeside(configPath); !got.Known {
		t.Error("从设置文件的位置没找到宿主状态 —— 两个文件必须在同一个目录里")
	}
}
