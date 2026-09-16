package review

import (
	"slices"
	"testing"
	"time"

	"github.com/open-spaced-repetition/go-fsrs/v4"
)

// 这条把「我们的调度器与**官方默认**差在哪」钉死 —— 针对的是**默认设置**
// （没填考试日期、没开模糊）。
//
// 它防的是一类**静默**的坏：库升级之后默认权重 / 保留率 / 上限悄悄变了，复习间隔于是整体变了，
// 而没有任何东西会红 —— 「你确定 FSRS 是完好的吗」这个问题的正确答案不该是「我读过代码，看着对」。
//
// **偏离只有两处，而且每一处都必须写明白**：
//
//  1. **学习步关掉**（四档的尺度要与「每日复习」对齐，理由见 Config.params）；
//  2. **期望保留率 0.95**，比官方默认的 0.90 高（理由与实测数字见 Config.RequestRetention）。
//
// 将来要再偏离一处，得先改这条测试 —— 而那一步是**故意**要人停下来想一想的。
//
// 用户自己能把上限压下来（填考试日期）、把模糊打开、把保留率调掉 —— 那几处由下面
// TestConfiguredParamsReachTheScheduler 盯着：它验的是「设置真的送到了库里」，
// 而不是「我们算出了一个数」。
func TestSchedulerDeviatesFromDefaultsInExactlyTwoPlaces(t *testing.T) {
	want := fsrs.DefaultParam()
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	got := DefaultConfig().params(now)

	// 先把「库的默认是什么」也钉住：上游把默认值本身改了，这几条会先红 ——
	// 好过让下面那些逐字段比较去猜是上游变了还是我们变了。
	if !want.EnableShortTerm {
		t.Fatal("官方默认的 EnableShortTerm 不再是 true 了 —— 上游改过默认值，先确认这是不是有意的")
	}
	if want.RequestRetention != 0.9 {
		t.Fatalf("官方默认的保留率不再是 0.9 了（现在是 %v）—— 上游改过默认值，先确认这是不是有意的",
			want.RequestRetention)
	}
	if len(want.W) != 21 {
		t.Fatalf("官方权重是 %d 位，FSRS-6 应当是 21 位", len(want.W))
	}

	// ── 偏离一：学习步关掉 ──
	if got.EnableShortTerm {
		t.Error("学习步应当是关掉的：四档的尺度要与「每日复习」对齐，理由见 Config.params")
	}

	// ── 偏离二：保留率比官方默认高 ──
	if got.RequestRetention != DefaultRetention {
		t.Errorf("RequestRetention = %v，想要 %v", got.RequestRetention, DefaultRetention)
	}
	if got.RequestRetention == want.RequestRetention {
		t.Error("保留率与官方默认相同了 —— 那这处偏离就没了，先确认是不是有意的")
	}

	// ── 其余每一项都必须与官方默认一致 ──
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
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	// 默认设置（没填考试日期）：四档的数值就是票 08 记下的那一组。
	sched := fsrs.NewFSRS(DefaultConfig().params(now))

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
	// 特意取一个接近日界线的时刻：这是「按天取」与「按绝对时刻取」最容易分道扬镳的地方。
	now := time.Date(2026, 9, 16, 23, 30, 0, 0, time.Local)
	sched := fsrs.NewFSRS(DefaultConfig().params(now))

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

// 设置真的送到了库里 —— 不是「我们算出了一个数」。
//
// 这条防的是另一类静默的坏：设置存下去了、界面也显示着，而那值从来没被塞进
// fsrs.Parameters 过（漏一次赋值、改个字段名都会这样，而且**没有任何东西会红**：
// 复习照常跑，间隔照旧长）。所以这里比的是**从参数里读出来的**结果。
func TestConfiguredParamsReachTheScheduler(t *testing.T) {
	want := fsrs.DefaultParam()
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)

	cfg := Config{RequestRetention: 0.97, Fuzz: true, ExamDate: "2026-12-19"} // 距 now 正好 94 天
	got := cfg.params(now)

	if !got.EnableFuzz {
		t.Error("设置了模糊，但调度器拿到的 EnableFuzz 还是 false")
	}
	if got.RequestRetention != 0.97 {
		t.Errorf("RequestRetention = %v，设置里填的是 0.97（这一项在界面上有得改，所以必须送进去）",
			got.RequestRetention)
	}
	const wantDays = 94
	if got.MaximumInterval != wantDays {
		t.Errorf("MaximumInterval = %v，想要距考试的天数 %d", got.MaximumInterval, wantDays)
	}

	// 其余各项仍必须是官方默认：这一页只露三个旋钮，不该顺手动到别的。
	if got.W != want.W {
		t.Error("权重被动过了 —— 设置页只该改模糊与上限")
	}
	if got.EnableShortTerm {
		t.Error("学习步应当仍然是关掉的")
	}
}

// 保留率这个旋钮的**方向**：调高 → 间隔变短。
//
// 用户最容易记反的就是这个方向（「保留率越高不是该记得越久吗」），所以把它钉成断言，
// 而不是只写在界面上的一句话里。
//
// 两件事都是量出来的，也都写在注释里：
//
//   - 拿**复习过的卡**看，差别是数量级的：0.90 是 141/196/318 天，0.95 是几十天。
//   - 拿**新卡**看几乎看不出来（只有 Easy 从 8 变 4，前三档都停在 1/2/3）—— 因为最短一天 +
//     次序各加一天那条地板把它们钉死了。所以这个旋钮是给**已经复习过的**题用的；
//     新题本来就在一两天内，调它没意义。
func TestHigherRetentionShortensIntervals(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)

	var prevDays int
	var prevRate float64
	for i, r := range []float64{0.90, 0.95, 0.97} {
		cfg := Config{RequestRetention: r}
		params := cfg.params(now)
		sched := fsrs.NewFSRS(params)

		reviewed, at := reviewedCard(sched, now)
		mature := intervals(sched, reviewed, at, params.MaximumInterval)
		fresh := intervals(sched, fsrs.NewCard(now), now, params.MaximumInterval)

		t.Logf("保留率 %.2f：新卡 %d/%d/%d/%d 天；复习过的卡 %d/%d/%d/%d 天",
			r, fresh[0].Days, fresh[1].Days, fresh[2].Days, fresh[3].Days,
			mature[0].Days, mature[1].Days, mature[2].Days, mature[3].Days)

		good := mature[2].Days
		if i > 0 && good >= prevDays {
			t.Errorf("复习过的卡在保留率 %.2f 时 Good 是 %d 天，不比 %.2f 的 %d 天短 —— 方向反了（越高应当越密）",
				r, good, prevRate, prevDays)
		}
		prevDays, prevRate = good, r
	}
}

// 上限**每天重算**：时钟往前走一天，它就小一天。
//
// 它防的是「启动时算好存下来」那种写法 —— 那样过了午夜上限就旧一天，而复习恰恰是
// 每天早上的事。所以这里把时钟推着走，而不是比一个常量。
func TestExamCapIsRecomputedEachDay(t *testing.T) {
	cfg := Config{ExamDate: "2026-12-19"}
	day := time.Date(2026, 12, 1, 8, 0, 0, 0, time.Local)

	for i := range 5 {
		at := day.AddDate(0, 0, i)
		want := float64(18 - i) // 12/1 距 12/19 是 18 天
		if got, active := cfg.maxInterval(at); !active || got != want {
			t.Fatalf("%v：上限 = %v（active=%v），想要 %v 天且生效",
				at.Format(dateLayout), got, active, want)
		}
	}
}

// 考试日期过去之后，上限**不再生效**，退回官方默认。
//
// 这一支要是漏了，天数会变成负数、被夹到 1，于是每道题每天都到期、队列一次炸开。
// 所以这里断言的恰恰是「它没有变成一个小数字」。
func TestPastExamDateFallsBackToLibraryDefault(t *testing.T) {
	def := fsrs.DefaultParam().MaximumInterval
	cfg := Config{ExamDate: "2026-12-19"}
	after := time.Date(2026, 12, 20, 9, 0, 0, 0, time.Local)

	got, active := cfg.maxInterval(after)
	if active {
		t.Error("考试已经过去了，上限不该还算「生效」—— 界面要照这个字段把话说明白")
	}
	if got != def {
		t.Errorf("上限 = %v，考过去之后应当退回官方默认 %v（夹到 1 会让每道题每天都到期）",
			got, def)
	}
}

// 考试当天：上限给 1 天。
//
// 给 0 会被官方库判为非法，而它的兜底是**悄悄换回 36500** —— 那就成了「填了考试日期
// 却毫无作用」，比给 1 天更让人摸不着头脑。
func TestExamTodayGivesOneDayNotZero(t *testing.T) {
	cfg := Config{ExamDate: "2026-12-19"}
	onExam := time.Date(2026, 12, 19, 7, 0, 0, 0, time.Local)

	got, active := cfg.maxInterval(onExam)
	if !active || got != 1 {
		t.Fatalf("考试当天：上限 = %v（active=%v），想要 1 天且生效", got, active)
	}
	p := cfg.params(onExam)
	if err := p.Validate(); err != nil {
		t.Errorf("这套参数官方库不认（那它会被悄悄换成默认值）：%v", err)
	}
}

// 预览要能看：四档都在、顺序对，而且**都被上限压着**。
//
// 这一页唯一要回答的问题就是「这么设之后间隔变多长」，所以把数打出来 ——
// 参数改动时一眼能看到间隔怎么变的。
func TestPreviewRespectsTheCap(t *testing.T) {
	now := time.Date(2026, 9, 16, 10, 0, 0, 0, time.Local)
	const capDays = 94
	p := preview(Config{ExamDate: "2026-12-19"}.params(now), now)

	for _, row := range []struct {
		name string
		got  []RatingInterval
	}{{"新卡", p.New}, {"复习过三次的卡", p.Reviewed}} {
		if len(row.got) != 4 {
			t.Fatalf("%s：预览有 %d 格，想要 4 格", row.name, len(row.got))
		}
		for i, ri := range row.got {
			if ri.Rating != Rating(i+1) {
				t.Errorf("%s：第 %d 格是 %v，四档的顺序应当是 Again/Hard/Good/Easy",
					row.name, i+1, ri.Rating)
			}
			if ri.Days < 1 || ri.Days > capDays {
				t.Errorf("%s：%v 是 %d 天 —— 应当落进 1..%d（上限就是距考试的天数）",
					row.name, ri.Rating, ri.Days, capDays)
			}
			t.Logf("%s %v = %d 天", row.name, ri.Rating, ri.Days)
		}
	}
}
