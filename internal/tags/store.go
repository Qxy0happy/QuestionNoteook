// Package tags 是三层标签（学科 > 章节 > 知识点）的持久化与读写。
//
// 标签与错题在**同一份** SQLite 里（ADR-0004 只把图片放到库外），表由 library 的迁移机制
// 建出来，连接也从它那里借（见 library.Store.DB）—— 不另开第二个到同一文件的连接。
//
// 测试缝只有服务层（spec）：测试直接 new 出 Service 调它的方法，不启动 Wails。
// 因此本文件的 SQL 都不带业务判断，校验与编排全在 Service 里。
package tags

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"questionbook/internal/library"
)

// Store 是标签与「错题-标签」关联的持久化层。
//
// 它也读 questions 表一处 —— untaggedQuestions（「一个标签都没挂」的落点在关联表上，
// 见那个方法的注释）。
//
// db 是**错题库**那一条连接，不是本包自己 Open 的：两者在同一份库文件里，
// 没有理由让两个连接池去抢同一把写锁。
type Store struct{ db *sql.DB }

// NewStore 用一个已经开好的连接构造存储。
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// tagCols 拼出 Tag 的列，顺序与 scanTag 一致 —— 只此一份，别再手抄一遍。
//
// alias 非空时带上表前缀：与 question_tags 拼一行查询时必须带，
// 两张表都有 created_at，不带就是一句 ambiguous column name。
func tagCols(alias string) string {
	prefix := ""
	if alias != "" {
		prefix = alias + "."
	}
	names := []string{"id", "parent_id", "name", "level", "created_at"}
	for i, n := range names {
		names[i] = prefix + n
	}
	return strings.Join(names, ", ")
}

// List 返回全部标签，先按层级、再按创建时间排。
//
// 平铺返回，由前端按 ParentID 自己拼树 —— 树是呈现形态，不是存储形态。
// 排序这么定：同一父下的兄弟因此按创建先后排（先建的在前），预置的四个学科排在最前
// （它们是建库时写进去的）。刻意不按名字排：SQLite 对中文按码点比，排出来对用户没有意义。
func (s *Store) List() ([]Tag, error) {
	rows, err := s.db.Query(`SELECT ` + tagCols("") + ` FROM tags ORDER BY level, created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("列出标签: %w", err)
	}
	defer rows.Close()

	ts := []Tag{} // 空切片而不是 nil：前端要拿到 []，不是 null
	for rows.Next() {
		t, err := scanTag(rows)
		if err != nil {
			return nil, fmt.Errorf("读标签: %w", err)
		}
		ts = append(ts, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列出标签: %w", err)
	}
	return ts, nil
}

// Get 按 id 取一个标签，取不到返回 ErrNotFound。
func (s *Store) Get(id int64) (Tag, error) {
	row := s.db.QueryRow(`SELECT `+tagCols("")+` FROM tags WHERE id = ?`, id)
	t, err := scanTag(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Tag{}, fmt.Errorf("%w: id=%d", ErrNotFound, id)
	}
	if err != nil {
		return Tag{}, fmt.Errorf("读标签: %w", err)
	}
	return t, nil
}

// insert 落一个新标签。parentID 为 0 表示顶层学科（库里存 NULL）。
func (s *Store) insert(parentID int64, name string, level Level) (Tag, error) {
	var parent any // 顶层学科没有父，存 NULL（schema 的 CHECK 也要求 level=1 时为 NULL）
	if parentID != 0 {
		parent = parentID
	}

	res, err := s.db.Exec(
		`INSERT INTO tags (parent_id, name, level, created_at) VALUES (?, ?, ?, ?)`,
		parent, name, int(level), time.Now().UTC().UnixMilli(),
	)
	if err != nil {
		return Tag{}, writeErr(err, name)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Tag{}, fmt.Errorf("取新标签 ID: %w", err)
	}
	return s.Get(id)
}

// rename 改名。只动 name 一列：位置（parent_id）与层级（level）不在这儿改。
func (s *Store) rename(id int64, name string) (Tag, error) {
	res, err := s.db.Exec(`UPDATE tags SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return Tag{}, writeErr(err, name)
	}
	// 调用方通常先 Get 过，但读与写之间可能已经被别处删掉，以实际影响的行数为准。
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return Tag{}, fmt.Errorf("%w: id=%d", ErrNotFound, id)
	}
	return s.Get(id)
}

// subtreeCTE 是「以 ? 为根的那棵子树」的公共表表达式。
//
// 删一棵树时要摸三样东西（有几个标签、波及几道错题、要删哪些行），三次都得带上它，
// 所以抽成常量。递归 CTE 正好干这个 —— 不必把树捞进内存再自己走一遍。
const subtreeCTE = `
	WITH RECURSIVE subtree(id) AS (
		SELECT id FROM tags WHERE id = ?
		UNION
		SELECT t.id FROM tags t JOIN subtree s ON t.parent_id = s.id
	)`

// delete 删掉 id 及其整棵子树，连同这些标签在错题上的关联，返回波及面。
//
// 子树是**显式**删干净的，没有只删一行靠外键级联：dsn 里开着 foreign_keys，
// 级联确实会发生（那是 db 层的兜底），但「删一个章节会带走它的知识点」是本包对外承诺的
// 行为，它得在这一层看得见，而不是藏在一个 pragma 后面。
//
// 全程一个事务：子树取到一半失败不会留下半棵残树。
func (s *Store) delete(id int64) (DeleteResult, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return DeleteResult{}, fmt.Errorf("删标签: %w", err)
	}
	defer tx.Rollback() // 提交之后是 no-op

	var res DeleteResult
	// 先数子树：既拿到波及面，也顺手判定了 id 在不在（0 就是不在）。
	if err := tx.QueryRow(subtreeCTE+` SELECT count(*) FROM subtree`, id).Scan(&res.DeletedTags); err != nil {
		return DeleteResult{}, fmt.Errorf("数子树: %w", err)
	}
	if res.DeletedTags == 0 {
		return DeleteResult{}, fmt.Errorf("%w: id=%d", ErrNotFound, id)
	}

	// 去重后数：一道题挂了这棵子树上好几个标签，只算它一道。
	const countQuestions = subtreeCTE + `
		SELECT count(DISTINCT question_id) FROM question_tags
		WHERE tag_id IN (SELECT id FROM subtree)`
	if err := tx.QueryRow(countQuestions, id).Scan(&res.UntaggedQuestions); err != nil {
		return DeleteResult{}, fmt.Errorf("数受影响的错题: %w", err)
	}

	if _, err := tx.Exec(subtreeCTE+` DELETE FROM question_tags WHERE tag_id IN (SELECT id FROM subtree)`, id); err != nil {
		return DeleteResult{}, fmt.Errorf("摘掉错题上的标签: %w", err)
	}
	if _, err := tx.Exec(subtreeCTE+` DELETE FROM tags WHERE id IN (SELECT id FROM subtree)`, id); err != nil {
		return DeleteResult{}, fmt.Errorf("删除标签: %w", err)
	}
	return res, tx.Commit()
}

// setQuestionTags 把一道错题挂着的标签整体替换成 tagIDs（空集 = 摘掉全部）。
//
// 一次替换在一个事务里完成：中间失败不会留下「删了旧的、没写新的」那种半截状态。
// tagIDs 假定已去重（Service 那边去过了，主键也不允许重复）。
func (s *Store) setQuestionTags(questionID int64, tagIDs []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("改错题标签: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM question_tags WHERE question_id = ?`, questionID); err != nil {
		return fmt.Errorf("清掉旧标签: %w", err)
	}

	now := time.Now().UTC().UnixMilli()
	for _, tagID := range tagIDs {
		_, err := tx.Exec(
			`INSERT INTO question_tags (question_id, tag_id, created_at) VALUES (?, ?, ?)`,
			questionID, tagID, now,
		)
		if err != nil {
			// 到这儿还能撞上约束，只可能是调用方绕过 Service 传了不存在的 id。
			return fmt.Errorf("挂标签 %d: %w", tagID, err)
		}
	}
	return tx.Commit()
}

// addQuestionTag 补挂一个标签；已经挂过就什么也不做（幂等）。
func (s *Store) addQuestionTag(questionID, tagID int64) error {
	_, err := s.db.Exec(
		`INSERT INTO question_tags (question_id, tag_id, created_at) VALUES (?, ?, ?)
		 ON CONFLICT (question_id, tag_id) DO NOTHING`,
		questionID, tagID, time.Now().UTC().UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("挂标签 %d: %w", tagID, err)
	}
	return nil
}

// removeQuestionTag 摘掉一个标签；本来就没挂也什么也不做（幂等）。
func (s *Store) removeQuestionTag(questionID, tagID int64) error {
	_, err := s.db.Exec(
		`DELETE FROM question_tags WHERE question_id = ? AND tag_id = ?`,
		questionID, tagID,
	)
	if err != nil {
		return fmt.Errorf("摘标签 %d: %w", tagID, err)
	}
	return nil
}

// tagsOfQuestion 返回一道错题挂着的标签，按层级排。
func (s *Store) tagsOfQuestion(questionID int64) ([]Tag, error) {
	rows, err := s.db.Query(
		`SELECT `+tagCols("t")+` FROM tags t JOIN question_tags qt ON qt.tag_id = t.id
		 WHERE qt.question_id = ? ORDER BY t.level, t.created_at, t.id`,
		questionID,
	)
	if err != nil {
		return nil, fmt.Errorf("读错题的标签: %w", err)
	}
	defer rows.Close()

	ts := []Tag{}
	for rows.Next() {
		t, err := scanTag(rows)
		if err != nil {
			return nil, fmt.Errorf("读错题的标签: %w", err)
		}
		ts = append(ts, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读错题的标签: %w", err)
	}
	return ts, nil
}

// tagsOfQuestions 一次取多道错题的标签，返回的顺序与传入的 ids 一致。
//
// 一次查完而不是一道道查：列表页要为屏上每一行显示标签，一道题一次 IPC 太浪费。
func (s *Store) tagsOfQuestions(questionIDs []int64) ([]QuestionTags, error) {
	if len(questionIDs) == 0 {
		return []QuestionTags{}, nil
	}

	args := make([]any, len(questionIDs))
	for i, id := range questionIDs {
		args[i] = id
	}
	rows, err := s.db.Query(
		`SELECT qt.question_id, `+tagCols("t")+`
		 FROM question_tags qt JOIN tags t ON t.id = qt.tag_id
		 WHERE qt.question_id IN (`+placeholders(len(questionIDs))+`)
		 ORDER BY qt.question_id, t.level, t.created_at, t.id`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("批量读错题的标签: %w", err)
	}
	defer rows.Close()

	byQuestion := map[int64][]Tag{}
	for rows.Next() {
		var (
			questionID int64
			t          Tag
		)
		if err := scanTagInto(rows, &questionID, &t); err != nil {
			return nil, fmt.Errorf("批量读错题的标签: %w", err)
		}
		byQuestion[questionID] = append(byQuestion[questionID], t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("批量读错题的标签: %w", err)
	}

	// 按传入顺序铺出来，没挂标签的题也在结果里（Tags 为空切片）—— 界面不必自己对齐。
	out := make([]QuestionTags, 0, len(questionIDs))
	seen := make(map[int64]bool, len(questionIDs))
	for _, id := range questionIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		ts := byQuestion[id]
		if ts == nil {
			ts = []Tag{}
		}
		out = append(out, QuestionTags{QuestionID: id, Tags: ts})
	}
	return out, nil
}

// untaggedQuestions 返回**一条标签都没挂**的错题，新的在前。
//
// 它读的是 questions 表，但落点在 question_tags 上：「没有关联行」是**关联表**的属性，
// 是标签规则的一种（对面那种是「挂上了某个标签」，在 library 的 ListQuestionsTaggedWith）。
// 表结构与连接都是同一份，所以放在本包不改变任何所有权，只是别处搜不到这条查询而已。
//
// 排序与 ListQuestions、ListQuestionsTaggedWith 一致（新的在前），
// 这样「未打标签」与其他标签页翻起来是同一个手感。
func (s *Store) untaggedQuestions() ([]library.Question, error) {
	// 用 NOT EXISTS 而不是 LEFT JOIN ... IS NULL，也不用 NOT IN：
	//   - 它就是这句话本身（「没有一行指向它」），读的人不必先认出 LEFT JOIN 的意图；
	//   - 一行题只会出一行（LEFT JOIN 得靠 DISTINCT 兜底），排序不会被关联行数搅乱；
	//   - NOT IN 碰上 NULL 会整条查询不出任何结果，这个坑没必要留着。
	// question_tags 的主键是 (question_id, tag_id) 且是 WITHOUT ROWID，
	// 所以这个子查询是按题走一次主键查找，不是每题扫一遍全表。
	rows, err := s.db.Query(
		`SELECT q.id, q.question_hash, q.answer_hash, q.created_at
		 FROM questions q
		 WHERE NOT EXISTS (SELECT 1 FROM question_tags qt WHERE qt.question_id = q.id)
		 ORDER BY q.created_at DESC, q.id DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("列未打标签的错题: %w", err)
	}
	defer rows.Close()

	qs := []library.Question{} // 空切片而不是 nil：前端拿到的是 []，不是 null
	for rows.Next() {
		var (
			q  library.Question
			ms int64
		)
		// 列的顺序与 library 的 scanQuestion 一致（那边没导出，扫法在这边再写一遍）。
		if err := rows.Scan(&q.ID, &q.QuestionHash, &q.AnswerHash, &ms); err != nil {
			return nil, fmt.Errorf("读未打标签的错题: %w", err)
		}
		q.CreatedAt = time.UnixMilli(ms).UTC()
		qs = append(qs, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列未打标签的错题: %w", err)
	}
	return qs, nil
}

// placeholders 生成 n 个 "?"，给动态 IN 列表用。
//
// 动态的只是**个数**，值仍然走占位符传，不拼进 SQL。
// 本包与 library 各留了一份：三行的东西不值得为它开一个包。
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// writeErr 把写标签时的约束报错翻成人话；认不出来的原样包一层带回去。
//
// 驱动只给一句字符串、没有错误码可认，所以按子串判断 —— 那两条唯一索引就写在本包的
// schema 里（migrations.go 的迁移 2），改它们的时候这里要一起改。
func writeErr(err error, name string) error {
	if strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return fmt.Errorf("%w: %q", ErrDuplicate, name)
	}
	return fmt.Errorf("写入标签 %q: %w", name, err)
}

// scanner 让 scanTag 同时吃得下 *sql.Row 与 *sql.Rows。
type scanner interface {
	Scan(dest ...any) error
}

func scanTag(sc scanner) (Tag, error) {
	var t Tag
	if err := scanTagInto(sc, nil, &t); err != nil {
		return Tag{}, err
	}
	return t, nil
}

// scanTagInto 把一行标签读进 t。questionID 非 nil 时，多读第一列的错题 id
// —— 批量查是「错题 id + 标签列」拼在一行里的。
func scanTagInto(sc scanner, questionID *int64, t *Tag) error {
	var (
		parentID sql.NullInt64
		level    int
		ms       int64
	)
	dest := []any{&t.ID, &parentID, &t.Name, &level, &ms}
	if questionID != nil {
		dest = append([]any{questionID}, dest...)
	}
	if err := sc.Scan(dest...); err != nil {
		return err
	}
	if parentID.Valid {
		t.ParentID = parentID.Int64
	}
	t.Level = Level(level)
	t.CreatedAt = time.UnixMilli(ms).UTC()
	return nil
}
