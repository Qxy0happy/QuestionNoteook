package tags

import (
	"errors"
	"fmt"
	"time"
)

// Level 是标签的层级。固定三层：学科 > 章节 > 知识点。
//
// 层级**存进库里**（tags.level）而不是每次爬上树现算。理由是固定三层是这套模型的硬约束：
// 写成列，schema 层（CHECK）与服务层就都能拦住「知识点下面再挂一个」；按层级排序、
// 取子树也都是顺手的事。代价是 level 必须与 parent_id 保持一致 —— 本包不提供「移动节点」，
// 所以这个一致性不会被破坏（见 Service 的注释）。
type Level int

const (
	LevelSubject Level = 1 // 学科
	LevelChapter Level = 2 // 章节
	LevelPoint   Level = 3 // 知识点
)

// MaxLevel 是树的最大深度，也就是知识点那一层。
const MaxLevel = LevelPoint

// String 返回层级的中文名，用在报错与日志里。
func (l Level) String() string {
	switch l {
	case LevelSubject:
		return "学科"
	case LevelChapter:
		return "章节"
	case LevelPoint:
		return "知识点"
	}
	return fmt.Sprintf("第 %d 层", int(l))
}

// Tag 是树上的一个标签。
type Tag struct {
	ID        int64     // 落库时分配；Create 之前是 0
	ParentID  int64     // 父标签的 id；0 表示顶层学科（库里存 NULL）
	Name      string    // 标签名，由用户自由填写
	Level     Level     // 层级，由父节点推出来
	CreatedAt time.Time // 创建时间，落库时截到毫秒（UTC）
}

// QuestionTags 是一道错题挂着的全部标签。批量取时用它把结果带回前端。
type QuestionTags struct {
	QuestionID int64
	Tags       []Tag // 空切片而不是 nil：前端要拿到 []，不是 null
}

// DeleteResult 报告一次删除的波及面。
//
// 删中间节点是**级联**的（见 Service.Delete），点一个节点删掉的东西可能不止一个 ——
// 返回值就是用来在界面上把这件事说清楚的：「将一并删掉 2 个子标签、从 3 道错题上摘掉」。
type DeleteResult struct {
	DeletedTags       int // 被删掉的标签数，含自身与全部子孙
	UntaggedQuestions int // 有多少道错题因此少了至少一个标签（去重）
}

var (
	// ErrNotFound 表示库里没有这个标签。判断用 errors.Is。
	ErrNotFound = errors.New("标签不存在")
	// ErrDuplicate 表示同一个父节点下已经有同名标签。
	ErrDuplicate = errors.New("同级下已有同名标签")
	// ErrMaxDepth 表示父节点已经在第三层，下面不能再挂。
	ErrMaxDepth = errors.New("标签只有三层")
	// ErrInvalidName 表示标签名是空的（或只有空白）。
	ErrInvalidName = errors.New("标签名不能为空")
)
