package discussion

import (
	"database/sql"
	"fmt"
	"time"
)

// Store 是讨论记录的持久化层。
//
// db 是**错题库**那一条连接，不是本包自己 Open 的：两者在同一份库文件里，
// 没有理由让两个连接池去抢同一把写锁（与 tags、review 那边同一个做法）。
type Store struct{ db *sql.DB }

// NewStore 用一个已经开好的连接构造存储。
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// execer 是「能跑一条写语句」这一面，*sql.DB 与 *sql.Tx 都满足它。
//
// 抽出来是为了让「写一条记录」在事务内外走**同一段代码**（insert）：INSERT 的那几个约定
// 只有一个出处，改一处不会漏一处。
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// append 追加一条记录，返回落库后的它（带上了分配到的 id 与截到毫秒的时刻）。
//
// 只增不改：讨论记录是历史，没有「改一句话」这回事（要纠正就在后面接着说）。
// 「重新生成」与「编辑并重发」看着像在改历史，其实是**作废掉一截再写下新的** ——
// 走 replaceFrom，动静都在明面上（见 Service 那两条的注释）。
func (s *Store) append(questionID int64, role Role, text string, at time.Time) (Message, error) {
	return insert(s.db, questionID, role, text, at)
}

// insert 落一条记录。db 可以是连接本身，也可以是事务 —— 由调用方决定这件事跟谁同生共死。
func insert(db execer, questionID int64, role Role, text string, at time.Time) (Message, error) {
	m := Message{
		QuestionID: questionID,
		Role:       role,
		Text:       text,
		// 截到毫秒：库里存的是 Unix 毫秒，不截的话返回值与落库值会差一个亚毫秒的尾巴，
		// 「读回来的时刻等于刚写进去的时刻」这句就不再成立（与题库、复习那边一致）。
		CreatedAt: at.UTC().Truncate(time.Millisecond),
	}

	res, err := db.Exec(
		`INSERT INTO discussions (question_id, role, text, created_at) VALUES (?, ?, ?, ?)`,
		questionID, string(role), text, m.CreatedAt.UnixMilli(),
	)
	if err != nil {
		return Message{}, fmt.Errorf("写讨论记录: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Message{}, fmt.Errorf("取讨论记录 ID: %w", err)
	}
	m.ID = id
	return m, nil
}

// replaceFrom 把 from 这条（含）及其之后的记录整段换掉：删掉它们，写下 role/text 这一条。
// **删与写在同一个事务里** —— 这是它的全部意义所在。
//
// from 用的是 id 而不是下标：记录都挂在这道题上，而 id 是 AUTOINCREMENT 的，先写的谁小 ——
// 于是 `id >= from` 恰好就是「这一条以及它之后的全部」，与 history 的排序同一个依据。
// question_id 那个条件一个字都不能省：少了它，一句 `id >= from` 会顺手删掉**别的题**的讨论。
//
// 为什么非要在事务里：这是「编辑并重发」落库的那一步。中间断了的话，用户改的那句话
// 既没写进去、原来那一截又被删了 —— 他打的字就真的没了（票据 33 的底线）。
// 要么两件都成，要么一件都没发生。
func (s *Store) replaceFrom(questionID, from int64, role Role, text string, at time.Time) (Message, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Message{}, fmt.Errorf("开事务: %w", err)
	}
	// 每一条提前返回的岔路都得退回滚，用 defer 一次管住，比在每个 return 前面写一遍可靠。
	// 提交之后的那一次是空操作（返回 ErrTxDone），丢掉即可。
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`DELETE FROM discussions WHERE question_id = ? AND id >= ?`,
		questionID, from,
	); err != nil {
		return Message{}, fmt.Errorf("删讨论记录: %w", err)
	}

	m, err := insert(tx, questionID, role, text, at)
	if err != nil {
		return Message{}, err
	}
	if err := tx.Commit(); err != nil {
		return Message{}, fmt.Errorf("提交讨论记录: %w", err)
	}
	return m, nil
}

// history 取一道错题的讨论记录，从早到晚。
//
// 排序先按时刻、再按 id：时刻只到毫秒，而用户那句与模型那句往往是在同一次调用里写下的
// （界面上按下「问」之后，两条记录前后脚落库），毫秒一样是常事。只按 created_at 排的话，
// 它们谁先谁后没有保证；id 是 AUTOINCREMENT 的，谁先写的谁小 —— 拿它兜底，
// 「按时间回看」才是确定的（与 questions 那边 created_at DESC, id DESC 同一个道理）。
//
// 题不存在时返回的是空集，那不是一个错：见 Service.History。
func (s *Store) history(questionID int64) ([]Message, error) {
	rows, err := s.db.Query(
		`SELECT id, question_id, role, text, created_at
		 FROM discussions WHERE question_id = ?
		 ORDER BY created_at, id`,
		questionID,
	)
	if err != nil {
		return nil, fmt.Errorf("取讨论记录: %w", err)
	}
	defer rows.Close()

	msgs := []Message{} // 空切片而不是 nil：前端要拿到 []，不是 null
	for rows.Next() {
		var (
			m    Message
			role string
			ms   int64
		)
		if err := rows.Scan(&m.ID, &m.QuestionID, &role, &m.Text, &ms); err != nil {
			return nil, fmt.Errorf("读讨论记录: %w", err)
		}
		m.Role = Role(role)
		m.CreatedAt = time.UnixMilli(ms).UTC()
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("取讨论记录: %w", err)
	}
	return msgs, nil
}
