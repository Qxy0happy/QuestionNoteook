// Package digest 是「每日汇总通知」：算好未来若干天各自有多少错题到期，
// 以及下一次该发的时间，把结果落成一份宿主读得懂的 JSON。
//
// ── 为什么算在 Go 侧，发却在 Java 侧 ──
//
// 通知的判据是「今天到期多少」。那套判据整个在 review 那一侧：到期是 FSRS 算出的**绝对时刻**，
// 「今天」按**本地日**算到次日零点，从没复习过的题拿创建时间顶到期。让 Java 宿主自己去读 SQLite
// 等于把那份判据抄第二遍，还要给宿主再引一套 SQLite —— 而宿主是 Wails 脚手架里的 Java，
// 它连库文件的路径都是拼出来的。
//
// 反过来，让 Go 自己定时发是做不到的：安卓上「应用没打开也要发」是**后台调度**
// （WorkManager / AlarmManager）的事，而调度拉起来的那一刻这个 Go 进程并不存在。
// Wails v3（beta.22）的安卓宿主只给了一个 **立即** 发一条通知的口子
// （application.Android.Notify），没有任何定时能力（见票据 12 的查证结论）。
//
// 于是分工是：**Go 在应用打开时把未来若干天的内容算好落盘，宿主只负责到点念出来。**
// 关键洞察是 `due_at` 是绝对时刻，所以今天就能算出明天、后天各有多少到期，不必等到那天。
//
// ── 落点 ──
//
// 两个文件都在应用私有目录下（与 vlm.json、library.db、cards/ 并列，目录来自 main.go 的
// dataDir()），路径由接线时传进来：
//
//   - <dataDir>/digest.json —— 用户的设置（开不开、几点发）。与 vlm.json 同一个模式。
//   - <dataDir>/digest-schedule.json —— 派生出来的排程，宿主读的就是它。每次重算都整份重写。
//
// 分成两个文件是因为它们是两种东西：设置只在用户点保存时变，排程每次重算都变。
// 混在一个文件里，重算就会去覆盖用户刚改的设置（或者反过来）。
//
// **这个包不写库**，只读。汇总通知不产生任何错题数据，也不需要一张表 ——
// 它是**配置**加**派生数据**，两样都不该进 SQLite（ADR-0004 立下的「库外的东西」那个模式）。
package digest

import (
	"errors"
	"fmt"
	"time"
)

const (
	// DefaultHour / DefaultMinute 是没配过时的发送时刻：晚上八点。
	//
	// 挑晚上是因为这是个**复习提醒**：早上发容易被一天的事冲掉，晚上那会儿才真有空坐下做题。
	DefaultHour   = 20
	DefaultMinute = 0

	// HorizonDays 是排程预算的天数。
	//
	// 它同时是「这份排程能撑多久」：应用连着 HorizonDays 天没打开过，排程就用完了，
	// 之后宿主没有可发的内容（**不发**，而不是发一条数字过期的）。对一个复习应用来说
	// 这个窗口够宽 —— 用户本来就是每天都要打开它做几道的。
	HorizonDays = 14

	// maxForecastDays 是 Forecast 一次最多算多少天。
	// 它是防手滑的：每多算一天就多扫一遍到期集合，一个 100000 传进来不该把手机卡住。
	maxForecastDays = 31

	// dateLayout 是「本地日」在字符串上的形状。
	//
	// 日期在**对外**（给宿主、给界面）一律用这个形状而不是裸的 time.Time：
	// 一个裸的时刻很容易在别处被再换算一次时区，而「9 月 16 日」是个日历上的日子，
	// 换算不动它。与库里存时刻（Unix 毫秒 UTC）的口径也不冲突 —— 见 endOfDay。
	dateLayout = "2006-01-02"
)

// ErrNotConfigured 表示还没有这份设置（文件不在）。判断用 errors.Is。
//
// 与 vlm 那边同一个意思：第一次装上这个应用时文件就是不在的，那不是读失败，
// 按默认值跑是正常状态。
var ErrNotConfigured = errors.New("还没配过每日提醒")

// ErrBadTime 表示发送时刻不在合法范围里。判断用 errors.Is。
//
// 时刻是个 **0-23 点、0-59 分**的钟点，不是绝对时刻：用户设的是「每天 20:00」，
// 那个 20:00 跟着设备时区走（见 Service.Next 的注释）。
var ErrBadTime = errors.New("发送时刻只能是 0-23 点、0-59 分")

// DayCount 是某一天到期的错题数。
type DayCount struct {
	// Date 是本地日，形如 "2026-09-16"。
	Date string

	// Count 是**那一天结束时**累计到期的错题数：含更早欠下的。
	//
	// 口径与复习队列一致（review.Service.Queue 取的也是「到期日不晚于今天的全部」），
	// 所以通知上的数字与用户点进去看到的队列长度是同一个 —— 汇总说 5 道、
	// 点进去只有 3 道，是这条口径在撑着的两处必须对齐的理由。
	Count int
}

// Slot 是一次汇总：什么时候发、发什么。
type Slot struct {
	// FireAt 是这次该发的时刻。它是一个**定时钟点**在某个本地日上的落点
	// （那一天的 Hour:Minute），不是「从现在算起的第几毫秒」。
	FireAt time.Time

	// Date 是那一天的本地日，形如 "2026-09-16"。
	//
	// 宿主拿它核对「我发的确实是这一天的」：闹钟可能被系统推迟到第二天才醒，
	// 那时候这条就不该再发了（也正是靠它，宿主能分辨「今天没有到期题」与
	// 「这份排程根本没覆盖今天」）。
	Date string

	// Count 是这一天到期的错题数；**0 表示到点不发**（ticket：当天没有到期题则不发）。
	Count int

	// Title / Body 是这条通知要显示的字，Go 侧就算好。
	//
	// 放在这里而不是让宿主自己拼：验收项「内容包含当天到期的数量」于是能在 Go 的测试里
	// 钉住，而不是散在 Java 里没法测 —— 宿主那一侧只有「念出来」一件事。
	Title string
	Body  string
}

// Schedule 是交给宿主的那份排程：未来 HorizonDays 天里哪天几点发、发什么。
//
// 它是**只读给宿主**的一份快照。宿主不读数据库、不读设置文件，只读它 —— 那正是
// 「宿主不必引一套 SQLite」这句话的落点。
type Schedule struct {
	// Enabled 为 false 时 Slots 一定是空的：关掉这件事在两处同时说出来，
	// 宿主少判一个条件就少一次误发的机会。
	Enabled bool

	Hour   int
	Minute int

	// GeneratedAt 是这份排程算出来的时刻。宿主可以据此判断它有多旧。
	GeneratedAt time.Time

	// Slots 从最近的一次算起，一天一条。空切片而不是 nil（前端要拿到 []，不是 null）。
	Slots []Slot
}

// endOfDay 返回 t 所在那一天的结束，也就是**次日零点**；时区取 t 自己的。
//
// 这是本包唯一涉及时区的地方，与 review 的 endOfToday 是同一句话，**刻意抄了一份**：
// 那边的同名函数没有导出，而两个包对「今天」的边界必须是同一条线 —— 汇总上的数字
// 与复习队列的长度要对得上（见 DayCount.Count）。两边一旦分叉，
// digest 的 TestForecastAgreesWithReviewQueue 会当场翻掉，那条测试就是这条线的看门人。
//
// 库里存的都是绝对时刻（Unix 毫秒 UTC），而用户看的是**本地日历**，两者之间差着时区。
// 整个判定里唯一涉及时区的地方就是这里的「今天」：把它换算成一条绝对时刻的界线
// （次日零点），剩下的就是一次纯粹的比大小。用 t 自己的时区而不是写死 Local，
// 是为了让测试能传一个固定时区的时刻进来 —— 断言才不会跟着跑测试的机器所在的时区变。
func endOfDay(t time.Time) time.Time {
	y, m, d := t.Date() // 用 t 自己的时区取日期
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location()).AddDate(0, 0, 1)
}

// startOfToday 返回 t 所在那一天的开始，也就是**当天零点**，时区取 t 自己的。
func startOfToday(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// nextFire 返回下一次该发的时刻：今天 cfg.Hour:cfg.Minute，这个点已经过了就是明天的同一时刻。
//
// 为什么是「钟点在本地日上的落点」而不是别的：用户设的是「每天 20:00」，那是个**墙上时间**，
// 所以它必须跟着设备时区走（换个时区，提醒跟着新时区走 —— 用户要的是「当地晚上八点」）。
// 而库里存的 `due_at` 是绝对时刻，两者在 endOfDay 那条界线上对齐。
//
// 边界取 !After 而不是 Before：正好落在这一刻时算「已经过了」，推到明天。
// 应用恰好在 20:00:00.000 这一刻算排程，那条通知该不该现在发是个没法回答的问题
// （此刻发已经是「刚才」了），推到明天是唯一不会重复发一条的选择。
func nextFire(cfg Config, now time.Time) time.Time {
	at := time.Date(now.Year(), now.Month(), now.Day(), cfg.Hour, cfg.Minute, 0, 0, now.Location())
	if !at.After(now) {
		at = at.AddDate(0, 0, 1)
	}
	return at
}

// message 拼出这一条通知的两个字段。
//
// 措辞是「今天」而不是「明天」：这条 Slot 会在它自己那一天被念出来（见 Slot.Date），
// 所以从宿主的嘴里说出来的永远是「今天」。
func message(count int) (title, body string) {
	return "错题本", fmt.Sprintf("今天有 %d 道待复习", count)
}

// clampDays 把 Forecast 要的天数收进 [1, maxForecastDays]。
//
// 收而不是报错：这是个「算多久」的量，不是用户输入的数据 —— 界面上多算一天少算一天
// 没有对错之分，为此让整个调用失败只会让界面显示不出来。传 0 当 1 天，传 1000 当 31 天。
func clampDays(days int) int {
	if days < 1 {
		return 1
	}
	if days > maxForecastDays {
		return maxForecastDays
	}
	return days
}
