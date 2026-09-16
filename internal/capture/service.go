package capture

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/jpeg" // 前端若哪天改传 JPEG，解不出来会变成一句难懂的报错，这里兜住
	"image/png"
)

// Service 是「采集」对前端暴露的那一面，也是这条链路上唯一的测试缝（见 spec）。
//
// 它只做搬运与编解码：真正的活（透视拉正、按内容 hash 落盘）在 Rectify 与 Store 里。
// 拆开是为了让那两样能在不启动 Wails 的情况下被测 —— NewService 出来的东西照样能直接调。
type Service struct {
	store *Store
}

// NewService 用一个已经开好的 Store 构造服务。
func NewService(store *Store) *Service {
	return &Service{store: store}
}

// Rectify 收一张帧（PNG 或 JPEG 的 base64）与四个角点，透视拉正后按内容 hash 落盘，返回该 hash。
//
// 角点坐标是**相对传入这张图**的像素坐标，顺序固定为 左上 → 右上 → 右下 → 左下（见 Quad）。
// 前端应当先按预览可见区裁好再传 —— 传整张传感器画面会白白多走一大截带宽与内存。
func (s *Service) Rectify(frameBase64 string, quad Quad) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(frameBase64)
	if err != nil {
		return "", fmt.Errorf("采集: 帧不是合法的 base64: %w", err)
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("采集: 帧解不出图像: %w", err)
	}

	card, err := Rectify(img, quad)
	if err != nil {
		return "", err
	}

	h, err := s.store.Save(card)
	if err != nil {
		return "", err
	}
	return h.String(), nil
}

// Card 按 hash 取回题图，返回 PNG 的 base64 供界面显示。
//
// 一律编成 PNG：题图是**原始像素**，不能再过一次有损编码（spec 的 Out of Scope）。
func (s *Service) Card(hash string) (string, error) {
	img, err := s.store.Load(Hash(hash))
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", fmt.Errorf("采集: 题图编码失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}
