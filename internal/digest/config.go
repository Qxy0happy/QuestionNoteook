package digest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Config 是每日汇总通知的设置。**不进 SQLite**。
//
// 它照的是 vlm 那份配置的做法（库外 JSON，与 library.db 并列），理由也一样：
// 配置不属于错题的数据模型，也不该跟着导出包被打包到别的手机上 ——
// 「这个手机上的提醒设在晚上八点」对换手机的人没有意义。
type Config struct {
	// Enabled 是总开关（ticket 的验收项：用户能关掉它）。
	//
	// 关掉之后排程文件里会写成 enabled:false 且 slots 为空（见 Schedule.Enabled），
	// 那是宿主唯一需要看的一件事 —— Go 没有别的渠道能通知到它，见 Service.Refresh。
	Enabled bool `json:"enabled"`

	// Hour / Minute 是每天发的那一刻，本地时间。
	//
	// 它是个**钟点**不是时刻：换时区之后提醒跟着新时区走。范围由 Validate 卡住。
	Hour   int `json:"hour"`
	Minute int `json:"minute"`
}

// DefaultConfig 是没配过时的设置：开着，晚上八点。
//
// 默认**开着**是因为它就是票据要的功能（story 40：容易忘的人想每天收到一条）；
// 关掉它是设置页里的一个开关，不是安装时要过的一道关。
func DefaultConfig() Config {
	return Config{Enabled: true, Hour: DefaultHour, Minute: DefaultMinute}
}

// Validate 检查这份设置能不能拿去算排程。错在哪都在这句话里说清楚。
func (c Config) Validate() error {
	if c.Hour < 0 || c.Hour > 23 {
		return fmt.Errorf("%w: 点给的是 %d", ErrBadTime, c.Hour)
	}
	if c.Minute < 0 || c.Minute > 59 {
		return fmt.Errorf("%w: 分给的是 %d", ErrBadTime, c.Minute)
	}
	return nil
}

// ConfigView 是设置的「界面版」。
//
// 与 vlm 那个不同，这里**没有**「凭据只出不进」那回事：这份设置没有任何秘密，
// 界面拿到是真的值，也是真的值传回来（见 Service.SetConfig）。
type ConfigView struct {
	// Path 是设置文件的落点、SchedulePath 是排程文件的落点。
	//
	// 两个都显示出来是因为安卓上这些路径用户平时够不着，出问题时至少知道该去哪儿找
	// （与 vlm 的 ConfigView.Path 同一个理由）。排程那份尤其值得露出来：
	// 它才是宿主真正读的东西，「我明明开着提醒可它就是不响」第一个要看的就是它。
	Path         string
	SchedulePath string

	Enabled bool
	Hour    int
	Minute  int

	// HorizonDays 是排程预算的天数，界面拿它说清「这份预告能撑多久」。
	HorizonDays int

	// Problem 非空时说明设置文件有问题（读不了、写坏了），此时上面几项是**默认值** ——
	// 界面不是白的，用户还能把它改回去。
	Problem string
}

// view 把设置转成给界面看的那一份。
func (c Config) view(configPath, schedulePath string) ConfigView {
	return ConfigView{
		Path:         configPath,
		SchedulePath: schedulePath,
		Enabled:      c.Enabled,
		Hour:         c.Hour,
		Minute:       c.Minute,
		HorizonDays:  HorizonDays,
	}
}

// LoadConfig 从 path 读设置。
//
// 文件不在时返回 ErrNotConfigured 与**默认设置**，而不是一个零值：那是「还没配过」，
// 不是读失败 —— 第一次装上这个应用时它就是不在的，应用不该因此起不来，
// 界面上也该显示那份默认值（开着、20:00）而不是「00:00 关闭」。
//
// 文件在但读坏了（不是 JSON、字段越界）返回 ErrNotConfigured 之外的错，设置值是零值：
// 那种情况下**不该**拿默认值硬跑 —— 用户明明配过，悄悄换成 20:00 只会让人以为设置丢了。
// 调用方（Service.Config）会把 Problem 显示出来。
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return DefaultConfig(), fmt.Errorf("%w: 设置文件 %s 不存在", ErrNotConfigured, path)
	}
	if err != nil {
		return Config{}, fmt.Errorf("每日提醒: 读设置 %s: %w", path, err)
	}

	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("每日提醒: 解析设置 %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("每日提醒: 设置文件 %s 里的值不能用: %w", path, err)
	}
	return c, nil
}

// Save 把设置写到 path。
//
// 先写同目录的临时文件再原子改名，与 vlm 的配置、题图落盘同一个套路：
// 中途崩了只会留下一个临时文件，不会留下一份被写了一半的设置。
//
// 权限 0600 照抄 vlm 那边。这里没有凭据，本可以放宽 —— 但同一个目录下的几个文件
// 用同一套权限，比每个文件各记一条「为什么是 0644」省心。
func (c Config) Save(path string) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("每日提醒: 编码设置: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("每日提醒: 建设置目录: %w", err)
	}

	f, err := os.CreateTemp(dir, "digest-*.json")
	if err != nil {
		return fmt.Errorf("每日提醒: 建临时文件: %w", err)
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
		return fmt.Errorf("每日提醒: 写设置: %w", err)
	}
	// Windows 上改名之前必须先放手，否则句柄还开着。
	if err := f.Close(); err != nil {
		return fmt.Errorf("每日提醒: 收尾临时文件: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("每日提醒: 落盘设置 %s: %w", path, err)
	}
	renamed = true
	return nil
}
