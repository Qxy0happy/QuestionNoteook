<script lang="ts">
  // 待批准改动：agent 提议的改动都积在这儿，等用户逐条点头。
  //
  // 外层这样接 —— 它自带取数（与 Discussion.svelte 同一个路子），外层只要把它挂上：
  //
  //   <Pending />
  //   <Pending refreshKey={answered} />   <!-- 问完 agent 一句就把它 +1，让这里重读一遍 -->
  //
  // 不传 refreshKey 也能用：它自己会在「被划到可见」时读一次（四页是同时挂载的，
  // 与 Agent.svelte 同一个做法）。refreshKey 只是给「用户还在这一页上、agent 刚提了新东西」
  // 那一种情况准备的 —— 那时不会有可见性变化，只有那个数变了。
  //
  // 这一层是**那条唯一写路径的守门人**：Go 侧的 agent.Pending 是全应用唯一持有执行者的
  // 东西，而它只认库里已经躺着的那几行。所以这一页没有「让 agent 直接改」这种按钮，
  // 每一行都得用户自己按 —— 界面上的克制与代码里的类型是一件事的两面。
  //
  // 逻辑一行都不在这儿：列表来自 Pending.List()，批准 Approve(id)，丢弃 Discard(id)。
  // **批准失败不抛错**：它返回的那条 Problem 里是原因，而状态仍停在 pending（Go 侧
  // Pending.Approve 的说明）。所以这里不读那个返回值 —— 按完一律重读列表，
  // 失败那条会自己带着 Problem 回来，界面显示的永远是库里那一份。
  //
  // 为什么用 List() 而不是 Count()：列表已经把「有几条」说清楚了，两个都调等于拿同一件事
  // 问库两遍。Count() 留给「不打开这一页也要显示角标」的地方（比如底栏那个数字）。

  import * as Pending from '../bindings/questionbook/internal/agent/pending';
  import type { PendingChange } from '../bindings/questionbook/internal/agent/models';

  interface Props {
    /** 外层手里的一个计数器：变了就重读一遍。用它通知「刚有新提议落库了」。 */
    refreshKey?: number;
  }

  let { refreshKey = 0 }: Props = $props();

  let root = $state<HTMLElement | null>(null);
  let items = $state<PendingChange[]>([]);
  let loading = $state(false);
  let error = $state('');
  // 正在处理的那一条。同一时刻只放一条过去：批准会写数据，连点两下没有意义，
  // 而 Go 侧对「已经决定过了」是明确报错的（不做幂等）。
  let busyId = $state<number | null>(null);

  function errorMessage(err: unknown): string {
    return err instanceof Error ? err.message : String(err);
  }

  async function reload() {
    loading = true;
    try {
      items = (await Pending.List()) ?? [];
      error = '';
    } catch (err) {
      error = errorMessage(err);
    } finally {
      loading = false;
    }
  }

  async function decide(id: number, approve: boolean) {
    if (busyId !== null) return;

    busyId = id;
    error = '';
    try {
      if (approve) {
        await Pending.Approve(id);
      } else {
        await Pending.Discard(id);
      }
    } catch (err) {
      // 只有「请求本身不成立」才走到这儿：id 不在了、或者它已经决定过了。
      // 执行失败（标签已被删掉之类）不抛错，见文件头那段说明。
      error = errorMessage(err);
    } finally {
      busyId = null;
      await reload();
    }
  }

  // 划到可见才读一次（这一页常常是挂着但看不见的）。
  $effect(() => {
    const el = root;
    if (!el) return;
    const io = new IntersectionObserver((entries) => {
      for (const entry of entries) {
        if (entry.isIntersecting) void reload();
      }
    });
    io.observe(el);
    return () => io.disconnect();
  });

  // 上一次读的时候 refreshKey 是多少。null = 还没读过。
  //
  // 为什么要有这个「还没读过」的状态：挂载时那一次读由上面那个「划到可见才读」负责，
  // 这里只管**之后**的变化。两个 effect 都从零起跑的话，一挂载就会读两遍。
  // （写成 $state 而不是普通变量：普通变量在初始化时读 refreshKey 会被 Svelte 记一条
  // 「这里只取到了它的初值」的警告，而那个警告是对的 —— 它确实不是响应式的读。）
  let loadedKey = $state<number | null>(null);
  $effect(() => {
    const k = refreshKey;
    if (loadedKey === null) {
      loadedKey = k;
      return;
    }
    if (k === loadedKey) return;
    loadedKey = k;
    void reload();
  });

  // 动作的名字。取值就是 Go 侧 agent.Action 那几个（两边是一份契约的两半）。
  function actionLabel(action: string): string {
    switch (action) {
      case 'create_tag':
        return '新建标签';
      case 'rename_tag':
        return '改名';
      case 'delete_tag':
        return '删除标签';
      case 'tag_question':
        return '换标签';
    }
    // 认不出来的照实显示：Go 那边加了新动作而这里还没跟上时，
    // 用户看到的应该是一个陌生的名字，而不是一片空白。
    return action;
  }

  // 删除是**级联**的（连子树一起删，还会从错题上摘掉这些标签）。
  // 波及面写在 Summary 里，这里只是给它一点视觉分量。
  function isDestructive(action: string): boolean {
    return action === 'delete_tag';
  }

  // 「今天 10:04」「09-16 10:04」。与 Discussion.svelte 同一个做法：不做「3 分钟前」，
  // 那种说法得跟着现在走，而这一块不会自己刷新。
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
</script>

<div class="pending" bind:this={root}>
  <header class="bar">
    <span class="bar-title">待批准改动</span>
    {#if items.length > 0}
      <span class="count">{items.length} 条</span>
    {/if}
  </header>

  <div class="body">
    <p class="note">agent 建议的改动都排在这儿。它自己改不了任何东西 —— 你按了「批准」，那条才生效。</p>

    {#if error}
      <p class="banner">{error}</p>
    {/if}

    {#if loading && items.length === 0}
      <p class="hint">正在读…</p>
    {:else if items.length === 0}
      <p class="hint">没有待批准的改动。</p>
    {:else}
      {#each items as item (item.ID)}
        <article class="item" class:danger={isDestructive(item.Action)}>
          <div class="head">
            <span class="action">{actionLabel(item.Action)}</span>
            <time class="at">{timeText(item.CreatedAt)}</time>
          </div>

          <!-- Summary 是 Go 侧在**提议时**写好的那句话（不是模型的措辞）：
               改哪个、波及多少。用户就是照着它点头的，所以它是这一行的主体。 -->
          <p class="summary">{item.Summary}</p>

          {#if item.Reason}
            <p class="reason">理由：{item.Reason}</p>
          {/if}

          {#if item.Problem}
            <!-- 上一次批准没成功。状态仍是「待批准」，所以它还在这一行上，
                 用户看完原因可以再按一次，或者丢掉它。 -->
            <p class="banner">上次没批成：{item.Problem}</p>
          {/if}

          <div class="actions">
            <button
              class="pill go"
              onclick={() => decide(item.ID, true)}
              disabled={busyId !== null}
            >
              {busyId === item.ID ? '处理中…' : '批准'}
            </button>
            <button class="pill" onclick={() => decide(item.ID, false)} disabled={busyId !== null}>
              丢弃
            </button>
          </div>
        </article>
      {/each}
    {/if}
  </div>
</div>

<style>
  .pending {
    display: flex;
    flex-direction: column;
    width: 100%;
  }

  .bar {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    padding: 0.6rem 0.15rem;
  }
  .bar-title {
    flex: 1;
    font-size: 0.9rem;
    font-weight: 600;
  }
  .count {
    font-size: 0.8rem;
    color: #9fd4ff;
  }

  .body {
    display: flex;
    flex-direction: column;
    gap: 0.6rem;
  }

  .note {
    margin: 0;
    font-size: 0.8rem;
    line-height: 1.5;
    color: rgba(244, 246, 251, 0.5);
  }

  .hint {
    margin: 0;
    font-size: 0.85rem;
    color: rgba(244, 246, 251, 0.5);
  }

  .item {
    display: flex;
    flex-direction: column;
    gap: 0.4rem;
    padding: 0.6rem 0.7rem;
    border: 1px solid rgba(244, 246, 251, 0.12);
    border-radius: 0.75rem;
    background: rgba(244, 246, 251, 0.04);
  }
  /* 删除一条是破坏性的（连子树一起删、还会从错题上摘标签）。摘要里写着波及面，
     这一圈描边只是让它别混在其它几条里。 */
  .item.danger {
    border-color: rgba(255, 180, 180, 0.35);
  }

  .head {
    display: flex;
    align-items: baseline;
    gap: 0.5rem;
  }
  .action {
    padding: 0.1rem 0.45rem;
    border-radius: 999px;
    background: rgba(130, 200, 255, 0.14);
    color: #9fd4ff;
    font-size: 0.72rem;
  }
  .item.danger .action {
    background: rgba(255, 180, 180, 0.14);
    color: #ffb4b4;
  }
  .at {
    flex: 1;
    text-align: right;
    font-size: 0.7rem;
    color: rgba(244, 246, 251, 0.4);
  }

  .summary {
    margin: 0;
    font-size: 0.92rem;
    line-height: 1.5;
    overflow-wrap: anywhere;
  }

  .reason {
    margin: 0;
    font-size: 0.8rem;
    line-height: 1.5;
    color: rgba(244, 246, 251, 0.5);
    overflow-wrap: anywhere;
  }

  .banner {
    margin: 0;
    padding: 0.4rem 0.55rem;
    border-radius: 0.5rem;
    background: rgba(255, 180, 180, 0.12);
    color: #ffb4b4;
    font-size: 0.8rem;
    line-height: 1.5;
    overflow-wrap: anywhere;
  }

  .actions {
    display: flex;
    gap: 0.5rem;
    padding-top: 0.1rem;
  }
  .pill {
    flex: none;
    padding: 0.45rem 1rem;
    border: 1px solid rgba(244, 246, 251, 0.24);
    border-radius: 999px;
    background: transparent;
    color: #f4f6fb;
    font: inherit;
    font-size: 0.85rem;
    cursor: pointer;
    touch-action: manipulation;
    -webkit-tap-highlight-color: transparent;
    --wails-draggable: no-drag;
  }
  .pill:active {
    background: rgba(244, 246, 251, 0.16);
  }
  .pill:disabled {
    opacity: 0.5;
  }
  .pill.go {
    border-color: rgba(130, 200, 255, 0.6);
    color: #9fd4ff;
  }
</style>
