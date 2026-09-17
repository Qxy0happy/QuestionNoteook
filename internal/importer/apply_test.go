package importer_test

// 这个文件测「换库那一半」：把暂存目录里那份换上正式目录，旧库改名留一手。
//
// 这里最要紧的一条是**不能把用户已有的错题弄丢**：换库是几步改名，中途失败会留下
// 「旧的挪走了、新的没到位」的空档，而紧接着 library.Open 见不到库文件就会新建一个空库。
// 所以每个子测试都在断言同一件事 —— 要么换成，要么还在。
//
// 有一处**没有测试**：换到一半失败时的回滚（rollback）。要触发它得让几步 os.Rename
// 里的某一步失败，而「目标被占住」这类办法在这套流程里走不通（每个目标都在前一步被
// 让开了）。这条路径因此只有代码里的顺序与回滚兜着，没有测试钉 —— 与其写一条绕开
// 真实失败点、看着很绿的测试，不如把这件事记在这儿。

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"questionbook/internal/capture"
	"questionbook/internal/export"
	"questionbook/internal/importer"
	"questionbook/internal/library"
)

// applyAt 关掉库、在指定时刻落地、再开回来（ApplyPending 要在没有连接的时候跑）。
func (e *env) applyAt(at time.Time) importer.Result {
	e.t.Helper()
	e.close()
	res, err := importer.ApplyPending(e.root, func() time.Time { return at })
	if err != nil {
		e.t.Fatalf("ApplyPending: %v", err)
	}
	e.reopen()
	return res
}

// backupDirs 列出数据根目录下的备份目录。
func backupDirs(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("列数据根目录: %v", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && len(e.Name()) > 4 && e.Name()[:4] == backupPrefixOf() {
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out
}

// backupPrefixOf 是备份目录的前缀。写在测试这边而不是导出它：那是实现的内部命名，
// 导出成公开 API 只为了让测试少写四个字符不划算。
func backupPrefixOf() string { return "bak-" }

// 换手机之后旧库还在：一份完整的旧库（库 + 图片）被挪进备份目录，而且它自己打得开。
func TestApplyKeepsOldLibraryInBackup(t *testing.T) {
	src := newEnv(t)
	src.addQuestion(src.saveImage(rgba(255, 0, 0)), "")
	zipPath := src.bundleZip()

	// 这台机器上本来有一道题（换手机那台是空的，这里是「重装」那种情形）。
	dst := newEnvIn(t, t.TempDir())
	oldQuestion := dst.addQuestion(dst.saveImage(rgba(0, 255, 0)), "")
	oldHash := oldQuestion.QuestionHash

	if _, err := dst.pickAndPrepare(zipPath); err != nil {
		t.Fatalf("PickAndPrepare: %v", err)
	}
	dst.confirm()

	if res := dst.applyAt(fixedNow()); !res.OK {
		t.Fatalf("落地失败：%+v", res)
	}

	dirs := backupDirs(t, dst.root)
	if len(dirs) != 1 {
		t.Fatalf("备份目录有 %d 个 %v，想要 1 个", len(dirs), dirs)
	}
	bak := filepath.Join(dst.root, dirs[0])

	// 旧库那一整份都在备份里：库文件、它的图片目录。这是「旧数据还在」的唯一证据 ——
	// 只说「有个目录叫 bak」是不够的。
	if _, err := os.Stat(filepath.Join(bak, export.DBName)); err != nil {
		t.Errorf("备份里没有库文件: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bak, export.ImageDir, oldHash+".png")); err != nil {
		t.Errorf("备份里没有旧库的那张图: %v", err)
	}

	// 而且那份旧库**打得开、读得出原来那道题** —— 一份打不开的备份等于没有备份。
	bakDB, err := library.Open(filepath.Join(bak, export.DBName))
	if err != nil {
		t.Fatalf("打开备份里的库: %v", err)
	}
	defer bakDB.Close()
	got, err := bakDB.GetQuestion(oldQuestion.ID)
	if err != nil {
		t.Fatalf("从备份里读那道题: %v", err)
	}
	if got.QuestionHash != oldHash {
		t.Errorf("备份里那道题的 hash = %q，想要 %q", got.QuestionHash, oldHash)
	}

	// 换上来的是包里那道题。
	now, err := dst.store.ListQuestions()
	if err != nil {
		t.Fatalf("列当前错题: %v", err)
	}
	if len(now) != 1 || now[0].QuestionHash == oldHash {
		t.Errorf("换上来之后库里的题 = %+v，想要包里那道", now)
	}
}

// 上一次解到一半留下的暂存目录（有清点、没有就绪标记）不该被落地，而且会被清掉。
//
// 这条是「用户没点确认就退出应用」那条路：重启之后如果它照样落地，那就是**没有经过
// 用户点头就换掉了他的库**。
func TestApplyIgnoresUnconfirmedStaging(t *testing.T) {
	src := newEnv(t)
	src.addQuestion(src.saveImage(rgba(255, 0, 0)), "")
	zipPath := src.bundleZip()

	dst := newEnvIn(t, t.TempDir())
	mine := dst.addQuestion(dst.saveImage(rgba(0, 255, 0)), "")
	before := tableCounts(t, dst.store)

	if _, err := dst.pickAndPrepare(zipPath); err != nil {
		t.Fatalf("PickAndPrepare: %v", err)
	}
	// 故意**不** confirm。

	res := dst.applyAt(fixedNow())
	if res.OK || res.Questions != 0 {
		t.Errorf("没确认的导入竟然落地了：%+v", res)
	}

	if got := tableCounts(t, dst.store); len(got) == 0 {
		t.Fatal("换完库之后一张表都没有 —— 库被弄没了")
	}
	qs, err := dst.store.ListQuestions()
	if err != nil {
		t.Fatalf("列错题: %v", err)
	}
	if len(qs) != 1 || qs[0].ID != mine.ID {
		t.Errorf("自己那道题不见了：%+v", qs)
	}
	if after := tableCounts(t, dst.store); !sameExceptBackup(before, after) {
		t.Errorf("没确认却也动了数据：前 %v，后 %v", before, after)
	}
	// 垃圾暂存目录该被清掉，否则每次启动都会看到一个装不了的东西。
	if _, err := os.Stat(filepath.Join(dst.root, "import-pending")); !errors.Is(err, os.ErrNotExist) {
		t.Error("没确认的暂存目录没被清掉")
	}
	if dirs := backupDirs(t, dst.root); len(dirs) != 0 {
		t.Errorf("没落地却留下了备份目录 %v", dirs)
	}
}

// 旧库的 -wal / -shm 必须一起挪走。
//
// 留着的话，SQLite 会把**上一个库**的 WAL 应用到刚换上去的新库上 —— 那不是「没清干净」，
// 是往新库里灌旧数据。所以这里手工放一个假的旁挂文件，断言它跟着库进了备份目录。
func TestApplyMovesWALSidecarsIntoBackup(t *testing.T) {
	src := newEnv(t)
	src.addQuestion(src.saveImage(rgba(255, 0, 0)), "")
	zipPath := src.bundleZip()

	dst := newEnvIn(t, t.TempDir())
	dst.addQuestion(dst.saveImage(rgba(0, 255, 0)), "")
	if _, err := dst.pickAndPrepare(zipPath); err != nil {
		t.Fatalf("PickAndPrepare: %v", err)
	}
	dst.confirm()

	// 库关掉之后手工摆一个旁挂文件（真跑起来时它是上一次运行留下的）。
	dst.close()
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(filepath.Join(dst.root, export.DBName+suffix), []byte("上一个库的旁挂文件"), 0o600); err != nil {
			t.Fatalf("摆 %s: %v", suffix, err)
		}
	}

	res, err := importer.ApplyPending(dst.root, fixedNow)
	if err != nil {
		t.Fatalf("ApplyPending: %v", err)
	}
	if !res.OK {
		t.Fatalf("落地失败：%+v", res)
	}

	// 判据必须在**重新打开库之前**看：一旦把新库开起来（WAL 模式），SQLite 自然会
	// 给它新建一对旁挂文件 —— 那是新库的，不是旧库留下的。这里要看的是旧的那一对
	// 有没有被让开。
	dirs := backupDirs(t, dst.root)
	if len(dirs) != 1 {
		t.Fatalf("备份目录有 %d 个，想要 1 个", len(dirs))
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(dst.root, export.DBName+suffix)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("正式目录里还留着 %s —— 它会被应用到新库上", export.DBName+suffix)
		}
		if _, err := os.Stat(filepath.Join(dst.root, dirs[0], export.DBName+suffix)); err != nil {
			t.Errorf("备份里该有 %s: %v", export.DBName+suffix, err)
		}
	}

	// 开起来之后新库是好的，而且里面是包里那道题（旧库的旁挂文件没被灌进来）。
	dst.reopen()
	if _, err := dst.store.ListQuestions(); err != nil {
		t.Fatalf("换库之后库打不开: %v", err)
	}
}

// 就绪标记在、库却不在的暂存目录：**必须拒绝**，而不是换上一个空库。
//
// 这是这一整套里最危险的一种坏法：`library.Open` 见不到库文件会新建一个空的，
// 于是用户攒的错题会悄无声息地变成一个空本子。判据是正式数据与备份目录都没动。
func TestApplyRefusesStagingWithoutLibrary(t *testing.T) {
	dst := newEnvIn(t, t.TempDir())
	mine := dst.addQuestion(dst.saveImage(rgba(0, 255, 0)), "")
	before := tableCounts(t, dst.store)

	staging := filepath.Join(dst.root, "import-pending")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatalf("建暂存目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staging, "manifest.json"), []byte(`{"questions":9,"images":9}`), 0o600); err != nil {
		t.Fatalf("写清点: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staging, "ready"), []byte("2026-09-17T21:30:00Z"), 0o600); err != nil {
		t.Fatalf("写就绪标记: %v", err)
	}

	// 这一条要看错误本身，所以不走 applyAt（它把错误直接 Fatal 掉了）。
	dst.close()
	res, err := importer.ApplyPending(dst.root, fixedNow)
	dst.reopen()
	if err == nil {
		t.Fatalf("缺库的暂存目录竟然装上了：%+v", res)
	}
	if res.OK || res.Problem == "" {
		t.Errorf("结果该是失败且写明原因：%+v", res)
	}
	if dirs := backupDirs(t, dst.root); len(dirs) != 0 {
		t.Errorf("拒绝了却动了旧库（留下了备份 %v）", dirs)
	}
	qs, err := dst.store.ListQuestions()
	if err != nil {
		t.Fatalf("列错题: %v", err)
	}
	if len(qs) != 1 || qs[0].ID != mine.ID {
		t.Errorf("自己那道题不见了：%+v", qs)
	}
	if after := tableCounts(t, dst.store); !sameExceptBackup(before, after) {
		t.Errorf("拒绝了却动了数据：前 %v，后 %v", before, after)
	}
}

// 落地结果要写下来给界面看（启动那一刻没有界面在场，这份文件是唯一能说话的）。
func TestApplyRecordsResult(t *testing.T) {
	src := newEnv(t)
	src.addQuestion(src.saveImage(rgba(255, 0, 0)), "")
	zipPath := src.bundleZip()

	dst := newEnvIn(t, t.TempDir())
	if _, err := dst.pickAndPrepare(zipPath); err != nil {
		t.Fatalf("PickAndPrepare: %v", err)
	}
	dst.confirm()
	res := dst.applyAt(fixedNow())

	got := dst.svc.Last()
	if !got.OK || got.Questions != res.Questions || got.Images != res.Images {
		t.Errorf("读回来的结果是 %+v，想要 %+v", got, res)
	}
	if !got.At.Equal(fixedNow()) {
		t.Errorf("读回来的时刻是 %v，想要 %v", got.At, fixedNow())
	}
	if got.Backup == "" {
		t.Error("结果里该写明旧库被挪到哪儿了")
	}
	if _, err := os.Stat(got.Backup); err != nil {
		t.Errorf("结果里那个备份路径不存在: %v", err)
	}
}

// 只留最近一份备份：它是整份库那么大，反复导入不该把它们攒起来。
func TestApplyKeepsOnlyLatestBackup(t *testing.T) {
	src := newEnv(t)
	src.addQuestion(src.saveImage(rgba(255, 0, 0)), "")
	first := src.bundleZip()
	src.addQuestion(src.saveImage(rgba(0, 0, 255)), "")
	second := src.bundleZip()

	dst := newEnvIn(t, t.TempDir())
	dst.addQuestion(dst.saveImage(rgba(0, 255, 0)), "")

	// 两次导入，各自一个不同的时刻（备份目录名带时间戳）。
	for _, step := range []struct {
		zip string
		at  time.Time
	}{
		{first, time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)},
		{second, time.Date(2026, 9, 17, 11, 0, 0, 0, time.UTC)},
	} {
		dst.picker.path = step.zip
		if _, err := dst.svc.PickAndPrepare(); err != nil {
			t.Fatalf("PickAndPrepare: %v", err)
		}
		if err := dst.svc.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if res := dst.applyAt(step.at); !res.OK {
			t.Fatalf("落地失败：%+v", res)
		}
	}

	dirs := backupDirs(t, dst.root)
	if len(dirs) != 1 {
		t.Fatalf("备份目录有 %d 个 %v，想要只剩最近那一个", len(dirs), dirs)
	}
	if want := backupPrefixOf() + "20260917-110000"; dirs[0] != want {
		t.Errorf("留下的是 %s，想要 %s（最近那次）", dirs[0], want)
	}
}

// 同一个时刻（同一秒内连着来两次）也不能撞：备份目录重名时第二次要能照常换。
func TestApplyTwiceInTheSameSecond(t *testing.T) {
	src := newEnv(t)
	src.addQuestion(src.saveImage(rgba(255, 0, 0)), "")
	first := src.bundleZip()
	second := src.bundleZip()

	dst := newEnvIn(t, t.TempDir())
	for _, zipPath := range []string{first, second} {
		dst.picker.path = zipPath
		if _, err := dst.svc.PickAndPrepare(); err != nil {
			t.Fatalf("PickAndPrepare: %v", err)
		}
		if err := dst.svc.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if res := dst.applyAt(fixedNow()); !res.OK {
			t.Fatalf("同一秒里第二次落地失败：%+v", res)
		}
	}
	if dirs := backupDirs(t, dst.root); len(dirs) != 1 {
		t.Errorf("备份目录有 %d 个 %v，想要 1 个", len(dirs), dirs)
	}
}

// 空库也能装：包里有库、没有图时照样落地（cards 目录在暂存里是空的）。
func TestApplyEmptyLibrary(t *testing.T) {
	src := newEnv(t)
	zipPath := src.bundleZip() // 一道题都没有

	dst := newEnvIn(t, t.TempDir())
	dst.addQuestion(dst.saveImage(rgba(0, 255, 0)), "")
	if _, err := dst.pickAndPrepare(zipPath); err != nil {
		t.Fatalf("PickAndPrepare: %v", err)
	}
	dst.confirm()
	if res := dst.applyAt(fixedNow()); !res.OK || res.Questions != 0 {
		t.Fatalf("空包落地失败：%+v", res)
	}

	qs, err := dst.store.ListQuestions()
	if err != nil {
		t.Fatalf("列错题: %v", err)
	}
	if len(qs) != 0 {
		t.Errorf("装完空包之后库里还有 %d 道题", len(qs))
	}
	// 图片目录还在（只是空的）—— 采集那边随时要往里写。
	if _, err := os.Stat(filepath.Join(dst.root, export.ImageDir)); err != nil {
		t.Errorf("落地之后图片目录不见了: %v", err)
	}
}

// 装回来的图能真的读出来（不是只搬了个名字）：拿采集那一侧读盘的路径核一遍。
func TestImportedImageIsReadable(t *testing.T) {
	src := newEnv(t)
	hash := src.saveImage(rgba(255, 0, 0))
	src.addQuestion(hash, "")
	zipPath := src.bundleZip()

	dst := newEnvIn(t, t.TempDir())
	if _, err := dst.pickAndPrepare(zipPath); err != nil {
		t.Fatalf("PickAndPrepare: %v", err)
	}
	dst.confirm()
	if res := dst.applyAt(fixedNow()); !res.OK {
		t.Fatalf("落地失败：%+v", res)
	}

	img, err := dst.images.Load(capture.Hash(hash))
	if err != nil {
		t.Fatalf("读回导入的图: %v", err)
	}
	if got := img.Bounds().Dx(); got != 4 {
		t.Errorf("读回来的图宽 %d，想要 4", got)
	}
}

// sameExceptBackup 比较两份行数签名。备份目录里那份库不在这份签名里（它不在正式库里），
// 所以这里就是普通的相等比较 —— 留着这个名字是为了让调用处读起来说清在比什么。
func sameExceptBackup(before, after map[string]int) bool {
	if len(before) != len(after) {
		return false
	}
	for k, v := range before {
		if after[k] != v {
			return false
		}
	}
	return true
}
