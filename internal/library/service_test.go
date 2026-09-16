package library_test

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/draw"
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

// newService 在临时目录里搭一套「库 + 题图目录」，与 main.go 的接线同一个形状。
func newService(t *testing.T) (*library.Service, *capture.Store) {
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

	return library.NewService(db, cards), cards
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
