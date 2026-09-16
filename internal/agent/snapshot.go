package agent

import (
	"fmt"
	"strings"

	"questionbook/internal/tags"
)

// snapshot 是**一次提问**里的那份数据快照，第一次要谁才去读谁，读完就记住。
//
// 为什么要它：工具是模型挑的，一次提问里它完全可能连着列三次题、翻两页。
// 每次调用都去读一遍整本错题本，是同一份数据读三遍 —— 而这份数据在一次提问的几秒钟里
// 根本不会变。所以按「一次提问 = 一份快照」缓存，而不是按「一次工具调用」读一次。
//
// 它**不落任何东西**，也不持有数据库句柄：读还是要走 Reader，这里只是个记住结果的地方。
type snapshot struct {
	reader Reader

	tags     []tags.Tag
	tagsErr  error
	tagsDone bool

	questions     []QuestionView
	questionsErr  error
	questionsDone bool

	stats     []TagStats
	statsErr  error
	statsDone bool

	// 下面是上面几份数据摊出来的索引，都是第一次用到时才建。
	byID     map[int64]tags.Tag
	children map[int64][]int64
	paths    map[int64]string
}

func newSnapshot(r Reader) *snapshot { return &snapshot{reader: r} }

// tags 取全部标签（一次提问里只读一遍）。
//
// 读失败会**被记住**：一次提问里读坏一次就是坏了，再问一次库不会好，
// 只会让模型把同一个失败重试三遍。
func (s *snapshot) tagsList() ([]tags.Tag, error) {
	if !s.tagsDone {
		s.tagsDone = true
		s.tags, s.tagsErr = s.reader.ListTags()
	}
	return s.tags, s.tagsErr
}

// questionsList 取全部错题。
func (s *snapshot) questionsList() ([]QuestionView, error) {
	if !s.questionsDone {
		s.questionsDone = true
		s.questions, s.questionsErr = s.reader.Questions()
	}
	return s.questions, s.questionsErr
}

// statsList 取按标签汇总的复习情况（store 那侧已经按子树上卷过）。
func (s *snapshot) statsList() ([]TagStats, error) {
	if !s.statsDone {
		s.statsDone = true
		s.stats, s.statsErr = s.reader.ReviewStats()
	}
	return s.stats, s.statsErr
}

// index 建出「id → 标签」与「父 → 子」两张索引。
func (s *snapshot) index() (map[int64]tags.Tag, map[int64][]int64, error) {
	if s.byID != nil {
		return s.byID, s.children, nil
	}
	all, err := s.tagsList()
	if err != nil {
		return nil, nil, err
	}
	s.byID = make(map[int64]tags.Tag, len(all))
	s.children = make(map[int64][]int64, len(all))
	for _, t := range all {
		s.byID[t.ID] = t
		s.children[t.ParentID] = append(s.children[t.ParentID], t.ID)
	}
	return s.byID, s.children, nil
}

// tagByID 找一个标签。
func (s *snapshot) tagByID(id int64) (tags.Tag, bool, error) {
	byID, _, err := s.index()
	if err != nil {
		return tags.Tag{}, false, err
	}
	t, ok := byID[id]
	return t, ok, nil
}

// path 返回一个标签的整条路径（「数学 / 概率论 / 全概率公式」）。
//
// 为什么要路径而不是只要名字：不同学科下同名是合法的（「数学 > 概率论」与
// 「专业课 > 概率论」是两条不同的知识点），光给名字模型分不清说的是哪一个。
//
// 找不到的 id 返回空串（上面那段循环也顺手防了环：真出现环时最多走 8 步就停）。
func (s *snapshot) path(id int64) string {
	if s.paths == nil {
		s.paths = map[int64]string{}
	}
	if p, ok := s.paths[id]; ok {
		return p
	}
	byID, _, err := s.index()
	if err != nil {
		return ""
	}

	var names []string
	for at, steps := id, 0; at != 0 && steps < 8; steps++ {
		t, ok := byID[at]
		if !ok {
			break
		}
		names = append(names, t.Name)
		at = t.ParentID
	}
	// 从根往下拼：上面是往上走的，名字是倒着的。
	for i, j := 0, len(names)-1; i < j; i, j = i+1, j-1 {
		names[i], names[j] = names[j], names[i]
	}
	p := strings.Join(names, " / ")
	s.paths[id] = p
	return p
}

// subtree 返回一个标签连同它全部子孙的 id 集合。
func (s *snapshot) subtree(id int64) map[int64]bool {
	_, children, err := s.index()
	if err != nil {
		return map[int64]bool{id: true}
	}
	out := map[int64]bool{}
	queue := []int64{id}
	for len(queue) > 0 {
		at := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if out[at] {
			continue
		}
		out[at] = true
		queue = append(queue, children[at]...)
	}
	return out
}

// descendants 数一个标签下面有几层子孙（不含它自己）。
func (s *snapshot) descendants(id int64) int {
	_, children, err := s.index()
	if err != nil {
		return 0
	}
	n, queue := 0, children[id]
	for len(queue) > 0 {
		at := queue[0]
		queue = queue[1:]
		n++
		queue = append(queue, children[at]...)
	}
	return n
}

// questionByID 找一道错题。
func (s *snapshot) questionByID(id int64) (QuestionView, bool, error) {
	all, err := s.questionsList()
	if err != nil {
		return QuestionView{}, false, err
	}
	for _, q := range all {
		if q.ID == id {
			return q, true, nil
		}
	}
	return QuestionView{}, false, nil
}

// inSubtreeOfAny 报告这组标签里有没有一个落在 wanted 的某棵子树里。
func inSubtreeOfAny(have []tags.Tag, wanted map[int64]bool) bool {
	for _, t := range have {
		if wanted[t.ID] {
			return true
		}
	}
	return false
}

// errUnknownTag 造一句「这个标签不存在」的说明。
func errUnknownTag(id int64) error {
	return fmt.Errorf("没有 id=%d 这个标签（id 要用 list_tags 返回的）", id)
}

// errUnknownQuestion 造一句「这道错题不存在」的说明。
func errUnknownQuestion(id int64) error {
	return fmt.Errorf("没有 id=%d 这道错题（id 要用 list_questions 返回的）", id)
}
