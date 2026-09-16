package agent

import (
	"time"

	"questionbook/internal/tags"
)

// Reader 是 agent 看数据的那一眼。**这是它唯一的入口，而入口上没有一个写方法。**
//
// 它长成这样不是随手定的：
//
//   - 每个方法只回答一个问题，返回的全是**值**（结构体、切片、时间），没有一个返回值
//     上挂着能改数据的方法。拿在手里也做不了什么。
//   - 它刻意**不提供**「按 id 取一道题」这种细粒度入口：模型要的是「有哪些题、各自挂了
//     什么标签、复习成什么样」，一次给全，比让它在循环里一道道捞省好几轮往返。
//     （这也顺手把 N+1 次全表读这件事挡在了接口外面。）
//   - 它不认识 database/sql，也不认识分页游标 —— 那些是 store 的事。
//
// 方法集的完整清单被 safety_test.go 用反射钉住：加一个方法就红，除非同时改那份白名单，
// 而那一步是**故意**要人停下来想一想的。
type Reader interface {
	// ListTags 返回全部标签，平铺（前端与模型都按 ParentID 自己拼树）。
	ListTags() ([]tags.Tag, error)

	// Questions 返回全部错题，新拍的在前，每条带上它的标签与复习概况。
	//
	// 「全部」在这里是刻意的：个人错题本是几百行的量级（spec 里那句「量级小」），
	// 一次读回来远比让模型一页页翻可靠。放进**上下文**的量由工具那一层卡（分页，
	// 见 tools.go 的 pageSize）—— 读多少与给模型看多少是两件事。
	Questions() ([]QuestionView, error)

	// ReviewStats 按标签汇总复习情况。每个标签的数字**已经含它的子孙**，
	// 所以「数学（学科）怎么样」与「概率论（章节）怎么样」是同一个调用里的两行。
	ReviewStats() ([]TagStats, error)

	// ReviewHistory 返回一道错题的复习记录，最近的在先，最多 limit 条。
	ReviewHistory(questionID int64, limit int) ([]ReviewEntry, error)
}

// QuestionView 是一道错题在 agent 眼里的样子：题本身 + 它的标签 + 它的复习概况。
//
// 字段都摊平了，而不是内嵌 library.Question：本包刻意**不 import internal/library**
// （那是 store 的事，它才是拿数据库句柄的那一层）。这里只需要几个值，不需要那个类型。
type QuestionView struct {
	ID        int64
	CreatedAt time.Time
	HasAnswer bool
	// Tags 是这道题挂着的全部标签（含父节点），空切片而不是 nil。
	Tags []tags.Tag

	// Reviewed 为假表示这道题从没复习过（库里没有 review_states 那一行 ——
	// 「还没复习过」这件事，没有行本身就说得清楚）。
	Reviewed bool
	// Reps / Lapses 是累计复习次数与「重来」次数（FSRS 的 reps / lapses）。
	Reps   int
	Lapses int
	// Due 是下次到期；Reviewed 为假时它是零值。
	Due time.Time
	// LastReview 是上次复习时刻；从没复习过时是零值。
	LastReview time.Time
}

// TagStats 是一个标签上的复习汇总。
//
// 全部计数**含子孙**：点「数学」看到的是它下面所有章节与知识点的合计，
// 否则「我数学哪块最弱」这个问题在学科那一层就没有答案。
type TagStats struct {
	TagID int64
	Name  string
	// Path 是从顶层学科到它自己的一条路径（「数学 / 概率论 / 全概率公式」），
	// 因为不同学科下同名是合法的，光给名字模型分不清说的是哪一个。
	Path  string
	Level tags.Level

	// Questions 是挂了它（或它某个子孙）的错题数。
	Questions int
	// Reviewed 是其中已经复习过的题数；NotReviewed = Questions - Reviewed。
	Reviewed int
	// Due 是其中**今天已经到期**的题数。没复习过的新题也算到期（从被拍下来那一刻起），
	// 与复习页那个「今天还剩几道」同一个口径 —— 两个数不一样的话，最难查。
	Due int
	// Reps / Lapses 是累计复习次数与「重来」次数。Lapses 高 = 这块反复错。
	Reps   int
	Lapses int
	// LastReview 是这块最近一次复习的时刻；零值表示从没复习过。
	LastReview time.Time
}

// ReviewEntry 是一条复习记录。
type ReviewEntry struct {
	QuestionID    int64
	Rating        int // 1 Again / 2 Hard / 3 Good / 4 Easy（fsrs.Rating 的取值）
	ReviewedAt    time.Time
	ScheduledDays int
	DueAt         time.Time
}
