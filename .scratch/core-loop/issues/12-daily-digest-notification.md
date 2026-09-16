# 12: 每日汇总通知

**What to build:** 每天固定时间收到**一条**汇总通知，形如「今天有 N 道待复习」；当天没有到期题则不发。

**Blocked by:** 08（复习）

**Status:** ready-for-human —— Go 侧 16 条测试全过、宿主补丁已编译进 APK 并部署；**真机一条都没验过**

- [x] 每天固定时间发一条，且只发一条（宿主一条闹钟 + receiver 末尾重排下一条）
- [x] 内容包含当天到期的数量（`Slot.Body` 已经算好）
- [x] 当天没有到期题时**不发**（`Slot.Count == 0` 就是「不发」）
- [x] 不发单题通知（只有汇总一条）
- [x] 用户能关掉它（`digest.json` 的 `enabled`，且关掉时会同时写空 `slots`）

## Comments

### 为什么调度必须在 Java 宿主里，以及那条**死的路**

「应用没打开也要发」在安卓上只有后台调度能做到，Web 层没有这条路。

**WorkManager 是死的** —— 这条值得单独记，因为它看起来是最理所当然的选择。官方 Doze 文档
（<https://developer.android.com/training/monitoring-device-state/doze-standby>）逐字：
「Doesn't let JobScheduler run. **WorkManager uses JobScheduler internally, so WorkManager tasks don't run.**」
也就是说 WorkManager 方案不是"会晚一点"，而是**静默地永不触发**。

**Wails 自己的通知不能定时。** `pkg/application/mobile_features_android.go` 的 `Notify` 注释里写着
支持 `delay`（秒），但 Java 端 `WailsBridge.postNotification` 从头到尾**没读这个字段** ——
文档里写了、实现里没有。所以别在它身上打主意。

**可用的那条**：`AlarmManager.setExactAndAllowWhileIdle`。Doze 里 `setExact()` / `setWindow()`
会被推到维护窗口，而 `setAndAllowWhileIdle()` / `setExactAndAllowWhileIdle()` 不会（限制是每应用
9 分钟最多一次，对一天一次毫无影响）。`setAlarmClock()` 也能穿 Doze，但会在状态栏常驻闹钟图标，
对错题提醒太重，不用。

### 用户已确认的代价：安卓 14+ 要自己去系统设置里开一次

- `SCHEDULE_EXACT_ALARM` **在 Android 14 起对新装应用默认被拒**
  （<https://developer.android.com/about/versions/14/changes/schedule-exact-alarms>：
  「no longer being pre-granted to most newly installed apps targeting Android 13 and higher」）。
  官方要求的做法：先 `canScheduleExactAlarms()` 检查，false 时用
  `ACTION_REQUEST_SCHEDULE_EXACT_ALARM` 引导用户去开，并在 `onResume()` 里复查。
- `USE_EXACT_ALARM` **不能用**：Google Play 把它列为受限权限（只给「核心功能是日历或闹钟」的应用），
  错题本不够格。
- **用户 2026-09-16 明确接受这个代价**（「接受，我去点那一下」）。拿不到精确权限时降级 `setWindow`。

### 设计：Go 算好，宿主**不信任地复核**

Go 侧算出**未来 14 天各自的到期数**写进 `digest-schedule.json`（今天就能算出明天 —— `due_at` 是
绝对时刻，不必等到那天），宿主到点醒了**自己再核一遍**才发：日期是不是今天、数量是不是 >0、
有没有比 `fire_at_ms` 晚太多（宽限 6 小时，挡「系统把闹钟推到第二天」）。

这样**最坏结果是「那天不发」，而不是「发一条数字错的」**。这条规则顺带把「用户改了时间」
「用户关掉了」「排程用尽了」三种情况一起兜住。

「关掉」怎么传到宿主：Go 与 Java **没有通道**（宿主是另一个进程，Go 没办法取消一个已排的闹钟，
Wails 里也没有能取消的方法）。唯一一条路是把话写进**宿主到点会读的那个文件**：关掉时写
`enabled:false` **同时**写空 `slots`（两处都说，宿主少判一个条件就少一次误发）。

排程文件用 epoch 毫秒而不是 RFC3339：宿主是 minSdk 21，`java.time` 用不了，`SimpleDateFormat`
啃字符串不如一句 `getLong` 比大小。`slots` 落成 `[]` 而不是 `null`（少一个 NPE 检查）。

### 顺着查出来的一处脚手架 bug

脚手架的 `POST_NOTIFICATIONS` 流程是「要完权限**立刻照发**」（`WailsBridge.java` 请求后没有 return，
紧接着 `nm.notify(...)`）。权限对话框还挂着时那条通知会被系统丢掉 —— **第一次那条一定是丢的**。
宿主补丁里必须改掉（没权限时只请求、不抢着发，等下一次到点再发；或在授权回调里补发）。

### 用户已确认：发失败了要在设置页说一声

**这条要实现必须等宿主补丁** —— Go 侧**无法**知道「精确闹钟权限被拒」「通知权限被关」
（那两件事只有 Java 知道）。做法是宿主**回写一个状态文件**（`canScheduleExactAlarms()` 的结果、
上一次实际发的时刻与结果），Go 读了之后在设置页露一行。所以它归到宿主补丁那一轮，
不是这里少做了一半。

### 设置存在哪

- `<dataDir>/digest.json` —— 用户的设置（`enabled` / `hour` / `minute`）。与 `vlm.json`、`library.db`、
  `cards/` 并列。**不进 SQLite**（配置不是错题的数据模型，ADR-0004 已经立了这个模式），也没碰迁移。
  文件**不在** = 还没配过 = 默认（开着、20:00）；文件**在但坏了** = 报错且不写盘（不拿默认值悄悄顶替）。
- `<dataDir>/digest-schedule.json` —— 派生的排程，宿主读的就是它。分成两份是因为：设置只在用户点保存时变，
  排程每次重算都变；混在一起，重算会覆盖用户刚改的设置。

### 两条可能分叉的接缝，各有一个看门人

- 到期判据在 `internal/digest` 抄了第二份（与 `review.dueItems` 的 WHERE/JOIN 逐字相同，因为
  `internal/review` 本轮不许改）→ `TestForecastAgreesWithReviewQueue` 逐日把本包的数与
  `review.Service.Queue` 的长度对一遍，review 那边改了判定它会当场翻掉。
- 落盘 JSON 与 Go 内部类型是两份东西 → `TestRefreshWritesWhatTheHostReads` 用一份
  **独立照着契约写的**结构体把文件读回来断言，不是复用实现里的结构体。

### 宿主方案（下一轮做）

新增 `DigestScheduler.java`（读排程文件、`arm`/`cancel`、按 `canScheduleExactAlarms()` 选
`setExactAndAllowWhileIdle` 或降级 `setWindow`）、`DigestAlarmReceiver.java`（不导出，复核后发，
**无论如何重新 arm 下一次**）、`DigestBootReceiver.java`（闹钟不跨重启，收
`BOOT_COMPLETED` + `MY_PACKAGE_REPLACED` 重排）。
改 `MainActivity.java`（每次启动 `arm` —— 这条是**必需**不是优化，见下面的卸载影响）、
`AndroidManifest.xml`（两个权限 + 两个 receiver）。
`app/build.gradle` **不用改**（不用 WorkManager，不必加 `androidx.work`）。

**卸载/重装的影响**：卸载会把 `digest.json` 与 `digest-schedule.json` 一起删掉、已排的闹钟被系统取消、
`POST_NOTIFICATIONS` 回到默认（13+ 上就是「关」）；重装后设置回默认（开着、20:00）、要重新授权、
安卓 14+ 上还要重新去系统设置里点一次「闹钟和提醒」。
**闹钟永远不会被备份**，所以「每次启动重排」是这套方案能自愈的唯一依据。
（manifest 现在是 `allowBackup="true"`，但那救不了闹钟。）

### 宿主回写的状态（用户要的那一行）

有两件事 Go 侧**无从知道** —— 系统里的通知权限有没有被关、精确闹钟的「闹钟和提醒」有没有给。
那只有 Java 知道（`areNotificationsEnabled()` / `canScheduleExactAlarms()`）。所以宿主每次
arm / 发完之后把状态**回写**到 `<dataDir>/digest-host.json`，Go 只读（`internal/digest/host.go`）。

**读不到、读坏了都当「不知道」，不报错** —— 它是诊断，不是用户做错了什么。把它当错误处理会把
「提醒设置」整页顶成红色。

**文件名与字段名是两个进程之间的契约**（中间没有类型能帮它们对齐），所以有一条**契约测试**
照着宿主写下的形状造一份、断言 Go 读得懂。改名要两边同时改，这条测试会当场翻。

那句人话（`HostStatus.Note`）在 Go 侧拼：它要同时看 `enabled` 与宿主报的两三个布尔量，
是一段**判断**而不是一段文案。措辞上有一条刻意的区分 —— **通知权限被关 = 真的发不出来**，
而**精确闹钟没给 = 发得出来但可能晚**（宿主降级到不精确的窗口闹钟）。两者混成一句就是在骗用户。

### 宿主补丁落地成了什么样

三个新文件（`DigestScheduler` / `DigestAlarmReceiver` / `DigestBootReceiver`）+ 三处改动
（`WailsJSBridge` 加 `copyToDownloads` 是票 13 的；`WailsBridge.postNotification` 只修权限顺序；
`MainActivity` 启动与 `onResume` 各 arm 一次；manifest 两条权限两个 receiver）。

**一个值得记下来的坑（不写下来会被重新踩）**：**绝不 arm 一个已经过去的时刻**。
`AlarmManager` 对过去的时刻会**立刻**触发，receiver 复核时看到「日期就是今天、距 fire_at 才几秒」
（在宽限内）→ 发；然后 receiver 末尾再 arm，又挑中同一条 → 立刻再触发 → **死循环刷通知**。
所以 `fire_at > now` 是硬条件，它不是洁癖。

**降级路径**：拿不到精确权限时走 `setWindow(..., 10min)`。取 10 分钟是权衡 ——
窗口再宽就等于把用户设的「几点发」也弄丢了。真正的代价是 Doze 里 `setWindow` 会被推到维护窗口
（可能晚几十分钟），这一条由 receiver 的 6 小时宽限兜着：超了就 `skipped_stale` 不发，
**那天的数字不会错**。另外 `setExactAndAllowWhileIdle` 外面单独 catch `SecurityException` 并降级 ——
权限可能在 `canScheduleExactAlarms()` 之后、那一句之前被用户撤掉，不该把整次排程丢掉。

**引导只在三个条件同时成立时弹**：只引导一次（SharedPreferences 标志，**不写进 `digest-host.json`**
——那是 Go 定的形状）、真的排上了一条之后（没有可发的东西时把人丢进系统设置是骚扰）、
且 `ctx instanceof Activity`（`arm` 也会被 receiver 调到，那种上下文里 `startActivity` 会被
后台启动限制挡掉，白烧掉那次机会）。

`MainActivity` 的 `onResume` 里也 arm 一次，**这不是我列的清单，是补丁自己加的**：用户刚去系统设置
点完「闹钟和提醒」回来时，排程还是降级的窗口闹钟，只有重排才能换成精确的 —— 官方指引也是这么说的。
加了 `contentIntent`（点通知回到应用），同样超出清单，理由是不加的话点它什么都不发生。

### 两条留给 Go 侧（已接）

- **`last_result` 为空串是常态**：宿主的约定是「arm 成功不动 `last_result`」，所以文件刚被 arm
  建出来、还没到过点时它就是空的。Go 侧把它当「还没到过点」而不是异常（`HostStatus.Note` 里
  有一条测试专门写着这句话）。
- **`last_fire_at_ms` 只在真的发出去时更新**（跳过不写），否则设置页会把一次跳过说成一次发送。

### 未验证

- **宿主一行没写**，所以「每天固定时间发一条」这条验收项完全没验。AlarmManager 在真 Doze 下的表现、
  以及小米/华为那类厂商省电策略的额外限制都没实测（官方文档对 OEM 行为没有承诺）。
- 排程文件被 Java 读的那一刻（并发写、字段拼错）**只有 Go 侧契约测试挡着**，宿主那侧没有任何测试。
- **「评完题就重算排程」这条链还没接上**：`Grade` 在 `internal/review` 里（本轮不许改），
  只能在 Agent 页的 `onMount` 与 `visibilitychange(hidden)` 上重算 —— 于是**只要用户从不打开设置页，
  排程就只在应用启动那一刻算过一次**。正规修法是 `Grade` 之后加一次 `Refresh`，归属主会话。
