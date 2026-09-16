package library

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Question 是一道错题 —— 复习的最小单位。
//
// 两个 hash 都是图片**内容**的 hash（算法由采集层定），图片本体在库外的文件里，
// 库里只记 hash（ADR-0004）。因此「同一张图被两道题引用」是合法的：去重发生在
// 文件那一层，不是题目这一层，所以 question_hash 上没有唯一约束。
type Question struct {
	ID           int64     // 落库时分配；AddQuestion 之前是 0
	QuestionHash string    // 题图的内容 hash，必有
	AnswerHash   string    // 答案图的内容 hash；空串表示还没拍
	CreatedAt    time.Time // 创建时间，落库时截到毫秒（UTC）
}

// HasAnswer 报告这道题有没有答案图。
func (q Question) HasAnswer() bool { return q.AnswerHash != "" }

// AddQuestion 落库一道新错题，返回补上了 ID 与 CreatedAt 的那条记录。
//
// q.ID 被忽略（由库分配）；q.CreatedAt 为零值时取当前时间。
func (s *Store) AddQuestion(q Question) (Question, error) {
	if q.QuestionHash == "" {
		return Question{}, errors.New("题图 hash 不能为空")
	}
	if q.CreatedAt.IsZero() {
		q.CreatedAt = time.Now()
	}
	q.CreatedAt = q.CreatedAt.UTC().Truncate(time.Millisecond)

	res, err := s.db.Exec(
		`INSERT INTO questions (question_hash, answer_hash, created_at) VALUES (?, ?, ?)`,
		q.QuestionHash, q.AnswerHash, q.CreatedAt.UnixMilli(),
	)
	if err != nil {
		return Question{}, fmt.Errorf("写入错题: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Question{}, fmt.Errorf("取新错题 ID: %w", err)
	}
	q.ID = id
	return q, nil
}

// GetQuestion 按 id 取一道错题，取不到返回 ErrNotFound。
func (s *Store) GetQuestion(id int64) (Question, error) {
	row := s.db.QueryRow(
		`SELECT id, question_hash, answer_hash, created_at FROM questions WHERE id = ?`,
		id,
	)
	q, err := scanQuestion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Question{}, fmt.Errorf("%w: id=%d", ErrNotFound, id)
	}
	if err != nil {
		return Question{}, fmt.Errorf("读错题: %w", err)
	}
	return q, nil
}

// ListQuestions 返回全部错题，新拍的在前。
func (s *Store) ListQuestions() ([]Question, error) {
	rows, err := s.db.Query(
		`SELECT id, question_hash, answer_hash, created_at
		 FROM questions ORDER BY created_at DESC, id DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("列出错题: %w", err)
	}
	defer rows.Close()

	// 返回空切片而不是 nil：前端拿到的是 []，不是 null。
	qs := []Question{}
	for rows.Next() {
		q, err := scanQuestion(rows)
		if err != nil {
			return nil, fmt.Errorf("读错题: %w", err)
		}
		qs = append(qs, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列出错题: %w", err)
	}
	return qs, nil
}

// DeleteQuestion 删掉一道错题，并返回被删掉的那条记录 —— 调用方据此知道该回收
// 哪两张图。id 不存在时返回 ErrNotFound。
//
// 这里只动库里的行，不碰图片文件：图片在库外，而且同一个 hash 可能还被别的错题
// 引用着，删不删文件得由调用方看过引用情况再定。
func (s *Store) DeleteQuestion(id int64) (Question, error) {
	q, err := s.GetQuestion(id)
	if err != nil {
		return Question{}, err
	}

	res, err := s.db.Exec(`DELETE FROM questions WHERE id = ?`, id)
	if err != nil {
		return Question{}, fmt.Errorf("删除错题: %w", err)
	}
	// 读和删之间可能被别人抢先删掉，以实际影响的行数为准。
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return Question{}, fmt.Errorf("%w: id=%d", ErrNotFound, id)
	}
	return q, nil
}

// scanner 让 scanQuestion 同时吃得下 *sql.Row 和 *sql.Rows。
type scanner interface {
	Scan(dest ...any) error
}

func scanQuestion(sc scanner) (Question, error) {
	var (
		q  Question
		ms int64
	)
	if err := sc.Scan(&q.ID, &q.QuestionHash, &q.AnswerHash, &ms); err != nil {
		return Question{}, err
	}
	q.CreatedAt = time.UnixMilli(ms).UTC()
	return q, nil
}
