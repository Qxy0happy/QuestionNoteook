// Package review 是「复习」：取今日复习队列、记下一次复习（四档评级），
// 并把评级交给 FSRS 算出下一次到期。
//
// 复习状态与复习记录跟错题在**同一份** SQLite 里（ADR-0004 只把图片放到库外），表由
// library 的迁移机制建出来（迁移 3），连接也从它那里借（见 library.Store.DB）——
// 不另开第二个到同一文件的连接。
//
// 调度跑在 Go 侧、用官方 go-fsrs/v4（FSRS-6，ADR-0002），权重是官方默认向量 ——
// 个人错题本量小，自训练参数不现实（spec）。这里不自己实现任何调度算法。
//
// 测试缝只有服务层（spec）：测试直接 new 出 Service 调它的方法，不启动 Wails。
// 因此本包的 SQL 都不带业务判断，校验与编排全在 Service 里。
package review

import (
	"errors"
	"time"

	"questionbook/internal/library"
)

// Rating 是复习后自评的四档，也是喂给 FSRS 的唯一输入。
//
// 取值刻意与 fsrs.Rating 相同（1..4）：落库时照这个存，schema 的 CHECK 就直接是
// 「必须是这四档之一」，中间不必再维护一张映射表。
type Rating int

const (
	Again Rating = 1 // 完全没想起来
	Hard  Rating = 2 // 想起来了，但很费劲
	Good  Rating = 3 // 想起来了
	Easy  Rating = 4 // 太简单，不用再排这么密
)

// String 返回档位的名字，用在报错与日志里（CONTEXT.md 里四档就叫这几个英文名）。
func (r Rating) String() string {
	switch r {
	case Again:
		return "Again"
	case Hard:
		return "Hard"
	case Good:
		return "Good"
	case Easy:
		return "Easy"
	}
	return "未知档位"
}

// valid 报告这个值在不在四档之内。
func (r Rating) valid() bool { return r >= Again && r <= Easy }

// QueueItem 是复习队列里的一条：一道错题，加上它现在的到期时刻。
//
// 题本身整个带回来（而不是只给一个 id）是因为复习界面要用它的两个 hash 去取题图与答案图
// —— 只给 id 的话，界面每显示一道题都得再问一次题库。library.Question 是前端到处都在用的
// 那个类型，带回来也让 AnswerBadge / hasAnswer 这些现成的东西直接吃得下。
type QueueItem struct {
	Question library.Question // 题图 hash、答案图 hash、创建时间
	// 到期时刻。从没复习过的题没有状态，这里就是它的创建时间 ——
	// 「刚拍完就出现在队列里」（story 12）与「拖了很久的题排在最前」是同一句话。
	DueAt time.Time
}

// ReviewResult 是一次复习的结果：喂进去的评级，和 FSRS 算出来的下一次到期。
//
// 与服务端落库的那两行是同一个来源 —— 界面拿它显示「下次：3 天后」，不必再回库问一次。
type ReviewResult struct {
	QuestionID    int64
	Rating        Rating
	ReviewedAt    time.Time // 这次复习的时刻
	DueAt         time.Time // 下次到期
	ScheduledDays int       // 当时算出的间隔（天）
}

// ErrInvalidRating 表示评级不是四档之一。判断用 errors.Is。
//
// 错题不存在走的是 library.ErrNotFound（与 tags 那边一致）：那是题库的事，
// 本包不另立一个意思相同的错误。
var ErrInvalidRating = errors.New("评级必须是 Again / Hard / Good / Easy 之一")
