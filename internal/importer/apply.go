package importer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"questionbook/internal/export"
	"questionbook/internal/library"
)

// ApplyPending 把暂存目录里那个**已经确认过**的导入换上去，返回这次落地的结果。
//
// **必须在 library.Open 之前调用** —— 库一旦打开，换它就成了另一件难得多的事（见包注释）。
//
// ── 为什么换的过程要写成「先挪旧、再挪新」并且处处能回滚 ──
//
// 这是一个把三四条路径依次挪开、再挪进来的动作。中途失败会留下「旧的已经挪走、新的还没
// 到位」的空档 —— 而那一刻紧接着 library.Open 就会跑，它见不到库文件会**新建一个空库**。
// 用户的错题就会这样悄无声息地变成一个空本子。所以：
//
//   - 每一步失败都把已经挪走的挪回来（改名是原子的，挪回来也一样）。
//   - 真回滚不了也要把这件事写进 last-import.json —— 启动时没有界面在场，
//     那份文件是用户唯一能知道「上次导入没成、旧库在哪儿」的地方。
func ApplyPending(root string, now func() time.Time) (Result, error) {
	staging := filepath.Join(root, stagingDirName)

	if _, err := os.Stat(staging); errors.Is(err, fs.ErrNotExist) {
		return Result{}, nil // 没有待落地的，正常
	}
	if _, err := os.Stat(filepath.Join(staging, readyName)); err != nil {
		// 没有就绪标记：上一次解到一半就没了（进程被杀、或者用户没点确认）。
		// 它是垃圾 —— 清掉，否则每次启动都会看到一个装不了的暂存目录。
		_ = os.RemoveAll(staging)
		return Result{}, nil
	}

	m, err := readManifest(staging)
	if err != nil {
		// 就绪标记在、清点却读不出来：这个暂存目录不值得再试了，清掉并留个话。
		_ = os.RemoveAll(staging)
		return record(root, Result{At: now(), Problem: "待落地的导入读不出来，已丢弃：" + err.Error()}), err
	}

	res := Result{At: now(), Questions: m.Questions, Images: m.Images}

	// 落地之前再核一次这个库打不打的开。
	//
	// Prepare 时已经验过一遍（打开 + 迁移 + VACUUM INTO），照理说这里是多余的。
	// 留着是因为「确认」与「重启」之间可能隔着好几天 —— 磁盘上那份东西此刻是不是
	// 还是好的，只有现在打开一次才知道。顺手也把它迁到本应用最新的模式。
	if err := checkStagedDB(filepath.Join(staging, export.DBName)); err != nil {
		res.Problem = err.Error()
		_ = os.RemoveAll(staging)
		return record(root, res), err
	}

	bak := filepath.Join(root, backupPrefix+now().Format("20060102-150405"))
	// 备份目录名精确到秒，同一秒里连着来两次就会撞上同一个名字。这时先把上一次那份
	// 删掉再建：反正规则本来就是「只留最近一份」，而留着一个目标已存在的目录会让
	// 下一步的改名在 Windows 上直接失败（那里不允许覆盖改名）。
	if _, err := os.Stat(bak); err == nil {
		if err := os.RemoveAll(bak); err != nil {
			res.Problem = "清掉同名的旧备份失败：" + err.Error()
			return record(root, res), err
		}
	}
	if err := os.MkdirAll(bak, 0o700); err != nil {
		res.Problem = "建备份目录失败：" + err.Error()
		return record(root, res), err
	}

	// ── 先挪旧 ──
	//
	// -wal / -shm **必须一起挪走**：它们是上一个库的旁挂文件，留在原地的话 SQLite
	// 会把上一份库的 WAL 应用到刚换上去的新库上 —— 那不是「没清干净」，是往新库里
	// 灌旧数据。
	moved, err := moveAside(root, bak, oldNames())
	if err != nil {
		res.Problem = "动不了现在这份库：" + err.Error()
		_ = rollback(root, bak, moved)
		_ = os.RemoveAll(staging)
		return record(root, res), err
	}
	if len(moved) > 0 {
		res.Backup = bak
	}

	// ── 再挪新 ──
	if _, err := moveAside(staging, root, newNames()); err != nil {
		res.Problem = "包里的东西挪不到位：" + err.Error()
		// 把旧的挪回来。挪不回来的话 Problem 里会再多一句 —— 那时用户只能自己去
		// bak 目录里手动救（路径就在 res.Backup 里）。
		if rbErr := rollback(root, bak, moved); rbErr != nil {
			res.Problem += "；把旧库挪回来也失败了（" + rbErr.Error() + "）"
		} else {
			res.Backup = ""
		}
		_ = os.RemoveAll(staging)
		return record(root, res), err
	}

	res.OK = true
	_ = os.RemoveAll(staging)
	// 只留最近这一份 bak：再导入一次就会再生成一份，攒着没有意义，而它是整份库那么大。
	clearOldBackups(root, filepath.Base(bak))

	return record(root, res), nil
}

// oldNames 是「正式目录里可能要被挪走的东西」：库（含它的旁挂文件）、图片目录、
// 以及那两份库外设置。
func oldNames() []string {
	names := []string{export.DBName, export.DBName + "-wal", export.DBName + "-shm", export.ImageDir}
	return append(names, export.SettingsNames...)
}

// newNames 是「暂存目录里可能要被挪进来的东西」。与 oldNames 少两个旁挂文件：
// 暂存里那份库是 VACUUM INTO 出来的单文件，本来就没有旁挂文件。
func newNames() []string {
	names := []string{export.DBName, export.ImageDir}
	return append(names, export.SettingsNames...)
}

// moveAside 把 names 里**存在于 from 的那些**逐个改名进 to，返回真的挪了哪些。
//
// 用改名而不是拷贝：同一个文件系统内改名是原子的，而且不占第二份空间 ——
// 图片可能有几百 MB，拷贝一次的代价与时间都不可接受。
func moveAside(from, to string, names []string) ([]string, error) {
	var moved []string
	for _, name := range names {
		src := filepath.Join(from, name)
		if _, err := os.Lstat(src); err != nil {
			continue // 不在就跳过：刚装上的机器没有 review.json，没有答案图时也没有 cards
		}
		if err := os.Rename(src, filepath.Join(to, name)); err != nil {
			return moved, fmt.Errorf("挪 %s: %w", name, err)
		}
		moved = append(moved, name)
	}
	return moved, nil
}

// rollback 把 moveAside 挪走的东西挪回 from。
func rollback(from, bak string, moved []string) error {
	var firstErr error
	for _, name := range moved {
		if err := os.Rename(filepath.Join(bak, name), filepath.Join(from, name)); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("挪回 %s: %w", name, err)
		}
	}
	return firstErr
}

// clearOldBackups 删掉除 keep 之外的备份目录。
//
// 只删挪完旧库**之后**才生成的自己那一份之外的东西，删失败不算错（它是垃圾，
// 不是这次导入的结果）。
func clearOldBackups(root, keep string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == keep || !strings.HasPrefix(e.Name(), backupPrefix) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, e.Name()))
	}
}

// checkStagedDB 打开暂存里那份库再关掉，确认它此刻还是好的。
//
// 打开顺带做了两件事：确认它是个真 SQLite、以及**模式不比本应用新**（library.Open 自己
// 会拒绝更新的库，那句「库的模式版本是 N，本程序只认到 M」是这条规矩惟一的出处）。
// 关掉之后把它旁边的 -wal/-shm 删掉 —— 那两个是这次检查产生的，跟着库挪进正式目录
// 就会变成「上一份库的旁挂文件」，正是要避免的东西。
func checkStagedDB(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("包里的库不见了: %w", err)
	}
	db, err := library.Open(path)
	if err != nil {
		return fmt.Errorf("包里的库打不开: %w", err)
	}
	if cerr := db.Close(); cerr != nil {
		return fmt.Errorf("关掉包里的库: %w", cerr)
	}
	removeSQLiteSidecars(path)
	return nil
}

// resultFile 是 last-import.json 的形状。
//
// 字段名要**稳**：这份文件是上次那个进程写给这次这个进程的，与 manifest 同一个道理。
type resultFile struct {
	OK        bool   `json:"ok"`
	Questions int    `json:"questions"`
	Images    int    `json:"images"`
	AtMS      int64  `json:"at_ms"`
	Backup    string `json:"backup,omitempty"`
	Problem   string `json:"problem,omitempty"`
}

// record 写下这次落地的结果并原样返回它。
//
// 写不下去不是「导入失败」—— 结果已经发生了，写不下来只说明界面之后看不到它。
// 所以这里的失败吞掉：让 ApplyPending 报错的原因应当只是导入本身。
func record(root string, res Result) Result {
	raw, err := json.Marshal(resultFile{
		OK:        res.OK,
		Questions: res.Questions,
		Images:    res.Images,
		AtMS:      res.At.UnixMilli(),
		Backup:    res.Backup,
		Problem:   res.Problem,
	})
	if err != nil {
		return res
	}
	_ = os.WriteFile(filepath.Join(root, resultName), raw, 0o600)
	return res
}

// readResult 读回上一次落地的结果。读不到（从没落地过、或者文件坏了）返回零值 ——
// 这一份是给人看的诊断，缺了它不该拦着任何东西。
func readResult(path string) Result {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Result{}
	}
	var f resultFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return Result{}
	}
	return Result{
		OK:        f.OK,
		Questions: f.Questions,
		Images:    f.Images,
		At:        time.UnixMilli(f.AtMS),
		Backup:    f.Backup,
		Problem:   f.Problem,
	}
}
