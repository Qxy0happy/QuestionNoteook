package tags

import (
	"errors"
	"fmt"
	"strings"

	"questionbook/internal/library"
)

// Service 是「标签」对前端暴露的那一面，也是本包唯一的测试缝（见 spec）。
//
// 它持有一条错题库连接（标签与错题同一份库文件）与题库本身 —— 按标签筛错题要读
// questions 表，那条查询在 library 那边（ListQuestionsTaggedWith）。
//
// 这里**没有「移动节点」**：层级由父节点推出来，移动一个章节等于连同它整棵子树改层级
// （或让层级与父子关系脱节），而这两种都不是现在需要的动作。要挪就删了重建。
type Service struct {
	tags      *Store
	questions *library.Store
}

// NewService 用一个已经开好的错题库构造标签服务。
//
// 标签用的连接就是题库那一条（ADR-0004 只把**图片**放到库外，元数据都在这一份 SQLite 里），
// 不再 Open 第二个到同一文件的连接：WAL 下能跑，但没有理由让两个池抢同一把写锁。
func NewService(questions *library.Store) *Service {
	return &Service{tags: NewStore(questions.DB()), questions: questions}
}

// List 返回全部标签，平铺。前端按 ParentID 自己拼树。
func (s *Service) List() ([]Tag, error) { return s.tags.List() }

// Create 在 parentID 下建一个标签，返回落库后的它。
//
// parentID 传 0 表示建**顶层学科** —— 顶层由用户自由填写，本包只在新库里预置考研四门
// 作初始值（迁移 2），预置的那些与用户后建的没有任何区别：可以改名，也可以删。
//
// 层级由父节点推出来，不用调用方传：父是学科就只能建章节。父是知识点时返回 ErrMaxDepth，
// 同级重名返回 ErrDuplicate，父不存在返回 ErrNotFound。
func (s *Service) Create(parentID int64, name string) (Tag, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Tag{}, ErrInvalidName
	}

	level := LevelSubject
	if parentID != 0 {
		parent, err := s.tags.Get(parentID)
		if errors.Is(err, ErrNotFound) {
			// 说清是**父节点**不存在：调用方是在建子节点，光说「标签不存在」指代不明。
			return Tag{}, fmt.Errorf("%w: 父节点 id=%d", ErrNotFound, parentID)
		}
		if err != nil {
			return Tag{}, err
		}
		if parent.Level >= MaxLevel {
			return Tag{}, fmt.Errorf("%w: 父节点是%s", ErrMaxDepth, parent.Level)
		}
		level = parent.Level + 1
	}
	return s.tags.insert(parentID, name, level)
}

// Rename 改一个标签的名字，返回改完的它。id 不存在时返回 ErrNotFound。
//
// 只改名字，不动它在树里的位置与层级 —— 见 Service 的注释（本包不做移动）。
func (s *Service) Rename(id int64, name string) (Tag, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Tag{}, ErrInvalidName
	}
	return s.tags.rename(id, name)
}

// Delete 删掉一个标签**连同它的整棵子树**，返回波及面。id 不存在时返回 ErrNotFound。
//
// 为什么是级联，而不是把子节点提到被删节点的位置：三层是**语义**层级，不只是结构深度 ——
// 「章节」的含义是「学科下的一块」，「知识点」是「章节里的一条」。把子节点提上来，等于让
// 一条知识点去冒充章节（名字照旧，含义变了），层级与内容从此对不上；而且父节点下往往还有
// 别的子节点，提上来的那批会挤进一个它们从没属于过的集合。反过来，「没有章节的学科」
// 或「没有知识点的章节」是**合法**的中间状态（先搭骨架再往里填），所以级联不会留下
// 不合法的树。
//
// 代价是这一步是破坏性的：错题上挂的这些标签一并摘掉（错题本身不动），
// 所以返回值里带上波及面，界面据此先问一句再删。
func (s *Service) Delete(id int64) (DeleteResult, error) { return s.tags.delete(id) }

// TagsOfQuestion 返回一道错题挂着的标签，按层级排。
//
// 题不存在时返回空集而不是报错：这是个读，界面拿一个刚被删掉的 id 来问是正常的，
// 看到「没有标签」就够了。写操作相反 —— 见 SetQuestionTags。
func (s *Service) TagsOfQuestion(questionID int64) ([]Tag, error) {
	return s.tags.tagsOfQuestion(questionID)
}

// TagsOfQuestions 一次取多道错题的标签，顺序与传入的 ids 一致（重复的会去掉）。
//
// 列表页要为屏上每一行显示标签，一道题一次调用太浪费。
// 没挂标签的题也会出现在结果里（Tags 为空切片），界面不必自己对齐。
func (s *Service) TagsOfQuestions(questionIDs []int64) ([]QuestionTags, error) {
	return s.tags.tagsOfQuestions(questionIDs)
}

// SetQuestionTags 把一道错题挂着的标签**整体替换**成 tagIDs（空集 = 摘掉全部）。
//
// 手工打标签在这一层就是一次替换：界面上是勾选，保存时把勾选结果整份交上来 ——
// 比在前后端各维护一份增量稳妥。传入重复的 id 会被去重。
//
// 题不存在返回 library.ErrNotFound，某个标签不存在返回 ErrNotFound；两种情况都不落任何写入。
func (s *Service) SetQuestionTags(questionID int64, tagIDs []int64) error {
	if err := s.checkQuestion(questionID); err != nil {
		return err
	}
	// 先全验一遍再落库：半路发现某个标签不存在就整份不生效，别留下改了一半的标签集。
	uniq, err := s.knownTags(tagIDs)
	if err != nil {
		return err
	}
	return s.tags.setQuestionTags(questionID, uniq)
}

// AddQuestionTag 给一道错题补挂一个标签。已经挂过就什么也不做（幂等）。
func (s *Service) AddQuestionTag(questionID, tagID int64) error {
	if err := s.checkQuestion(questionID); err != nil {
		return err
	}
	if _, err := s.tags.Get(tagID); err != nil {
		return err
	}
	return s.tags.addQuestionTag(questionID, tagID)
}

// RemoveQuestionTag 从一道错题上摘掉一个标签。本来就没挂也什么也不做（幂等）。
func (s *Service) RemoveQuestionTag(questionID, tagID int64) error {
	if err := s.checkQuestion(questionID); err != nil {
		return err
	}
	if _, err := s.tags.Get(tagID); err != nil {
		return err
	}
	return s.tags.removeQuestionTag(questionID, tagID)
}

// QuestionsByTags 按标签筛错题，新的在前；多个标签取**并集**。
//
// 筛选入口挂在标签服务上而不是题库服务上：它读的是 questions 表，但规则是标签树的 ——
// 「点一个学科要带出它下面所有章节与知识点的题」属于树（见 library.Store.ListQuestionsTaggedWith）。
//
// tagIDs 为空表示**不筛**，返回全部错题：界面上「一个标签都没勾」就是这个意思，
// 于是筛与不筛是同一个调用，接线时不必分两条路。
func (s *Service) QuestionsByTags(tagIDs []int64) ([]library.Question, error) {
	return s.questions.ListQuestionsTaggedWith(tagIDs)
}

// checkQuestion 确认这道错题存在。
//
// 写之前先查一次：拿一个不存在的 id 来写，应当收到 ErrNotFound，
// 而不是一个「悄悄什么都没发生」的成功。
func (s *Service) checkQuestion(id int64) error {
	_, err := s.questions.GetQuestion(id)
	return err
}

// knownTags 确认这些标签都存在，并顺手去重（保持首次出现的顺序）。
//
// 一个个查是刻意的：这里的量级是「用户在界面上勾了几个标签」，几十个封顶，
// 不值得为它写一条动态 IN 查询。
func (s *Service) knownTags(ids []int64) ([]int64, error) {
	uniq := make([]int64, 0, len(ids))
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if _, err := s.tags.Get(id); err != nil {
			return nil, err
		}
		uniq = append(uniq, id)
	}
	return uniq, nil
}
