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
  // 问出去走 Ask()（spec：逻辑刻意不往前端放，这里不做自动化测试）。

  import { onMount } from 'svelte';
  import * as Discussion from '../bindings/questionbook/internal/discussion/service';

  // 只用到这四个字段，所以不从生成的 bindings 里 import 模型类型（与 Tagging.svelte 同一个
  // 做法）：形状对得上就够。Role 在这儿当普通字符串看 —— Wails 给 Go 的具名字符串类型生成的
  // 是 TS 枚举，拿枚举去跟 'user' 比过不了类型检查（字符串枚举是 string 的子类型，
  // 反过来赋值没问题，所以绑定回来的东西直接这么收下即可）。
  type ChatMessage = {
    ID: number;
    Role: string;
    Text: string;
    CreatedAt: string;
  };

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

  onMount(() => {
    void loadPresets();
  });

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

  async function loadPresets() {
    try {
      presets = (await Discussion.Presets()) ?? [];
    } catch {
      // 预设只是入口：取不到就只剩自由输入，不必把整块顶成错误。
    }
  }

  async function loadHistory(id: number) {
    try {
      const msgs = (await Discussion.History(id)) ?? [];
      // 取数是异步的：这中间用户可能已经翻到下一道题了，别把旧会话贴上去。
      if (questionId !== id) return;
      messages = msgs;
    } catch (err) {
      if (questionId === id) error = errorMessage(err);
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

    try {
      const turn = await Discussion.Ask(id, body);
      // 这中间换了题：结果丢掉（它已经在库里了，回到那道题还看得到）。
      if (questionId !== id) return;
      messages = [...messages, turn.Question, turn.Reply];
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
      }
    }
  }

  function onSubmit(event: SubmitEvent) {
    event.preventDefault();
    void send(draft);
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
      <div class="panel">
        <div class="log">
          {#if messages.length === 0 && !pending}
            <p class="empty">还没聊过。挑一句问起，也可以自己打字。</p>
          {/if}

          {#each messages as m (m.ID)}
            <div class="line" class:mine={m.Role === 'user'}>
              <div class="bubble">{m.Text}</div>
              <time class="at">{timeText(m.CreatedAt)}</time>
            </div>
          {/each}

          {#if pending}
            <div class="line mine">
              <div class="bubble">{pending}</div>
            </div>
          {/if}

          {#if busy}
            <p class="thinking">正在想…</p>
          {/if}
        </div>

        {#if error}
          <p class="banner">{error}</p>
        {/if}

        {#if presets.length > 0}
          <div class="presets">
            {#each presets as p (p)}
              <button class="preset" onclick={() => send(p)} disabled={busy}>{p}</button>
            {/each}
          </div>
        {/if}

        <form class="composer" onsubmit={onSubmit}>
          <input
            class="input"
            bind:value={draft}
            placeholder="继续问…"
            disabled={busy}
            enterkeyhint="send"
          />
          <button class="ask" type="submit" disabled={busy || draft.trim() === ''}>问</button>
        </form>
      </div>
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
    padding-bottom: 0.25rem;
  }

  /* 记录自己滚：讨论再长也不把下面的输入框顶出屏幕。 */
  .log {
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
    max-height: 40vh;
    overflow-y: auto;
    overscroll-behavior-y: contain;
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

  .at {
    font-size: 0.7rem;
    color: rgba(244, 246, 251, 0.4);
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
