package digest_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"questionbook/internal/digest"
	"questionbook/internal/library"
	"questionbook/internal/review"
)

// 全应用只有一条测试缝：服务层（spec）。这里从头到尾只调 digest.Service 的方法，
// 不断言它内部调了谁几次，也不启动 Wails。
//
// 唯一的例外是**读那个排程文件**（TestRefreshWritesWhatTheHostReads）：那份 JSON 的
// 读者不在这个仓库里 —— 它是 build/android 那个 Java 宿主补丁要按着读的东西 ——
// 所以它的形状没法从 Go 这一侧「调服务」观察出来，只能照着契约把它读一遍。
// 那正是这条测试存在的理由（与 main_test.go 测 packageFromCmdline 是同一类例外：
// 有一处接缝在进程外，不钉住就只能靠真机手验）。

// zone 是测试用的固定时区：UTC+8。
//
// 刻意钉死一个时区，而不是用 time.Local —— 「今天」是排程判定里唯一涉及时区的地方，
// 跟着跑测试的机器走的话，同一份断言在 CI（多半是 UTC）和开发机上会给出不同的结果。
var zone = time.FixedZone("UTC+8", 8*60*60)

// base 是测试的时间基准：2026-09-16 10:00（UTC+8）。默认的发送时刻是 20:00，
// 所以基准这一刻「今天的 20:00 还没到」—— 排程从今天起算。
var base = time.Date(2026, 9, 16, 10, 0, 0, 0, zone)

// dayAt 返回 base 起第 days 天的 hour 点（本地时区）。
//
// 造题的时间全从这里来，免得某一处的手算把「哪一天」写歪 —— 本包的断言全落在天上的。
func dayAt(days, hour int) time.Time {
	return time.Date(base.Year(), base.Month(), base.Day(), hour, 0, 0, 0, zone).AddDate(0, 0, days)
}

// localDate 返回 base 起第 days 天的本地日，形如 "2026-09-16"。
func localDate(days int) string { return dayAt(days, 0).Format("2006-01-02") }

// clock 是一个可以拨的假时钟（与 review 的测试同一个形状）。
type clock struct {
	zone *time.Location
	t    time.Time
}

func newClock(at time.Time) *clock { return &clock{zone: at.Location(), t: at} }

func (c *clock) Now() time.Time { return c.t }

// Set 把时钟拨到某个绝对时刻，但保留测试时区 —— 服务算「今天」用的就是这个时区。
func (c *clock) Set(at time.Time) { c.t = at.In(c.zone) }

// env 是一整套接线，与 main.go 里那份同一个形状：两个服务共用题库那一条连接。
type env struct {
	svc       *digest.Service
	rev       *review.Service
	lib       *library.Store
	clk       *clock
	cfgPath   string
	schedPath string

	// start 是这套环境开局时的时刻。造数据的辅助函数会把时钟拨来拨去
	// （见 addDue），拨完得拨回**这里**而不是拨回 base —— 否则「今天几点」就被
	// 造数据的手顺带改掉了，断言跟着一起歪。
	start time.Time
}

// newEnv 在临时目录里搭一套「错题库 + 复习 + 汇总通知」。
func newEnv(t *testing.T, at time.Time) *env {
	t.Helper()

	lib, err := library.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	t.Cleanup(func() { lib.Close() })

	clk := newClock(at)
	cfgPath := filepath.Join(t.TempDir(), "digest.json")
	return &env{
		svc:       digest.NewService(lib, cfgPath, digest.WithNow(clk.Now)),
		rev:       review.NewService(lib, filepath.Join(t.TempDir(), "review.json"), review.WithNow(clk.Now)),
		lib:       lib,
		clk:       clk,
		cfgPath:   cfgPath,
		schedPath: filepath.Join(filepath.Dir(cfgPath), "digest-schedule.json"),
		start:     at,
	}
}

// addQuestion 落一道错题，创建时间显式给。
func addQuestion(t *testing.T, lib *library.Store, hash string, at time.Time) library.Question {
	t.Helper()

	q, err := lib.AddQuestion(library.Question{QuestionHash: hash, CreatedAt: at})
	if err != nil {
		t.Fatalf("AddQuestion(%q): %v", hash, err)
	}
	return q
}

// addDue 落一道**到期日正好是 base 起第 day 天**的错题。
//
// 做法是：在第 day-1 天那一刻拍下并评 Again —— Again 是四档里最近的一档，正好推一天
// （review 那边的 TestQueueUsesLocalDay 钉的就是这件事）。于是「哪一天到期」由这一句
// 直接写死，不必去猜 FSRS 会推多远。做完把时钟拨回这套环境开局的那一刻，
// 别让造数据的时间漏给断言。
func addDue(t *testing.T, e *env, hash string, day int) library.Question {
	t.Helper()

	when := dayAt(day-1, 10)
	e.clk.Set(when)
	q := addQuestion(t, e.lib, hash, when)
	if _, err := e.rev.Grade(q.ID, review.Again); err != nil {
		t.Fatalf("Grade(%q): %v", hash, err)
	}
	e.clk.Set(e.start)
	return q
}

// forecast 取未来几天的到期数，失败即终止测试。
func forecast(t *testing.T, svc *digest.Service, days int) []digest.DayCount {
	t.Helper()

	got, err := svc.Forecast(days)
	if err != nil {
		t.Fatalf("Forecast(%d): %v", days, err)
	}
	return got
}

// counts 把预报摊成数字，方便按天断言。
func counts(days []digest.DayCount) []int {
	out := make([]int, 0, len(days))
	for _, d := range days {
		out = append(out, d.Count)
	}
	return out
}

// assertCounts 断言每天的到期数与 want 完全一致。
func assertCounts(t *testing.T, got []digest.DayCount, want ...int) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("算出来 %d 天 %v，想要 %d 天 %v", len(got), counts(got), len(want), want)
	}
	for i := range want {
		if got[i].Count != want[i] {
			t.Fatalf("算出来 %v，想要 %v", counts(got), want)
		}
	}
}

// ── 那个排程文件：宿主读的形状 ──
//
// 下面这两个结构体是**照着契约**写的，不是照 Go 侧的类型抄的：宿主是 Java
// （脚手架 minSdk 21，用不上 java.time），它读的就是这几个字段名与 epoch 毫秒。
// 实现里改了字段名而这里没改，解析出来的就是零值，断言会当场翻掉。

type hostFile struct {
	Enabled       bool       `json:"enabled"`
	Hour          int        `json:"hour"`
	Minute        int        `json:"minute"`
	GeneratedAtMS int64      `json:"generated_at_ms"`
	Note          string     `json:"note"`
	Slots         []hostSlot `json:"slots"`
}

type hostSlot struct {
	FireAtMS int64  `json:"fire_at_ms"`
	Date     string `json:"date"`
	Count    int    `json:"count"`
	Title    string `json:"title"`
	Body     string `json:"body"`
}

// readHostFile 把宿主会读到的那份文件读出来。
func readHostFile(t *testing.T, path string) (hostFile, string) {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读排程文件 %s: %v", path, err)
	}
	var hf hostFile
	if err := json.Unmarshal(raw, &hf); err != nil {
		t.Fatalf("解析排程文件: %v\n原文：%s", err, raw)
	}
	return hf, string(raw)
}

// ── 票据 12 的核心：未来若干天各自的到期数 ──

// 今天就能算出明天、后天各有多少到期 —— 那是这张票的全部立足点（due_at 是绝对时刻）。
// 而且计数是**累计**的：含更早欠下的，与复习队列同一口径。
func TestForecastCountsEveryUpcomingDay(t *testing.T) {
	e := newEnv(t, base)

	addDue(t, e, "sha256:今天到期", 0)
	addDue(t, e, "sha256:明天到期", 1)
	addDue(t, e, "sha256:大后天到期", 3)

	got := forecast(t, e.svc, 5)
	assertCounts(t, got, 1, 2, 2, 3, 3)

	// 日期也要对：第一天是**今天**，不是明天，也不是「从这一刻起的 24 小时」。
	for i, d := range got {
		if want := localDate(i); d.Date != want {
			t.Errorf("第 %d 天的日期是 %q，想要 %q", i, d.Date, want)
		}
	}
}

// 空库：每天都是 0，而不是报错、也不是空切片。
//
// 「当天没有到期题则不发」靠的就是这个 0（ticket 的验收项）—— 宿主看到 count 0 就不发。
func TestForecastOnEmptyLibraryIsZeroEveryDay(t *testing.T) {
	e := newEnv(t, base)

	got := forecast(t, e.svc, 3)
	if got == nil {
		t.Fatal("预报是 nil，前端要拿到 [] 不是 null")
	}
	assertCounts(t, got, 0, 0, 0)
}

// days 会被收进 [1, 31]（见 clampDays）：这是个「算多久」的量，不是用户输入的数据，
// 为此让整个调用失败只会让界面显示不出来。
func TestForecastClampsDays(t *testing.T) {
	e := newEnv(t, base)

	if got := forecast(t, e.svc, 0); len(got) != 1 {
		t.Errorf("Forecast(0) 给了 %d 天，想要 1 天", len(got))
	}
	if got := forecast(t, e.svc, -5); len(got) != 1 {
		t.Errorf("Forecast(-5) 给了 %d 天，想要 1 天", len(got))
	}
	if got := forecast(t, e.svc, 1000); len(got) != 31 {
		t.Errorf("Forecast(1000) 给了 %d 天，想要 31 天", len(got))
	}
}

// 本包那条计数查询与 review.dueItems 是**故意**的两份（见 store.go 的注释），
// 这条测试就是挡它们分叉的那道闸：逐日把本包算出的数与复习队列的长度对一遍。
//
// 用 review.WithNow 把一个**临时**的复习服务钉在那一天的中午 —— 它的队列就是
// 「那一天到期的全部」。这是那个开关的正经用法（换掉「现在」，只有测试会用）。
//
// review 那边一旦改了判定（换边界、加筛选、动 COALESCE），这条会当场翻掉，
// 而不是等到用户发现通知上的数字与点进去看到的对不上。
func TestForecastAgreesWithReviewQueue(t *testing.T) {
	e := newEnv(t, base)

	addDue(t, e, "sha256:今天到期", 0)
	addDue(t, e, "sha256:明天到期", 1)
	addDue(t, e, "sha256:大后天到期", 3)

	got := forecast(t, e.svc, 6)
	for i, d := range got {
		noon := dayAt(i, 12)
		rs := review.NewService(e.lib, filepath.Join(t.TempDir(), "review.json"), review.WithNow(func() time.Time { return noon }))
		items, err := rs.Queue()
		if err != nil {
			t.Fatalf("第 %d 天的 Queue: %v", i, err)
		}
		if len(items) != d.Count {
			t.Errorf("第 %d 天（%s）：汇总算的是 %d 道，复习队列里是 %d 道 —— 两处口径分叉了",
				i, d.Date, d.Count, len(items))
		}
	}
}

// ── 「下一次该发的时间」 ──

func TestNextIsTodayWhenTheHourHasNotPassed(t *testing.T) {
	e := newEnv(t, base) // 今天 10:00，默认 20:00
	addDue(t, e, "sha256:今天到期", 0)

	slot, err := e.svc.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if want := dayAt(0, 20); !slot.FireAt.Equal(want) {
		t.Errorf("下一次该发 = %s，想要 %s", slot.FireAt, want)
	}
	if slot.Date != localDate(0) {
		t.Errorf("日期是 %q，想要 %q", slot.Date, localDate(0))
	}
	if slot.Count != 1 {
		t.Errorf("那一天到期 %d 道，想要 1 道", slot.Count)
	}
	if !strings.Contains(slot.Body, "1") {
		t.Errorf("正文是 %q —— 里面没有当天到期的数量", slot.Body)
	}
	if slot.Title == "" {
		t.Error("标题是空的")
	}
}

// 今天的这个点已经过了（含正好落在这一刻），下一次就是明天的同一时刻。
func TestNextRollsOverToTomorrow(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   time.Time
	}{
		{"已经过了", dayAt(0, 21)},
		{"正好落在这一刻", dayAt(0, 20)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.at)
			addDue(t, e, "sha256:明天到期", 1)

			slot, err := e.svc.Next()
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if want := dayAt(1, 20); !slot.FireAt.Equal(want) {
				t.Errorf("下一次该发 = %s，想要 %s", slot.FireAt, want)
			}
			if slot.Date != localDate(1) {
				t.Errorf("日期是 %q，想要 %q", slot.Date, localDate(1))
			}
			if slot.Count != 1 {
				t.Errorf("明天到期 %d 道，想要 1 道", slot.Count)
			}
		})
	}
}

// 下一次那个时刻没有到期题时，Count 是 0 —— 宿主据此**不发**（ticket 的验收项）。
// 「有没有下一次」不是问题：时刻总是定死的。
func TestNextReportsZeroWhenNothingIsDue(t *testing.T) {
	e := newEnv(t, base)
	addDue(t, e, "sha256:三天后到期", 3)

	slot, err := e.svc.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if slot.Count != 0 {
		t.Errorf("今天到期 %d 道，想要 0 道（那道题是大后天的）", slot.Count)
	}
}

// 设置里换了钟点，下一次就跟着换。
func TestNextFollowsConfiguredHour(t *testing.T) {
	e := newEnv(t, base) // 今天 10:00
	if err := e.svc.SetConfig(digest.Config{Enabled: true, Hour: 7, Minute: 30}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	slot, err := e.svc.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	// 今天 07:30 已经过了（现在 10:00），所以是明天 07:30。
	if want := dayAt(1, 7).Add(30 * time.Minute); !slot.FireAt.Equal(want) {
		t.Errorf("下一次该发 = %s，想要 %s", slot.FireAt, want)
	}
}

// ── 落给宿主的那份排程 ──

func TestRefreshWritesWhatTheHostReads(t *testing.T) {
	e := newEnv(t, base)
	addDue(t, e, "sha256:今天到期", 0)
	addDue(t, e, "sha256:明天到期", 1)

	sched, err := e.svc.Refresh()
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !sched.Enabled {
		t.Error("默认设置是开着的，排程却说关着")
	}
	if len(sched.Slots) != digest.HorizonDays {
		t.Fatalf("排程有 %d 条，想要 %d 条", len(sched.Slots), digest.HorizonDays)
	}

	hf, raw := readHostFile(t, e.schedPath)

	if !hf.Enabled {
		t.Error("文件里 enabled 是 false，想要 true")
	}
	if hf.Hour != digest.DefaultHour || hf.Minute != digest.DefaultMinute {
		t.Errorf("文件里是 %02d:%02d，想要 %02d:%02d",
			hf.Hour, hf.Minute, digest.DefaultHour, digest.DefaultMinute)
	}
	if hf.GeneratedAtMS == 0 {
		t.Error("文件里没有 generated_at_ms —— 宿主没法知道这份排程有多旧")
	}
	if len(hf.Slots) != digest.HorizonDays {
		t.Fatalf("文件里有 %d 条，想要 %d 条", len(hf.Slots), digest.HorizonDays)
	}
	// 落盘的条数必须与服务返回的是同一份，不是各算一遍。
	if len(hf.Slots) != len(sched.Slots) {
		t.Fatalf("文件里 %d 条，服务返回 %d 条", len(hf.Slots), len(sched.Slots))
	}

	// 与 Forecast 的数逐个对齐：同一份数据不能有两个答案。
	want := forecast(t, e.svc, digest.HorizonDays)
	for i, s := range hf.Slots {
		if s.Date != want[i].Date {
			t.Errorf("第 %d 条的日期是 %q，Forecast 给的是 %q", i, s.Date, want[i].Date)
		}
		if s.Count != want[i].Count {
			t.Errorf("第 %d 天的数量是 %d，Forecast 给的是 %d", i, s.Count, want[i].Count)
		}
		// 时刻必须是那一天**本地**的 Hour:Minute，还得是 epoch 毫秒
		// （宿主是 Java，minSdk 21 用不上 java.time，字符串时刻它啃不动）。
		if w := dayAt(i, digest.DefaultHour); s.FireAtMS != w.UnixMilli() {
			t.Errorf("第 %d 条的 fire_at_ms 是 %d，想要 %d（%s）",
				i, s.FireAtMS, w.UnixMilli(), w)
		}
	}

	// 通知的文案是 Go 侧算好的：验收项「内容包含当天到期的数量」由它在 Go 的测试里钉住。
	if hf.Slots[0].Title == "" {
		t.Error("第一条没有标题")
	}
	if !strings.Contains(hf.Slots[0].Body, "1") {
		t.Errorf("第一条的正文是 %q —— 里面没有当天到期的数量", hf.Slots[0].Body)
	}
	if !strings.Contains(hf.Slots[1].Body, "2") {
		t.Errorf("第二条的正文是 %q —— 里面没有那天到期的数量", hf.Slots[1].Body)
	}

	// 空的那些天照样写下来：宿主靠「这一天在不在排程里」分辨
	// 「今天确实没有到期题（不发）」与「这份排程根本没覆盖今天（也别乱发）」。
	if hf.Slots[2].Count != 2 {
		t.Errorf("第三天有 %d 道到期，想要 2 道（累计口径，含欠下的）", hf.Slots[2].Count)
	}

	// 落盘的形状对得上：空切片是 []，不是 null —— 宿主是 Java，少一个 null 检查就少一次 NPE。
	if hf.Slots == nil {
		t.Error("文件里的 slots 是 null，想要 []")
	}
	if !strings.Contains(raw, `"slots"`) {
		t.Errorf("文件里没有 slots 字段：%s", raw)
	}
}

// 关了之后，排程文件同时用两处说这件事：enabled:false 与空的 slots。
//
// 这是「别发了」传达到宿主的唯一一条路 —— Go 这边没有任何办法去叫它取消一个
// 已经排好的闹钟，只能把话写进它到点会读的那个文件里。
func TestRefreshWhenDisabledSaysSoInTheFile(t *testing.T) {
	e := newEnv(t, base)
	addDue(t, e, "sha256:今天到期", 0)

	if err := e.svc.SetConfig(digest.Config{Enabled: false, Hour: 21, Minute: 30}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	hf, raw := readHostFile(t, e.schedPath)

	if hf.Enabled {
		t.Error("关掉之后文件里 enabled 还是 true")
	}
	if len(hf.Slots) != 0 {
		t.Errorf("关掉之后还有 %d 条排程，想要 0 条", len(hf.Slots))
	}
	if hf.Slots == nil {
		t.Error("文件里的 slots 是 null，想要 []")
	}
	if strings.Contains(raw, `"slots": null`) {
		t.Errorf("空排程落成了 null：%s", raw)
	}
	// 钟点照写：用户下次打开这一页时要看到自己设的是几点。
	if hf.Hour != 21 || hf.Minute != 30 {
		t.Errorf("文件里是 %02d:%02d，想要 21:30", hf.Hour, hf.Minute)
	}
	if hf.Note == "" {
		t.Error("关掉时没有留一句话说明排程为什么是空的")
	}

	// 关掉**不影响算**：界面照样要显示「今天有几道」「下次是几点」。
	if got := forecast(t, e.svc, 1); got[0].Count != 1 {
		t.Errorf("关掉之后今天算出来 %d 道，想要 1 道 —— 关的是通知，不是复习", got[0].Count)
	}
	if _, err := e.svc.Next(); err != nil {
		t.Errorf("关掉之后 Next 报错: %v", err)
	}
}

// 数据变了，重算就看得见 —— 排程不是算一次就冻住的。
func TestRefreshPicksUpNewQuestions(t *testing.T) {
	e := newEnv(t, base)

	sched, err := e.svc.Refresh()
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if sched.Slots[0].Count != 0 {
		t.Fatalf("空库时今天有 %d 道到期，想要 0 道", sched.Slots[0].Count)
	}

	addDue(t, e, "sha256:今天到期", 0)

	sched, err = e.svc.Refresh()
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if sched.Slots[0].Count != 1 {
		t.Errorf("新题入库之后今天有 %d 道到期，想要 1 道", sched.Slots[0].Count)
	}
}

// 排程落在设置文件旁边，名字是固定的 —— 宿主按名字找它，改名就等于没了。
func TestScheduleLivesNextToTheConfig(t *testing.T) {
	e := newEnv(t, base)
	if _, err := e.svc.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if filepath.Dir(e.schedPath) != filepath.Dir(e.cfgPath) {
		t.Fatalf("排程在 %s，设置文件在 %s —— 两个不在一起", e.schedPath, e.cfgPath)
	}
	if _, err := os.Stat(e.schedPath); err != nil {
		t.Fatalf("排程文件不在 %s: %v", e.schedPath, err)
	}
}

// ── 设置 ──

// 没配过（文件不在）不是错误：显示默认值（开着、20:00），Problem 空。
func TestConfigDefaultsWhenNeverConfigured(t *testing.T) {
	e := newEnv(t, base)

	v := e.svc.Config()
	if v.Problem != "" {
		t.Errorf("还没配过就报了问题：%s", v.Problem)
	}
	if !v.Enabled || v.Hour != digest.DefaultHour || v.Minute != digest.DefaultMinute {
		t.Errorf("默认设置是 %v %02d:%02d，想要 true 20:00", v.Enabled, v.Hour, v.Minute)
	}
	if v.Path == "" || v.SchedulePath == "" {
		t.Error("两个路径都要带出来 —— 安卓上用户够不着它们，出问题时只能靠界面说")
	}
	if v.HorizonDays != digest.HorizonDays {
		t.Errorf("HorizonDays 是 %d，想要 %d", v.HorizonDays, digest.HorizonDays)
	}
}

// 设置是整份替换（不是补丁）：关掉、换钟点，一次存下去就都生效。
func TestSetConfigIsAWholeReplacement(t *testing.T) {
	e := newEnv(t, base)

	if err := e.svc.SetConfig(digest.Config{Enabled: false, Hour: 7, Minute: 5}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	v := e.svc.Config()
	if v.Enabled || v.Hour != 7 || v.Minute != 5 {
		t.Fatalf("读回来的是 %v %02d:%02d，想要 false 07:05", v.Enabled, v.Hour, v.Minute)
	}

	// 再存一份，上次那几项不能有一样被「没提」而留了下来。
	if err := e.svc.SetConfig(digest.Config{Enabled: true, Hour: 23, Minute: 59}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	v = e.svc.Config()
	if !v.Enabled || v.Hour != 23 || v.Minute != 59 {
		t.Errorf("读回来的是 %v %02d:%02d，想要 true 23:59", v.Enabled, v.Hour, v.Minute)
	}
}

// 坏的时刻当场退回，**什么都不写**：一个 25 点的时刻存下去也算不出排程，
// 让它躺在盘上只会让下一次读的时候再坏一次。
func TestSetConfigRejectsBadTime(t *testing.T) {
	e := newEnv(t, base)

	for _, bad := range []digest.Config{
		{Enabled: true, Hour: 24, Minute: 0},
		{Enabled: true, Hour: -1, Minute: 0},
		{Enabled: true, Hour: 12, Minute: 60},
		{Enabled: true, Hour: 12, Minute: -1},
	} {
		if err := e.svc.SetConfig(bad); !errors.Is(err, digest.ErrBadTime) {
			t.Errorf("存 %02d:%02d 返回 %v，想要 ErrBadTime", bad.Hour, bad.Minute, err)
		}
	}

	if _, err := os.Stat(e.cfgPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("被退回的设置还是留下了文件（stat 返回 %v）", err)
	}
}

// 设置文件写坏了：照实说，同时**不拿默认值悄悄顶替**，也不动上一份排程。
//
// 留下上一份排程还能发（总比整个哑掉强），而界面会把 Problem 显示出来让用户改回去。
func TestCorruptConfigIsReportedAndScheduleIsLeftAlone(t *testing.T) {
	e := newEnv(t, base)
	if err := e.svc.SetConfig(digest.Config{Enabled: true, Hour: 20, Minute: 0}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	before, err := os.ReadFile(e.schedPath)
	if err != nil {
		t.Fatalf("读排程: %v", err)
	}

	if err := os.WriteFile(e.cfgPath, []byte("{ 这不是 JSON"), 0o600); err != nil {
		t.Fatalf("写坏设置文件: %v", err)
	}

	if _, err := e.svc.Refresh(); err == nil {
		t.Error("设置文件坏了，Refresh 却成功了")
	}
	after, err := os.ReadFile(e.schedPath)
	if err != nil {
		t.Fatalf("读排程: %v", err)
	}
	if string(before) != string(after) {
		t.Error("设置文件坏了，排程却被改写了")
	}

	// 界面那一侧：看得出有问题，而且还有一份能改回去的值。
	v := e.svc.Config()
	if v.Problem == "" {
		t.Error("设置文件坏了，Config 却没报 Problem")
	}
	if v.Hour != digest.DefaultHour {
		t.Errorf("坏掉时摆出来的钟点是 %d 点，想要默认的 %d 点", v.Hour, digest.DefaultHour)
	}
}
