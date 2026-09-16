package library

// migration 是一次模式变更。stmts 按写下的顺序执行，整体包在一个事务里。
type migration struct {
	version int
	name    string
	stmts   []string
}

// migrations 是本库的全部模式变更，version 从 1 起严格递增。
//
// 加一张表 / 加一列 = 往末尾追加一条，version 取当前最大 +1。
// **已经发布出去的那几条永远不要改** —— 老库只会跑比自己版本号大的那些，
// 改了它们等于让老库和新库模式不一致。要改就追加新的一条。
//
// 这张表里还没有的：复习状态、复习记录、讨论、待批准改动。
// 它们跟着各自的票走，各加一条迁移。
var migrations = []migration{
	{
		version: 1,
		name:    "建 questions 表",
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
}

// latestVersion 是已定义的最高模式版本，空列表时为 0。
func latestVersion() int {
	if len(migrations) == 0 {
		return 0
	}
	return migrations[len(migrations)-1].version
}
