// Package agent 是「agent 管理数据」这一层：用户用一句话提要求（「把中值定理改个名」
// 「我数学哪块最弱」），它自己去查数据、自己决定调哪几个工具，最后要么回答，要么
// 提一条**待批准改动**（CONTEXT.md）交给用户点头。
//
// ── 这个包最重要的性质：它写不了数据 ──
//
// 「agent 只能提议、不能直接改」不是靠纪律，是**类型上没有那条路**：
//
//   - 它的数据入口是 Reader —— 一个只有读方法的接口，方法返回的全部是无方法的值类型。
//     这个包里没有、也拿不到任何能 Exec 的句柄。
//   - 它唯一的输出是 Change（一条待批准改动），而 Change 只能交给 Proposer 落成
//     pending_changes 里的一行 —— 那一行**不影响任何正式数据**。
//   - 真正写数据的只有 agent/apply 那个 Applier，它只认 Change，而且**只有待批准服务
//     持有它**。本包连 agent/apply 都不 import（见下面那条架构守卫测试）。
//
// 这条性质是用测试钉住的，不是用注释保证的：internal/agent/safety_test.go 拿反射
// 扫本包依赖的接口方法集、扫两个 store 的方法集、扫 *Service 能摸到的每一个类型，
// 任何一处冒出一个写方法都会红。别改那份白名单来「修」测试 —— 那是把锁拆掉。
//
// ── 三层，各管一件事 ──
//
//	internal/agent        领域模型 + 工具 + 工具循环 + 待批准服务。没有数据库句柄。
//	internal/agent/store  持久化：ReadStore（只读，字段类型里没有 Exec）与
//	                      PendingStore（只碰 pending_changes 一张表）。
//	internal/agent/apply  唯一写正式数据的东西：吃一条 Change，调标签服务。
//
// 依赖方向是 agent ← store、agent ← apply。反方向不行，而且有测试拦着。
//
// ── 为什么不复用 vlm.Service ──
//
// 打标签 / 讨论那条路上的 vlm.Service 是「一次请求一次回答」：拼好消息，发出去，
// 解析回来。工具调用循环要的是另一件事 —— 模型可以先要一次数据、看到结果再要第二次。
// 所以这一层直接拿着 vlm.Provider 那一层（下面的 Model 接口，只取 Chat 一个动作），
// 自己转这个循环。模型名依旧从配置来（ADR-0005：代码里一个默认模型 ID 都不许出现）。
package agent
