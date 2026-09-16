package capture_test

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"testing"

	"questionbook/internal/capture"
)

// Service 是前端唯一碰得到的那一面，而它之前一行测试都没有 ——
// 底下 Rectify 与 Store 各有覆盖，卡在它们上面这层（base64 进、base64 出、
// 角点顺序、以及两次落盘只出一个文件）反而没人管。
//
// 只用导出的 API，自己造图，不依赖 fixture_test.go 里的东西。

// quadrantImage 造一张四角分色的方块，用来在拉正后检查每块颜色还在不在原处。
// 颜色按左下/右下的顺序摆，与 Quad 的约定对应：
//
//	左上红 · 右上绿
//	左下黄 · 右下蓝
func quadrantImage(n int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	topLeft := color.RGBA{R: 255, A: 255}
	topRight := color.RGBA{G: 255, A: 255}
	bottomLeft := color.RGBA{R: 255, G: 255, A: 255}
	bottomRight := color.RGBA{B: 255, A: 255}

	for y := range n {
		for x := range n {
			var c color.RGBA
			switch {
			case x < n/2 && y < n/2:
				c = topLeft
			case x >= n/2 && y < n/2:
				c = topRight
			case x < n/2 && y >= n/2:
				c = bottomLeft
			default:
				c = bottomRight
			}
			img.Set(x, y, c)
		}
	}
	return img
}

// toRGBA 把任意 image.Image 落成 RGBA，好逐像素比。
// 不能直接断言 *image.RGBA —— PNG 解码器给回来的可能是 *image.NRGBA。
func toRGBA(img image.Image) *image.RGBA {
	b := img.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), img, b.Min, draw.Src)
	return dst
}

func encodePNG(t *testing.T, img image.Image) string {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("编 PNG 失败: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func newService(t *testing.T) (*capture.Service, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := capture.NewStore(dir)
	if err != nil {
		t.Fatalf("开 store 失败: %v", err)
	}
	return capture.NewService(store), dir
}

// squareQuad 是 quadrantImage(n) 的四角，按约定顺序：左上 → 右上 → 右下 → 左下。
func squareQuad(n int) capture.Quad {
	f := float64(n)
	return capture.Quad{
		{X: 0, Y: 0},
		{X: f, Y: 0},
		{X: f, Y: f},
		{X: 0, Y: f},
	}
}

// reversedSquareQuad 是同一个正方形的另一种走法：左上 → 左下 → 右下 → 右上。
// 它不是自交，所以服务端拦不住 —— 见 TestServiceQuadOrderChangesTheResult。
func reversedSquareQuad(n int) capture.Quad {
	f := float64(n)
	return capture.Quad{
		{X: 0, Y: 0},
		{X: 0, Y: f},
		{X: f, Y: f},
		{X: f, Y: 0},
	}
}

// 一张正正方形的框 == 恒等变换。这走的正是今天前端唯一的调用形态
// （选框是轴对齐矩形，传进来的 quad 就是裁后图的四个角），所以它必须过。
func TestServiceRectifyThenCardRoundTrip(t *testing.T) {
	const n = 40
	src := quadrantImage(n)

	svc, dir := newService(t)
	encoded := encodePNG(t, src)

	hash, err := svc.Rectify(encoded, squareQuad(n))
	if err != nil {
		t.Fatalf("Rectify 失败: %v", err)
	}
	if hash == "" {
		t.Fatal("Rectify 返回了空 hash")
	}

	// 同一张图再来一次：hash 必须一样，目录里也不能多出第二个文件。
	again, err := svc.Rectify(encoded, squareQuad(n))
	if err != nil {
		t.Fatalf("第二次 Rectify 失败: %v", err)
	}
	if again != hash {
		t.Errorf("同一张图两次得到不同 hash: %q vs %q", hash, again)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读题图目录失败: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("去重失效：目录里有 %d 个文件，期望 1 个", len(entries))
	}

	// Card 取回来应当与直接 Rectify 逐像素相同 —— 这一路上过一次 PNG 编码、
	// 一次 PNG 解码，任何有损环节都会在这里露馅。
	want, err := capture.Rectify(src, squareQuad(n))
	if err != nil {
		t.Fatalf("参照用的 Rectify 失败: %v", err)
	}
	cardB64, err := svc.Card(hash)
	if err != nil {
		t.Fatalf("Card 失败: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(cardB64)
	if err != nil {
		t.Fatalf("Card 回的不是合法 base64: %v", err)
	}
	got, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("Card 回的 base64 解不出图像: %v", err)
	}
	if got.Bounds() != want.Bounds() {
		t.Fatalf("尺寸不一致: got %v, want %v", got.Bounds(), want.Bounds())
	}
	if !bytes.Equal(toRGBA(got).Pix, want.Pix) {
		t.Error("取回的题图与直接 Rectify 的结果不是逐像素相同")
	}
}

// 角点顺序是**前端的责任**，服务端拦不住也纠正不了。
//
// Quad 的文档里写明了这一点：把矩形按相反方向传，得到的是一张翻转的图，
// 而不是自交 —— 蝴蝶结检测（分母在定义域内变号）不会触发。所以这里断言的是
// 「两个方向给出不同的结果」，把这层契约钉住，而不是假装我们能报错。
func TestServiceQuadOrderChangesTheResult(t *testing.T) {
	const n = 40
	svc, _ := newService(t)
	encoded := encodePNG(t, quadrantImage(n))

	forward := squareQuad(n)
	reversed := reversedSquareQuad(n)

	a, err := svc.Rectify(encoded, forward)
	if err != nil {
		t.Fatalf("正向 Rectify 失败: %v", err)
	}
	b, err := svc.Rectify(encoded, reversed)
	if err != nil {
		t.Fatalf("反向的角点不该报错（它是同一个正方形的另一种走法）: %v", err)
	}
	if a == b {
		t.Error("两种角点顺序给出了同一个 hash —— 顺序没有生效，或图像恰好对称")
	}
}

func TestServiceRejectsBadInput(t *testing.T) {
	const n = 40
	svc, _ := newService(t)

	t.Run("不是 base64", func(t *testing.T) {
		if _, err := svc.Rectify("这显然不是 base64！！", squareQuad(n)); err == nil {
			t.Error("非 base64 应当报错")
		}
	})
	t.Run("是 base64 但不是图像", func(t *testing.T) {
		notAnImage := base64.StdEncoding.EncodeToString([]byte("这不是图片"))
		if _, err := svc.Rectify(notAnImage, squareQuad(n)); err == nil {
			t.Error("非图像应当报错")
		}
	})
	t.Run("退化成线的角点", func(t *testing.T) {
		f := float64(n)
		flat := capture.Quad{{X: 0, Y: 0}, {X: f, Y: 0}, {X: f, Y: 0}, {X: 0, Y: 0}}
		if _, err := svc.Rectify(encodePNG(t, quadrantImage(n)), flat); err == nil {
			t.Error("退化四边形应当报错")
		}
	})
	t.Run("取不存在的 hash", func(t *testing.T) {
		missing := "0000000000000000000000000000000000000000000000000000000000000000"
		if _, err := svc.Card(missing); err == nil {
			t.Error("取不存在的 hash 应当报错")
		}
	})
}
