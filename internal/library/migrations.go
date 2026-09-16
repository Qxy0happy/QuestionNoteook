package library

// migration 是一次模式变更。stmts 按写下的顺序执行，整体包在一个事务里。
type migration struct {
	version int
	name    string
	stmts   []string
	// tables 是这条迁移**建的**表（不含索引 —— 索引跟着表一起被删）。
	//
	// 它存在只为一件事：让「退回某个版本」可推。测试里要扮演一个票据 05 年代的库，
	// 就得把「那之后每条迁移建的表」删掉；把这份清单抄在测试里，每加一条迁移都要
	// 记得去改 N 个文件 —— 这件事已经被漏掉四次了（迁移 3、4、5 各一次，加上一次
	// 差点漏）。所以清单放在这儿，由 TablesIntroducedAfter 汇总，测试只管调它。
	//
	// 它不是「另一份要靠自觉维护的名单」：与 stmts 的一致性由
	// TestMigrationTablesMatchWhatStmtsCreate 盯着，写错了会红。
	tables []string
}

// migrations 是本库的全部模式变更，version 从 1 起严格递增。
//
// 加一张表 / 加一列 = 往末尾追加一条，version 取当前最大 +1。
// **已经发布出去的那几条永远不要改** —— 老库只会跑比自己版本号大的那些，
// 改了它们等于让老库和新库模式不一致。要改就追加新的一条。
//
// 这张表里还没有的：（暂空 —— 待批准改动是最后加进来的那一条，见版本 5）。
var migrations = []migration{
	{
		version: 1,
		name:    "建 questions 表",
		tables:  []string{"questions"},
		stmts: []string{
			`CREATE TABLE questions (
				-- AUTOINCREMENT：id 不回收。删掉的错题不该让新题捡到它的 id。
				id            INTEGER PRIMARY KEY AUTOINCREMENT,
				-- 题图的内容 hash，必有。图片本体在库外（ADR-0004）。
				question_hash TEXT    NOT NULL CHECK (question_hash <> ''),
				-- 答案图的内容 hash；空串表示还没拍答案。
				answer_hash   TEXT    NOT NULL DEFAULT '',
				-- Unix 毫秒（UTC）。存整数而不是时间文本，既省得纠结时区，
				-- 也避开了时间字符串按字典序排不对的坑。
				created_at    INTEGER NOT NULL
			)`,
			// 列表按「新拍的在前」排，这条索引正好覆盖那个排序。
			`CREATE INDEX questions_created_at_idx ON questions (created_at DESC, id DESC)`,
		},
	},
	{
		version: 2,
		name:    "建标签树与错题-标签关联，并预置考研四门",
		tables:  []string{"tags", "question_tags"},
		stmts: []string{
			// 标签是自引用树，固定三层：学科 > 章节 > 知识点。
			// 读写它的代码在 internal/tags —— 表建在这儿是因为这个库只有一套迁移机制。
			`CREATE TABLE tags (
				-- AUTOINCREMENT：id 不回收。删掉的标签不该让新建的标签捡到它的 id。
				id         INTEGER PRIMARY KEY AUTOINCREMENT,
				-- 父标签；NULL 表示顶层学科（学科没有父）。
				parent_id  INTEGER REFERENCES tags(id) ON DELETE CASCADE,
				name       TEXT    NOT NULL CHECK (name <> ''),
				-- 1 学科 / 2 章节 / 3 知识点。层级**存下来**而不是每次现算：
				-- 固定三层是硬约束，写成列才能让「知识点下面不许再挂」在 schema 层也拦得住。
				level      INTEGER NOT NULL CHECK (level BETWEEN 1 AND 3),
				created_at INTEGER NOT NULL,
				-- 有父必有层级、没父必是第一层：两者不能各说各话。
				CHECK ((parent_id IS NULL) = (level = 1))
			)`,
			// 同一个父下不许重名。拆两条索引是因为 SQLite 认为 NULL 互不相等，
			// 一条 (parent_id, name) 的唯一索引拦不住两个同名的顶层学科。
			`CREATE UNIQUE INDEX tags_sibling_name_uniq ON tags (parent_id, name)
				WHERE parent_id IS NOT NULL`,
			`CREATE UNIQUE INDEX tags_subject_name_uniq ON tags (name)
				WHERE parent_id IS NULL`,
			// 按父取子：前端拼树、删子树都要走它。
			`CREATE INDEX tags_parent_idx ON tags (parent_id, created_at)`,

			// 一道错题可以挂多个标签，一个标签也可以挂在多道题上。
			`CREATE TABLE question_tags (
				question_id INTEGER NOT NULL REFERENCES questions(id) ON DELETE CASCADE,
				tag_id      INTEGER NOT NULL REFERENCES tags(id)      ON DELETE CASCADE,
				created_at  INTEGER NOT NULL,
				-- 同一道题挂同一个标签只算一次；这条主键同时也是「按题查标签」的索引。
				PRIMARY KEY (question_id, tag_id)
			) WITHOUT ROWID`,
			// 筛题走的是反过来那一侧：按标签找题。
			`CREATE INDEX question_tags_tag_idx ON question_tags (tag_id, question_id)`,

			// 预置考研四门。它们与用户后建的学科没有任何区别（可改名、可删），
			// 只是替用户省掉「一开始必然是这几门」那一步。
			//
			// 写在迁移里 = 只在建库时发生一次：用户删掉某个预置学科之后，它不会自己长回来
			// （换个位置做「启动时补齐」就会，那是错的）。
			`INSERT INTO tags (parent_id, name, level, created_at) VALUES
				(NULL, '数学',   1, CAST(strftime('%s','now') AS INTEGER) * 1000),
				(NULL, '英语',   1, CAST(strftime('%s','now') AS INTEGER) * 1000),
				(NULL, '政治',   1, CAST(strftime('%s','now') AS INTEGER) * 1000),
				(NULL, '专业课', 1, CAST(strftime('%s','now') AS INTEGER) * 1000)`,
		},
	},
	{
		version: 3,
		name:    "建复习状态与复习记录",
		tables:  []string{"review_states", "review_logs"},
		stmts: []string{
			// 每道错题一份 FSRS 状态。列与官方 go-fsrs 的 Card 一一对应 ——
			// 这一行**就是**那张卡，中间不加一层自己的翻译（ADR-0002）。
			// 读写它的代码在 internal/review。
			//
			// 没有行的错题 = 从没复习过 = 新题。刻意不在建题时预建一行零值：
			// 「还没复习过」这件事，没有行本身就说得清楚；预建一行的代价是每个读它的
			// 地方都得再判断「这行是真的还是占位的」。队列那侧用
			// COALESCE(due_at, created_at) 表达了「新题从被拍下来那一刻起就到期」。
			`CREATE TABLE review_states (
				-- 一道题一份；题没了状态跟着走。主键本身就是「按题查状态」的索引。
				question_id     INTEGER PRIMARY KEY REFERENCES questions(id) ON DELETE CASCADE,
				-- Unix 毫秒（UTC）—— 与 questions.created_at 同一个约定。
				due_at          INTEGER NOT NULL,
				-- FSRS 的记忆量。REAL 是因为它们本来就是浮点，取整会把算法算出来的东西弄丢。
				stability       REAL    NOT NULL,
				difficulty      REAL    NOT NULL,
				-- 上次评级算出的间隔（天）。0 = 还没到过「天」的尺度（新题的零值）。
				scheduled_days  INTEGER NOT NULL,
				reps            INTEGER NOT NULL,
				lapses          INTEGER NOT NULL,
				-- 0 新 / 1 学习 / 2 复习 / 3 重学：就是 fsrs.State 的取值。
				state           INTEGER NOT NULL CHECK (state BETWEEN 0 AND 3),
				-- 上次复习时刻；0 表示还没有过（fsrs.Card 的零值 LastReview）。
				-- 不用 NULL：这里的「没有」恰好就是 0，多一种空值只是多一种要判的情况。
				last_review_at  INTEGER NOT NULL,
				remaining_steps INTEGER NOT NULL
			) WITHOUT ROWID`,
			// 队列只按一条查询取：到期时刻小于某条界线。这条索引正好覆盖它。
			`CREATE INDEX review_states_due_idx ON review_states (due_at)`,

			// 每次复习一条，只增不改。评级、时刻、当时算出的间隔是票里点名的三样；
			// 另外记下当时算出的到期与记忆量 —— FSRS 的输出就在手上，不留下来，
			// 「最近的表现」（票据 14）以后只能反推。
			`CREATE TABLE review_logs (
				id             INTEGER PRIMARY KEY AUTOINCREMENT,
				question_id    INTEGER NOT NULL REFERENCES questions(id) ON DELETE CASCADE,
				-- 1 Again / 2 Hard / 3 Good / 4 Easy：就是 fsrs.Rating 的取值。
				rating         INTEGER NOT NULL CHECK (rating BETWEEN 1 AND 4),
				reviewed_at    INTEGER NOT NULL,
				-- **这次**算出来的间隔，不是复习之前的那个（见 review.Store.save）。
				scheduled_days INTEGER NOT NULL,
				due_at         INTEGER NOT NULL,
				stability      REAL    NOT NULL,
				difficulty     REAL    NOT NULL
			)`,
			// 回看一道题的复习史（票据 14 的「最近的表现」）走这条。
			`CREATE INDEX review_logs_question_idx ON review_logs (question_id, reviewed_at DESC)`,
		},
	},
	{
		version: 4,
		name:    "建讨论记录",
		tables:  []string{"discussions"},
		stmts: []string{
			// 讨论是「与 VLM 就某道错题展开的对话」（CONTEXT.md），挂在错题上、可回看。
			// **一行 = 讨论里的一句话**：同一道题的那些行合起来就是它的会话，
			// 而每道题只有一条会话，所以不必再来一张会话表。
			// 读写它的代码在 internal/discussion。
			`CREATE TABLE discussions (
				-- AUTOINCREMENT：一行的先后不只由时间戳决定，见下面那条索引的说明。
				id          INTEGER PRIMARY KEY AUTOINCREMENT,
				-- 挂在哪道错题上。题没了，它的讨论跟着走 —— 不留指向不存在错题的行。
				question_id INTEGER NOT NULL REFERENCES questions(id) ON DELETE CASCADE,
				-- 谁说的。取值就是 vlm.Role 的那两个（user / assistant）：它要原样进请求报文，
				-- 中间不该隔一张对照表。
				role        TEXT    NOT NULL CHECK (role IN ('user', 'assistant')),
				-- 说了什么。空话不落库 —— 服务层先拦一道（空的问题根本发不出去），
				-- schema 这里再拦一道。
				text        TEXT    NOT NULL CHECK (text <> ''),
				-- Unix 毫秒（UTC）—— 与 questions.created_at 同一个约定。
				created_at  INTEGER NOT NULL
			)`,
			// 回看一道题的讨论只走这一条查询。尾上的 id 不是装饰：时间戳只到毫秒，
			// 用户那句与模型答的那句往往写在同一毫秒里，只按 created_at 排的话，
			// 同一个话题的两句话谁先谁后由 SQLite 的心情决定。id 是自增的，
			// 谁先写的谁小 —— 用它兜底，顺序就是确定的（与 questions 那边的排序同一个道理）。
			`CREATE INDEX discussions_question_idx ON discussions (question_id, created_at, id)`,
		},
	},
	{
		version: 5,
		name:    "建待批准改动表",
		tables:  []string{"pending_changes"},
		stmts: []string{
			// agent 提议的、尚未生效的改动（CONTEXT.md 的「待批准改动」）。**类型 + 载荷 + 状态**
			// 就是这张表的全部；载荷是一个 JSON 对象，按 action 解释（agent.Payload）。
			// 读写它的代码在 internal/agent，这条迁移只是借这个库的迁移机制 ——
			// 与标签、复习、讨论同一个做法。
			//
			// 为什么**落库**而不是只放在内存里：这一层是用户的一个收件箱。应用重启之后
			// 「agent 提议过什么」要是没了，用户就再也看不到那些改动，而他并没有拒绝过它们；
			// 界面上那个「待批准 N 条」也会自己归零。批准是一个**人的决定**，决定得有对象，
			// 而对象得活过这一进程。
			//
			// 与前面四条一样是裸的 CREATE，**不加 IF NOT EXISTS**。
			//
			// 这条迁移一度带过 IF NOT EXISTS，那是为了绕开一个测试维护问题：三个包里的
			// TestUpgradeFromOlderSchema 要扮演老库，得把「之后每条迁移建的表」删掉，
			// 而当时那份清单是手抄的、漏了这条新建的表。绕法是错的 —— IF NOT EXISTS 是
			// 真的弱化：「表在、但列不对」时它会静默放过，而那恰恰是迁移最该拦住的坏库。
			// 现在表清单由 migration.tables 汇总（见 TablesIntroducedAfter），测试不必手抄，
			// 于是没有绕的理由了。
			`CREATE TABLE pending_changes (
				-- AUTOINCREMENT：id 不回收。它同时也是「这条改动是第几条」的先后依据。
				id         INTEGER PRIMARY KEY AUTOINCREMENT,
				-- 要干什么。取值就是 agent.Action 那几个 —— 两边是一份契约的两半，
				-- 改一处就得改另一处（agent/change.go 的常量上写着这句话）。
				-- schema 这层也拦一道：别的种类的改动**根本存不进来**，
				-- 于是「agent 只能提议标签」不只是代码里的自觉。
				action     TEXT    NOT NULL CHECK (action IN ('create_tag', 'rename_tag', 'delete_tag', 'tag_question')),
				-- 载荷，一个 JSON 对象。存文本而不是拆成一堆列：不同的 action 要的字段不一样，
				-- 拆成列就会长出一片「这条改动用不上」的空列，而它本来就是**模型说的那句话**。
				payload    TEXT    NOT NULL,
				-- 一句给人看的中文（「把「中值定理」改名为「微分中值定理」」）。在**提议时**就写好：
				-- 那时 agent 手上还有整棵标签树，算得出波及面；批准的时候只要读一行。
				summary    TEXT    NOT NULL,
				-- 模型自己交代的为什么这么改。
				reason     TEXT    NOT NULL DEFAULT '',
				status     TEXT    NOT NULL CHECK (status IN ('pending', 'applied', 'discarded')),
				-- Unix 毫秒（UTC）—— 与 questions.created_at 同一个约定。
				created_at INTEGER NOT NULL,
				-- 用户做决定的时刻；0 表示还没决定。
				decided_at INTEGER NOT NULL DEFAULT 0,
				-- 批准时写库失败的原因。**状态仍是 pending** —— 那一步确实没发生，
				-- 不能因为试过一次就把它标成已生效。
				problem    TEXT    NOT NULL DEFAULT ''
			)`,
			// 列表（只要待批准的，早的在前）与计数都只走这一条。
			`CREATE INDEX pending_changes_status_idx ON pending_changes (status, created_at, id)`,
		},
	},
}

// latestVersion 是已定义的最高模式版本，空列表时为 0。
// TablesIntroducedAfter 返回版本号大于 version 的那些迁移建的表，按迁移顺序。
//
// 给测试用：要扮演一个某一年代的库，就得把「那之后建的表」都删掉再重开。
// 清单从 migrations 汇总，所以**新增一条迁移之后不必改任何测试** ——
// 这件事以前靠在每个测试里手抄，被漏掉过四次。
func TablesIntroducedAfter(version int) []string {
	var out []string
	for _, m := range migrations {
		if m.version > version {
			out = append(out, m.tables...)
		}
	}
	return out
}

func latestVersion() int {
	if len(migrations) == 0 {
		return 0
	}
	return migrations[len(migrations)-1].version
}
