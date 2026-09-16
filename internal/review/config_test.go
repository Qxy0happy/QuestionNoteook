package review_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-spaced-repetition/go-fsrs/v4"

	"questionbook/internal/library"
	"questionbook/internal/review"
)

// newServiceWithConfig 与 newService 是同一套（错题库 + 复习服务 + 假时钟），
// 只是把**设置文件的落点**也交出来。
//
// 复习参数这一票要验的正是「文件里存了什么」与「换一份文件之后服务怎么表现」，
// 所以路径不能藏在 newService 里面。
func newServiceWithConfig(t *testing.T, at time.Time) (*review.Service, *library.Store, *clock, string) {
	t.Helper()

	lib, err := library.Open(filepath.Join(t.TempDir(), "library.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	t.Cleanup(func() { lib.Close() })

	c := newClock(at)
	cfgPath := filepath.Join(t.TempDir(), "review.json")
	return review.NewService(lib, cfgPath, review.WithNow(c.Now)), lib, c, cfgPath
}

// state 是一行复习状态里我们关心的几列。断言「哪一列被动过」只能看库。
type state struct {
	due           time.Time
	stability     float64
	difficulty    float64
	scheduledDays int
}

// stateOf 直接读那一行复习状态。
//
// 越过服务层直接查库，是因为这里要验的正是**持久化下来的东西**（到期日被改成什么、
// 哪几列一个字节都没动）；与 service_test.go 里那几处同一条路子。
func stateOf(t *testing.T, lib *library.Store, id int64) state {
	t.Helper()

	var (
		dueMS     int64
		scheduled int64
		s         state
	)
	err := lib.DB().QueryRow(
		`SELECT due_at, stability, difficulty, scheduled_days
		 FROM review_states WHERE question_id = ?`, id,
	).Scan(&dueMS, &s.stability, &s.difficulty, &scheduled)
	if err != nil {
		t.Fatalf("读题 %d 的复习状态: %v", id, err)
	}
	s.due = time.UnixMilli(dueMS).UTC()
	s.scheduledDays = int(scheduled)
	return s
}

// rateEasyPast 把一道题一路按 Easy 评下去，直到**这一次算出的间隔**超过 days 天。
//
// 用真实评级把题推到远期，而不是往 review_states 里塞一行到期日：后者测的是我们写的
// 那几句 SQL，前者才是这一票要处理的那件事实 —— 「复习真的会产生排到几个月后的题」。
//
// 返回最后一次复习的时刻与它算出的到期日。**最后一次之后不再拨时钟**，所以调用方
// 此刻的 now 就是最后那次复习的时刻。停下来的判据是「间隔 > days」而不是「到期日在某天
// 之后」：拉回来的界线是「现在 + days」，两者的差正好就是这个间隔，用间隔判才严丝合缝。
func rateEasyPast(t *testing.T, svc *review.Service, clk *clock, q library.Question, days int) (due, reviewedAt time.Time) {
	t.Helper()

	for range 30 {
		res, err := svc.Grade(q.ID, review.Easy)
		if err != nil {
			t.Fatalf("Grade(Easy): %v", err)
		}
		if res.ScheduledDays > days {
			return res.DueAt, res.ReviewedAt
		}
		clk.Set(res.DueAt) // 跟着到期走：下一次复习就发生在到期那一刻
	}
	t.Fatalf("评了 30 次 Easy 也没把间隔推过 %d 天 —— 参数或时间基准变了", days)
	return
}

// 存下去、读回来是同一份；而且同一天存两次不会留下第二个文件。
func TestSaveThenLoadRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "review.json")
	want := review.Config{Fuzz: true, ExamDate: "2026-12-19"}
	if err := want.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := review.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got != want {
		t.Errorf("读回来是 %+v，想要 %+v", got, want)
	}

	// 再存一次（原子上改名的那条路）：目录里不该多出一个临时文件。
	if err := want.Save(path); err != nil {
		t.Fatalf("再存一次: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("目录里有 %d 个文件（%v），想要 1 个 —— 存的过程不该留下临时文件",
			len(entries), entries)
	}
}

// 文件不在 = 还没配过，**不是错误**；拿到的是默认设置。
func TestLoadMissingFileIsNotConfigured(t *testing.T) {
	cfg, err := review.LoadConfig(filepath.Join(t.TempDir(), "还没配过.json"))
	if !errors.Is(err, review.ErrNotConfigured) {
		t.Errorf("文件不在时给的是 %v，想要 ErrNotConfigured", err)
	}
	if cfg != review.DefaultConfig() {
		t.Errorf("文件不在时给的是 %+v，想要默认设置 %+v", cfg, review.DefaultConfig())
	}
}

// 默认设置：保留率 0.95、模糊关着、没有考试日期。
//
// 前两项是**有意**的（0.95 的理由与实测见票 08：0.90 会让复习过的卡全顶到考试上限上），
// 后两项与官方默认一致。装上之后的行为本来就与本设置存在之前**不**一样了 —— 这正是要的。
func TestDefaultConfig(t *testing.T) {
	got := review.DefaultConfig()
	if got.RequestRetention != review.DefaultRetention {
		t.Errorf("默认保留率是 %v，想要 %v", got.RequestRetention, review.DefaultRetention)
	}
	if got.Fuzz || got.ExamDate != "" {
		t.Errorf("默认设置是 %+v，想要「模糊关着、没有考试日期」", got)
	}
}

// 旧版设置文件里没有 `request_retention` 这一项 —— 读出来该是**默认的 0.95**，不是「文件坏了」。
//
// 装机上那份就是这么写的（它是加这个字段之前存的）。判它非法的话，用户升一次级就会连同
// 他填的考试日期一起被退回默认值 —— 那等于把他配的东西丢了。
func TestOldConfigWithoutRetentionUsesDefault(t *testing.T) {
	svc, _, _, path := newServiceWithConfig(t, base)
	if err := os.WriteFile(path, []byte(`{"fuzz":true,"exam_date":"2026-12-19"}`), 0o600); err != nil {
		t.Fatalf("写一份旧版设置: %v", err)
	}

	view := svc.Config()
	if view.Problem != "" {
		t.Errorf("旧版设置被当成坏文件了：%s", view.Problem)
	}
	if view.RequestRetention != review.DefaultRetention {
		t.Errorf("视图里的保留率是 %v，想要默认值 %v", view.RequestRetention, review.DefaultRetention)
	}
	if !view.Fuzz || view.ExamDate != "2026-12-19" {
		t.Errorf("旧文件里那两项该原样保留，拿到 fuzz=%v exam=%q", view.Fuzz, view.ExamDate)
	}
}

// 保留率越界要在**存之前**拦住；而「0」不算越界 —— 它表示「没设过」，用默认值。
func TestValidateRetentionRange(t *testing.T) {
	for _, bad := range []float64{0.69, 1.0, 1.5, -0.5} {
		if err := (review.Config{RequestRetention: bad}).Validate(); !errors.Is(err, review.ErrBadRetention) {
			t.Errorf("保留率 %v 应当被拒，得到 %v", bad, err)
		}
	}
	for _, ok := range []float64{0, 0.70, 0.95, 0.99} {
		if err := (review.Config{RequestRetention: ok}).Validate(); err != nil {
			t.Errorf("保留率 %v 应当通过，得到 %v", ok, err)
		}
	}
}

// 日期写坏了要在**存之前**拦住 —— 存下去再发现，用户会以为改成功了。
func TestValidateRejectsBadExamDate(t *testing.T) {
	for _, bad := range []string{"2026/12/19", "12-19-2026", "2026-13-01", "明天"} {
		if err := (review.Config{ExamDate: bad}).Validate(); !errors.Is(err, review.ErrBadExamDate) {
			t.Errorf("ExamDate=%q 应当被拒，得到 %v", bad, err)
		}
	}
	if err := (review.Config{ExamDate: "2026-12-19"}).Validate(); err != nil {
		t.Errorf("正常日期被拒了：%v", err)
	}
	if err := (review.Config{}).Validate(); err != nil {
		t.Errorf("不填考试日期是合法的：%v", err)
	}
}

// 还没配过时：没有生效上限、上限显示官方默认那个数、预览有四档、而且**不算有问题**。
func TestDefaultViewShowsNoCap(t *testing.T) {
	svc, _, _, _ := newServiceWithConfig(t, base)
	view := svc.Config()

	if view.ExamActive {
		t.Error("没填考试日期，上限不该算「生效」—— 界面照这个字段说话")
	}
	def := int(fsrs.DefaultParam().MaximumInterval)
	if view.MaxIntervalDays != def {
		t.Errorf("生效上限是 %d 天，没填考试日期时应当是官方默认的 %d 天", view.MaxIntervalDays, def)
	}
	if view.Problem != "" {
		t.Errorf("文件根本不在是正常状态，不该报问题：%q", view.Problem)
	}
	if len(view.Preview.New) != 4 || len(view.Preview.Reviewed) != 4 {
		t.Errorf("预览该是 4 档（新卡 %d 格、复习过的卡 %d 格）",
			len(view.Preview.New), len(view.Preview.Reviewed))
	}
}

// 保存：视图里的天数、生效与否、落盘的内容，三样都要对上。
func TestSetConfigReportsTheCountdownAndPersists(t *testing.T) {
	svc, _, _, path := newServiceWithConfig(t, base) // base = 2026-09-16

	res, err := svc.SetConfig(review.Config{Fuzz: true, ExamDate: "2026-12-19"})
	if err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if res.View.DaysToExam != 94 {
		t.Errorf("距考试 %d 天，想要 94 天", res.View.DaysToExam)
	}
	if !res.View.ExamActive {
		t.Error("填的是将来的考试日期，上限该是生效的")
	}
	if res.View.MaxIntervalDays != 94 {
		t.Errorf("生效上限是 %d 天，想要 94 天", res.View.MaxIntervalDays)
	}
	if !res.View.Fuzz {
		t.Error("模糊开了，视图里该看得出来")
	}
	if res.PulledBack != 0 {
		t.Errorf("库里还没有排出去的题，不该拉回 %d 道", res.PulledBack)
	}

	// 落盘的那一份用**真的** LoadConfig 读回来 —— 不信 View 自报的东西。
	onDisk, err := review.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if onDisk != (review.Config{Fuzz: true, ExamDate: "2026-12-19"}) {
		t.Errorf("文件里存的是 %+v", onDisk)
	}
	if again := svc.Config(); again.ExamDate != "2026-12-19" || !again.Fuzz {
		t.Errorf("再读一次服务给的设置是 %+v，与刚存的不一致", again)
	}
}

// 上限生效时，排到上限之外的题被拉回来 —— 而且**只动到期日**。
func TestSetConfigPullsBackDistantDueDates(t *testing.T) {
	svc, lib, clk, _ := newServiceWithConfig(t, base)
	const capDays = 94

	// 两道题：一道正常复习（排在几天后），一道一路 Easy 推到 94 天以外。
	near := addQuestion(t, lib, "sha256:排得近", base)
	far := addQuestion(t, lib, "sha256:排到很久以后", base)

	if _, err := svc.Grade(near.ID, review.Good); err != nil {
		t.Fatalf("Grade(near): %v", err)
	}
	nearBefore := stateOf(t, lib, near.ID)
	if !nearBefore.due.Before(base.AddDate(0, 0, capDays)) {
		t.Fatalf("前提不成立：near 的到期日是 %v，本来就该在上限之内", nearBefore.due)
	}

	farDue, farReviewedAt := rateEasyPast(t, svc, clk, far, capDays)
	farBefore := stateOf(t, lib, far.ID)
	t.Logf("far 在拉回来之前：到期 %v（间隔 %d 天），最后复习在 %v",
		farDue.Format(time.DateOnly), farBefore.scheduledDays,
		farReviewedAt.Format(time.RFC3339))

	res, err := svc.SetConfig(review.Config{ExamDate: "2026-12-19"})
	if err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if res.PulledBack != 1 {
		t.Errorf("拉回了 %d 道，想要 1 道（只有 far 排在上限之外）", res.PulledBack)
	}

	// 上限是**按此刻重算**的，不是 base 那天的 94 天：一路 Easy 推下来时钟已经走到了
	// 11/29，距 12/19 只剩 20 天。这两条一起钉住「上限跟着日期走」这件事。
	if res.View.DaysToExam != 20 || res.View.MaxIntervalDays != 20 {
		t.Errorf("此刻距考试 %d 天、生效上限 %d 天，都该是 20 天",
			res.View.DaysToExam, res.View.MaxIntervalDays)
	}

	// far：到期日被拉到「此刻 + 20 天」，也就是考试那一天。
	farAfter := stateOf(t, lib, far.ID)
	wantDue := farReviewedAt.Add(time.Duration(res.View.MaxIntervalDays) * 24 * time.Hour).UTC()
	if !farAfter.due.Equal(wantDue) {
		t.Errorf("far 的到期日拉成了 %v，想要 %v（复习那一刻 + %d 天）",
			farAfter.due, wantDue, res.View.MaxIntervalDays)
	}

	// 而它那条 FSRS 状态一个字段都没动 —— 这是本票最要紧的一条。
	if farAfter.stability != farBefore.stability || farAfter.difficulty != farBefore.difficulty {
		t.Errorf("stability/difficulty 被动过了：%v/%v → %v/%v。"+
			"那两列是「这个人对这道题记得多牢」的估计，是 FSRS 从他的评级史学出来的；\n"+
			"为了配合一个新的上限去改它们，等于伪造他过去的表现，而且下一次评级的起点也错了",
			farBefore.stability, farBefore.difficulty, farAfter.stability, farAfter.difficulty)
	}
	if farAfter.scheduledDays != farBefore.scheduledDays {
		t.Errorf("scheduled_days 被动过了：%d → %d（它是「上次排出去时算的是多少天」，是历史）",
			farBefore.scheduledDays, farAfter.scheduledDays)
	}

	// near：一行都没动。
	nearAfter := stateOf(t, lib, near.ID)
	if !nearAfter.due.Equal(nearBefore.due) {
		t.Errorf("near 的到期日在上限之内，不该被碰：%v → %v", nearBefore.due, nearAfter.due)
	}
}

// 没有生效上限时（没填、或者清掉了），一行都不许动；而且**已经拉近的不弹回去**。
func TestSetConfigWithoutCapTouchesNoRows(t *testing.T) {
	svc, lib, clk, _ := newServiceWithConfig(t, base)
	const capDays = 94

	far := addQuestion(t, lib, "sha256:排到很久以后", base)
	farDue, _ := rateEasyPast(t, svc, clk, far, capDays)
	if !farDue.After(base.AddDate(0, 0, capDays)) {
		t.Fatalf("前提不成立：far 的到期日是 %v，本来就该在上限之外", farDue)
	}
	before := stateOf(t, lib, far.ID)

	// 1) 只开模糊：没有生效上限，不该动任何行。
	res, err := svc.SetConfig(review.Config{Fuzz: true})
	if err != nil {
		t.Fatalf("SetConfig(只开模糊): %v", err)
	}
	if res.View.ExamActive || res.PulledBack != 0 {
		t.Errorf("没填考试日期时上限不该生效、也不该拉回任何题，报的是 active=%v 拉回 %d 道",
			res.View.ExamActive, res.PulledBack)
	}
	if mid := stateOf(t, lib, far.ID); !mid.due.Equal(before.due) {
		t.Errorf("没有生效上限时到期日被动了：%v → %v", before.due, mid.due)
	}

	// 2) 填上考试日期：这一次拉回来（清掉之后不会弹回去，所以这里也得先确认它动过）。
	res, err = svc.SetConfig(review.Config{ExamDate: "2026-12-19"})
	if err != nil {
		t.Fatalf("SetConfig(填考试日期): %v", err)
	}
	if res.PulledBack != 1 {
		t.Errorf("上限生效的那一次该拉回 1 道，报了 %d 道", res.PulledBack)
	}
	pulled := stateOf(t, lib, far.ID)

	// 3) 再清掉考试日期：到期日**不会**弹回去（那一步要用到的信息在拉的时候已经没了）。
	res, err = svc.SetConfig(review.Config{})
	if err != nil {
		t.Fatalf("SetConfig(清掉考试日期): %v", err)
	}
	if res.View.ExamDate != "" || res.View.ExamActive {
		t.Errorf("清掉之后视图里还留着考试日期：%+v", res.View)
	}
	after := stateOf(t, lib, far.ID)
	if !after.due.Equal(pulled.due) {
		t.Errorf("上限撤掉之后到期日动了：%v → %v（只往回拉，不往远推）", pulled.due, after.due)
	}
	if after.stability != before.stability || after.difficulty != before.difficulty {
		t.Error("stability/difficulty 在这一串保存里被动过了")
	}
	t.Logf("far：原本 %v → 拉回 %v，撤掉上限后仍是 %v",
		before.due.Format(time.DateOnly), pulled.due.Format(time.DateOnly),
		after.due.Format(time.DateOnly))
}

// 设置文件坏了不能让复习瘫掉 —— 退回默认值继续跑，问题显示在设置页上。
//
// 这与 digest 那边的取向**相反**（那份设置坏了宁可报错也不拿默认值顶替）：提醒在错的
// 时间响比不响更糟，而排程要是一起瘫掉，这个应用就没有主功能了。
func TestCorruptConfigDoesNotBreakReview(t *testing.T) {
	svc, lib, _, path := newServiceWithConfig(t, base)
	q := addQuestion(t, lib, "sha256:题", base)

	if err := os.WriteFile(path, []byte("{不是 JSON"), 0o600); err != nil {
		t.Fatalf("写坏文件: %v", err)
	}

	// 复习照跑：这道新题在队列里，评级也落得下去。
	items, err := svc.Queue()
	if err != nil {
		t.Fatalf("设置坏了之后 Queue 失败了：%v", err)
	}
	if len(items) != 1 {
		t.Fatalf("队列里有 %d 道，想要 1 道", len(items))
	}
	if _, err := svc.Grade(q.ID, review.Good); err != nil {
		t.Fatalf("设置坏了之后 Grade 失败了：%v", err)
	}

	// 而设置页要说出问题，并且显示的是默认值 —— 用户还能把它改回去。
	view := svc.Config()
	if view.Problem == "" {
		t.Error("设置文件是坏的，界面该拿到一句话说明，而不是默默用默认值")
	}
	if view.ExamActive || view.Fuzz || view.ExamDate != "" {
		t.Errorf("坏文件应当退回默认设置，拿到的是 %+v", view)
	}
	if view.MaxIntervalDays != int(fsrs.DefaultParam().MaximumInterval) {
		t.Errorf("坏文件时上限应当是官方默认，拿到 %d 天", view.MaxIntervalDays)
	}

	// 改回去就好了：同一个服务，问题消失。
	if _, err := svc.SetConfig(review.Config{ExamDate: "2026-12-19"}); err != nil {
		t.Fatalf("改回一份好设置: %v", err)
	}
	if again := svc.Config(); again.Problem != "" || !again.ExamActive {
		t.Errorf("改回好设置之后仍然报着问题：%+v", again)
	}
}
