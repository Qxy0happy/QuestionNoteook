// Package library 是错题的持久化层：增、删、查。
//
// 元数据存 SQLite，图片体积大、存在库外的文件里，库里只记内容 hash（ADR-0004）。
// 图片的**读**也归这里（spec 的服务划分），只是文件层由外部注入 —— 见 Service.images。
// 库文件路径由调用方注入（安卓上落在应用私有目录），测试传临时目录即可。
//
// 模式（schema）版本记在 SQLite 自带的 PRAGMA user_version 里，Open 时按序号补齐
// 缺失的迁移。加表 / 加列只需往 migrations.go 的列表末尾追加一条。
//
// 同一个库文件上还有别的表（标签是头一张），它们的读写代码在各自的包里
// （internal/tags），只有迁移与「按标签筛错题」这条查询留在这里 ——
// 前者是因为这个库只有一套迁移机制，后者是因为它要读 questions 表。
//
// 内容 hash 在这一层是**裸 string**，不是具名类型（票据 16 记的那条 Primitive Obsession）。
// 判定留着：具名的那一个在采集侧（capture.Hash），而题库不能 import 采集 —— 依赖只有
// 采集 → 题库一个方向。在这条边界上再立第三个 hash 类型换不来任何安全：两端看到的本来
// 就是文本（SQLite 的 TEXT 列、前端收到的字符串），只会让 main.go 的适配壳与
// capture.Store.LoadByHash 各多出一圈转换，而要动它就得三处一起动。
package library

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	// 纯 Go 实现的 SQLite 驱动，注册名 "sqlite"。
	// 选它而不是 mattn/go-sqlite3，是为了安卓交叉编译不沾 cgo。
	_ "modernc.org/sqlite"
)

const (
	driverName = "sqlite"

	// MemoryPath 传给 Open 时建一个进程内的库，进程退出即消失。
	//
	// 票据 16 把它记成「机会性的规格化过度」—— 全仓只有 TestOpenMemoryPath 用它。
	// 判定留着：删它就得连那条测试一起删（它是唯一的调用方），等于拿「少一条覆盖」
	// 换「少五行代码」；而 Open 里随它而来的 SetMaxOpenConns(1) 也不是可顺手删的装饰 ——
	// 内存库跟着连接走，池里再开一条就等于换了个空库。真有人用了（或真没人用且
	// 那条测试也删了），这两处一起再议。
	MemoryPath = ":memory:"
)

// ErrNotFound 表示库里没有这道错题。判断用 errors.Is。
var ErrNotFound = errors.New("错题不存在")

// Store 是错题的存储。必须用 Open 构造，零值不可用。
type Store struct {
	db *sql.DB
}

// Open 打开 path 处的库（不存在就新建），把模式升到最新版本后返回。
//
// path 是文件系统路径而不是 DSN —— 连接参数由本函数拼。父目录不存在会一并建出来。
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("库文件路径为空")
	}
	if path != MemoryPath {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("建库目录: %w", err)
		}
	}

	db, err := sql.Open(driverName, dsn(path))
	if err != nil {
		return nil, fmt.Errorf("打开库: %w", err)
	}
	if path == MemoryPath {
		// 内存库是跟着连接走的：池里再开一条连接就等于换了个空库，故限死一条。
		db.SetMaxOpenConns(1)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关掉底层连接池。之后再调用本对象的方法都会失败。
func (s *Store) Close() error {
	return s.db.Close()
}

// DB 返回底层连接池，供同一个库文件上的其它表共用（标签，以后还有复习状态等）。
//
// 那些包不该再 Open 一次同一个文件：WAL 下两条连接虽然能跑，但没有理由让两个池
// 去抢同一把写锁。对方拿到的是同一条连接，事务与锁的语义因此与这里一致。
func (s *Store) DB() *sql.DB { return s.db }

// ListQuestionsTaggedWith 返回挂了 tagIDs 里任一标签的错题，新的在前。
//
// 筛一个标签时**连它的子孙一起算**：点「数学」要带出它下面所有章节与知识点的题，
// 否则三层树在筛选上就没有意义了。想只筛某个节点本身，传叶子标签。
//
// tagIDs 为空表示**不筛**，返回全部错题 —— 界面上「一个标签都没勾」就是这个意思，
// 于是筛与不筛是同一个调用。传了不存在的 id 不算错，只是筛不出东西
// （标签可能刚被别处删掉，那是并发下的正常情况）。
func (s *Store) ListQuestionsTaggedWith(tagIDs []int64) ([]Question, error) {
	if len(tagIDs) == 0 {
		return s.ListQuestions()
	}

	// 子树用递归 CTE 算，筛选与展开在同一条语句里完成 —— 不必先把子孙捞回来再拼 IN。
	args := make([]any, len(tagIDs))
	for i, id := range tagIDs {
		args[i] = id
	}
	rows, err := s.db.Query(
		`WITH RECURSIVE subtree(id) AS (
			SELECT id FROM tags WHERE id IN (`+placeholders(len(tagIDs))+`)
			UNION
			SELECT t.id FROM tags t JOIN subtree s ON t.parent_id = s.id
		 )
		 SELECT DISTINCT q.id, q.question_hash, q.answer_hash, q.created_at
		 FROM questions q JOIN question_tags qt ON qt.question_id = q.id
		 WHERE qt.tag_id IN (SELECT id FROM subtree)
		 ORDER BY q.created_at DESC, q.id DESC`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("按标签筛错题: %w", err)
	}
	defer rows.Close()

	qs := []Question{} // 空切片而不是 nil：前端拿到的是 []，不是 null
	for rows.Next() {
		q, err := scanQuestion(rows)
		if err != nil {
			return nil, fmt.Errorf("按标签筛错题: %w", err)
		}
		qs = append(qs, q)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("按标签筛错题: %w", err)
	}
	return qs, nil
}

// placeholders 生成 n 个 "?"，给动态 IN 列表用。
//
// 动态的只是**个数**，值仍然走占位符传，不拼进 SQL。
// tags 包那边也留了一份：三行的东西不值得为它开一个包。
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// dsn 拼出驱动要的连接串。
//
// 路径前不加 "file:" 前缀，这样 Windows 的盘符和反斜杠都不用转义；驱动只把 "?"
// 之后的部分当参数解析（Windows 文件名本来就不允许 "?"，所以路径里不会撞上）。
//
// journal_mode 选 WAL：读写不互斥。代价是库文件旁边会多出 -wal / -shm 两个文件，
// 以后做导出（打包 zip）时得先 checkpoint，否则拷走的库可能缺最近的写入。
func dsn(path string) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)") // 撞上锁时先等一会儿，别立刻报 busy
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)") // 给后面的票要加的关联表（错题-标签）立规矩
	return path + "?" + q.Encode()
}

// migrate 把库的模式补到 migrations 里最高的版本。
func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("读模式版本: %w", err)
	}
	if want := latestVersion(); version > want {
		// 别拿新库喂老程序：老程序不认识新表，写下去只会把数据搞坏。
		return fmt.Errorf("库的模式版本是 %d，本程序只认到 %d，拒绝打开", version, want)
	}

	for _, m := range migrations {
		if m.version <= version {
			continue
		}
		if err := s.applyMigration(m); err != nil {
			return fmt.Errorf("迁移 %d (%s): %w", m.version, m.name, err)
		}
	}
	return nil
}

// applyMigration 在同一个事务里跑完一条迁移并推进 user_version。
// 中途失败整体回滚，不会留下半截模式。
func (s *Store) applyMigration(m migration) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // 提交之后是 no-op

	for _, stmt := range m.stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	// PRAGMA 不收占位符，只能拼串；version 来自本包写死的迁移表，不是外部输入。
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return err
	}
	return tx.Commit()
}
