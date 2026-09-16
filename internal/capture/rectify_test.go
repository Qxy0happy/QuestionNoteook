package capture_test

import (
	"errors"
	"image"
	"image/color"
	"testing"

	"questionbook/internal/capture"
)

// TestRectify_Perspective 是这一票的核心断言：喂一张程序生成的、带已知畸变的棋盘格，
// 断言拉正之后每一格的中心都是它该有的颜色。
//
// 畸变来自 quadSamples 这个梯形。如果实现偷懒只裁了个轴对齐包围盒，
// 格心会整体错位，这条必挂。
func TestRectify_Perspective(t *testing.T) {
	src := makePerspectiveChecker(quadSamplesSize, quadSamples)

	got, err := capture.Rectify(src, quadSamples)
	if err != nil {
		t.Fatalf("Rectify: %v", err)
	}
	b := got.Bounds()

	// 逐格取格心：格心离格线足够远，双线性插值不会把相邻格的颜色混进来。
	for cu := 0; cu < checkerCells; cu++ {
		for cv := 0; cv < checkerCells; cv++ {
			u := (float64(cu) + 0.5) / checkerCells
			v := (float64(cv) + 0.5) / checkerCells
			x, y := int(u*float64(b.Dx())), int(v*float64(b.Dy()))

			want := checkerColor(u, v, checkerCells)
			if diff := maxChannelDiff(got.RGBAAt(x, y), want); diff > 40 {
				t.Errorf("格 (%d,%d) 的格心像素 (%d,%d) = %v，想要 %v（差 %d）",
					cu, cv, x, y, got.RGBAAt(x, y), want, diff)
			}
		}
	}

	// 四边形之外是红色。结果里除了边缘那圈插值混出来的过渡像素，不该有红色；
	// 有一片红就说明映射把选区外的东西也拉进来了。
	red := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := got.RGBAAt(x, y)
			if c.R > 150 && int(c.R)-int(c.G) > 100 && int(c.R)-int(c.B) > 100 {
				red++
			}
		}
	}
	if frac := float64(red) / float64(b.Dx()*b.Dy()); frac > 0.10 {
		t.Errorf("拉正结果里有 %.1f%% 的像素是选区外的红色，想要接近 0", frac*100)
	} else {
		t.Logf("卡片图 %v，其中 %.2f%% 是边缘插值混出来的选区外红色", b, frac*100)
	}
}

// TestPerspectiveFixtureHasTeeth 是给上面那条测试自己上的保险。
//
// 把同样的格心断言套在「只裁轴对齐包围盒」这个错误实现上，它必须挂 ——
// 否则说明这张 fixture 分辨不出透视拉正，上面那条测试通过也说明不了什么。
func TestPerspectiveFixtureHasTeeth(t *testing.T) {
	src := makePerspectiveChecker(quadSamplesSize, quadSamples)
	naive := cropBBox(src, quadSamples)
	b := naive.Bounds()

	mismatches := 0
	for cu := 0; cu < checkerCells; cu++ {
		for cv := 0; cv < checkerCells; cv++ {
			u := (float64(cu) + 0.5) / checkerCells
			v := (float64(cv) + 0.5) / checkerCells
			x, y := int(u*float64(b.Dx())), int(v*float64(b.Dy()))
			if maxChannelDiff(naive.RGBAAt(x, y), checkerColor(u, v, checkerCells)) > 40 {
				mismatches++
			}
		}
	}
	if mismatches == 0 {
		t.Fatal("fixture 对「只裁包围盒」也全过，它分辨不出透视拉正，换一张")
	}
	t.Logf("只裁包围盒时 %d/%d 个格心对不上", mismatches, checkerCells*checkerCells)
}

// TestRectify_AxisAlignedQuad 用一张四角分色的图钉住最基本的行为：
// 框多大就出多大，而且颜色原样搬过来，没有翻转、没有偏移。
func TestRectify_AxisAlignedQuad(t *testing.T) {
	src := solidBlocks()

	// 正中 4×4 的区域：横跨四个色块。
	got, err := capture.Rectify(src, capture.Quad{
		{X: 2, Y: 2}, {X: 6, Y: 2}, {X: 6, Y: 6}, {X: 2, Y: 6},
	})
	if err != nil {
		t.Fatalf("Rectify: %v", err)
	}
	if want := image.Rect(0, 0, 4, 4); got.Bounds() != want {
		t.Fatalf("输出尺寸 = %v，想要 %v（框了 4×4 就该出 4×4）", got.Bounds(), want)
	}

	// 轴对齐且尺寸不变时，目标像素中心正好落在源像素中心上，颜色应当逐点相等。
	want := [4][4]color.RGBA{
		{{R: 255, A: 255}, {R: 255, A: 255}, {G: 255, A: 255}, {G: 255, A: 255}},
		{{R: 255, A: 255}, {R: 255, A: 255}, {G: 255, A: 255}, {G: 255, A: 255}},
		{{B: 255, A: 255}, {B: 255, A: 255}, {R: 255, G: 255, A: 255}, {R: 255, G: 255, A: 255}},
		{{B: 255, A: 255}, {B: 255, A: 255}, {R: 255, G: 255, A: 255}, {R: 255, G: 255, A: 255}},
	}
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			if diff := maxChannelDiff(got.RGBAAt(x, y), want[y][x]); diff > 4 {
				t.Errorf("像素 (%d,%d) = %v，想要 %v（差 %d）", x, y, got.RGBAAt(x, y), want[y][x], diff)
			}
		}
	}
}

// TestRectify_RotatedQuad 把图转 90° 拉正：源图左上角的红块应当落到输出的右上角。
// 轴对齐的包围盒裁剪会把它留在左上角，所以这条卡的是「按角点顺序摆放」。
func TestRectify_RotatedQuad(t *testing.T) {
	src := solidBlocks()

	// 角点顺序仍是 左上→右上→右下→左下，只是这次左上角点落在源图的左下角。
	got, err := capture.Rectify(src, capture.Quad{
		{X: 0, Y: 8}, {X: 0, Y: 0}, {X: 8, Y: 0}, {X: 8, Y: 8},
	})
	if err != nil {
		t.Fatalf("Rectify: %v", err)
	}
	if got.Bounds() != image.Rect(0, 0, 8, 8) {
		t.Fatalf("输出尺寸 = %v，想要 8×8", got.Bounds())
	}

	// 转过 90° 之后，源图的四个角块在输出里整体转了一格：
	// 左上红 → 右上，右上绿 → 右下，右下黄 → 左下，左下蓝 → 左上。
	cases := []struct {
		x, y int
		want color.RGBA
	}{
		{6, 1, color.RGBA{R: 255, A: 255}},         // 红
		{1, 1, color.RGBA{B: 255, A: 255}},         // 蓝
		{1, 6, color.RGBA{R: 255, G: 255, A: 255}}, // 黄
		{6, 6, color.RGBA{G: 255, A: 255}},         // 绿
	}
	for _, c := range cases {
		if diff := maxChannelDiff(got.RGBAAt(c.x, c.y), c.want); diff > 4 {
			t.Errorf("像素 (%d,%d) = %v，想要 %v（差 %d）", c.x, c.y, got.RGBAAt(c.x, c.y), c.want, diff)
		}
	}
}

// TestRectify_RejectsUnusableInput 覆盖抠不出卡片图的那几种输入。
func TestRectify_RejectsUnusableInput(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			src.SetRGBA(x, y, color.RGBA{R: uint8(x * 8), A: 255})
		}
	}

	t.Run("空图", func(t *testing.T) {
		_, err := capture.Rectify(image.NewRGBA(image.Rect(0, 0, 0, 0)), capture.Quad{
			{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1},
		})
		if !errors.Is(err, capture.ErrEmptyImage) {
			t.Fatalf("err = %v，想要 ErrEmptyImage", err)
		}
	})

	bad := map[string]capture.Quad{
		"缩成一点":     {{X: 8, Y: 8}, {X: 8, Y: 8}, {X: 8, Y: 8}, {X: 8, Y: 8}},
		"缩成一条线":    {{X: 0, Y: 16}, {X: 32, Y: 16}, {X: 32, Y: 16}, {X: 0, Y: 16}},
		"左右对称的蝴蝶结": {{X: 4, Y: 4}, {X: 28, Y: 28}, {X: 28, Y: 4}, {X: 4, Y: 28}},
		"歪一点的蝴蝶结":  {{X: 4, Y: 4}, {X: 28, Y: 28}, {X: 24, Y: 2}, {X: 4, Y: 28}},
	}
	for name, q := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := capture.Rectify(src, q); !errors.Is(err, capture.ErrDegenerateQuad) {
				t.Fatalf("err = %v，想要 ErrDegenerateQuad", err)
			}
		})
	}
}

// TestRectify_ParallelogramStillWorks 是一条回归线：对边平行时透视退化成仿射，
// 闭式解走的是另一个分支，别让它掉进除零或者算反。
func TestRectify_ParallelogramStillWorks(t *testing.T) {
	src := solidBlocks()

	// 平行四边形：上边整体右移 2 像素，下边不动。四条边的水平跨度都是 4。
	got, err := capture.Rectify(src, capture.Quad{
		{X: 2, Y: 0}, {X: 6, Y: 0}, {X: 4, Y: 8}, {X: 0, Y: 8},
	})
	if err != nil {
		t.Fatalf("Rectify: %v", err)
	}
	if want := image.Rect(0, 0, 4, 8); got.Bounds() != want {
		t.Fatalf("输出尺寸 = %v，想要 %v（对边都是 4 宽，斜边量出来约 8.25 取整）", got.Bounds(), want)
	}

	// 倾斜被拉直之后，源图左右两半的颜色分界应当斜着落在输出里：
	// 上边分界在正中，越往下越靠右。
	cases := []struct {
		x, y int
		want color.RGBA
	}{
		{0, 0, color.RGBA{R: 255, A: 255}}, // 分界在 u=0.53，左边是红
		{3, 0, color.RGBA{G: 255, A: 255}}, // 右边是绿
		{0, 7, color.RGBA{B: 255, A: 255}}, // 底部左边是蓝
	}
	for _, c := range cases {
		if diff := maxChannelDiff(got.RGBAAt(c.x, c.y), c.want); diff > 4 {
			t.Errorf("像素 (%d,%d) = %v，想要 %v（差 %d）", c.x, c.y, got.RGBAAt(c.x, c.y), c.want, diff)
		}
	}
}
