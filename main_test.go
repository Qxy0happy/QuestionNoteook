package main

import "testing"

// packageFromCmdline 是安卓那条路径的解析核心，而安卓路径在桌面上跑不到 ——
// 所以把它拆成纯函数单独测，别让它只能靠真机验证。
func TestPackageFromCmdline(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"安卓的典型形态：尾部还跟一个 NUL", []byte("com.questionbook.app\x00"), "com.questionbook.app"},
		{"后面还有参数", []byte("com.questionbook.app\x00--flag\x00"), "com.questionbook.app"},
		{"没有 NUL（桌面上就是这种）", []byte("com.questionbook.app"), "com.questionbook.app"},
		{"空内容", []byte(""), ""},
		{"只有一个 NUL", []byte("\x00"), ""},
		{"首尾带空白", []byte("  com.questionbook.app  \x00"), "com.questionbook.app"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := packageFromCmdline(c.raw); got != c.want {
				t.Errorf("packageFromCmdline(%q) = %q，期望 %q", c.raw, got, c.want)
			}
		})
	}
}
