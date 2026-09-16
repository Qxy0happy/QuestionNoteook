// Package apply 是**唯一**把一条待批准改动写进正式数据的地方。
//
// 它单独占一个包，不放进 internal/agent，是这一层设计里最要紧的一件事：
//
//   - 它是全应用里唯一持有标签服务（写的那一面）的东西。
//   - 它的输入只有一种东西：agent.Change。没有「谁提的」「哪个模型说的」这种东西 ——
//     改动从哪来它不关心，它只负责执行。
//   - 它由待批准服务（agent.Pending）持有，而 **agent 包连 import 它都 import 不到**
//     （那一条有架构守卫测试钉着）。
//
// 于是「绕过待批准层直接改数据」在这套类型里不是被禁止的，是**不存在**的：
// 没有第二个持有 Applier 的东西，也没有第二条能把 Change 送进 Applier 的路。
// 要绕过去，唯一的办法是新建一个包去 import tags 服务自己写 —— 而那是一个肉眼可见的、
// 需要新文件的动作，不是「顺手加一行」。
//
// 为什么它可以放心执行级联删除（删一个章节会带走它的知识点、并摘掉错题上的这些标签）：
// 那条提议落库之前，agent 那一层已经把波及面写进了 Summary（删几个子标签、从几道题上摘），
// 用户是**读着那句话**点的头。执行者只要照着做，不必自己再判一次该不该。
package apply

import (
	"fmt"

	"questionbook/internal/agent"
	"questionbook/internal/tags"
)

// Applier 把一条改动变成标签树上的一次真实变更。
type Applier struct {
	// tags 是标签服务 —— 本应用里改标签树的唯一入口。别的包想改标签，走的也是它。
	tags *tags.Service
}

// New 接上标签服务。它是这个包的唯一构造入口。
func New(tagSvc *tags.Service) *Applier { return &Applier{tags: tagSvc} }

// Apply 执行一条改动。执行不了时返回的错会原样变成界面上那句「为什么没批成」，
// 所以它就写成人话（标签服务与题库的报错本来就是中文的）。
//
// 执行之前**再验一次口径**：这一行可能是几天前写的，那时的世界与现在不是同一个
// —— 标签可能已经被删了、错题可能已经被删了。校验管的是「这条改动说的东西还成立吗」，
// 实际的删除与改名由标签服务自己判（它那边也要确认 id 还在）。
func (a *Applier) Apply(c agent.Change) error {
	if err := c.Validate(); err != nil {
		return err
	}

	switch c.Action {
	case agent.ActionCreateTag:
		_, err := a.tags.Create(c.Payload.ParentID, c.Payload.Name)
		return err

	case agent.ActionRenameTag:
		_, err := a.tags.Rename(c.Payload.TagID, c.Payload.Name)
		return err

	case agent.ActionDeleteTag:
		// 级联删子树是标签服务本来就有的行为（见 tags.Service.Delete 上那段说明），
		// 这里不另做一遍 —— 换一种删法是另一条规则，不该由一个执行者偷偷决定。
		_, err := a.tags.Delete(c.Payload.TagID)
		return err

	case agent.ActionTagQuestion:
		// **整体替换**：这与手工打标签、与 SaveTags 是同一个语义（界面上是勾选，
		// 保存时整份交上来），所以一条改动执行完的结果是确定的，与执行几次无关。
		return a.tags.SetQuestionTags(c.Payload.QuestionID, c.Payload.TagIDs)
	}

	// 走到这里说明 agent.Change 的口径变宽了而这里没跟上。上面那句 Validate 本可以拦住
	// 它（会返回 ErrBadChange），但留一句自己的话：将来真加了动作，报错要指向**执行者**，
	// 而不是说「这条改动不合口径」——后者会让人以为是提议那边写错了。
	return fmt.Errorf("标签执行者不认识动作 %q", c.Action)
}
