// Package store 是 agent 那两样东西的持久化层：只读的数据快照，与待批准改动。
//
// ── 这个包的安全姿态（与 internal/agent 的包注释配对读）──
//
// 全应用里能摸到数据库句柄的代码只有寥寥几处，这里是其中两处，而它们的性质完全不同：
//
//   - **ReadStore 在类型上就写不了**。它持有的不是 *sql.DB，而是 Querier ——
//     一个只有 Query / QueryRow 的接口。它的字段里根本没有 Exec 这个东西，
//     所以「顺手加一句 UPDATE」不是被禁止，是**没有可以调用的东西**。
//     更宽的那个能力（*sql.DB）留在调用方（main.go）手里，这一层从来没拿到过。
//   - **PendingStore 有写的能力，但它只碰 pending_changes 一张表**。它的五个方法
//     全部只对这一张表说话，而这份方法集被 safety_test.go 用反射钉死：
//     往这一层多写一条改 questions / tags / review_states 的 SQL，就得先往那份白名单里
//     加一个方法，那一步是**故意**要人停下来想一想的。
//
// 这两件事合起来是「不存在绕过待批准层的路径」这句话的持久化侧。
//
// 连接一律是**错题库那一条**（由调用方传进来，见 library.Store.DB），本包不 Open 第二个
// 到同一文件的连接 —— WAL 下能跑，但没有理由让两个池去抢同一把写锁（与 tags、review 同一个做法）。
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"questionbook/internal/agent"
	"questionbook/internal/library"
	"questionbook/internal/tags"
)

// Querier 是数据库**只读的那一半**：只有 Query 与 QueryRow，没有 Exec。
//
// 这是本包最重要的一行设计。ReadStore 拿的是它而不是 *sql.DB，于是
// 「ReadStore 能不能写库」在类型上就有答案：不能，因为它手上没有那个方法。
//
// 为什么不干脆把连接开成只读的（DSN 上 mode=ro）：那份库是 WAL 的，库里只要还有
// -wal / -shm 文件，只读连接就可能打不开；而且 ADR-0004 说了元数据都走同一条连接。
// 换个连接参数换来的是一个只在特定时刻冒出来的故障，代价远大于这一行接口。
type Querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// questionReader 是题库用得上的那一小面。*library.Store 满足它。
//
// 为什么不直接收 *library.Store：那上面有 DeleteQuestion / SetAnswerHash / AddQuestion
// ——把整个 Store 递给读侧，等于把三个写动作也一起递过去，只为了用其中一个读方法。
// 接口按**用得上什么**来定，而不是按「谁现成」来定。
type questionReader interface {
	ListQuestions() ([]library.Question, error)
}

// tagReader 是标签服务用得上的那一小面。*tags.Service 满足它。
//
// 同理：tags.Service 上有 Create / Rename / Delete / SetQuestionTags，这里只要两个读方法。
type tagReader interface {
	List() ([]tags.Tag, error)
	TagsOfQuestions(questionIDs []int64) ([]tags.QuestionTags, error)
}

// ReadStore 是 agent 看数据的那一眼在数据库上的落地。
//
// 字段是三个接口加一个取时刻的函数 —— 一个数据库句柄都没有（见 Querier）。
type ReadStore struct {
	db        Querier
	questions questionReader
	tags      tagReader

	// now 决定「今天」是哪一天（到期计数要用它），也决定读出来的时刻换算到哪个时区。
	// 做成字段是为了能在测试里钉死：不然「今天到期几道」这种断言会跟着跑测试的机器走。
	now func() time.Time
}

// Option 给只读存储补一样可选能力。
type Option func(*ReadStore)

// WithNow 换掉取当前时刻的方式。只有测试会用。
func WithNow(now func() time.Time) Option {
	return func(s *ReadStore) { s.now = now }
}

// NewReadStore 接上连接、题库与标签树。
//
// db 通常是 library.Store.DB()。注意它是**宽**的那一个（*sql.DB），而本层只把它当
// Querier 用 —— 收窄发生在这一行赋值上，之后这个包里再也没有 Exec 可调。
func NewReadStore(db Querier, questions questionReader, tagSvc tagReader, opts ...Option) *ReadStore {
	s := &ReadStore{db: db, questions: questions, tags: tagSvc, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ListTags 返回全部标签。直接借标签服务的那一条查询，不在这里再抄一遍 SQL。
func (s *ReadStore) ListTags() ([]tags.Tag, error) { return s.tags.List() }

// Questions 返回全部错题，新拍的在前，每条带上它的标签与复习概况。
//
// 三次查询（错题、标签、复习状态）而不是一道题一次：个人错题本是几百行的量级，
// 一次读完远比让模型一页页翻可靠（见 agent.Reader 上那段说明）。
//
// 时间一律换算到 `now` 所在的时区再交出去：库里存的是绝对时刻（Unix 毫秒 UTC），
// 而用户与模型看的是**本地的日子**（「今天到期几道」也按本地日算，与 review 同一个口径）。
// 在这里换一次，上面的工具那一层就不必再操心时区。
func (s *ReadStore) Questions() ([]agent.QuestionView, error) {
	qs, err := s.questions.ListQuestions()
	if err != nil {
		return nil, fmt.Errorf("读错题: %w", err)
	}

	ids := make([]int64, 0, len(qs))
	for _, q := range qs {
		ids = append(ids, q.ID)
	}

	byQuestion := make(map[int64][]tags.Tag, len(ids))
	groups, err := s.tags.TagsOfQuestions(ids)
	if err != nil {
		return nil, fmt.Errorf("批量读错题的标签: %w", err)
	}
	for _, g := range groups {
		byQuestion[g.QuestionID] = g.Tags
	}

	states, err := s.reviewStates(ids)
	if err != nil {
		return nil, err
	}

	loc := s.now().Location()
	out := make([]agent.QuestionView, 0, len(qs))
	for _, q := range qs {
		v := agent.QuestionView{
			ID:        q.ID,
			CreatedAt: q.CreatedAt.In(loc),
			HasAnswer: q.AnswerHash != "",
			Tags:      byQuestion[q.ID],
		}
		if v.Tags == nil {
			v.Tags = []tags.Tag{} // 空切片而不是 nil：前端与模型都拿到 []，不是 null
		}
		if st, ok := states[q.ID]; ok {
			v.Reviewed = true
			v.Reps = st.reps
			v.Lapses = st.lapses
			v.Due = st.due.In(loc)
			v.LastReview = st.last.In(loc)
		}
		out = append(out, v)
	}
	return out, nil
}

// ReviewStats 按标签汇总复习情况，每个标签的数字**含它的子孙**。
//
// 为什么在 store 这一层上卷、而不是把「直接挂在这个标签上的数」交出去让上面自己加：
// 上卷要沿 parent 链走，而父链只有这里手上有（标签服务只给平的列表）。
// 更实际的是——把上卷放在这里，模型看到的每一行就已经是「这一块整体怎么样」，
// 「我数学哪块最弱」这种问题因此不需要模型自己去做加法（它做不好）。
//
// 两道查询：一次拿「(标签, 错题, 复习状态)」三者的关系，一次拿标签表。
// 题数不多（spec 里那句「量级小」），一次扫完比按标签递归下去省事得多。
//
// 返回的行数**以标签数为界**，与错题数无关：一个学科一行、一个章节一行，
// 无论底下挂了 5 道题还是 500 道题，交给模型的都还是那几行。
//
// 数字按「**一道题只算一次**」汇总。这一条比看上去容易写错：一道题常常同时挂在路径的
// 每一层上（手工打标签、与 SaveTags 都是那个形状），只把行数加一遍的话，
// 「数学」那一行会把它数三遍（自己一层 + 两个子孙各一层），于是学科的数字比错题总数还大。
func (s *ReadStore) ReviewStats() ([]agent.TagStats, error) {
	all, err := s.tags.List()
	if err != nil {
		return nil, fmt.Errorf("读标签: %w", err)
	}

	loc := s.now().Location()
	cutoff := endOfToday(s.now()).UnixMilli()

	// 先把每个标签起一行：一道题都没挂的标签也要出现（「这块我一道题都没拍」是答案的一部分，
	// 藏起来反而让模型以为那个标签不存在）。顺序沿用标签服务给的（层级 → 创建时间）。
	byID := make(map[int64]tags.Tag, len(all))
	order := make([]int64, 0, len(all))
	totals := make(map[int64]*agent.TagStats, len(all))
	for _, t := range all {
		byID[t.ID] = t
		order = append(order, t.ID)
		totals[t.ID] = &agent.TagStats{TagID: t.ID, Name: t.Name, Level: t.Level}
	}

	rows, err := s.db.Query(
		`SELECT qt.tag_id, q.id, q.created_at, rs.due_at, rs.reps, rs.lapses, rs.last_review_at
		 FROM question_tags qt
		 JOIN questions q      ON q.id = qt.question_id
		 LEFT JOIN review_states rs ON rs.question_id = q.id`,
	)
	if err != nil {
		return nil, fmt.Errorf("读复习汇总: %w", err)
	}
	defer rows.Close()

	// 一道题在一个标签上的那一份计数。
	type qagg struct {
		reviewed bool
		due      bool
		reps     int
		lapses   int
		last     time.Time
	}

	// direct 是「直接挂在这个标签上的那些题」，键是**题 id** 而不是计数。
	// 这正是「一道题只算一次」的落点：同一道题在路径的每一层上各出现一行，
	// 而这里按 id 收进同一个 map，一道题就只会留下一个条目。
	direct := map[int64]map[int64]qagg{}
	for rows.Next() {
		var (
			tagID             int64
			questionID        int64
			createdAt         int64
			due, reps, lapses sql.NullInt64
			lastReview        sql.NullInt64
		)
		if err := rows.Scan(&tagID, &questionID, &createdAt, &due, &reps, &lapses, &lastReview); err != nil {
			return nil, fmt.Errorf("读复习汇总: %w", err)
		}
		if _, ok := byID[tagID]; !ok {
			// 外键保证不会发生（标签删了关联跟着走）。真出现了也不该把整次读弄崩，
			// 跳过这一行 —— 它没有名字，交给模型也没用。
			continue
		}

		a := qagg{}
		if due.Valid { // 有这一行 = 复习过
			a.reviewed = true
			a.reps = int(reps.Int64)
			a.lapses = int(lapses.Int64)
			a.last = time.UnixMilli(lastReview.Int64).In(loc)
			// 到期时刻用的是**复习队列那一条规则**：没复习过的题没有 review_states 那一行，
			// 而它是新题 —— 从被拍下来那一刻起就该复习，所以拿 created_at 顶到期时刻
			// （review.Store.dueItems 的 COALESCE 就是这句话）。
			//
			// 这一条必须与那边一模一样：不然「今天还剩几道」在复习页与 agent 嘴里会差一个数，
			// 而那种不一致最难查 —— 两个数看着都挺合理。
			a.due = due.Int64 < cutoff
		} else {
			a.due = createdAt < cutoff
		}

		m := direct[tagID]
		if m == nil {
			m = make(map[int64]qagg)
			direct[tagID] = m
		}
		m[questionID] = a
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读复习汇总: %w", err)
	}

	// 把「直接挂在某标签上」的那些题沿父链往上并一份。并的是**题**不是计数：
	// 同一道题从自己那一层和从子孙那一层走上来，落进的是同一个条目，
	// 所以「数学」看到的是「它下面有几道题」而不是「有几条路径」。
	//
	// 步数上限是防环的，与 tagPath 同一个数：真出现环时最多走 8 步就停。
	merged := make(map[int64]map[int64]qagg, len(totals))
	for id, qs := range direct {
		for at, steps := id, 0; steps < maxTagDepth; steps++ {
			t, ok := byID[at]
			if !ok {
				break
			}
			m := merged[at]
			if m == nil {
				m = make(map[int64]qagg, len(qs))
				merged[at] = m
			}
			for qid, a := range qs {
				m[qid] = a
			}
			if t.ParentID == 0 || t.ParentID == at {
				break
			}
			at = t.ParentID
		}
	}

	out := make([]agent.TagStats, 0, len(order))
	for _, id := range order {
		t := totals[id]
		t.Path = tagPath(byID, id)
		for _, a := range merged[id] {
			t.Questions++
			if a.reviewed {
				t.Reviewed++
			}
			if a.due {
				t.Due++
			}
			t.Reps += a.reps
			t.Lapses += a.lapses
			if a.last.After(t.LastReview) {
				t.LastReview = a.last
			}
		}
		out = append(out, *t)
	}
	return out, nil
}

// maxTagDepth 是沿父链往上走时的步数上限，防环用。与 tagPath 里那个 8 是同一件事：
// 标签只有三层，8 步纯属保险丝，正常永远走不到。
const maxTagDepth = 8

// ReviewHistory 返回一道错题的复习记录，最近的在先。
//
// 题不存在时返回空集而不是报错：这是个读，拿一个刚被删掉的 id 来问是正常的
// （与 tags.TagsOfQuestion、discussion.History 同一条规矩）。
//
// limit <= 0 表示不限量：SQLite 里 LIMIT 为负数就是这个意思，所以传 -1 而不是 0
// ——「没给上限」与「要 0 条」不该混成同一件事。
func (s *ReadStore) ReviewHistory(questionID int64, limit int) ([]agent.ReviewEntry, error) {
	if limit <= 0 {
		limit = -1
	}
	loc := s.now().Location()

	// 同一毫秒里可能写下好几条（时钟粒度的锅），所以再按自增 id 兜一层
	// ——与讨论记录、错题列表同一个道理：谁先写的谁小。
	rows, err := s.db.Query(
		`SELECT question_id, rating, reviewed_at, scheduled_days, due_at
		 FROM review_logs WHERE question_id = ?
		 ORDER BY reviewed_at DESC, id DESC LIMIT ?`,
		questionID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("读复习记录: %w", err)
	}
	defer rows.Close()

	out := []agent.ReviewEntry{} // 空切片而不是 nil
	for rows.Next() {
		var (
			e          agent.ReviewEntry
			reviewedAt int64
			dueAt      int64
		)
		if err := rows.Scan(&e.QuestionID, &e.Rating, &reviewedAt, &e.ScheduledDays, &dueAt); err != nil {
			return nil, fmt.Errorf("读复习记录: %w", err)
		}
		e.ReviewedAt = time.UnixMilli(reviewedAt).In(loc)
		e.DueAt = time.UnixMilli(dueAt).In(loc)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读复习记录: %w", err)
	}
	return out, nil
}

// reviewState 是一道题在 review_states 里的那一行，只取汇总用得上的几列。
type reviewState struct {
	due    time.Time
	reps   int
	lapses int
	last   time.Time
}

// reviewStates 一次取多道题的复习状态。
//
// 直接对 review_states 写查询，而不是去借复习服务：那边的读（Queue）是**队列**，
// 问的是「今天该做哪些」，而这里要的是「每道题复习成什么样」——两个问题。
// 代价是本包知道了复习那几张表的列名，迁移 3 改列时这里要一起改。
func (s *ReadStore) reviewStates(questionIDs []int64) (map[int64]reviewState, error) {
	out := map[int64]reviewState{}
	if len(questionIDs) == 0 {
		return out, nil
	}

	args := make([]any, len(questionIDs))
	for i, id := range questionIDs {
		args[i] = id
	}
	rows, err := s.db.Query(
		`SELECT question_id, due_at, reps, lapses, last_review_at
		 FROM review_states WHERE question_id IN (`+placeholders(len(questionIDs))+`)`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("读复习状态: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id           int64
			due, last    int64
			reps, lapses int
		)
		if err := rows.Scan(&id, &due, &reps, &lapses, &last); err != nil {
			return nil, fmt.Errorf("读复习状态: %w", err)
		}
		out[id] = reviewState{
			due:    time.UnixMilli(due),
			reps:   reps,
			lapses: lapses,
			last:   time.UnixMilli(last),
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读复习状态: %w", err)
	}
	return out, nil
}

// tagPath 从一个标签往上走到根，拼出「数学 / 概率论 / 全概率公式」。
//
// 与 agent.snapshot.path 是同一条规则的另一份实现：那边在内存里走索引，这边在
// 一张已经拿到的 map 上走。为三行代码把两边缝起来不值得 —— 但两处要一起改。
//
// 步数上限是防环的：真出现环时最多走 8 步就停，不会把一次提问挂死。
func tagPath(byID map[int64]tags.Tag, id int64) string {
	var names []string
	for at, steps := id, 0; at != 0 && steps < 8; steps++ {
		t, ok := byID[at]
		if !ok {
			break
		}
		names = append(names, t.Name)
		at = t.ParentID
	}
	for i, j := 0, len(names)-1; i < j; i, j = i+1, j-1 {
		names[i], names[j] = names[j], names[i]
	}
	out := ""
	for i, n := range names {
		if i > 0 {
			out += " / "
		}
		out += n
	}
	return out
}

// endOfToday 返回 now 所在那一天的结束，也就是**次日零点**，时区取 now 自己的。
//
// 与 review.endOfToday 是同一条规则的另一份实现（那个没导出）。为什么是「今天」而不是
// 「到期时刻已经过去」：详细理由写在 review 那一份上，这里只重复结论 ——
// 「今天该复习哪些」是个**天**的问题，票里的三处（今日队列、当天到期数量、每日汇总）
// 问的都是天。两份实现的口径必须一致，否则「今天还剩几道」在两个页面上会是两个数。
func endOfToday(now time.Time) time.Time {
	y, m, d := now.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, now.Location()).AddDate(0, 0, 1)
}

// placeholders 生成 n 个 "?"，给动态 IN 列表用。
//
// 动态的只是**个数**，值仍然走占位符传，不拼进 SQL。
// library、tags 各留了一份：三行的东西不值得为它开一个包。
func placeholders(n int) string {
	out := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			out += ","
		}
		out += "?"
	}
	return out
}

// ── 待批准改动 ──

// PendingStore 是 pending_changes（迁移 5）这一张表的读写。
//
// 它是本包唯一有写能力的东西，而它写的**只有这一张表**：五个方法里的每一句 SQL 都只对
// pending_changes 说话。这份方法集被 safety_test.go 用反射钉死，见包注释。
type PendingStore struct{ db *sql.DB }

// NewPendingStore 用一个已经开好的连接构造存储。
//
// 这里收的是**宽**的 *sql.DB（不是 Querier）：它要写。能力的边界靠方法集划，
// 不靠把句柄收窄 —— 收窄了就没法 INSERT 了。
func NewPendingStore(db *sql.DB) *PendingStore { return &PendingStore{db: db} }

// pendingCols 拼出待批准改动的列，顺序与 scanPending 一致 —— 只此一份，别再手抄一遍。
const pendingCols = `id, action, payload, summary, reason, status, created_at, decided_at, problem`

// Propose 记下一条改动，返回落库后的它。
//
// 落之前再验一次口径：调用方（agent.Pending）已经验过了，这里不省这一道 ——
// 这个类型是可以被直接构造出来用的（安全测试就绕开服务层直接用它），
// 而「脏东西不进这张表」这条规矩不该依赖调用方守规矩。
func (s *PendingStore) Propose(c agent.Change, at time.Time) (agent.PendingChange, error) {
	if err := c.Validate(); err != nil {
		return agent.PendingChange{}, err
	}
	payload, err := json.Marshal(c.Payload)
	if err != nil {
		return agent.PendingChange{}, fmt.Errorf("编码改动载荷: %w", err)
	}

	res, err := s.db.Exec(
		`INSERT INTO pending_changes (action, payload, summary, reason, status, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		string(c.Action), string(payload), c.Summary, c.Reason,
		string(agent.StatusPending), at.UTC().Truncate(time.Millisecond).UnixMilli(),
	)
	if err != nil {
		return agent.PendingChange{}, fmt.Errorf("写入待批准改动: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return agent.PendingChange{}, fmt.Errorf("取待批准改动的 ID: %w", err)
	}
	return s.Get(id)
}

// List 返回**还没决定**的那些，早的在前。
//
// 已经生效 / 已丢弃的不在里面：生效的已经落在标签树上（去那儿看得到），
// 丢弃的是用户自己不要了 —— 两者都不该继续占着这份「积压的活」。
// 它们的行留在表里，是给「这条改动后来怎么了」留的凭据。
func (s *PendingStore) List() ([]agent.PendingChange, error) {
	rows, err := s.db.Query(
		`SELECT `+pendingCols+` FROM pending_changes
		 WHERE status = ? ORDER BY created_at, id`,
		string(agent.StatusPending),
	)
	if err != nil {
		return nil, fmt.Errorf("列出待批准改动: %w", err)
	}
	defer rows.Close()

	out := []agent.PendingChange{} // 空切片而不是 nil：前端要拿到 []，不是 null
	for rows.Next() {
		p, err := scanPending(rows)
		if err != nil {
			return nil, fmt.Errorf("读待批准改动: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("列出待批准改动: %w", err)
	}
	return out, nil
}

// Count 数还没决定的那些。
func (s *PendingStore) Count() (int, error) {
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM pending_changes WHERE status = ?`, string(agent.StatusPending),
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("数待批准改动: %w", err)
	}
	return n, nil
}

// Get 按 id 取一条（不管什么状态）。没有就返回 agent.ErrNotFound。
func (s *PendingStore) Get(id int64) (agent.PendingChange, error) {
	row := s.db.QueryRow(`SELECT `+pendingCols+` FROM pending_changes WHERE id = ?`, id)
	p, err := scanPending(row)
	if errors.Is(err, sql.ErrNoRows) {
		return agent.PendingChange{}, fmt.Errorf("%w: id=%d", agent.ErrNotFound, id)
	}
	if err != nil {
		return agent.PendingChange{}, fmt.Errorf("读待批准改动: %w", err)
	}
	return p, nil
}

// Decide 记下用户对一条改动的决定。
//
// status 为 pending 时走的是**另一条**语句：只写 problem，不碰 status 也不碰 decided_at
// ——那是「批准时执行失败了」，那一步确实没有发生（见 agent.PendingStore 的说明）。
//
// 两个分支都以实际影响的行数为准：id 不存在就是 ErrNotFound，而不是一个
// 「悄悄什么都没发生」的成功（与 library、tags、review 那边同一条规矩）。
func (s *PendingStore) Decide(id int64, status agent.Status, at time.Time, problem string) error {
	if status == agent.StatusPending {
		res, err := s.db.Exec(`UPDATE pending_changes SET problem = ? WHERE id = ?`, problem, id)
		if err != nil {
			return fmt.Errorf("记下待批准改动的问题: %w", err)
		}
		return s.checkAffected(res, id)
	}

	res, err := s.db.Exec(
		`UPDATE pending_changes SET status = ?, decided_at = ?, problem = ? WHERE id = ?`,
		string(status), at.UTC().Truncate(time.Millisecond).UnixMilli(), problem, id,
	)
	if err != nil {
		return fmt.Errorf("记下对待批准改动的决定: %w", err)
	}
	return s.checkAffected(res, id)
}

// checkAffected 把「影响 0 行」翻成 ErrNotFound。
func (s *PendingStore) checkAffected(res sql.Result, id int64) error {
	n, err := res.RowsAffected()
	if err != nil {
		return nil // 驱动不给就算了，不值得为它把一次成功的写入报成失败
	}
	if n == 0 {
		return fmt.Errorf("%w: id=%d", agent.ErrNotFound, id)
	}
	return nil
}

// scanner 让 scanPending 同时吃得下 *sql.Row 与 *sql.Rows。
type scanner interface {
	Scan(dest ...any) error
}

func scanPending(sc scanner) (agent.PendingChange, error) {
	var (
		p                    agent.PendingChange
		action, status       string
		payload              string
		createdAt, decidedAt int64
	)
	if err := sc.Scan(&p.ID, &action, &payload, &p.Summary, &p.Reason, &status, &createdAt, &decidedAt, &p.Problem); err != nil {
		return agent.PendingChange{}, err
	}
	p.Action = agent.Action(action)
	p.Status = agent.Status(status)
	if err := json.Unmarshal([]byte(payload), &p.Payload); err != nil {
		return agent.PendingChange{}, fmt.Errorf("解析改动载荷: %w", err)
	}
	p.CreatedAt = time.UnixMilli(createdAt).UTC()
	if decidedAt != 0 {
		p.DecidedAt = time.UnixMilli(decidedAt).UTC()
	}
	return p, nil
}
