package library

// Service 是「题库」对前端暴露的那一面。
//
// 和采集那边一样：这一层只做转接，增删查本身在 Store 里，所以不启动 Wails 也能测。
type Service struct {
	store *Store
}

// NewService 用一个已经开好的 Store 构造服务。
func NewService(store *Store) *Service {
	return &Service{store: store}
}

// Add 落一道新错题，返回落库后的记录（含分配到的 id 与创建时间）。
//
// 两个参数都是题图的内容 hash；answerHash 传空串表示还没拍答案图。
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
