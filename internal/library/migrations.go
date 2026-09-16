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
// 这张表里还没有的：标签（自引用树）、错题-标签关联、复习状态、复习记录、
// 讨论、待批准改动。它们跟着各自的票走，各加一条迁移。
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
}

// latestVersion 是已定义的最高模式版本，空列表时为 0。
func latestVersion() int {
	if len(migrations) == 0 {
		return 0
	}
	return migrations[len(migrations)-1].version
}
