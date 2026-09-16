package digest

import (
	"errors"
	"fmt"
	"time"

	"questionbook/internal/library"
)

// Service 是「每日汇总通知」对前端暴露的那一面，也是本包唯一的测试缝（spec：
// 全应用只有一条缝，在服务层）。它同样只有这一面 —— 宿主那一侧读的是磁盘上的
// 排程文件，不调这里任何方法（宿主是 Java，调不动 Go）。
//
// 它做的事有两条线：往下是「某个本地日到期多少」，往上是「未来若干天什么时候发、发什么」。
// 两条线都只是**算**：本包不改任何错题数据，也不写库。
type Service struct {
	// store 只读那条连接（与 review、tags 共用题库那一条，见 Store）。
	store *Store

	// configPath 是设置文件的落点，schedulePath 是排程文件的落点（由前者的目录推出来）。
	// 两者都来自接线（main.go 的 dataDir() 下），本包不知道应用私有目录在哪。
	configPath   string
	schedulePath string

	// now 是取「现在」的方式。做成字段是为了能在测试里钉死 —— 「今天」是哪一天直接决定
	// 排程里有哪些天、下一次发送是什么时候，用真实时钟就没法断言边界
	// （与 review.Service 的 now 同一个理由）。
	now func() time.Time
}

// Option 给服务补一样可选能力。
type Option func(*Service)

// WithNow 换掉取当前时刻的方式。只有测试会用。
func WithNow(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// NewService 接上错题库与设置文件的落点。
//
// 两个都是接线时必须给的，没有默认值 —— 尤其路径：它由应用私有目录推出来，
// 只有 main.go 知道那个目录在哪（安卓上那是 /data/data/<包名>/files/questionbook）。
//
// **构造时不读盘、不建文件**（与 vlm 同一个规矩）：一个还没配过提醒的应用要能照常
// 拍照、复习、复习完还得能自己重算排程。盘上那份排程什么时候写，由 Refresh 说了算。
func NewService(questions *library.Store, configPath string, opts ...Option) *Service {
	s := &Service{
		store:        NewStore(questions.DB()),
		configPath:   configPath,
		schedulePath: schedulePathFor(configPath),
		now:          time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Config 返回设置的「界面版」。
//
// 读不到也要给一份视图（与 vlm 的 Config 同一个立场）：界面正是靠 Problem 告诉用户
// 那个文件怎么了。文件**不在**不算问题 —— 那是「还没配过」，显示默认值（开着、20:00）。
func (s *Service) Config() ConfigView {
	cfg, err := LoadConfig(s.configPath)
	switch {
	case err == nil:
		return cfg.view(s.configPath, s.schedulePath)
	case errors.Is(err, ErrNotConfigured):
		return DefaultConfig().view(s.configPath, s.schedulePath)
	default:
		// 文件在但坏了：把默认值摆出来（用户改一下就能存回去），同时说清坏在哪。
		v := DefaultConfig().view(s.configPath, s.schedulePath)
		v.Problem = err.Error()
		return v
	}
}

// SetConfig 把设置写下去，并立刻重算排程。
//
// 传进来的是**整份**设置，不是补丁 —— 与 vlm.SetConfig 刻意不同。那边是补丁，因为
// 界面拿不到凭据的值，整份覆盖会把 key 抹掉；这里没有任何秘密，界面读到的是真值，
// 也就能把整份原样送回来。补丁语义在布尔量上还会直接坏掉：「关掉」与「没提这一项」
// 都是 false，分不开。
//
// 先验再写：一个 25 点的时刻存下去也算不出排程，不如当场退回，别让它躺在盘上。
// 写完立刻 Refresh 是**这个方法的重点**：关掉提醒这件事只有落进排程文件才算数
// （Go 没有别的渠道能通知到宿主，见 Refresh）。
func (s *Service) SetConfig(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := cfg.Save(s.configPath); err != nil {
		return err
	}
	if _, err := s.Refresh(); err != nil {
		// 设置已经落盘了，只是排程没跟上。照实分开说，别让用户以为设置也没存上。
		return fmt.Errorf("设置已经存下了，但排程没重算（%v）；重开一次应用或再存一次都行", err)
	}
	return nil
}

// Forecast 返回从今天起 days 天里，每一天到期的错题数。
//
// 「每一天」是**本地日**，边界是那一天的次日零点（见 endOfDay），Count 是累计到那一刻
// 的数量 —— 含更早欠下的，与复习队列同一口径（见 DayCount.Count）。
//
// days 会被收进 [1, 31]（见 clampDays）：这是个「算多久」的量，不是用户输入的数据。
//
// 这个方法**只读不写**：它不落盘、不碰设置，随便调。
func (s *Service) Forecast(days int) ([]DayCount, error) {
	return s.dayCounts(startOfToday(s.now()), clampDays(days))
}

// Next 返回下一次该发的那一条：什么时候发、那一天到期多少、发什么字。
//
// 它**永远有值**：时刻是定死的（今天 cfg.Hour:cfg.Minute，这个点过了就是明天的同一时刻），
// 到点会不会真发看 Count —— 0 就是不发（当天没有到期题）。所以这里没有「有没有下一次」
// 这种问题，也不需要第二个返回值。
//
// 它不落盘（要看落盘的那份用 Refresh）。
func (s *Service) Next() (Slot, error) {
	cfg, err := s.settings()
	if err != nil {
		return Slot{}, err
	}
	return s.slotAt(nextFire(cfg, s.now()))
}

// Refresh 重算未来 HorizonDays 天的排程，落盘，并把刚写下的那份带回来。
//
// 这是本包唯一写盘的地方，也是**宿主那份文件的唯一来路**。它得被这几处调到：
// 应用打开时、设置保存时（SetConfig 内部已经调了）、以及应用退到后台时 ——
// 最后那一处是必要的：用户在退出去之前刚评过几道题，那些新的到期时刻只有到这一刻
// 才落进排程里；不重算的话，第二天那条通知报的数字就少了几道。
//
// 关掉时写出去的是 enabled:false 加空的 slots（见 Schedule.Enabled）。这是「别发了」
// 传达到宿主的**唯一**一条路：宿主是另一个进程里的 Java，Go 这边没有任何办法去叫它
// 取消一个已经排好的闹钟，只能把话写进它到点会读的那个文件里。
//
// 设置文件坏了（不是「不在」）时返回错误且**不写盘**：说不出该几点发，就不该拿默认值
// 悄悄顶替 —— 用户明明配过，被换成 20:00 只会让人以为设置丢了。留下上一份排程还能发，
// 而界面会把 Problem 显示出来让用户改回去。
func (s *Service) Refresh() (Schedule, error) {
	cfg, err := s.settings()
	if err != nil {
		return Schedule{}, err
	}
	now := s.now()

	note := ""
	slots := []Slot{} // 空切片而不是 nil：落盘时是 []，不是 null（见 scheduleFile.Slots）
	if cfg.Enabled {
		fire := nextFire(cfg, now)
		for i := range HorizonDays {
			slot, err := s.slotAt(fire.AddDate(0, 0, i))
			if err != nil {
				return Schedule{}, err
			}
			slots = append(slots, slot)
		}
	} else {
		note = "已关闭每日提醒"
	}

	sched := Schedule{
		Enabled:     cfg.Enabled,
		Hour:        cfg.Hour,
		Minute:      cfg.Minute,
		GeneratedAt: now.Truncate(time.Millisecond), // 截到毫秒：写下去的是 Unix 毫秒
		Slots:       slots,
	}
	if err := writeSchedule(s.schedulePath, sched, note); err != nil {
		return Schedule{}, err
	}
	return sched, nil
}

// ── 内部 ──

// slotAt 算出「落在 at 那个时刻」的一条：那一刻所属的那一天到期多少、发什么字。
//
// at 必须是某一天的 Hour:Minute（nextFire 与它的 AddDate 给的就是），
// 因为判定用的就是**它所在的那一天**，不是那一刻 —— 见 endOfDay。
func (s *Service) slotAt(at time.Time) (Slot, error) {
	n, err := s.store.countDueBy(endOfDay(at))
	if err != nil {
		return Slot{}, err
	}
	title, body := message(n)
	return Slot{
		FireAt: at,
		Date:   at.Format(dateLayout),
		Count:  n,
		Title:  title,
		Body:   body,
	}, nil
}

// dayCounts 从 from 那一天起算 days 天，每天一条。
func (s *Service) dayCounts(from time.Time, days int) ([]DayCount, error) {
	out := make([]DayCount, 0, days)
	for i := range days {
		day := from.AddDate(0, 0, i)
		n, err := s.store.countDueBy(endOfDay(day))
		if err != nil {
			return nil, err
		}
		out = append(out, DayCount{Date: day.Format(dateLayout), Count: n})
	}
	return out, nil
}

// settings 读设置，文件不在就用默认值（第一次装上这个应用时它就该不在）。
//
// 文件**坏了**则原样报错，不拿默认值顶替 —— 理由见 Refresh。
func (s *Service) settings() (Config, error) {
	cfg, err := LoadConfig(s.configPath)
	if err == nil {
		return cfg, nil
	}
	if errors.Is(err, ErrNotConfigured) {
		return DefaultConfig(), nil
	}
	return Config{}, err
}
