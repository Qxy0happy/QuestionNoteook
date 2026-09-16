<script lang="ts">
  // 与 agent 对话：问的是**整库范围**的问题（「我数学哪块最弱」），它自己去查数据。
  //
  // 为什么单独一个组件：它是**聊天**，不是一项配置 —— 一串消息加一个常驻在底部的输入框。
  // 塞在设置那张滚动表单里既不好找也不好读（用户 2026-09-16 的原话：「就做成对话标签页」）。
  // 拆开还有个好处：Agent.svelte 那边只剩「配置」这一件事，两边互不牵扯。
  //
  // 会话**不落库**：Go 侧这一路只有 Agent.Ask，没有历史（与讨论那边不一样 —— 那边的会话
  // 挂在错题上、存在库里）。所以 messages 只活在内存里：切到「设置」再切回来还在
  // （靠 Agent.svelte 一直挂着我、只是把这一块藏起来），杀掉进程就没了。
  //
  // agent 读得到一切，但**每一次写都变成一条待批准改动** —— 那是 Go 侧用反射锁与源码扫描
  // 钉死的性质（internal/agent/safety_test.go），不是这里的自觉。所以在这儿问一句最坏的结果
  // 是「多出几条待批准」，正式数据一个字节都不会动。

  import { onMount } from 'svelte';
  // 宿主事件（键盘高度那条）：见下面「键盘」那一节。
  import { Events } from '@wailsio/runtime';

  import * as Agent from '../bindings/questionbook/internal/agent/service';
  import Markdown from './Markdown.svelte';
  import Pending from './Pending.svelte';

  interface Props {
    /**
     * VLM 配好了没有。null = 还没读到那份配置 —— 这时什么都不说，免得先闪一句不准确的话。
     *
     * 要它是因为这一页现在是**默认停的那一页**：新装的机器一进来就落在这儿，却一句都问不动。
     * 与其等他打完一句再弹错，不如一开始就告诉他去哪儿配。
     */
    configured: boolean | null;
  }

  let { configured }: Props = $props();

  // 一句话。Role 与 Discussion.svelte 那套同义：'me' 靠右，'agent' 靠左。
  type Turn = {
    /** 只在这份列表里有意义：本地自增，给 {#each} 当 key 用。 */
    ID: number;
    Role: 'me' | 'agent';
    Text: string;
    /** 这一轮提了几条待批准改动。0 表示没提。 */
    Proposals: number;
    /** 非空表示这一轮没答出正常的话（回合用尽、模型一句没说…），照实说给用户看。 */
    Problem: string;
    /**
     * 这一轮的思考过程（Go 侧 Answer.Reasoning）。空串 = 没有（模型没吐、或者这一轮是用户那句）。
     *
     * 与讨论那边一样**不落库**，而且这边还多一层：agent 的对话本来就一条都不存 ——
     * messages 只活在内存里（见文件头）。所以它只在这个组件挂着的期间看得见。
     */
    Reasoning: string;
  };

  // 例子问题：点一下只把字**填进**输入框，不替他发出去 —— 发不发是他的决定。
  // （讨论页那几个预设是「点了就发」，那儿合适是因为问题短且不用改；这儿问的是整库范围的
  // 事，用户多半想按自己的情况改两个字再问。）
  //
  // 挑这两句是因为它们各自对着 agent 真有的那条路：第一句走 review_stats（票 11 的验收项
  // 原话就是这个例子），第二句走 list_tags —— 顺带会演示「提议改动 → 待批准」那一条
  // （它改不动正式数据，只能提）。别写它答不了的（比如带时间窗的「最近一周」：
  // review_stats 没有时间这个维度）。
  const EXAMPLES = ['我数学哪块最弱', '我的标签树乱不乱'];

  let messages = $state<Turn[]>([]);
  let draft = $state('');
  let busy = $state(false);
  let error = $state('');
  // 正在等回答的那一句。乐观上屏：用户打的字立刻出现，不等模型（与讨论那边同一个做法）。
  let pending = $state('');
  // 变了就让待批准清单重读一遍：agent 刚提的新东西得当场出现（与拆标签之前同一个接法）。
  let pendingKey = $state(0);
  let logEl = $state<HTMLElement | null>(null);
  let inputEl = $state<HTMLInputElement | null>(null);
  // 整个输入框那一行（不只是 input）：量「底边离屏幕底边多远」要的是它，因为底下那条导航栏
  // 是压在这一行下面的（见「键盘」那一节的 composerGap）。
  let composerEl = $state<HTMLElement | null>(null);
  // 本地 id 的计数器。不是 $state：它只在下面那两个函数里自增，自己变了不该引起重渲染。
  let nextID = 1;

  function errorMessage(err: unknown): string {
    return err instanceof Error ? err.message : String(err);
  }

  // 「最后一条提过议的回答」是哪个下标（-1 = 这一段会话里一条提议都没有）。
  //
  // 清单**只挂一处**：它列的是库里全部的待批准，不是这一轮的 —— 挂到每一条提过议的回答下面
  // 就是同一份东西重复 N 遍。挂在最后那一条下面，既挨着「刚提的那些」，又只有一份。
  const proposalSlot = $derived.by(() => {
    for (let i = messages.length - 1; i >= 0; i -= 1) {
      if (messages[i].Proposals > 0) return i;
    }
    return -1;
  });

  async function send() {
    const q = draft.trim();
    if (q === '' || busy) return;

    busy = true;
    error = '';
    pending = q;
    draft = '';

    try {
      const a = await Agent.Ask(q);
      // 上屏用的是 Go 回显的那句（a.Question，那边 TrimSpace 过），不是本地这一串 ——
      // 屏幕上看到的与库里那份是同一个东西（与讨论那边同一个做法）。
      const mine: Turn = {
        ID: nextID++,
        Role: 'me',
        Text: a.Question || q,
        Proposals: 0,
        Problem: '',
        Reasoning: '',
      };
      const reply: Turn = {
        ID: nextID++,
        Role: 'agent',
        Text: a.Text,
        // 空切片与 null 都当 0：这一轮没提改动。
        Proposals: a.Proposals?.length ?? 0,
        Problem: a.Problem,
        Reasoning: a.Reasoning ?? '',
      };
      // 撤掉「等回答」那一句与挂上这一轮要在同一次 flush 里做完：分两次写会在中间闪出一屏
      // 两个「我」。
      pending = '';
      messages = [...messages, mine, reply];
      // 无论这一轮提没提改动都 +1 —— 重读一次是最省事的「与库对齐」。
      pendingKey += 1;
    } catch (err) {
      error = errorMessage(err);
      // 失败时也得把问出去的那句留在屏幕上：agent 这一路**没有任何地方存着它**
      // （讨论那边 Go 会先把它落库，这儿不会），丢掉就是用户白打了一遍。
      pending = '';
      messages = [...messages, { ID: nextID++, Role: 'me', Text: q, Proposals: 0, Problem: '', Reasoning: '' }];
    } finally {
      busy = false;
    }
  }

  function onSubmit(event: SubmitEvent) {
    event.preventDefault();
    void send();
  }

  // 点了例子：把字填进输入框，焦点也跟过去 —— 用户接着改两个字，或者直接按「问」。
  function fillDraft(text: string) {
    draft = text;
    inputEl?.focus();
  }

  // 有新消息就把滚动条推到底：聊天窗口的默认期待是「最新那句在眼前」。
  // 依赖是**读出来的**：要跟的那两样得在下面真读一遍，Svelte 才会在它们变化时重跑这个 effect。
  $effect(() => {
    const anchor = messages.length > 0 ? messages[messages.length - 1].ID : 0;
    const waiting = pending;
    const el = logEl;
    if (!el) return;
    // 一条消息都没有、也没在等回答时不用滚 —— 那是空状态，本来就停在顶上。
    if (anchor === 0 && waiting === '') return;
    // 这一块被藏起来时（用户切到了「设置」）没有布局盒子、scrollHeight 是 0：
    // 这时把 scrollTop 写成 0，等他切回来就会看到整段会话跳回最顶上。
    // 那种情况（问了之后立刻切走、回答在后台落下来）宁可停在上一次的位置。
    if (el.scrollHeight === 0) return;
    el.scrollTop = el.scrollHeight;
  });

  // ── 键盘 ──
  //
  // 键盘高度**只能**从宿主那儿要。这不是偷懒：安卓 15 + targetSdk 35 之后系统强制边到边，
  // manifest 里的 `adjustResize` 对输入法**已经不生效**（窗口不再被压短），而 WebView 自己
  // 也没有 OSK 感知 —— `visualViewport`、`dvh`、`interactive-widget` 在 WebView 里**都不动**
  // （crbug 40287394）。
  //
  // ⚠️ 这一节原先走的正是 `visualViewport` 那条路（拿「布局视口比可视视口高出来的那一截」当
  // 键盘高度）—— 它在这个目标版本上**恒为 0**，等于没跑，输入框照样被键盘盖住。现在换成宿主
  // 报的 `common:keyboard`（那条监听在 main.go 里打开一次，与讨论页用的是同一个事件）。
  //
  // 桌面端这条事件永远不来，kbInset 一直是 0：同一份布局在桌面上照旧。
  let kbHeight = $state(0);
  // 输入框底边到屏幕底边本来有多远（底下那条导航栏 + 手势区）。抬的时候要把这一截减掉，
  // 否则输入框会浮在键盘上方、空出一个导航栏的高度。聚焦那一刻量一次 —— 那时键盘还没起来。
  let composerGap = $state(0);
  let baseHeight = 0;
  const kbInset = $derived(Math.max(0, kbHeight - composerGap));

  // 收宿主那份键盘报告。载荷是 Java 那边 JSONObject.toString() 出来的字符串，
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

    // 安卓报的是**物理**像素（WindowInsets 的坐标空间），样式用的是 CSS 像素 ——
    // 中间差一个 devicePixelRatio。不换算的话，三倍屏上会把输入框顶到屏幕中间去。
    const css = raw / (window.devicePixelRatio || 1);

    // 万一某个平台上窗口确实被压短了（别的安卓版本、别的 Wails 版本），就别再抬一次 ——
    // 抬两次会高出键盘一大截。基准是聚焦那一刻的视口高，8px 是给动画留的余量。
    const shrunk = baseHeight > 0 ? baseHeight - window.innerHeight : 0;
    kbHeight = shrunk >= css - 8 ? 0 : css;
  }

  // 聚焦时量一下输入框底边离屏幕底边多远。键盘已经起来时不量：那时的间距里含着我抬上去的
  // 那一段，量出来的基准是错的（再减一次就越抬越低）。反正接下来那条事件会用上一次量好的值。
  function measureGap(el: HTMLElement | null): void {
    if (!el) return;
    baseHeight = window.innerHeight;
    if (kbHeight !== 0) return;
    const gap = window.innerHeight - el.getBoundingClientRect().bottom;
    if (gap >= 0) composerGap = gap;
  }

  onMount(() => {
    // 宿主那条监听不在这儿开（它是接线的事，main.go 里开一次），这里只订阅。
    const off = Events.On('common:keyboard', (event) => applyKeyboard(event.data));
    return () => off();
  });
</script>

<div class="chat" style="padding-bottom: {kbInset}px">
  <div class="log" bind:this={logEl}>
    {#if messages.length === 0 && pending === ''}
      <div class="empty">
        <p class="empty-title">还没聊过。</p>
        <!-- 这段是原来设置页里那段说明的搬家（问它整库的问题 / 它改不动正式数据），
             那几句是这个页面的自我介绍，不该跟着旧版式一起丢掉。 -->
        <p class="empty-note">
          问它一句关于全部错题的话（比如「我数学哪块最弱」），它会自己去查数据。
          它改不动正式数据：想改什么只能提一条待批准改动，等你逐条点头。
        </p>
        {#if configured === false}
          <p class="banner warn">
            还没配模型 —— 先到「设置」那页填上接口地址与 API Key，这儿才问得动。
          </p>
        {/if}
        <p class="examples-label">挑一句试试（点一下只填进下面的输入框，发不发你定）：</p>
        <div class="examples">
          {#each EXAMPLES as e (e)}
            <button class="example" type="button" onclick={() => fillDraft(e)}>{e}</button>
          {/each}
        </div>
      </div>
    {/if}

    {#each messages as m, i (m.ID)}
      <div class="line" class:mine={m.Role === 'me'}>
        <div class="bubble">
          <!-- 两边都过 markdown + KaTeX：模型几乎必然吐公式，而用户自己也可能写 LaTeX
               （与讨论那边同一条规矩、同一个组件）。 -->
          {#if m.Text}
            <Markdown text={m.Text} />
          {/if}
          {#if m.Proposals > 0}
            <p class="proposed">这一轮提了 {m.Proposals} 条待批准改动，在下面。</p>
          {/if}
          {#if m.Problem}
            <!-- 模型没给出正常回答（回合用尽 / 一句没说）。它是**字段而不是错**：
                 已经提议出来的那几条改动一条都没丢，用户照样能逐条点头。 -->
            <p class="banner warn">{m.Problem}</p>
          {/if}
        </div>

        <!-- agent 这条回答的思考过程：**默认收着**，点一下才铺开。
             它与讨论那边是同一块东西、同一套理由（见 Discussion.svelte 里那段注释）：
             它是「它在干活」的凭据，不是回答本身，一直摊着会把回答挤出屏幕。
             多出来的一层：agent 一次提问要往返好几轮（查标签、查题、再回答），
             这里摆的是**几轮拼起来的**那一段 —— 拼法见 Go 侧 Answer.Reasoning。
             它也不落库，而且这边连对话本身都不落库：切走再回来（组件被卸载）就没了。 -->
        {#if m.Role === 'agent' && m.Reasoning}
          <details class="think">
            <summary class="think-head">思考过程</summary>
            <div class="think-body"><Markdown text={m.Reasoning} /></div>
          </details>
        {/if}
      </div>

      {#if i === proposalSlot}
        <!-- 待批准清单（票 11）：agent 提议的改动都在这儿，逐条批准或丢弃。
             批准之前不影响正式数据。refreshKey 每次提问后 +1，让它当场重读。 -->
        <Pending refreshKey={pendingKey} />
      {/if}
    {/each}

    {#if pending !== ''}
      <div class="line mine">
        <div class="bubble"><Markdown text={pending} /></div>
      </div>
    {/if}

    {#if busy}
      <p class="thinking">正在查…</p>
    {/if}

    {#if error}
      <p class="banner">{error}</p>
    {/if}

    {#if proposalSlot < 0}
      <!-- 这一段会话里还没提过任何提议，但库里可能躺着上一次（甚至上一次开应用时）提的：
           清单挂在末尾，打开这一页就看得见，不必等他再问一句。 -->
      <Pending refreshKey={pendingKey} />
    {/if}
  </div>

  <!-- 常驻输入框：它是这一列的最后一个孩子，所以在文档流的底部；键盘起来时由 .chat 的
       padding-bottom（kbInset）把它顶上去，记录区自己让位并滚（见上面的 .log）。
       onfocusin 而不是 onfocus：点 input 与点按钮都会冒上来，而量间距只需要一次。 -->
  <form class="composer" bind:this={composerEl} onfocusin={() => measureGap(composerEl)} onsubmit={onSubmit}>
    <input
      class="input"
      bind:this={inputEl}
      bind:value={draft}
      placeholder="问一句…"
      disabled={busy}
      autocapitalize="none"
      autocorrect="off"
      spellcheck="false"
      enterkeyhint="send"
    />
    <button class="ask" type="submit" disabled={busy || draft.trim() === ''}>问</button>
  </form>
</div>

<style>
  .chat {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
    /* 底部的内边距只在键盘兜底那条路上有值（内联样式），平时是 0。 */
  }

  /* 记录自己滚：聊得再长也不把下面的输入框顶出屏幕。
     flex:1 + min-height:0 是关键：这一块按**剩下的**空间定高，而不是按内容 ——
     内容再多也只能在自己内部滚，不会把这个弹性列撑高。 */
  .log {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    /* 滚到底之后接着滑，别把外层（横向那四页）一起带动。 */
    overscroll-behavior-y: contain;
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
    padding: 1rem;
  }

  .empty {
    display: flex;
    flex-direction: column;
    gap: 0.45rem;
  }
  .empty-title {
    margin: 0;
    font-size: 0.95rem;
    font-weight: 600;
  }
  .empty-note {
    margin: 0;
    font-size: 0.85rem;
    line-height: 1.5;
    color: rgba(244, 246, 251, 0.55);
  }
  .examples-label {
    margin: 0.25rem 0 0;
    font-size: 0.8rem;
    color: rgba(244, 246, 251, 0.45);
  }
  .examples {
    display: flex;
    flex-wrap: wrap;
    gap: 0.35rem;
  }
  /* 长相与讨论页那几个预设一样（同一个作者、同一套配色），但点下去做的事不一样：
     那边是「点了就发」，这边只是把字填进输入框 —— 所以旁边那行字要说清楚。 */
  .example {
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

  .line {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 0.15rem;
    max-width: 88%;
  }
  /* 自己问的那句靠右：一眼能分出谁说的。 */
  .line.mine {
    align-self: flex-end;
    align-items: flex-end;
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
  }
  .line.mine .bubble {
    background: rgba(130, 200, 255, 0.16);
    color: #dff0ff;
  }

  /* 提了几条改动那句话：它挨着 markdown 的最后一段，得自己带一点上间距。 */
  .proposed {
    margin: 0.45rem 0 0;
    font-size: 0.8rem;
    color: rgba(159, 212, 255, 0.9);
  }

  /* 思考过程那一块：写法与讨论页那份一样（同一块东西在两个页面上该长一个样）。
     它在气泡**外面** —— 气泡是 pre-wrap 的，半截缩进会被它放大。 */
  .think {
    /* 与 .bubble 同一条理由（见 Discussion.svelte 里 .bubble 那段）：.line 是 column flex，
       里面的宽内容（表格、pre、KaTeX 的盒子）会把 min-content 顶上去、一路撑出屏幕。
       上限钉在 .line 的宽度上，超宽的东西改在它们自己内部滚。 */
    max-width: 100%;
    font-size: 0.8rem;
    color: rgba(244, 246, 251, 0.6);
  }
  .think-head {
    /* 整行都给点：只让「思考过程」那几个字可点的话，手指很难点中。
       三角标记由浏览器画（那一行左边留出的就是它）。 */
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
    line-height: 1.5;
    overflow-wrap: anywhere;
  }
  .banner.warn {
    background: rgba(255, 200, 130, 0.12);
    color: rgba(255, 200, 130, 0.95);
  }
  /* 夹在气泡里时（模型没答上话那一条）与上面的 markdown 拉开一点。 */
  .bubble .banner {
    margin-top: 0.45rem;
  }

  /* 输入框 + 「问」。它是这一列在文档流里的最后一格，不是浮层。 */
  .composer {
    flex: none;
    display: flex;
    gap: 0.4rem;
    padding: 0.6rem 1rem;
    border-top: 1px solid rgba(244, 246, 251, 0.1);
    background: rgba(10, 12, 22, 0.6);
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
    --wails-draggable: no-drag;
  }
  .input::placeholder {
    color: rgba(244, 246, 251, 0.35);
  }
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
  .ask:disabled {
    opacity: 0.4;
  }
</style>
