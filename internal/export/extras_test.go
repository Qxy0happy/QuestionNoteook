package export_test

// 包外那两份设置的进出（WithBundleExtras）。
//
// 它们与库、图不一样：**不在也不算错**。刚装上的机器还没有 review.json —— 那不是
// 「这个包缺了东西」，而是「这台机器还没配过」。把这两种情形分开报，是这几条测试的要点。

import (
	"bytes"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"questionbook/internal/export"
)

// withExtras 在既有环境上再挂几份库外文件（它们与库同目录，与真机上一样）。
func withExtras(t *testing.T, e *env, names ...string) []string {
	t.Helper()
	paths := make([]string, 0, len(names))
	for _, name := range names {
		paths = append(paths, filepath.Join(filepath.Dir(e.dbPath), name))
	}
	e.svc = export.NewService(e.store, bundleImages{store: e.images},
		export.WithTempDir(e.stageDir), export.WithBundleExtras(paths...))
	return paths
}

// 配过的设置跟着进包，内容原样。
func TestBundleCarriesSettingsFiles(t *testing.T) {
	e := newEnv(t)
	hash := e.saveImage(color.RGBA{R: 255, A: 255})
	e.addQuestion(hash, "")
	paths := withExtras(t, e, "review.json", "digest.json")

	contents := []string{
		`{"fuzz":true,"retention":0.95,"exam_date":"2026-12-19"}`,
		`{"enabled":true,"hour":21,"minute":0}`,
	}
	for i, path := range paths {
		if err := os.WriteFile(path, []byte(contents[i]), 0o600); err != nil {
			t.Fatalf("写 %s: %v", path, err)
		}
	}

	b := e.bundle()

	b.assertEntries(
		"library.db",
		export.ImageDir+"/"+hash+".png",
		"review.json",
		"digest.json",
	)
	// 条目名取的是 base name（包与私有目录同名那条规矩），内容原样。
	for i, name := range []string{"review.json", "digest.json"} {
		if !bytes.Equal(b.entries[name], []byte(contents[i])) {
			t.Errorf("包里的 %s 是 %q，想要 %q", name, b.entries[name], contents[i])
		}
	}
}

// 没配过的（文件不在）不进制、也不算错 —— 这是与「库里引用的图缺了」完全不同的情形。
func TestBundleSkipsMissingSettings(t *testing.T) {
	e := newEnv(t)
	hash := e.saveImage(color.RGBA{R: 255, A: 255})
	e.addQuestion(hash, "")
	withExtras(t, e, "review.json", "digest.json") // 两个都没建出来

	b := e.bundle()

	b.assertEntries("library.db", export.ImageDir+"/"+hash+".png")
	if b.result != (export.Result{Questions: 1, Images: 1, Bytes: int64(len(b.raw))}) {
		t.Errorf("Result = %+v，想要 {1 1 %d}", b.result, len(b.raw))
	}
}

// 那份设置的落点是个目录（或者别的非普通文件）时明确报错 —— 因为它会被当成
// 「配过了」而读出一个空包，那种包看着像备份、装回去却少东西。
func TestBundleRejectsNonRegularSettings(t *testing.T) {
	e := newEnv(t)
	paths := withExtras(t, e, "review.json")
	if err := os.Mkdir(paths[0], 0o755); err != nil {
		t.Fatalf("建同名目录: %v", err)
	}

	var buf bytes.Buffer
	_, err := e.svc.WriteBundle(&buf)
	if err == nil {
		t.Fatal("那份设置是个目录，WriteBundle 该失败")
	}
	if !strings.Contains(err.Error(), "review.json") {
		t.Errorf("错误里该点名是哪一个：%v", err)
	}
}
