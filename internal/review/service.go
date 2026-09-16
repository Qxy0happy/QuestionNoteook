package review

import (
	"fmt"
	"sync"
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
	// cfgPath 是复习参数的设置文件（库外 JSON，与 library.db 并列），见 Config。
	cfgPath string
	// 取「现在」的方式。做成字段是为了能在测试里钉死 —— 「今天」是哪一天直接决定
	// 队列里有什么、上限还剩几天，用真实时钟就没法断言边界。
	now func() time.Time

	// cfg 是缓存下来的设置。读写都要过 mu：设置从界面那侧随时可能被改，
	// 而每一次复习是另一个 goroutine。读的是缓存、不是每次都读文件 ——
	// 但 scheduler 仍然会拿「此刻」重新推上限，见 params。
	mu  sync.RWMutex
	cfg Config
}

// Option 给服务补一样可选能力。
type Option func(*Service)

// WithNow 换掉取当前时刻的方式。只有测试会用。
func WithNow(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// NewService 用一个已经开好的错题库和一份设置文件的落点构造复习服务。
//
// 复习用的连接就是题库那一条（ADR-0004 只把**图片**放到库外，元数据都在这一份 SQLite 里），
// 不再 Open 第二个到同一文件的连接：WAL 下能跑，但没有理由让两个池抢同一把写锁。
//
// 设置文件**不存在**是正常状态（刚装上）；**读坏了**也不拦着服务起来 —— 那两种情况都退回
// 默认设置，问题由 Config 带给界面（见 loadConfigFile）。
func NewService(questions *library.Store, cfgPath string, opts ...Option) *Service {
	// 这里不记「文件有什么问题」：Config 每次都重读一遍，那一处才把它端给界面。
	cfg, _ := loadConfigFile(cfgPath)
	s := &Service{
		store:     NewStore(questions.DB()),
		questions: questions,
		cfgPath:   cfgPath,
		cfg:       cfg,
		now:       time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// params 按当前缓存下来的设置与**此刻**算出调度参数。
//
// 每次用的时候现算，不在构造服务时算好存着：上限是「距考试还有几天」，存下来的话
// 过了午夜它就旧一天 —— 而复习恰恰是每天早上的事。算一遍只是填一个参数结构体，
// 这个开销可以忽略。
func (s *Service) params() fsrs.Parameters {
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	return cfg.params(s.now())
}

// clampToCap 把一次评级算出的下一张卡收进上限之内。
//
// 要我们自己收这一道，是因为**官方库的 MaximumInterval 是软的**：它先把每一档夹到上限，
// 再为了保住 Again < Hard < Good < Easy 的严格次序往上加。实测（上限 94 天、复习过三次
// 的卡）：Hard 94 / Good **95** / Easy **96** —— 「任何评级都不会排到上限之外」这句话，
// 库本身给不了，它能超出上限两天。
//
// 而这一票要的正是那句话：上限是**从考试日期推出来的**，超出两天就是考后两天。
// 所以收在最后一刻，收成硬的。
//
// 收的只是**排出去的日期**（Due 与 ScheduledDays）：stability 与 difficulty 仍是库算的
// 原值 —— 那两列是「这道题记得多牢」的估计，不该为了凑一个上限去动它。
//
// 收口只有这一处（服务里评级走它，预览也走它，见 config.go 的 intervals）：预览必须显示
// **真的会发生的事**，否则一个写着 95 天、真去做却是 94 天的预览比不做预览还糟。
func clampToCap(card fsrs.Card, reviewedAt time.Time, maxInterval float64) fsrs.Card {
	if float64(card.ScheduledDays) <= maxInterval {
		return card
	}
	card.ScheduledDays = uint64(maxInterval)
	// Due 与间隔的关系照库的约定：到期 = 复习那一刻 + N × 24 小时。
	// 用 24 小时的整数倍而不是 AddDate，是为了与库算出来的时刻**逐位一致** ——
	// 只有一处口径，比较时才不会差出一个夏令时。
	card.Due = reviewedAt.Add(time.Duration(card.ScheduledDays) * 24 * time.Hour)
	return card
}

// Queue 返回当前的**今日复习队列**：到期日是今天或更早的错题，最该复习的在前。
//
// 队列的长度就是「今天还剩多少道」—— 评过的题会把到期推到至少明天（见 Config.params），
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

	params := s.params()
	info, err := fsrs.NewFSRS(params).Next(card, now, fsrs.Rating(rating))
	if err != nil {
		return ReviewResult{}, fmt.Errorf("复习: FSRS 算不出下一次到期: %w", err)
	}
	// 把结果收进上限之内 —— 官方库那个上限是软的（见 clampToCap 里量出来的那两个数）。
	next := clampToCap(info.Card, now, params.MaximumInterval)

	if err := s.store.save(questionID, fsrs.Rating(rating), next); err != nil {
		return ReviewResult{}, err
	}

	return ReviewResult{
		QuestionID:    questionID,
		Rating:        rating,
		ReviewedAt:    now.UTC(),
		DueAt:         next.Due.UTC(),
		ScheduledDays: int(next.ScheduledDays),
	}, nil
}

// Config 返回当前的复习参数设置（含四档预览）；文件坏了就退回默认值，并把问题带回去。
//
// 每次都**重读一遍文件**，不只读缓存：这一页是用户改设置的地方，它显示的必须是他刚存下的
// 那一份。顺带把缓存也刷新了 —— 排程与界面看到的因此是同一个值。
func (s *Service) Config() ConfigView {
	cfg, problem := loadConfigFile(s.cfgPath)
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
	return cfg.view(s.cfgPath, s.now(), problem)
}

// Preview 按**传进来的这份设置**算一遍视图：不落盘、不动库、也不改缓存。
//
// 为什么值得单开一个方法，而不是等 SetConfig 的返回值：这一页唯一要回答的问题是
// 「这么设之后间隔变多长」，而那个答案要在按下保存**之前**就看得见 —— 否则每试一个日期
// 都是一次真的写入（还会顺手拉一批到期日回来）。
//
// 日期不合法时返回错误，界面那侧静默退回「按已保存那份显示」：用户还在打字的中间，
// 一个打了一半的日期不该弹一句红字。
func (s *Service) Preview(c Config) (ConfigView, error) {
	if err := c.Validate(); err != nil {
		return ConfigView{}, err
	}
	return c.view(s.cfgPath, s.now(), ""), nil
}

// SaveResult 是保存设置之后带回来的：新的设置（含预览），以及这次顺手动了几行到期日。
type SaveResult struct {
	View ConfigView
	// PulledBack 是「到期日原本排在生效上限之外、这次被拉回来的」题目数。
	//
	// 只在有生效上限时才可能非 0。它是界面必须说出来的一件事：用户改了考试日期之后，
	// 库里那些排到几个月后的题刚刚**被动了**，那不该悄悄发生。
	PulledBack int
}

// SetConfig 存下复习参数，并把库里已经排到生效上限之外的到期日拉回来。
//
// 拉回来放在这里、而不是每次算调度参数时顺手做，因为那是一个**用户动作**的结果
// （他填了或者改了考试日期）：要能给他一个「动了几道」的数字，而且不该天天偷偷改库。
//
// 顺序是先落盘再动库 —— 反过来的话会留下「库已经被改了，而设置没存下去」的状态，
// 那个上限下次就没人记得，被拉的题白拉。
func (s *Service) SetConfig(c Config) (SaveResult, error) {
	if err := c.Validate(); err != nil {
		return SaveResult{}, err
	}
	if err := c.Save(s.cfgPath); err != nil {
		return SaveResult{}, err
	}

	now := s.now()
	s.mu.Lock()
	s.cfg = c
	s.mu.Unlock()

	view := c.view(s.cfgPath, now, "")

	mi, active := c.maxInterval(now)
	if !active {
		// 没有生效的上限（没填考试日期、或者考过了）：一行都不动。
		// 也**不恢复**已经拉近过的到期日 —— 那一步的信息在拉的时候就已经没了。
		return SaveResult{View: view}, nil
	}

	n, err := s.store.clampDue(now, int(mi))
	if err != nil {
		// 设置已经存下去了，失败的只是「拉回来」这一半。照实报在错的那件事上，
		// 别让它听起来像「设置没保存」—— 那会让用户以为白改了一次。
		return SaveResult{View: view}, err
	}
	return SaveResult{View: view, PulledBack: n}, nil
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
