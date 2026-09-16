package vlm

import (
	"context"
	"encoding/hex"
	"fmt"
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

	// Chats / Uploaded 是收到过的东西，供断言用。
	Chats    []Request
	Uploaded []Image

	chats int
	ups   int
	turns int
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

	// Turns 优先：它表达得了「这一轮要调工具」这种形状，Replies 只能给一句话。
	if len(f.Turns) > 0 {
		i := f.turns
		if i >= len(f.Turns) {
			i = len(f.Turns) - 1
		}
		f.turns++
		return f.Turns[i], nil
	}

	if len(f.Replies) == 0 {
		return Reply{Text: fmt.Sprintf(`{"tags":[{"subject":"占位学科","chapter":"","point":""}]}`)}, nil
	}
	i := f.chats
	if i >= len(f.Replies) {
		i = len(f.Replies) - 1
	}
	f.chats++
	return Reply{Text: f.Replies[i]}, nil
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
