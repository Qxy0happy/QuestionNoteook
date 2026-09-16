package agent_test

import (
	"database/sql"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"questionbook/internal/agent"
	"questionbook/internal/agent/apply"
	"questionbook/internal/agent/store"
)

// 票据 11 的验收标准是一条**安全属性**，不是功能：
//
//	「不存在绕过待批准层直接改数据的路径。」
//
// 这个文件就是它的证据。它不是「我读过代码，没看见谁写库」——那种保证在有下一个人
// 顺手加一行的时候就没了。这里要把那句话变成**可执行的断言**：真有人加了一条写数据的
// 路，这些测试必须有几条变红。
//
// ── 这几把锁各锁一件事 ──
//
//	A. 接口的方法集恰好是那份白名单。agent.Reader 只能有那四个读方法，多一个就红。
//	B. 服务对象**手里攥着什么**：沿字段传递地走一遍，每个能碰到的本模块类型的方法名
//	   都必须在白名单里，而且不许碰得到 database/sql。
//	C. 具体的存储：*store.ReadStore 的方法集白名单，以及它连 *sql.DB 都拿不到。
//	D. *store.PendingStore 是**唯一**有写能力的那个，而它的方法集恰好是那五个
//	   ——「它只写 pending_changes 这一张表」靠这条 + 锁 E 的 SQL 检查一起成立。
//	E. 源码守卫：internal/agent 这一棵子树里 import 了什么、写了哪些 SQL、在哪调了 Exec。
//	   这一条管的是反射看不见的那一半（未导出的方法与 import）。
//	F. 行为：一轮提问把四种写操作全提一遍，正式数据逐行一模一样 —— 在 agent_test.go 的
//	   TestProposeTouchesNothingButPendingChanges。
//
// ── 这套锁诚实地讲，边界在哪 ──
//
// 反射只看得到**导出**的方法。一个包内未导出的 (s *ReadStore) exec(...) 在反射里
// 不存在 —— 但它同样也没法从包外被调用，而包内那一半由锁 E 兜底
// （.Exec( 出现在哪个文件、写 SQL 出现在哪些字面量里）。两半合起来才是完整的。
//
// 更强的那个保证不在测试里，在类型里：ReadStore 的字段是 Querier（只有 Query/QueryRow），
// 所以它**不是被禁止写**，是根本没有可以调用的东西 —— 见 store.Querier 上的说明。

// ── 白名单 ──
//
// 它们故意写成字面量、而不是从别处算出来：这份清单被改动，就是「这一层多了一个动作」
// 这件事本身。让它能自动跟着实现走的话，这把锁就白装了。

// 每份清单都按**字典序**排（测试用的是 slices.Equal，顺序也对）。写成读起来顺手的
// 「读方法在前」那种顺序会红，而那跟安全属性毫无关系 —— 排序这件事由 gofmt 之外
// 的一行注释交代，别让它变成一次无谓的失败。

var (
	// agent.Reader：只读入口的四个方法。
	allowReader = []string{"ListTags", "Questions", "ReviewHistory", "ReviewStats"}

	// agent.Proposer：唯一的出口，而它只往待批准表里写。
	allowProposer = []string{"Propose"}

	// agent.Model：只有说话这一个动作（没有 Upload —— agent 这条路上一张图都没有）。
	allowModel = []string{"Chat"}

	// agent.Applier：执行者唯一的动作。
	allowApplier = []string{"Apply"}

	// agent.Service：从它出发沿字段能碰到的**全部**本模块方法。
	allowServiceReach = []string{
		"Ask", "Chat", "ListTags", "Propose", "Questions", "ReviewHistory", "ReviewStats",
	}

	// agent.Pending：能批、能丢、能列、能数 —— 加上它手里那两个接口的方法。
	allowPendingReach = []string{
		"Apply", "Approve", "Count", "Decide", "Discard", "Get", "List", "Propose",
	}

	// store.ReadStore：四个读方法 + 它那三个接口字段的方法。
	allowReadStoreReach = []string{
		"List", "ListQuestions", "ListTags", "Query", "QueryRow",
		"Questions", "ReviewHistory", "ReviewStats", "TagsOfQuestions",
	}

	// 这些名字出现在任何**可达**类型上都不行 —— 它们就是「能写」本身。
	// 与白名单重复了一道是有意的：白名单管「多了一个动作就拦下来」，
	// 这一份管「不管你怎么绕，写出这个能力本身就拦下来」。
	forbiddenMethods = []string{
		"Begin", "BeginTx", "Exec", "ExecContext", "Prepare", "PrepareContext",
	}
)

// ── 锁 A：接口的方法集恰好是白名单 ──

func TestLockInterfacesAreExactlyTheAllowList(t *testing.T) {
	cases := []struct {
		name  string
		iface reflect.Type
		want  []string
		why   string
	}{
		{"agent.Reader", typeOf[agent.Reader](), allowReader,
			"agent 看数据的那一眼多一个方法，就多一条能碰数据的路"},
		{"agent.Proposer", typeOf[agent.Proposer](), allowProposer,
			"agent 的出口多一个动作，就多一条改世界状态的路"},
		{"agent.Model", typeOf[agent.Model](), allowModel,
			"多一个方法就是把不该有的能力（比如上传）递进了这一层"},
		{"agent.Applier", typeOf[agent.Applier](), allowApplier,
			"执行者多一个动作，就是多一条绕过 Change 这个口径的路"},
		{"store.Querier", typeOf[store.Querier](), []string{"Query", "QueryRow"},
			"只读的那一半一旦多出 Exec，ReadStore 就真的能写库了 —— 这是整套设计的地基"},
	}
	for _, c := range cases {
		got := methodNames(c.iface)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s 的方法集 = %v，白名单是 %v（%s）", c.name, got, c.want, c.why)
		}
	}
}

// ── 锁 C / D：具体类型的方法集 ──
//
// 与锁 A 的区别：A 锁的是**被递出去的接口**，这里锁的是**真实类型**。
// 多一个导出方法却不放进接口，一样能从 main.go 那边被调到 —— 那也算一条路。

func TestLockConcreteTypesAreExactlyTheAllowList(t *testing.T) {
	cases := []struct {
		name string
		typ  reflect.Type
		want []string
	}{
		{"*store.ReadStore", typeOf[*store.ReadStore](),
			[]string{"ListTags", "Questions", "ReviewHistory", "ReviewStats"}},
		{"*store.PendingStore", typeOf[*store.PendingStore](),
			[]string{"Count", "Decide", "Get", "List", "Propose"}},
		{"*agent.Service", typeOf[*agent.Service](), []string{"Ask"}},
		{"*agent.Pending", typeOf[*agent.Pending](),
			[]string{"Approve", "Count", "Discard", "List", "Propose"}},
		{"*apply.Applier", typeOf[*apply.Applier](), []string{"Apply"}},
	}
	for _, c := range cases {
		got := methodNames(c.typ)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s 的方法集 = %v，白名单是 %v", c.name, got, c.want)
		}
	}
}

// PendingStore 是唯一该有写能力的那个，而它只写一张表。
//
// 这条把「唯一」这件事本身钉下来：它**应当**碰得到 database/sql。
// 如果哪天它变得碰不到了（有人为了「更安全」把它也收窄成 Querier），
// 那说明有人动了这套设计 —— 那时这条会红，提醒他回来看这里为什么是这么定的。
func TestLockPendingStoreIsTheOnlyOneHoldingAWriter(t *testing.T) {
	reached := reachableFrom(typeOf[*store.PendingStore]())
	if !slices.ContainsFunc(reached, isDatabaseSQL) {
		t.Errorf("*store.PendingStore 碰不到 database/sql 了 —— 它靠什么写 pending_changes？"+
			"（碰到的东西：%v）", typeNames(reached))
	}
	if got := methodNames(typeOf[*store.PendingStore]()); len(got) != 5 {
		t.Errorf("*store.PendingStore 有 %d 个导出方法，想要 5 个", len(got))
	}
}

// ── 锁 B：沿字段走一遍，看它手里攥着什么 ──

func TestLockReadSideCannotReachADatabase(t *testing.T) {
	// 这三个出发点合起来就是「agent 这一层拿到的全部东西」。
	roots := []struct {
		name string
		typ  reflect.Type
		want []string
	}{
		{"*agent.Service", typeOf[*agent.Service](), allowServiceReach},
		{"*agent.Pending", typeOf[*agent.Pending](), allowPendingReach},
		{"*store.ReadStore", typeOf[*store.ReadStore](), allowReadStoreReach},
	}

	for _, r := range roots {
		reached := reachableFrom(r.typ)

		// 1. 碰不到 database/sql 的任何类型。
		if hit := slices.IndexFunc(reached, isDatabaseSQL); hit >= 0 {
			t.Errorf("%s 沿字段碰得到 %s —— 那是一条能写库的路。读侧要么拿 Querier，要么什么也别拿。",
				r.name, reached[hit])
		}

		// 2. 也碰不到任何一个名字就是「能写」的方法。
		for _, typ := range reached {
			for _, name := range methodNames(typ) {
				if slices.Contains(forbiddenMethods, name) {
					t.Errorf("%s 沿字段碰得到 %s.%s", r.name, typ, name)
				}
			}
		}

		// 3. 本模块里的每一个方法名都在白名单里。
		var extra []string
		for _, typ := range reached {
			if !isOurs(typ) {
				continue
			}
			for _, name := range methodNames(typ) {
				if !slices.Contains(r.want, name) {
					extra = append(extra, typ.String()+"."+name)
				}
			}
		}
		if len(extra) > 0 {
			slices.Sort(extra)
			t.Errorf("%s 这一侧多出了白名单上没有的方法：%v\n"+
				"（白名单：%v）\n"+
				"加方法之前先问一句：它会不会成为一条绕开待批准层的路？"+
				"想清楚了就把名字加进 safety_test.go 的 allow* 里。", r.name, extra, r.want)
		}
	}
}

// 这条保证上面那几条**不是空转的**：那套「碰得到 database/sql 就红」的检查，
// 在一个真的有写句柄的对象上必须真的报警。
//
// 没有它的话，万一遍历函数哪天写坏了（比如忘了走结构体字段），上面三条会全绿 ——
// 而全绿看起来和「一切正常」一模一样。一个只会空转的测试毫无价值。
func TestLockTheDatabaseCheckActuallyFires(t *testing.T) {
	reached := reachableFrom(typeOf[*store.PendingStore]())

	var hits []string
	for _, typ := range reached {
		if isDatabaseSQL(typ) {
			hits = append(hits, typ.String())
		}
	}
	if len(hits) == 0 {
		t.Fatalf("遍历函数连真的写句柄都找不到（走到的东西：%v）—— 那几条锁是空的", typeNames(reached))
	}
	// 而且撞上的里面就该有 *sql.DB —— 那正是「能写」这件事的载体，
	// 而它上面挂着 Exec。
	if !slices.Contains(hits, "*sql.DB") {
		t.Errorf("碰到的 database/sql 类型是 %v，没有 *sql.DB", hits)
	}
	if !slices.Contains(methodNames(reflect.TypeOf(&sql.DB{})), "Exec") {
		t.Error("*sql.DB 上找不到 Exec —— 这条测试的前提变了，得重新想它到底在证明什么")
	}
}

// ── 锁 E：源码守卫 ──
//
// 反射看不见未导出的方法，也看不见 import 与 SQL 字面量。这一条把那一半补上：
// 它读 internal/agent 这一棵子树的**源码**，逐个文件看四件事。
//
// 报错一律带上文件名与那一段原文，而不是一句「检查失败」—— 一份看不出来哪里错了的
// 守卫测试，下一次就会被人注释掉。

func TestLockAgentSourcesCannotReachAWriter(t *testing.T) {
	files := agentSourceFiles(t)

	writes := 0
	execs := map[string]int{}

	for _, file := range files {
		name := filepath.Base(file)
		pkg := filepath.Base(filepath.Dir(file))

		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("读 %s: %v", file, err)
		}
		f, err := parser.ParseFile(token.NewFileSet(), file, src, 0)
		if err != nil {
			t.Fatalf("解析 %s: %v", file, err)
		}

		for _, spec := range f.Imports {
			path := unquote(t, spec.Path.Value)

			// (a) 除了 store 那一层，谁都不许 import database/sql。
			//
			// 只有 store 需要它，而 store 里也只有 PendingStore 真的用到（ReadStore 拿到的是
			// Querier）。别的包 import 它，就等于那一边也能开库、也能写。
			if path == "database/sql" && pkg != "store" {
				t.Errorf("%s 里 import 了 database/sql —— 只有 store 那一层该碰它", name)
			}

			// (b) agent 这个包不许回头 import 拿句柄的那几层。
			//
			// 这条是**依赖方向**的守卫：agent ← store、agent ← apply（箭头指向被依赖者）。
			// 反过来了就成环，而环一旦成了，「agent 拿不到写句柄」这句话就不再成立。
			if pkg == "agent" && slices.Contains([]string{
				"database/sql",
				"questionbook/internal/agent/store",
				"questionbook/internal/agent/apply",
				"questionbook/internal/library",
			}, path) {
				t.Errorf("%s 里 import 了 %q —— agent 这个包不能回头 import 拿句柄的那几层", name, path)
			}
		}

		// (c) 整棵子树里的写 SQL，只许对 pending_changes 说。
		//
		// 这是「PendingStore 只碰一张表」那句话的直接证据，而且它覆盖将来可能加进来的
		// 任何一句 SQL —— 不管写在哪个方法、哪个文件里。
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s := unquote(t, lit.Value)
			if writeSQL.MatchString(s) {
				writes++
				if !strings.Contains(s, "pending_changes") {
					t.Errorf("%s 里出现了对着别处写的 SQL：%s", name, oneLine(s))
				}
			}
			return true
		})

		// (d) .Exec( 这样一个写动作的调用点，只该出现在一个文件里。
		if n := strings.Count(string(src), ".Exec("); n > 0 {
			execs[name] = n
		}
	}

	// 非空：一条写 SQL 都没扫到的话，(c) 是空转的。
	if writes == 0 {
		t.Error("整棵子树里一条写 SQL 都没扫到 —— (c) 是空转的（PendingStore 总得 INSERT 点什么）")
	}

	// (d) 非空且唯一：store.go 是那个有写能力的文件，别的文件里出现 .Exec( 就是新的写路。
	if execs["store.go"] == 0 {
		t.Errorf("store.go 里没有 .Exec( —— pending_changes 是靠什么写进去的？（扫到的是 %v）", execs)
	}
	for name, n := range execs {
		if name != "store.go" {
			t.Errorf("%s 里出现了 %d 处 .Exec( —— 有写能力的文件只该有一个（store.go）", name, n)
		}
	}
}

// writeSQL 认的是**真的写语句**，不是随便哪个含 update 的字符串。
//
// 认宽了会误伤（比如一句「整体替换」的提示词），认窄了会漏（比如一行写歪的
// 「UPDATE  tags  SET ...」）。这三个形状是 SQLite 里写数据仅有的三种起手式。
var writeSQL = regexp.MustCompile(`(?i)\b(insert\s+into|update\s+\S+\s+set|delete\s+from)\b`)

// agentSourceFiles 返回 internal/agent 这一棵子树里全部非测试的 Go 源文件。
//
// 用 runtime.Caller 定位而不是「internal/agent」这个相对路径：测试的工作目录是包目录，
// 一个写死的相对路径会在别的调用方式下静默扫到空集合 —— 而空集合会让上面几条全绿。
func agentSourceFiles(t *testing.T) []string {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller 拿不到本文件的路径")
	}
	root := filepath.Dir(thisFile)

	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s: %v", root, err)
	}
	if len(out) == 0 {
		t.Fatalf("%s 下一个源文件都没找到 —— 这条守卫是空的", root)
	}
	slices.Sort(out)
	return out
}

// ── 小工具 ──

// typeOf 拿到一个类型的反射句柄。接口要 Elem() 一下才是接口自己。
func typeOf[T any]() reflect.Type {
	t := reflect.TypeOf((*T)(nil)).Elem()
	if t.Kind() == reflect.Interface {
		return t
	}
	return t
}

// methodNames 返回一个类型的**导出**方法名，排好序。
//
// 只管导出方法：反射根本看不见未导出的那些，而它们也无法从包外被调用
// —— 包内那一半由 TestLockAgentSourcesCannotReachAWriter 兜。
func methodNames(t reflect.Type) []string {
	out := make([]string, 0, t.NumMethod())
	for i := 0; i < t.NumMethod(); i++ {
		out = append(out, t.Method(i).Name)
	}
	slices.Sort(out)
	return out
}

// reachableFrom 从 root 出发，沿**结构体字段**传递地走一遍，返回碰到过的全部类型。
//
// 三条规矩，都是为了让它回答的问题足够清楚：
//
//   - 只走字段，不走方法的参数与返回值。这里问的是「这个对象手里攥着什么」，
//     不是「它能造出什么类型来」。沿签名走会把 context.Context、*sql.Rows 这些
//     每次调用现造的东西也算成「持有的」，那种结论没有意义，还会把白名单撑得很大。
//   - 指针**先记下它自己再往里走**：*sql.DB 上有 Exec，而 sql.DB（值类型）的方法集
//     是空的。少记这一步，forbiddenMethods 那道网就会漏掉最要紧的那个。
//   - 接口**只记下、不往里走**：接口的方法名是要断言的东西，而它签名里的类型不是
//     这个对象持有的东西（agent.PendingStore 的 Get 返回 PendingChange，
//     不代表 agent.Pending 持有 *sql.DB）。
func reachableFrom(root reflect.Type) []reflect.Type {
	const maxDepth = 6

	seen := map[reflect.Type]bool{}
	var out []reflect.Type

	var walk func(t reflect.Type, depth int)
	walk = func(t reflect.Type, depth int) {
		if t == nil || depth > maxDepth || seen[t] {
			return
		}
		seen[t] = true
		out = append(out, t)

		switch t.Kind() {
		case reflect.Pointer:
			walk(t.Elem(), depth+1)
		case reflect.Struct:
			for i := 0; i < t.NumField(); i++ {
				walk(t.Field(i).Type, depth+1)
			}
		case reflect.Slice, reflect.Array, reflect.Chan:
			walk(t.Elem(), depth+1)
		case reflect.Map:
			walk(t.Key(), depth+1)
			walk(t.Elem(), depth+1)
		}
	}
	walk(root, 0)
	return out
}

// isOurs 报告这个类型是不是本项目的（好把 sync.Mutex、time.Time 这些滤掉）。
//
// **先剥掉指针**：指针类型没有 PkgPath（`reflect.TypeOf(&sql.DB{}).PkgPath()` 是空串），
// 不剥的话 *sql.DB、*tags.Service 这些全会被当成「不是本项目的」而跳过 ——
// 于是白名单那道网恰好漏掉最要紧的那些类型（它们的方法几乎全挂在指针接收者上）。
func isOurs(t reflect.Type) bool {
	t = namedOf(t)
	return t != nil && strings.HasPrefix(t.PkgPath(), "questionbook/")
}

// isDatabaseSQL 报告这个类型是不是 database/sql 里的。同样要先剥指针，理由同上
// —— 少了这一步，往 ReadStore 里塞一个 *sql.DB 字段是**测不出来**的。
func isDatabaseSQL(t reflect.Type) bool {
	t = namedOf(t)
	return t != nil && t.PkgPath() == "database/sql"
}

// namedOf 剥掉全部指针，拿到那个有名字的类型。
func namedOf(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func typeNames(ts []reflect.Type) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.String())
	}
	slices.Sort(out)
	return out
}

// unquote 解开一个 Go 字符串字面量（反引号或双引号都行）。
func unquote(t *testing.T, lit string) string {
	t.Helper()
	s, err := strconv.Unquote(lit)
	if err != nil {
		t.Fatalf("解不开字面量 %s: %v", lit, err)
	}
	return s
}

// oneLine 把一段 SQL 压成一行，好让报错里看得清是哪一句。
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
