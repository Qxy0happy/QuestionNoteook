package vlm

// ExtractJSONObject 从一段可能夹着寒暄或代码块围栏的文字里截出最外层的 JSON 对象，
// 截不出来返回空串。
//
// 它把 prompt.go 里那个不导出的小函数借出去，给**别的包**用：推荐今日复习量那条路
// （internal/workload）拿到的是同一家模型、同一类回答，容错也该是同一套。
// 那边不自己抄一遍 —— 剥围栏、截最外层大括号这几步两边各写一份，迟早会走偏，
// 而这正是本包当初把「跟模型说话」收成一层的理由（见讨论那条接缝 Ask 的注释）。
//
// 只导出这一个函数，没有顺带把 parseTagReply 也拆出去：那个的产物是分层标签，
// 只有打标签这一条路要它。
func ExtractJSONObject(text string) string { return extractJSONObject(text) }
