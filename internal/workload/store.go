package workload

import (
	"database/sql"
	"fmt"
	"time"

	"questionbook/internal/review"
)

// Store 是「最近表现」的持久化层。
//
// 它是**只读**的：本包从头到尾只有两条 SELECT，一条 INSERT / UPDATE 都没有 ——
// 这正是「推荐不改变任何到期时间」在代码上的样子。
//
// db 是**错题库**那一条连接，不是本包自己 Open 的：复习记录与错题在同一份库文件里，
// 没有理由再开第二个连接池（与 review / discussion 那边同一条规矩）。
type Store struct{ db *sql.DB }

// NewStore 用一个已经开好的连接构造存储。
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Recent 是「最近表现」：一段窗口里的复习记录，全部来自 review_logs 真实记着的列。
//
// 它刻意**只**用那三列算：评级（rating）、时刻（reviewed_at）、题号（question_id）。
// 同一张表里还有 scheduled_days / stability / due_at —— 那些是 FSRS 当时的输出，说的是
// 「这道题被排到了多远」，不是「他表现得怎么样」。把一堆间隔取平均得不出比四档分布更
// 说明问题的东西，所以不往里塞（迁移 3 把它们留下来是为了别的用处）。
//
// **没有**记、因而这里也就给不出的（照实列在这儿，别当成有）：
//
//   - 每次复习花了多久：应用里就没有这个计时，无从谈起；
//   - 上一次的推荐是多少、他采纳没有：推荐**不落库**（它本来就不该改数据），
//     于是模型看不到「我上次说做 10 道，他到底做完没有」。
//
// 这两条是这一票唯一「数据不够」的地方；现在的推荐仍然站得住，是因为它要回答的
// 是「今天再做几道」，而「他最近做得怎么样」那些列答得了。
type Recent struct {
	Days       int // 这个窗口是多少天
	Reviews    int // 这段时间一共复习了几次
	Again      int
	Hard       int
	Good       int
	Easy       int
	ActiveDays int // 其中几天动过（按本地日数）
	Questions  int // 涉及几道不同的题
}

// recent 读窗口内的复习记录，汇总成「最近表现」。
//
// 窗口取 [now-window, now] —— 一段**滚动**的时间，不是「最近 N 个自然日」。自然日要引入
// 时区与「今天是哪一天」，而那个判定在 review 那边只有一处（endOfToday）；在这儿再定一个
// 「一天从几点开始」就长出第二份定义，两份迟早对不上。滚动窗口不需要时区就能算准，
// 代价只是它不跟日历对齐 —— 这一票里没人需要那个对齐（「动过几天」仍然按本地日数，
// 那只是把一个时刻归到某一天，不涉及窗口的边界）。
//
// 按 reviewed_at 过滤，而 review_logs 上那条索引是 (question_id, reviewed_at)：
// 前导列不是时间，这条查询走不上它，是一次全表扫。不为它另加索引 —— 个人的错题本，
// 几年的记录也就几万行，而加索引要动 library 的迁移（那一处正被别的改动追加着）。
func (s *Store) recent(now time.Time, window time.Duration) (Recent, error) {
	rows, err := s.db.Query(
		`SELECT rating, reviewed_at, question_id FROM review_logs WHERE reviewed_at >= ?`,
		now.Add(-window).UnixMilli(),
	)
	if err != nil {
		return Recent{}, fmt.Errorf("读最近的表现: %w", err)
	}
	defer rows.Close()

	r := Recent{Days: int(window.Hours() / 24)}
	questions := map[int64]bool{}
	days := map[string]bool{}
	for rows.Next() {
		var (
			rating int
			atMS   int64
			qid    int64
		)
		if err := rows.Scan(&rating, &atMS, &qid); err != nil {
			return Recent{}, fmt.Errorf("读复习记录: %w", err)
		}

		r.Reviews++
		questions[qid] = true
		days[localDay(atMS, now.Location())] = true
		switch review.Rating(rating) {
		case review.Again:
			r.Again++
		case review.Hard:
			r.Hard++
		case review.Good:
			r.Good++
		case review.Easy:
			r.Easy++
		default:
			// schema 上有 CHECK (rating BETWEEN 1 AND 4)，走到这儿说明库被人在别处改过。
			// 不为此报错：这条记录的时刻与题号仍然算数（他确实复习了那一道），
			// 只是不知道该记进哪一档 —— 于是四档之和会小于 Reviews。
		}
	}
	if err := rows.Err(); err != nil {
		return Recent{}, fmt.Errorf("读最近的表现: %w", err)
	}

	r.Questions = len(questions)
	r.ActiveDays = len(days)
	return r, nil
}

// neverReviewed 数出队列里有多少道是**从没复习过**的新题。
//
// 判定就是「review_states 里有没有它那一行」（没有行 = 新题，见迁移 3 的注释），
// 不拿「到期时刻等于创建时刻」去猜 —— 那只是个约等式，不是定义。
//
// 为什么把整张状态表的主键读回来、而不是只查队列里那些 id：队列长度没有上限（这张票的
// 前提就是「几百道到期」），拼一条几百个占位符的 IN 会撞上 SQLite 的参数上限。
// review_states 是 WITHOUT ROWID、主键就是 question_id，这一读只走主键那一棵 B 树。
func (s *Store) neverReviewed(items []review.QueueItem) (int, error) {
	rows, err := s.db.Query(`SELECT question_id FROM review_states`)
	if err != nil {
		return 0, fmt.Errorf("读复习状态: %w", err)
	}
	defer rows.Close()

	reviewed := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("读复习状态: %w", err)
		}
		reviewed[id] = true
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("读复习状态: %w", err)
	}

	n := 0
	for _, item := range items {
		if !reviewed[item.Question.ID] {
			n++
		}
	}
	return n, nil
}

// localDay 把一个时刻归到它所在的那一天（按 loc 那个日历）。
//
// 用「年-月-日」的字符串当身份：它只用来数「有几天动过」，不需要是个能再参与计算的时刻。
func localDay(atMS int64, loc *time.Location) string {
	return time.UnixMilli(atMS).In(loc).Format("2006-01-02")
}
