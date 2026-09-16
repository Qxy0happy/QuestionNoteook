package capture_test

// 这个文件放测试用的 fixture 生成器。
//
// 所有 fixture 都是程序生成的，仓库里不提交任何二进制图片。
// 透视 fixture 的生成路径刻意与被测代码分开走：这里用 DLT（直接线性变换）
// + 高斯消元自己解一遍映射，方向还相反（原图 → 单位正方形），
// 而被测的 Rectify 用的是 Heckbert 闭式解（单位正方形 → 原图）。
// 两边算法不同、方向不同，才能算交叉验证，而不是把被测代码的答案抄一遍再拿去断言。

import (
	"image"
	"image/color"
	"image/draw"
	"math"

	"questionbook/internal/capture"
)

// checkerCells 是 fixture 棋盘格的格数（每边）。
const checkerCells = 5

// outsideColor 是四边形之外的填充色。它不该出现在拉正结果里 ——
// 出现了就说明映射把选区外的东西也拉进来了。
var outsideColor = color.RGBA{R: 255, A: 255}

// checkerColor 返回「未畸变的正方形」上 (u,v) 处的棋盘格颜色。
func checkerColor(u, v float64, cells int) color.RGBA {
	if (int(u*float64(cells))+int(v*float64(cells)))%2 == 0 {
		return color.RGBA{R: 255, G: 255, B: 255, A: 255}
	}
	return color.RGBA{A: 255}
}

// unitMap 是把原图坐标映到单位正方形坐标的单应矩阵，未知量排成
// [a b c d e f g h]，对应 u = (ax+by+c)/(gx+hy+1)、v = (dx+ey+f)/(gx+hy+1)。
type unitMap [8]float64

// solveUnitMap 解出把四边形 q 映到单位正方形的单应矩阵。
// 四个点各给两行方程，凑成 8×8，列主元高斯消元。
func solveUnitMap(q capture.Quad) unitMap {
	var m [8][9]float64
	for i, uv := range [4][2]float64{{0, 0}, {1, 0}, {1, 1}, {0, 1}} {
		x, y, u, v := q[i].X, q[i].Y, uv[0], uv[1]
		m[2*i] = [9]float64{x, y, 1, 0, 0, 0, -x * u, -y * u, u}
		m[2*i+1] = [9]float64{0, 0, 0, x, y, 1, -x * v, -y * v, v}
	}

	for col := 0; col < 8; col++ {
		pivot := col
		for r := col + 1; r < 8; r++ {
			if math.Abs(m[r][col]) > math.Abs(m[pivot][col]) {
				pivot = r
			}
		}
		m[col], m[pivot] = m[pivot], m[col]
		for r := col + 1; r < 8; r++ {
			f := m[r][col] / m[col][col]
			if f == 0 {
				continue
			}
			for c := col; c < 9; c++ {
				m[r][c] -= f * m[col][c]
			}
		}
	}

	var sol unitMap
	for r := 7; r >= 0; r-- {
		s := m[r][8]
		for c := r + 1; c < 8; c++ {
			s -= m[r][c] * sol[c]
		}
		sol[r] = s / m[r][r]
	}
	return sol
}

// mapPoint 把原图坐标映到单位正方形坐标。
func (h unitMap) mapPoint(x, y float64) (float64, float64) {
	den := h[6]*x + h[7]*y + 1
	return (h[0]*x + h[1]*y + h[2]) / den, (h[3]*x + h[4]*y + h[5]) / den
}

// makePerspectiveChecker 程序生成一张「带已知畸变」的棋盘格。
//
// 做法是反过来：逐个原图像素算出它落在未畸变正方形的哪个 (u,v)，再按 (u,v) 涂色。
// 于是这张图就是「一张平整的棋盘格被 q 这个四边形透视扭曲之后」的样子，
// 用 q 去拉正它，理应还原出那张平整的棋盘格。四边形之外涂成醒目的红色。
func makePerspectiveChecker(size image.Point, q capture.Quad) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size.X, size.Y))
	inv := solveUnitMap(q)
	for y := 0; y < size.Y; y++ {
		for x := 0; x < size.X; x++ {
			u, v := inv.mapPoint(float64(x)+0.5, float64(y)+0.5)
			c := outsideColor
			if u >= 0 && u < 1 && v >= 0 && v < 1 {
				c = checkerColor(u, v, checkerCells)
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

// quadSamples 是测试里反复用的那个四边形：上边比下边短得多、四条边都不与坐标轴平行，
// 是个货真价实的梯形。轴对齐的包围盒裁剪在这张图上必然拿不到正确结果，
// 所以它能把「真的做了透视重映射」和「只是裁了个包围盒」区分开。
var quadSamples = capture.Quad{
	{X: 20, Y: 10},  // 左上
	{X: 140, Y: 30}, // 右上
	{X: 170, Y: 150},
	{X: 10, Y: 130},
}

// quadSamplesSize 是 quadSamples 所在原图的尺寸。
var quadSamplesSize = image.Point{X: 240, Y: 180}

// cropBBox 是「只裁轴对齐包围盒」这个错误实现，只为给 fixture 验明正身用：
// 拉正和裁剪的区别全在于前者抹平了梯形畸变，如果 fixture 对两者一视同仁，
// 那它就没有测到任何东西。见 TestPerspectiveFixtureHasTeeth。
func cropBBox(src *image.RGBA, q capture.Quad) *image.RGBA {
	b := src.Bounds()
	minX, minY := b.Max.X, b.Max.Y
	maxX, maxY := b.Min.X, b.Min.Y
	for _, p := range q {
		minX = min(minX, int(p.X))
		minY = min(minY, int(p.Y))
		maxX = max(maxX, int(p.X))
		maxY = max(maxY, int(p.Y))
	}
	r := image.Rect(minX, minY, maxX, maxY).Intersect(b)
	out := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(out, out.Bounds(), src, r.Min, draw.Src)
	return out
}

// solidBlocks 画一张 8×8 的图：四个 4×4 的角块各一色，方便按颜色断言位置。
func solidBlocks() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	blocks := [4]struct {
		rect image.Rectangle
		c    color.RGBA
	}{
		{image.Rect(0, 0, 4, 4), color.RGBA{R: 255, A: 255}},         // 左上 红
		{image.Rect(4, 0, 8, 4), color.RGBA{G: 255, A: 255}},         // 右上 绿
		{image.Rect(0, 4, 4, 8), color.RGBA{B: 255, A: 255}},         // 左下 蓝
		{image.Rect(4, 4, 8, 8), color.RGBA{R: 255, G: 255, A: 255}}, // 右下 黄
	}
	for _, b := range blocks {
		for y := b.rect.Min.Y; y < b.rect.Max.Y; y++ {
			for x := b.rect.Min.X; x < b.rect.Max.X; x++ {
				img.SetRGBA(x, y, b.c)
			}
		}
	}
	return img
}

// stripes 画一张有结构的图：纯色图连「转换搞错了」都测不出来，
// 所以按列涂不同颜色，只要像素被改动过就看得出来。
func stripes(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(x * 255 / max(w-1, 1)),
				G: uint8(y * 255 / max(h-1, 1)),
				B: uint8((x + y) % 256),
				A: 255,
			})
		}
	}
	return img
}

// maxChannelDiff 返回两个颜色之间最大的单通道差。
func maxChannelDiff(a, b color.RGBA) int {
	d := 0
	for _, p := range [4][2]uint8{{a.R, b.R}, {a.G, b.G}, {a.B, b.B}, {a.A, b.A}} {
		diff := int(p[0]) - int(p[1])
		if diff < 0 {
			diff = -diff
		}
		d = max(d, diff)
	}
	return d
}
