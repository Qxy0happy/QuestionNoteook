package review_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"questionbook/internal/library"
	"questionbook/internal/review"
)

// 全应用只有一条测试缝：服务层（spec）。这里从头到尾只调 review.Service 的方法，
// 不断言它内部调了谁几次，也不启动 Wails。

// ratings 是四档，从差到好。好几处要按这个顺序走一遍。
var ratings = []review.Rating{review.Again, review.Hard, review.Good, review.Easy}

// zone 是测试用的固定时区：UTC+8。
//
// 刻意钉死一个时区，而不是用 time.Local —— 「今天」是队列判定里唯一涉及时区的地方，
// 跟着跑测试的机器走的话，同一份断言在 CI（多半是 UTC）和开发机上会给出不同的结果。
var zone = time.FixedZone("UTC+8", 8*60*60)

// base 是测试的时间基准：2026-09-16 10:00（UTC+8）。
var base = time.Date(2026, 9, 16, 10, 0, 0, 0, zone)

// clock 是一个可以拨的假时钟。
//
// 「今天到期」是日期相关的判定，用真实时钟就没法把边界钉死；服务层的 WithNow 就是为它留的。
type clock struct {
	zone *time.Location
	t    time.Time
}

func newClock(t time.Time) *clock { return &clock{zone: t.Location(), t: t} }

func (c *clock) Now() time.Time { return c.t }

// Set 把时钟拨到某个绝对时刻，但保留测试时区 —— 服务算「今天」用的就是这个时区。
func (c *clock) Set(at time.Time) { c.t = at.In(c.zone) }

// advance 在当前位置上往前走一段。
func (c *clock) advance(d time.Duration) { c.Set(c.t.Add(d)) }

// newService 在临时目录里搭一套「错题库 + 复习服务」，与 main.go 的接线同一个形状：
// 复习服务共用题库那条连接，不是自己 Open 的第二个。
func newService(t *testing.T, at time.Time) (*review.Service, *library.Store, *clock) {
	t.Helper()

	lib, err := library.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	t.Cleanup(func() { lib.Close() })

	c := newClock(at)
	return review.NewService(lib, filepath.Join(t.TempDir(), "review.json"), review.WithNow(c.Now)), lib, c
}

// addQuestion 落一道错题，创建时间显式给 —— 队列里「新题从创建那一刻起就到期」，
// 不钉住创建时间就断言不了排序。
func addQuestion(t *testing.T, lib *library.Store, hash string, at time.Time) library.Question {
	t.Helper()

	q, err := lib.AddQuestion(library.Question{QuestionHash: hash, CreatedAt: at})
	if err != nil {
		t.Fatalf("AddQuestion(%q): %v", hash, err)
	}
	return q
}

// queue 取今日复习队列，失败即终止测试。
func queue(t *testing.T, svc *review.Service) []review.QueueItem {
	t.Helper()

	items, err := svc.Queue()
	if err != nil {
		t.Fatalf("Queue: %v", err)
	}
	return items
}

// queuedIDs 把队列摊成 id，方便按序列断言。
func queuedIDs(items []review.QueueItem) []int64 {
	out := make([]int64, 0, len(items))
	for _, item := range items {
		out = append(out, item.Question.ID)
	}
	return out
}

// assertQueued 断言队列里的 id 序列与 want 完全一致（顺序也算）。
func assertQueued(t *testing.T, svc *review.Service, want ...int64) {
	t.Helper()

	got := queuedIDs(queue(t, svc))
	if len(got) != len(want) {
		t.Fatalf("队列里有 %v，想要 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("队列 = %v，想要 %v", got, want)
		}
	}
}

// isQueued 报告某道题在不在队列里。
func isQueued(t *testing.T, svc *review.Service, id int64) bool {
	t.Helper()

	for _, item := range queue(t, svc) {
		if item.Question.ID == id {
			return true
		}
	}
	return false
}

// countLogs 数一道题的复习记录条数。
//
// 这是本文件唯一一处绕过服务层的地方：复习记录是只增不改的历史，服务层没有任何一个方法
// 会把它读出来（界面上要的「下次几号」是刚算出来的那个值，不是历史），所以「记录被写下」
// 这件事在缝上观察不到，只能回库看。**只用来验证「写下了没有、写的是什么」，
// 不用它替代任何行为断言。**
func countLogs(t *testing.T, lib *library.Store, id int64) int {
	t.Helper()

	var n int
	if err := lib.DB().QueryRow(
		`SELECT count(*) FROM review_logs WHERE question_id = ?`, id,
	).Scan(&n); err != nil {
		t.Fatalf("数复习记录: %v", err)
	}
	return n
}

// 票 08 的核心断言：同一道题连喂四次评级，Good 比 Again 推得远。
//
// 「同一道题」在这里是四份**一模一样**的副本：同一时刻创建、从同一时刻起评。
// 四条链各连喂四次同一个档位，每次都在它刚到期的那一刻喂。四份副本之间唯一的差别就是
// 喂进去的评级 —— 不这么钉住起点，四档之间比出来的就不是「评级的影响」，
// 而是「上一档把卡片状态改成了什么」。
func TestFourRatingsOnSameQuestion(t *testing.T) {
	svc, lib, clk := newService(t, base)

	due := map[review.Rating]time.Time{}
	days := map[review.Rating]int{}
	for _, r := range ratings {
		id := addQuestion(t, lib, "sha256:"+r.String(), base).ID
		res := chain(t, svc, clk, base, id, r, 4)
		due[r] = res.DueAt
		days[r] = res.ScheduledDays
		t.Logf("连喂四次 %v：间隔 %d 天，下次到期 %s", r, res.ScheduledDays, res.DueAt.In(zone).Format(time.DateTime))
	}

	// 票里点名的那一条。
	if !due[review.Good].After(due[review.Again]) {
		t.Errorf("Good 推到 %s，Again 推到 %s —— Good 应当推得更远",
			due[review.Good], due[review.Again])
	}
	if days[review.Good] <= days[review.Again] {
		t.Errorf("Good 的间隔 %d 天，Again 的 %d 天 —— Good 应当更长", days[review.Good], days[review.Again])
	}

	// 而且整个四档是严格单调的：Again < Hard < Good < Easy。
	// 只断言 Good 与 Again 之间的话，中间两档谁先谁后就没人管了。
	for i := 1; i < len(ratings); i++ {
		lo, hi := ratings[i-1], ratings[i]
		if !due[hi].After(due[lo]) {
			t.Errorf("%v 推到 %s，%v 推到 %s —— 更好的评级应当推得更远", hi, due[hi], lo, due[lo])
		}
		if days[hi] <= days[lo] {
			t.Errorf("%v 的间隔 %d 天，%v 的 %d 天 —— 更好的评级应当更长", hi, days[hi], lo, days[lo])
		}
	}

	// 最短的一档也落在「天」这个尺度上。队列按本地日取（见 Service.Queue），
	// 「评过的题今天不再出现」就是靠这一条撑着的；它一旦掉到分钟级，那条保证就没了。
	for _, r := range ratings {
		if days[r] < 1 {
			t.Errorf("%v 算出的间隔是 %d 天 —— 四档里的最短一档也应当是整天", r, days[r])
		}
	}
}

// chain 把一道题连喂 n 次同一个评级，每次都在它刚到期的那一刻喂，返回最后一次的结算。
//
// 每次都从 from 重新起算：四档各走一条链，起点必须一样，否则比的就不是评级了。
// 拨到「它到期了」再喂，是真实用法的形状 —— 一道题不会在半路上被评两次。
func chain(t *testing.T, svc *review.Service, clk *clock, from time.Time, id int64, r review.Rating, n int) review.ReviewResult {
	t.Helper()

	clk.Set(from)
	var res review.ReviewResult
	for i := range n {
		var err error
		res, err = svc.Grade(id, r)
		if err != nil {
			t.Fatalf("第 %d 次 Grade(%d, %v): %v", i+1, id, r, err)
		}
		clk.Set(res.DueAt)
	}
	return res
}

// 队列里装的必须是**已到期**的错题：新题（从没复习过）算到期，还没到日子的不算。
func TestQueueIsDueOnly(t *testing.T) {
	svc, lib, clk := newService(t, base)

	// 三天前拍的，还从没复习过 —— 拖最久的排最前。
	overdue := addQuestion(t, lib, "sha256:三天前", base.AddDate(0, 0, -3))
	// 今天早上刚拍的 —— 「刚拍完就出现在复习队列里」（story 12）。
	fresh := addQuestion(t, lib, "sha256:今天早上", base.Add(-time.Hour))
	// 昨天拍、昨天评过，被推到了几天之后。
	pushed := addQuestion(t, lib, "sha256:推到未来", base.AddDate(0, 0, -1))
	clk.Set(base.AddDate(0, 0, -1))
	if _, err := svc.Grade(pushed.ID, review.Good); err != nil {
		t.Fatalf("Grade: %v", err)
	}
	clk.Set(base)

	assertQueued(t, svc, overdue.ID, fresh.ID)
}

// 「今天」是**本地日**，不是 UTC 日。这条断言在按 UTC 取日期时会直接翻掉。
func TestQueueUsesLocalDay(t *testing.T) {
	svc, lib, clk := newService(t, base)

	// 今天 12:00（UTC+8）拍下并评 Again —— 四档里最近的一档，推到明天 12:00（UTC+8）。
	q := addQuestion(t, lib, "sha256:题图", base.Add(2*time.Hour))
	clk.Set(base.Add(2 * time.Hour))
	res, err := svc.Grade(q.ID, review.Again)
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if want := base.AddDate(0, 0, 1).Add(2 * time.Hour); !res.DueAt.Equal(want) {
		t.Fatalf("Again 的下次到期 = %s，想要 %s", res.DueAt, want)
	}

	// 当天剩下的时间里它都不该回来（acceptance：评过的题在本次队列里不再出现）。
	clk.Set(base.Add(13*time.Hour + 59*time.Minute)) // 今天 23:59
	if isQueued(t, svc, q.ID) {
		t.Error("同一天晚上 23:59 又出现了，可它的到期日是明天")
	}

	// 第二天凌晨 02:00（UTC+8）—— **离它真正的到期时刻还有 10 小时**，
	// 但它到期的日子就是今天，所以它该在队列里。
	//
	// 这一刻换到 UTC 是 09-16 18:00：UTC 的「今天」还是 09-16，而它到期的 UTC 日是
	// 09-17 —— 按 UTC 取日期的话它会漏掉，按用户所在时区的日历取它就在。
	// 这条断言钉的就是「拿哪个日历算今天」。
	clk.Set(base.Add(16 * time.Hour))
	if !isQueued(t, svc, q.ID) {
		t.Error("到了它到期的那个本地日，它却不在队列里")
	}
}

// 评过的题在**本次队列**里不再出现：不是永久消失，而是被推到了以后。
func TestGradedQuestionLeavesTheQueue(t *testing.T) {
	svc, lib, clk := newService(t, base)

	for _, r := range ratings {
		q := addQuestion(t, lib, "sha256:"+r.String(), base)
		if !isQueued(t, svc, q.ID) {
			t.Fatalf("刚拍下的题不在队列里（%v）", r)
		}

		if _, err := svc.Grade(q.ID, r); err != nil {
			t.Fatalf("Grade(%v): %v", r, err)
		}

		// 立刻重取一次队列 —— 界面重进本页就会重取，刚做过的不能又冒出来。
		if isQueued(t, svc, q.ID) {
			t.Errorf("评过 %v 之后它还在队列里", r)
		}
		// 当天再晚也不回来。
		clk.Set(base.Add(13*time.Hour + 30*time.Minute))
		if isQueued(t, svc, q.ID) {
			t.Errorf("评过 %v 之后，当天晚上它又回来了", r)
		}
		clk.Set(base)
	}
}

// 评级要落库：状态更新了，复习记录也写下了一条。
func TestGradePersistsStateAndLog(t *testing.T) {
	svc, lib, clk := newService(t, base)
	q := addQuestion(t, lib, "sha256:题图", base)

	if got := countLogs(t, lib, q.ID); got != 0 {
		t.Fatalf("还没复习就有 %d 条复习记录", got)
	}

	at := base.Add(90 * time.Minute)
	clk.Set(at)
	res, err := svc.Grade(q.ID, review.Good)
	if err != nil {
		t.Fatalf("Grade: %v", err)
	}

	if got := countLogs(t, lib, q.ID); got != 1 {
		t.Fatalf("复习记录有 %d 条，想要 1 条", got)
	}
	// 记录里那三样：评级、时刻、当时算出的间隔。
	var (
		rating    int
		reviewed  int64
		scheduled int
		dueMS     int64
	)
	if err := lib.DB().QueryRow(
		`SELECT rating, reviewed_at, scheduled_days, due_at FROM review_logs WHERE question_id = ?`,
		q.ID,
	).Scan(&rating, &reviewed, &scheduled, &dueMS); err != nil {
		t.Fatalf("读复习记录: %v", err)
	}
	if rating != int(review.Good) {
		t.Errorf("记录里的评级 = %d，想要 %d", rating, int(review.Good))
	}
	if got := time.UnixMilli(reviewed).UTC(); !got.Equal(at.UTC()) {
		t.Errorf("记录里的时刻 = %s，想要 %s", got, at)
	}
	if scheduled != res.ScheduledDays {
		t.Errorf("记录里的间隔 = %d 天，算出来的是 %d 天", scheduled, res.ScheduledDays)
	}
	// 记录里的间隔必须是**这次**算出来的那个（新卡上的），而不是复习之前的 ——
	// fsrs.ReviewLog 自带的 scheduled_days 是后者，顺手拿会拿到 0。
	if scheduled == 0 {
		t.Error("记录里的间隔是 0 —— 多半是拿了复习之前的那个 scheduled_days")
	}
	if got := time.UnixMilli(dueMS).UTC(); !got.Equal(res.DueAt) {
		t.Errorf("记录里的下次到期 = %s，算出来的是 %s", got, res.DueAt)
	}

	// 状态也跟着更新了：队列那侧读的就是它（新题才回落到 created_at）。
	clk.Set(res.DueAt)
	if !isQueued(t, svc, q.ID) {
		t.Error("到了刚算出的到期日，它却不在队列里 —— 状态可能没落库")
	}
}

// 写的入口要挑输入：题不存在、评级不是四档，都要明确报出来，且什么都不落。
func TestGradeRejectsBadInput(t *testing.T) {
	svc, lib, _ := newService(t, base)
	q := addQuestion(t, lib, "sha256:题图", base)

	if _, err := svc.Grade(404, review.Good); !errors.Is(err, library.ErrNotFound) {
		t.Errorf("评一道不存在的题返回 %v，想要 library.ErrNotFound", err)
	}

	for _, bad := range []review.Rating{0, 5, -1} {
		if _, err := svc.Grade(q.ID, bad); !errors.Is(err, review.ErrInvalidRating) {
			t.Errorf("评级 %d 返回 %v，想要 ErrInvalidRating", int(bad), err)
		}
	}

	if got := countLogs(t, lib, q.ID); got != 0 {
		t.Errorf("失败的评级留下了 %d 条复习记录", got)
	}
}

// 没有到期题时回空切片而不是 nil：前端要拿到 []，不是 null。
func TestQueueIsEmptyNotNil(t *testing.T) {
	svc, _, _ := newService(t, base)

	items := queue(t, svc)
	if items == nil {
		t.Fatal("队列是 nil，前端要拿到 [] 不是 null")
	}
	if len(items) != 0 {
		t.Errorf("空库里队列有 %d 条", len(items))
	}
}

// 删掉错题，它的复习状态与复习记录一并带走 —— 不留指向不存在错题的行。
// （外键 cascade 做的，dsn 里开着 foreign_keys，这条测试钉住那件事。）
func TestDeleteQuestionTakesReviewRows(t *testing.T) {
	svc, lib, clk := newService(t, base)
	q := addQuestion(t, lib, "sha256:题图", base)

	clk.Set(base.Add(time.Hour))
	if _, err := svc.Grade(q.ID, review.Good); err != nil {
		t.Fatalf("Grade: %v", err)
	}
	if _, err := lib.DeleteQuestion(q.ID); err != nil {
		t.Fatalf("DeleteQuestion: %v", err)
	}

	for _, table := range []string{"review_states", "review_logs"} {
		var n int
		if err := lib.DB().QueryRow(
			`SELECT count(*) FROM `+table+` WHERE question_id = ?`, q.ID,
		).Scan(&n); err != nil {
			t.Fatalf("数 %s 的残留: %v", table, err)
		}
		if n != 0 {
			t.Errorf("删题之后 %s 里还剩 %d 行", table, n)
		}
	}
	assertQueued(t, svc)
}

// 老库（票据 07 时期的形状：模式版本停在 2）升上来：复习状态与复习记录建出来，
// 原来的错题一条不少，而且立刻就能复习 —— 老题没有状态，按「新题」对待。
func TestUpgradeFromOlderSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.db")

	lib, err := library.Open(path)
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	old := addQuestion(t, lib, "sha256:老库里就有的题", base)

	// 把库退回票据 07 的形状：复习那两张表还没建、版本号停在 2。
	// 这是测试在**扮演**一个老库文件，不是绕过服务层的实现细节。
	//
	// 要删的表从迁移表推出来，不手抄（与 tags 那边同一条规矩）。
	for _, tbl := range library.TablesIntroducedAfter(2) {
		if _, err := lib.DB().Exec(`DROP TABLE ` + tbl); err != nil {
			t.Fatalf("退回老模式 (DROP TABLE %s): %v", tbl, err)
		}
	}
	if _, err := lib.DB().Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatalf("退回老模式 (PRAGMA user_version = 2): %v", err)
	}
	if err := lib.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := library.Open(path)
	if err != nil {
		t.Fatalf("重开失败: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })

	clk := newClock(base)
	svc := review.NewService(reopened, filepath.Join(t.TempDir(), "review.json"), review.WithNow(clk.Now))

	// 老题还在，而且立刻就该复习。
	assertQueued(t, svc, old.ID)
	if _, err := svc.Grade(old.ID, review.Good); err != nil {
		t.Fatalf("升级之后 Grade: %v", err)
	}
	if isQueued(t, svc, old.ID) {
		t.Error("升级之后评过的题还留在队列里")
	}
	if got := countLogs(t, reopened, old.ID); got != 1 {
		t.Errorf("升级之后的复习记录有 %d 条，想要 1 条", got)
	}
}
