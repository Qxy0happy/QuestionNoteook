package export_test

// 这个文件的断言分两种，别把它们混起来：
//
//   - **包本身**：条目名、条目内容、Result 的清点。
//   - **包与库的关系**：两条**双向**断言 —— 库里引用的每一张图都在包里；包里没有库不引用的图。
//     两条不对称（一条管漏、一条管多），必须分开测，见 TestBundleHasEveryReferencedImage
//     与 TestBundleHasNoOrphanImages。
//
// 「库里引用的 hash」一律从**包里那份库**里读出来，不是从内存里那个库 —— 这样
// 「包里的图和包里的库是同一时刻的」也一并被测到。

import (
	"archive/zip"
	"bytes"
	"errors"
	"image"
	"image/color"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"questionbook/internal/capture"
	"questionbook/internal/export"
	"questionbook/internal/library"
)

// bundleImages 把采集那侧的图片文件层接到导出的 ImageFiles 上。
//
// 与 main.go 里要补的那层壳是同一个形状：capture.Store.Path 收的是具名类型
// capture.Hash，而导出要的是 string（与 library.ImageStore 那条边界同一个理由）。
type bundleImages struct{ store *capture.Store }

func (i bundleImages) PathByHash(hash string) string { return i.store.Path(capture.Hash(hash)) }

// env 是一次导出测试的环境：一个临时目录里的库、一个真的图片文件层、接好的导出服务。
//
// 图片走真的 capture.Store 而不是手拼文件名：内容 hash、<hash>.png 这个命名、
// 盘上真实存在的那些字节，都该是导出要面对的那一份，测试里不该另立一套。
type env struct {
	t      *testing.T
	dbPath string // 库文件在盘上的位置（TestNaiveCopyOfDBFileLosesRecentWrite 要看它）
	store  *library.Store
	images *capture.Store
	svc    *export.Service
}

func newEnv(t *testing.T) *env {
	t.Helper()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "library.db")
	store, err := library.Open(dbPath)
	if err != nil {
		t.Fatalf("开库: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	images, err := capture.NewStore(filepath.Join(dir, "cards"))
	if err != nil {
		t.Fatalf("开图片目录: %v", err)
	}

	// 快照的落点也钉在临时目录里，免得往系统临时目录里拉屎。
	svc := export.NewService(store, bundleImages{store: images}, export.WithTempDir(t.TempDir()))
	return &env{t: t, dbPath: dbPath, store: store, images: images, svc: svc}
}

// saveImage 造一张纯色图落进图片目录，返回它的内容 hash。
func (e *env) saveImage(c color.RGBA) string {
	e.t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	h, err := e.images.Save(img)
	if err != nil {
		e.t.Fatalf("存图: %v", err)
	}
	return h.String()
}

// addQuestion 往库里落一道错题，失败即终止。
func (e *env) addQuestion(questionHash, answerHash string) library.Question {
	e.t.Helper()
	q, err := e.store.AddQuestion(library.Question{
		QuestionHash: questionHash,
		AnswerHash:   answerHash,
	})
	if err != nil {
		e.t.Fatalf("落库错题: %v", err)
	}
	return q
}

// bundle 是拆开的导出包。
type bundle struct {
	t       *testing.T
	raw     []byte
	entries map[string][]byte
	order   []string // 条目在包里出现的顺序
	result  export.Result
}

// bundle 导出一次并把包拆成「条目名 → 内容」。
func (e *env) bundle() bundle {
	e.t.Helper()

	var buf bytes.Buffer
	res, err := e.svc.WriteBundle(&buf)
	if err != nil {
		e.t.Fatalf("WriteBundle: %v", err)
	}

	raw := buf.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		e.t.Fatalf("读回刚打出的包: %v", err)
	}
	b := bundle{t: e.t, raw: raw, entries: map[string][]byte{}, result: res}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			e.t.Fatalf("打开条目 %s: %v", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			e.t.Fatalf("读条目 %s: %v", f.Name, err)
		}
		b.entries[f.Name] = data
		b.order = append(b.order, f.Name)
	}
	return b
}

// assertEntries 断言包里的条目正好是 names 这些（两侧都断：不多、不少）。
func (b bundle) assertEntries(names ...string) {
	b.t.Helper()

	want := append([]string(nil), names...)
	sort.Strings(want)
	got := append([]string(nil), b.order...)
	sort.Strings(got)

	if len(got) != len(want) {
		b.t.Fatalf("包里有 %d 个条目 %v，想要 %d 个 %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			b.t.Fatalf("第 %d 个条目 = %q，想要 %q（全量：%v）", i, got[i], want[i], got)
		}
	}
}

// imageNames 返回包里 cards/ 下的条目名。
func (b bundle) imageNames() []string {
	b.t.Helper()
	var names []string
	for _, name := range b.order {
		if strings.HasPrefix(name, export.ImageDir+"/") {
			names = append(names, name)
		}
	}
	return names
}

// hashOfEntry 从条目名里剥出内容 hash。
func hashOfEntry(b bundle, name string) string {
	b.t.Helper()
	h := strings.TrimPrefix(name, export.ImageDir+"/")
	if h == name || !strings.HasSuffix(h, ".png") {
		b.t.Fatalf("条目名 %q 不是 <hash>.png 这个形状", name)
	}
	return strings.TrimSuffix(h, ".png")
}

// openDB 把包里的库解到一个**干净**目录里打开 —— 那个目录里只有 library.db，
// 没有任何 -wal / -shm 陪着它（包里本来也没有）。
//
// 打开它本身就是一条断言：模式版本若在打包时丢了，Open 会从头重跑迁移、
// 撞上已经存在的表而报错，这里就 Fatal 了。
func (b bundle) openDB() *library.Store {
	b.t.Helper()

	data, ok := b.entries["library.db"]
	if !ok {
		b.t.Fatalf("包里没有 library.db，只有 %v", b.order)
	}
	path := filepath.Join(b.t.TempDir(), "library.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		b.t.Fatalf("解出库文件: %v", err)
	}
	s, err := library.Open(path)
	if err != nil {
		b.t.Fatalf("打开包里的库: %v", err)
	}
	b.t.Cleanup(func() { s.Close() })
	return s
}

// listQuestions 列出包里的库中全部错题，失败即终止。
func listQuestions(t *testing.T, s *library.Store) []library.Question {
	t.Helper()
	qs, err := s.ListQuestions()
	if err != nil {
		t.Fatalf("列出包里的错题: %v", err)
	}
	return qs
}

// referencedHashes 返回一组错题引用到的全部图片 hash（题图列与答案图列都算），
// 空串不算 —— 那是「还没拍答案」的占位。
func referencedHashes(qs []library.Question) map[string]bool {
	set := map[string]bool{}
	for _, q := range qs {
		for _, h := range []string{q.QuestionHash, q.AnswerHash} {
			if h != "" {
				set[h] = true
			}
		}
	}
	return set
}

// 库与图都在包里，而且只有它们：库在根上，图在 cards/ 下按 hash 命名。
func TestWriteBundlePutsLibraryAndImagesIn(t *testing.T) {
	e := newEnv(t)
	question := e.saveImage(color.RGBA{R: 255, A: 255})
	answer := e.saveImage(color.RGBA{G: 255, A: 255})
	other := e.saveImage(color.RGBA{B: 255, A: 255})
	first := e.addQuestion(question, answer)
	second := e.addQuestion(other, "") // 还没拍答案

	b := e.bundle()

	b.assertEntries(
		"library.db",
		export.ImageDir+"/"+question+".png",
		export.ImageDir+"/"+answer+".png",
		export.ImageDir+"/"+other+".png",
	)
	// 库排在头一个不是巧合：解包那一侧先落地库，再按库去核图。
	if len(b.order) == 0 || b.order[0] != "library.db" {
		t.Errorf("第一个条目 = %v，想要 library.db", b.order)
	}

	if b.result != (export.Result{Questions: 2, Images: 3, Bytes: int64(len(b.raw))}) {
		t.Errorf("Result = %+v，想要 {2 3 %d}", b.result, len(b.raw))
	}

	// 包里的库**是同一份库**：不只是 questions 表，模式与别的表也一起过来了。
	db := b.openDB()
	got := listQuestions(t, db)
	if len(got) != 2 {
		t.Fatalf("包里的库有 %d 道错题，想要 2 道", len(got))
	}
	if got[0].ID != second.ID || got[0].QuestionHash != other || got[0].AnswerHash != "" {
		t.Errorf("包里的库第一条 = %+v，想要 %+v", got[0], second)
	}
	if got[1].ID != first.ID || got[1].QuestionHash != question || got[1].AnswerHash != answer {
		t.Errorf("包里的库第二条 = %+v，想要 %+v", got[1], first)
	}

	var tags int
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM tags`).Scan(&tags); err != nil {
		t.Fatalf("数包里的标签: %v", err)
	}
	if tags != 4 {
		t.Errorf("包里的库有 %d 个标签，想要 4 个预置学科", tags)
	}
}

// 方向一：库里引用的**每一张**图都能在包里找到。
//
// 判据取自**包里那份库**，题图列与答案图列都算 —— 两张表、两个列，只查一边的写法
// 会在这条测试上过不去。
func TestBundleHasEveryReferencedImage(t *testing.T) {
	e := newEnv(t)
	e.addQuestion(e.saveImage(color.RGBA{R: 255, A: 255}), e.saveImage(color.RGBA{G: 255, A: 255}))
	e.addQuestion(e.saveImage(color.RGBA{B: 255, A: 255}), "") // 没答案图，答案 hash 是空串
	e.addQuestion(e.saveImage(color.RGBA{R: 255, G: 255, A: 255}), e.saveImage(color.RGBA{G: 255, B: 255, A: 255}))

	b := e.bundle()
	db := b.openDB()

	refs := referencedHashes(listQuestions(t, db))
	if len(refs) != 5 {
		t.Fatalf("包里的库引用了 %d 张图，想要 5 张 —— 断言在空集合上空转就没有意义", len(refs))
	}

	for h := range refs {
		name := export.ImageDir + "/" + h + ".png"
		if _, ok := b.entries[name]; !ok {
			t.Errorf("库引用了 %s，包里却没有 %s（包里有 %v）", h, name, b.order)
		}
	}
}

// 方向二：包里**没有**库不引用的孤儿图片。
//
// 盘上真留一张没人引用的图（删错题时没回收掉的那种残留），它必须进不了包 ——
// 否则这条测试只是因为盘上本来就干净才过的。
func TestBundleHasNoOrphanImages(t *testing.T) {
	e := newEnv(t)
	question := e.saveImage(color.RGBA{R: 255, A: 255})
	answer := e.saveImage(color.RGBA{G: 255, A: 255})
	e.addQuestion(question, answer)

	orphan := e.saveImage(color.RGBA{B: 255, A: 255})
	orphanPath := e.images.Path(capture.Hash(orphan))
	if _, err := os.Stat(orphanPath); err != nil {
		t.Fatalf("残留图没造出来: %v", err)
	}

	b := e.bundle()

	refs := referencedHashes(listQuestions(t, b.openDB()))
	if len(refs) != 2 {
		t.Fatalf("包里的库引用了 %d 张图，想要 2 张", len(refs))
	}

	imgs := b.imageNames()
	if len(imgs) != 2 {
		t.Fatalf("包里有 %d 张图 %v，想要 2 张", len(imgs), imgs)
	}
	for _, name := range imgs {
		if !refs[hashOfEntry(b, name)] {
			t.Errorf("包里有一张库没引用的孤儿图 %s", name)
		}
	}
	if _, ok := b.entries[export.ImageDir+"/"+orphan+".png"]; ok {
		t.Errorf("盘上没人引用的 %s 不该进包", orphanPath)
	}
}

// 刚写进去的东西在包里：写一道错题、**不关库**、立刻导出，包里读得出来。
//
// 这一条测的是 WAL（library.dsn 把 journal_mode 设成了 WAL）：最近的提交可能还只在
// library.db-wal 里，直接拷 library.db 的写法会把这一截丢掉。包里的库是单独解出来的，
// 那个目录里没有任何 -wal / -shm 陪着它。
func TestWriteBundleSeesJustWrittenQuestion(t *testing.T) {
	e := newEnv(t)
	hash := e.saveImage(color.RGBA{R: 255, A: 255})

	added := e.addQuestion(hash, "") // 到这为止库一直开着，一次都没 checkpoint 过

	b := e.bundle()

	for _, name := range b.order {
		if strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") {
			t.Errorf("包里不该有旁挂文件 %s：解包那一侧拿到的会是一份对不上的库", name)
		}
	}

	db := b.openDB()
	fetched, err := db.GetQuestion(added.ID)
	if err != nil {
		t.Fatalf("包里的库读不回刚写的那道题: %v", err)
	}
	if fetched.QuestionHash != hash {
		t.Errorf("读回来 QuestionHash = %q，想要 %q", fetched.QuestionHash, hash)
	}
	if !fetched.CreatedAt.Equal(added.CreatedAt) {
		t.Errorf("读回来 CreatedAt = %v，想要 %v", fetched.CreatedAt, added.CreatedAt)
	}
}

// 上面那条测试要有牙齿：**直接拷主库文件**（票里点名的那个错误做法）拿到的库读不出
// 那道刚写的题。没有这一条，上面那条在「碰巧 checkpoint 过了」的情况下也会过，
// 于是它到底测没测到 WAL 就说不清了。
func TestNaiveCopyOfDBFileLosesRecentWrite(t *testing.T) {
	e := newEnv(t)
	hash := e.saveImage(color.RGBA{R: 255, A: 255})
	added := e.addQuestion(hash, "") // 库还开着，没 checkpoint 过

	// 拷的就是主库文件本身，-wal / -shm 不跟着走。
	raw, err := os.ReadFile(e.dbPath)
	if err != nil {
		t.Fatalf("读主库文件: %v", err)
	}
	path := filepath.Join(t.TempDir(), "library.db")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("写出这份拷贝: %v", err)
	}

	naive, err := library.Open(path)
	if err != nil {
		// 库缺到连开都开不了 —— 那也是丢了，这条测试照样成立。
		t.Logf("直接拷来的库开不了（同样是丢）: %v", err)
		return
	}
	defer naive.Close()

	if _, err := naive.GetQuestion(added.ID); err == nil {
		t.Error("直接拷主库文件竟然读得到刚写的那道题：上面那条 WAL 测试就没有牙齿了")
	}
}

// 同一个 hash 被多道题引用时，图只进包一次，而且不算孤儿。
//
// 去重发生在**文件那一层**（ADR-0004）：同一个 hash 完全可能既是 A 的题图、又是 B 的答案图，
// 按错题去重的写法会把它当成两张图、或者当成孤儿丢掉。
func TestSharedHashAppearsOnce(t *testing.T) {
	e := newEnv(t)
	shared := e.saveImage(color.RGBA{R: 255, A: 255})
	bImage := e.saveImage(color.RGBA{G: 255, A: 255})
	cImage := e.saveImage(color.RGBA{B: 255, A: 255})

	e.addQuestion(shared, "")     // A 的题图，与下面那道题的答案图是同一张
	e.addQuestion(bImage, shared) // B 的题图 + 答案图（答案图正是 shared）
	e.addQuestion(cImage, cImage) // C 的题图与答案图是同一张

	b := e.bundle()

	name := export.ImageDir + "/" + shared + ".png"
	n := 0
	for _, got := range b.order {
		if got == name {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%s 在包里出现了 %d 次，想要 1 次（全量：%v）", shared, n, b.order)
	}
	if _, ok := b.entries[name]; !ok {
		t.Fatalf("被两道题共用的 %s 没进包", shared)
	}
	b.assertEntries(
		"library.db",
		name,
		export.ImageDir+"/"+bImage+".png",
		export.ImageDir+"/"+cImage+".png",
	)
	// 5 个引用（A 题图、B 题图 + B 答案图、C 题图 + C 答案图）落在 3 个不同的 hash 上。
	if b.result.Images != 3 {
		t.Errorf("包里 %d 张图，想要 3 张", b.result.Images)
	}
	refs := referencedHashes(listQuestions(t, b.openDB()))
	if !refs[shared] {
		t.Errorf("包里的库还引用着 %s，它不该被当成孤儿", shared)
	}
}

// 库里引用的一张图在盘上找不到时，导出**必须失败**：一个缺图的包看着像备份，
// 等真要用的时候才发现少东西。
//
// 而且一个字节都不写 —— 核图在动手之前做（见 WriteBundle）。
func TestWriteBundleFailsWhenImageMissing(t *testing.T) {
	t.Run("图不在盘上", func(t *testing.T) {
		e := newEnv(t)
		gone := e.saveImage(color.RGBA{R: 255, A: 255})
		kept := e.saveImage(color.RGBA{G: 255, A: 255})
		e.addQuestion(gone, kept)

		// 那张图丢了：盘上被别的东西删掉，或者从来没落成过。
		if err := os.Remove(e.images.Path(capture.Hash(gone))); err != nil {
			t.Fatalf("删图: %v", err)
		}

		var buf bytes.Buffer
		res, err := e.svc.WriteBundle(&buf)
		if !errors.Is(err, export.ErrImageMissing) {
			t.Fatalf("WriteBundle 返回 %v，想要 ErrImageMissing", err)
		}
		if !strings.Contains(err.Error(), gone) {
			t.Errorf("错误里该点名是哪个 hash 丢了：%v", err)
		}
		if res != (export.Result{}) {
			t.Errorf("失败时 Result = %+v，想要零值", res)
		}
		if buf.Len() != 0 {
			t.Errorf("缺图时一个字节都不该写出去，w 上落了 %d 字节", buf.Len())
		}
	})

	t.Run("那个名字上是个目录", func(t *testing.T) {
		e := newEnv(t)
		hash := e.saveImage(color.RGBA{R: 255, A: 255})
		e.addQuestion(hash, "")

		path := e.images.Path(capture.Hash(hash))
		if err := os.Remove(path); err != nil {
			t.Fatalf("删图: %v", err)
		}
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatalf("建同名目录: %v", err)
		}

		var buf bytes.Buffer
		if _, err := e.svc.WriteBundle(&buf); !errors.Is(err, export.ErrImageMissing) {
			t.Errorf("WriteBundle 返回 %v，想要 ErrImageMissing", err)
		}
	})
}

// 空库也能导出：包里只有库这一样，不是错误。
func TestWriteBundleEmptyLibrary(t *testing.T) {
	e := newEnv(t)

	b := e.bundle()

	b.assertEntries("library.db")
	if b.result != (export.Result{Bytes: int64(len(b.raw))}) {
		t.Errorf("Result = %+v，想要 {0 0 %d}", b.result, len(b.raw))
	}
	if len(listQuestions(t, b.openDB())) != 0 {
		t.Error("空库导出的包里不该有错题")
	}
}

// 同一份库导出两次，得到同样的字节 —— 条目顺序是排过的，时间戳也没进包。
// 两份备份因此可以直接比。
func TestWriteBundleIsDeterministic(t *testing.T) {
	e := newEnv(t)
	e.addQuestion(e.saveImage(color.RGBA{R: 255, A: 255}), e.saveImage(color.RGBA{G: 255, A: 255}))
	e.addQuestion(e.saveImage(color.RGBA{B: 255, A: 255}), "")

	first := e.bundle()
	second := e.bundle()

	if !bytes.Equal(first.raw, second.raw) {
		t.Errorf("同一份库导出两次得到 %d 与 %d 字节，内容不同", len(first.raw), len(second.raw))
	}
}

// 导出的图是盘上那个文件的**原样字节**：包的图不经解码再编码 ——
// 文件名就是内容的 hash，搬家不该改内容。
func TestBundleImagesAreByteIdentical(t *testing.T) {
	e := newEnv(t)
	hash := e.saveImage(color.RGBA{R: 255, A: 255})
	e.addQuestion(hash, "")

	b := e.bundle()

	onDisk, err := os.ReadFile(e.images.Path(capture.Hash(hash)))
	if err != nil {
		t.Fatalf("读盘上的图: %v", err)
	}
	if !bytes.Equal(b.entries[export.ImageDir+"/"+hash+".png"], onDisk) {
		t.Error("包里的图与盘上的图字节不同")
	}
}
