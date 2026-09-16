package library

import (
	"fmt"
	"strings"
	"testing"
)

// 每条迁移声明的「我建了哪些表」必须与它自己的 SQL 对得上。
//
// 这份声明是 TablesIntroducedAfter 的唯一来源，而后者决定了几处测试扮演老库时要删掉什么。
// 声明写错（漏一张、拼错名）的后果是：那些测试**静默地**开始测一个假的场景 ——
// 老库不是老库，升级路径根本没被走到，但测试是绿的。所以这里要双向卡住。
func TestMigrationTablesMatchWhatStmtsCreate(t *testing.T) {
	for _, m := range migrations {
		joined := strings.Join(m.stmts, "\n")

		// 声明的每一张都要真的建出来（挡住拼错名 / 声明了却没写 SQL）
		for _, tbl := range m.tables {
			if !strings.Contains(joined, "CREATE TABLE "+tbl+" (") {
				t.Errorf("迁移 %d（%s）声明建 %q，但 SQL 里没有那句 CREATE TABLE",
					m.version, m.name, tbl)
			}
		}

		// SQL 里每一处 CREATE TABLE 都要被声明到（挡住建了却没声明）。
		//
		// 数个数而不是解析表名：这几条语句是我们自己写的、形状固定，而数个数不会
		// 因为解析器的边界情况漏判 —— 一个漏判的解析器会让这条测试自己变成空转。
		if n := strings.Count(joined, "CREATE TABLE "); n != len(m.tables) {
			t.Errorf("迁移 %d（%s）的 SQL 里有 %d 处 CREATE TABLE，但 tables 声明了 %d 张（%v）",
				m.version, m.name, n, len(m.tables), m.tables)
		}
	}
}

// 迁移全跑完之后，库里**恰好**是各条迁移声明的那些表。
//
// 双向断言：声明了却没建出来会红，建了却没声明也会红。后半条是真正防漂移的那半边 ——
// 新加一条迁移时忘了填 tables，就是从这里红的。
func TestDeclaredTablesAreAllThereAfterMigrating(t *testing.T) {
	s := newStore(t)
	defer s.Close()

	rows, err := s.db.Query(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatalf("读 sqlite_master 失败: %v", err)
	}
	defer rows.Close()

	actual := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("读表名失败: %v", err)
		}
		actual[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读表名失败: %v", err)
	}

	declared := map[string]bool{}
	for _, m := range migrations {
		for _, tbl := range m.tables {
			declared[tbl] = true
		}
	}

	// 空集合会让下面两个循环都空转，所以先确认两边都不是空的。
	if len(declared) == 0 || len(actual) == 0 {
		t.Fatalf("集合是空的，断言会空转：声明 %d 张、实际 %d 张", len(declared), len(actual))
	}

	for tbl := range declared {
		if !actual[tbl] {
			t.Errorf("迁移声明了 %q，但全跑完之后库里没有它", tbl)
		}
	}
	for tbl := range actual {
		if !declared[tbl] {
			t.Errorf("库里有 %q，但没有任何迁移声明它（往 tables 里补上）", tbl)
		}
	}
}

// TablesIntroducedAfter 是几处测试扮演老库时唯一的依据，形状要钉住。
func TestTablesIntroducedAfter(t *testing.T) {
	// 退回票据 05 年代的库（version = 1）时，要删掉的就是这六张，且按迁移顺序。
	want := []string{
		"tags", "question_tags", // 迁移 2
		"review_states", "review_logs", // 迁移 3
		"discussions",     // 迁移 4
		"pending_changes", // 迁移 5
	}
	got := TablesIntroducedAfter(1)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("TablesIntroducedAfter(1) = %v，想要 %v", got, want)
	}

	// 当前版本之后没有东西了 —— 这条同时挡住「playMigrations 之后又有人追加迁移却忘了
	// 更新上面那份 want」（那种情况 want 少一项，上一条会红；而这里保证末尾是干净的）。
	if got := TablesIntroducedAfter(latestVersion()); len(got) != 0 {
		t.Errorf("TablesIntroducedAfter(%d) = %v，想要空", latestVersion(), got)
	}
}
