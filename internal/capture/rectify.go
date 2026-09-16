package capture

import (
	"image"
	"image/color"
	"math"
)

// Rectify 把 img 上由 q 围出的四边形透视重映射成一张拉正的矩形卡片图。
//
// 输出尺寸取四边形对边里较长的那条，于是拉正后的图既不丢细节也不留黑边。
// 采样走双线性插值，落盘的是原始像素，没有任何增强。
// 采样点落在原图之外时取全透明 —— 正常流程里角点都在图内，这只兜底。
func Rectify(img image.Image, q Quad) (*image.RGBA, error) {
	if img == nil {
		return nil, ErrEmptyImage
	}
	if img.Bounds().Empty() {
		return nil, ErrEmptyImage
	}

	// 统一到原点在 (0,0) 的 RGBA，角点跟着平移，这样角点坐标与像素下标一一对应。
	src, shift := canonicalRGBA(img)
	for i := range q {
		q[i].X -= float64(shift.X)
		q[i].Y -= float64(shift.Y)
	}

	inv, err := inverseMap(q)
	if err != nil {
		return nil, err
	}

	w := int(math.Round(math.Max(dist(q[TopLeft], q[TopRight]), dist(q[BottomLeft], q[BottomRight]))))
	h := int(math.Round(math.Max(dist(q[TopLeft], q[BottomLeft]), dist(q[TopRight], q[BottomRight]))))
	if w < 1 || h < 1 {
		return nil, ErrDegenerateQuad
	}

	// 反向映射：遍历目标矩形的每个像素，反查它在原图上的位置。
	// 正向映射会在目标上留下永远写不到的空洞，所以必须反着来。
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		v := (float64(y) + 0.5) / float64(h)
		row := dst.Pix[y*dst.Stride : y*dst.Stride+w*4]
		for x := 0; x < w; x++ {
			u := (float64(x) + 0.5) / float64(w)
			sx, sy := inv.mapPoint(u, v)
			c := sampleBilinear(src, sx, sy)
			off := x * 4
			row[off+0] = c.R
			row[off+1] = c.G
			row[off+2] = c.B
			row[off+3] = c.A
		}
	}
	return dst, nil
}

// dist 是两点间的欧氏距离。
func dist(p, q Point) float64 { return math.Hypot(p.X-q.X, p.Y-q.Y) }

// homography 是定义在 (u,v) ∈ [0,1]² 上的透视变换：
//
//	x = (a·u + b·v + c) / (g·u + h·v + 1)
//	y = (d·u + e·v + f) / (g·u + h·v + 1)
//
// 分母恒为 1 时它退化成仿射变换（g = h = 0），此时平行边仍然平行。
type homography struct {
	a, b, c, d, e, f, g, h float64
}

// mapPoint 把定义域里的 (u,v) 映到值域。
func (t homography) mapPoint(u, v float64) (float64, float64) {
	den := t.g*u + t.h*v + 1
	return (t.a*u + t.b*v + t.c) / den, (t.d*u + t.e*v + t.f) / den
}

// denominator 是 mapPoint 的分母。它是 (u,v) 的线性函数，
// 所以单位正方形的四个顶点就覆盖了它在这个定义域上的全部极值。
func (t homography) denominator(u, v float64) float64 { return t.g*u + t.h*v + 1 }

// inverseMap 校验四边形并解出「目标矩形 → 原图四边形」的透视变换。
//
// 它就是拉正要用的反向映射：目标上的 (u,v) 代进去得到原图上的采样点。
func inverseMap(q Quad) (homography, error) {
	if math.Abs(signedArea(q)) < 1e-6 {
		// 退化成一个点或一条线时鞋带公式给零，围不出面积就谈不上拉正。
		return homography{}, ErrDegenerateQuad
	}

	t := squareToQuad(q)

	// 自交的四边形（蝴蝶结）不是任何矩形在透视下的像，它的分母会在定义域里变号。
	// 撞上这种情况直接报错，别把 Inf/NaN 悄悄写进卡片图。
	var hasPositive, hasNegative bool
	for _, uv := range [4][2]float64{{0, 0}, {1, 0}, {1, 1}, {0, 1}} {
		switch d := t.denominator(uv[0], uv[1]); {
		case d > 1e-9:
			hasPositive = true
		case d < -1e-9:
			hasNegative = true
		default:
			return homography{}, ErrDegenerateQuad
		}
	}
	if hasPositive && hasNegative {
		return homography{}, ErrDegenerateQuad
	}
	return t, nil
}

// signedArea 是四边形按角点顺序围出的有向面积，用鞋带公式算。
func signedArea(q Quad) float64 {
	sum := 0.0
	for i := range q {
		j := (i + 1) % len(q)
		sum += q[i].X*q[j].Y - q[j].X*q[i].Y
	}
	return sum / 2
}

// squareToQuad 解出把单位正方形映到四边形 q 的透视变换。
//
// 这里用 Heckbert 的闭式解，比列 8×8 方程组再高斯消元短得多；
// 测试里另有一份独立的 DLT 实现用来交叉验证它。
func squareToQuad(q Quad) homography {
	x0, y0 := q[TopLeft].X, q[TopLeft].Y
	x1, y1 := q[TopRight].X, q[TopRight].Y
	x2, y2 := q[BottomRight].X, q[BottomRight].Y
	x3, y3 := q[BottomLeft].X, q[BottomLeft].Y

	dx1, dx2, dx3 := x1-x2, x3-x2, x0-x1+x2-x3
	dy1, dy2, dy3 := y1-y2, y3-y2, y0-y1+y2-y3

	if dx3 == 0 && dy3 == 0 {
		// 两对对边分别平行：透视没戏可唱，退化成仿射。
		return homography{
			a: x1 - x0, b: x3 - x0, c: x0,
			d: y1 - y0, e: y3 - y0, f: y0,
		}
	}

	den := dx1*dy2 - dx2*dy1
	g := (dx3*dy2 - dx2*dy3) / den
	h := (dx1*dy3 - dx3*dy1) / den
	return homography{
		a: x1 - x0 + g*x1, b: x3 - x0 + h*x3, c: x0,
		d: y1 - y0 + g*y1, e: y3 - y0 + h*y3, f: y0,
		g: g, h: h,
	}
}

// sampleBilinear 在原图坐标 (x,y) 处取双线性插值的颜色。
//
// 采样点落在原图之外时返回全透明。插值核越过图边时按边缘像素外推，
// 免得卡片图的边上出现一圈半透明的缝。
func sampleBilinear(src *image.RGBA, x, y float64) color.RGBA {
	b := src.Bounds()
	if x < float64(b.Min.X) || x >= float64(b.Max.X) ||
		y < float64(b.Min.Y) || y >= float64(b.Max.Y) {
		return color.RGBA{}
	}

	// 像素 i 覆盖 [i, i+1)，中心在 i+0.5，所以 x 两侧的像素中心是
	// floor(x-0.5) 与 floor(x-0.5)+1，权重就是 x 落在两者之间的比例。
	fx, fy := x-0.5, y-0.5
	x0, y0 := int(math.Floor(fx)), int(math.Floor(fy))
	tx, ty := fx-float64(x0), fy-float64(y0)
	x1, y1 := clampInt(x0+1, b.Min.X, b.Max.X-1), clampInt(y0+1, b.Min.Y, b.Max.Y-1)
	x0, y0 = clampInt(x0, b.Min.X, b.Max.X-1), clampInt(y0, b.Min.Y, b.Max.Y-1)

	c00, c10 := pixelAt(src, x0, y0), pixelAt(src, x1, y0)
	c01, c11 := pixelAt(src, x0, y1), pixelAt(src, x1, y1)

	w00 := (1 - tx) * (1 - ty)
	w10 := tx * (1 - ty)
	w01 := (1 - tx) * ty
	w11 := tx * ty

	// image.RGBA 是预乘 alpha，直接在预乘空间里插值才是对的。
	return color.RGBA{
		R: round8(c00[0]*w00 + c10[0]*w10 + c01[0]*w01 + c11[0]*w11),
		G: round8(c00[1]*w00 + c10[1]*w10 + c01[1]*w01 + c11[1]*w11),
		B: round8(c00[2]*w00 + c10[2]*w10 + c01[2]*w01 + c11[2]*w11),
		A: round8(c00[3]*w00 + c10[3]*w10 + c01[3]*w01 + c11[3]*w11),
	}
}

// pixelAt 读出 (x,y) 处像素的四个通道。
func pixelAt(p *image.RGBA, x, y int) [4]float64 {
	off := p.PixOffset(x, y)
	return [4]float64{
		float64(p.Pix[off]),
		float64(p.Pix[off+1]),
		float64(p.Pix[off+2]),
		float64(p.Pix[off+3]),
	}
}

// round8 把插值结果收进一个字节，顺手夹住浮点误差造成的越界。
func round8(v float64) uint8 {
	switch {
	case v <= 0:
		return 0
	case v >= 255:
		return 255
	default:
		return uint8(v + 0.5)
	}
}

func clampInt(v, lo, hi int) int {
	switch {
	case v < lo:
		return lo
	case v > hi:
		return hi
	default:
		return v
	}
}
