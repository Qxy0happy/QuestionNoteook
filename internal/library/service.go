package library

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // 与采集那边同一个理由：前端若哪天改传 JPEG，解不出来会变成一句难懂的报错
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

// Point 是图像像素坐标系里的一个点：原点在左上角，X 向右、Y 向下。
//
// 与采集那边的 capture.Point 是同一个概念、同一份形状 —— 前端在拍题图与补拍答案图
// 两条路径上传的是同一种东西。另立一个只是因为依赖只能有一个方向：题库不能 import 采集。
type Point struct {
	X, Y float64
}

// Quad 是补拍答案图时四个角点的顺序，固定为 左上 → 右上 → 右下 → 左下
// （与 capture.Quad 同一个约定，前端照搬拍题图那一份即可）。
type Quad [4]Point

// ImageFiles 是题库对图片文件层的**写**那一面。
//
// 读那一面是构造时必给的 ImageStore；写这一面是可选的，因为只有补拍答案图与
// 「删错题顺带回收图片」用它。采集那边的 capture.Rectify 与 *capture.Store 合起来
// 就是这件事，但它们的 Quad / Hash 是采集的具名类型，所以在 main.go 里用一层薄壳接上
// （与 ImageStore 同一个道理）。
type ImageFiles interface {
	// RectifyAndSave 把一帧原图按四个角点拉正，再按内容 hash 落盘，返回答案图的 hash。
	// 这一步与拍题图走的是**同一条**路径：拉正用 capture.Rectify，落盘用 *capture.Store.Save。
	RectifyAndSave(frame image.Image, quad Quad) (string, error)

	// RemoveByHash 按内容 hash 删掉库外的那个图片文件。
	// 文件本来就不在时应当返回 nil —— 调用方分不清也不关心这一点。
	RemoveByHash(hash string) error
}

// Service 是「题库」对前端暴露的那一面。
//
// 和采集那边一样：这一层只做转接，增删查与读图本身在 Store 里，所以不启动 Wails 也能测。
type Service struct {
	store  *Store
	images ImageStore
	// 图片文件层的写路径。没接上时为 nil，补拍答案图会明确报错，其余功能照常。
	files ImageFiles
}

// Option 给服务补一样可选能力。
type Option func(*Service)

// WithImageFiles 接上图片文件层的写：补拍答案图，以及删除错题时回收图片。
//
// 做成可选项而不是必给的构造参数，是因为既有的那条接线（拍题图、读题图）不需要它。
func WithImageFiles(files ImageFiles) Option {
	return func(s *Service) { s.files = files }
}

// NewService 用一个已经开好的库、一个图片文件层构造服务。
func NewService(store *Store, images ImageStore, opts ...Option) *Service {
	s := &Service{store: store, images: images}
	for _, opt := range opts {
		opt(s)
	}
	return s
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

// AttachAnswer 给一道**已有的**错题补上答案图：收一张帧（PNG 或 JPEG 的 base64）与四个角点，
// 透视拉正后按内容 hash 落盘，再把 hash 写到这道题的 answer_hash 上，返回更新后的那条记录。
//
// 它走的是与题图**完全相同**的那条采集路径 —— 拉正与内容寻址落盘都在采集那边，这里只把
// 落点从「新建一道错题」换成「这道题的 answer_hash」（spec：答案图与题图同一条路径，只是落点不同）。
//
// 角点坐标是**相对传入这张图**的像素坐标，顺序固定为 左上 → 右上 → 右下 → 左下（见 Quad）。
// 重拍答案图就是再调一次：换下来的旧图若已无人引用会被回收。
func (s *Service) AttachAnswer(id int64, frameBase64 string, quad Quad) (Question, error) {
	if s.files == nil {
		return Question{}, errors.New("题库: 没有接上图片文件层，补拍答案图不可用")
	}

	// 先确认这道题在，再动盘。顺序反过来的话，id 不存在时会在盘上留下一张
	// 没有任何错题引用的答案图。
	q, err := s.store.GetQuestion(id)
	if err != nil {
		return Question{}, err
	}

	raw, err := base64.StdEncoding.DecodeString(frameBase64)
	if err != nil {
		return Question{}, fmt.Errorf("题库: 帧不是合法的 base64: %w", err)
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return Question{}, fmt.Errorf("题库: 帧解不出图像: %w", err)
	}

	hash, err := s.files.RectifyAndSave(img, quad)
	if err != nil {
		return Question{}, err
	}

	updated, err := s.store.SetAnswerHash(id, hash)
	if err != nil {
		// 没写成，刚落的这张图就没人引用了 —— 用 reclaimImage 而不是直接删，是因为
		// 内容寻址下它可能早就被别的错题引用着。
		s.reclaimImage(hash)
		return Question{}, err
	}

	if q.AnswerHash != "" && q.AnswerHash != hash {
		s.reclaimImage(q.AnswerHash)
	}
	return updated, nil
}

// Get 按 id 取一道错题。
func (s *Service) Get(id int64) (Question, error) {
	return s.store.GetQuestion(id)
}

// List 列出全部错题，新的在前。
func (s *Service) List() ([]Question, error) {
	return s.store.ListQuestions()
}

// Delete 删掉一道错题，顺带回收它的题图与答案图，并返回被删掉的那条记录。
//
// 回收是引用计数式的：同一个 hash 可能还被别的错题引用着（去重发生在文件那一层，
// 见 ADR-0004），所以只有数到没人用了才真的删文件。
//
// 没接上 ImageFiles 时只删库里的行，图片留在盘上 —— 那是些没人引用的孤儿。
func (s *Service) Delete(id int64) (Question, error) {
	q, err := s.store.DeleteQuestion(id)
	if err != nil {
		return Question{}, err
	}
	// 行删掉之后再回收图片：反过来的话，写库失败就把还在被引用的图删了。
	s.reclaimImage(q.QuestionHash)
	s.reclaimImage(q.AnswerHash)
	return q, nil
}

// reclaimImage 在某个 hash 已经没有任何错题引用时，把它对应的图片文件删掉。
//
// 去重是内容级的：同一个 hash 完全可能既是一道题的题图、又是另一道题的答案图，
// 所以删文件之前必须回库问一句（CountQuestionsWithHash）——
// 「两道题拍了同一张图，删掉一道不能把另一道也弄瞎」。
//
// 清不掉不算失败：库那一侧已经改完了，剩下一个没人引用的文件不值得让整个操作失败
// （导出时按库里的引用去打包，孤儿不会进包）。
func (s *Service) reclaimImage(hash string) {
	if s.files == nil || hash == "" {
		return
	}
	n, err := s.store.CountQuestionsWithHash(hash)
	if err != nil || n > 0 {
		return
	}
	_ = s.files.RemoveByHash(hash)
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
