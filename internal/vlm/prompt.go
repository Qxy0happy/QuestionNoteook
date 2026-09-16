package vlm

import (
	"encoding/json"
	"fmt"
	"strings"

	"questionbook/internal/tags"
)

// 打标签的提示词。
//
// ⚠️ **暂定**。票据 09 明说「先看 01 的结论，据此定打标签的 prompt」，而 01
// （拿真实印刷体照片验一次标签质量）还没做。所以这里定死的只有**接口契约**那部分：
// 三层固定为 学科 > 章节 > 知识点（CONTEXT.md 的层级），回答是 JSON 对象（代码要解析它）。
// 至于怎么问、给不给示例、要不要交代「题面是印刷体」、一条题最多给几条 —— 全是占位，
// 等 01 的结论回来再改，改的时候只动这一个文件。
//
// 本包**不自创词表、不自创分类体系**：那是 01 要定的事。这里唯一用到的词表是
// 用户自己已经在用的那份（buildTagPrompt 附在末尾当参考）。
//
// 想不改代码就试措辞：配置里的 tag_prompt 一旦非空，整份内置提示词都被它顶掉。

// maxSuggestedTags 是一道题最多要几条建议。
//
// 暂定值，同样等 01。给上限是因为没有上限时模型会一口气列七八条，
// 用户逐条改的负担比它省下的还大。
const maxSuggestedTags = 3

// tagUserPrompt 是随图发出去的那句话。
//
// 指令在系统提示词里，这里只交代「这张图是什么」。单独拎出来是因为它跟系统提示词一样
// 属于暂定，而且它**必须**待在 user 消息里 —— 图片只能放 user 消息，
// 所以图与它的那段文字得在同一条消息里。
const tagUserPrompt = "这是一道错题的照片（印刷体的题面，可能带公式），请按系统提示的要求给它打标签。"

// buildTagPrompt 拼出内置的打标签提示词。
//
// vocabulary 是用户现有的标签树（平铺，可能为空），会作为**参考**附在后面：
// 用户用得越久越该往他已经有的名字上靠，而不是每次另起一套同义不同名的标签
// （spec 的用户故事 17「标签词表随我用得越久越准」）。
//
// 用「参考」而不是「只能从这里挑」，是因为顶层学科由用户自由填写（用户故事 14）：
// 词表里没有的学科该提就提，提出来由用户决定收不收。
func buildTagPrompt(vocabulary []tags.Tag) string {
	var b strings.Builder
	b.WriteString(`你在帮一个考研学生整理他的错题本。下面这张照片是一道错题。

请给它打**分层**标签，三层固定为：学科 > 章节 > 知识点。
- 学科：最大的一层，比如「数学」「英语」「政治」「专业课」，也可以是别的任何合适的名字。
- 章节：这门学科下的一块，比如「高等数学」「线性代数」。
- 知识点：这一块里的具体考点，比如「中值定理」「特征值」。

要求：
`)
	fmt.Fprintf(&b, "1. 一道题最多给 %d 条，按贴切程度从高到低排。\n", maxSuggestedTags)
	b.WriteString(`2. 三层尽量都给全。哪一层实在定不下来，就把那一层写成空字符串，不要硬凑。
3. 名字用中文，短一点（10 个字以内），不要带编号、书名号或标点。
4. 如果下面「这个学生已有的标签」里已经有合适的名字，**照抄它**，别换个说法另起一个。
   那只是参考：没有合适的就自己起。
5. 只输出一个 JSON 对象，形如：
   {"tags":[{"subject":"数学","chapter":"高等数学","point":"中值定理"}]}
   不要解释、不要用代码块包起来、不要输出 JSON 以外的任何字。
`)

	if len(vocabulary) > 0 {
		b.WriteString("\n这个学生已有的标签（缩进表示层级）：\n")
		b.WriteString(formatVocabulary(vocabulary))
	}
	return b.String()
}

// formatVocabulary 把平铺的标签树摊成带缩进的几行，喂给模型看。
//
// 摊的是**用户自己的树**，不是一份内置词表 —— 所以这里不需要、也不该出现任何
// 硬编码的学科名。按层级缩进就够了：层级字段本来就在标签上（tags.Level），
// 不必自己爬一遍父子关系。
func formatVocabulary(vocabulary []tags.Tag) string {
	var b strings.Builder
	for _, t := range vocabulary {
		indent := strings.Repeat("  ", int(t.Level)-1)
		fmt.Fprintf(&b, "%s- %s\n", indent, t.Name)
	}
	return b.String()
}

// ── 解析回答 ──

// tagWire 是模型回答在**线上**的形状：小写的键。
//
// 为什么不直接往 ProposedTag 上打 json 标签，而要另立一个形状：
// ProposedTag 的字段名同时是 Wails 生成的 TS 属性名（生成器取的是 json 标签名），
// 给它打上 `json:"subject"` 就等于让前端也跟着服务方的报文风格写小写。
// 两边各留一个形状、在这里对一次，比把上游的命名习惯渗进界面干净。
type tagWire struct {
	Tags []struct {
		Subject string `json:"subject"`
		Chapter string `json:"chapter"`
		Point   string `json:"point"`
	} `json:"tags"`
}

// parseTagReply 把模型的回答解析成分层标签建议。
//
// 容错是刻意的：提示词里写了「只要 JSON」也拦不住模型偶尔拿代码块包一层、
// 或者前面加一句「好的，这是标签：」。这里先剥围栏、再截出最外层的那个 {}，
// 剩下的交给 json 包。
//
// 回答里一条可用的都没有（包括「tags 是空数组」这种合法的「我分不出来」）时
// 返回空切片与 nil，由调用方决定怎么跟用户说 —— 那不是解析失败，
// 是模型老老实实说了它没想法。
func parseTagReply(text string) ([]ProposedTag, error) {
	body := extractJSONObject(text)
	if body == "" {
		return nil, ErrBadReply
	}

	var wire tagWire
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadReply, err)
	}

	out := make([]ProposedTag, 0, len(wire.Tags))
	seen := make(map[ProposedTag]bool, len(wire.Tags))
	for _, t := range wire.Tags {
		p := ProposedTag{
			Subject: cleanName(t.Subject),
			Chapter: cleanName(t.Chapter),
			Point:   cleanName(t.Point),
		}
		// 没有学科就不知道该挂到哪棵树下面，整条丢掉。
		if p.Subject == "" {
			continue
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
		if len(out) >= maxSuggestedTags {
			break // 提示词里说了上限，模型不听就替它裁掉
		}
	}
	return out, nil
}

// extractJSONObject 从一段可能夹着寒暄或代码块围栏的文字里截出最外层的 JSON 对象。
// 截不出来返回空串。
func extractJSONObject(text string) string {
	s := text

	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		// 围栏上可能写着语言名（ ```json ）。
		s = strings.TrimPrefix(strings.TrimSpace(s), "json")
		if j := strings.Index(s, "```"); j >= 0 {
			s = s[:j]
		}
	}

	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end < start {
		return ""
	}
	return s[start : end+1]
}

// cleanName 收拾模型给的一个名字：去空白、去它顺手带上的引号与书名号。
//
// 只做这一层清理，不做同义词归并 —— 归并是词表该干的事，不是解析该干的事。
func cleanName(name string) string {
	s := strings.TrimSpace(name)
	s = strings.Trim(s, `"'“”‘’《》`)
	return strings.TrimSpace(s)
}
