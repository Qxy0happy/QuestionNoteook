package vlm_test

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"questionbook/internal/vlm"
)

// 这一份从**服务层**这一面看流式：分片按顺序播出去、拼起来等于整条、
// 流式坏了静默回落，以及取模型清单那一次请求（「校验模型」按钮与两个模型名下拉的来源）。
//
// 解析那一段（SSE 的一行行长什么样）不在这一份里，它是白盒的 sse_test.go ——
// 那里喂一个 strings.Reader 进去，能断言到每一片。这里只摆布 provider 摆得出来的东西。
//
// 「一次网络都不打」照旧：provider 是 vlm.Fake；「播出一片」那一步换成一个往切片里写的函数
// （真实的那一步是往 Wails 发事件，测试里没有应用，也不该起一个 —— 见 vlm/events.go）。

// recorded 记下播出去的一片：带的是哪个号、那一片是什么。
type recorded struct {
	streamID string
	d        vlm.Delta
}

// askHistory 是「就一道错题问一句」的最小历史：只有最后那句要回答的用户消息。
func askHistory() []vlm.Message {
	return []vlm.Message{{Role: vlm.RoleUser, Text: "这步为什么"}}
}

// ── 流式 ──

// 分片一片一片地来、顺序与到达顺序一致，拼起来**一字不差**等于返回的整条。
//
// 这是这一票对界面许下的那句话在服务层的落点：界面拿到的是逐片的那几片，
// 而落库用的是返回值 —— 两者必须对得上，否则屏幕上最后会跳一下（看见的和存下的不同）。
func TestStreamingEmitsChunksInOrderAndReturnsTheWhole(t *testing.T) {
	var sent []recorded
	h := newHarnessWith(t, []vlm.Option{
		vlm.WithEmitter(func(id string, d vlm.Delta) {
			sent = append(sent, recorded{streamID: id, d: d})
		}),
		// 摆了分片脚本，这一句就用不上了（见 vlm.Fake.Streams）。
	}, "分片脚本说了算")
	// 两层切片：外层一条 = **一次**流式调用（这里只有一次），里层是这一次要播的那几片 ——
	// 四片一片一片地来，正是真实流式那个形状（见 vlm.Fake.Streams）。
	h.fake.Streams = [][]vlm.Delta{{
		{Reasoning: "把根号里那一整段看成一个整体。"},
		{Text: "先配方，"},
		{Reasoning: "两边同时加上同一个常数。"},
		{Text: "再求根。"},
	}}
	q := h.addQuestion(t, 0x11)

	const streamID = "s-1"
	reply, err := h.svc.Ask(q.ID, askHistory(), streamID)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	// 先确认真的走了流式那条路：假 provider 记着「流式被尝试过」。
	if n := h.fake.StreamCount(); n != 1 {
		t.Fatalf("流式被尝试了 %d 次，想要 1 次", n)
	}
	// 一次成功的流式与一次成功的非流式在记账上一样（各占一条）——
	// 「两条路最终写进库里的东西一致」那条测试就是靠这一点成立的。
	if n := h.fake.ChatCount(); n != 1 {
		t.Errorf("与模型往返了 %d 次，想要 1 次", n)
	}

	// 每一片都带着这一次的号：前端就是靠它筛出自己那一次的。
	// 号错了等于把这一轮的话播到上一轮那一块里去。
	want := []vlm.Delta{
		{Reasoning: "把根号里那一整段看成一个整体。"},
		{Text: "先配方，"},
		{Reasoning: "两边同时加上同一个常数。"},
		{Text: "再求根。"},
	}
	if len(sent) != len(want) {
		t.Fatalf("播了 %d 片，想要 %d 片：%+v", len(sent), len(want), sent)
	}
	for i, got := range sent {
		if got.streamID != streamID {
			t.Errorf("第 %d 片带的号是 %q，想要 %q", i, got.streamID, streamID)
		}
		if got.d != want[i] {
			t.Errorf("第 %d 片 = %+v，想要 %+v（顺序也要一样）", i, got.d, want[i])
		}
	}

	// 拼起来 == 整条：正文与思考过程各攒一路（服务方那边这两路也是分开的）。
	var texts, reasons strings.Builder
	for _, one := range sent {
		texts.WriteString(one.d.Text)
		reasons.WriteString(one.d.Reasoning)
	}
	if texts.String() != reply.Text {
		t.Errorf("分片拼出来的正文 = %q，返回的整条 = %q", texts.String(), reply.Text)
	}
	if reasons.String() != reply.Reasoning {
		t.Errorf("分片拼出来的思考过程 = %q，返回的整条 = %q", reasons.String(), reply.Reasoning)
	}
	if reply.Text != "先配方，再求根。" {
		t.Errorf("整条正文 = %q，想要 %q", reply.Text, "先配方，再求根。")
	}
	// 讨论必然带图，所以走视觉那一路的模型名（配置里那个**原样**过去）。
	if len(h.fake.Chats) == 0 || h.fake.Chats[0].Model != testConfig().VisionModel {
		t.Errorf("报文里的模型名 = %+v，想要配置里的 %q", h.fake.Chats, testConfig().VisionModel)
	}
}

// 流式坏了**静默回落**：回答照旧拿得到，用户不该因为流式出问题就问不出话。
//
// 摆在明面上的代价：这一次会多花一份完整请求（流式那次的钱白花了）——
// 所以这里顺带断言「回落之前确实试过流式」，而不是从头就没走那条路。
func TestStreamFailureFallsBackSilently(t *testing.T) {
	var sent []recorded
	h := newHarnessWith(t, []vlm.Option{
		vlm.WithEmitter(func(id string, d vlm.Delta) {
			sent = append(sent, recorded{streamID: id, d: d})
		}),
	}, "整条回答还是来了。")
	h.fake.StreamErr = errors.New("流到一半断了")
	q := h.addQuestion(t, 0x12)

	reply, err := h.svc.Ask(q.ID, askHistory(), "s-2")
	if err != nil {
		// 这一句是关键：流式的错**一个字都不该冒到调用方那里**。
		t.Fatalf("流式失败时 Ask 返回了 %v，想要静默回落、照旧答出来", err)
	}
	if reply.Text != "整条回答还是来了。" {
		t.Errorf("回答 = %q，想要回落那条路拿到的那一句", reply.Text)
	}

	if n := h.fake.StreamCount(); n != 1 {
		t.Errorf("流式被尝试了 %d 次，想要 1 次（先试流式、失败了才回落）", n)
	}
	if n := h.fake.ChatCount(); n != 1 {
		t.Errorf("非流式跑了 %d 次，想要 1 次", n)
	}
	// 一片都没播出去：断在哪儿都不知道，半截话留在界面上比什么都不留更糟。
	if len(sent) != 0 {
		t.Errorf("失败的那次流式仍然播了 %d 片：%+v", len(sent), sent)
	}
}

// 前端没给号（streamID 为空）时**连流都不试**：没有号，界面认不出哪些分片是自己的，
// 流了也只是往别人那一块里灌字（见 NewSink）。
func TestEmptyStreamIDNeverTriesStreaming(t *testing.T) {
	var sent []recorded
	h := newHarnessWith(t, []vlm.Option{
		vlm.WithEmitter(func(id string, d vlm.Delta) {
			sent = append(sent, recorded{streamID: id, d: d})
		}),
	}, "走了非流式。")
	q := h.addQuestion(t, 0x13)

	reply, err := h.svc.Ask(q.ID, askHistory(), "")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if reply.Text != "走了非流式。" {
		t.Errorf("回答 = %q", reply.Text)
	}
	if n := h.fake.StreamCount(); n != 0 {
		t.Errorf("没有号却试了 %d 次流式", n)
	}
	if len(sent) != 0 {
		t.Errorf("没有号却播了 %d 片：%+v", len(sent), sent)
	}
}

// ── 取模型清单（「校验模型」与两个下拉的来源）──

// 清单来自 provider，原样带回来；而且**一个字节都不落盘** ——
// 补丁只用于这一次请求，用户点「校验」时顺手把一份打错的地址存下去，
// 恰好会把那份能用的配置顶掉，而校验的意义正是「先试试」。
func TestModelsReturnsTheListAndSavesNothing(t *testing.T) {
	h := newHarness(t)
	h.fake.ModelIDs = []string{"deepseek-flash", "deepseek-reasoner"}

	before, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatalf("读配置: %v", err)
	}

	// 补丁里换掉的是**地址与凭据**（现实里用户正是在这里先填了新的、再点校验）。
	got, err := h.svc.Models(vlm.Config{
		BaseURL:     "https://刚填的.invalid/v1",
		APIKey:      "刚填的凭据",
		VisionModel: "刚挑的模型",
	})
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if !slices.Equal(got, []string{"deepseek-flash", "deepseek-reasoner"}) {
		t.Errorf("清单 = %v，想要与预置的一样", got)
	}
	if n := h.fake.ModelsCount(); n != 1 {
		t.Errorf("取清单发了 %d 次，想要 1 次", n)
	}
	// 与对话不是一回事：这一次请求不该占掉一条回答，也不该记进 Chats。
	if n := h.fake.ChatCount(); n != 0 {
		t.Errorf("取清单却记了 %d 次对话", n)
	}

	after, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatalf("再读配置: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("取一次清单把配置写回去了：\n改前 %s\n改后 %s", before, after)
	}
}

// 地址或凭据不齐时**一个请求都不发**，当场报「还没配」——
// 这也是设置页那个按钮的意义：先在本地把明显写错的挡住。
func TestModelsRefusesToGoOutWithoutEndpointOrCredential(t *testing.T) {
	h := newHarness(t)
	// 把盘上那份删掉：于是既没有地址也没有凭据（补丁里那些空字段是「这次不改」的意思，
	// 不是「清空」—— 它们兜不住底，见 withOverrides）。
	if err := os.Remove(h.cfgPath); err != nil {
		t.Fatalf("删配置: %v", err)
	}

	_, err := h.svc.Models(vlm.Config{})
	if !errors.Is(err, vlm.ErrNotConfigured) {
		t.Errorf("err = %v，想要 vlm.ErrNotConfigured", err)
	}
	if n := h.fake.ModelsCount(); n != 0 {
		t.Errorf("配置不齐却发了 %d 次请求", n)
	}

	// 地址填了个不合法的：同样不该发出去。
	_, err = h.svc.Models(vlm.Config{BaseURL: "不是个地址", APIKey: "k"})
	if err == nil {
		t.Fatal("地址不合法却过了")
	}
	if n := h.fake.ModelsCount(); n != 0 {
		t.Errorf("地址不合法却发了 %d 次请求", n)
	}
}

// 服务方说的话原样冒上来（界面上要照实显示那几句话），而**凭据不许出现在里面** ——
// 那是一行会被用户截图发出去的字（见 ConfigView 的「凭据只出不进」）。
func TestModelsSurfacesTheProvidersOwnWordsWithoutTheCredential(t *testing.T) {
	h := newHarness(t)
	h.fake.ModelsErr = errors.New("401 Authentication Fails, Your api key is invalid")

	_, err := h.svc.Models(vlm.Config{BaseURL: "https://刚填的.invalid/v1", APIKey: "刚填的凭据"})
	if err == nil {
		t.Fatal("provider 报错却返回了 nil")
	}
	if !strings.Contains(err.Error(), "401 Authentication Fails") {
		t.Errorf("报错里没有服务方那句话：%v", err)
	}
	if strings.Contains(err.Error(), "刚填的凭据") || strings.Contains(err.Error(), testConfig().APIKey) {
		t.Errorf("报错里带出了凭据：%v", err)
	}
}
