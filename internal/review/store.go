package review

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/open-spaced-repetition/go-fsrs/v4"

	"questionbook/internal/library"
)

// Store 是复习状态与复习记录的持久化层。
//
// db 是**错题库**那一条连接，不是本包自己 Open 的：两者在同一份库文件里，
// 没有理由让两个连接池去抢同一把写锁。
type Store struct{ db *sql.DB }

// NewStore 用一个已经开好的连接构造存储。
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// dueItems 取到期时刻落在 until 之前的错题，最该复习的在前。
//
// 没有 review_states 那一行的错题是**新题**：它从被拍下来的那一刻起就该复习，
// 所以拿 created_at 顶到期时刻（COALESCE 就是这句话）。刻意不在建题时预建一行状态：
// 「还没复习过」这件事，没有行本身就说得清楚，多一行零值反而要在别处判断它在不在。
//
// 排序按到期时刻升序 —— 拖得最久的排最前；同一时刻到期的按 id，让顺序是确定的。
func (s *Store) dueItems(until time.Time) ([]QueueItem, error) {
	rows, err := s.db.Query(
		`SELECT q.id, q.question_hash, q.answer_hash, q.created_at,
		        COALESCE(rs.due_at, q.created_at) AS due_at
		 FROM questions q
		 LEFT JOIN review_states rs ON rs.question_id = q.id
		 WHERE COALESCE(rs.due_at, q.created_at) < ?
		 ORDER BY due_at, q.id`,
		until.UTC().UnixMilli(),
	)
	if err != nil {
		return nil, fmt.Errorf("取复习队列: %w", err)
	}
	defer rows.Close()

	items := []QueueItem{} // 空切片而不是 nil：前端要拿到 []，不是 null
	for rows.Next() {
		var (
			q       library.Question
			created int64
			due     int64
		)
		if err := rows.Scan(&q.ID, &q.QuestionHash, &q.AnswerHash, &created, &due); err != nil {
			return nil, fmt.Errorf("读错题: %w", err)
		}
		q.CreatedAt = time.UnixMilli(created).UTC()
		items = append(items, QueueItem{Question: q, DueAt: time.UnixMilli(due).UTC()})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("取复习队列: %w", err)
	}
	return items, nil
}

// card 读一道错题当前的 FSRS 状态。ok 为 false 表示这道题还没有过复习状态（新题），
// 不是错误 —— 调用方拿一张新卡从零起算即可。
//
// 列与 fsrs.Card 一一对应，这一行**就是**那张卡，中间不加一层自己的翻译。
func (s *Store) card(questionID int64) (card fsrs.Card, ok bool, err error) {
	var (
		dueMS      int64
		scheduled  int64
		reps       int64
		lapses     int64
		state      int
		lastReview int64
	)
	err = s.db.QueryRow(
		`SELECT due_at, stability, difficulty, scheduled_days, reps, lapses, state,
		        last_review_at, remaining_steps
		 FROM review_states WHERE question_id = ?`,
		questionID,
	).Scan(&dueMS, &card.Stability, &card.Difficulty, &scheduled, &reps, &lapses, &state,
		&lastReview, &card.RemainingSteps)
	if errors.Is(err, sql.ErrNoRows) {
		return fsrs.Card{}, false, nil
	}
	if err != nil {
		return fsrs.Card{}, false, fmt.Errorf("读复习状态: %w", err)
	}

	card.Due = time.UnixMilli(dueMS).UTC()
	card.ScheduledDays = uint64(scheduled)
	card.Reps = uint64(reps)
	card.Lapses = uint64(lapses)
	card.State = fsrs.State(state)
	// 0 表示「还没复习过」，也就是 fsrs.Card 的零值 LastReview —— 不能拿 0 去
	// time.UnixMilli，那会变成 1970 年，被 FSRS 当成一次真实的陈年复习。
	if lastReview != 0 {
		card.LastReview = time.UnixMilli(lastReview).UTC()
	}
	return card, true, nil
}

// save 在一个事务里落两样：更新后的 FSRS 状态，和一条只增不改的复习记录。
//
// 两样必须一起成或一起不成：状态更新了而记录没写，复习史就断了一截；
// 记录写了而状态没更新，下次还会按旧状态排。
//
// 复习记录里的「间隔」取的是**这次算出来的** scheduled_days（新卡上的那个），
// 不是 fsrs.ReviewLog 里带的那个 —— 后者记的是复习**之前**的间隔（库那边的约定），
// 两者差一次复习，顺手拿会拿错。
func (s *Store) save(questionID int64, rating fsrs.Rating, card fsrs.Card) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("记一次复习: %w", err)
	}
	defer tx.Rollback() // 提交之后是 no-op

	// 第一次复习走 INSERT，之后走 UPDATE —— 一次 upsert 表达两种情形，
	// 不必先查一次「有没有行」再分岔（那中间还多一个竞态窗口）。
	const upsertState = `
		INSERT INTO review_states
			(question_id, due_at, stability, difficulty, scheduled_days, reps, lapses,
			 state, last_review_at, remaining_steps)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (question_id) DO UPDATE SET
			due_at          = excluded.due_at,
			stability       = excluded.stability,
			difficulty      = excluded.difficulty,
			scheduled_days  = excluded.scheduled_days,
			reps            = excluded.reps,
			lapses          = excluded.lapses,
			state           = excluded.state,
			last_review_at  = excluded.last_review_at,
			remaining_steps = excluded.remaining_steps`
	if _, err := tx.Exec(upsertState,
		questionID, card.Due.UnixMilli(), card.Stability, card.Difficulty,
		int64(card.ScheduledDays), int64(card.Reps), int64(card.Lapses),
		int(card.State), lastReviewMS(card), card.RemainingSteps,
	); err != nil {
		return fmt.Errorf("写复习状态: %w", err)
	}

	if _, err := tx.Exec(
		`INSERT INTO review_logs
			(question_id, rating, reviewed_at, scheduled_days, due_at, stability, difficulty)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		questionID, int(rating), lastReviewMS(card), int64(card.ScheduledDays),
		card.Due.UnixMilli(), card.Stability, card.Difficulty,
	); err != nil {
		return fmt.Errorf("写复习记录: %w", err)
	}

	return tx.Commit()
}

// clampDue 把到期时刻晚于「now 之后 days 天」的复习状态拉回到那一刻，返回动了几行。
//
// **只动 due_at**。stability 与 difficulty 是「这个人对这道题记得多牢」的估计，是 FSRS
// 从他的评级史里学出来的 —— 为了配合一个新的上限去改它们，等于伪造他过去的表现，
// 而且下一次评级的起点就错了。改 due_at 只是**提前复习**：那是 FSRS 允许的用法
// （提前复习时稳定度涨得少一点，仅此而已），不是把算法骗过去。
// scheduled_days 也不动 —— 它是「上次排出去时算的是多少天」，是历史。
//
// 只往回拉，不往远推：到期日比上限近的题一行都不碰。也**不恢复** —— 上限放宽（考试日期
// 往后改、或者清掉）时，已经拉近的到期日不会自己弹回去，那一步要用到的信息在拉的时候
// 就已经没了。
func (s *Store) clampDue(now time.Time, days int) (int, error) {
	// 与调度那边同一个口径：N 天 = N × 24 小时（不是 AddDate）。
	// 只有一处口径，被拉回来的到期日才会与「若此刻重新评一次会算出的时刻」逐位一致。
	limit := now.Add(time.Duration(days) * 24 * time.Hour).UnixMilli()
	res, err := s.db.Exec(`UPDATE review_states SET due_at = ? WHERE due_at > ?`, limit, limit)
	if err != nil {
		return 0, fmt.Errorf("把到期日拉回上限之内: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// 拿不到行数不算失败：设置已经存下去了，少一句「动了几道」不该变成一次报错。
		return 0, nil
	}
	return int(n), nil
}

// lastReviewMS 把卡上的上次复习时刻换成库里存的毫秒。
//
// 零值存 0（与 card 那边读回来时的约定对称）；直接 .UnixMilli() 会得到一个很大的负数，
// 那个数看着像「公元前 1754 年复习过一次」。
func lastReviewMS(card fsrs.Card) int64 {
	if card.LastReview.IsZero() {
		return 0
	}
	return card.LastReview.UnixMilli()
}
