package workload

import (
	"fmt"
	"strings"
)

// recommendPrompt 是推荐复习量用的系统提示词。
//
// ⚠️ **暂定**。与 vlm 里那两份提示词同一个处境：措辞还没有在真实数据上试过。
// 这里定死的只有**接口契约**那部分 —— 回答是一个 JSON 对象，里面有 suggest 与 reason
// （代码要解析它们）；其余（怎么措辞、要不要给例子、要不要交代「他是考研的、题是印刷体」）
// 等真跑过再调。
//
// 与配置里的 tag_prompt 不同，这一份没有留覆盖口子：那一处留口子是因为打标签的质量
// 还没有结论（票据 01），而这里连一次真实的推荐都还没跑过，先不给一个没人调过的旋钮。
//
// 「拿不准就偏少」那一条是这一层的**目的**，不是一句客套：用户会来要这个推荐，正是
// 因为他不想被几百道淹没（story 25）。一个偏大的数达不到这个目的。
const recommendPrompt = `你在帮一个考研学生决定今天还要再复习几道错题。

他手上有一个按遗忘曲线排好的复习队列，现在还有若干道没做。全做完可能压垮他，
做得太少又会让积压越滚越大。你的任务是给一个「今天再做几道」的数。

你会看到几个事实：队列里现在还有多少道、其中多少道是他从没复习过的、积压了多久，
以及他最近一段时间的复习记录（做了多少次、四档评级各占多少、有几天动过）。

要求：
1. 给的数必须在 1 到「队列里还有多少道」之间。
2. 拿不准就**偏少**：连着几天做不完的队列，会让人干脆不打开它。
3. 主要看他最近的表现：Again 多、或者好几天没动过，就再少给一点；
   最近明显轻松（Good 与 Easy 占多数、而且几乎天天在动）就可以多给。
4. 只输出一个 JSON 对象，形如：
   {"suggest":12,"reason":"最近忘得有点多，先做 12 道"}
   suggest 是数字，不要加引号；reason 是中文的一句话，20 个字以内，说清为什么是这个数。
   不要解释、不要用代码块包起来、不要输出 JSON 以外的任何字。`

// digest 是交给模型的那份**事实**。
//
// 这里是这一票里唯一一处定义「最近表现长什么样」的地方，而它只由 review_logs 的原始列
// 拼出来（见 Store.recent），一个形容词都不加：「最近状态不好」这种结论该由模型从数里
// 得出来，而不是由我们替它下 —— 我们下的结论它没法质疑，也就没法在别处用上。
type digest struct {
	Due         int    // 队列里现在还有多少道
	OverdueDays int    // 队头那道拖了几天（0 = 今天才到期）
	Fresh       int    // 其中从没复习过的
	Recent      Recent // 最近这一段的表现
}

// text 把这份事实摊成要发出去的那几行。
//
// 写成给**人**看的样子而不是紧凑的 JSON，是因为模型读它跟人读它没差别，而人还要读它 ——
// 调提示词时得能一眼看出喂进去了什么（测试里也是这么断言它的，见 workload_test.go）。
func (d digest) text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "队列里现在还有：%d 道\n", d.Due)
	if d.Fresh > 0 {
		fmt.Fprintf(&b, "其中从没复习过的：%d 道\n", d.Fresh)
	}
	if d.OverdueDays > 0 {
		fmt.Fprintf(&b, "拖得最久的一道：已经逾期 %d 天\n", d.OverdueDays)
	}

	r := d.Recent
	if r.Reviews == 0 {
		// 一次都没复习过也是一个**事实**，而且是最该说出口的那个：他断更了。
		fmt.Fprintf(&b, "最近 %d 天：一次都没复习过\n", r.Days)
		return b.String()
	}
	fmt.Fprintf(&b, "最近 %d 天：复习 %d 次，涉及 %d 道题，其中 %d 天有复习\n",
		r.Days, r.Reviews, r.Questions, r.ActiveDays)
	fmt.Fprintf(&b, "最近 %d 天的评级：Again %d / Hard %d / Good %d / Easy %d\n",
		r.Days, r.Again, r.Hard, r.Good, r.Easy)
	return b.String()
}
