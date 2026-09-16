package library_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"questionbook/internal/capture"
	"questionbook/internal/library"
)

// 读取题图 / 答案图归题库（spec 的服务划分）。这两条读路径以前挂在采集上
// （capture.Service.Card），现在题库持有文件层的读，构造时注入。
//
// 文件层就用采集那边的 *capture.Store —— 接线时传的就是它，于是这里顺带把
// 「它确实满足 library.ImageStore」这层契约钉住。

// openHarness 在临时目录里搭一套「库 + 题图目录」，与 main.go 的接线同一个形状。
func openHarness(t *testing.T) (*library.Store, *capture.Store) {
	t.Helper()
	root := t.TempDir()

	cards, err := capture.NewStore(filepath.Join(root, "cards"))
	if err != nil {
		t.Fatalf("开题图目录失败: %v", err)
	}
	db, err := library.Open(filepath.Join(root, "library.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return db, cards
}

// newService 是只接了读图那一面的服务。
func newService(t *testing.T) (*library.Service, *capture.Store) {
	t.Helper()
	db, cards := openHarness(t)
	return library.NewService(db, cards), cards
}

// newServiceWithFiles 多接一个图片文件层的**写**：补拍答案图与删除时回收图片都要它。
func newServiceWithFiles(t *testing.T) (*library.Service, *capture.Store) {
	t.Helper()
	db, cards := openHarness(t)
	return library.NewService(db, cards, library.WithImageFiles(cardFiles{store: cards})), cards
}

// cardFiles 是 main.go 那层薄壳的测试版：把采集那边的具名类型（Quad / Hash）接成
// library.ImageFiles。
//
// 生产代码里这层壳待在 main —— 依赖只能有一个方向（采集 → 题库），题库不能 import 采集。
// 这里另写一份是为了让服务层测得动，**形状必须与 main.go 里那份一致**。
type cardFiles struct{ store *capture.Store }

func (f cardFiles) RectifyAndSave(frame image.Image, quad library.Quad) (string, error) {
	card, err := capture.Rectify(frame, capture.Quad{
		{X: quad[0].X, Y: quad[0].Y},
		{X: quad[1].X, Y: quad[1].Y},
		{X: quad[2].X, Y: quad[2].Y},
		{X: quad[3].X, Y: quad[3].Y},
	})
	if err != nil {
		return "", err
	}
	h, err := f.store.Save(card)
	if err != nil {
		return "", err
	}
	return h.String(), nil
}

func (f cardFiles) RemoveByHash(hash string) error {
	err := os.Remove(f.store.Path(capture.Hash(hash)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil // 本来就不在，删除的目的已经达到
	}
	return err
}

// testImage 造一张按坐标变色的图：纯色图连「通道搞混了」都测不出来。
func testImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(x * 40),
				G: uint8(y * 60),
				B: uint8(x + y),
				A: 255,
			})
		}
	}
	return img
}

// toRGBA 把任意 image.Image 落成 RGBA 好逐像素比 —— PNG 解码器给回来的
// 可能是 *image.NRGBA，直接断言 *image.RGBA 会误伤。
func toRGBA(img image.Image) *image.RGBA {
	b := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), img, b.Min, draw.Src)
	return dst
}

// 落一张图进文件层，再用它的 hash 建一道错题，返回落盘的题图与那道错题。
func seed(t *testing.T, svc *library.Service, cards *capture.Store, answerHash string) (*image.RGBA, library.Question) {
	t.Helper()

	src := testImage(6, 4)
	hash, err := cards.Save(src)
	if err != nil {
		t.Fatalf("题图落盘失败: %v", err)
	}
	q, err := svc.Add(hash.String(), answerHash)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	return src, q
}

// decodeImage 把服务回的那串 base64 解回图像。
func decodeImage(t *testing.T, b64 string) image.Image {
	t.Helper()
	if b64 == "" {
		t.Fatal("回的是空串")
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("回的不是合法 base64: %v", err)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("回的 base64 解不出图像: %v", err)
	}
	return img
}

func TestQuestionImageRoundTrip(t *testing.T) {
	svc, cards := newService(t)
	src, q := seed(t, svc, cards, "")

	b64, err := svc.QuestionImage(q.QuestionHash)
	if err != nil {
		t.Fatalf("QuestionImage: %v", err)
	}

	// 原样回来：这一路上过一次 PNG 编码、一次 PNG 解码，任何有损环节都会露馅。
	got := decodeImage(t, b64)
	if got.Bounds() != src.Bounds() {
		t.Fatalf("尺寸不一致: got %v, want %v", got.Bounds(), src.Bounds())
	}
	if !bytes.Equal(toRGBA(got).Pix, src.Pix) {
		t.Error("取回的题图与落盘的那张不是逐像素相同")
	}
}

func TestAnswerImageRoundTrip(t *testing.T) {
	svc, cards := newService(t)

	// 答案图与题图走同一条路径，只是落点（hash 字段）不同。
	answer := testImage(5, 7)
	answerHash, err := cards.Save(answer)
	if err != nil {
		t.Fatalf("答案图落盘失败: %v", err)
	}
	_, q := seed(t, svc, cards, answerHash.String())

	if !q.HasAnswer() {
		t.Fatal("存了答案图 hash，HasAnswer 应当为真")
	}
	b64, err := svc.AnswerImage(q.AnswerHash)
	if err != nil {
		t.Fatalf("AnswerImage: %v", err)
	}

	got := decodeImage(t, b64)
	if got.Bounds() != answer.Bounds() {
		t.Fatalf("尺寸不一致: got %v, want %v", got.Bounds(), answer.Bounds())
	}
	if !bytes.Equal(toRGBA(got).Pix, answer.Pix) {
		t.Error("取回的答案图与落盘的那张不是逐像素相同")
	}
}

// 空 hash 要明确报出来，不能拿它去拼一个叫 ".png" 的路径，再回一句看不懂的「打不开」。
func TestImageReadsRejectEmptyHash(t *testing.T) {
	svc, _ := newService(t)

	t.Run("题图", func(t *testing.T) {
		if _, err := svc.QuestionImage(""); err == nil {
			t.Error("题图 hash 为空时应当报错")
		}
	})
	t.Run("答案图", func(t *testing.T) {
		// 还没拍答案图的错题，它的 AnswerHash 就是空串。
		if _, err := svc.AnswerImage(""); err == nil {
			t.Error("答案图 hash 为空时应当报错")
		}
	})
}

// 库里有记录、文件层没有对应文件（被清理过 / 导出包不全）时要报错，不能回半张图。
func TestQuestionImageMissingFile(t *testing.T) {
	svc, cards := newService(t)
	_, q := seed(t, svc, cards, "")

	missing := strings.Repeat("0", 64) // 形状合法，但库里没有这张图
	if _, err := svc.QuestionImage(missing); err == nil {
		t.Error("取不存在的 hash 应当报错")
	}
	if missing == q.QuestionHash {
		t.Fatal("测试自己写错了：拿到的 hash 与不存在的那个撞上了")
	}
}

// encodePNG 把一张图编成帧要的那种 base64。
func encodePNG(t *testing.T, img image.Image) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("编 PNG 失败: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// identityQuad 是「裁出来的那张子图」的四个角，顺序 左上 → 右上 → 右下 → 左下。
//
// 选框是轴对齐的矩形，所以它在几何上就是恒等变换 —— 前端今天传的正是这一份
// （见 Capture.svelte 的 confirm）。
func identityQuad(w, h int) library.Quad {
	fw, fh := float64(w), float64(h)
	return library.Quad{
		{X: 0, Y: 0},   // 左上
		{X: fw, Y: 0},  // 右上
		{X: fw, Y: fh}, // 右下
		{X: 0, Y: fh},  // 左下
	}
}

// imageFiles 列出图片目录里的文件名，用来看落盘到底落了几个。
func imageFiles(t *testing.T, cards *capture.Store) []string {
	t.Helper()
	entries, err := os.ReadDir(cards.Dir())
	if err != nil {
		t.Fatalf("读图片目录失败: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// attach 补拍一张答案图，失败即终止测试。
func attach(t *testing.T, svc *library.Service, id int64, img image.Image) library.Question {
	t.Helper()
	b := img.Bounds()
	q, err := svc.AttachAnswer(id, encodePNG(t, img), identityQuad(b.Dx(), b.Dy()))
	if err != nil {
		t.Fatalf("AttachAnswer: %v", err)
	}
	return q
}

// 补拍答案图：拉正 + 按内容 hash 落盘 + 写到这道题的 answer_hash 上，一趟做完。
func TestAttachAnswerRoundTrip(t *testing.T) {
	svc, cards := newServiceWithFiles(t)
	_, q := seed(t, svc, cards, "")
	if q.HasAnswer() {
		t.Fatal("刚建的错题不该有答案图")
	}

	answer := testImage(5, 7)
	updated := attach(t, svc, q.ID, answer)

	if updated.ID != q.ID {
		t.Errorf("补拍之后 ID = %d，想要 %d", updated.ID, q.ID)
	}
	if !updated.HasAnswer() {
		t.Fatal("补拍之后 HasAnswer 应当为真")
	}
	// 补的是答案，题图一个字都不该动。
	if updated.QuestionHash != q.QuestionHash {
		t.Errorf("题图 hash 被改了: %q → %q", q.QuestionHash, updated.QuestionHash)
	}

	// 库里的那条也变了，不是只改了返回的副本。
	got, err := svc.Get(q.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AnswerHash != updated.AnswerHash {
		t.Errorf("库里的 AnswerHash = %q，想要 %q", got.AnswerHash, updated.AnswerHash)
	}

	// 盘上真的落了这张图，取回来与喂进去的那张逐像素相同
	// （这一路上过一次 PNG 编码、一次 PNG 解码，任何有损环节都会露馅）。
	b64, err := svc.AnswerImage(updated.AnswerHash)
	if err != nil {
		t.Fatalf("AnswerImage: %v", err)
	}
	decoded := toRGBA(decodeImage(t, b64))
	if decoded.Bounds() != answer.Bounds() {
		t.Fatalf("尺寸不一致: got %v, want %v", decoded.Bounds(), answer.Bounds())
	}
	if !bytes.Equal(decoded.Pix, answer.Pix) {
		t.Error("存下的答案图与喂进去的那张不是逐像素相同")
	}

	// 题图 + 答案图，两个文件。
	if files := imageFiles(t, cards); len(files) != 2 {
		t.Errorf("图片目录里有 %v，期望 2 个文件", files)
	}
}

// 补的必须是**已有的**那道错题：id 不存在时什么都不该留下 —— 尤其不能在盘上丢一张
// 没有任何错题引用的答案图（先查题、再落盘就是为了这个）。
func TestAttachAnswerRejectsUnknownQuestion(t *testing.T) {
	svc, cards := newServiceWithFiles(t)

	before := imageFiles(t, cards)
	_, err := svc.AttachAnswer(404, encodePNG(t, testImage(5, 7)), identityQuad(5, 7))
	if !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("补拍到不存在的错题返回 %v，想要 ErrNotFound", err)
	}
	if after := imageFiles(t, cards); len(after) != len(before) {
		t.Errorf("失败的补拍落了盘：%v → %v", before, after)
	}
}

// 帧不合法时同样不能留下半张图，题上的 answer_hash 也不能被动过。
func TestAttachAnswerRejectsBadFrame(t *testing.T) {
	svc, cards := newServiceWithFiles(t)
	_, q := seed(t, svc, cards, "")
	before := imageFiles(t, cards)

	cases := []struct {
		name  string
		frame string
		quad  library.Quad
	}{
		{
			name:  "不是 base64",
			frame: "这显然不是 base64！！",
			quad:  identityQuad(5, 7),
		},
		{
			name:  "是 base64 但不是图像",
			frame: base64.StdEncoding.EncodeToString([]byte("这不是图片")),
			quad:  identityQuad(5, 7),
		},
		{
			name:  "退化成线的角点",
			frame: encodePNG(t, testImage(5, 7)),
			// 四个点连成一条线，围不出可拉正的四边形。
			quad: library.Quad{{X: 0, Y: 0}, {X: 5, Y: 0}, {X: 5, Y: 0}, {X: 0, Y: 0}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.AttachAnswer(q.ID, tc.frame, tc.quad); err == nil {
				t.Fatal("这一次补拍应当报错")
			}

			got, err := svc.Get(q.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.HasAnswer() {
				t.Error("失败的补拍不该把答案 hash 写上去")
			}
			if after := imageFiles(t, cards); len(after) != len(before) {
				t.Errorf("失败的补拍落了盘：%v → %v", before, after)
			}
		})
	}
}

// 重拍答案图：新的顶掉旧的，旧的那张没人引用了就回收 —— 不然每重拍一次漏一个文件。
func TestAttachAnswerReplacesAndReclaimsTheOldOne(t *testing.T) {
	svc, cards := newServiceWithFiles(t)
	_, q := seed(t, svc, cards, "")

	first := attach(t, svc, q.ID, testImage(5, 7))
	second := attach(t, svc, q.ID, testImage(3, 4))

	if second.AnswerHash == first.AnswerHash {
		t.Fatal("两张不同的图不该落成同一个 hash")
	}
	got, err := svc.Get(q.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AnswerHash != second.AnswerHash {
		t.Errorf("库里的 AnswerHash = %q，想要 %q", got.AnswerHash, second.AnswerHash)
	}

	if _, err := svc.AnswerImage(first.AnswerHash); err == nil {
		t.Error("换下来的旧答案图应当已经被回收")
	}
	// 题图 + 新的答案图。
	if files := imageFiles(t, cards); len(files) != 2 {
		t.Errorf("图片目录里有 %v，期望 2 个文件", files)
	}
}

// 去重的副作用：两道题的答案图是同一张（内容相同 → 同一个 hash）。
// 换掉其中一道的答案时，另一道还引用着的那张不能被回收。
func TestAttachAnswerKeepsSharedImages(t *testing.T) {
	svc, cards := newServiceWithFiles(t)

	_, a := seed(t, svc, cards, "")
	_, b := seed(t, svc, cards, "")
	// seed 给两道题落的题图内容相同，所以它们共用一个 hash —— 顺带也钉住这一点。
	if a.QuestionHash != b.QuestionHash {
		t.Fatal("测试自己写错了：两张题图本该内容相同")
	}

	shared := attach(t, svc, a.ID, testImage(5, 7))
	attach(t, svc, b.ID, testImage(5, 7))
	if !shared.HasAnswer() {
		t.Fatal("补拍之后应当有答案图")
	}

	// a 换成另一张：b 还指着原来那张。
	attach(t, svc, a.ID, testImage(3, 4))
	if _, err := svc.AnswerImage(shared.AnswerHash); err != nil {
		t.Errorf("还有一道错题引用着这张答案图，不该被回收: %v", err)
	}
	// 两份题图（内容相同，只落一份）+ 两张答案图 = 3 个文件。
	if files := imageFiles(t, cards); len(files) != 3 {
		t.Errorf("图片目录里有 %v，期望 3 个文件", files)
	}
}

// 删掉一道错题，它的两张图都要清掉（票据 06）。
func TestDeleteRemovesBothImages(t *testing.T) {
	svc, cards := newServiceWithFiles(t)
	_, q := seed(t, svc, cards, "")
	q = attach(t, svc, q.ID, testImage(5, 7))

	if files := imageFiles(t, cards); len(files) != 2 {
		t.Fatalf("图片目录里有 %v，期望 2 个文件", files)
	}

	deleted, err := svc.Delete(q.ID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if deleted.QuestionHash != q.QuestionHash || deleted.AnswerHash != q.AnswerHash {
		t.Errorf("Delete 回的记录是 %+v，想要 %+v", deleted, q)
	}

	if files := imageFiles(t, cards); len(files) != 0 {
		t.Errorf("删掉错题之后图片目录里还剩 %v", files)
	}
	if _, err := svc.QuestionImage(q.QuestionHash); err == nil {
		t.Error("题图应当已经被清掉")
	}
	if _, err := svc.AnswerImage(q.AnswerHash); err == nil {
		t.Error("答案图应当已经被清掉")
	}
}

// 去重的副作用，必须有测试：两张错题拍的是同一张图（同一个 hash），
// 删掉其中一道**不能**把另一道还引用着的文件删掉。
func TestDeleteKeepsImagesStillReferenced(t *testing.T) {
	svc, cards := newServiceWithFiles(t)

	// seed 给每道题落的题图内容都一样 → 两道题共用同一份题图文件。
	_, a := seed(t, svc, cards, "")
	_, b := seed(t, svc, cards, "")
	if a.QuestionHash != b.QuestionHash {
		t.Fatal("测试自己写错了：两张题图本该内容相同")
	}
	if files := imageFiles(t, cards); len(files) != 1 {
		t.Fatalf("图片目录里有 %v，期望 1 个文件（内容寻址去重）", files)
	}

	if _, err := svc.Delete(a.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.QuestionImage(b.QuestionHash); err != nil {
		t.Errorf("还有一道错题引用着这张题图，不该被删: %v", err)
	}

	// 另一个引用也没了，这才轮到它。
	if _, err := svc.Delete(b.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if files := imageFiles(t, cards); len(files) != 0 {
		t.Errorf("引用都断了，图片目录里还剩 %v", files)
	}
}

// 题图与答案图恰好是同一张（同一个 hash）时，回收会被喊两次 —— 第二次不能报错，
// 也不能因为「第一次已经删掉了」就把删除整个搞失败。
func TestDeleteWhenBothHashesMatch(t *testing.T) {
	svc, cards := newServiceWithFiles(t)

	src := testImage(6, 4)
	h, err := cards.Save(src)
	if err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	q, err := svc.Add(h.String(), h.String())
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	if _, err := svc.Delete(q.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if files := imageFiles(t, cards); len(files) != 0 {
		t.Errorf("图片目录里还剩 %v", files)
	}
}

// 没接图片文件层时（票据 06 之前的接线），删除照旧只删库里的行。
func TestDeleteWithoutImageFilesLeavesFilesAlone(t *testing.T) {
	svc, cards := newService(t)
	_, q := seed(t, svc, cards, "")

	if _, err := svc.Delete(q.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if files := imageFiles(t, cards); len(files) != 1 {
		t.Errorf("没接图片文件层时不该动文件，目录里剩 %v", files)
	}
}

// 没接图片文件层时补拍答案图要明确报错，而不是悄悄什么都不做。
func TestAttachAnswerWithoutImageFilesErrors(t *testing.T) {
	svc, cards := newService(t)
	_, q := seed(t, svc, cards, "")

	_, err := svc.AttachAnswer(q.ID, encodePNG(t, testImage(5, 7)), identityQuad(5, 7))
	if err == nil {
		t.Fatal("没接图片文件层时补拍应当报错")
	}
}
