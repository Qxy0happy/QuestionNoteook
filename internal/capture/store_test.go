package capture_test

import (
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"questionbook/internal/capture"
)

// TestStore_SaveDedupesSameContent 是落盘那一半的核心断言：
// 同一张图落两次，目录里只有一个文件，文件名就是它的内容 hash。
func TestStore_SaveDedupesSameContent(t *testing.T) {
	dir := t.TempDir()
	store, err := capture.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	img := stripes(48, 32)

	first, err := store.Save(img)
	if err != nil {
		t.Fatalf("第一次 Save: %v", err)
	}
	second, err := store.Save(img)
	if err != nil {
		t.Fatalf("第二次 Save: %v", err)
	}
	if first != second {
		t.Errorf("两次落盘的 hash 不同：%s vs %s", first, second)
	}

	entries := readDirNames(t, dir)
	if len(entries) != 1 {
		t.Fatalf("目录里有 %d 个文件 %v，想要 1 个", len(entries), entries)
	}
	// 文件名 = 内容 hash（外加一个扩展名，具体叫什么由 Path 定义，这里不重复写死）。
	if want := filepath.Base(store.Path(first)); entries[0] != want {
		t.Errorf("文件名 = %q，想要 %q", entries[0], want)
	}
	if !strings.HasPrefix(entries[0], first.String()) {
		t.Errorf("文件名 %q 没有以内容 hash %s 开头", entries[0], first)
	}
}

// TestStore_KeepsDifferentContentApart 反过来保证去重没有去过头。
func TestStore_KeepsDifferentContentApart(t *testing.T) {
	dir := t.TempDir()
	store, err := capture.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	a, err := store.Save(stripes(48, 32))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	b, err := store.Save(stripes(49, 32)) // 只差一列
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if a == b {
		t.Fatalf("两张不同的图得到了同一个 hash %s", a)
	}
	if entries := readDirNames(t, dir); len(entries) != 2 {
		t.Fatalf("目录里有 %d 个文件 %v，想要 2 个", len(entries), entries)
	}
}

// TestStore_LoadRoundTripsPixels 确认落下去的东西还能原样读回来。
func TestStore_LoadRoundTripsPixels(t *testing.T) {
	store, err := capture.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	img := stripes(40, 24)

	h, err := store.Save(img)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	back, err := store.Load(h)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if back.Bounds() != img.Bounds() {
		t.Fatalf("读回来的尺寸 = %v，想要 %v", back.Bounds(), img.Bounds())
	}
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			// 走 RGBA() 比较：它给的是同一套 16 位预乘数值，跟具体色彩模型无关。
			r0, g0, b0, a0 := img.At(x, y).RGBA()
			r1, g1, b1, a1 := back.At(x, y).RGBA()
			if r0 != r1 || g0 != g1 || b0 != b1 || a0 != a1 {
				t.Fatalf("像素 (%d,%d) 读回来变成了 %v，原本是 %v",
					x, y, back.At(x, y), img.At(x, y))
			}
		}
	}
}

// TestStore_RejectsEmptyInput 覆盖建库与落盘的边界。
func TestStore_RejectsEmptyInput(t *testing.T) {
	t.Run("空目录名", func(t *testing.T) {
		if _, err := capture.NewStore(""); err == nil {
			t.Fatal("NewStore(\"\") 没报错")
		}
	})

	t.Run("空图", func(t *testing.T) {
		store, err := capture.NewStore(t.TempDir())
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		if _, err := store.Save(image.NewRGBA(image.Rect(0, 0, 0, 0))); err == nil {
			t.Fatal("Save 空图没报错")
		}
	})

	t.Run("目录不存在就建", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "images", "cards")
		if _, err := capture.NewStore(dir); err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("目录没建出来：%v", err)
		}
	})
}

// TestCapture_DedupesAcrossRuns 把两半接起来跑一遍真实流程：
// 同一道题拍两次 —— 两次拉正的输入逐像素相同 —— 落盘只该有一个文件。
func TestCapture_DedupesAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	store, err := capture.NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	src := makePerspectiveChecker(quadSamplesSize, quadSamples)

	var hashes []capture.Hash
	for i := 0; i < 2; i++ {
		card, err := capture.Rectify(src, quadSamples)
		if err != nil {
			t.Fatalf("第 %d 次 Rectify: %v", i+1, err)
		}
		h, err := store.Save(card)
		if err != nil {
			t.Fatalf("第 %d 次 Save: %v", i+1, err)
		}
		hashes = append(hashes, h)
	}
	if hashes[0] != hashes[1] {
		t.Errorf("两次拍摄落了两个 hash：%s vs %s", hashes[0], hashes[1])
	}
	if entries := readDirNames(t, dir); len(entries) != 1 {
		t.Fatalf("目录里有 %d 个文件 %v，想要 1 个", len(entries), entries)
	}
}

// readDirNames 读出目录里的文件名，顺便把残留的临时文件暴露出来。
func readDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
