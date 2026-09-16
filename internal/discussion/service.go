package discussion

import (
	"strings"
	"time"

	"questionbook/internal/library"
	"questionbook/internal/vlm"
)

// Service 是「讨论」对前端暴露的那一面，也是本包唯一的测试缝（见 spec）。
//
// 它持有一条错题库连接（讨论与错题同一份库文件）与题库本身 —— 写之前要确认这道题在，
// 那条查询在 library 那边，不必在本包再抄一遍 SQL。
type Service struct {
	store     *Store
	questions *library.Store
	asker     Asker

	// 取「现在」的方式。做成字段是为了能在测试里钉死：带上时间戳的排序要断言，
	// 用真实时钟就没法让两条记录落在同一毫秒里（而那正是自增主键兜底的那一格）。
	now func() time.Time
}

// Option 给服务补一样可选能力。
type Option func(*Service)

// WithNow 换掉取当前时刻的方式。只有测试会用。
func WithNow(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// NewService 用一个已经开好的错题库、一条「跟模型说话」的接缝构造讨论服务。
//
// 讨论用的连接就是题库那一条（ADR-0004 只把**图片**放到库外，元数据都在这一份 SQLite 里），
// 不再 Open 第二个到同一文件的连接。
//
// asker 传的就是 *vlm.Service —— 它正好满足 Asker，接线时不必再加一层适配壳。
func NewService(questions *library.Store, asker Asker, opts ...Option) *Service {
	s := &Service{
		store:     NewStore(questions.DB()),
		questions: questions,
		asker:     asker,
		now:       time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// History 返回一道错题的讨论记录，从早到晚（同一毫秒内按落库先后）。
//
// 题不存在时返回空集而不是报错：这是个读，界面拿一个刚被删掉的 id 来问是正常的，
// 看到「还没聊过」就够了（与 tags.TagsOfQuestion 同一条规矩）。写操作相反 —— 见 Ask。
func (s *Service) History(questionID int64) ([]Message, error) {
	return s.store.history(questionID)
}

// Ask 就一道错题问一句：先记下用户这句，再交给 VLM，把回答也记下。
//
// **用户那句先落库，顺序在实现里而不只是在注释里。** 模型调用可能断网、超时、没配 key，
// 那些失败都不该把用户打的字带走（票据 33：讨论时被打断的人，讨论记录不会丢）。
// 代价是失败之后讨论里会留下一条没有回应的用户消息 —— 那是如实的，他确实问了，
// 而模型确实没答上来；界面上重读一次 History 就能看到，再问一次即可。
//
// 失败时返回零值 Turn 与错，只有前两种不落任何写入：
//   - 要问的是空的 → ErrEmptyMessage
//   - 题不存在 → library.ErrNotFound
//   - 模型没回答（空文本）→ ErrEmptyReply
//   - 没配 VLM / 网络 / 服务方报错 → provider 那边原样冒上来
//
// streamID 由界面编好传进来，只为让**这一轮**的回答边生成边显示（见 Asker.Ask）。
// 它不落库：拿回答那一句话永远来自下面那个返回值（流式的分片只是给眼睛看的），
// 于是「流式与非流式最终写进库里的东西一样」这件事，在这里就是同一行代码在写。
func (s *Service) Ask(questionID int64, text string, streamID string) (Turn, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Turn{}, ErrEmptyMessage
	}
	// 先确认这道题在，再动库。写一个不存在的 id 应当收到 ErrNotFound，
	// 而不是一个「悄悄什么都没发生」的成功（与 tags、review 那边同一条规矩）。
	if _, err := s.questions.GetQuestion(questionID); err != nil {
		return Turn{}, err
	}

	asked, err := s.store.append(questionID, RoleUser, text, s.now())
	if err != nil {
		return Turn{}, err
	}

	// 回库取整段对话当上下文 —— 包含刚写下的这句。不复用「刚 append 的 + 手上的那份」：
	// 那样得在内存里再维护一份和库里一样的东西，而这份历史本来就在库里。
	history, err := s.store.history(questionID)
	if err != nil {
		return Turn{}, err
	}

	reply, err := s.asker.Ask(questionID, wireHistory(history), streamID)
	if err != nil {
		return Turn{}, err
	}
	answer := strings.TrimSpace(reply.Text)
	if answer == "" {
		return Turn{}, ErrEmptyReply
	}

	said, err := s.store.append(questionID, RoleAssistant, answer, s.now())
	if err != nil {
		return Turn{}, err
	}
	// 思考过程只在返回值上贴一下，不进库（理由写在 Message.Reasoning 上）。
	// 贴在这一条而不是 Turn 上：界面本来就按「每条回答」渲染那一块，不必为它单开一个槽。
	said.Reasoning = strings.TrimSpace(reply.Reasoning)
	return Turn{Question: asked, Reply: said}, nil
}

// wireHistory 把库里的记录摊成要发出去的那份对话。
//
// 只带角色与正文：图归 vlm 那一层挂 —— 只有它知道题图与答案图在哪，也只有它走得到
// 那份上传缓存。
//
// 刻意**不做截断**：整段对话原样发出去。代价是长对话会把上下文吃光（服务方那边有上限，
// 超了就是一次报错），换来的是这一版没有「从第几句开始丢」这个需要调的东西。
// 要截断时改这里一处：保留最后 N 轮，并在被截断的头一条前面加一句说明。
func wireHistory(msgs []Message) []vlm.Message {
	out := make([]vlm.Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, vlm.Message{Role: vlm.Role(m.Role), Text: m.Text})
	}
	return out
}
