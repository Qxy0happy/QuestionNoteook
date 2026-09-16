package export_test

import (
	"archive/zip"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"questionbook/internal/export"
)

// stagedNames 列暂存目录里所有暂存相关的东西 —— 用来断言「没留下垃圾」。
func stagedNames(t *testing.T, dir string) []string {
	t.Helper()
	all, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读暂存目录: %v", err)
	}
	var out []string
	for _, f := range all {
		out = append(out, f.Name())
	}
	return out
}

// Stage 交出去的那个路径，必须真的指向一个能打开的导出包 —— 宿主拿到的就是它。
func TestStageLeavesAReadableBundle(t *testing.T) {
	e := newEnv(t)
	qh := e.saveImage(color.RGBA{R: 1, A: 255})
	ah := e.saveImage(color.RGBA{G: 2, A: 255})
	e.addQuestion(qh, ah)

	b, err := e.svc.Stage()
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}

	if _, err := os.Stat(b.Path); err != nil {
		t.Fatalf("包不在盘上: %v", err)
	}
	// Name 是宿主拿去当「下载」目录里文件名的那个 —— 它必须就是包的文件名，
	// 不一致的话用户看到的与 Go 说的就不是一个东西。
	if b.Name != filepath.Base(b.Path) {
		t.Errorf("Name = %q，落点的文件名是 %q，两者必须一致", b.Name, filepath.Base(b.Path))
	}
	if !strings.HasSuffix(b.Name, ".zip") {
		t.Errorf("Name = %q，不像一个 zip", b.Name)
	}

	// 清点数据要与包里实际的东西对得上：界面拿它说「导出了什么、多大」。
	if b.Questions != 1 || b.Images != 2 {
		t.Errorf("清点 = %d 题 / %d 图，想要 1 / 2", b.Questions, b.Images)
	}
	if b.Bytes <= 0 {
		t.Errorf("Bytes = %d，想要正数", b.Bytes)
	}

	zr, err := zip.OpenReader(b.Path)
	if err != nil {
		t.Fatalf("包打不开: %v", err)
	}
	defer zr.Close()

	var db, cards int
	for _, f := range zr.File {
		switch {
		case f.Name == export.DBName:
			db++
		case strings.HasPrefix(f.Name, export.ImageDir+"/"):
			cards++
		default:
			t.Errorf("包里出现了不该有的条目: %s", f.Name)
		}
	}
	if db != 1 {
		t.Errorf("包里有 %d 份库，想要 1", db)
	}
	if cards != 2 {
		t.Errorf("包里有 %d 张图，想要 2", cards)
	}
}

// 同一天再导出一次：文件名本来就相同（按日期命名），所以是**原地覆盖**，
// 而且不能留下临时的那半截（stage-*.zip）—— 宿主只该看见一个包。
func TestStageSameDayOverwritesAndLeavesNoTemp(t *testing.T) {
	e := newEnv(t)
	e.addQuestion(e.saveImage(color.RGBA{B: 3, A: 255}), "")

	first, err := e.svc.Stage()
	if err != nil {
		t.Fatalf("第一次 Stage: %v", err)
	}
	second, err := e.svc.Stage()
	if err != nil {
		t.Fatalf("第二次 Stage: %v", err)
	}

	if second.Path != first.Path {
		t.Errorf("同一天两次落在了不同路径上：%s / %s —— 文件名是按日期定的，应当一致",
			first.Path, second.Path)
	}
	if _, err := os.Stat(second.Path); err != nil {
		t.Errorf("第二次之后包不在: %v", err)
	}

	// 目录里只该有一个 zip：临时文件已经被改名，没有残骸。
	var zips []string
	for _, name := range stagedNames(t, e.stageDir) {
		if strings.HasSuffix(name, ".zip") {
			zips = append(zips, name)
		}
	}
	if len(zips) != 1 {
		t.Errorf("暂存目录里有 %d 个 zip（%v），想要 1 个", len(zips), zips)
	}
}

// 隔天再导出：上一天那个包要被清掉 —— 它是垃圾，而且宿主可能拿错文件。
func TestStageClearsABundleFromAnEarlierDay(t *testing.T) {
	e := newEnv(t)
	e.addQuestion(e.saveImage(color.RGBA{B: 4, A: 255}), "")

	// 两次导出指到**同一个**暂存目录，只是时钟不同 —— 模拟「昨天导过一次，
	// 今天又导了一次」。这也是唯一能测到 clearStaged 的办法。
	sameDir := func(day int) *export.Service {
		at := time.Date(2026, 9, day, 10, 0, 0, 0, time.Local)
		return export.NewService(e.store, bundleImages{store: e.images},
			export.WithTempDir(e.stageDir),
			export.WithNow(func() time.Time { return at }))
	}

	yesterday, err := sameDir(15).Stage()
	if err != nil {
		t.Fatalf("昨天的 Stage: %v", err)
	}
	today, err := sameDir(16).Stage()
	if err != nil {
		t.Fatalf("今天的 Stage: %v", err)
	}

	if _, err := os.Stat(yesterday.Path); !os.IsNotExist(err) {
		t.Errorf("昨天那个包还在（%s），它该被清掉", yesterday.Path)
	}
	if _, err := os.Stat(today.Path); err != nil {
		t.Errorf("今天那个包不在: %v", err)
	}
}

// 失败时**什么都不留** —— 半截的 zip 顶着备份的名字留在盘上比没有包更坏。
func TestStageLeavesNothingBehindWhenItFails(t *testing.T) {
	e := newEnv(t)
	// 库里引用了一张盘上没有的图：WriteBundle 会以 ErrImageMissing 失败。
	e.addQuestion("sha256:盘上没有这张图", "")

	dir := e.stageDir
	if _, err := e.svc.Stage(); err == nil {
		t.Fatal("缺图时 Stage 应当失败")
	} else if !strings.Contains(err.Error(), "盘上没有") && !strings.Contains(err.Error(), "sha256") {
		// 错误里至少要能看出是缺图，不然用户/我们无从下手。
		t.Logf("缺图这次的错误是: %v", err)
	}

	for _, name := range stagedNames(t, dir) {
		if strings.HasSuffix(name, ".zip") {
			t.Errorf("失败之后还留下了 %s", name)
		}
	}
}

// 文件名里的日期来自注入的时钟，所以它可测、也不会在午夜线上抖。
func TestStageNameUsesTheInjectedClock(t *testing.T) {
	e := newEnv(t)
	e.addQuestion(e.saveImage(color.RGBA{R: 9, A: 255}), "")

	fixed := time.Date(2026, 9, 16, 23, 59, 0, 0, time.Local)
	svc := export.NewService(e.store, bundleImages{store: e.images},
		export.WithTempDir(t.TempDir()),
		export.WithNow(func() time.Time { return fixed }))

	b, err := svc.Stage()
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if want := "错题本-2026-09-16.zip"; b.Name != want {
		t.Errorf("Name = %q，想要 %q", b.Name, want)
	}
}
