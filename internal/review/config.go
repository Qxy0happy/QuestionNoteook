package review

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/open-spaced-repetition/go-fsrs/v4"
)

// dateLayout 是考试日期在设置文件里、也在界面上的形状。
const dateLayout = "2006-01-02"

// DefaultRetention 是期望保留率的默认值。**不是**官方默认的 0.90 —— 这是有意的偏离，
// 理由与那两个数写在 Config.RequestRetention 上。
const DefaultRetention = 0.95

// 保留率允许的范围。上限 0.99 而不是 1.0：官方库的 Validate 判的是 (0, 1]，而 1.0 会让
// 间隔公式里的对数变成 0 —— 那不是「复习得非常密」，是算不出来。下限 0.70 是照 FSRS
// 那边的经验区间收的，再低就接近「随便排」了。
const (
	minRetention = 0.70
	maxRetention = 0.99
)

// ErrBadRetention 表示期望保留率越界。判断用 errors.Is。
var ErrBadRetention = errors.New("期望保留率要在 0.70 到 0.99 之间")

// ErrBadExamDate 表示考试日期读不出来。判断用 errors.Is。
var ErrBadExamDate = errors.New("考试日期要形如 2026-12-19")

// ErrNotConfigured 表示这份设置还没配过（文件不在）。判断用 errors.Is。
//
// 与 digest 那边同名同义：**文件不在不是错误**，是「刚装上」。
var ErrNotConfigured = errors.New("复习参数还没配过")

// Config 是复习参数的设置。**不进 SQLite** —— 照 vlm / digest 的做法（库外 JSON，
// 与 library.db 并列）：配置不属于错题的数据模型，也不该跟着导出包被搬到别的手机上
// （「这个人的考试在 12 月 19 日」对换手机的人没有意义）。
//
// 只有两项（ADR-0008）：FSRS 有 21 个权重、期望保留率、最大间隔、模糊开关，这里只露
// 模糊与考试日期 —— 保留率与手填上限都是多余的旋钮，上限由考试日期推出来。
type Config struct {
	// RequestRetention 是期望保留率：一张卡到下次复习时**还记得**的概率目标。
	// 调高 → 间隔明显变短、复习更密。
	//
	// 默认 **0.95**，比官方默认的 0.90 高 —— 这是本包第二处有意偏离官方默认（第一处是
	// 关掉学习步，见 params）。理由是量出来的（票 08 里那张表）：考研是有截止日期的活，
	// 0.90 时复习过三次的卡算出来是 141/196/318 天，全在考试之外，于是全被上限截到
	// 「距考试 94 天」那附近 —— 四档评级因此挤成 90/91/92，在间隔上完全看不出区别，
	// 而且每一张卡都堆到考前几天一起到期。0.95 时同一张卡是 29/38/56 天：四档分得开，
	// 也没有一张碰到上限。
	RequestRetention float64 `json:"request_retention"`

	// Fuzz 打开官方库的间隔模糊。默认关（与官方默认一致）。
	//
	// 关着的时候同一天评级的题会**永远**撞在同一天到期（今天拍的一批题都评 Good，
	// 196 天后同一天全到期），复习量一阵一阵的。打开之后间隔散开几个百分点。
	// 顺带一句：官方库在间隔小于 2.5 天时不模糊（fuzz.go），所以 Again 那一档仍是准数。
	Fuzz bool `json:"fuzz"`

	// ExamDate 是考试日期，本地日历上的那一天，形如 "2026-12-19"；空 = 没设。
	//
	// 它是**日期**不是时刻：算的是「还剩几个日历天」，与几点钟无关。
	ExamDate string `json:"exam_date"`
}

// DefaultConfig 是没配过时的复习参数：保留率 0.95、模糊关着、没有考试日期。
func DefaultConfig() Config { return Config{RequestRetention: DefaultRetention} }

// retention 返回**实际生效**的保留率。
//
// 0 在这里当「没设」而不是「非法」：装机上那份 `review.json` 是加这个字段**之前**写的，
// 里面没有 `request_retention`，反序列化出来就是 0。要是判它非法，整份设置（连同用户
// 填的考试日期）会被当成坏文件退回默认值 —— 那等于升级一次就把他配的东西丢了。
// 所以旧文件读出来是「用默认值」，而这正是我们想让他得到的值。
func (c Config) retention() float64 {
	if c.RequestRetention == 0 {
		return DefaultRetention
	}
	return c.RequestRetention
}

// Validate 检查这份设置能不能拿去算调度参数。
func (c Config) Validate() error {
	if r := c.retention(); r < minRetention || r > maxRetention {
		return fmt.Errorf("%w，现在给的是 %v", ErrBadRetention, r)
	}
	if c.ExamDate == "" {
		return nil
	}
	if _, err := time.Parse(dateLayout, c.ExamDate); err != nil {
		return fmt.Errorf("%w，现在给的是 %q", ErrBadExamDate, c.ExamDate)
	}
	return nil
}

// daysToExam 返回「距考试还有几个本地日历天」，负数表示已经过去了。
//
// 两个日期都取成 **UTC 零点**再相减，差因此是 24 小时的整数倍。这不是绕远路：
// 拿本地时刻直接相减，遇上夏令时开始/结束的那两天会差出 23 或 25 小时，
// 除下来就少一天或多一天 —— 而「还剩几天」正是本功能唯一要算对的东西。
//
// 日期解析失败时返回 0（当成今天）。Validate 挡在前面，这里只是不让一个坏值把
// 除数变成笑话。
func (c Config) daysToExam(now time.Time) int {
	exam, err := time.Parse(dateLayout, c.ExamDate)
	if err != nil {
		return 0
	}
	ny, nm, nd := now.Date() // 用 now 自己的时区取「今天」，生产里就是设备本地日
	today := time.Date(ny, nm, nd, 0, 0, 0, 0, time.UTC)
	return int(math.Round(exam.Sub(today).Hours() / 24))
}

// maxInterval 返回**生效的最大间隔**（天），以及它是不是被考试日期压下来的。
//
// 没设考试日期、或者考试已经过去，都退回官方默认值：那两种情况都不是「上限为 1 天」。
// 尤其「考过了」这一支 —— 不特判的话天数会变成负数，被官方库的 Validate 判为非法，
// 而它的兜底是**悄悄换回 36500**；我们要么给个合法值，要么明确说自己不管。
func (c Config) maxInterval(now time.Time) (float64, bool) {
	def := fsrs.DefaultParam().MaximumInterval
	if c.ExamDate == "" {
		return def, false
	}
	days := c.daysToExam(now)
	switch {
	case days < 0:
		// 考过了。上限**不生效**，退回官方默认。
		//
		// 不这么办的话天数会被夹到最小 1，于是**每一道题每天都到期**，队列一次炸开 ——
		// 那比「上限不管用」糟得多。
		return def, false
	case days == 0:
		// 就是今天。给 1 天：0 会被官方库判为非法（要 > 0），而它的兜底是悄悄换回 36500，
		// 那就成了「填了考试日期却毫无作用」，比给 1 天更让人摸不着头脑。
		return 1, true
	case float64(days) > def:
		// 考试还远得超出官方上限（一百年）：用官方那档，别去撞它的合法区间。
		return def, true
	}
	return float64(days), true
}

// params 按**这份设置与此刻**算出整套调度参数。
//
// 每次要用的时候现算，不在构造服务时算好存着：上限是「距考试还有几天」，
// 存下来的话过了午夜它就旧一天 —— 而复习恰恰是每天早上的事。
//
// 权重是官方默认向量，从未自训练、也没手工调过（ADR-0002）。没填考试日期时返回的就是
// 「官方默认 + 关掉学习步」—— 与票 08 那版一字不差，这也是 params_test.go 那条
// 「与官方默认只差一处」的断言能继续成立的地方。
//
// ── 关掉学习步（EnableShortTerm）的理由 ──
//
// 官方默认参数带学习步，四档的间隔是**分钟级**的（Again 1 分钟 / Hard 6 分钟 /
// Good 10 分钟），而这个应用是**每日**复习仪式：队列问的是「今天该复习哪些」，
// 通知问的是「今天到期多少」，界面上写的是「今天还剩多少道」。留着分钟级的步长
// 会有两个后果 ——
//
//  1. 「今天还剩多少道」在同一分钟里自己变；
//  2. 刚评过的题一两分钟后又到期、又出现在队列里，与「评过的题在本次队列里不再出现」
//     直接冲突。
//
// 关掉之后走官方库里的 long-term 调度器：它保证四档里最短的一档也至少推到**一天**之后
// （nextInterval 里那句 max(…, 1)，再被 Again ≤ Hard−1 ≤ Good−2 ≤ Easy−3 逐档拉开）。
// 于是「评过的题今天不会再出现」是一条结构上的保证，而不是靠「间隔通常够长」。
func (c Config) params(now time.Time) fsrs.Parameters {
	p := fsrs.DefaultParam()
	p.EnableShortTerm = false
	p.RequestRetention = c.retention()
	p.EnableFuzz = c.Fuzz
	if mi, ok := c.maxInterval(now); ok {
		p.MaximumInterval = mi
	}
	return p
}

// loadConfigFile 读一遍设置文件，返回设置与一句「这份文件有什么问题」（没问题时为空）。
//
// 文件不在（刚装上）与文件坏了，在这里**都**退回默认设置 —— 这与 digest 那边的取向相反，
// 而且是**有意**的：那份设置坏了宁可报错也不拿默认值顶替（提醒在错的时间响比不响更糟），
// 而排程要是瘫掉，这个应用就没有主功能了。所以这里一律退回默认值继续跑，
// 把问题显示在设置页上，让用户自己去改。
func loadConfigFile(path string) (Config, string) {
	cfg, err := LoadConfig(path)
	switch {
	case err == nil:
		return cfg, ""
	case errors.Is(err, ErrNotConfigured):
		return DefaultConfig(), "" // 还没配过，不是问题
	default:
		return DefaultConfig(), err.Error()
	}
}

// Preview 是「按现在这套参数，四档分别会推到多少天之后」。
//
// 两行：新卡（还没复习过）与复习过三次的卡。给出它是因为复习参数这一页唯一要回答的
// 问题就是「这么设之后间隔到底变多长」—— 让用户对着两份配置自己猜，是这一页最容易
// 失败的地方。
//
// 开着模糊时这几行**每次看都不一样**（差几个百分点）—— 那是模糊的定义，不是算错了。
type Preview struct {
	New      []RatingInterval
	Reviewed []RatingInterval
}

// RatingInterval 是预览里的一格：哪一档、多少天之后。Rating 用的就是四档那个类型，
// 界面照自己那套标签显示，不必在这里再写一遍中文名。
type RatingInterval struct {
	Rating Rating
	Days   int
}

// preview 按给出的参数把两行预览算出来。
func preview(params fsrs.Parameters, now time.Time) Preview {
	sched := fsrs.NewFSRS(params)
	reviewed, at := reviewedCard(sched, now)
	return Preview{
		New:      intervals(sched, fsrs.NewCard(now), now, params.MaximumInterval),
		Reviewed: intervals(sched, reviewed, at, params.MaximumInterval),
	}
}

// intervals 算一张卡四档各推多少天。库的 Repeat 一次给全四档，所以这里只调一次。
//
// 每一档都要过一遍 clampToCap —— 与真实评级**同一个收口**。少了这一步，预览会照着库
// 那个软上限显示 95/96 天，而真去做是 94 天：一个说一套做一套的预览，比不做预览还糟。
//
// 算不出来（参数坏到那个地步）时返回 nil：宁可这一行空着，也不要编一个数出来。
func intervals(sched *fsrs.FSRS, card fsrs.Card, at time.Time, maxInterval float64) []RatingInterval {
	log, err := sched.Repeat(card, at)
	if err != nil {
		return nil
	}
	out := make([]RatingInterval, 0, 4)
	for _, r := range []Rating{Again, Hard, Good, Easy} {
		next := clampToCap(log[fsrs.Rating(r)].Card, at, maxInterval)
		out = append(out, RatingInterval{Rating: r, Days: int(next.ScheduledDays)})
	}
	return out
}

// reviewedCard 造一张「复习过三次」的卡，返回它与「下一次复习发生在什么时候」。
//
// 每次都要把时钟推到**到期那一刻**再喂下一档 —— 同一个时刻连喂三次是不现实的情形：
// 那时 elapsedDays 恒为 0，遗忘曲线在 R=1，而稳定度的增长项 exp((1-R)·w10)-1 恰好是 0，
// 四档会一直停在 1/2/3/4（票 08 里我自己踩过这个坑，那张表因此是错的）。
//
// 返回的 at 就是那张卡自己的最后复习时刻：拿它去 Repeat，才不会撞上官方库那句
// 「上次复习在将来」的拒绝 —— 那是它做得对，不该绕过去。
func reviewedCard(sched *fsrs.FSRS, now time.Time) (fsrs.Card, time.Time) {
	card, at := fsrs.NewCard(now), now
	for range 3 {
		info, err := sched.Next(card, at, fsrs.Good)
		if err != nil {
			return card, at // 算不出来就退回手上这张，预览那边顶多显示得朴素些
		}
		card, at = info.Card, info.Card.Due
	}
	return card, at
}

// ConfigView 是复习参数设置的「界面版」。
//
// 与 digest 那份不同，这里的设置**没有任何秘密**：界面拿到是真的值，也是真的值传回来。
type ConfigView struct {
	// Path 是设置文件的落点（安卓上用户平时够不着，出问题时至少知道去哪儿找）。
	Path string

	// RequestRetention 是**实际生效**的那个值（旧设置文件里没这一项时就是默认的 0.95），
	// 不是文件里存的原始值 —— 界面要拿它显示，不该显示一个「0」。
	RequestRetention float64
	Fuzz             bool
	ExamDate         string

	// DaysToExam 是距考试还有几个日历天（负数 = 已经过去）。仅在 ExamDate 非空时有意义。
	DaysToExam int
	// ExamActive 说的是「考试日期这个上限现在有没有生效」—— 没填、或者考过了，都是 false。
	// 界面必须把这件事说出来，否则用户会以为填了没用（或者以为它还在管着）。
	ExamActive bool

	// MaxIntervalDays 是**当前生效的**最大间隔（天）。
	//
	// 没填考试日期时它就是官方默认那个数，一样显示出来 —— 用户要能看出「填了会变成多少」。
	MaxIntervalDays int

	// Preview 是四档在当前参数下的间隔。
	Preview Preview

	// Problem 非空时说明设置文件有问题（读不了、写坏了、日期解析不了），
	// 此时上面几项是**默认值** —— 界面不是白的，用户还能把它改回去。
	Problem string
}

// view 把设置转成给界面看的那一份。
func (c Config) view(path string, now time.Time, problem string) ConfigView {
	mi, active := c.maxInterval(now)
	return ConfigView{
		Path:             path,
		RequestRetention: c.retention(),
		Fuzz:             c.Fuzz,
		ExamDate:         c.ExamDate,
		DaysToExam:      c.daysToExam(now),
		ExamActive:      active,
		MaxIntervalDays: int(mi),
		Preview:         preview(c.params(now), now),
		Problem:         problem,
	}
}

// LoadConfig 从 path 读设置。
//
// 文件不在时返回 **ErrNotConfigured 与默认设置**，而不是零值 —— 那是「还没配过」，
// 不是读失败：刚装上这个应用时它就是不在的，复习不该因此起不来。
//
// 文件在但读坏了（不是 JSON、字段读不出）返回 ErrNotConfigured 之外的错，设置值是零值。
// 调用方（Service.Config）会把默认值端上去、并把这句话显示出来 —— 见 loadConfig 的注释。
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return DefaultConfig(), fmt.Errorf("%w: 设置文件 %s 不存在", ErrNotConfigured, path)
	}
	if err != nil {
		return Config{}, fmt.Errorf("复习参数: 读设置 %s: %w", path, err)
	}

	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("复习参数: 解析设置 %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("复习参数: 设置文件 %s 里的值不能用: %w", path, err)
	}
	return c, nil
}

// Save 把设置写到 path。
//
// 先写同目录的临时文件再原子改名，与 vlm / digest 的配置、题图落盘同一个套路：
// 中途崩了只会留下一个临时文件，不会留下一份被写了一半的设置。
//
// 权限 0600 照抄那两处（这里同样没有凭据，但同一个目录下的几个文件用同一套权限，
// 比每个文件各记一条「为什么是 0644」省心）。
func (c Config) Save(path string) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("复习参数: 编码设置: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("复习参数: 建设置目录: %w", err)
	}

	f, err := os.CreateTemp(dir, "review-*.json")
	if err != nil {
		return fmt.Errorf("复习参数: 建临时文件: %w", err)
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
		return fmt.Errorf("复习参数: 写设置: %w", err)
	}
	// Windows 上改名之前必须先放手，否则句柄还开着。
	if err := f.Close(); err != nil {
		return fmt.Errorf("复习参数: 收尾临时文件: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("复习参数: 落盘设置 %s: %w", path, err)
	}
	renamed = true
	return nil
}
