package agent

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Action 是一条待批准改动要干的事。
//
// 取值就是 pending_changes.action 上那条 CHECK 约束写死的那几个字符串（迁移 5）——
// 两边是一份契约的两半，改一处就得改另一处。库那侧也拦一道的意思很实在：
// **别的种类的改动根本存不进来**，所以「agent 只能提议标签」不只是这一层写没写的问题。
//
// 四样全是标签，因为 v1 的写范围就是标签（票据 11 钉的）。复习状态与删错题不在这里 ——
// 不是还没做，是明确不做。
type Action string

const (
	ActionCreateTag   Action = "create_tag"   // 在某个父节点下新建一个标签
	ActionRenameTag   Action = "rename_tag"   // 给一个标签改名（不动它在树里的位置）
	ActionDeleteTag   Action = "delete_tag"   // 删一个标签，连同它的整棵子树
	ActionTagQuestion Action = "tag_question" // 把一道错题挂着的标签**整体换成**另一组
)

// AllActions 返回全部动作。
//
// 它是「这个动作认不认识」的白名单，也是测试的清单来源：顺手加一个新动作时，
// 下面 Validate 的 switch 与安全测试里那份清单会有一处对不上，编译或测试就红了 ——
// 这正是想要的，因为加动作的同时必须回头看 Applier 那边认不认它。
func AllActions() []Action {
	return []Action{ActionCreateTag, ActionRenameTag, ActionDeleteTag, ActionTagQuestion}
}

// valid 报告这个动作是不是本包认识的那几个。
func (a Action) valid() bool {
	for _, known := range AllActions() {
		if a == known {
			return true
		}
	}
	return false
}

// Status 是一条待批准改动的状态。
//
// 只有三种，也就是 pending_changes.status 上 CHECK 的那三个：
//
//	pending    等用户点头。**批准失败之后仍然停在它**（见 Pending.Approve）。
//	applied    已经写进正式数据了。
//	discarded  用户不要它。
type Status string

const (
	StatusPending   Status = "pending"
	StatusApplied   Status = "applied"
	StatusDiscarded Status = "discarded"
)

// String 返回状态的中文名，用在给人看的报错里。
func (s Status) String() string {
	switch s {
	case StatusPending:
		return "待批准"
	case StatusApplied:
		return "已生效"
	case StatusDiscarded:
		return "已丢弃"
	}
	return string(s)
}

// Payload 是一条改动的载荷 —— 干这件事需要的那几个参数。
//
// 一个结构体罩住四种动作，而不是每种动作一个类型：它要能原样存成 pending_changes.payload
// 里的一段 JSON（迁移 5 的注释：拆成一堆列就会长出一片「这条改动用不上」的空列）。
// 哪个动作用得上哪几个字段，由 Validate 说清楚。
type Payload struct {
	// ParentID 是新标签的父节点。0 表示顶层学科（库里存 NULL）—— 与标签包同一个约定。
	ParentID int64 `json:"parent_id,omitempty"`

	// TagID 是被改 / 被删的那个标签。
	TagID int64 `json:"tag_id,omitempty"`

	// QuestionID 是被打标签的那道错题。
	QuestionID int64 `json:"question_id,omitempty"`

	// Name 是标签名（新建、改名用）。
	Name string `json:"name,omitempty"`

	// TagIDs 是 tag_question 要换上的那组标签。**整体替换**，空集表示摘掉全部 ——
	// 与 tags.SetQuestionTags 同一个语义（界面上是勾选，保存时把勾选结果整份交上来）。
	//
	// 刻意**没有 omitempty**：空集与「没说」是两件事，而这条改动里它们的意思差得很远
	// （摘掉全部 vs 参数缺了）。payload 存成 JSON 再读回来，omitempty 会把前者变成后者，
	// 于是用户点「批准」时会撞上一句莫名其妙的「缺 tag_ids」。
	TagIDs []int64 `json:"tag_ids"`
}

// Change 是**一条待批准改动的内容**：要干什么、拿什么参数干、以及两句给人看的话。
//
// 它还没有身份（没有 id）也还没有状态 —— 那是落库之后才有的事，见 PendingChange。
// 它同时也是 Applier 唯一的输入：写数据的那一步只认这个类型。
type Change struct {
	Action  Action
	Payload Payload

	// Summary 是一句给人看的中文（「把「中值定理」改名为「微分中值定理」」）。
	//
	// 它由**我们**在提议时写好，不是模型的原话。这是有意的：用户点头之前读到的
	// 应当是一句陈述事实的话（改哪个、波及多少），而不是模型的劝说。模型只提供
	// 动作、参数与 Reason。
	Summary string

	// Reason 是模型自己交代的为什么这么改。它是给人参考的，不参与任何校验。
	Reason string
}

// Validate 检查一条改动是不是本包认识的样子。
//
// 三处会调它，而且三处都要：工具那一层（模型给错了立刻把话喂回去让它重来）、
// 落库之前（脏东西不进 pending_changes）、写数据之前（批准的那一刻再验一次 ——
// 行可能是几天前写的，那时的世界与现在不是同一个）。
func (c Change) Validate() error {
	if !c.Action.valid() {
		return fmt.Errorf("%w: 不认识的动作 %q", ErrBadChange, c.Action)
	}
	if strings.TrimSpace(c.Summary) == "" {
		// 摘要不是装饰：用户就是照着它点头的，没有它这条改动等于没说明白要干什么。
		return fmt.Errorf("%w: 每条改动都得有一句给人看的摘要", ErrBadChange)
	}
	if c.Payload.ParentID < 0 || c.Payload.TagID < 0 || c.Payload.QuestionID < 0 {
		return fmt.Errorf("%w: id 不能是负数", ErrBadChange)
	}

	switch c.Action {
	case ActionCreateTag:
		if strings.TrimSpace(c.Payload.Name) == "" {
			return fmt.Errorf("%w: 新建标签得有个名字", ErrBadChange)
		}
	case ActionRenameTag:
		if c.Payload.TagID == 0 {
			return fmt.Errorf("%w: 改名得说清改的是哪个标签（tag_id）", ErrBadChange)
		}
		if strings.TrimSpace(c.Payload.Name) == "" {
			return fmt.Errorf("%w: 改名得给个新名字", ErrBadChange)
		}
	case ActionDeleteTag:
		if c.Payload.TagID == 0 {
			return fmt.Errorf("%w: 删除得说清删的是哪个标签（tag_id）", ErrBadChange)
		}
	case ActionTagQuestion:
		if c.Payload.QuestionID == 0 {
			return fmt.Errorf("%w: 打标签得说清打在哪道题上（question_id）", ErrBadChange)
		}
		if c.Payload.TagIDs == nil {
			// 空集是合法的（「把这道题的标签全摘掉」），nil 不是（模型多半是忘了写）。
			return fmt.Errorf("%w: 打标签得给一组 tag_ids（可以是空的，那表示摘掉全部）", ErrBadChange)
		}
	}
	return nil
}

// PendingChange 是一条**已经记下来**的待批准改动：内容 + 身份 + 状态。
//
// 为什么不直接把 Change 嵌进来：这个类型是给前端的（Wails 生成的 TS 模型），
// 嵌一层就多一层要拆的包装。字段重了四个，值那点重复。
type PendingChange struct {
	ID     int64
	Action Action
	// Payload 是这条改动的参数。JSON 往返之后 tag_ids 会变成 null 而不是缺项 ——
	// 见 Payload.TagIDs 上那段说明。
	Payload Payload
	Summary string
	Reason  string

	Status Status

	// CreatedAt 是 agent 提议的那一刻，DecidedAt 是用户点头 / 丢弃的那一刻
	// （零值表示还没决定）。都是 UTC，与库里其它时间戳同一个约定。
	CreatedAt time.Time
	DecidedAt time.Time

	// Problem 非空表示**上一次批准没成功**，这里是原因（比如「标签不存在」——
	// 行写得久了，那个标签可能已经被删了）。此时 Status 仍是 pending：
	// 那一步确实没发生，不能因为试过一次就把它标成已生效。
	Problem string
}

// Change 取回这条改动的内容，交给 Applier 用。
func (p PendingChange) Change() Change {
	return Change{Action: p.Action, Payload: p.Payload, Summary: p.Summary, Reason: p.Reason}
}

var (
	// ErrBadChange 表示一条改动不合本包的口径（动作不认识、参数缺了、没有摘要）。
	// 判断用 errors.Is。
	ErrBadChange = errors.New("这条改动不合口径")

	// ErrNotPending 表示这条改动已经做过决定了（生效了或丢掉了），不能再批一次、再丢一次。
	ErrNotPending = errors.New("这条改动已经决定过了")

	// ErrNotFound 表示没有这个 id 的待批准改动。
	ErrNotFound = errors.New("待批准改动不存在")

	// ErrEmptyQuestion 表示问了一句空的。
	ErrEmptyQuestion = errors.New("要问 agent 的话是空的")
)
