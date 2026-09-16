package capture_test

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"questionbook/internal/capture"
	"questionbook/internal/library"
)

// Service 是前端唯一碰得到的那一面。采集这半边现在还要把拍下的一道题落成**错题**
// （capture → library），所以这些测试搭的是与 main.go 同一套接线：真文件层 + 真库，
// 都落在临时目录里 —— 服务层是唯一的测试缝（见 spec），不必跑起 Wails。
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

// harness 是服务层那套接线：题图 Store + 库 + 采集。
// 两个服务都拿得到，因为一次采集的产物横跨它们俩。
type harness struct {
	svc      *capture.Service
	lib      *library.Service
	cardsDir string
}

func newHarness(t *testing.T) harness {
	t.Helper()
	root := t.TempDir()
	cardsDir := filepath.Join(root, "cards")

	cards, err := capture.NewStore(cardsDir)
	if err != nil {
		t.Fatalf("开题图目录失败: %v", err)
	}
	db, err := library.Open(filepath.Join(root, "library.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	lib := library.NewService(db, cards)
	return harness{svc: capture.NewService(cards, lib), lib: lib, cardsDir: cardsDir}
}

// listCards 返回题图目录里的文件名，用来看落盘到底落了几个。
func (h harness) listCards(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(h.cardsDir)
	if err != nil {
		t.Fatalf("读题图目录失败: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
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

// 本票要买的就是这个：**一次调用之后库里就有那道错题**。
//
// 以前是「先要 hash、前端再另起一次 Add」，两次 IPC 之间失败就留下一张库未引用的题图。
// 所以这里断言的不是「返回了一个 id」，而是返回的那道错题**已经在库里**、并且题图
// 按它的 hash 取得回来 —— 前端不需要再补写任何东西。
func TestCaptureLeavesTheQuestionInLibrary(t *testing.T) {
	const n = 40
	h := newHarness(t)

	q, err := h.svc.Capture(encodePNG(t, quadrantImage(n)), squareQuad(n))
	if err != nil {
		t.Fatalf("Capture 失败: %v", err)
	}
	if q.ID == 0 {
		t.Fatal("Capture 返回的错题没有 id")
	}
	if q.QuestionHash == "" {
		t.Fatal("Capture 返回的错题没有题图 hash")
	}
	if q.HasAnswer() {
		t.Error("手边还没答案图，AnswerHash 应当是空串")
	}

	// 返回的那条就是库里的那条，不是一条「稍后再由前端补写」的承诺。
	got, err := h.lib.Get(q.ID)
	if err != nil {
		t.Fatalf("Capture 之后按 id 取不到错题: %v", err)
	}
	if got.QuestionHash != q.QuestionHash || got.AnswerHash != q.AnswerHash || !got.CreatedAt.Equal(q.CreatedAt) {
		t.Errorf("库里的错题是 %+v，Capture 返回的是 %+v", got, q)
	}

	// 题库列表里也看得到它 —— 拍完滑到题库页就该有这一道。
	qs, err := h.lib.List()
	if err != nil {
		t.Fatalf("列出错题: %v", err)
	}
	if len(qs) != 1 || qs[0].ID != q.ID {
		t.Fatalf("库里有 %d 道错题（%+v），期望只有刚拍的这一道", len(qs), qs)
	}

	// 题图也真的按内容 hash 落了盘，取得回来。
	if files := h.listCards(t); len(files) != 1 {
		t.Errorf("题图目录里有 %v，期望只有 1 个文件", files)
	}
	if _, err := h.lib.QuestionImage(q.QuestionHash); err != nil {
		t.Errorf("按返回的 hash 取不回题图: %v", err)
	}
}

// 一张正正方形的框 == 恒等变换。这走的正是今天前端唯一的调用形态
// （选框是轴对齐矩形，传进来的 quad 就是裁后图的四个角），所以它必须过。
func TestServiceCaptureRoundTrip(t *testing.T) {
	const n = 40
	src := quadrantImage(n)
	h := newHarness(t)
	encoded := encodePNG(t, src)

	q, err := h.svc.Capture(encoded, squareQuad(n))
	if err != nil {
		t.Fatalf("Capture 失败: %v", err)
	}

	// 同一张图再来一次：内容寻址，hash 必须一样，目录里也不能多出第二个文件。
	again, err := h.svc.Capture(encoded, squareQuad(n))
	if err != nil {
		t.Fatalf("第二次 Capture 失败: %v", err)
	}
	if again.QuestionHash != q.QuestionHash {
		t.Errorf("同一张图两次得到不同 hash: %q vs %q", q.QuestionHash, again.QuestionHash)
	}
	if files := h.listCards(t); len(files) != 1 {
		t.Errorf("去重失效：题图目录里有 %v，期望只有 1 个文件", files)
	}
	// 两次采集是两道错题，共用同一张题图 —— 去重发生在文件那一层，不在错题这一层（ADR-0004）。
	qs, err := h.lib.List()
	if err != nil {
		t.Fatalf("列出错题: %v", err)
	}
	if len(qs) != 2 {
		t.Errorf("两次采集之后库里有 %d 道错题，期望 2 道", len(qs))
	}

	// 取回来应当与直接 Rectify 的结果逐像素相同 —— 这一路上过一次 PNG 编码、
	// 一次 PNG 解码，任何有损环节都会在这里露馅。
	want, err := capture.Rectify(src, squareQuad(n))
	if err != nil {
		t.Fatalf("参照用的 Rectify 失败: %v", err)
	}
	cardB64, err := h.lib.QuestionImage(q.QuestionHash)
	if err != nil {
		t.Fatalf("取题图失败: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(cardB64)
	if err != nil {
		t.Fatalf("题图回的不是合法 base64: %v", err)
	}
	got, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("题图回的 base64 解不出图像: %v", err)
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
	h := newHarness(t)
	encoded := encodePNG(t, quadrantImage(n))

	a, err := h.svc.Capture(encoded, squareQuad(n))
	if err != nil {
		t.Fatalf("正向 Capture 失败: %v", err)
	}
	b, err := h.svc.Capture(encoded, reversedSquareQuad(n))
	if err != nil {
		t.Fatalf("反向的角点不该报错（它是同一个正方形的另一种走法）: %v", err)
	}
	if a.QuestionHash == b.QuestionHash {
		t.Error("两种角点顺序给出了同一个 hash —— 顺序没有生效，或图像恰好对称")
	}
}

// 采集失败时，库里不许留下半条记录、盘上也不许留下半张图。
func TestCaptureRejectsBadInput(t *testing.T) {
	const n = 40

	cases := []struct {
		name  string
		frame string
		quad  capture.Quad
	}{
		{
			name:  "不是 base64",
			frame: "这显然不是 base64！！",
			quad:  squareQuad(n),
		},
		{
			name:  "是 base64 但不是图像",
			frame: base64.StdEncoding.EncodeToString([]byte("这不是图片")),
			quad:  squareQuad(n),
		},
		{
			name:  "退化成线的角点",
			frame: encodePNG(t, quadrantImage(n)),
			// 四个点连成一条线，围不出可拉正的四边形。
			quad: capture.Quad{{X: 0, Y: 0}, {X: float64(n), Y: 0}, {X: float64(n), Y: 0}, {X: 0, Y: 0}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)

			if _, err := h.svc.Capture(tc.frame, tc.quad); err == nil {
				t.Fatal("这一次采集应当报错")
			}

			qs, err := h.lib.List()
			if err != nil {
				t.Fatalf("列出错题: %v", err)
			}
			if len(qs) != 0 {
				t.Errorf("失败的采集不该建出错题，库里有 %d 道", len(qs))
			}
			if files := h.listCards(t); len(files) != 0 {
				t.Errorf("失败的采集不该落盘，题图目录里有 %v", files)
			}
		})
	}
}
