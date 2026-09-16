package library

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
)

// ImageStore 是题库读题图 / 答案图所需要的那一面：按内容 hash 把图读回来。
//
// 之所以要一个接口，是因为依赖只能有一个方向：采集要用题库把拍下的题落成错题
// （capture → library），题库就不能反过来 import 采集。采集那边的 *capture.Store
// 结构上就满足它（见 LoadByHash），接线时直接传进来即可。
type ImageStore interface {
	LoadByHash(hash string) (image.Image, error)
}

// Service 是「题库」对前端暴露的那一面。
//
// 和采集那边一样：这一层只做转接，增删查与读图本身在 Store 里，所以不启动 Wails 也能测。
type Service struct {
	store  *Store
	images ImageStore
}

// NewService 用一个已经开好的库、一个图片文件层构造服务。
func NewService(store *Store, images ImageStore) *Service {
	return &Service{store: store, images: images}
}

// Add 落一道新错题，返回落库后的记录（含分配到的 id 与创建时间）。
//
// questionHash 是题图的内容 hash；answerHash 是答案图的，传空串表示还没拍。
func (s *Service) Add(questionHash string, answerHash string) (Question, error) {
	return s.store.AddQuestion(Question{
		QuestionHash: questionHash,
		AnswerHash:   answerHash,
	})
}

// Get 按 id 取一道错题。
func (s *Service) Get(id int64) (Question, error) {
	return s.store.GetQuestion(id)
}

// List 列出全部错题，新的在前。
func (s *Service) List() ([]Question, error) {
	return s.store.ListQuestions()
}

// Delete 删掉一道错题，并返回被删掉的那条记录 —— 调用方据此知道该回收哪两张题图。
//
// 注意它只删库里的行，不碰图片文件：同一张题图可能被多道题引用，回收是引用计数的事。
func (s *Service) Delete(id int64) (Question, error) {
	return s.store.DeleteQuestion(id)
}

// QuestionImage 按内容 hash 取回题图，返回 PNG 的 base64 供界面显示。
//
// 读取题图 / 答案图归题库（spec 的服务划分），采集那边不再有读路径。
func (s *Service) QuestionImage(hash string) (string, error) {
	return s.image(hash, "题图")
}

// AnswerImage 按内容 hash 取回答案图，返回 PNG 的 base64。
//
// 还没拍答案图的错题，它的 AnswerHash 就是空串 —— 这里明确报出来，
// 而不是拿空串去拼一个叫 ".png" 的路径再报一句「打不开」。
func (s *Service) AnswerImage(hash string) (string, error) {
	return s.image(hash, "答案图")
}

// image 是两条读路径的共同实现：取出图来编成 PNG 的 base64。
//
// 一律编 PNG：图存的是**原始像素**，不能再过一次有损编码（spec 的 Out of Scope）。
func (s *Service) image(hash, label string) (string, error) {
	if hash == "" {
		return "", fmt.Errorf("题库: %s 的内容 hash 为空", label)
	}

	img, err := s.images.LoadByHash(hash)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", fmt.Errorf("题库: %s 编码失败: %w", label, err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}
