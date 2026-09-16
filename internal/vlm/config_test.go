package vlm_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"questionbook/internal/vlm"
)

// 配置落在应用私有目录的一个 JSON 文件里，不进 SQLite（ADR-0004 的「库外」模式，
// 加上凭据本来就不属于错题的数据模型）。这里验的是读写那一圈。

// 存了再读，回来的是同一份。
func TestConfigSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vlm.json")
	want := testConfig()

	if err := want.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := vlm.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if got.BaseURL != want.BaseURL || got.APIKey != want.APIKey ||
		got.VisionModel != want.VisionModel || got.TextModel != want.TextModel {
		t.Errorf("读回来的 = %+v，想要 %+v", got, want)
	}
}

// 尾部斜杠会被收拾掉：之后代码要直接拼 "/chat/completions"。
func TestConfigNormalizesBaseURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vlm.json")
	cfg := testConfig()
	cfg.BaseURL = "https://vlm.invalid/v1///"

	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := vlm.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got.BaseURL != "https://vlm.invalid/v1" {
		t.Errorf("BaseURL = %q，想要没有尾部斜杠", got.BaseURL)
	}
}

// 文件不在 = 还没配，不是读失败：第一次装上这个应用时它就是不在的。
func TestLoadConfigMissingFile(t *testing.T) {
	_, err := vlm.LoadConfig(filepath.Join(t.TempDir(), "nope.json"))
	if !errors.Is(err, vlm.ErrNotConfigured) {
		t.Fatalf("err = %v，想要 ErrNotConfigured", err)
	}
}

// 读坏了要说清是哪个文件坏了，而不是静默当成「还没配」。
func TestLoadConfigBrokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vlm.json")
	if err := os.WriteFile(path, []byte("{ 这不是 JSON"), 0o600); err != nil {
		t.Fatalf("写坏文件: %v", err)
	}

	_, err := vlm.LoadConfig(path)
	if err == nil {
		t.Fatal("想要一个错，拿到 nil")
	}
	if errors.Is(err, vlm.ErrNotConfigured) {
		t.Error("读坏了不该被当成「还没配」：那样用户会一直以为自己没填")
	}
}

// 校验：缺项、地址不成形、档位取值不对，都要在发请求之前就报出来。
func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name string
		cfg  vlm.Config
		want bool // true = 应当通过
	}{
		{"完整", testConfig(), true},
		{"缺 base_url", vlm.Config{APIKey: "k", VisionModel: "m"}, false},
		{"地址不成形", vlm.Config{BaseURL: "vlm.invalid", APIKey: "k", VisionModel: "m"}, false},
		{"缺 vision_model", vlm.Config{BaseURL: "https://vlm.invalid", APIKey: "k"}, false},
		{"缺 api_key", vlm.Config{BaseURL: "https://vlm.invalid", VisionModel: "m"}, false},
		{"档位不认识", vlm.Config{BaseURL: "https://vlm.invalid", APIKey: "k", VisionModel: "m", Detail: "高清"}, false},
		{"档位留空可以", vlm.Config{BaseURL: "https://vlm.invalid", APIKey: "k", VisionModel: "m", Detail: ""}, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.Validate()
			if c.want && err != nil {
				t.Errorf("Validate = %v，想要通过", err)
			}
			if !c.want && err == nil {
				t.Error("Validate = nil，想要一个错")
			}
		})
	}
}

// 档位留空时补成 original：票据要求**显式**请求高保真，题面是密集文档。
func TestConfigDetailDefaultsToOriginal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vlm.json")
	if err := testConfig().Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	v := mustConfigView(t, path)
	if v.Detail != vlm.DetailOriginal {
		t.Errorf("Detail = %q，想要 %q", v.Detail, vlm.DetailOriginal)
	}
}

// 配置文件里除了那份配置本身，不该多出别的东西 —— 尤其不该有凭据的副本或派生值。
func TestConfigFileHoldsOnlyConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vlm.json")
	if err := testConfig().Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回配置: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("解析配置: %v", err)
	}

	allowed := map[string]bool{
		"base_url": true, "api_key": true, "vision_model": true,
		"text_model": true, "detail": true, "tag_prompt": true,
	}
	for k := range m {
		if !allowed[k] {
			t.Errorf("配置里多了一个字段 %q", k)
		}
	}
}

// 写配置走的是「临时文件 + 改名」：目录不存在也要能建出来。
func TestConfigSaveCreatesDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "vlm.json")
	if err := testConfig().Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("配置没落盘: %v", err)
	}
}

// mustConfigView 读一份配置，返回给界面看的那一份 —— 几条测试只用得上这个。
func mustConfigView(t *testing.T, path string) vlm.ConfigView {
	t.Helper()

	cfg, err := vlm.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg.View(path)
}
