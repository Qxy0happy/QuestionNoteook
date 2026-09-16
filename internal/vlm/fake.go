package vlm

import (
	"context"
	"encoding/hex"
	"sync"
)

// Fake 是 Provider 的假实现：一次网络都不打，回答与失败都摆在明面上由调用方摆布。
//
// 为什么它待在非 _test.go 里：
//
//   - 后面几张票（就题讨论、agent 整理数据、每日推荐做几道）的测试也要用它，
//     放在 _test.go 里别的包就够不着了。
//   - 没有 key 的时候，它能把这套界面在真机上点通 —— 票据说真实 provider
//     「代码写好但不测」，那整条流程就只剩这一个可用的替身。
//
// 它**不校验模型名**：模型名来自配置，是调用方的事；这里只把收到的东西记下来，
// 让测试能断言「配置里的名字确实传到了这一层」。
type Fake struct {
	mu sync.Mutex

	// Replies 是 Chat 依次返回的文本；用完最后一条就一直重复它。
	// 空的话返回一句默认的、形状正确的 JSON，好让只关心别的东西的测试不必每次都塞一句。
	//
	// 它只表达得了「一句话」，多轮形状（先回一串工具调用、再回一句话）用 Turns。
	Replies []string

	// Turns 是一整轮一整轮的回答（文本 + 工具调用 + 推理过程），优先于 Replies；
	// 用完最后一条就一直重复它。
	//
	// 工具调用循环必须能这么摆：第一轮模型要调三个工具、第二轮才说话 ——
	// 只给字符串的 Replies 摆不出这个形状。
	Turns []Reply

	// UploadIDs 是 Upload 依次返回的引用；用完就按图的 hash 现编一个。
	UploadIDs []string

	// UploadErr / ChatErr 非 nil 时对应的方法一律返回它。
	// 把 UploadErr 设成 ErrNoUpload，就得到一个「只能内联传图」的 provider ——
	// 回落那条路要验的就是它。
	UploadErr error
	ChatErr   error

	// Streams 是流式那条路上依次播放的分片脚本，用它来摆「分片长什么样」：
	// 切成几片、每片是正文还是思考过程。用完最后一条就一直重复它。
	//
	// **只有正文与思考过程**表达得出来：工具调用是碎片，Streams 里摆不了 ——
	// 要摆工具调用那种多轮回合就用 Turns（那时这一轮的整条回答会当成一片播出去）。
	Streams [][]Delta

	// StreamErr 非 nil 时**只有流式**那条路失败，非流式照常成功 ——
	// 「流式坏了要静默回落」这条要求就是拿它验的（ChatErr 是两条路一起坏）。
	StreamErr error

	// ModelIDs 是 Models 返回的清单；ModelsErr 非 nil 时返回它。
	ModelIDs  []string
	ModelsErr error

	// Chats / Uploaded / Deltas 是收到过的东西，供断言用。
	Chats    []Request
	Uploaded []Image
	Deltas   []Delta

	chats   int
	ups     int
	turns   int
	streams int
	scripts int
	models  int
}

// NewFake 造一个依次回这几句的假 provider。
func NewFake(replies ...string) *Fake { return &Fake{Replies: replies} }

// Upload 记下这张图，返回一个编出来的引用。
func (f *Fake) Upload(_ context.Context, img Image) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.UploadErr != nil {
		return "", f.UploadErr
	}
	f.Uploaded = append(f.Uploaded, img)

	if f.ups < len(f.UploadIDs) {
		id := f.UploadIDs[f.ups]
		f.ups++
		return id, nil
	}
	f.ups++
	// 按 hash 现编一个：同一张图编出来的 id 一样，读测试的人一眼能对上号。
	sum := img.Hash
	if len(sum) > 8 {
		sum = sum[:8]
	}
	if sum == "" {
		sum = hex.EncodeToString([]byte{byte(f.ups)})
	}
	return "file-" + sum, nil
}

// Chat 记下这次请求，返回下一句预置的回答。
func (f *Fake) Chat(_ context.Context, req Request) (Reply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.ChatErr != nil {
		return Reply{}, f.ChatErr
	}
	f.Chats = append(f.Chats, req)
	return f.nextReplyLocked(), nil
}

// ChatStream 走流式那条路：播分片、返回整条。
//
// 这里刻意让「成功一次流式」与「成功一次非流式」在记账上**完全一样**
// （Chats 各多一条、下一句的回答各消耗一条）：调用方看不出差别，
// 于是同一份测试既能跑在流式上、也能跑在非流式上 ——
// 「两条路最终写进库里的东西必须一致」那件事就是这么验的。
func (f *Fake) ChatStream(_ context.Context, req Request, onDelta Sink) (Reply, error) {
	f.mu.Lock()

	// 记「流式被尝试过」。回落那条路要断言的就是它：先走了流式，失败了才回落。
	f.streams++

	// 非流式那个错在流式这条路上同样要生效：不然把 ChatErr 一设，
	// 测试以为自己摆的是「模型坏了」，实际上却从流里拿到了回答。
	if f.ChatErr != nil {
		f.mu.Unlock()
		return Reply{}, f.ChatErr
	}
	if f.StreamErr != nil {
		f.mu.Unlock()
		return Reply{}, f.StreamErr
	}
	f.Chats = append(f.Chats, req)

	var reply Reply
	var deltas []Delta
	if len(f.Streams) > 0 {
		// 摆了脚本就照脚本播：分片怎么切由测试说了算。
		i := f.scripts
		if i >= len(f.Streams) {
			i = len(f.Streams) - 1
		}
		f.scripts++
		deltas = f.Streams[i]
	} else {
		// 没摆脚本：把这一轮该说的话当成**一片**播出去。
		// 流式的形状有了，而回答（含工具调用与推理过程）与非流式那条路一字不差。
		reply = f.nextReplyLocked()
		deltas = []Delta{{Text: reply.Text, Reasoning: reply.Reasoning}}
	}
	f.mu.Unlock()

	// 分片在**锁外**一片一片地叫 onDelta：收分片的那一端将来若回头碰 Fake
	// （记一笔、或者拿一个真的播法），锁里叫出去就是一个死锁。
	// 记账那一小段仍然上锁 —— 播是播，记是记。
	var got Reply
	for _, d := range deltas {
		f.mu.Lock()
		f.Deltas = append(f.Deltas, d)
		f.mu.Unlock()

		got.Text += d.Text
		got.Reasoning += d.Reasoning
		if onDelta != nil {
			onDelta(d)
		}
	}
	if len(f.Streams) > 0 {
		// 脚本那条路上，「整条」就是分片拼出来的 —— 要验的就是这个等式。
		reply = got
	}
	return reply, nil
}

// Models 返回预置的模型清单；一次都不记进 Chats（它跟对话不是一回事）。
func (f *Fake) Models(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.models++
	if f.ModelsErr != nil {
		return nil, f.ModelsErr
	}
	// 拷一份出去：调用方改它不该动到 Fake 里的那几条。
	return append([]string(nil), f.ModelIDs...), nil
}

// nextReplyLocked 取这一轮该说的话。Chat 与 ChatStream 都走它 ——
// 两条路消耗回答的次序因此只有一个出处，测试摆一份 Replies 两条路都能用。
func (f *Fake) nextReplyLocked() Reply {
	// Turns 优先：它表达得了「这一轮要调工具」这种形状，Replies 只能给一句话。
	if len(f.Turns) > 0 {
		i := f.turns
		if i >= len(f.Turns) {
			i = len(f.Turns) - 1
		}
		f.turns++
		return f.Turns[i]
	}

	if len(f.Replies) == 0 {
		return Reply{Text: `{"tags":[{"subject":"占位学科","chapter":"","point":""}]}`}
	}
	i := f.chats
	if i >= len(f.Replies) {
		i = len(f.Replies) - 1
	}
	f.chats++
	return Reply{Text: f.Replies[i]}
}

// ChatCount 返回 Chat 被调了几次。测试里断言「第二次没再上传」这类事时，
// 顺带也会想知道到底跑了几轮。
func (f *Fake) ChatCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Chats)
}

// UploadCount 返回**成功**上传了几张（失败的那次不留记录）。
//
// 它是「同一张图没有反复上传」这条要求的直接证据：同一个 Service 上打两次标签，
// 这个数应当还是 1。
func (f *Fake) UploadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Uploaded)
}

// StreamCount 返回流式那条路被**尝试**了几次（失败的那次也算）。
//
// 与 ChatCount 一起看才说明得了回落：StreamCount=1 而 ChatCount=1，
// 就是「先走了流式、它失败了、于是回落了一次」。
func (f *Fake) StreamCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streams
}

// ModelsCount 返回取模型清单被调了几次。「校验模型」那个按钮的测试要它：
// 配置不合规时这个数必须是 0（没发出去之前就报错了）。
func (f *Fake) ModelsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.models
}
