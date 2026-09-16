package review

import (
	"slices"
	"testing"
	"time"

	"github.com/open-spaced-repetition/go-fsrs/v4"
)

// 这条把「我们的调度器与**官方默认**差在哪」钉死。
//
// 它防的是一类**静默**的坏：库升级之后默认权重 / 保留率 / 上限悄悄变了，复习间隔于是整体变了，
// 而没有任何东西会红 —— 「你确定 FSRS 是完好的吗」这个问题的正确答案不该是「我读过代码，看着对」。
//
// 所以这里逐字段比，并把「**只允许一处偏离**」写成断言：将来要再偏离一处，得先改这条测试，
// 而那一步是**故意**要人停下来想一想的。
func TestSchedulerDeviatesFromDefaultsInExactlyOnePlace(t *testing.T) {
	want := fsrs.DefaultParam()
	got := newScheduler().Parameters

	// 先把「库的默认是什么」也钉住：上游把默认值本身改了，这两条会先红 ——
	// 好过让下面那些逐字段比较去猜是上游变了还是我们变了。
	if !want.EnableShortTerm {
		t.Fatal("官方默认的 EnableShortTerm 不再是 true 了 —— 上游改过默认值，先确认这是不是有意的")
	}
	if len(want.W) != 21 {
		t.Fatalf("官方权重是 %d 位，FSRS-6 应当是 21 位", len(want.W))
	}

	// ── 唯一允许的偏离：学习步关掉 ──
	if got.EnableShortTerm {
		t.Error("学习步应当是关掉的：理由见 newScheduler 的注释（四档的尺度要与「每日复习」对齐）")
	}

	// ── 其余每一项都必须与官方默认一致 ──
	if got.RequestRetention != want.RequestRetention {
		t.Errorf("RequestRetention = %v，官方默认是 %v", got.RequestRetention, want.RequestRetention)
	}
	if got.MaximumInterval != want.MaximumInterval {
		t.Errorf("MaximumInterval = %v，官方默认是 %v", got.MaximumInterval, want.MaximumInterval)
	}
	// 权重：我们没有自训练、也没有手工调过（ADR-0002），所以必须逐位等于官方向量。
	if got.W != want.W {
		t.Error("权重与官方默认不一致 —— ADR-0002 钉的是「官方默认向量」，没有自训练")
	}
	if got.EnableFuzz != want.EnableFuzz {
		t.Errorf("EnableFuzz = %v，官方默认是 %v（这一项**动过就是有意的**，见下面的注释）",
			got.EnableFuzz, want.EnableFuzz)
	}
	if !slices.Equal(got.LearningSteps, want.LearningSteps) {
		t.Errorf("LearningSteps = %v，官方默认是 %v", got.LearningSteps, want.LearningSteps)
	}
	if !slices.Equal(got.RelearningSteps, want.RelearningSteps) {
		t.Errorf("RelearningSteps = %v，官方默认是 %v", got.RelearningSteps, want.RelearningSteps)
	}
	if got.Decay != want.Decay || got.Factor != want.Factor {
		t.Errorf("Decay/Factor = %v/%v，官方默认是 %v/%v（它们由权重推出来，不该单独偏）",
			got.Decay, got.Factor, want.Decay, want.Factor)
	}
}

// 四档的间隔：新卡与复习过的卡各来一遍，断言**严格递增**、且最短的一档也**至少一天**。
//
// 后者不是一个好看的断言 —— 它是「评过的题在本次队列里不再出现」那条结构保证的落点：
// 只要最短一档 ≥ 1 天，评完的题当天就不可能回到队列里（见 newScheduler 的注释）。
// 顺带把实际数字打出来，改动参数时能一眼看到间隔怎么变的。
func TestFourRatingsAreStrictlyOrderedAndAtLeastOneDay(t *testing.T) {
	sched := newScheduler()
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)

	// 造一张「复习过几次」的卡，而且**每次都要把时钟推到到期那一刻** ——
	// 在同一时刻连喂三次是不现实的情形：那时 elapsedDays 恒为 0，而遗忘曲线在 R=1 时
	// 让稳定度的增长项恰好为 0（`exp((1-R)·w10) - 1`），四档会一直停在 1/2/3/4。
	// 跟着到期时间往前走，才是一次真实的复习序列。
	reviewed := fsrs.NewCard(now)
	at := now
	for i := 0; i < 3; i++ {
		info, err := sched.Next(reviewed, at, fsrs.Good)
		if err != nil {
			t.Fatalf("造卡时 Next: %v", err)
		}
		reviewed = info.Card
		at = reviewed.Due
	}

	for _, tc := range []struct {
		name string
		card fsrs.Card
		// at 是「这次复习发生在什么时候」。**必须跟着卡自己的时间线走** ——
		// 拿一张上次复习在将来的卡去在过去复习，库会明确拒绝
		// （`last review date … is after current time`），那是它做得对。
		at time.Time
	}{
		{"新卡", fsrs.NewCard(now), now},
		{"复习过三次的卡", reviewed, reviewed.Due},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, err := sched.Repeat(tc.card, tc.at)
			if err != nil {
				t.Fatalf("Repeat: %v", err)
			}

			days := make([]uint64, 0, 4)
			for _, r := range []fsrs.Rating{fsrs.Again, fsrs.Hard, fsrs.Good, fsrs.Easy} {
				d := log[r].Card.ScheduledDays
				days = append(days, d)
				if d < 1 {
					t.Errorf("%v 的间隔是 %d 天 —— 最短的一档也必须至少一天，否则评过的题当天会回到队列里", r, d)
				}
			}
			t.Logf("Again %d 天 / Hard %d 天 / Good %d 天 / Easy %d 天",
				days[0], days[1], days[2], days[3])

			for i := 1; i < len(days); i++ {
				if days[i] <= days[i-1] {
					t.Fatalf("四档不是严格递增的：%v（越好的评级必须推得越远）", days)
				}
			}
		})
	}
}

// Due 是**复习那一刻往后挪 N 天**的绝对时刻，不是某个「当天零点」。
//
// 这条钉住它，因为语义换掉会让下面那句推理失效：队列按「本地日的次日零点」取界线
// （见 endOfToday），而「复习时刻 + N 天」恰好落在第 N 天那天之内 —— 两者是自洽的。
// 若哪天改成「当天零点 + N 天」，那么一次晚上 23:00 的复习会把到期日**提前**一整天，
// 而这正是这条测试会拦住的那类改动。
func TestDueIsTheIntervalAwayFromTheReviewMoment(t *testing.T) {
	sched := newScheduler()
	// 特意取一个接近日界线的时刻：这是「按天取」与「按绝对时刻取」最容易分道扬镳的地方。
	now := time.Date(2026, 9, 16, 23, 30, 0, 0, time.Local)

	info, err := sched.Next(fsrs.NewCard(now), now, fsrs.Good)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}

	want := now.Add(time.Duration(info.Card.ScheduledDays) * 24 * time.Hour)
	if !info.Card.Due.Equal(want) {
		t.Errorf("Due = %v，想要「复习时刻 + %d 天」= %v", info.Card.Due, info.Card.ScheduledDays, want)
	}

	// 顺带把「它与按天取的自洽性」也断言出来 —— 那是两条，缺一条都不叫自洽：
	//   1) 复习**当天**它不在队列里（「评过的题本次不再出现」那条不变量）；
	//   2) **第 N 天**它在队列里（「N 天后」真的是 N 天后，不是 N+1 天）。
	// 这两条正是「绝对时刻的 Due」与「按本地日取的界线」能对上的地方。
	if info.Card.Due.Before(endOfToday(now)) {
		t.Errorf("到期 %v 落在复习当天的界线（%v）之前 —— 评过的题当天就会回到队列里",
			info.Card.Due, endOfToday(now))
	}
	dayN := now.AddDate(0, 0, int(info.Card.ScheduledDays))
	if !info.Card.Due.Before(endOfToday(dayN)) {
		t.Errorf("到期 %v 不在第 %d 天（%v）之内，那天它不会出现在队列里",
			info.Card.Due, info.Card.ScheduledDays, dayN)
	}
}
