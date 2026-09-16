package vlm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// 两个体积上限，都是防御性的：服务方那边另有自己的限制，这里只是别让一次
// 出错的调用把内存吃光、或者把一屏 HTML 塞进日志。
const (
	maxReplyBytes    = 4 << 20 // 回应体最多读这么多（正常回答是几 KB）
	maxErrBodyRunes  = 300     // 报错时从回应里摘多少字给人看
	requestTimeout   = 3 * time.Minute
	defaultUserAgent = "questionbook/1.0"
)

// DeepSeek 是 Provider 在 DeepSeek 上的实现：OpenAI 兼容的 /chat/completions 加 Files API。
//
// 它不持有任何写死的模型名或端点 —— 两者都由 Config 给（ADR-0005）。
// 它也不判「该用视觉模型还是文本模型」：Request.Model 是调用方从配置里取好传进来的，
// provider 只管照着说。
//
// 只用 OpenAI 兼容那一套报文。服务方同时提供 Anthropic 的 /messages 与 Responses API，
// 三条路都能做同一件事，多接一条只是多一处要跟着上游动的东西。
type DeepSeek struct {
	baseURL string
	apiKey  string

	// detail 是内联传图时**显式**带上的高保真档位。
	// Files API 那条路用不上它：服务方明说 detail 对 file_id 的图被忽略（见 imageBlock）。
	detail string

	http *http.Client
}

// NewDeepSeek 按一份配置构造真实 provider。配置先 Validate 过一遍：
// 缺项在这里就说清楚，而不是等到发请求时收到一个 401 再回头猜。
func NewDeepSeek(cfg Config) (*DeepSeek, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return newDeepSeek(cfg.normalized()), nil
}

// newDeepSeek 按一份**已经收拾干净**的配置造 provider，不替调用方验配置。
//
// 单独留这么一条是因为 Models 那一次请求只认端点与凭据（见 validateEndpoint）：
// 它恰恰是**为了挑模型名**才发出去的，拿「缺 vision_model」把它挡下来就成了先有鸡后有蛋。
func newDeepSeek(c Config) *DeepSeek {
	return &DeepSeek{
		baseURL: c.BaseURL,
		apiKey:  c.APIKey,
		detail:  c.detailOrDefault(),
		http:    &http.Client{Timeout: requestTimeout},
	}
}

// Upload 把一张图交给 Files API，返回可跨请求复用的引用（形如 file-api-xxxxxxxx）。
//
// 走 multipart：file 是要传的图，purpose 必填且只有一个取值 user_data。
// **不带 expires_after** —— 不设过期就是长期有效，而同一张题图会被反复引用，
// 设个 30 天到期只会换来一个「某天起又得重传」的哑巴故障。
func (d *DeepSeek) Upload(ctx context.Context, img Image) (string, error) {
	if len(img.Data) == 0 {
		return "", errors.New("VLM: 要传的图是空的")
	}

	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	// 文件名带扩展名：服务方按**内容**判格式（文档原话），但有个像样的名字便于人工排查。
	part, err := w.CreateFormFile("file", "card"+extOf(img.MIME))
	if err != nil {
		return "", fmt.Errorf("VLM: 组装上传报文: %w", err)
	}
	if _, err := part.Write(img.Data); err != nil {
		return "", fmt.Errorf("VLM: 组装上传报文: %w", err)
	}
	if err := w.WriteField("purpose", "user_data"); err != nil {
		return "", fmt.Errorf("VLM: 组装上传报文: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("VLM: 组装上传报文: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+"/files", &body)
	if err != nil {
		return "", fmt.Errorf("VLM: 建上传请求: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	d.authorize(req)

	resp, err := d.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("VLM: 上传图片: %w", err)
	}
	defer resp.Body.Close()

	raw, err := readBody(resp, "上传图片")
	if err != nil {
		return "", err
	}

	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("VLM: 上传图片: 回应不是预期的 JSON: %w", err)
	}
	if out.ID == "" {
		return "", errors.New("VLM: 上传图片: 回应里没有 id")
	}
	return out.ID, nil
}

// Chat 发一次对话。
func (d *DeepSeek) Chat(ctx context.Context, req Request) (Reply, error) {
	raw, err := d.chatBody(req, false)
	if err != nil {
		return Reply{}, err
	}

	resp, err := d.post(ctx, raw)
	if err != nil {
		return Reply{}, err
	}
	defer resp.Body.Close()

	respBody, err := readBody(resp, "调用模型")
	if err != nil {
		return Reply{}, err
	}

	var out chatResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return Reply{}, fmt.Errorf("VLM: 调用模型: 回应不是预期的 JSON: %w", err)
	}
	if len(out.Choices) == 0 {
		return Reply{}, errors.New("VLM: 调用模型: 回应里没有 choices")
	}

	msg := out.Choices[0].Message
	reply := Reply{Text: msg.Content, Reasoning: msg.ReasoningContent}
	// 原样搬过来，不在这里解释 arguments（它可能不是合法 JSON，见 ToolCall.Arguments）。
	for _, c := range msg.ToolCalls {
		reply.ToolCalls = append(reply.ToolCalls, ToolCall{
			ID:        c.ID,
			Name:      c.Function.Name,
			Arguments: c.Function.Arguments,
		})
	}
	return reply, nil
}

// ChatStream 发一次对话，边收边把分片交给 onDelta，返回**拼好的整条**。
//
// 契约（SSE 的行格式、[DONE]、思考模式下 reasoning 一起流）写在 stream.go 的说明里，
// 实现按它写：分片先并进本地缓冲，再原样交出去 —— 返回的 Reply 就是那几个缓冲拼出来的，
// 于是「分片拼起来等于整条」不是一句承诺，而是同一个变量。
//
// 失败一律**原样报上去**（不在这里回落）：回落那件事的归属是调用方（ChatStreaming），
// 因为只有它知道「这一路上还有没有非流式那条退路」。
func (d *DeepSeek) ChatStream(ctx context.Context, req Request, onDelta Sink) (Reply, error) {
	raw, err := d.chatBody(req, true)
	if err != nil {
		return Reply{}, err
	}

	resp, err := d.post(ctx, raw)
	if err != nil {
		return Reply{}, err
	}
	defer resp.Body.Close()

	return readStream(resp.Body, onDelta)
}

// Models 要一次服务方的模型清单（GET /models）。
//
// 这是「校验配置」与「模型下拉」两件事共同的那一次请求，也是**最省**的一次：
// 它一个 token 都不生成（服务方按接口调用计数，不按 token 计费）。
//
// 它验到的只有两件事：**端点通、凭据被认**。验不到的多着呢，别把它当体检报告：
//   - 验不到视觉模型能不能读图 —— 那要真发一张图过去；
//   - 验不到某个模型名存在 —— 清单里的名字随时会变，而用户填的那个可能与清单无关
//     （清单为空的原因也可能是服务方那一侧就没实现这个端点）。
func (d *DeepSeek) Models(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("VLM: 建取模型清单的请求: %w", err)
	}
	d.authorize(req)

	resp, err := d.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("VLM: 取模型清单: %w", err)
	}
	defer resp.Body.Close()

	raw, err := readBody(resp, "取模型清单")
	if err != nil {
		return nil, err
	}

	// 回应的形状（官方文档：object 恒为 "list"，data 里每一项是 {id, object, owned_by}）。
	// 只取 id —— 其余两项在这里没有用处。
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("VLM: 取模型清单: 回应不是预期的 JSON: %w", err)
	}

	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if id := strings.TrimSpace(m.ID); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// chatBody 拼一次对话的报文。stream 为真时服务方回 SSE（见 ChatStream）。
func (d *DeepSeek) chatBody(req Request, stream bool) ([]byte, error) {
	if strings.TrimSpace(req.Model) == "" {
		// 走到这里说明配置缺项从 Validate 那儿漏过去了。宁可当场报清楚，
		// 也不要让服务方替我们挑一个默认模型 —— 那正是「不得硬编码模型 ID」要防的事。
		return nil, fmt.Errorf("%w: 这次请求没给模型名", ErrNotConfigured)
	}

	payload := chatRequest{Model: req.Model, Messages: d.messages(req), Stream: stream}
	if req.JSON {
		// 打标签那条路径要的就是它：回答得能直接解析成结构化标签。
		payload.ResponseFormat = &responseFormat{Type: "json_object"}
	}
	payload.Tools = wireTools(req.Tools)

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("VLM: 组装对话报文: %w", err)
	}
	return raw, nil
}

// post 把一份报文发到 /chat/completions，拿回 2xx 的回应。
//
// 非 2xx 在这里就地变成错误：**流式那条路也一样** —— 那时服务方回的是一整条 JSON 报错，
// 不是 SSE，交给读流的那段代码去解析只会得到一句「不是预期的 JSON」，把真正的原因盖掉。
func (d *DeepSeek) post(ctx context.Context, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("VLM: 建对话请求: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	d.authorize(httpReq)

	resp, err := d.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("VLM: 调用模型: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// readBody 在非 2xx 时必然返回一个错（里面带着服务方的原话）。
		if _, rerr := readBody(resp, "调用模型"); rerr != nil {
			resp.Body.Close()
			return nil, rerr
		}
		resp.Body.Close()
		return nil, fmt.Errorf("VLM: 调用模型: 服务方返回 %s", resp.Status)
	}
	return resp, nil
}

// messages 把本包的 Request 摊成线上的消息数组。
//
// 三条规矩守在这里：
//   - 系统提示词只带文本 —— 图片放进 system 会被服务方以 400 拒掉。
//   - 没有图的消息用纯字符串 content，有图的用块数组。字符串那一形是各家都吃的，
//     块数组只在真有图时才需要，没必要全局改用后者。
//   - 工具调用那两条消息（assistant 带 tool_calls、tool 带 tool_call_id）原样翻译，
//     见 message。
func (d *DeepSeek) messages(req Request) []chatMessage {
	msgs := make([]chatMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, chatMessage{Role: RoleSystem, Content: req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, d.message(m))
	}
	return msgs
}

// message 摊一条消息。
func (d *DeepSeek) message(m Message) chatMessage {
	out := chatMessage{
		Role:       m.Role,
		ToolCallID: m.ToolCallID,
		// 推理过程与工具调用是配套的：官方那份示例把整条 assistant 消息原样喂回去，
		// 所以这边也原样带着（空的不发，见 wire 上的 omitempty）。
		ReasoningContent: m.Reasoning,
	}

	if len(m.Images) > 0 {
		blocks := make([]contentBlock, 0, len(m.Images)+1)
		if m.Text != "" {
			blocks = append(blocks, contentBlock{Type: "text", Text: m.Text})
		}
		for _, img := range m.Images {
			blocks = append(blocks, d.imageBlock(img))
		}
		out.Content = blocks
	} else if m.Text == "" && len(m.ToolCalls) > 0 {
		// 模型那一轮只调工具、没说话时，官方示例里 content 是 null。
		// 发空串多半也能过，但对着示例发 null 少一个「服务方认不认空串」的未知。
		out.Content = nil
	} else {
		out.Content = m.Text
	}

	if len(m.ToolCalls) > 0 {
		out.ToolCalls = wireToolCalls(m.ToolCalls)
	}
	return out
}

// imageBlock 按「这张图传上去过没有」选传法。
//
// 有引用就用引用（Files API 那条路，票据要的「避免同一张图反复上传」），没有就内联 base64。
//
// 高保真那件事两条路不一样，这是复核官方文档时发现的一个坑：
// detail 只对 image_url 生效，**用 file_id 传时它被忽略**。所以「走 Files API」与
// 「显式要高保真」不是同一层上的两个开关 —— 前者换传输方式，后者只在内联那条路上
// 表达得出来。带一个不生效的 detail 进 file_id 报文，只会让人以为设置了什么。
func (d *DeepSeek) imageBlock(img Image) contentBlock {
	if img.FileID != "" {
		return contentBlock{Type: "file", FileID: img.FileID}
	}
	return contentBlock{
		Type: "image_url",
		ImageURL: &imageURL{
			URL:    dataURL(img),
			Detail: d.detail, // 显式给：题面是密集文档，默认档位不该由我们猜
		},
	}
}

// authorize 把凭据放进请求头。**只放在这里**，绝不进日志或报错。
func (d *DeepSeek) authorize(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+d.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", defaultUserAgent)
}

// readBody 读出回应体；非 2xx 时把状态码与一小段回应体包成错误。
//
// 摘一小段原话是刻意的：服务方抱怨什么（模型名不对、图太大、detail 取值不认）
// 全在回应体里，光有状态码是查不动的。回应体里不会有凭据 —— 我们从没把它放进去过。
func readBody(resp *http.Response, what string) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes))
	if err != nil {
		return nil, fmt.Errorf("VLM: %s: 读回应: %w", what, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("VLM: %s: 服务方返回 %s: %s", what, resp.Status, snippet(raw))
	}
	return raw, nil
}

// snippet 把一段回应体压成一行短句，够看清它在抱怨什么就行。
//
// 按**字**截而不是按字节截：中文报错很常见，按字节切会把最后一个字劈成乱码。
func snippet(raw []byte) string {
	s := strings.Join(strings.Fields(string(raw)), " ")
	if r := []rune(s); len(r) > maxErrBodyRunes {
		s = string(r[:maxErrBodyRunes]) + "…"
	}
	return s
}

// dataURL 拼内联传图用的 data URL。
//
// MIME 缺省成 image/png：本应用存下来的题图就是 PNG，缺省值与实际一致。
func dataURL(img Image) string {
	mime := img.MIME
	if mime == "" {
		mime = "image/png"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img.Data)
}

// extOf 从媒体类型推一个扩展名，只用于上传时的文件名。
func extOf(mime string) string {
	switch mime {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

// 线上报文的形状。字段名照服务方文档抄，别顺手改。

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	// Stream 为真时服务方回 SSE（text/event-stream）而不是一整条 JSON，见 ChatStream / readStream。
	Stream         bool            `json:"stream,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	// 没有 tool_choice：有工具时服务方默认就是 auto，而 required / 指定函数在思考模式下
	// 会被 400 拒掉（官方文档），所以这个旋钮根本不接出来。
	Tools []toolSpec `json:"tools,omitempty"`
}

// chatMessage 的 Content 是 any：纯文本时是字符串，带图时是块数组，只调工具时是 null。
// 三种形状服务方都认，见 message。
type chatMessage struct {
	Role    Role `json:"role"`
	Content any  `json:"content"`
	// ToolCalls 只在 assistant 消息上出现；ToolCallID 只在 tool 消息上出现。
	ToolCalls  []toolCallWire `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	// ReasoningContent 只在思考模式下出现，且必须跟着带 tool_calls 的那条消息一起回去。
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type contentBlock struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	FileID   string    `json:"file_id,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type responseFormat struct {
	Type string `json:"type"`
}

// toolSpec 是 tools 数组里的一项。字段名照官方文档抄。
type toolSpec struct {
	Type     string       `json:"type"` // 恒为 function —— 官方说目前只支持函数这一类
	Function functionSpec `json:"function"`
}

type functionSpec struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// 原样转发声明方给的 JSON Schema（见 Tool.Parameters）。
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

// toolCallWire 是模型回来的那一次工具调用。arguments 是**字符串**，不是对象。
type toolCallWire struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
			// 思考模式下与 tool_calls 配套的那段推理过程，见 Message.Reasoning。
			ReasoningContent string         `json:"reasoning_content"`
			ToolCalls        []toolCallWire `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
}

// 流式那条路的报文形状（stream:true）。与上面非流式的差别只有一处：
// 内容在 choices[0].delta 上，而且**一片只有一点点**。
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string       `json:"content"`
			ReasoningContent string       `json:"reasoning_content"`
			ToolCalls        []streamCall `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
}

// streamCall 是一片工具调用碎片。第一片带 id / type / function.name，
// 之后几片只给 function.arguments 的一段；前后片靠 index 认亲（见 absorbing.callAt）。
type streamCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// doneMarker 是服务方给的结束标记，单独一行 `data: [DONE]`。
const doneMarker = "[DONE]"

// absorbing 攒一次流式的中间结果。
//
// 正文与思考过程各攒一份、**不合并** —— 官方那份流式样例就是这么做的，
// 于是 Reply 上那两个字段与非流式那条路一一对应。
type absorbing struct {
	text      strings.Builder
	reasoning strings.Builder
	calls     []toolCallWire
}

// absorb 把一片增量并进缓冲，并把这一片**新增的内容**原样交给 onDelta。
//
// 交出去的与并进去的是同一份字符串：返回值（整条）与播给界面的分片因此天然一致，
// 不需要另外去保证「分片拼起来等于整条」。
func (a *absorbing) absorb(c streamChunk, onDelta Sink) {
	if len(c.Choices) == 0 {
		// 只带 usage 之类的一片，没有正文。
		return
	}
	d := c.Choices[0].Delta // 与 Chat 一样：只看第一条 choice（n 从没传过，恒为一条）

	for _, tc := range d.ToolCalls {
		a.callAt(tc)
	}
	if d.Content != "" {
		a.text.WriteString(d.Content)
	}
	if d.ReasoningContent != "" {
		a.reasoning.WriteString(d.ReasoningContent)
	}
	if onDelta != nil && (d.Content != "" || d.ReasoningContent != "") {
		onDelta(Delta{Text: d.Content, Reasoning: d.ReasoningContent})
	}
}

// callAt 把一片工具调用碎片并到它该在的位置上。
//
// index 是碎片自带的位次（官方规定要有它）。id、函数名只有第一片带，
// 后面几片同样会带 index、但是空的 —— 所以「非空才覆盖」，
// 而 arguments 是**要拼起来**的那一段，只有它用 += 。
func (a *absorbing) callAt(f streamCall) {
	i := f.Index
	for len(a.calls) <= i {
		a.calls = append(a.calls, toolCallWire{})
	}
	c := &a.calls[i]
	if f.ID != "" {
		c.ID = f.ID
	}
	if f.Type != "" {
		c.Type = f.Type
	}
	if f.Function.Name != "" {
		c.Function.Name = f.Function.Name
	}
	c.Function.Arguments += f.Function.Arguments
}

// sseData 从 SSE 的一行里取出 data 的载荷。
//
// 行格式是 SSE 通用那套：前缀 `data:`，冒号后那一个空格是**可选**的装饰；
// 以 `:` 开头的是注释行（服务方拿它做 keep-alive），`event:` / `id:` / `retry:`
// 这几样这里一个都不用 —— 官方只发 data 行。
// ok 为假就是「这一行不是一条增量」，跳过即可。
func sseData(line string) (string, bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	return strings.TrimSpace(line[len("data:"):]), true
}

// readStream 把一次流式回应的正文读到底：边读边播，返回拼好的整条。
//
// 结束条件是**读到 `data: [DONE]`**，只靠「连接关了」不算数：
// 中间隔一层代理或者网络抖一下时，半截回应也可能干干净净地结束。
// 认了它就会把一段半截话当成整条答案落库（Discussion 里模型回答最后是落库的），
// 而那是一句错话留在用户的错题本里 —— 比多花一次请求贵得多。
// 于是这里宁可报错，交给 ChatStreaming 去回落。代价写在它那儿：重发一次完整请求。
//
// 出错时返回的 Reply 是空的：已经流出去的那半截由调用方在收尾时整条替换掉，
// 不是靠这里把半截拼回去（见 Discussion.svelte 的 live 那一块）。
func readStream(body io.Reader, onDelta Sink) (Reply, error) {
	var acc absorbing
	done := false

	sc := bufio.NewScanner(body)
	// 默认上限 64KB 不够用：一片特别长的正文、或者一张工具调用表的分片都可能超过它，
	// 撞上了 Scanner 会拿一个 ErrTooLong 把整次流式判死。
	sc.Buffer(make([]byte, 0, 64*1024), maxReplyBytes)

	for sc.Scan() {
		payload, ok := sseData(sc.Text())
		if !ok {
			continue
		}
		if payload == doneMarker {
			done = true
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return Reply{}, fmt.Errorf("VLM: 调用模型: 流里的分片不是预期的 JSON: %w", err)
		}
		acc.absorb(chunk, onDelta)
	}
	if err := sc.Err(); err != nil {
		return Reply{}, fmt.Errorf("VLM: 调用模型: 读流: %w", err)
	}
	if !done {
		return Reply{}, errors.New("VLM: 调用模型: 流没读到结束标记就断了")
	}

	reply := Reply{Text: acc.text.String(), Reasoning: acc.reasoning.String()}
	// 与非流式那条路一样：原样搬，不在这里解释 arguments（见 ToolCall.Arguments）。
	for _, c := range acc.calls {
		reply.ToolCalls = append(reply.ToolCalls, ToolCall{
			ID:        c.ID,
			Name:      c.Function.Name,
			Arguments: c.Function.Arguments,
		})
	}
	return reply, nil
}

// wireTools 把本包的工具清单摊成线上的 tools 数组；没有工具时返回 nil（omitempty 就不发）。
func wireTools(tools []Tool) []toolSpec {
	if len(tools) == 0 {
		return nil
	}
	out := make([]toolSpec, 0, len(tools))
	for _, t := range tools {
		out = append(out, toolSpec{
			Type:     "function",
			Function: functionSpec{Name: t.Name, Description: t.Description, Parameters: t.Parameters},
		})
	}
	return out
}

// wireToolCalls 把模型那轮的调用摊回报文。type 恒为 function（官方目前只支持这一类）。
func wireToolCalls(calls []ToolCall) []toolCallWire {
	out := make([]toolCallWire, 0, len(calls))
	for _, c := range calls {
		var w toolCallWire
		w.ID = c.ID
		w.Type = "function"
		w.Function.Name = c.Name
		w.Function.Arguments = c.Arguments
		out = append(out, w)
	}
	return out
}
