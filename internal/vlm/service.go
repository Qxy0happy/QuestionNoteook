package vlm

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"questionbook/internal/library"
	"questionbook/internal/tags"
)

// Service 是「VLM 打标签」对前端暴露的那一面，也是本包唯一的测试缝
// （spec：全应用只有一条缝，在服务层 —— 调用它、断言它的返回与它落库的结果）。
//
// 它做的是一件完整的事：取题图 → 交给 provider（能复用引用就复用）→ 拿回分层标签 →
// **先给用户改** → 用户点了保存才落成一棵真的标签树并挂到题上。
//
// 「先给用户改」是硬的：SuggestTags 一个字节都不写库，SaveTags 才写。
// 所以这一层有两半，中间隔着一次人的决定。
type Service struct {
	questions *library.Service
	tags      *tags.Service

	// configPath 是那份 JSON 配置的落点。上传缓存放它旁边 —— 接线时只需要知道
	// 这一个路径（main.go 的 dataDir() 下），缓存文件名是定死的（见 fileCacheName）。
	configPath string
	cache      *fileIDCache
	cachePath  string

	// fixed 非空时永远用它，不看配置建的那个。测试与「没有 key 也想把界面点通」用它。
	fixed Provider

	// emit 把一片增量播给界面（讨论页与 agent 页一边生成一边显示那件事）。
	//
	// 它是个**字段**而不是直接调 EmitDelta：本包的业务逻辑因此一行都不依赖 Wails，
	// 测试里换成一个往切片里写的函数，整条流式路径照样跑，一条事件都不发（见 events.go）。
	emit func(streamID string, d Delta)

	mu       sync.Mutex
	loaded   bool
	loadErr  error
	cfg      Config
	provider Provider
}

// Option 给服务补一样可选能力。
type Option func(*Service)

// WithProvider 换掉 provider：测试里给假实现。
//
// 真实接线不传这一项 —— provider 按配置现建，那正是「provider 是一层抽象」的落点。
// 注入了之后配置照样会读（模型名、提示词、凭据指纹都从它来），但读不到也不挡事：
// 注入的 provider 是彻底顶替，见 current。
func WithProvider(p Provider) Option { return func(s *Service) { s.fixed = p } }

// WithEmitter 换掉「把一片增量播给界面」那一步。默认是 EmitDelta（走 Wails 事件）。
//
// 传 nil 等于没传：没有播的出口时，NewSink 会给一个 nil，于是整条路**压根不走流式** ——
// 那正是非流式那条退路（见 ChatStreaming）。
func WithEmitter(emit func(streamID string, d Delta)) Option {
	return func(s *Service) {
		if emit != nil {
			s.emit = emit
		}
	}
}

// NewService 接上错题库、标签树与配置文件的落点。
//
// 三个都是接线时必须给的，没有默认值 —— 尤其配置路径：它由应用私有目录推出来，
// 只有 main.go 知道那个目录在哪（安卓上那是 /data/data/<包名>/files/questionbook）。
//
// **构造时不读盘**。一个还没配过 VLM 的应用要能照常启动、照常拍照入库，
// 配置是「用的时候才需要」的东西 —— 票据要求标签功能不挡拍摄。
func NewService(questions *library.Service, tagSvc *tags.Service, configPath string, opts ...Option) *Service {
	s := &Service{
		questions:  questions,
		tags:       tagSvc,
		configPath: configPath,
		cachePath:  filepath.Join(filepath.Dir(configPath), fileCacheName),
		emit:       EmitDelta,
	}
	for _, opt := range opts {
		opt(s)
	}
	s.cache = openFileIDCache(s.cachePath)
	return s
}

// ── 打标签 ──

// ProposedTag 是**一条**分层标签建议：学科 > 章节 > 知识点。
//
// 它还没落库。它既可能来自模型，也可能是用户在界面上改过之后的版本 ——
// 两者形状完全一样，因为「改完的」才是最终要保存的东西。
//
// 章节或知识点可以是空串：模型定不下来时留空，是**合法**的中间状态
// （与标签包那边「先搭骨架再往里填」一致）。学科不能空 —— 空了就不知道该挂到哪棵树下。
type ProposedTag struct {
	Subject string
	Chapter string
	Point   string
}

// TagSuggestion 是「让 VLM 给这道题打标签」这一次的结果。
type TagSuggestion struct {
	QuestionID int64

	// Tags 是模型给的建议，空切片而不是 nil（前端要拿到 []，不是 null）。
	Tags []ProposedTag

	// Raw 是模型的原话。解析成功时它也在 —— 人工核对「它是不是真这么说的」时有用，
	// 票据 01 要评的就是它的质量。它**没有**凭据，模型看不到也不该看到凭据。
	Raw string

	// Problem 非空表示这次调用成功了，但结果不能用：模型没按约定的 JSON 回，
	// 或者回了空集（它自己也没主意）。此时 Tags 是空的，而 Raw 里是它说的原话。
	//
	// 为什么它是字段而不是 error：Wails 那边 error 会把返回值一起丢掉，
	// 而这里恰恰**要把模型的原话交给用户看** —— 调提示词全靠那句原话。
	// 真正的调用失败（没配置、网络、401）仍然是 error，走的仍是另一条路。
	Problem string
}

// SuggestTags 让 VLM 给一道错题拟一份分层标签。
//
// **只读**：不建标签、不往题上挂任何东西。票据要求这些建议先交给用户改、
// 保存之后才生效 —— 那一步是 SaveTags。所以这个方法可以随便重试，不会留下痕迹。
//
// 题不存在返回 library.ErrNotFound；没配 VLM 返回 ErrNotConfigured；
// 模型给了不能用但调用成功的结果时 error 为 nil，看 TagSuggestion.Problem。
func (s *Service) SuggestTags(questionID int64) (TagSuggestion, error) {
	provider, cfg, err := s.current()
	if err != nil {
		return TagSuggestion{}, err
	}

	q, err := s.questions.Get(questionID)
	if err != nil {
		return TagSuggestion{}, err
	}

	img, err := s.loadImage(q)
	if err != nil {
		return TagSuggestion{}, err
	}
	img = s.resolveImage(provider, cfg, img)

	system := strings.TrimSpace(cfg.TagPrompt)
	if system == "" {
		// 把用户现有的词表当参考喂进去（见 buildTagPrompt）。
		// 只在用内置提示词时才去读词表：配置里自带提示词时它用不上，白读一次库。
		vocabulary, err := s.tags.List()
		if err != nil {
			return TagSuggestion{}, err
		}
		system = buildTagPrompt(vocabulary)
	}

	reply, err := provider.Chat(context.Background(), Request{
		// 打标签必然带图，所以走视觉那一路（见 Config.modelFor）。
		Model:  cfg.modelFor(true),
		System: system,
		// 图片只能挂在 user 消息里（服务方的硬约束）。提示词在 system，图与它那段
		// 文字在同一条 user 消息里 —— 这是唯一放得下图的角色。
		Messages: []Message{{Role: RoleUser, Text: tagUserPrompt, Images: []Image{img}}},
		JSON:     true,
	})
	if err != nil {
		return TagSuggestion{}, err
	}

	proposed, perr := parseTagReply(reply.Text)
	if perr != nil && !errors.Is(perr, ErrBadReply) {
		return TagSuggestion{}, perr
	}

	res := TagSuggestion{QuestionID: questionID, Tags: proposed, Raw: reply.Text}
	if res.Tags == nil {
		res.Tags = []ProposedTag{}
	}
	switch {
	case perr != nil:
		res.Problem = "模型没按约定的 JSON 格式回答，下面是它说的原话。"
	case len(res.Tags) == 0:
		res.Problem = "模型没给出可用的标签，下面是它说的原话。"
	}
	return res, nil
}

// SaveTags 把（用户改过的）这份建议落成一棵真的标签树，并整份挂到这道题上。
//
// 这是「保存后才生效」的那一步，也是本包唯一会写库的地方。缺的标签现建：
// 名字在（父, 名字）这一对上找不到就建一个。已经有的照用，不新建 ——
// 所以用户反复用同一批标签时，词表不会长出一堆同名节点。
//
// 挂上去的是**整条路径**（学科 + 章节 + 知识点），不是只有叶子。理由是跟另一条入口对齐：
// 手工打标签那个勾选面板勾一个知识点时会连带它的父一起勾上（见 TagFilter.svelte），
// 两条路挂出来的东西得是同一个形状，否则「这道题有几个标签」这个数会随入口不同而不同。
//
// 返回落库之后这道题真实的标签集。
func (s *Service) SaveTags(questionID int64, proposed []ProposedTag) ([]tags.Tag, error) {
	// 先确认这道题在：写之前查一次，拿一个不存在的 id 来写应当收到 ErrNotFound，
	// 而不是一个「悄悄什么都没发生」的成功（与标签包那边同一个规矩）。
	if _, err := s.questions.Get(questionID); err != nil {
		return nil, err
	}

	// 词表一次取全，之后在内存里按 (父, 名字) 找 —— 一次 List 换一张索引，
	// 比每一层都去问一次库省事。标签包没有「按名字找」这个接口，也不必为这一票去加。
	all, err := s.tags.List()
	if err != nil {
		return nil, err
	}
	index := newNameIndex(all)

	var ids []int64
	seen := make(map[int64]bool)
	for _, p := range proposed {
		path, err := index.resolve(s.tags, p)
		if err != nil {
			return nil, err
		}
		for _, t := range path {
			if seen[t.ID] {
				continue
			}
			seen[t.ID] = true
			ids = append(ids, t.ID)
		}
	}

	// 整体替换而不是一条条增删：界面上给的就是改完之后的整份，
	// 一次替换比在前后端各维护一份增量稳妥（与手工打标签那条路同一个选择）。
	if err := s.tags.SetQuestionTags(questionID, ids); err != nil {
		return nil, err
	}
	return s.tags.TagsOfQuestion(questionID)
}

// ── 配置 ──

// Config 返回配置的「界面版」：**不含凭据的值**，只说有没有、多长。
func (s *Service) Config() ConfigView {
	s.mu.Lock()
	defer s.mu.Unlock()

	_ = s.ensureLocked() // 读不到也要给一份视图：界面正是靠 Problem 告诉用户缺什么
	return s.cfg.View(s.configPath)
}

// SetConfig 把配置写下去并立刻生效（换 key、换模型、试提示词都不必重启）。
//
// 传进来的那份是**补丁**不是整份：空字符串表示不动这一项。这不是为了省事 ——
// 界面拿不到凭据的值（本包从不把它读出去），所以它改别的项时没法把 key 原样带回来。
// 要清空某一项就删掉配置文件重来。
//
// 先验再写：一份缺项的配置存下去也用不了，不如当场退回，别让它躺在盘上冒充「配好了」。
func (s *Service) SetConfig(patch Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 拿盘上那份当底。读不回来（文件不在、或者内容坏了）就当它不存在，
	// 直接用这次传来的这份 —— 那正是「第一次配置」的情形。
	s.loaded = true
	if err := s.readLocked(); err != nil && !errors.Is(err, ErrNotConfigured) {
		s.cfg = Config{}
		s.provider = nil
	}

	merged := s.cfg.withOverrides(patch)
	if err := merged.Validate(); err != nil {
		return err
	}
	if err := merged.Save(s.configPath); err != nil {
		return err
	}

	s.loadErr = s.readLocked()
	return s.loadErr
}

// ModelLister 是「能报出服务方那边有哪些模型」这一面。只有取清单那一次请求用它 ——
// 与 Streamer 一样是个小一号的接口，Provider 上不再长东西。
type ModelLister interface {
	Models(ctx context.Context) ([]string, error)
}

// Models 拿**盘上那份配置叠加这份补丁**，去服务方要一次模型清单。
//
// 它是两件事共用的那一次请求：设置页的「校验模型」按钮，以及两个模型名下拉的数据来源。
// 共用在这里说得通 —— 两件事问的是同一个问题：这个端点、这份凭据，服务方认不认。
// 而且它是**最省**的一次请求：一个 token 都不生成（见 deepseek.go 的 Models）。
//
// 三个规矩：
//
//   - **不落盘**。补丁只用于这一次请求。用户点「校验」时顺手把它存下去，
//     一个打错的地址就会把那份能用的配置顶掉 —— 而校验的意义恰恰是「先试试」。
//     想存是 SetConfig 那一步的事，界面上的「保存」按钮走的就是它。
//   - 只验端点与凭据（validateEndpoint），**不要求模型名** —— 它就是为了挑模型名才发的。
//   - 凭据只出不进：回来的只有模型名，补丁里的 key 不会被带出来（更不会进日志或报错）。
//
// 验到了什么、没验到什么，写在 deepseek.go 的 Models 上 —— 那段话就是界面要照实说的。
func (s *Service) Models(patch Config) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 拿盘上那份当底，与 SetConfig 同一个道理：界面拿不到凭据的值，
	// 它只能把用户这次改的项传进来，剩下的靠这一份兜着。
	// 盘上那份读坏了（不是「还没配」）就当它不存在，只看补丁 —— 与 SetConfig 同一套规矩。
	if err := s.ensureLocked(); err != nil && !errors.Is(err, ErrNotConfigured) {
		s.cfg = Config{}
	}

	merged := s.cfg.withOverrides(patch)
	if err := merged.validateEndpoint(); err != nil {
		return nil, err
	}

	if s.fixed != nil {
		// 注入的 provider（测试、或者没有 key 的那种跑法）。上面那句照样要过：
		// 「你填的这份看起来能不能用」是界面上的规矩，不该因为换了个 provider 就变。
		lister, ok := s.fixed.(ModelLister)
		if !ok {
			return nil, errors.New("VLM: 这个 provider 不支持取模型清单")
		}
		return lister.Models(context.Background())
	}

	// 这里不能走 NewDeepSeek：它要求一份完整配置（缺 vision_model 就报错），
	// 而这一次请求恰恰是**为了挑模型名**才发的（先有鸡后有蛋，见 newDeepSeek）。
	return newDeepSeek(merged).Models(context.Background())
}

// ── 内部 ──

// chat 发一次对话：能流就流，流不动静默回落到非流式（那条规矩的出处见 ChatStreaming）。
//
// 本包里「provider、streamID、播出去的那一步」凑到一起只有这一处 ——
// 讨论与 agent 两条路都从这儿过。streamID 为空（前端没给）时连流都不试：
// 没有 id，界面认不出哪些分片是自己的（见 NewSink）。
func (s *Service) chat(p Chatter, req Request, streamID string) (Reply, error) {
	return ChatStreaming(context.Background(), p, req, NewSink(streamID, s.emit))
}

// current 返回这次该用的 provider 与配置。第一次用到时才读盘（见 NewService）。
func (s *Service) current() (Provider, Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	err := s.ensureLocked()
	if s.fixed != nil {
		// 注入的 provider 是彻底顶替：配置读不到也照样跑（读到了就用它的模型名与提示词）。
		// 测试与「没有 key 也想把界面点通」要的就是这个。
		return s.fixed, s.cfg, nil
	}
	if err != nil {
		return nil, Config{}, err
	}
	if s.provider == nil {
		return nil, Config{}, ErrNotConfigured
	}
	return s.provider, s.cfg, nil
}

// ensureLocked 保证 s.cfg 与 s.provider 已经就位，必要时读一次盘。
//
// 读失败的结果会缓存下来：配置是用户手写的，一个坏文件不该在每次点「打标签」时
// 都重读一遍、重报一遍。换过配置（SetConfig）之后这个缓存会被重置。
func (s *Service) ensureLocked() error {
	if s.loaded {
		return s.loadErr
	}
	s.loaded = true
	s.loadErr = s.readLocked()
	return s.loadErr
}

// readLocked 无条件读一次配置并据此重建 provider。调用方必须已经持有 s.mu。
func (s *Service) readLocked() error {
	cfg, err := LoadConfig(s.configPath)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	s.cfg = cfg
	s.provider = nil
	if s.fixed == nil {
		p, err := NewDeepSeek(cfg)
		if err != nil {
			return err
		}
		s.provider = p
	}
	return nil
}

// loadImage 按这道题的题图 hash 把图读出来，包成要交给模型的那一份。
//
// 走题库的 QuestionImage 而不是自己去碰文件层：题图的读归题库（spec 的服务划分），
// 而且它回来的已经是 PNG 的 base64 —— 正好是服务方要的那份字节，
// 这里解一次 base64 就能直接塞进报文，不必再把图像解出来又编回去。
func (s *Service) loadImage(q library.Question) (Image, error) {
	b64, err := s.questions.QuestionImage(q.QuestionHash)
	if err != nil {
		return Image{}, err
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return Image{}, fmt.Errorf("VLM: 题图不是合法的 base64: %w", err)
	}
	return Image{Hash: q.QuestionHash, Data: raw, MIME: "image/png"}, nil
}

// resolveImage 把这张图交上去换一个可复用的引用；换不到就原样返回（留给内联那条路）。
//
// 三种「换不到」在这里是同一种处理：服务方没有文件接口（ErrNoUpload）、这次传失败、
// 或者结果就是这个。都不该让打标签整个失败 —— 内联一样能把图送到，
// 代价只是下次还得再传一遍。上传失败的具体原因不在这里报，是因为紧接着的 Chat
// 会拿到同一个根因（没配置好、key 不对），由它报一次比这里报两次清楚。
func (s *Service) resolveImage(p Provider, cfg Config, img Image) Image {
	print := keyPrint(cfg.APIKey)
	if id, ok := s.cache.get(img.Hash, print); ok {
		img.FileID = id
		return img
	}

	id, err := p.Upload(context.Background(), img)
	if err != nil {
		return img
	}
	s.cache.put(img.Hash, id, print)
	img.FileID = id
	return img
}

// ── 词表索引 ──

// nameIndex 是「(父, 名字) → 标签」的一张内存索引：建标签之前先在这里找，找不到才去建。
//
// 键用「父 + 名字」而不是只用名字，是因为不同学科下同名是合法的 ——
// 「数学 > 概率论」与「专业课 > 概率论」是两条不同的知识点，不能混成一个。
type nameIndex map[int64]map[string]tags.Tag

func newNameIndex(all []tags.Tag) nameIndex {
	idx := nameIndex{}
	for _, t := range all {
		idx.put(t)
	}
	return idx
}

func (idx nameIndex) put(t tags.Tag) {
	if idx[t.ParentID] == nil {
		idx[t.ParentID] = map[string]tags.Tag{}
	}
	idx[t.ParentID][t.Name] = t
}

func (idx nameIndex) find(parentID int64, name string) (tags.Tag, bool) {
	t, ok := idx[parentID][name]
	return t, ok
}

// resolve 把一条建议落成树上的一条路径，缺的节点现建。
//
// 返回的路径可能短于三层：某一层名字是空的就到此为止（没有章节的学科是合法的中间状态）。
// 学科是空的那条建议整个丢掉 —— 没有学科就不知道该挂到哪棵树下面。
func (idx nameIndex) resolve(svc *tags.Service, p ProposedTag) ([]tags.Tag, error) {
	subject := strings.TrimSpace(p.Subject)
	if subject == "" {
		return nil, nil
	}

	path := make([]tags.Tag, 0, 3)
	node, err := idx.ensure(svc, 0, subject)
	if err != nil {
		return nil, err
	}
	path = append(path, node)

	// 层级由父节点推出来（tags.Create 自己算），所以这里只管顺着往下走。
	//
	// 模型把章节写成与学科同名时会建出「数学 > 数学」这样一层。看着怪，但那是模型的锅，
	// 不是树的错 —— 悄悄替它改名或者丢掉，只会让用户看不到模型到底给了什么，
	// 而「看到模型给了什么」正是这一步的全部意义。
	for _, name := range []string{p.Chapter, p.Point} {
		name = strings.TrimSpace(name)
		if name == "" {
			break
		}
		node, err = idx.ensure(svc, node.ID, name)
		if err != nil {
			return nil, err
		}
		path = append(path, node)
	}
	return path, nil
}

// ensure 找到 (parentID, name) 对应的标签，没有就建一个。
//
// 建的时候可能撞上「同时被别处建了」（ErrDuplicate）—— 那就把词表重读一次合进索引再找。
// 一个用户一次点一下，这种事几乎不会发生，但发生了不该让整次保存失败。
func (idx nameIndex) ensure(svc *tags.Service, parentID int64, name string) (tags.Tag, error) {
	if t, ok := idx.find(parentID, name); ok {
		return t, nil
	}

	t, err := svc.Create(parentID, name)
	if err == nil {
		idx.put(t)
		return t, nil
	}
	if !errors.Is(err, tags.ErrDuplicate) {
		return tags.Tag{}, err
	}

	fresh, lerr := svc.List()
	if lerr != nil {
		return tags.Tag{}, lerr
	}
	for _, t := range fresh {
		idx.put(t)
	}
	if t, ok := idx.find(parentID, name); ok {
		return t, nil
	}
	return tags.Tag{}, err
}
