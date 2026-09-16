// Package discussion 是「就一道错题跟 VLM 聊」：把讨论记录挂在错题上落库，
// 再把每一轮交给模型。
//
// 记录与错题、标签、复习状态同在**同一份** SQLite 里（ADR-0004 只把图片放到库外），
// 表由 library 的迁移机制建出来（迁移 4），连接也从它那里借（library.Store.DB）——
// 不另开第二个到同一文件的连接。
//
// 跟模型说话**不在这里**：本包只认 Asker 那一个接口，实现落在 internal/vlm
// （配置、题图的读法、hash → file_id 的上传缓存都在那边，见 vlm.Service.Ask）。
// 测试里塞一个假的 provider 进去，整条流程就能跑完，**一次网络都不打**。
//
// 测试缝只有服务层（spec）：测试直接 new 出 Service 调它的方法，不启动 Wails。
// 因此本包的 SQL 都不带业务判断，校验与编排全在 Service 里。
package discussion

import (
	"errors"
	"time"

	"questionbook/internal/vlm"
)

// Role 是一条记录是谁说的。
//
// 取值与 vlm.Role 逐字相同（user / assistant）：它要原样进 vlm.Message、也要原样进
// 库里的 role 列，中间不该隔一张对照表。所以下面两个常量**由 vlm 的那两个值定义** ——
// 哪边改了，这边编译就过不去，两个包不可能悄悄跑偏。
type Role string

const (
	RoleUser      Role = Role(vlm.RoleUser)
	RoleAssistant Role = Role(vlm.RoleAssistant)
)

// Message 是讨论里的一条记录。
type Message struct {
	// ID 落库时分配。它同时是同一毫秒内两条记录的先后依据（见 Store.history）。
	ID int64
	// QuestionID 是它挂在哪道错题上。
	QuestionID int64
	// Role 是谁说的。
	Role Role
	// Text 是说了什么。
	Text string
	// CreatedAt 是这句话的时刻，落库时截到毫秒（UTC）。
	CreatedAt time.Time
}

// Turn 是刚过去的一轮问答：用户问的那句，和模型答的那句。
//
// 两条都已经落库了 —— 带回来只是省得界面再回库读一次。
type Turn struct {
	Question Message // 用户这一句
	Reply    Message // 模型的回答
}

// Asker 是「把这轮对话交给 VLM」的那条接缝。
//
// 形状正落在 vlm.Service.Ask 上：那边持有配置、题图的读法、以及「同一张图只传一次」的
// 上传缓存 —— 讨论每一轮都要带上题图（与答案图，如果有），那份缓存必须与打标签那条路
// 是同一份。本包不碰那些，也不该碰。
//
// 做成接口是为了测试能塞一个假的进来（与 vlm 那边的 Provider 同一个用途），
// 顺带让本包只依赖那一层对外的形状，而不依赖它的实现。
type Asker interface {
	// Ask 就 questionID 这道题把 history 发出去，返回模型的回答。
	// history 的最后一条必须是要问的那句（用户消息）。
	Ask(questionID int64, history []vlm.Message) (vlm.Reply, error)
}

var (
	// ErrEmptyMessage 表示这次要问的是空的（全是空白也算）。
	//
	// 空话不落库：界面上它也发不出去，存进去只会在讨论里留一条点不动的空行。
	ErrEmptyMessage = errors.New("讨论: 要问的话是空的")

	// ErrEmptyReply 表示模型答了，但答的是空的。
	//
	// 与「调用失败」分开报：那不是没打通，是它没说话。空回答同样不落库，
	// 于是讨论里留下一条没有回应的用户消息 —— 那是**如实**的（他确实问了，
	// 而模型确实没答上来），再问一次即可。
	ErrEmptyReply = errors.New("讨论: 模型没有回答")
)
