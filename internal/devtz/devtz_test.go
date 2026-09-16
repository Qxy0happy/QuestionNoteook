package devtz

import (
	"strings"
	"testing"
	"time"
)

// 这个包能工作的**前提**是把 time/tzdata 嵌进来 —— 安卓上没有系统那份 zoneinfo。
// 所以第一条测试就是「一个真实的 IANA 名载得进来」，它挂了就说明那个空导入被删了。
//
// 断言偏移而不是当前日期：偏移是稳定的，日期会在午夜线上抖。
func TestSetMakesNowUseTheDeviceZone(t *testing.T) {
	if _, err := Set("Asia/Shanghai"); err != nil {
		t.Fatalf("Set(Asia/Shanghai): %v —— time/tzdata 那个空导入还在吗？", err)
	}
	t.Cleanup(func() { _, _ = Set("UTC") })

	got := Now()
	if name := got.Location().String(); name != "Asia/Shanghai" {
		t.Errorf("Now() 的时区 = %q，想要 Asia/Shanghai", name)
	}
	if _, off := got.Zone(); off != 8*3600 {
		// UTC 会给 0；这里要的正是那个 +8。
		t.Errorf("Now() 的偏移 = %d 秒，想要 %d", off, 8*3600)
	}
}

// 认不出的名字**不能覆盖**已经记着的那个：宁可继续用旧的，也好过让后面所有的「今天」
// 都算在一个不存在的时区上（那会静默地全部错掉）。
func TestSetRejectsUnknownAndKeepsTheOldOne(t *testing.T) {
	if _, err := Set("Asia/Shanghai"); err != nil {
		t.Fatalf("先设一个正常的: %v", err)
	}
	t.Cleanup(func() { _, _ = Set("UTC") })

	before := Now()

	for _, bad := range []string{"不是时区", "Mars/Olympus", "Asia/Shanghai/extra"} {
		if _, err := Set(bad); err == nil {
			t.Errorf("Set(%q) 应当报错", bad)
		}
	}

	// 时区没被换掉。
	if _, off := Now().Zone(); off != 8*3600 {
		t.Errorf("坏名字之后偏移变成了 %d 秒，想要仍然是 %d", off, 8*3600)
	}
	// 时刻当然还在往前走，只是时区没变。
	if after := Now(); after.Before(before.Add(-time.Second)) {
		t.Error("Now() 倒退了 —— 那不是时区的问题，是时钟的问题")
	}
	if id := ID(); id != "Asia/Shanghai" {
		t.Errorf("ID() = %q，坏名字不该改掉它", id)
	}
}

func TestSetRejectsEmpty(t *testing.T) {
	if _, err := Set("   "); err == nil {
		t.Error("空名字应当报错")
	}
}

// ID 的默认值是空串 —— 那正是「前端还没告诉过 Go」这个状态，
// 界面上要靠它说清「现在用的是 UTC」。
func TestIDReportsWhatWasSet(t *testing.T) {
	if _, err := Set("Asia/Tokyo"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	t.Cleanup(func() { _, _ = Set("UTC") })

	got, err := Set("Asia/Shanghai")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got != "Asia/Shanghai" {
		t.Errorf("Set 返回 %q，想要它返回记下的那个名字", got)
	}
	if id := ID(); id != "Asia/Shanghai" {
		t.Errorf("ID() = %q", id)
	}
	if !strings.HasPrefix(Now().Location().String(), "Asia/") {
		t.Errorf("Now() 的时区 = %q", Now().Location().String())
	}
}
