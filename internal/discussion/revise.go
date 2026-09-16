package discussion

import "strings"

// 这一份是「把已经问过的话改一改再问一遍」：重新生成，与编辑并重发。
//
// 两条都**必须**守住 Ask 那条不变量：用户打出去的字先落库、模型失败也还在（票据 33）。
// 它们与 Ask 的差别只是「问的是哪句」—— Ask 问一句新的，这两条问库里已经有的、
// 或者刚被改掉的那句。所以顺序也一样：先把用户那半边的字在库里定下来，再去找模型。
//
// 两条都会让一段记录消失，那是这两个动作**本身**要丢掉的东西（重说一遍、改一句），
// 不是失败弄丢的。失败时它们一步都不动库，各自的注释里写着为什么做得到。
//
// 两条都返回换过之后的**整段**历史而不是一条记录：被作废的那一截不止一条，
// 界面拿去做一次整体替换最省事，也最不容易与库里那份走岔。
//
// 返回的整段里只有**最后一条**带着思考过程（withReasoning），因为只有它是这一轮刚生成的；
// 其余记录一律是空的，那不是漏了，是它本来就不落库（见 Message.Reasoning）。

// Regenerate 让模型把最后那条回答重说一遍。
//
// 「删掉最后那条模型回答，拿前面那句用户提问再问一次」—— 但实现是**先问、后换**，
// 不是先删后问。先删的话，模型这一次没答上来就把一条本来好好的回答弄没了：
// 用户什么都没换到，还倒赔一条。顺序因此是：读历史 → 砍到那条提问为止当上下文 → 问模型 →
// 在事务里把旧回答换成新的（Store.replaceFrom）。失败时库里一个字节都没动，再点一次即可。
//
// 只认最后一条记录、而且它得是模型的回答，位置不对返回 ErrNotRegeneratable。
// 「重说中间某一条回答」听着像同一件事，其实要顺手丢掉它后面那几轮对话 —— 那是用户
// 没要求过的删除，不该藏在「重新生成」这个动作里。要那个效果走 EditAndResend。
//
// streamID 与 Ask 那个是同一个东西：这一轮边生成边显示用的。落库的仍然是返回值里那一句。
func (s *Service) Regenerate(questionID, messageID int64, streamID string) ([]Message, error) {
	// 与 Ask 同一条规矩：写一个不存在的 id 应当收到 ErrNotFound，而不是一个
	// 「悄悄什么都没发生」的成功。
	if _, err := s.questions.GetQuestion(questionID); err != nil {
		return nil, err
	}

	history, err := s.store.history(questionID)
	if err != nil {
		return nil, err
	}
	at := indexOf(history, messageID)
	if at < 0 {
		return nil, ErrNoSuchMessage
	}
	// 三个条件一起判：是最后一条、是模型的回答、它前面那句是用户提问。
	// 对用户它们是同一件事（这个菜单项不该出现在那儿），报文里带上实情即可。
	if at != len(history)-1 || history[at].Role != RoleAssistant ||
		at == 0 || history[at-1].Role != RoleUser {
		return nil, ErrNotRegeneratable
	}

	// 上下文砍到那条回答为止（**不含**它）：这次就是要它重新说。
	// 于是最后一条正好是那条提问，满足 Asker 对 history 形状的要求。
	reply, err := s.asker.Ask(questionID, wireHistory(history[:at]), streamID)
	if err != nil {
		return nil, err
	}
	answer := strings.TrimSpace(reply.Text)
	if answer == "" {
		return nil, ErrEmptyReply
	}

	if _, err := s.store.replaceFrom(questionID, messageID, RoleAssistant, answer, s.now()); err != nil {
		return nil, err
	}
	return s.withReasoning(questionID, reply.Reasoning)
}

// EditAndResend 把一条用户消息改成新的字，再拿改过的这段对话重问一次。
//
// 顺序就是 Ask 的顺序，只是「记下用户这句」换成了「换掉用户这句」：**先落库，再问模型**。
// 换掉 = 从这条（含）起往后的记录全部作废、写进新的这句，删与写在同一个事务里
// （Store.replaceFrom）。所以模型失败时用户刚打的字还在库里 —— 界面重读一次 History
// 就能看到它，那就是那条不变量的样子。
//
// 作废掉的后半截是这个动作**本身**要丢的东西（改了一句，后面那些就都不作数了），
// 不是失败弄丢的：失败弄丢的顶多是「模型这次没答上来」这一件。界面上得把这件事
// 说在动手之前（见 Discussion.svelte 的编辑态）。
//
// 只能改**用户自己**说的那句；模型那条走 Regenerate。
//
// streamID 与 Ask 那个是同一个东西：这一轮边生成边显示用的。
func (s *Service) EditAndResend(questionID, messageID int64, text, streamID string) ([]Message, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, ErrEmptyMessage
	}
	if _, err := s.questions.GetQuestion(questionID); err != nil {
		return nil, err
	}

	history, err := s.store.history(questionID)
	if err != nil {
		return nil, err
	}
	at := indexOf(history, messageID)
	if at < 0 {
		return nil, ErrNoSuchMessage
	}
	if history[at].Role != RoleUser {
		return nil, ErrNotUserMessage
	}

	// 先把改过的这句落库（连同作废它之后那些，一个事务）—— 这一句落地之后，
	// 后面无论发生什么，用户打的字都不会丢。
	if _, err := s.store.replaceFrom(questionID, messageID, RoleUser, text, s.now()); err != nil {
		return nil, err
	}

	// 再回库取整段当上下文，与 Ask 一模一样（包含刚写下的这句）。
	// 不复用内存里那一份：库里那份才是真的，而且它已经把后半个对话砍掉了。
	updated, err := s.store.history(questionID)
	if err != nil {
		return nil, err
	}
	reply, err := s.asker.Ask(questionID, wireHistory(updated), streamID)
	if err != nil {
		return nil, err
	}
	answer := strings.TrimSpace(reply.Text)
	if answer == "" {
		return nil, ErrEmptyReply
	}
	if _, err := s.store.append(questionID, RoleAssistant, answer, s.now()); err != nil {
		return nil, err
	}
	return s.withReasoning(questionID, reply.Reasoning)
}

// withReasoning 读回整段历史，把这一轮的思考过程贴到**最后一条**上 —— 也就是刚写下的那条
// 模型回答（两条修订路径都是「写一条新的回答、它是全场的最后一条」）。
//
// 为什么不直接改 store.history 的返回值再返回：那要在这里再走一遍 append 的返回值，
// 而 Ask 已经这么做了（它拿得到 append 回来的那条）。这两条路径里回答是 replaceFrom /
// append 写进去的，手上没有那条 Message，与其把 id 猜出来，不如按「最后一条」认 ——
// 读取顺序由 Store.history 定死（同一毫秒按 id 递增），最后一条就是刚写的那条。
//
// 思考过程不进库，所以贴的是内存里这一份：见 Message.Reasoning。
func (s *Service) withReasoning(questionID int64, reasoning string) ([]Message, error) {
	history, err := s.store.history(questionID)
	if err != nil {
		return nil, err
	}
	reasoning = strings.TrimSpace(reasoning)
	if reasoning != "" && len(history) > 0 {
		history[len(history)-1].Reasoning = reasoning
	}
	return history, nil
}

// indexOf 找这条记录在历史里的下标，没有就是 -1。
func indexOf(msgs []Message, id int64) int {
	for i, m := range msgs {
		if m.ID == id {
			return i
		}
	}
	return -1
}
