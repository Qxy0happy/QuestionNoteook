package agent

import (
	"context"
	"strings"
	"sync"

	"questionbook/internal/vlm"
)

// maxToolRounds 是「一轮提问里最多跟模型往返几次」的上限。
//
// 为什么必须有：工具循环是一个**模型控制的不动点**——它可能一直要数据、一直不回答，
// 每一轮都是一次真金白银的调用。没有上限就等于把「花多久、花多少」交给模型决定。
//
// 8 是怎么来的：正常的问题两三轮就收（列一次标签、列一次题、然后回答），
// 需要来回翻找的也是五六轮。8 留了余量，又不至于在模型卡住时烧掉一屏调用。
//
// 用尽之后发生什么见 roundsExhaustedNote：**把工具收走再问一次**，
// 让它拿现有信息作答并说明哪里不确定 —— 而不是把这轮的提议全丢掉。
const maxToolRounds = 8

// maxProposals 是一轮提问里最多提议几条改动。
//
// 与回合上限同一个理由：提议的次数也是模型控制的。20 条远超用户一天愿意逐条点头的量，
// 撞上它基本意味着模型在乱试 —— 那时候让它停下比让它继续更有用。
const maxProposals = 20

// Model 是 agent 跟模型说话要用的那**一个**动作。
//
// 刻意不直接用 vlm.Provider：那上面还有个 Upload（把图交上去）。agent 这条路上一张图都
// 没有 —— 工具调用的报文里不带图（官方文档里图片只能挂在 user 消息上，而工具结果是
// role:"tool" 的消息）—— 所以这一层**不需要**上传能力，也就不该拿着它。
// 接口小一号，「能做的事」就少一档。
type Model interface {
	Chat(ctx context.Context, req vlm.Request) (vlm.Reply, error)
}

// Proposer 收下一条改动，把它记成一条待批准改动。
//
// 这是本包**唯一**一条能改变世界状态的路，而它改的只是 pending_changes 那一张表：
// 正式数据一个字节都不动。真正的写数据在 Applier 那边，而持有 Applier 的是
// 待批准服务（Pending），不是这里 —— 这个包连 Applier 都 import 不到。
type Proposer interface {
	Propose(c Change) (PendingChange, error)
}

// Service 是「问 agent 一句话」那一面对前端暴露的样子，也是本包唯一的测试缝（见 spec）。
//
// 它手里的四样东西没有一样能写正式数据：reader 只读、proposer 只往待批准表里写、
// model 只会说话、configPath 是个路径。这正是包注释里那条性质的落点。
type Service struct {
	reader   Reader
	proposer Proposer

	// configPath 是那份 JSON 配置的落点（与 VLM 服务用的是同一份文件）。
	// 模型名必须从它来 —— ADR-0005：代码里一个默认模型 ID 都不许出现。
	configPath string

	// fixed 非空时永远用它，不看配置建的那个。测试与「没有 key 也想把界面点通」用它。
	fixed Model

	// maxRounds 是回合上限，默认 maxToolRounds。做成字段是为了让测试能把它压到 1、
	// 拿两轮就把「用尽之后会怎样」跑到。
	maxRounds int

	// 配置按需读、读一次就记住：与 vlm.Service 同一套（一个坏文件不该每次点一下重读一遍）。
	mu        sync.Mutex
	loaded    bool
	loadErr   error
	modelName string
	model     Model
}

// Option 给服务补一样可选能力。
type Option func(*Service)

// WithModel 换掉模型：测试里给假实现。
//
// 真实接线不传这一项 —— 模型按配置现建（见 current）。
func WithModel(m Model) Option { return func(s *Service) { s.fixed = m } }

// WithMaxRounds 换掉回合上限。只有测试会用。
func WithMaxRounds(n int) Option {
	return func(s *Service) {
		if n > 0 {
			s.maxRounds = n
		}
	}
}

// NewService 接上只读的数据入口、一个「记下待批准改动」的出口与配置文件的落点。
//
// **构造时不读盘**，与 vlm.Service 同一个理由：一个还没配过 VLM 的应用要能照常启动、
// 照常拍照入库，配置是「用的时候才需要」的东西。
func NewService(reader Reader, proposer Proposer, configPath string, opts ...Option) *Service {
	s := &Service{
		reader:     reader,
		proposer:   proposer,
		configPath: configPath,
		maxRounds:  maxToolRounds,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Answer 是「问一句」这一次的结果。
type Answer struct {
	// Question 是原样收下的那句话，好让界面把它显示成这一轮的问句。
	Question string

	// Text 是模型的回答。为空的情形见 Problem。
	Text string

	// Proposals 是这一轮提议出来的待批准改动，空切片而不是 nil（前端要拿到 []，不是 null）。
	//
	// 它们**已经在库里**了（提议那一刻就落了 pending_changes）。所以这一轮就算没答出话，
	// 用户也能在待批准列表里看到、逐条点头 —— 那份工作没有白做。
	Proposals []PendingChange

	// Rounds 是实际跟模型往返了几轮（含用尽之后那次不带工具的收尾）。
	Rounds int

	// Problem 非空表示这次没给出正常回答：回合用尽了，或者模型一句话都没说。
	//
	// 它是字段而不是 error，与 vlm.TagSuggestion.Problem 同一个理由：Wails 出错时会把
	// 返回值一起丢掉，而这里恰恰有东西要给用户看（已经提议出来的那几条改动）。
	// 真正的调用失败（没配置、网络、服务方报错）仍然是 error，走另一条路。
	Problem string
}

// Ask 问 agent 一句话：它自己去查数据，然后回答，或者提一条待批准改动。
//
// **只读 + 只提议**：这一步不会改动任何正式数据。它会往 pending_changes 里写行
// （那是「提议」这个动作本身），但那些行要等用户逐条点头才生效。
//
// 没配 VLM 返回 vlm.ErrNotConfigured；问的是空的返回 ErrEmptyQuestion。
func (s *Service) Ask(question string) (Answer, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return Answer{}, ErrEmptyQuestion
	}

	model, name, err := s.current()
	if err != nil {
		return Answer{}, err
	}

	r := &runner{
		model:     model,
		modelName: name,
		proposer:  s.proposer,
		snap:      newSnapshot(s.reader),
		// 非 nil 起步：一次没提议的提问也要给前端一个 []，而不是 null
		//（与 List、ReviewHistory 那边同一条规矩）。
		proposals: []PendingChange{},
	}
	text, problem, rounds, err := r.converse(context.Background(), question, s.roundLimit())
	if err != nil {
		return Answer{}, err
	}
	return Answer{
		Question:  question,
		Text:      text,
		Proposals: r.proposals,
		Rounds:    rounds,
		Problem:   problem,
	}, nil
}

// ── 内部 ──

// roundLimit 返回这次该用的回合上限。
func (s *Service) roundLimit() int {
	if s.maxRounds <= 0 {
		return maxToolRounds
	}
	return s.maxRounds
}

// current 返回这次该用的模型与模型名。第一次用到时才读盘（见 NewService）。
func (s *Service) current() (Model, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	err := s.ensureLocked()
	if s.fixed != nil {
		// 注入的模型是彻底顶替：配置读不到也照样跑（读到了就用它的模型名）。
		return s.fixed, s.modelName, nil
	}
	if err != nil {
		return nil, "", err
	}
	if s.model == nil {
		return nil, "", vlm.ErrNotConfigured
	}
	return s.model, s.modelName, nil
}

// ensureLocked 保证 s.model 与 s.modelName 已经就位，必要时读一次盘。
func (s *Service) ensureLocked() error {
	if s.loaded {
		return s.loadErr
	}
	s.loaded = true
	s.loadErr = s.readLocked()
	return s.loadErr
}

// readLocked 无条件读一次配置并据此重建模型。调用方必须已经持有 s.mu。
func (s *Service) readLocked() error {
	cfg, err := vlm.LoadConfig(s.configPath)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	s.modelName = agentModel(cfg)
	s.model = nil
	if s.fixed == nil {
		p, err := vlm.NewDeepSeek(cfg)
		if err != nil {
			return err
		}
		s.model = p
	}
	return nil
}

// agentModel 挑这次该用哪个模型名。
//
// 与 vlm.Config 里那条规则（带图走 VisionModel、不带图走 TextModel）的**偏差**在这儿：
// agent 这条路上一张图都没有，所以规则更简单 —— 优先 text_model，没配就回落到 vision_model。
//
// 为什么可以这么回落：官方能力矩阵里 deepseek-flash 一个模型同时支持 Tool Calls 与
// Vision（同一份定价页上写着），「文本模型」与「视觉模型」在这个服务方那边不是两个必须
// 分开的东西。而 text_model 在配置里本来就是可选的（json tag 上写着 omitempty）——
// 不回落的话，一个只配了 vision_model 的用户会发现 agent 整个用不了，而他配的那个模型
// 完全够用。
func agentModel(cfg vlm.Config) string {
	if strings.TrimSpace(cfg.TextModel) != "" {
		return cfg.TextModel
	}
	return cfg.VisionModel
}

// ── 工具调用循环 ──

// runner 是一轮提问的状态：模型、数据快照、以及这一轮提议出来的改动。
//
// 它是一次性的（Ask 里现造一个），所以不必加锁：整个循环跑在同一个 goroutine 里。
type runner struct {
	model     Model
	modelName string

	proposer Proposer
	snap     *snapshot

	proposals []PendingChange
}

// converse 跑到出答案为止：模型要数据就给数据，要到它不再要、或者回合用尽。
//
// 返回值是（回答正文、Problem、往返轮数、出错）。
//
// 循环的规矩照官方文档（见 vlm 包注释里那段契约复核）：模型要调工具那一轮，
// 先把**那条 assistant 消息原样** append 回去（带 tool_calls，也带上 reasoning），
// 再为每一次调用 append 一条 role:"tool" 的消息带 tool_call_id。少了 assistant 那条，
// 下一轮的 tool 结果就没有归属。
func (r *runner) converse(ctx context.Context, question string, maxRounds int) (string, string, int, error) {
	msgs := []vlm.Message{{Role: vlm.RoleUser, Text: question}}

	for round := 1; ; round++ {
		// 回合用尽：把工具**收走**再问最后一次。
		//
		// 为什么是「再问一次」而不是直接报错：手上已经有了一堆工具结果，模型完全可以
		// 据此作答；直接放弃等于把前面几轮的钱白花，用户还什么都看不到。Tools 为空是
		// 这一轮的全部意义 —— 它只能回答，不能再要数据。
		exhausted := round > maxRounds

		req := vlm.Request{
			Model:    r.modelName,
			System:   systemPrompt,
			Messages: msgs,
		}
		if exhausted {
			req.Messages = append(msgs, vlm.Message{Role: vlm.RoleUser, Text: roundsExhaustedNote})
		} else {
			req.Tools = toolDecls()
		}

		reply, err := r.model.Chat(ctx, req)
		if err != nil {
			return "", "", round, err
		}

		if exhausted {
			text, problem := wrap(reply.Text, roundsExhaustedProblem)
			return text, problem, round, nil
		}
		if len(reply.ToolCalls) == 0 {
			text, problem := wrap(reply.Text, "")
			return text, problem, round, nil
		}

		// 先 assistant 那条，再逐条工具结果。顺序是官方文档点名的，不能换。
		msgs = append(msgs, vlm.Message{
			Role:      vlm.RoleAssistant,
			Text:      reply.Text,
			ToolCalls: reply.ToolCalls,
			// 思考模式下那段推理过程要跟着一起回去，见 vlm.Message.Reasoning。
			Reasoning: reply.Reasoning,
		})
		for _, call := range reply.ToolCalls {
			msgs = append(msgs, vlm.Message{
				Role:       vlm.RoleTool,
				ToolCallID: call.ID,
				Text:       r.call(call),
			})
		}
	}
}

// wrap 定下这一轮的回答正文与 Problem。
//
// 空回答一律算 Problem：模型说了跟没说一样，界面得把那件事说清楚，而不是显示一片空白。
// problem 非空（回合用尽）但模型还是答了话时，回答照给、Problem 也照留 ——
// 用户该同时看到「它说了什么」与「这是怎么回事」。
func wrap(reply, problem string) (string, string) {
	text := strings.TrimSpace(reply)
	if text == "" {
		if problem == "" {
			problem = "模型没有给出回答。"
		}
		return "", problem
	}
	return text, problem
}

// systemPrompt 是这一层的系统提示词。
//
// 写死在代码里、**不给配置留口子**（vlm.Config 那边有个 TagPrompt 是可配的，这里刻意不照做）：
// 那一段讲的是措辞，而这一段讲的是这一层能做什么、不能做什么 ——「改动只是提议，
// 用户点头才生效」这句话与代码里的实际行为是一份契约的两半。让它能被配置改掉，
// 就等于允许界面上出现一句与真实行为不符的承诺。
const systemPrompt = `你是错题本里的助手。用户手里是一本考研错题本：每道错题有题图与答案图，
题上挂着「学科 > 章节 > 知识点」三层标签，每道题另有复习状态与复习记录。

要看数据就用工具，不要凭空猜。工具参数里的 id 一律从工具返回的结果里拿，不要自己编。

关于改动：你**不能**直接改数据。所有改动只能通过 propose_tag_change 提出来，它会落成
一条「待批准改动」，由用户逐条点头才生效。因此：
- 一次只提一件要改的事，理由（reason）写清楚是为了什么。
- 提出来不算完成：还要用自己的话告诉用户你建议改什么、为什么。
- 要删东西时先看清楚会波及多少（工具返回的 summary 里写着），如实告诉用户。

回答用中文，直接、简短。数字必须来自工具返回的数据，不要编。`

// roundsExhaustedNote 是回合用尽时追加的那一条，告诉模型「现在只能回答」。
const roundsExhaustedNote = `（系统提示：这一轮已经到了工具调用次数上限，不能再取数据了。）
请基于手上已有的信息作答；如果有哪一处因为没取到数据而拿不准，就明说是哪一处。`

// roundsExhaustedProblem 是回合用尽时给用户看的那句话。
const roundsExhaustedProblem = "已经到了工具调用次数上限，下面是它最后说的话。"
