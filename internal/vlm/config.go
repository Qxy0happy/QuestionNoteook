package vlm

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Detail 的取值，照服务方文档的原话。空着不放任不管：见 Config.detailOrDefault。
const (
	DetailLow      = "low"      // 降到 512×512
	DetailHigh     = "high"     // 保留原尺寸（为兼容提供，等价于 original）
	DetailOriginal = "original" // 保留原尺寸
	DetailAuto     = "auto"     // 服务方自动选择，当前等同于 original
)

// Config 是 VLM 那一份配置。
//
// 它**不进 SQLite**。三个理由，按重要性排：
//
//   - 配置与凭据本来就不属于错题的数据模型。ADR-0004 已经把「库外的东西」这个模式立在了
//     图片上（库不膨胀、备份不变慢），这里是同一个道理。
//   - 凭据不该跟着导出包一起被打包、被复制来复制去。
//   - 顺带的：库的迁移这会儿正被别的改动追加着，为一份配置去抢那一行不值当。
//
// 落点是应用私有目录下的一个 JSON 文件，与 library.db、cards/ 并列
// （目录来自 main.go 的 dataDir()，路径由接线时传进来）。
//
// 模型名**必须**由这里给（ADR-0005：不得硬编码模型 ID）。所以本包在代码里找不到任何
// 默认模型名：空着就是没配置，调模型的动作会明确报 ErrNotConfigured，
// 而不是偷偷用一个写死的名字跑起来。
type Config struct {
	// BaseURL 是服务方的接入点，不带尾部斜杠。OpenAI 兼容的那几个端点都挂在它下面
	// （对话是 /chat/completions，文件是 /files）。
	BaseURL string `json:"base_url"`

	// APIKey 是凭据。**绝不写进日志、报错或界面**：要报告它的存在，只说有没有、多长。
	APIKey string `json:"api_key"`

	// VisionModel 是能看图的模型名：打标签、就题讨论都走它。
	VisionModel string `json:"vision_model"`

	// TextModel 是纯文本的模型名，没有图片的流量走它。
	//
	// 留这一路是因为视觉模型带 Exp 后缀、官方声明可能被替换，而纯文本那边的活
	// （agent 整理数据、推荐今天做几道）不需要跟着它一起冒险。
	TextModel string `json:"text_model,omitempty"`

	// Detail 是内联传图时的高保真档位，取值见 Detail* 那几个常量。
	//
	// 空着按 original 发（见 detailOrDefault）：票据要求**显式**请求高保真 ——
	// 题面是密集文档，降到 512×512 就看不清了，而这种事不该指望服务方的默认值不变。
	Detail string `json:"detail,omitempty"`

	// TagPrompt 是打标签的提示词，空着用内置的那份（见 prompt.go）。
	//
	// 留这个口子是因为打标签的质量还没有定论（票据 01），措辞要能不改代码就换 ——
	// 真机上试一版措辞比重新构建一个 APK 快得多。
	TagPrompt string `json:"tag_prompt,omitempty"`
}

// Validate 检查这份配置能不能拿去调模型。缺哪一项、错在哪都在这句话里说清楚，
// 而不是等到发请求时收到一个 401 再回头猜。
func (c Config) Validate() error {
	if err := c.validateEndpoint(); err != nil {
		return err
	}
	if strings.TrimSpace(c.VisionModel) == "" {
		return fmt.Errorf("%w: 缺 vision_model", ErrNotConfigured)
	}
	switch c.Detail {
	case "", DetailLow, DetailHigh, DetailOriginal, DetailAuto:
	default:
		return fmt.Errorf("VLM: detail 只能是 low / high / original / auto，给的是 %q", c.Detail)
	}
	return nil
}

// validateEndpoint 只检查「够不够把一次请求发出去」：接入点像个地址、凭据在。
//
// 从 Validate 里单拆出来，是给 Models 那一次请求用的（见 Service.Models）：
// 它恰恰是**为了挑模型名**才发出去的，要求先有模型名就成了先有鸡后有蛋。
// 这里不查模型名，别的什么都不放松 —— 端点与凭据仍然是必须的，那两样缺了
// 连一句像样的报错都拿不到。
func (c Config) validateEndpoint() error {
	if strings.TrimSpace(c.BaseURL) == "" {
		return fmt.Errorf("%w: 缺 base_url", ErrNotConfigured)
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("VLM: base_url 不是个完整的地址: %q", c.BaseURL)
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return fmt.Errorf("%w: 缺 api_key", ErrNotConfigured)
	}
	return nil
}

// detailOrDefault 把「没写」补成 original。
//
// 这是本包唯一一处给配置项兜底的地方，兜的是一个**传输细节**而不是身份：
// 模型名兜底就成了「偷偷用一个写死的模型」，那是 ADR-0005 明令禁止的；
// 而档位兜成「保留原尺寸」只是在落实票据那句「显式请求高保真」。
func (c Config) detailOrDefault() string {
	if strings.TrimSpace(c.Detail) == "" {
		return DetailOriginal
	}
	return c.Detail
}

// modelFor 挑这次该用哪个模型。
//
// 这一句就是票据说的「保留纯文本模型作为非图片流量的**回落**」：
// 带图的活只能走视觉模型；不带图的活走文本模型 —— 视觉模型带 Exp 后缀、
// 官方声明可能被修订或替换，而纯文本那边的活（agent 整理数据、推荐今天做几道）
// 不该跟着它一起冒险。
//
// 本票只用到带图那一路（打标签必然有图），不带图那一路是给后面几张票留的接口：
// 回落点在这里，不在 provider —— provider 只认 Request.Model，不管它怎么来的。
func (c Config) modelFor(withImage bool) string {
	if withImage {
		return c.VisionModel
	}
	return c.TextModel
}

// normalized 把几处手写容易带进来的空白与斜杠收拾干净。
//
// 只在读写配置的边界上做一次，之后的代码可以假定 BaseURL 能直接拼 "/chat/completions"。
func (c Config) normalized() Config {
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	c.APIKey = strings.TrimSpace(c.APIKey)
	c.VisionModel = strings.TrimSpace(c.VisionModel)
	c.TextModel = strings.TrimSpace(c.TextModel)
	c.Detail = strings.TrimSpace(c.Detail)
	return c
}

// withOverrides 用 patch 里的**非空项**覆盖自己，返回合并后的配置。
//
// 为什么是「非空才覆盖」而不是整份替换：界面拿不到凭据的值（本包从不把它读出去），
// 于是它改个模型名时没法把 key 原样带回来。空串=不动，是这里唯一说得通的语义。
// 真要清空某一项，删掉配置文件重来 —— 那种事一年也不会发生一次。
func (c Config) withOverrides(patch Config) Config {
	if strings.TrimSpace(patch.BaseURL) != "" {
		c.BaseURL = patch.BaseURL
	}
	if strings.TrimSpace(patch.APIKey) != "" {
		c.APIKey = patch.APIKey
	}
	if strings.TrimSpace(patch.VisionModel) != "" {
		c.VisionModel = patch.VisionModel
	}
	if strings.TrimSpace(patch.TextModel) != "" {
		c.TextModel = patch.TextModel
	}
	if strings.TrimSpace(patch.Detail) != "" {
		c.Detail = patch.Detail
	}
	if strings.TrimSpace(patch.TagPrompt) != "" {
		c.TagPrompt = patch.TagPrompt
	}
	return c.normalized()
}

// ConfigView 是配置的「界面版」：**没有凭据的值**。
//
// 凭据只进不出。界面要知道的是「配没配」，够用的是有没有与多长，不是那串东西本身 ——
// 票据的硬约束：绝不在代码、注释、日志或界面里打印凭据的值。
type ConfigView struct {
	// Path 是配置文件的落点。显示出来，是因为安卓上这个路径用户平时够不着，
	// 出问题时至少知道该去哪儿找。
	Path string

	// Configured 为真表示这份配置已经能拿去调模型。
	Configured bool

	BaseURL     string
	VisionModel string
	TextModel   string
	Detail      string

	// APIKeySet / APIKeyLength 是凭据的**存在性**与长度，不是它的值。
	APIKeySet    bool
	APIKeyLength int

	// Problem 非空时说明这份配置为什么还不能用（缺了哪一项）。
	Problem string
}

// View 把配置转成给界面看的那一份。path 是这份配置的落点，一并带出去。
func (c Config) View(path string) ConfigView {
	v := ConfigView{
		Path:         path,
		BaseURL:      c.BaseURL,
		VisionModel:  c.VisionModel,
		TextModel:    c.TextModel,
		Detail:       c.detailOrDefault(),
		APIKeySet:    c.APIKey != "",
		APIKeyLength: len(c.APIKey),
	}
	if err := c.Validate(); err != nil {
		v.Problem = err.Error()
	} else {
		v.Configured = true
	}
	return v
}

// LoadConfig 从 path 读配置。
//
// 文件不在时返回 ErrNotConfigured 而不是别的错：那是「还没配」，不是读失败 ——
// 第一次装上这个应用时它就是不在的，应用不该因此起不来。
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, fmt.Errorf("%w: 配置文件 %s 不存在", ErrNotConfigured, path)
	}
	if err != nil {
		return Config{}, fmt.Errorf("VLM: 读配置 %s: %w", path, err)
	}

	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("VLM: 解析配置 %s: %w", path, err)
	}
	return c.normalized(), nil
}

// Save 把配置写到 path。
//
// 权限 0600（里面有凭据），先写同目录的临时文件再原子改名 —— 与题图落盘同一个套路：
// 中途崩了只会留下一个临时文件，不会留下一份被写了一半的配置。
func (c Config) Save(path string) error {
	raw, err := json.MarshalIndent(c.normalized(), "", "  ")
	if err != nil {
		return fmt.Errorf("VLM: 编码配置: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("VLM: 建配置目录: %w", err)
	}

	f, err := os.CreateTemp(dir, "vlm-*.json")
	if err != nil {
		return fmt.Errorf("VLM: 建临时文件: %w", err)
	}
	tmp := f.Name()
	renamed := false
	defer func() {
		f.Close() // 正常路径上已经关过了，这里只会拿到 ErrClosed，忽略
		if !renamed {
			os.Remove(tmp)
		}
	}()

	if _, err := f.Write(raw); err != nil {
		return fmt.Errorf("VLM: 写配置: %w", err)
	}
	// Windows 上改名之前必须先放手，否则句柄还开着。
	if err := f.Close(); err != nil {
		return fmt.Errorf("VLM: 收尾临时文件: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("VLM: 落盘配置 %s: %w", path, err)
	}
	renamed = true
	return nil
}
