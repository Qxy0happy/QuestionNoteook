package digest

import (
	"database/sql"
	"fmt"
	"time"
)

// Store 是「到期多少」这个问题的只读入口。
//
// db 是**错题库**那一条连接，不是本包自己 Open 的：questions 与 review_states 都在
// 同一份库文件里，没有理由让第三个连接池去抢同一把写锁（与 review.Store 同一个理由）。
//
// 本包**只读**：一条 INSERT / UPDATE / DELETE 都没有。汇总通知不改任何错题数据。
type Store struct{ db *sql.DB }

// NewStore 用一个已经开好的连接构造存储。
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// countDueBy 数到期时刻落在 until 之前的错题有几道。
//
// ── 这条查询为什么与 review.dueItems 长得一样 ──
//
// 两处的 WHERE 与 JOIN 是**故意**一模一样的：LEFT JOIN 复习状态、没有状态行的题拿
// created_at 顶到期（那是新题，从被拍下来的那一刻起就该复习）。它必须一样，因为
// 「今天到期多少」这个数在通知里与在复习队列里指的是同一件事 —— 汇总说 5 道、
// 点进去只有 3 道，用户只会觉得通知坏了。
//
// 差别只有一处：这里要的是**个数**，不是那些行。review 那边没有计数的方法
// （它的 Queue 会把整行都取回来，那是复习界面要的），而给 review 加一个计数方法是
// 改别人的包 —— 于是这份判定在本包留了第二份。
//
// 留第二份的风险是分叉，挡它的是 digest 的 TestForecastAgreesWithReviewQueue：
// 它逐日把本包的数与 review.Service.Queue 的长度对一遍。review 那边改了判定
// （换边界、加筛选、动 COALESCE），那条测试会当场翻掉，而不会等到用户发现通知在骗人。
//
// 顺带：真到了要优化的时候，这里换成 COUNT 也是一次改动的事 —— 正因为口径只有这一处，
// review 那边要加的「计数方法」才有意义（那是它能省掉的行，不是它能省掉的判定）。
func (s *Store) countDueBy(until time.Time) (int, error) {
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*)
		 FROM questions q
		 LEFT JOIN review_states rs ON rs.question_id = q.id
		 WHERE COALESCE(rs.due_at, q.created_at) < ?`,
		until.UTC().UnixMilli(),
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("数到期错题: %w", err)
	}
	return n, nil
}
