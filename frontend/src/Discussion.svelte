<script lang="ts">
  // 讨论：就这道错题问几句。预设追问是入口，之后可以自由打字。
  //
  // 外层这样接 —— 它自带取数（与 Review.svelte 同一个路子），外层只要把题号给它：
  //
  //   <Discussion questionId={current.Question.ID} />
  //
  // 会话挂在错题上、存在库里：换一道题就是换一整份会话，下次再进这道题还是这些
  // （票据 32）。默认收成一行入口，点开才铺开 —— 复习那一页的主体是题图，
  // 讨论是评完之后的事，不该一上来就占掉半屏。
  //
  // 逻辑一行都不在这儿：预设追问来自 Discussion.Presets()、历史来自 History()、
  // 问出去走 Ask()、重说走 Regenerate()、改一句走 EditAndResend()（spec：逻辑刻意
  // 不往前端放，这里不做自动化测试）。
  //
  // 这一稿是三处用户报上来的毛病各自对应的东西，「为什么这么写」都写在跟前：
  //   1) 打字时输入框被键盘盖住   → applyKeyboard / .panel 的 --kb 那两段
  //   2) 宽内容把气泡撑出屏幕     → .bubble 的 max-width（根因在那条注释里）+ 表格自己的滚动
  //   3) 长按一条消息什么菜单都没有 → pressStart 那一段（含「自己画菜单」这个取舍的代价）

  import { onMount } from 'svelte';
  import { Events } from '@wailsio/runtime';
  import * as Discussion from '../bindings/questionbook/internal/discussion/service';
  import Markdown from './Markdown.svelte';

  // 只用到这四个字段，所以不从生成的 bindings 里 import 模型类型（与 Tagging.svelte 同一个
  // 做法）：形状对得上就够。Role 在这儿当普通字符串看 —— Wails 给 Go 的具名字符串类型生成的
  // 是 TS 枚举，拿枚举去跟 'user' 比过不了类型检查（字符串枚举是 string 的子类型，
  // 反过来赋值没问题，所以绑定回来的东西直接这么收下即可）。
  type ChatMessage = {
    ID: number;
    Role: string;
    Text: string;
    CreatedAt: string;
    /**
     * Reasoning 是模型这条回答的思考过程（Go 侧 discussion.Message.Reasoning）。
     *
     * 写成**可选**是有意的：它不落库，只有刚产生这条回答的那一次调用（Ask / Regenerate /
     * EditAndResend）的返回值上才有；History 读回来的每一条都没有这个字段的值。
     */
    Reasoning?: string;
  };

  /** menu 是长按一条消息之后那个菜单：位置按下的地方，内容看是哪一条。 */
  type Menu = {
    m: ChatMessage;
    /** 是不是整段对话的最后一条 —— 「重新生成」只认那个位置（见 Go 侧 Regenerate）。 */
    last: boolean;
    x: number;
    y: number;
  };

  /** Turn 是刚过去的一轮（Go 侧 discussion.Turn）：问出去的那句 + 模型答的那句。 */
  type Turn = { Question: ChatMessage; Reply: ChatMessage };

  /**
   * 流式增量那一条事件的载荷（Go 侧 vlm.StreamDelta）。
   *
   * 事件名与这三个字段名都与 Go 侧逐字相同 —— 它是 emit 出去的一份 JSON，
   * 不经过生成的绑定，所以这儿的类型只能自己写一份。
   */
  type StreamDelta = { stream_id: string; kind: string; text: string };

  // 与 Go 侧 vlm.KindText / KindReasoning 逐字相同。两路分开发（见 vlm/events.go），
  // 界面上它们也是两块地方：思考过程折在上面、回答在气泡里。
  const KIND_TEXT = 'text';
  const KIND_REASONING = 'reasoning';

  interface Props {
    /** 讨论挂在哪道错题上。null = 还没有题（队列空着的时候），此时整块不出现。 */
    questionId: number | null;
  }

  let { questionId }: Props = $props();

  let open = $state(false);
  let presets = $state<string[]>([]);
  let messages = $state<ChatMessage[]>([]);
  let draft = $state('');
  let busy = $state(false);
  let error = $state('');
  // 正在等回答的那一句。乐观显示：用户打的字立刻上屏，不等模型
  // （Go 侧也确实先把它落库了，见 Discussion.Ask）。
  let pending = $state('');

  // ── 流式 ──
  //
  // liveId 是**这一次调用**的号（下面 newStreamId 编的，随调用交给 Go）。
  // live 是这一次调用期间已经收到的分片：两路各自攒一份。
  //
  // 为什么号由前端编而不是 Go 返回一个：Ask 这类调用只到最后才返回，而分片在这中间就来了 ——
  // 界面得先有个办法认出「哪些分片是我这一次的」。前端编一个、传进去、Go 给每一片打上它，
  // 界面按它筛。这样不必多一次往返，也不必等 Go 先给个回执。
  //
  // 它只是**这一次调用期间**的显示。调用一返回（成功或失败）就清掉，屏幕上只剩落库回来的
  // 那一条 —— 「最终显示与库里那条对齐」就是这么做出来的（Go 侧 ChatStreaming 的说明里写着
  // 为什么必须整条替换：流到一半断掉时会回落重发，已经流出去的那半截不作数）。
  let liveId = $state('');
  let live = $state<{ text: string; reasoning: string } | null>(null);
  // 有没有东西可显示（两路都空时还是那句「正在想…」）。
  const liveActive = $derived(
    live !== null && (live.text !== '' || live.reasoning !== ''),
  );

  // 一闪而过的回执（「已复制」这类）。它不是错误，所以不跟 error 抢那一行。
  let notice = $state('');
  let noticeTimer: ReturnType<typeof setTimeout> | undefined;

  // 长按出来的那个菜单；null = 没开着。
  let menu = $state<Menu | null>(null);
  // 「选择文本」放开了这一条的可选中（全局是禁选的，见 style.css）。
  // 同时只放开一条：不然拖动选字会在几条消息之间乱跳。
  let pickId = $state<number | null>(null);
  // 正在改的那一条。非 null 时输入框是编辑框：按「问」走 EditAndResend 而不是新问一句。
  let editing = $state<{ id: number } | null>(null);

  // 记录那一块的元素（滚到底用）与最后一条的 id（长按菜单要用它判断「是不是最后一条」）。
  let logEl = $state<HTMLDivElement>();
  const lastId = $derived(messages.length > 0 ? messages[messages.length - 1].ID : -1);

  // ── 键盘 ──
  //
  // 安卓上（targetSdk 35 + Android 15 的强制边到边）系统**不再为键盘压缩窗口**：manifest 里
  // 的 adjustResize 在这个目标版本上对 IME 已经不生效，于是页面里没有任何东西知道键盘起来了 ——
  // 100dvh 还是整屏、贴底那个输入框就被键盘盖住。WebView 自己也没有 OSK 感知
  // （visualViewport / dvh / interactive-widget 全都不动，crbug 40287394），
  // 所以键盘高度只有一个可信来源：宿主报的 common:keyboard（那条监听在 main.go 里打开，
  // 那儿写着它为什么默认是关的）。
  //
  // 桌面端这条事件永远不来，kbHeight 就一直是 0：同一份布局在桌面上照旧。
  let kbHeight = $state(0);
  // 输入框底边到屏幕底边本来有多远（底下那条导航栏 + 手势区）。抬的时候要把这一截减掉，
  // 否则输入框会浮在键盘上方空着一个导航栏的高度。聚焦那一刻量一次 —— 那时键盘还没起来，
  // 量到的是「没被抬起过」的布局。
  let composerGap = $state(0);
  let composerEl = $state<HTMLFormElement>();
  // 聚焦那一刻的视口高，用来判断「窗口是不是已经被压短了」。
  let baseHeight = 0;

  // 抬起量：键盘多高，就往上让多少，但底下本来那一段不用再让。
  const kbPad = $derived(Math.max(0, kbHeight - composerGap));

  onMount(() => {
    void loadPresets();
    // 宿主那条键盘监听**不在这儿开**：它是接线的事，已经在 main.go 里打开一次
    // （早先临时挪到过讨论包里，分层不对，已挪走）。这里只订阅事件。
    const off = Events.On('common:keyboard', (event) => applyKeyboard(event.data));
    // 流式增量那条事件（Go 侧 vlm.StreamEvent 发出来的，事件名逐字相同）。
    const offDelta = Events.On('vlm:delta', (event) => applyDelta(event.data));
    return () => {
      off();
      offDelta();
    };
  });

  // newStreamId 编一个这次调用用的号。
  //
  // 只要求「这一次调用期间不跟别的撞上」，所以时间戳加一段随机数就够（它不会被存下来、
  // 也不会跨设备比较）。randomUUID 要安全上下文，WebView 里未必有 —— 留一条老路。
  function newStreamId(): string {
    if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
      return crypto.randomUUID();
    }
    return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
  }

  // applyDelta 收一片流式增量。
  //
  // 先按号筛：不是这一次调用的（上一次没散尽的、别处发的）一律丢掉。号是自己编的，
  // 所以只在**这一次调用**期间认它 —— 调用一返回 liveId 就清空了。
  //
  // 两路各归各位：思考过程进上面那折起来的一块，正文进气泡。Go 侧把它们分成两条事件发，
  // 正是因为它们在界面上是两块地方（见 vlm/events.go）。
  function applyDelta(data: unknown): void {
    const d = data as StreamDelta | null;
    if (!d || liveId === '' || d.stream_id !== liveId) return;
    const text = typeof d.text === 'string' ? d.text : '';
    if (text === '') return;
    if (live === null) live = { text: '', reasoning: '' };
    if (d.kind === KIND_REASONING) live.reasoning += text;
    else if (d.kind === KIND_TEXT) live.text += text;
    scrollToEnd();
  }

  // applyKeyboard 收宿主那份键盘报告。
  //
  // 载荷是 Java 那边 JSONObject.toString() 出来的字符串（见 WailsBridge.emitKeyboard），
  // 但也可能已经被这一层解成了对象 —— 两种都收，别的形状当作「不知道」。
  function applyKeyboard(data: unknown): void {
    let info: { visible?: unknown; height?: unknown } = {};
    if (typeof data === 'string') {
      try {
        info = JSON.parse(data);
      } catch {
        return;
      }
    } else if (data !== null && typeof data === 'object') {
      info = data as typeof info;
    } else {
      return;
    }

    const raw = Math.max(0, Math.round(Number(info.height) || 0));
    if (info.visible !== true || raw === 0) {
      kbHeight = 0;
      return;
    }

    // 安卓报的是**物理**像素：WindowInsets 的坐标空间（WailsBridge.setKeyboardWatch 里
    // 读的就是 insets.getInsets(Type.ime()).bottom），而样式用的是 CSS 像素 ——
    // 中间差一个 devicePixelRatio。不换算的话，三倍屏上会把输入框顶到屏幕中间去。
    // （iOS 那边报的是点，本身就接近 CSS 像素；这里只针对安卓。）
    const css = raw / (window.devicePixelRatio || 1);

    // 万一某个平台上窗口**确实**被压短了（别的 Wails 版本、别的安卓版本、桌面端将来也发），
    // 就别再抬一次 —— 抬两次会高出键盘一大截。基准是聚焦那一刻的视口高，8px 是给动画留的余量。
    const shrunk = baseHeight > 0 ? baseHeight - window.innerHeight : 0;
    kbHeight = shrunk >= css - 8 ? 0 : css;
  }

  // 聚焦时量一下输入框底边到屏幕底边的距离。
  //
  // 键盘已经起来时不量：那时的间距里含着我自己抬上去的那一段，量出来的基准是错的
  // （再减一次就会越抬越低）。反正接下来这条事件会用上一次量好的值。
  function measureGap(): void {
    baseHeight = window.innerHeight;
    if (kbHeight !== 0) return;
    const el = composerEl;
    if (!el) return;
    const gap = window.innerHeight - el.getBoundingClientRect().bottom;
    if (gap >= 0) composerGap = gap;
  }

  // 换题就换一整份会话。loadedFor 记着「现在这份是谁的」，免得同一道题的重复渲染又去拉一次。
  // 它是普通变量而不是 $state：这个判断本身不该引起重渲染。
  let loadedFor: number | null = null;
  $effect(() => {
    const id = questionId;
    if (id === loadedFor) return;
    loadedFor = id;
    messages = [];
    error = '';
    pending = '';
    // 流到一半的那一份也是「某一道题的这一次调用」：换了题它指的就不是同一件事了。
    live = null;
    liveId = '';
    // 菜单、选中、编辑态都是「某一条消息」上的东西：换了题它们指的就不是同一条了。
    menu = null;
    pickId = null;
    editing = null;
    if (id !== null) void loadHistory(id);
  });

  function errorMessage(err: unknown): string {
    return err instanceof Error ? err.message : String(err);
  }

  // 「今天 10:04」「09-16 10:04」—— 回看时最要紧的是「这是哪天聊的」。
  // 不做「3 分钟前」那种相对时间：它得跟着现在走，而这一块不会自己刷新，
  // 挂一会儿就会显示成一句不准的话。
  function timeText(iso: string): string {
    const at = new Date(iso);
    if (Number.isNaN(at.getTime())) return '';
    const pad = (n: number) => String(n).padStart(2, '0');
    const hhmm = `${pad(at.getHours())}:${pad(at.getMinutes())}`;
    const now = new Date();
    if (at.toDateString() === now.toDateString()) return `今天 ${hhmm}`;
    const md = `${pad(at.getMonth() + 1)}-${pad(at.getDate())}`;
    return at.getFullYear() === now.getFullYear() ? `${md} ${hhmm}` : `${at.getFullYear()}-${md} ${hhmm}`;
  }

  function flash(text: string): void {
    notice = text;
    clearTimeout(noticeTimer);
    noticeTimer = setTimeout(() => {
      notice = '';
    }, 2000);
  }

  async function loadPresets() {
    try {
      presets = (await Discussion.Presets()) ?? [];
    } catch {
      // 预设只是入口：取不到就只剩自由输入，不必把整块顶成错误。
    }
  }

  async function loadHistory(id: number): Promise<ChatMessage[]> {
    try {
      const msgs = (await Discussion.History(id)) ?? [];
      // 取数是异步的：这中间用户可能已经翻到下一道题了，别把旧会话贴上去。
      if (questionId !== id) return [];
      messages = msgs;
      return msgs;
    } catch (err) {
      if (questionId === id) error = errorMessage(err);
      return [];
    }
  }

  async function send(text: string) {
    const id = questionId;
    const body = text.trim();
    if (id === null || body === '' || busy) return;

    busy = true;
    error = '';
    pending = body;
    if (draft.trim() === body) draft = ''; // 按预设问的，别顺手清掉用户打了一半的字

    const stream = newStreamId();
    liveId = stream;
    live = { text: '', reasoning: '' };

    try {
      const turn = await Discussion.Ask(id, body, stream);
      // 这中间换了题：结果丢掉（它已经在库里了，回到那道题还看得到）。
      if (questionId !== id) return;
      messages = [...messages, turn.Question, turn.Reply];
      scrollToEnd();
    } catch (err) {
      if (questionId !== id) return;
      error = errorMessage(err);
      // 用户那句**已经落库了**（Go 侧先写它、再调模型），所以这里重读一次而不是把它丢掉 ——
      // 屏幕上看到的与库里存的一致，这就是「被打断时记录不会丢」那条在界面上的样子。
      await loadHistory(id);
    } finally {
      if (questionId === id) {
        busy = false;
        pending = '';
        // 流出去的那一份在这里收掉：上面已经把落库回来的整条挂上去了，留着它屏幕上就是
        // 同一句话出现两遍（一份半截的、一份完整的）。
        live = null;
        liveId = '';
      }
    }
  }

  // ── 编辑并重发 ──

  // 编辑态不弹一个原地的小编辑框：重发要打字，而打字就要那个已经贴着键盘的输入框
  // （键盘那一套补偿只挂在它身上，副输入框会各有各的毛病）。
  function startEdit(): void {
    const target = menu?.m;
    closeMenu();
    if (!target || target.Role !== 'user') return;
    editing = { id: target.ID };
    draft = target.Text;
    error = '';
  }

  function cancelEdit(): void {
    editing = null;
    draft = '';
  }

  async function resend() {
    const id = questionId;
    const target = editing;
    const body = draft.trim();
    if (id === null || target === null || body === '' || busy) return;

    busy = true;
    error = '';
    const stream = newStreamId();
    liveId = stream;
    live = { text: '', reasoning: '' };
    try {
      messages = (await Discussion.EditAndResend(id, target.id, body, stream)) ?? [];
      editing = null;
      draft = '';
      scrollToEnd();
    } catch (err) {
      if (questionId !== id) return;
      error = errorMessage(err);
      // 与 send 同一件事：改过的那句**已经落库了**（Go 侧在一个事务里先把旧的删掉、
      // 写下新的，然后才去问模型），所以重读一次而不是把它丢掉 ——
      // 失败之后输入框里那句与库里那句是同一句，屏幕上看得见。
      const fresh = await loadHistory(id);
      // 编辑态留着，但它指的必须是**新写进去的那一行**：刚才那条的 id 已经随删除作废了
      // （AUTOINCREMENT：id 不回收），继续拿旧的 id 去改会被 ErrNoSuchMessage 挡下。
      // 改成最后那条用户消息，用户再按一次「问」就是接着重试同一句，不会多出一条重复的。
      const lastUser = [...fresh].reverse().find((m) => m.Role === 'user');
      editing = lastUser ? { id: lastUser.ID } : null;
    } finally {
      if (questionId === id) {
        busy = false;
        live = null;
        liveId = '';
      }
    }
  }

  // ── 重新生成 ──

  async function regenerate() {
    const id = questionId;
    const target = menu?.m;
    closeMenu();
    if (id === null || !target || target.Role !== 'assistant' || busy) return;

    busy = true;
    error = '';
    const stream = newStreamId();
    liveId = stream;
    live = { text: '', reasoning: '' };
    try {
      messages = (await Discussion.Regenerate(id, target.ID, stream)) ?? [];
    } catch (err) {
      if (questionId !== id) return;
      error = errorMessage(err);
      // 这里**不重读**历史，而编辑重发那边要重读 —— 差别在 Go 侧的顺序：
      // 重新生成是先问、后换，问失败时库里一个字节都没动，屏幕上这份就是库里那份；
      // 编辑重发是先落库、后问，失败时库里已经换了，得回库对齐。
    } finally {
      if (questionId === id) {
        busy = false;
        live = null;
        liveId = '';
      }
    }
  }

  function onSubmit(event: SubmitEvent) {
    event.preventDefault();
    if (editing) {
      void resend();
      return;
    }
    void send(draft);
  }

  function scrollToEnd(): void {
    // 等这一帧画完再滚：新消息的高度这时才算得出来。用 requestAnimationFrame
    // 而不是 await tick()，因为 logEl 是原生元素、不归 Svelte 的更新周期管。
    requestAnimationFrame(() => {
      const el = logEl;
      if (el) el.scrollTop = el.scrollHeight;
    });
  }

  // ── 长按菜单 ──
  //
  // **为什么自己画一个菜单，而不是把系统那套长按选择接过来**（另一条路是：放开记录的可选中，
  // 长按交给 WebView，再在气泡旁边常年挂几个小按钮）：
  //
  //   - 系统的长按菜单**加不了项**。复制是它自带的，而这里要的还有「编辑并重发」「重新生成」——
  //     那是这个应用自己的动作，系统菜单里没有它们的位置。另一条路只能做成「系统选择 + 自己
  //     再挂一排按钮」，同一件事于是有两个入口、两套位置，读起来像两个功能。
  //   - 全局是禁选的（见 style.css，这是应用不是文档）。为了长按复制而把记录整片放开可选中，
  //     代价是每一次拖动都变成选字 —— 而这一页横向还能翻页，手势会打架。
  //
  // 代价说清楚：单纯想复制一小段，现在比系统多一步（长按 → 菜单 → 复制整条／选择文本）。
  // 「选择文本」按下去就交还给系统：放开这一条的可选中、把它整体选上，系统的手柄、
  // 放大镜与它自己的复制菜单都是现成的 —— 细选与那一步复制仍归系统，不自己实现。
  let press: { x: number; y: number; timer: ReturnType<typeof setTimeout> } | null = null;

  // 长按 = 按住不动 450ms。挪开、松手、滑出这一条都作废（那是滚动或点击）。
  // 鼠标不算「长按」：桌面上走右键（oncontextmenu），按住不放是选字的常规操作。
  function pressStart(event: PointerEvent, m: ChatMessage, last: boolean): void {
    if (busy || event.pointerType === 'mouse' || event.button !== 0) return;
    const { clientX: x, clientY: y } = event;
    press = {
      x,
      y,
      // 位置在按下这一刻就抄下来：等定时器烧完再读事件对象，那个对象可能已经不在了。
      timer: setTimeout(() => {
        press = null;
        openMenu(m, last, x, y);
      }, 450),
    };
  }

  function pressMove(event: PointerEvent): void {
    if (!press) return;
    if (Math.abs(event.clientX - press.x) > 10 || Math.abs(event.clientY - press.y) > 10) pressEnd();
  }

  function pressEnd(): void {
    if (!press) return;
    clearTimeout(press.timer);
    press = null;
  }

  // 桌面上的右键：同一份菜单，位置就在指针那儿。
  function onMessageContext(event: MouseEvent, m: ChatMessage, last: boolean): void {
    event.preventDefault();
    if (busy) return;
    openMenu(m, last, event.clientX, event.clientY);
  }

  function openMenu(m: ChatMessage, last: boolean, x: number, y: number): void {
    pickId = null; // 上一次「选择文本」放开的那条收回去：同时只该有一条能选
    // 夹回屏幕里。菜单多大是估的（宽度由 .menu 定死 = 13rem，高度按最多四项算），
    // 估大一点总比让它从边上冒出去强。
    //
    // 底下被键盘盖住的那一段要扣掉：WebView 里 innerHeight 不会因为键盘变小，
    // 不扣的话，在记录靠下的地方长按会把菜单放到键盘后面去。（键盘没起来时 kbHeight 是 0，
    // 这一句就是原来的意思。）
    const w = 208;
    const h = 4 * 46;
    const usable = Math.max(h, window.innerHeight - kbHeight);
    menu = {
      m,
      last,
      x: Math.min(Math.max(8, x), Math.max(8, window.innerWidth - w - 8)),
      y: Math.min(Math.max(8, y), Math.max(8, usable - h - 8)),
    };
  }

  function closeMenu(): void {
    menu = null;
  }

  // 复制整条。
  //
  // navigator.clipboard 在两个 WebView 里能不能用不好保证（要安全上下文 + 文档有焦点），
  // 所以底下留一条老路：临时 textarea + execCommand('copy')。
  // 那个 textarea 必须自己把 user-select 打开 —— 全局是禁选的（style.css），
  // 而选不中的东西 execCommand 也复制不出来。
  async function copyText(text: string): Promise<boolean> {
    try {
      if (navigator.clipboard?.writeText) {
        await navigator.clipboard.writeText(text);
        return true;
      }
    } catch {
      // 落到下面那条老路去。
    }

    const ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.top = '-1000px';
    ta.style.opacity = '0';
    ta.style.userSelect = 'text';
    ta.style.setProperty('-webkit-user-select', 'text');
    document.body.appendChild(ta);
    ta.select();
    let ok = false;
    try {
      ok = document.execCommand('copy');
    } catch {
      ok = false;
    }
    ta.remove();
    return ok;
  }

  async function copyWhole(): Promise<void> {
    const m = menu?.m;
    closeMenu();
    if (!m) return;
    flash((await copyText(m.Text)) ? '已复制整条' : '复制不了，改用「选择文本」');
  }

  // 「选择文本」：把这一条放开可选中，再整体选上，剩下交给系统。
  // 系统的手柄、放大镜、它自己的复制菜单都是现成的 —— 自己实现一套选字只会更差。
  function selectText(): void {
    const m = menu?.m;
    closeMenu();
    if (!m) return;
    pickId = m.ID;
    // 等 pick 这个 class 生效（user-select 还是 none 的时候，选区建不出来）。
    requestAnimationFrame(() => {
      const el = document.getElementById(bubbleId(m.ID));
      if (!el) return;
      const range = document.createRange();
      range.selectNodeContents(el);
      const sel = window.getSelection();
      sel?.removeAllRanges();
      sel?.addRange(range);
      flash('已选中，可拖动改范围');
    });
  }

  function bubbleId(id: number): string {
    return `disc-bubble-${id}`;
  }
</script>

{#if questionId !== null}
  <div class="discussion">
    <button class="head" onclick={() => (open = !open)} aria-expanded={open}>
      <span class="head-title">与 VLM 讨论</span>
      {#if messages.length > 0}
        <span class="head-count">{messages.length} 句</span>
      {/if}
      <span class="head-toggle">{open ? '收起' : '展开'}</span>
    </button>

    {#if open}
      <!-- --kb 是键盘抬起来的那一段（见脚本里的 kbPad）：面板补一段等高的下边距把它顶上去，
           记录同时少这么高。两个数一个来源，改一个不会漏另一个。 -->
      <div class="panel" style="--kb: {kbPad}px">
        <!-- role="log" 是这个容器的本义（一段对话记录，新来的那句该被读屏念出来）。
             里面每一条是 role="article"：长按那几个手势得有个角色挂着，
             否则读屏根本看不见这一条是可操作的（svelte 的 a11y 检查也是这么要求的）。 -->
        <div class="log" role="log" aria-label="讨论记录" bind:this={logEl} onscroll={closeMenu}>
          {#if messages.length === 0 && !pending}
            <p class="empty">还没聊过。挑一句问起，也可以自己打字。</p>
          {/if}

          {#each messages as m (m.ID)}
            <div
              class="line"
              role="article"
              class:mine={m.Role === 'user'}
              class:editing={editing?.id === m.ID}
              onpointerdown={(e) => pressStart(e, m, m.ID === lastId)}
              onpointermove={pressMove}
              onpointerup={pressEnd}
              onpointercancel={pressEnd}
              onpointerleave={pressEnd}
              oncontextmenu={(e) => onMessageContext(e, m, m.ID === lastId)}
            >
              <!-- 模型这条回答的思考过程：**在回答上面**，默认收着，点一下才铺开。
                   为什么摆在上面：它是得出这个回答的过程，先看过程、再看结论，读起来是一条线；
                   摆在底下时它挡在回答与下一句提问之间，读的人得先跳过它才接得上下文。

                   为什么收着：它是「它确实在干活」的凭据，不是回答本身 —— 一直摊着会把回答
                   挤出屏幕，而多数时候用户要的就是回答。也不是拿来替「正在想…」那句等待提示的：
                   那句是等的时候看的，这块是回来之后才有的（见下面 live / busy 那两段）。
                   字体是虚的（见 .think 的字号与颜色），比正文小一号、也不那么亮 ——
                   它是给回答做注脚的，不该比回答本身还抢眼。

                   为什么只有模型那条有、而且不落库：见 Go 侧 discussion.Message.Reasoning。
                   这里如实写下一个看得见的后果 —— 换一道题再回来、或者失败后重读历史
                   （loadHistory），这一块就不在了。那是有意的，不是这里漏了。 -->
              {#if m.Role === 'assistant' && m.Reasoning}
                <details class="think">
                  <summary class="think-head">思考过程</summary>
                  <div class="think-body"><Markdown text={m.Reasoning} /></div>
                </details>
              {/if}

              <!-- 两边都过 markdown + KaTeX：模型几乎必然吐公式，而用户自己也可能写 LaTeX。 -->
              <div class="bubble" class:pick={pickId === m.ID} id={bubbleId(m.ID)}>
                <Markdown text={m.Text} />
              </div>
              <time class="at">{timeText(m.CreatedAt)}</time>
            </div>
          {/each}

          {#if pending}
            <div class="line mine">
              <div class="bubble"><Markdown text={pending} /></div>
            </div>
          {/if}

          <!-- 正在流的那一份。它是**这一次调用期间**的显示，两路各流各的：
               思考过程在上面（摊开着 —— 它正在流，收起来就等于看不见「它在想什么」），
               回答在下面那个气泡里。调用一返回就整块换成上面落库回来的那一条。
               思考过程这块在收尾后会变成落库那条的样子（默认收着）—— 「最终与库里那条对齐」
               就是这个意思，不是这里忘了留 open。 -->
          {#if liveActive && live}
            <div class="line">
              {#if live.reasoning}
                <details class="think" open>
                  <summary class="think-head">思考过程</summary>
                  <div class="think-body"><Markdown text={live.reasoning} /></div>
                </details>
              {/if}
              {#if live.text}
                <div class="bubble"><Markdown text={live.text} /></div>
              {/if}
            </div>
          {/if}

          <!-- 一个字都还没来的时候才是这句。它只在「等第一片」那一小段里出现 ——
               流起来之后上面那一块就是「正在想」本身（它带着已经写出来的那些字）。 -->
          {#if busy && !liveActive}
            <p class="thinking">正在想…</p>
          {/if}
        </div>

        {#if error}
          <p class="banner">{error}</p>
        {/if}
        {#if notice}
          <p class="notice">{notice}</p>
        {/if}

        {#if editing}
          <!-- 这句要写在动手之前：作废它后面那一截是这个动作本身要丢的东西（见 Go 侧
               EditAndResend 的注释），不是失败弄丢的，但不能等用户发现。 -->
          <div class="editbar">
            <span class="editbar-text">正在改这一句：后面那些话会被丢掉</span>
            <button class="editbar-cancel" type="button" onclick={cancelEdit}>取消</button>
          </div>
        {/if}

        {#if presets.length > 0 && !editing}
          <div class="presets">
            {#each presets as p (p)}
              <button class="preset" onclick={() => send(p)} disabled={busy}>{p}</button>
            {/each}
          </div>
        {/if}

        <form class="composer" onsubmit={onSubmit} bind:this={composerEl}>
          <input
            class="input"
            bind:value={draft}
            placeholder={editing ? '改完按「问」重发…' : '继续问…'}
            disabled={busy}
            enterkeyhint="send"
            onfocus={measureGap}
          />
          {#if editing}
            <button class="cancel" type="button" onclick={cancelEdit} disabled={busy}>取消</button>
          {/if}
          <button class="ask" type="submit" disabled={busy || draft.trim() === ''}>
            {editing ? '重发' : '问'}
          </button>
        </form>
      </div>
    {/if}
  </div>
{/if}

{#if menu}
  <!-- 点别处关掉。它是一层盖住全屏的透明按钮：菜单开着的时候下面那条记录不跟着滚，
       否则菜单钉在原地、它指着的那条却滑走了。
       ⚠️ 用 pointerdown 而不是 click：长按那根手指松开时也可能冒出一个 click，
       而它按 hit-test 走的是当时屏幕上的东西（也就是这层刚冒出来的遮罩）——
       菜单会当场被自己关掉。pointerdown 不会：那根手指的 pointerdown 早在菜单出现之前就用掉了。 -->
  <button class="scrim" type="button" aria-label="关闭菜单" onpointerdown={closeMenu}></button>
  <div class="menu" style="left: {menu.x}px; top: {menu.y}px">
    <button class="mi" type="button" onclick={copyWhole}>
      <span class="mi-name">复制整条</span>
    </button>
    <button class="mi" type="button" onclick={selectText}>
      <span class="mi-name">选择文本</span>
      <span class="mi-hint">拖动可改范围</span>
    </button>
    {#if menu.m.Role === 'user'}
      <button class="mi" type="button" onclick={startEdit}>
        <span class="mi-name">编辑并重发</span>
      </button>
    {:else}
      <button class="mi" type="button" disabled={!menu.last} onclick={regenerate}>
        <span class="mi-name">重新生成</span>
        {#if !menu.last}
          <!-- 位置不对就不做（Go 侧只认最后一条）：但要说清为什么灰着，
               不然用户会以为功能没做。 -->
          <span class="mi-hint">只重说最后一条</span>
        {/if}
      </button>
    {/if}
  </div>
{/if}

<style>
  .discussion {
    display: flex;
    flex-direction: column;
    border-top: 1px solid rgba(244, 246, 251, 0.1);
  }

  .head {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    padding: 0.6rem 0.15rem;
    border: 0;
    background: transparent;
    color: #f4f6fb;
    font: inherit;
    font-size: 0.9rem;
    text-align: left;
    cursor: pointer;
    touch-action: manipulation;
    -webkit-tap-highlight-color: transparent;
    --wails-draggable: no-drag;
  }
  .head-title {
    flex: 1;
    font-weight: 600;
  }
  .head-count,
  .head-toggle {
    font-size: 0.8rem;
    color: rgba(244, 246, 251, 0.5);
  }
  .head-toggle {
    color: #9fd4ff;
  }

  .panel {
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
    /* 键盘抬起来的那一段：键盘多高就在底下让出多高。
       **不用 transform 顶上去**：那样输入框会盖在记录上面（记录那块不动），
       而且 transform 会给里面的 position: fixed（长按菜单）换一套坐标系。
       这条边距让面板整体变高，而它在 .review 那个列 flex 里、题图区是 flex:1 ——
       题图区自己让出这一段，输入框正好落在键盘上方，记录也还看得见。
       具体数值由脚本算（键盘高度减掉底下导航栏那一段），见 kbPad。 */
    padding-bottom: calc(0.25rem + var(--kb, 0px));
  }

  /* 记录自己滚：讨论再长也不把下面的输入框顶出屏幕。 */
  .log {
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
    /* 键盘起来时同步少这么高，面板的总高因此基本不变（题图区不会被挤两次）。
       留一个 6rem 的下限：键盘高到把记录压没的时候，宁可让面板略微超高被裁一点，
       也不能把记录挤成零高（那样讨论看起来就像空了）。 */
    max-height: max(6rem, calc(40vh - var(--kb, 0px)));
    overflow-y: auto;
    overscroll-behavior-y: contain;
    /* ⚠️ 这一轴的 auto 是上面那条 overflow-y 带来的（CSS 里一轴 visible、另一轴非 visible 时，
       visible 会算成 auto），**不是**用来挡宽内容的：宽内容在气泡那一层就被卡住了
       （见 .bubble 的 max-width）。靠这里藏，只是把症状盖住。 */
  }

  .line {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 0.15rem;
    max-width: 88%;
    /* 长按要按住不动，不能变成选中/双击缩放那套手势。 */
    touch-action: manipulation;
    -webkit-tap-highlight-color: transparent;
  }
  /* 自己问的那句靠右：一眼能分出谁说的。 */
  .line.mine {
    align-self: flex-end;
    align-items: flex-end;
  }
  /* 正在改的那条：标出来，免得用户改的是哪句还要自己数。 */
  .line.editing .bubble {
    outline: 2px solid rgba(130, 200, 255, 0.55);
    outline-offset: 1px;
  }

  .bubble {
    padding: 0.5rem 0.7rem;
    border-radius: 0.75rem;
    background: rgba(244, 246, 251, 0.1);
    font-size: 0.92rem;
    line-height: 1.5;
    /* 模型会回换行与列表，这些空白得留住。 */
    white-space: pre-wrap;
    word-break: break-word;
    /* **超宽的根因就在这一条。**
       .line 是 column flex，气泡在交叉轴上按 flex-start 摆，宽度因此是 fit-content：
       fit-content = min(max-content, max(min-content, 可用宽度))。里面的 min-content
       一旦比可用宽度还大（不能折行的 pre、宽表格、KaTeX 的内部盒子、一长串没有空格的
       东西），这个 min(...) 取到的就是 min-content —— 气泡于是比 .line 宽、比屏幕宽，
       一路顶出去。overflow-wrap: anywhere 只管得住「能折的文本」，管不住这些。
       max-width: 100% 把上限钉死在 .line 的宽度上（= 记录宽度的 88%）：
       气泡不再超标，超宽的东西改在它自己内部滚（pre / .katex-display / 表格各有一条）。
       不是靠 overflow-x: hidden 藏起来的 —— 那样内容还是那么宽，只是看不见了。 */
    max-width: 100%;
  }
  .line.mine .bubble {
    background: rgba(130, 200, 255, 0.16);
    color: #dff0ff;
  }
  /* 「选择文本」按下去之后这一条才可选中。
     全局是禁选的（style.css），这里 opt-in —— user-select 是**继承**属性，
     在这一层写一次就够，不必去管子元素（KaTeX 吐出来的那一堆 span 也都跟着开）。 */
  .bubble.pick {
    -webkit-user-select: text;
    user-select: text;
    -webkit-touch-callout: default;
  }

  .at {
    font-size: 0.7rem;
    color: rgba(244, 246, 251, 0.4);
  }

  /* 思考过程那一块：在气泡**外面**（气泡是 pre-wrap 的，半截缩进会被它放大），
     所以这里自己排版。默认收着 = 不给 open 属性，交给 details 自己的行为
     （正在流的那一份才带 open，见模板里那一段）。
     位置在气泡**上面**（模板里的先后顺序就是这个意思）：它是回答的来路，先过程后结论。 */
  .think {
    /* 与 .bubble 同一条理由：.line 是 column flex，块在交叉轴上是 fit-content，
       里面的宽内容（表格、pre）会把 min-content 顶上去、一路撑出屏幕。
       上限钉在 .line 的宽度上，超宽的东西改在它们自己内部滚（Markdown.svelte 里有各条）。 */
    max-width: 100%;
    /* 虚化：比正文（.bubble 是 0.92rem）小一号、颜色更淡。
       用字号与前景色而不是 opacity —— opacity 会把折叠条、边框、背景一起变淡，
       那看起来像「这一块不能用」，而不是「这一块是注脚」。 */
    font-size: 0.8rem;
    color: rgba(244, 246, 251, 0.5);
  }
  .think-head {
    /* 整行都给点：只让「思考过程」那几个字可点的话，手指很难点中。
       它是 summary，三角标记由浏览器画（那一行左边留出的就是它）。 */
    padding: 0.25rem 0.5rem;
    border-radius: 0.4rem;
    background: rgba(244, 246, 251, 0.06);
    color: rgba(244, 246, 251, 0.55);
    cursor: pointer;
    touch-action: manipulation;
    -webkit-tap-highlight-color: transparent;
  }
  .think-body {
    margin: 0.3rem 0 0;
    padding: 0.4rem 0.6rem;
    border-left: 2px solid rgba(244, 246, 251, 0.15);
    border-radius: 0 0.4rem 0.4rem 0;
    background: rgba(6, 7, 15, 0.35);
  }

  .empty,
  .thinking {
    margin: 0;
    font-size: 0.85rem;
    color: rgba(244, 246, 251, 0.5);
  }

  .banner {
    margin: 0;
    padding: 0.45rem 0.6rem;
    border-radius: 0.5rem;
    background: rgba(255, 180, 180, 0.12);
    color: #ffb4b4;
    font-size: 0.8rem;
  }
  .notice {
    margin: 0;
    padding: 0.35rem 0.6rem;
    border-radius: 0.5rem;
    background: rgba(130, 200, 255, 0.12);
    color: #9fd4ff;
    font-size: 0.8rem;
  }

  .editbar {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    padding: 0.35rem 0.6rem;
    border-radius: 0.5rem;
    background: rgba(255, 220, 150, 0.12);
    color: #ffd699;
    font-size: 0.78rem;
  }
  .editbar-text {
    flex: 1;
  }
  .editbar-cancel {
    flex: none;
    padding: 0.15rem 0.5rem;
    border: 1px solid rgba(255, 214, 153, 0.5);
    border-radius: 0.4rem;
    background: transparent;
    color: inherit;
    font: inherit;
    cursor: pointer;
    touch-action: manipulation;
    --wails-draggable: no-drag;
  }

  .presets {
    display: flex;
    flex-wrap: wrap;
    gap: 0.35rem;
  }
  .preset {
    padding: 0.3rem 0.6rem;
    border: 1px solid rgba(130, 200, 255, 0.4);
    border-radius: 999px;
    background: rgba(130, 200, 255, 0.1);
    color: #9fd4ff;
    font: inherit;
    font-size: 0.8rem;
    cursor: pointer;
    touch-action: manipulation;
    -webkit-tap-highlight-color: transparent;
    --wails-draggable: no-drag;
  }
  .preset:disabled {
    opacity: 0.5;
  }

  .composer {
    display: flex;
    gap: 0.4rem;
  }
  .input {
    flex: 1;
    min-width: 0;
    padding: 0.5rem 0.6rem;
    border: 1px solid rgba(244, 246, 251, 0.2);
    border-radius: 0.6rem;
    background: rgba(244, 246, 251, 0.06);
    color: #f4f6fb;
    font: inherit;
    font-size: 0.9rem;
    /* 输入框里的字得能选（全选、改一个字）—— 全局那条禁选是给界面用的，不是给可编辑区域的。 */
    -webkit-user-select: text;
    user-select: text;
    --wails-draggable: no-drag;
  }
  .input::placeholder {
    color: rgba(244, 246, 251, 0.35);
  }
  .cancel,
  .ask {
    flex: none;
    padding: 0 1rem;
    border: 1px solid rgba(130, 200, 255, 0.6);
    border-radius: 0.6rem;
    background: rgba(130, 200, 255, 0.14);
    color: #9fd4ff;
    font: inherit;
    font-size: 0.9rem;
    font-weight: 600;
    cursor: pointer;
    touch-action: manipulation;
    -webkit-tap-highlight-color: transparent;
    --wails-draggable: no-drag;
  }
  .cancel {
    border-color: rgba(244, 246, 251, 0.25);
    background: transparent;
    color: rgba(244, 246, 251, 0.7);
    font-weight: 400;
  }
  .cancel:disabled,
  .ask:disabled {
    opacity: 0.4;
  }

  /* 长按菜单：fixed 是必须的 —— 它是从记录那块（自己有滚动）里冒出来的，
     用 absolute 会被 .log 裁掉。面板那一层没有 transform，所以这里坐标就是屏幕坐标。 */
  .scrim {
    position: fixed;
    inset: 0;
    z-index: 40;
    border: 0;
    padding: 0;
    background: transparent;
    -webkit-tap-highlight-color: transparent;
  }
  .menu {
    position: fixed;
    z-index: 41;
    width: 13rem;
    display: flex;
    flex-direction: column;
    padding: 0.25rem;
    border: 1px solid rgba(244, 246, 251, 0.16);
    border-radius: 0.7rem;
    background: #12162a;
    box-shadow: 0 10px 30px rgba(0, 0, 0, 0.5);
  }
  .mi {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 0.1rem;
    padding: 0.5rem 0.6rem;
    border: 0;
    border-radius: 0.5rem;
    background: transparent;
    color: #f4f6fb;
    font: inherit;
    font-size: 0.88rem;
    text-align: left;
    cursor: pointer;
    touch-action: manipulation;
    -webkit-tap-highlight-color: transparent;
    --wails-draggable: no-drag;
  }
  .mi:active {
    background: rgba(244, 246, 251, 0.08);
  }
  .mi:disabled {
    color: rgba(244, 246, 251, 0.4);
    cursor: default;
  }
  .mi-hint {
    font-size: 0.72rem;
    color: rgba(244, 246, 251, 0.4);
  }
</style>
