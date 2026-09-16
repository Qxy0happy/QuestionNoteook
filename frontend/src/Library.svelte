<script lang="ts">
  // 题库页：错题列表 + 点开看题图。
  // 排序、落库、取图都在 Go 侧，这里只做取数与呈现 —— spec 说前端是薄视图、不做自动化测试，
  // 所以能往前端放的逻辑就别放。

  import * as Library from '../bindings/questionbook/internal/library/service';
  import * as Capture from '../bindings/questionbook/internal/capture/service';
  import type { Question } from '../bindings/questionbook/internal/library/models';

  // 三页是同时挂载的，切到这一页才值得拉一次数据。
  let root = $state<HTMLElement | null>(null);

  let questions = $state<Question[]>([]);
  let loading = $state(false);
  let listError = $state('');

  // 点开的那道题；null 表示停在列表上。
  let opened = $state<Question | null>(null);
  let openedUrl = $state<string | null>(null);
  // 取图失败与删除失败分开存：前者占着图的位置，后者不该把已经看到的图顶掉。
  let openedError = $state('');
  let actionError = $state('');

  // 删除是两步的，且不可撤销（图片文件也不是这里删的），所以先问一句。
  let confirming = $state(false);
  let busy = $state(false);

  // 取回来的题图按 hash 存一份：同一道题反复开合不必重走一次 IPC。
  // 一张题图撑死几百 KB，错题本量小，先不做淘汰。
  const cards = new Map<string, string>();

  async function refresh() {
    loading = true;
    try {
      // 后端已按创建时间倒序排好（新的在前），这里不再排一次 —— 再排会跟它的并列规则打架。
      questions = (await Library.List()) ?? [];
      listError = '';
    } catch (err) {
      listError = err instanceof Error ? err.message : String(err);
    } finally {
      loading = false;
    }
  }

  // 每次切到本页都重拉一次。这样刚拍完的题滑过来就在列表里，不必手动刷新。
  $effect(() => {
    const el = root;
    if (!el) return;

    // 默认 root（视口）已经算上祖先滚动容器的裁剪，滑出 .pages 时比率就是 0。
    const io = new IntersectionObserver((entries) => {
      for (const entry of entries) {
        if (entry.isIntersecting) void refresh();
      }
    });
    io.observe(el);
    return () => io.disconnect();
  });

  async function open(q: Question) {
    opened = q;
    openedError = '';
    actionError = '';
    confirming = false;
    openedUrl = cards.get(q.QuestionHash) ?? null;
    if (openedUrl) return;

    try {
      // Card 回来的是 PNG 的 base64，包成 data URL 直接给 img。
      const base64 = await Capture.Card(q.QuestionHash);
      if (!base64) {
        if (opened?.ID === q.ID) openedError = '这道题的题图不在，可能已被清理';
        return;
      }
      const url = `data:image/png;base64,${base64}`;
      cards.set(q.QuestionHash, url);
      // 取图是异步的：这中间用户可能已经返回、或换了一道题 —— 别把旧图贴上去。
      if (opened?.ID === q.ID) openedUrl = url;
    } catch (err) {
      if (opened?.ID === q.ID) openedError = err instanceof Error ? err.message : String(err);
    }
  }

  function close() {
    opened = null;
    openedUrl = null;
    openedError = '';
    actionError = '';
    confirming = false;
  }

  function startConfirm() {
    confirming = true;
  }

  function cancelConfirm() {
    confirming = false;
  }

  function confirmDelete() {
    // 闭包里 narrowed 不住 `opened`，所以判空在这儿做。
    const q = opened;
    if (q) void remove(q);
  }

  async function remove(q: Question) {
    if (busy) return;
    busy = true;
    try {
      // Delete 只删库里的行、不碰图片文件（引用计数式回收是别的票的事），并返回被删记录。
      await Library.Delete(q.ID);
      close();
      await refresh();
    } catch (err) {
      // 删失败就留在原题上把话说出来，别让用户以为删掉了。
      actionError = err instanceof Error ? err.message : String(err);
    } finally {
      busy = false;
    }
  }

  // CreatedAt 是 Go 那边来的 RFC3339 串（UTC）。显示成本地时间的「几月几日 几点几分」——
  // 秒和毫秒对「这是哪道题」没有帮助。
  function formatTime(raw: string): string {
    const t = new Date(raw);
    if (Number.isNaN(t.getTime())) return raw; // 解不了就把原串亮出来，别显示 Invalid Date
    return t.toLocaleString('zh-CN', {
      month: 'numeric',
      day: 'numeric',
      hour: '2-digit',
      minute: '2-digit',
    });
  }
</script>

<div class="library" bind:this={root}>
  {#if opened}
    <header class="bar">
      <button class="ghost" onclick={close}>返回</button>
      <span class="bar-title">{formatTime(opened.CreatedAt)}</span>
      {#if confirming}
        <button class="ghost danger" onclick={confirmDelete} disabled={busy}>
          {busy ? '删除中…' : '确认删除'}
        </button>
        <button class="ghost" onclick={cancelConfirm} disabled={busy}>取消</button>
      {:else}
        <button class="ghost danger" onclick={startConfirm}>删除</button>
      {/if}
    </header>

    {#if actionError}
      <p class="banner">{actionError}</p>
    {/if}

    <div class="stage">
      {#if openedUrl}
        <img src={openedUrl} alt="题图" />
      {:else if openedError}
        <p class="hint error">{openedError}</p>
      {:else}
        <p class="hint">正在取题图…</p>
      {/if}
    </div>
  {:else}
    <header class="bar">
      <span class="bar-title">题库</span>
      {#if questions.length > 0}
        <span class="count">{questions.length} 道</span>
      {/if}
    </header>

    {#if listError}
      <p class="hint error">{listError}</p>
    {:else if loading && questions.length === 0}
      <p class="hint">正在读题库…</p>
    {:else if questions.length === 0}
      <div class="empty">
        <p class="empty-main">还没有错题</p>
        <p class="empty-sub">滑到中间那页拍一道，它就会出现在这里。</p>
      </div>
    {:else}
      <ul class="list">
        {#each questions as q (q.ID)}
          <li>
            <button class="row" onclick={() => open(q)}>
              <span class="row-time">{formatTime(q.CreatedAt)}</span>
              <span class="row-meta" class:missing={!q.AnswerHash}>
                {q.AnswerHash ? '有答案图' : '缺答案图'}
              </span>
            </button>
          </li>
        {/each}
      </ul>
    {/if}
  {/if}
</div>

<style>
  .library {
    width: 100%;
    height: 100%;
    display: flex;
    flex-direction: column;
    /* 整页铺满：页容器是网格居中，不给尺寸的话这里会缩成内容大小。 */
    overflow: hidden;
  }

  .bar {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    padding: max(0.75rem, env(safe-area-inset-top)) 1rem 0.75rem;
    border-bottom: 1px solid rgba(244, 246, 251, 0.1);
  }
  .bar-title {
    flex: 1;
    font-size: 1.05rem;
    font-weight: 700;
    letter-spacing: 0.04em;
    /* 时间戳是可能被压窄的，截断比换行好看。 */
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .count {
    font-size: 0.85rem;
    color: rgba(244, 246, 251, 0.5);
  }

  .ghost {
    flex: none;
    padding: 0.4rem 0.85rem;
    border: 1px solid rgba(244, 246, 251, 0.24);
    border-radius: 999px;
    background: transparent;
    color: #f4f6fb;
    font: inherit;
    font-size: 0.9rem;
    cursor: pointer;
    touch-action: manipulation;
    -webkit-tap-highlight-color: transparent;
    --wails-draggable: no-drag;
  }
  .ghost:active {
    background: rgba(244, 246, 251, 0.16);
  }
  .ghost:disabled {
    opacity: 0.5;
  }
  .ghost.danger {
    border-color: rgba(255, 180, 180, 0.45);
    color: #ffb4b4;
  }

  .list {
    flex: 1;
    min-height: 0;
    margin: 0;
    padding: 0.75rem;
    list-style: none;
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
    overflow-y: auto;
    /* 列表自己滚，滑到头不把整页三页横滑也带走。 */
    overscroll-behavior-y: contain;
  }

  .row {
    width: 100%;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.75rem;
    padding: 0.9rem 1rem;
    border: 1px solid rgba(244, 246, 251, 0.12);
    border-radius: 0.75rem;
    background: rgba(244, 246, 251, 0.06);
    color: #f4f6fb;
    font: inherit;
    text-align: left;
    cursor: pointer;
    touch-action: manipulation;
    -webkit-tap-highlight-color: transparent;
    --wails-draggable: no-drag;
  }
  .row:active {
    background: rgba(244, 246, 251, 0.16);
  }
  .row-time {
    font-size: 1rem;
    font-weight: 600;
  }
  .row-meta {
    flex: none;
    font-size: 0.8rem;
    color: rgba(244, 246, 251, 0.55);
  }
  /* 缺答案图的题标记出来，这样知道该去补（story 10）。 */
  .row-meta.missing {
    color: rgba(255, 200, 130, 0.8);
  }

  .stage {
    flex: 1;
    min-height: 0;
    display: grid;
    place-items: center;
    padding: 0.75rem;
  }
  .stage img {
    max-width: 100%;
    max-height: 100%;
    /* 整张看全：题图是原始像素，不裁。 */
    object-fit: contain;
    border-radius: 0.5rem;
    /* 垫一层白：题图可能有透明区，深色底上会看成黑洞。 */
    background: #fff;
    -webkit-user-drag: none;
  }

  .empty {
    flex: 1;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: 0.6rem;
    padding: 2rem;
    text-align: center;
  }
  .empty-main {
    margin: 0;
    font-size: 1.05rem;
    font-weight: 600;
  }
  .empty-sub {
    margin: 0;
    font-size: 0.9rem;
    color: rgba(244, 246, 251, 0.5);
  }

  .hint {
    margin: 1.5rem 0 0;
    font-size: 0.9rem;
    text-align: center;
    color: rgba(244, 246, 251, 0.55);
  }
  .hint.error {
    color: #ffb4b4;
    padding: 0 1.5rem;
  }

  .banner {
    margin: 0;
    padding: 0.6rem 1rem;
    background: rgba(255, 180, 180, 0.12);
    color: #ffb4b4;
    font-size: 0.85rem;
    text-align: center;
  }
</style>
