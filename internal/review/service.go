package review

import (
	"fmt"
	"time"

	"github.com/open-spaced-repetition/go-fsrs/v4"

	"questionbook/internal/library"
)

// Service 是「复习」对前端暴露的那一面，也是本包唯一的测试缝（见 spec）。
//
// 它持有一条错题库连接（复习状态与错题同一份库文件）与题库本身 —— 写之前要确认这道题在，
// 那条查询在 library 那边，不必在本包再抄一遍 SQL。
type Service struct {
	store     *Store
	questions *library.Store
	scheduler *fsrs.FSRS
	// 取「现在」的方式。做成字段是为了能在测试里钉死 —— 「今天」是哪一天直接决定
	// 队列里有什么，用真实时钟就没法断言边界。
	now func() time.Time
}

// Option 给服务补一样可选能力。
type Option func(*Service)

// WithNow 换掉取当前时刻的方式。只有测试会用。
func WithNow(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// NewService 用一个已经开好的错题库构造复习服务。
//
// 复习用的连接就是题库那一条（ADR-0004 只把**图片**放到库外，元数据都在这一份 SQLite 里），
// 不再 Open 第二个到同一文件的连接：WAL 下能跑，但没有理由让两个池抢同一把写锁。
func NewService(questions *library.Store, opts ...Option) *Service {
	s := &Service{
		store:     NewStore(questions.DB()),
		questions: questions,
		scheduler: newScheduler(),
		now:       time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// newScheduler 造出官方 FSRS-6 的调度器：官方默认权重（ADR-0002 钉的），
// 只把**学习步**（short-term steps）关掉。
//
// 关掉它的理由是尺度对不上。默认参数带学习步，四档的间隔是分钟级的（Again 1 分钟 /
// Hard 6 分钟 / Good 10 分钟），而这个应用是**每日**复习仪式：队列问的是「今天该复习
// 哪些」，通知问的是「今天到期多少」，界面上写的是「今天还剩多少道」。留着分钟级的步长
// 会有两个后果 ——
//
//  1. 「今天还剩多少道」在同一分钟里自己变；
//  2. 刚评过的题一两分钟后又到期、又出现在队列里，与「评过的题在本次队列里不再出现」
//     直接冲突。
//
// 关掉之后走官方库里的 long-term 调度器：它保证四档里最短的一档也至少推到**一天**之后
// （nextInterval 里那句 max(…, 1)，再被 Again ≤ Hard−1 ≤ Good−2 逐档拉开）。于是
// 「评过的题今天不会再出现」是一条结构上的保证，而不是靠「间隔通常够长」。
//
// 权重没有动：还是官方的默认向量，没有自训练，也没有改 RequestRetention / MaximumInterval。
func newScheduler() *fsrs.FSRS {
	params := fsrs.DefaultParam()
	params.EnableShortTerm = false
	return fsrs.NewFSRS(params)
}

// Queue 返回当前的**今日复习队列**：到期日是今天或更早的错题，最该复习的在前。
//
// 队列的长度就是「今天还剩多少道」—— 评过的题会把到期推到至少明天（见 newScheduler），
// 所以做完一道它就少一道，中途重进本页也不会把刚做过的捞回来。
//
// 判定按**本地日**取，取到次日零点为止（endOfToday）。这么定的理由、以及它与「到期其实
// 是一个时刻」之间的关系，写在 endOfToday 上 —— 那是本包唯一涉及时区的地方。
func (s *Service) Queue() ([]QueueItem, error) {
	return s.store.dueItems(endOfToday(s.now()))
}

// Grade 记录一次复习：把评级交给 FSRS 算出下一次到期，落库，并把结果带回来。
//
// 题不存在返回 library.ErrNotFound，评级不是四档之一返回 ErrInvalidRating；
// 两种情况都不落任何写入。
//
// 刻意**不检查「这道题现在到期了吗」**：界面只把到期的题端上来，但用户也可能从题库页
// 点进来复习一道还没到期的题 —— 那是他的选择，不是错误。评级照样从此刻往后排。
func (s *Service) Grade(questionID int64, rating Rating) (ReviewResult, error) {
	if !rating.valid() {
		return ReviewResult{}, fmt.Errorf("%w: %d", ErrInvalidRating, int(rating))
	}
	// 先确认这道题在，再动库。写一个不存在的 id 应当收到 ErrNotFound，
	// 而不是一个「悄悄什么都没发生」的成功（与 tags 那边同一条规矩）。
	if _, err := s.questions.GetQuestion(questionID); err != nil {
		return ReviewResult{}, err
	}

	// 截到毫秒：库里存的是 Unix 毫秒，不截的话返回值与落库值会差一个亚毫秒的尾巴，
	// 「读回来的到期等于刚算出的到期」这句就不再成立（library 那边同样截）。
	now := s.now().Truncate(time.Millisecond)

	card, ok, err := s.store.card(questionID)
	if err != nil {
		return ReviewResult{}, err
	}
	if !ok {
		card = fsrs.NewCard(now) // 新题：从零起算
	}

	info, err := s.scheduler.Next(card, now, fsrs.Rating(rating))
	if err != nil {
		return ReviewResult{}, fmt.Errorf("复习: FSRS 算不出下一次到期: %w", err)
	}

	if err := s.store.save(questionID, fsrs.Rating(rating), info.Card); err != nil {
		return ReviewResult{}, err
	}

	return ReviewResult{
		QuestionID:    questionID,
		Rating:        rating,
		ReviewedAt:    now.UTC(),
		DueAt:         info.Card.Due.UTC(),
		ScheduledDays: int(info.Card.ScheduledDays),
	}, nil
}

// endOfToday 返回 now 所在的那一天的结束，也就是**次日零点**；
// 时区取 now 自己的 —— 生产里 now 来自 time.Now()，所以是设备本地时区。
//
// ── 为什么队列按「今天」取，而不是按「到期时刻已经过去」取 ──
//
// 「到期」是一个**时刻**（FSRS 算出来的），但「今天该复习哪些」是一个**天**的问题：
// 票的标题就是「今日队列」，票 12 要「当天到期的数量」，票 14 要「今日到期多少」，
// 三处问的都是天。若改成瞬时口径（due ≤ now），会有两个后果 ——
//
//  1. 「今天还剩多少道」在一整天里自己往上涨：晚上 8 点复习的题，第二天早上 8 点
//     还没「到期」，于是早上看到的数字比晚上小；
//  2. 票 12 那条每天固定时刻的汇总通知会更难看：早上那个时刻报 0 道，晚上才报出真实
//     数字，而用户只会觉得它坏了。
//
// 代价是「到期日是今天的题，今天一整天都在队列里」—— 晚上 23:00 才到期的题，早上就能做。
// 这对复习无害（提前做，评级照常，下次到期按评级那一刻往后推），而且正是「今天该做的」。
//
// ── 时区 ──
//
// 库里存的都是绝对时刻（Unix 毫秒 UTC，与 questions.created_at 同一个约定），而用户看
// 的是**本地日历**，两者之间差着时区。整个判定里唯一涉及时区的地方就是这里的「今天」：
// 把它换算成一条绝对时刻的界线（次日零点），剩下的就是一次纯粹的比大小。
// 用 now 自己的时区而不是写死 Local，是为了让测试能传一个固定时区的时刻进来 ——
// 断言才不会跟着跑测试的机器所在的时区变。
func endOfToday(now time.Time) time.Time {
	y, m, d := now.Date() // 用 now 自己的时区取日期
	return time.Date(y, m, d, 0, 0, 0, 0, now.Location()).AddDate(0, 0, 1)
}
