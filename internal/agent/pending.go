package agent

import (
	"fmt"
	"time"
)

// Applier 把一条待批准改动写进正式数据。
//
// 这是**整个应用里唯一一个能写标签数据的地方**，而它的输入只有一种东西：Change。
// 它不认识模型、不认识工具、也不认识 http —— 一条改动从哪来它不关心，它只负责执行
// （实现里连一个「谁提的」都读不到，因为那个信息根本没进 Change）。
//
// 它由待批准服务持有（见 Pending），而 agent.Service 连这个类型都拿不到 ——
// 这就是「agent 写不了数据」那句话在类型上的完整含义。
type Applier interface {
	Apply(c Change) error
}

// PendingStore 是待批准改动的持久化层。
//
// 它只碰 pending_changes 这一张表（迁移 5），方法也只有这五个。这份方法集被
// safety_test.go 用反射钉死：往里加方法就得同时改那份白名单，而改白名单那一步是
// **故意**要人停下来想一想的 —— 「这一层该不该多一个动作」正是安全属性本身。
type PendingStore interface {
	// Propose 记下一条改动，返回落库后的它。at 是提议的时刻。
	Propose(c Change, at time.Time) (PendingChange, error)

	// List 返回**还没决定**的那些，早的在前。已经生效 / 已丢弃的不在里面
	// —— 它们的事已经办完了（生效的已经落在标签树上，丢弃的是用户不要了）。
	List() ([]PendingChange, error)

	// Count 数还没决定的那些。界面上那个角标要的就是它。
	Count() (int, error)

	// Get 按 id 取一条（不管什么状态）。
	Get(id int64) (PendingChange, error)

	// Decide 记下用户对一条改动的决定。
	//
	// status 为 StatusPending 时表示「批准没成功」，此时只写 problem，decided_at 不动
	// —— 那一步确实没有发生，不能因为试过一次就把它标成已生效。
	Decide(id int64, status Status, at time.Time, problem string) error
}

// Pending 是「待批准改动」这一面对前端暴露的样子。
//
// 它同时是那条**唯一的写路径**的守门人：Approve 是唯一会调到 Applier 的地方，
// 而 Applier 是唯一会写正式数据的地方。所以「绕过待批准层」在这套类型里不是被禁止的，
// 是**不存在**的 —— 没有第二个持有 Applier 的东西，也没有第二个能执行 Change 的东西。
//
// 它从不跟模型说话：模型建议什么由 agent.Service 收下，这个类型只管那些已经躺在
// pending_changes 里的行。两半各管一段，中间隔着用户的一次点击。
type Pending struct {
	store   PendingStore
	applier Applier

	// 取「现在」的方式。做成字段是为了能在测试里钉死：决定时刻要断言。
	now func() time.Time
}

// PendingOption 给待批准服务补一样可选能力。
type PendingOption func(*Pending)

// WithNow 换掉取当前时刻的方式。只有测试会用。
//
// 它换的是服务这一侧的时钟；store 那一侧的时刻由调用方传进去（见 PendingStore），
// 两边因此永远是同一个时刻。
func WithNow(now func() time.Time) PendingOption {
	return func(p *Pending) { p.now = now }
}

// NewPending 接上待批准改动的存储与那个唯一的执行者。
//
// 两个都得给，没有默认值：尤其 applier —— 一个没接上执行者的待批准服务是能列、
// 能数、但批不了的，那种半残的状态不该靠构造时的默认值悄悄成立。
func NewPending(store PendingStore, applier Applier, opts ...PendingOption) *Pending {
	p := &Pending{store: store, applier: applier, now: time.Now}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Propose 记下一条待批准改动。它是 Proposer 的实现，也就是 agent 那条路的出口。
//
// **它不改动任何正式数据**：写的只有 pending_changes 里的一行。
func (p *Pending) Propose(c Change) (PendingChange, error) {
	if err := c.Validate(); err != nil {
		return PendingChange{}, err
	}
	return p.store.Propose(c, p.now())
}

// List 返回还没决定的那些待批准改动，早的在前。
//
// 早的在前而不是新的在前：这是一份**积压的活**，要按用户提出的顺序一条条过。
// 前端要「新的在前」可以自己翻 —— 而服务这一侧给出的是它本来的顺序。
//
// 空切片而不是 nil：前端要拿到 []，不是 null。
func (p *Pending) List() ([]PendingChange, error) { return p.store.List() }

// Count 返回还没决定的条数。界面上那个「待批准 N 条」要的就是它。
func (p *Pending) Count() (int, error) { return p.store.Count() }

// Approve 批准一条改动：把它写进正式数据，然后标成已生效。
//
// 三种结果，区别很重要：
//
//   - 成功：返回的那条 Status 是 applied，正式数据已经改了。
//   - **执行失败**（标签已经被删了、错题已经不在了）：error 为 nil，返回的那条
//     Problem 里是原因、状态仍旧停在 pending。这不是「调用失败」，是「这一条现在做不了」
//     —— 用户该看到原因并且还能再试或者丢掉它。用 error 表达的话 Wails 会把返回值一起
//     丢掉，于是界面既不知道原因也拿不回那条记录（与 vlm.TagSuggestion.Problem 同一个理由）。
//   - 请求本身不成立（id 不存在、已经决定过了）：这才是 error。
//
// 已经生效 / 已丢弃的再批一次返回 ErrNotPending：不做幂等。
// 「已经生效了还批准一次」与「已经丢掉了又批准」是两件对不上的事，悄悄成功只会掩盖它。
func (p *Pending) Approve(id int64) (PendingChange, error) {
	pc, err := p.store.Get(id)
	if err != nil {
		return PendingChange{}, err
	}
	if pc.Status != StatusPending {
		return PendingChange{}, fmt.Errorf("%w: 它已经%s了", ErrNotPending, pc.Status)
	}

	if err := p.applier.Apply(pc.Change()); err != nil {
		problem := err.Error()
		if problem != pc.Problem {
			// 同一个原因不重复写库：用户连点几下不该换来几行一样的字。
			_ = p.store.Decide(id, StatusPending, time.Time{}, problem)
		}
		pc.Problem = problem
		return pc, nil
	}

	if err := p.store.Decide(id, StatusApplied, p.now(), ""); err != nil {
		return PendingChange{}, err
	}
	pc.Status = StatusApplied
	pc.DecidedAt = p.now()
	pc.Problem = ""
	return pc, nil
}

// Discard 丢掉一条改动：它不再出现在待批准列表里，正式数据一点没动。
//
// 丢掉一条**不影响**别的待批准改动，更不影响已经生效的那些 —— 每一条都是一个独立的
// 事务性决定，这里从头到尾只碰它自己那一行。
//
// 已经丢过的再丢一次是**幂等**的（返回那条记录，不报错）：界面上连点两下、或者列表
// 还没刷新时又点了一下，都不该看到一句错。
func (p *Pending) Discard(id int64) (PendingChange, error) {
	pc, err := p.store.Get(id)
	if err != nil {
		return PendingChange{}, err
	}
	switch pc.Status {
	case StatusDiscarded:
		return pc, nil
	case StatusApplied:
		// 已经生效了再丢掉：两件事对不上。明确报错，而不是把状态改回「丢弃」——
		// 那会让已经改了的数据与「这条改动被丢掉了」这句话自相矛盾。
		return PendingChange{}, fmt.Errorf("%w: 它已经%s了", ErrNotPending, pc.Status)
	}

	if err := p.store.Decide(id, StatusDiscarded, p.now(), ""); err != nil {
		return PendingChange{}, err
	}
	pc.Status = StatusDiscarded
	pc.DecidedAt = p.now()
	return pc, nil
}
