// Package library 是错题的持久化层：增、删、查。
//
// 元数据存 SQLite，图片体积大、存在库外的文件里，库里只记内容 hash（ADR-0004）。
// 库文件路径由调用方注入（安卓上落在应用私有目录），测试传临时目录即可。
//
// 模式（schema）版本记在 SQLite 自带的 PRAGMA user_version 里，Open 时按序号补齐
// 缺失的迁移。加表 / 加列只需往 migrations.go 的列表末尾追加一条。
package library

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	// 纯 Go 实现的 SQLite 驱动，注册名 "sqlite"。
	// 选它而不是 mattn/go-sqlite3，是为了安卓交叉编译不沾 cgo。
	_ "modernc.org/sqlite"
)

const (
	driverName = "sqlite"

	// MemoryPath 传给 Open 时建一个进程内的库，进程退出即消失。
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
