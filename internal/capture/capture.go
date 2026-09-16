// Package capture 是采集闭环的 Go 侧：把用户手拖出的四边形透视拉正成题图，
// 再按内容 hash 把题图落进应用私有目录。
//
// 两条刻意的边界，别当成疏漏：
//   - 不做任何自动检测。四个角点全部来自用户拖动，这里不找纸面四角、
//     不识别文本块、也不切题（ADR-0006）。
//   - 不做图像增强。拉正后存的是原始像素，没有背景归一化 / CLAHE / 锐化
//     （spec 的 Out of Scope）。
//
// 原图即弃：题图落盘之后原图就不再保留（ADR-0007），
// 所以确认框选那一刻即定稿，框歪了只能重拍。
package capture

import (
	"errors"
	"image"
	"image/draw"
)

// Point 是图像像素坐标系里的一个点：原点在左上角，X 向右、Y 向下。
type Point struct {
	X, Y float64
}

// Quad 是用户拖出的四个角点，顺序固定为 左上 → 右上 → 右下 → 左下。
//
// 顺序就是一切：它不要求是矩形、不要求轴对齐，甚至不要求凸 —— 透视拉正
// 存在的意义正是抹平这个歪掉的四边形。反过来，顺序错了（比如把角点按
// 顺时针传成了逆时针），拉正结果就是翻转或自交的，这里只能报错拦不住。
type Quad [4]Point

// Quad 的下标含义，供调用方按名字取角点。
const (
	TopLeft = iota
	TopRight
	BottomRight
	BottomLeft
)

var (
	// ErrEmptyImage 表示输入图一个像素都没有。
	ErrEmptyImage = errors.New("capture: 输入图是空的")
	// ErrDegenerateQuad 表示四个角点围不出一个能拉正的四边形：退化成一个点
	// 或一条线，或者自交成了蝴蝶结（某个角点被拖到了对边的另一侧）。
	ErrDegenerateQuad = errors.New("capture: 角点退化，围不出可拉正的四边形")
)

// canonicalRGBA 把任意 image.Image 统一成一份原点在 (0,0) 的 *image.RGBA，
// 并返回被抹掉的原点偏移 —— 角点坐标要减掉这个偏移才能对上像素下标。
//
// 已经是原点在 (0,0) 的 *image.RGBA 时直接返回，不拷贝：
// 拉正的结果正是这种图，没必要为了统一再翻一倍内存。
func canonicalRGBA(img image.Image) (*image.RGBA, image.Point) {
	b := img.Bounds()
	if r, ok := img.(*image.RGBA); ok && b.Min == (image.Point{}) {
		return r, image.Point{}
	}
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), img, b.Min, draw.Src)
	return dst, b.Min
}
