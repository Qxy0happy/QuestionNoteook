package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"questionbook/internal/tags"
	"questionbook/internal/vlm"
)

// 工具名。只用小写字母与下划线 —— 服务方对名字的限制是 a-z A-Z 0-9 _ -，最长 128。
const (
	toolListTags      = "list_tags"
	toolListQuestions = "list_questions"
	toolReviewStats   = "review_stats"
	toolReviewHistory = "review_history"
	toolProposeChange = "propose_tag_change"
)

// 读的上限：**放进上下文的量**由这几个数卡住。
//
// 为什么卡在这里而不是「不许读」：读多少与给模型看多少是两件事。个人错题本整本读回来
// 是几百行的小事（spec 里那句「量级小」），但把几百行塞进上下文既贵又没用 ——
// 模型要的是「先看一眼总量，再决定翻哪一页」。所以数据一次读全（快照），
// 交给模型的每一页有上限，并且回话里带着总数，翻不翻页由它自己判断。
const (
	defaultPage    = 50  // 不带 limit 时给多少条错题
	maxPage        = 200 // 一页最多多少条
	defaultHistory = 50  // 不带 limit 时给多少条复习记录
	maxHistory     = 200 // 复习记录一次最多多少条
)

// toolDecls 是交给模型的工具清单。
//
// 五个工具的分工：前四个只读（列标签、列错题、看汇总、看某道题的复习史），
// 最后一个只**提议**。也就是说这一层给模型的能力里，写的那一半是不存在的 ——
// 它连一个「直接改」的工具都没有可挑。
//
// 描述写中文：这些字是给模型看的提示词的一部分，与系统提示词同一口气，
// 而且它们讲的都是这个应用里的概念（错题、标签、复习），中文更省 token 也更准。
func toolDecls() []vlm.Tool {
	return []vlm.Tool{
		{
			Name:        toolListTags,
			Description: "列出全部标签（三层树：学科 > 章节 > 知识点），平铺返回，每项带 id、parent_id、name、level 与整条 path。要用标签、或者要提议改标签之前，先用它拿到 id。",
			Parameters:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		},
		{
			Name:        toolListQuestions,
			Description: "列出错题，新的在前，每道题带上它的标签与复习概况（复习过没有、复习次数、重来次数、下次到期）。默认最多 50 条，返回里带总数，要往后翻就用 offset。",
			Parameters: json.RawMessage(`{"type":"object","properties":{
				"tag_ids":{"type":"array","items":{"type":"integer"},"description":"只看挂了这些标签（含它们的子孙）的错题；不传表示全部。"},
				"limit":{"type":"integer","description":"最多返回几条，默认 50，上限 200。"},
				"offset":{"type":"integer","description":"跳过前几条，用于翻页。"}
			},"additionalProperties":false}`),
		},
		{
			Name:        toolReviewStats,
			Description: "按标签汇总复习情况：每个标签下有多少题、复习过多少、今天到期多少、累计复习次数与重来次数、最近一次复习是什么时候。每个标签的数字**已经含它的子孙**，所以看学科那一行就等于看它整棵树。「哪块最弱」这类问题看它：重来次数高、复习次数低的就是弱的地方。",
			Parameters:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		},
		{
			Name:        toolReviewHistory,
			Description: "看某一道错题的复习记录，最近的在先。评级是 Again / Hard / Good / Easy 四档。想看「这道题为什么老是错」时用它。",
			Parameters: json.RawMessage(`{"type":"object","properties":{
				"question_id":{"type":"integer","description":"哪道错题，id 从 list_questions 拿。"},
				"limit":{"type":"integer","description":"最多返回几条，默认 50，上限 200。"}
			},"required":["question_id"],"additionalProperties":false}`),
		},
		{
			Name:        toolProposeChange,
			Description: "提议一条标签改动，交给用户批准。它**不会立刻生效** —— 会落进「待批准改动」列表，用户逐条点头才生效。一次只提一件要改的事；提完之后还要用你自己的话告诉用户你建议改什么、为什么。",
			Parameters: json.RawMessage(`{"type":"object","properties":{
				"action":{"type":"string","enum":["create_tag","rename_tag","delete_tag","tag_question"],"description":"要干什么。create_tag 建标签；rename_tag 改名；delete_tag 删标签（连同它下面整棵子树）；tag_question 换掉某道题挂着的标签。"},
				"reason":{"type":"string","description":"为什么这么改，一句话。必填。"},
				"parent_id":{"type":"integer","description":"create_tag 用：新标签挂在谁下面；0 表示顶层学科。"},
				"tag_id":{"type":"integer","description":"rename_tag / delete_tag 用：改哪个标签。"},
				"name":{"type":"string","description":"create_tag / rename_tag 用：标签名。"},
				"question_id":{"type":"integer","description":"tag_question 用：哪道错题。"},
				"tag_ids":{"type":"array","items":{"type":"integer"},"description":"tag_question 用：换上这组标签，**整体替换**（空数组表示把这道题的标签全摘掉）。一般要把路径上每一层都带上（学科、章节、知识点）。"}
			},"required":["action","reason"],"additionalProperties":false}`),
		},
	}
}

// ── 分发 ──

// call 执行一次工具调用，返回要喂回模型的那段文本。
//
// 它**永远返回一段东西，从不返回 error** —— 这是有意的。模型挑错工具、参数写歪、
// 引用了不存在的 id，全都是它自己改得过来的事：把这些变成一次 Go 错误就等于把循环打断，
// 而正确做法是把「哪里不对」喂回去让它重试。
//
// 官方文档特意提醒过 arguments 是**字符串**且「不总是合法 JSON」，所以「解不开」在这里
// 是最常见的一种喂回去——见 decode。
func (r *runner) call(call vlm.ToolCall) string {
	switch call.Name {
	case toolListTags:
		return r.doListTags()
	case toolListQuestions:
		args, bad := decode[listQuestionsArgs](call.Arguments)
		if bad != "" {
			return bad
		}
		return r.doListQuestions(args)
	case toolReviewStats:
		return r.doReviewStats()
	case toolReviewHistory:
		args, bad := decode[reviewHistoryArgs](call.Arguments)
		if bad != "" {
			return bad
		}
		return r.doReviewHistory(args)
	case toolProposeChange:
		args, bad := decode[proposeChangeArgs](call.Arguments)
		if bad != "" {
			return bad
		}
		return r.doProposeChange(args)
	}
	return toolError("没有叫 %q 的工具。可用的有：%s", call.Name,
		strings.Join([]string{toolListTags, toolListQuestions, toolReviewStats, toolReviewHistory, toolProposeChange}, "、"))
}

// decode 把模型给的 arguments 解成 T。
//
// 解不开时第二个返回值是一段给模型看的话（空的表示解开了）。空字符串当 `{}` 收下 ——
// 一个没有参数的工具，模型经常直接给空串。
func decode[T any](raw string) (T, string) {
	var v T
	if strings.TrimSpace(raw) == "" {
		raw = "{}"
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return v, toolError("参数不是合法的 JSON：%v。请按这个工具的 schema 重新生成参数。", err)
	}
	return v, ""
}

// toolError 造一段给模型看的失败说明。
//
// 回给模型的是一个**对象**而不是一句裸文本：工具结果一律是 JSON，模型解析起来一致。
func toolError(format string, a ...any) string {
	return encode(map[string]string{"error": fmt.Sprintf(format, a...)})
}

// encode 把结果编成 JSON 文本。编不出来时说一句，而不是给模型一个空串。
func encode(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return `{"error":"内部错误：结果编不成 JSON"}`
	}
	return string(raw)
}

// ── list_tags ──

func (r *runner) doListTags() string {
	all, err := r.snap.tagsList()
	if err != nil {
		return toolError("读标签失败：%v", err)
	}

	type row struct {
		ID        int64  `json:"id"`
		ParentID  int64  `json:"parent_id"`
		Name      string `json:"name"`
		Level     int    `json:"level"`
		LevelName string `json:"level_name"`
		Path      string `json:"path"`
	}
	rows := make([]row, 0, len(all))
	for _, t := range all {
		rows = append(rows, row{
			ID:        t.ID,
			ParentID:  t.ParentID,
			Name:      t.Name,
			Level:     int(t.Level),
			LevelName: t.Level.String(),
			Path:      r.snap.path(t.ID),
		})
	}
	return encode(map[string]any{"count": len(rows), "tags": rows})
}

// ── list_questions ──

type listQuestionsArgs struct {
	TagIDs []int64 `json:"tag_ids"`
	Limit  int     `json:"limit"`
	Offset int     `json:"offset"`
}

func (r *runner) doListQuestions(args listQuestionsArgs) string {
	all, err := r.snap.questionsList()
	if err != nil {
		return toolError("读错题失败：%v", err)
	}

	// 按标签筛：点一个学科要带出它下面所有章节与知识点的题，否则三层树在筛选上就没意义
	// （与 tags.QuestionsByTags 同一条规矩）。所以这里比的是**子树**，不是标签本身。
	var wanted map[int64]bool
	if len(args.TagIDs) > 0 {
		wanted = map[int64]bool{}
		for _, id := range args.TagIDs {
			if _, ok, err := r.snap.tagByID(id); err != nil {
				return toolError("读标签失败：%v", err)
			} else if !ok {
				return toolError("%v", errUnknownTag(id))
			}
			for in := range r.snap.subtree(id) {
				wanted[in] = true
			}
		}
	}

	filtered := make([]QuestionView, 0, len(all))
	for _, q := range all {
		if wanted == nil || inSubtreeOfAny(q.Tags, wanted) {
			filtered = append(filtered, q)
		}
	}

	limit := clamp(args.Limit, defaultPage, maxPage)
	offset := args.Offset
	if offset < 0 {
		offset = 0
	}
	if offset > len(filtered) {
		offset = len(filtered)
	}
	end := offset + limit
	if end > len(filtered) {
		end = len(filtered)
	}

	rows := make([]map[string]any, 0, end-offset)
	for _, q := range filtered[offset:end] {
		rows = append(rows, questionRow(q, r.snap))
	}
	return encode(map[string]any{
		"total":     len(filtered),
		"offset":    offset,
		"count":     len(rows),
		"questions": rows,
	})
}

// questionRow 把一道错题摊成给模型看的那一行。
//
// 时间**只到日**：模型要回答的是「最近拍的」「什么时候该复习」这一类问题，
// 毫秒在这里只是噪声，还占上下文。日期按月-日给（2026-09-16），是本地的日子 ——
// store 那侧已经把时刻换算到当前时区了（见 store.ReadStore 的说明）。
func questionRow(q QuestionView, snap *snapshot) map[string]any {
	paths := make([]string, 0, len(q.Tags))
	for _, t := range q.Tags {
		paths = append(paths, snap.path(t.ID))
	}
	row := map[string]any{
		"id":         q.ID,
		"created_at": day(q.CreatedAt),
		"has_answer": q.HasAnswer,
		"tags":       paths,
		"reviewed":   q.Reviewed,
		"reps":       q.Reps,
		"lapses":     q.Lapses,
	}
	if q.Reviewed {
		row["due"] = day(q.Due)
		row["last_review"] = day(q.LastReview)
	}
	return row
}

// ── review_stats ──

func (r *runner) doReviewStats() string {
	stats, err := r.snap.statsList()
	if err != nil {
		return toolError("读复习汇总失败：%v", err)
	}

	type row struct {
		TagID       int64  `json:"tag_id"`
		Name        string `json:"name"`
		Path        string `json:"path"`
		Level       int    `json:"level"`
		LevelName   string `json:"level_name"`
		Questions   int    `json:"questions"`
		Reviewed    int    `json:"reviewed"`
		NotReviewed int    `json:"not_reviewed"`
		Due         int    `json:"due"`
		Reps        int    `json:"reps"`
		Lapses      int    `json:"lapses"`
		LastReview  string `json:"last_review,omitempty"`
	}
	rows := make([]row, 0, len(stats))
	for _, t := range stats {
		s := row{
			TagID:       t.TagID,
			Name:        t.Name,
			Path:        t.Path,
			Level:       int(t.Level),
			LevelName:   t.Level.String(),
			Questions:   t.Questions,
			Reviewed:    t.Reviewed,
			NotReviewed: t.Questions - t.Reviewed,
			Due:         t.Due,
			Reps:        t.Reps,
			Lapses:      t.Lapses,
		}
		if !t.LastReview.IsZero() {
			s.LastReview = day(t.LastReview)
		}
		rows = append(rows, s)
	}
	return encode(map[string]any{
		"note": "每个标签的计数已经含它的子孙。reviewed 是复习过的题数，lapses 是「重来」次数 —— 重来多而复习少的地方就是弱的地方。",
		"tags": rows,
	})
}

// ── review_history ──

type reviewHistoryArgs struct {
	QuestionID int64 `json:"question_id"`
	Limit      int   `json:"limit"`
}

func (r *runner) doReviewHistory(args reviewHistoryArgs) string {
	if args.QuestionID == 0 {
		return toolError("要给 question_id：看哪道题的复习记录。")
	}
	if _, ok, err := r.snap.questionByID(args.QuestionID); err != nil {
		return toolError("读错题失败：%v", err)
	} else if !ok {
		return toolError("%v", errUnknownQuestion(args.QuestionID))
	}

	entries, err := r.snap.reader.ReviewHistory(args.QuestionID, clamp(args.Limit, defaultHistory, maxHistory))
	if err != nil {
		return toolError("读复习记录失败：%v", err)
	}

	type row struct {
		Rating        int    `json:"rating"`
		RatingName    string `json:"rating_name"`
		ReviewedAt    string `json:"reviewed_at"`
		ScheduledDays int    `json:"scheduled_days"`
		DueAt         string `json:"due_at"`
	}
	rows := make([]row, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, row{
			Rating:        e.Rating,
			RatingName:    ratingName(e.Rating),
			ReviewedAt:    day(e.ReviewedAt),
			ScheduledDays: e.ScheduledDays,
			DueAt:         day(e.DueAt),
		})
	}
	return encode(map[string]any{"question_id": args.QuestionID, "count": len(rows), "entries": rows})
}

// ratingName 把评级翻成它在本应用里的名字（CONTEXT.md 的四个词，与 FSRS 一致）。
func ratingName(r int) string {
	switch r {
	case 1:
		return "Again"
	case 2:
		return "Hard"
	case 3:
		return "Good"
	case 4:
		return "Easy"
	}
	return fmt.Sprintf("%d", r)
}

// ── propose_tag_change ──

type proposeChangeArgs struct {
	Action     Action  `json:"action"`
	Reason     string  `json:"reason"`
	ParentID   int64   `json:"parent_id"`
	TagID      int64   `json:"tag_id"`
	Name       string  `json:"name"`
	QuestionID int64   `json:"question_id"`
	TagIDs     []int64 `json:"tag_ids"`
}

// doProposeChange 是这一层唯一改变世界状态的动作 —— 而它改的只是**待批准**那一张表。
//
// 三件事按顺序做，缺一不可：
//
//  1. 先把参数落成一句话（summarize）。这一步同时**替模型把事情核对了一遍**：
//     引用的标签 / 错题在不在、会不会挂到第四层、删除会波及多少。核对不过就把话喂回去，
//     让它改 —— 而不是让用户面对一条根本执行不了的提议。
//  2. 摘要由**我们**写，不是模型的原话。用户点头之前该读到一句陈述事实的话
//     （改哪个、波及多少），而不是模型的劝说。模型只提供动作、参数与理由。
//  3. 落库。到这一步为止，正式数据一个字节都没动。
func (r *runner) doProposeChange(args proposeChangeArgs) string {
	if len(r.proposals) >= maxProposals {
		return toolError("这一轮提议的改动已经有 %d 条了，够了。先停下来，把已经提的那些跟用户说清楚。", maxProposals)
	}
	if strings.TrimSpace(args.Reason) == "" {
		return toolError("reason 是必填的：说清为什么这么改，用户是照着它点头的。")
	}

	c := Change{
		Action: args.Action,
		Payload: Payload{
			ParentID:   args.ParentID,
			TagID:      args.TagID,
			QuestionID: args.QuestionID,
			Name:       strings.TrimSpace(args.Name),
			TagIDs:     args.TagIDs,
		},
		Reason: strings.TrimSpace(args.Reason),
	}

	summary, err := r.summarize(c)
	if err != nil {
		return toolError("%v", err)
	}
	c.Summary = summary
	if err := c.Validate(); err != nil {
		return toolError("%v", err)
	}

	saved, err := r.proposer.Propose(c)
	if err != nil {
		return toolError("记下这条改动失败：%v", err)
	}
	r.proposals = append(r.proposals, saved)

	return encode(map[string]any{
		"id":      saved.ID,
		"status":  string(saved.Status),
		"summary": saved.Summary,
		"note":    "已经记进「待批准改动」列表了，**还没有生效**。用户点头之后才会真的执行。",
	})
}

// summarize 把一条改动摊成一句给人看的中文，顺便把这件事能不能做核对一遍。
//
// 它返回的错是**给模型看**的（哪一步对不上，它自己改得过来），不是故障。
func (r *runner) summarize(c Change) (string, error) {
	switch c.Action {
	case ActionCreateTag:
		if c.Payload.ParentID == 0 {
			return fmt.Sprintf("新建顶层学科「%s」", c.Payload.Name), nil
		}
		parent, ok, err := r.snap.tagByID(c.Payload.ParentID)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", errUnknownTag(c.Payload.ParentID)
		}
		if parent.Level >= tags.MaxLevel {
			// 三层是硬约束（CONTEXT.md 的标签：学科 > 章节 > 知识点）。
			// 在这里拦住而不是等落库时失败：模型收到这句话就能换个父节点重来。
			return "", fmt.Errorf("「%s」已经是%s了，标签只有三层，下面不能再挂节点",
				r.snap.path(parent.ID), parent.Level.String())
		}
		return fmt.Sprintf("在「%s」下新建%s「%s」",
			r.snap.path(parent.ID), (parent.Level + 1).String(), c.Payload.Name), nil

	case ActionRenameTag:
		t, ok, err := r.snap.tagByID(c.Payload.TagID)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", errUnknownTag(c.Payload.TagID)
		}
		return fmt.Sprintf("把「%s」改名为「%s」", r.snap.path(t.ID), c.Payload.Name), nil

	case ActionDeleteTag:
		t, ok, err := r.snap.tagByID(c.Payload.TagID)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", errUnknownTag(c.Payload.TagID)
		}
		// 删除是**级联**的（tags.Service.Delete）：连子树一起删，错题上的这些标签也一并摘掉。
		// 波及面必须在摘要里说清楚 —— 那正是用户点头之前要知道的事。
		sub := r.snap.subtree(t.ID)
		q, err := r.snap.questionsList()
		if err != nil {
			return "", err
		}
		affected := 0
		for _, one := range q {
			if inSubtreeOfAny(one.Tags, sub) {
				affected++
			}
		}
		kids := r.snap.descendants(t.ID)
		return fmt.Sprintf("删除「%s」（连同它下面 %d 个子标签，会从 %d 道错题上摘掉这些标签）",
			r.snap.path(t.ID), kids, affected), nil

	case ActionTagQuestion:
		q, ok, err := r.snap.questionByID(c.Payload.QuestionID)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", errUnknownQuestion(c.Payload.QuestionID)
		}

		// 每个都要在树里，否则这条提议执行不了。
		names := make([]string, 0, len(c.Payload.TagIDs))
		for _, id := range c.Payload.TagIDs {
			if _, ok, err := r.snap.tagByID(id); err != nil {
				return "", err
			} else if !ok {
				return "", errUnknownTag(id)
			}
			names = append(names, r.snap.path(id))
		}

		// 这是**整体替换**（与手工打标签、与 SaveTags 同一个语义），所以要说清楚
		// 少了哪些 —— 悄悄摘掉用户自己挂的标签是最不该发生的事。
		before := map[int64]bool{}
		for _, t := range q.Tags {
			before[t.ID] = true
		}
		after := map[int64]bool{}
		for _, id := range c.Payload.TagIDs {
			after[id] = true
		}
		var added, removed []string
		for _, t := range q.Tags {
			if !after[t.ID] {
				removed = append(removed, r.snap.path(t.ID))
			}
		}
		for _, id := range c.Payload.TagIDs {
			if !before[id] {
				added = append(added, r.snap.path(id))
			}
		}

		summary := fmt.Sprintf("把错题 #%d 挂着的标签整体换成 %d 个", q.ID, len(names))
		if len(added) > 0 {
			summary += fmt.Sprintf("；新增%s", quoteAll(added))
		}
		if len(removed) > 0 {
			summary += fmt.Sprintf("；摘掉%s", quoteAll(removed))
		}
		return summary, nil
	}

	return "", fmt.Errorf("%w: 不认识的动作 %q", ErrBadChange, c.Action)
}

// quoteAll 把一串名字拼成「「甲」「乙」」。
func quoteAll(names []string) string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, "「"+n+"」")
	}
	return strings.Join(out, "")
}

// clamp 把 limit 收进 [1, max]；没给（0 或负数）时给 def。
func clamp(v, def, max int) int {
	if v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

// day 把一个时刻按**它自己的时区**写成日期。
//
// store 那侧读出来的时刻已经换算到当前时区了（见 store.ReadStore 的说明），
// 所以这里直接格式化就是本地日子，与「今天到期」的判定用的是同一个时区。
func day(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02")
}
