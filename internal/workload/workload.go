// Package workload 是「今天建议再做几道」。
//
// 它是**加在 FSRS 之上的一道闸**，不是第二个调度器：到期时刻仍然只由 FSRS 算（ADR-0002），
// 这道闸只回答「今天再做几道」。FSRS 那边用 RequestRetention 控制到期量，这里在它之上
// 再看一眼「最近表现怎么样」—— 今天到期 200 道全端上来，与端上来 40 道，是两件事。
//
// 闸是**摘得掉的**，而且在代码上是结构性地摘得掉：本包一个字节都不写库、也从不让队列
// 改样子，它只给出一个数。采纳与否是用户当场按下的那个按钮；不采纳就等于这一层不存在，
// 队列仍然逐字是 review.Queue 给的那一条。
//
// 「最近表现」全部来自 review_logs 真实记着的列（评级、时刻、题号），见 Store.recent ——
// 那些列是票据 08 特意为这一票留下来的（迁移 3 的注释点了名），这里不反推任何没记的东西。
//
// 跟模型说话**不在这里**：本包只认 Asker 那一个接口，实现落在 internal/vlm（模型名的选法
// 与配置都在那边，见 vlm.Service.AskText）。测试里塞一个假 provider 进去，整条流程就能
// 跑完，一次网络都不打。
//
// 测试缝只有服务层（spec）：测试直接 new 出 Service 调它的方法，不启动 Wails。
// 因此本包的 SQL 都不带业务判断，拼装与校验全在 Service 里。
package workload

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"questionbook/internal/library"
	"questionbook/internal/review"
	"questionbook/internal/vlm"
)

// Asker 是「问模型一句不带图的话」这条接缝。
//
// 形状正落在 vlm.Service.AskText 上：那边持有配置与模型名的选法 —— 本包不碰那些，
// 也不该碰（ADR-0005：模型名一律从配置来，代码里不许出现模型 ID）。
//
// 做成接口是为了测试能塞一个假的进来（与 vlm 那边的 Provider、讨论那边的 Asker 同一个用途）。
type Asker interface {
	// AskText 把这段系统提示词与正文发出去，返回模型的回答。回答是 JSON 对象。
	AskText(system, user string) (vlm.Reply, error)
}

// Recommendation 是一次推荐的结果。
//
// 它**不是**一条新的到期时间，也不是对队列的修改：它只是「今天再做几道」这个数。
type Recommendation struct {
	// Suggest 是建议今天再做的道数，恒在 [1, Due] 之内。
	//
	// 0 只有一个意思：**没有可推荐的东西** —— 队列已经空了（今天一道都没到期，或者
	// 都做完了）。那时不会去调模型（见 Recommend），界面也不该显示这一条。
	Suggest int

	// Due 是这次推荐时**队列里还有多少道**。
	//
	// 「今天到期多少」与「还剩多少」在这个应用里是同一个数：评过的题被 FSRS 推到至少
	// 明天之后（review.newScheduler 关掉学习步就是为了这条），所以做完一道它就少一道。
	// 把模型看到的那个数带出来，界面上就不必拿队列长度去猜 —— 用户做掉一道之后，
	// 界面上那个长度与模型当时看到的就不是同一个数了。
	Due int

	// Reason 是模型给的一句话理由，可能为空（它没给，或者给了一堆空白）。
	// 界面上要显示它：用户得看得出这个数是怎么来的，才谈得上采纳或不采纳。
	Reason string
}

// ErrUnusableAnswer 表示模型答得不能用：回答里没有一个能当数的东西。
//
// 它只用来区分「调用成功了，但结果没法用」这一种情形 —— 没配 VLM 是 vlm.ErrNotConfigured，
// 断网与凭据是 provider 报上来的错，那两种在 vlm 那边就有了。
//
// 三种在界面上是同一种处理：**静默退化**（不显示推荐，队列照常工作）。
var ErrUnusableAnswer = errors.New("复习量: 模型的回答里没有一个能用的数")

// defaultWindow 是「最近表现」默认看多长。
//
// 一周：复习是每天的事，短于一周看不出「他是不是断更了」，长于一周又会被很久以前
// 的状态稀释。它是个暂定值，测试可以拨（WithWindow），真跑过之后要调也只调这一处。
const defaultWindow = 7 * 24 * time.Hour

// Service 是「今天建议再做几道」对前端暴露的那一面，也是本包唯一的测试缝（见 spec）。
type Service struct {
	review *review.Service
	store  *Store
	asker  Asker

	// 取「现在」的方式。做成字段是为了能在测试里钉死 —— 「最近几天」是个时间窗，
	// 用真实时钟就没法把窗口的边界钉住（与 review / discussion 那边同一条理由）。
	now func() time.Time

	// window 是「最近表现」看多长，默认 defaultWindow。
	window time.Duration
}

// Option 给服务补一样可选能力。
type Option func(*Service)

// WithNow 换掉取当前时刻的方式。只有测试会用。
func WithNow(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// WithWindow 换掉「最近表现」的时间窗。只有测试会用。
func WithWindow(d time.Duration) Option {
	return func(s *Service) { s.window = d }
}

// NewService 接上复习服务、错题库，与一条「问模型不带图的问题」的接缝。
//
// 队列**不自己算**：直接问 review.Service.Queue。到期怎么判、没复习过的题怎么算到期、
// 「今天」按哪个日历取，全在那边（endOfToday）。在这儿重写一遍就是等两份实现走偏，
// 而这道闸要基于的恰恰是用户此刻看到的那一条队列。
//
// 连接从 questions 那里借（library.Store.DB），与 review / tags / discussion 同一条：
// 库只有一份 SQLite（ADR-0004 只把图片放到库外），没有理由再开第二条。
//
// asker 传的就是 *vlm.Service —— 它正好满足 Asker，接线时不必再加一层适配壳。
func NewService(reviewSvc *review.Service, questions *library.Store, asker Asker, opts ...Option) *Service {
	s := &Service{
		review: reviewSvc,
		store:  NewStore(questions.DB()),
		asker:  asker,
		now:    time.Now,
		window: defaultWindow,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Recommend 让模型看着「现在还剩多少道」与「最近的表现」，给一个今日复习量。
//
// **只读**：不碰队列、不碰 review_states、不碰 review_logs。这一点是这一票的验收里
// 最要紧的一条 —— 推荐只是一个数，用户不采纳时一切照旧。
//
// 队列空时不调模型，直接回一个 Suggest 为 0 的结果：没有可推荐的东西，为此发一次请求
// 既是白花钱，也会让「今天没有要复习的题」这一屏莫名其妙地转一会儿圈。
//
// 拿不到推荐时一律是 error，界面据此**静默退化**：没配 VLM 是 vlm.ErrNotConfigured、
// 断网是 provider 报上来的错、模型答得不能用是 ErrUnusableAnswer。推荐是锦上添花，
// 不能变成单点故障。
func (s *Service) Recommend() (Recommendation, error) {
	items, err := s.review.Queue()
	if err != nil {
		return Recommendation{}, err
	}
	due := len(items)
	if due == 0 {
		return Recommendation{Due: 0}, nil
	}

	now := s.now()
	recent, err := s.store.recent(now, s.window)
	if err != nil {
		return Recommendation{}, err
	}
	fresh, err := s.store.neverReviewed(items)
	if err != nil {
		return Recommendation{}, err
	}

	reply, err := s.asker.AskText(recommendPrompt, digest{
		Due: due,
		// 队头就是到期最早的那一道（review 那边按到期时刻升序排），
		// 所以「积压有多深」拿它一个数就说得清。
		OverdueDays: overdueDays(now, items[0].DueAt),
		Fresh:       fresh,
		Recent:      recent,
	}.text())
	if err != nil {
		return Recommendation{}, err
	}

	return parseRecommendation(reply.Text, due)
}

// overdueDays 是队头那道拖了几天。
//
// 今天才到期的题算 0 天，不是负数：队列按**天**取（到期日是今天或更早的都算到期），
// 而库里存的是一个时刻，于是「今天 23:00 才到期」的那道在早上看就是负的。把它说成
// 「逾期 -0.4 天」对模型没有任何意义，而 0 至少是句实话（它今天到期，没拖）。
func overdueDays(now, due time.Time) int {
	if d := int(now.Sub(due).Hours() / 24); d > 0 {
		return d
	}
	return 0
}

// ── 解析回答 ──

// maxReasonLen 是理由最多留多少个字。
//
// 提示词里要的是「一句话」，模型不听就替它裁掉（与打标签那边的 maxSuggestedTags
// 同一条规矩）：这一行在复习页上只有一行的位置，放不下就会把别的挤走。
const maxReasonLen = 40

// wire 是模型回答在**线上**的形状：小写的键。
//
// 为什么不直接往 Recommendation 上打 json 标签：Recommendation 的字段名同时是 Wails
// 生成的 TS 属性名（生成器取的是 json 标签名），给它打上 `json:"suggest"` 就等于让前端
// 也跟着服务方的报文风格写小写。两边各留一个形状、在这里对一次，与 tagWire 同一个理由。
//
// Suggest 用 json.Number 而不是 int，换来三样：能区分「没给这个键」与「给了 0」
// （前者 ParseFloat 报错，后者不报）；模型偶尔给个 12.5 也不会把整个回答判死；
// 连被引号包起来的 "12" 也认（json.Number 收它）—— 那是模型很常干的事。
// 真正判死的只有一种：值不是一个数。
type wire struct {
	Suggest json.Number `json:"suggest"`
	Reason  string      `json:"reason"`
}

// parseRecommendation 把模型的回答解析成推荐值。
//
// 容错与打标签那条路共用一套（vlm.ExtractJSONObject）：提示词里写了「只要 JSON」，
// 也拦不住模型偶尔拿代码块包一层、或者前面加一句「好的」。
//
// 数落在 [1, due] 之外时**夹回去**，而不是判这次失败：越界说明模型没看懂那条约束，
// 但它的方向是清楚的（说 500 就是「全做完」，说 0 就是「少做点」）。为一个方向清楚的
// 越界数把整条推荐丢掉、让用户什么都看不到，比夹一下更亏。
func parseRecommendation(text string, due int) (Recommendation, error) {
	body := vlm.ExtractJSONObject(text)
	if body == "" {
		return Recommendation{}, fmt.Errorf("%w: %q", ErrUnusableAnswer, text)
	}

	var w wire
	if err := json.Unmarshal([]byte(body), &w); err != nil {
		return Recommendation{}, fmt.Errorf("%w: %v", ErrUnusableAnswer, err)
	}
	f, err := w.Suggest.Float64()
	if err != nil {
		return Recommendation{}, fmt.Errorf("%w: %q", ErrUnusableAnswer, text)
	}

	return Recommendation{
		Suggest: clampToDue(int(math.Round(f)), due),
		Due:     due,
		Reason:  trimReason(w.Reason),
	}, nil
}

// clampToDue 把建议量夹进 [1, due]。
//
// 下界是 1 而不是 0：「今天一道也别做」不是这一层能给的答案 —— 想休息的人直接不打开
// 这一页就行了，而界面上显示一条「建议做 0 道」只会让人以为坏了。
func clampToDue(n, due int) int {
	if n < 1 {
		return 1
	}
	if n > due {
		return due
	}
	return n
}

// trimReason 收拾模型给的那句理由：去空白，太长就截断加省略号。
func trimReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if r := []rune(reason); len(r) > maxReasonLen {
		return string(r[:maxReasonLen]) + "…"
	}
	return reason
}
