<script lang="ts">
  // 题库页：错题列表 + 点开看题图。
  // 排序、落库、取图都在 Go 侧，这里只做取数与呈现 —— spec 说前端是薄视图、不做自动化测试，
  // 所以能往前端放的逻辑就别放。

  import * as Library from '../bindings/questionbook/internal/library/service';
  import type { Question } from '../bindings/questionbook/internal/library/models';
  import * as Tags from '../bindings/questionbook/internal/tags/service';
  import type { Tag } from '../bindings/questionbook/internal/tags/models';
  import * as VLM from '../bindings/questionbook/internal/vlm/service';
  import type { ProposedTag } from '../bindings/questionbook/internal/vlm/models';
  import AnswerBadge from './AnswerBadge.svelte';
  import TagFilter from './TagFilter.svelte';
  import Tagging from './Tagging.svelte';

  // 三页是同时挂载的，切到这一页才值得拉一次数据。
  // 题库页发起的「补拍答案图」：把这道题交给上层（App），由它切到取景页并进入补拍模式。
  let { onCaptureAnswer }: { onCaptureAnswer?: (q: Question) => void } = $props();

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

  // ── 标签 ──
  // 词表是平的（父由 ParentID 指出），怎么画成三层树是 TagFilter 的事。
  let allTags = $state<Tag[]>([]);
  // 当前筛中的标签；空数组 = 不筛。两层界面各用一份：列表页筛，详情页给单道题打。
  let picked = $state<number[]>([]);
  let openedTagIDs = $state<number[]>([]);
  let showFilter = $state(false);
  let showTags = $state(false);
  // 新学科的名字（顶层）。章节与知识点由识别那边建，这里只管用户自己填的学科。
  let newSubject = $state('');
  // 标签相关的失败都归这儿：建学科没成、打标签没写进去。
  let tagError = $state('');

  // ── VLM 打标签（票据 09）──
  // 识别是**主动触发**且**只读**的：结果先给用户改，保存那一步才写库。
  // 所以这些状态与上面那份「已经挂在题上的标签」分开存 —— 建议没保存之前不算数。
  let showTagging = $state(false);
  let vlmBusy = $state(false);
  let vlmError = $state('');
  let suggested = $state<ProposedTag[] | null>(null);
  // 调用成功但结果不能用：模型没按格式回、或一条都没给。它带着原话一起回来（票据 01 靠它调 prompt）。
  let vlmProblem = $state('');
  let vlmRaw = $state('');

  // 同一个转换在下面出现好几处，收成一个 —— 都是要把 unknown 变成能给用户看的一句话。
  function errorMessage(err: unknown): string {
    return err instanceof Error ? err.message : String(err);
  }

  // 取回来的题图按 hash 存一份：同一道题反复开合不必重走一次 IPC。
  // 一张题图撑死几百 KB，错题本量小，先不做淘汰。
  const cards = new Map<string, string>();

  async function refresh() {
    loading = true;
    try {
      // 后端已按创建时间倒序排好（新的在前），这里不再排一次 —— 再排会跟它的并列规则打架。
      // 空数组 = 不筛 = 全部错题，点父标签会自动带上子孙，所以筛与不筛是同一个调用。
      questions = (await Tags.QuestionsByTags(picked)) ?? [];
      listError = '';
    } catch (err) {
      listError = errorMessage(err);
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
        if (entry.isIntersecting) {
          void refresh();
          // 标签词表跟着一起刷：识别那边（票据 09）会在后台往里加知识点，重进本页时就该看到。
          void refreshTags();
        }
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
    showTags = false;
    openedTagIDs = [];
    void loadTags(q.ID);
    openedUrl = cards.get(q.QuestionHash) ?? null;
    if (openedUrl) return;

    try {
      // Card 回来的是 PNG 的 base64，包成 data URL 直接给 img。
      // 题图的读现在归题库（票据 15：图片的读与写都在它那一侧）。
      const base64 = await Library.QuestionImage(q.QuestionHash);
      if (!base64) {
        if (opened?.ID === q.ID) openedError = '这道题的题图不在，可能已被清理';
        return;
      }
      const url = `data:image/png;base64,${base64}`;
      cards.set(q.QuestionHash, url);
      // 取图是异步的：这中间用户可能已经返回、或换了一道题 —— 别把旧图贴上去。
      if (opened?.ID === q.ID) openedUrl = url;
    } catch (err) {
      if (opened?.ID === q.ID) openedError = errorMessage(err);
    }
  }

  // 「补拍答案图」交给上层 —— 采集界面在另一页，题库页不该知道它怎么被拉起。
  function captureAnswer() {
    if (opened) onCaptureAnswer?.(opened);
  }

  // ── 标签：取词表、筛列表、给一道题打标签 ──

  async function refreshTags() {
    try {
      allTags = (await Tags.List()) ?? [];
      tagError = '';
    } catch (err) {
      tagError = errorMessage(err);
    }
  }

  // 筛选面板勾选变了：先记下筛了什么，再重拉一次列表。
  function pickTags(ids: number[]) {
    picked = ids;
    void refresh();
  }

  async function createSubject() {
    const name = newSubject.trim();
    if (!name) return;
    try {
      // 0 = 顶层：没有父的标签就是学科，层级由这一条推出来，不用自己算。
      await Tags.Create(0, name);
      newSubject = '';
      tagError = '';
      await refreshTags();
    } catch (err) {
      // 同级重名会以错误回来（后端不做 upsert），那就把它说出来，别静默吞掉。
      tagError = errorMessage(err);
    }
  }

  function onSubjectKey(e: KeyboardEvent) {
    if (e.key === 'Enter') void createSubject();
  }

  async function loadTags(id: number) {
    try {
      const ts = (await Tags.TagsOfQuestion(id)) ?? [];
      // 取标签是异步的：这中间用户可能已经返回或换了一道题，别把旧题的标签贴上去。
      if (opened?.ID === id) openedTagIDs = ts.map((t) => t.ID);
    } catch (err) {
      if (opened?.ID === id) tagError = errorMessage(err);
    }
  }

  function toggleTags() {
    showTags = !showTags;
    showTagging = false; // 两块面板抢同一格
    tagError = '';
  }

  // 勾选变了就整体替换这道题的标签，不做「增删一条」—— 面板给的本来就是全新的整份数组。
  function saveTags(ids: number[]) {
    const q = opened;
    if (!q) return;
    openedTagIDs = ids; // 先上屏：勾选立刻要有反馈，写库在下面
    void writeTags(q.ID, ids);
  }

  async function writeTags(id: number, ids: number[]) {
    try {
      await Tags.SetQuestionTags(id, ids);
      tagError = '';
    } catch (err) {
      tagError = errorMessage(err);
      // 没写进去就把这道题真实的标签读回来 —— 界面不该停在一个假的勾选状态上。
      await loadTags(id);
    }
  }

  // ── 识别标签 ──

  async function startTagging() {
    const q = opened;
    if (!q || vlmBusy) return;
    showTags = false; // 两块面板抢同一格，一次只开一个
    showTagging = true;
    vlmBusy = true;
    vlmError = '';
    try {
      // 识别只读，随便重试都不留痕迹。
      const s = await VLM.SuggestTags(q.ID);
      // 这中间用户可能已经返回或换了一道题 —— 别把旧题的建议贴上去。
      if (opened?.ID !== q.ID) return;
      suggested = s.Tags;
      vlmProblem = s.Problem;
      vlmRaw = s.Raw;
    } catch (err) {
      if (opened?.ID === q.ID) vlmError = errorMessage(err);
    } finally {
      vlmBusy = false;
    }
  }

  function closeTagging() {
    showTagging = false;
    suggested = null;
    vlmProblem = '';
    vlmRaw = '';
    vlmError = '';
  }

  // 保存才写库。整条路径（学科+章节+知识点）由 Go 侧建或复用，回来的是这道题最终挂着的标签，
  // 所以这里重读一次而不是把建议直接当成结果 —— 目录里已有的同名标签会被复用，不是新建。
  async function saveSuggested(tags: ProposedTag[]) {
    const q = opened;
    if (!q) return;
    try {
      await VLM.SaveTags(q.ID, tags);
      closeTagging();
      tagError = '';
      await loadTags(q.ID);
    } catch (err) {
      vlmError = errorMessage(err);
    }
  }

  function close() {
    opened = null;
    openedUrl = null;
    openedError = '';
    actionError = '';
    confirming = false;
    showTags = false;
    openedTagIDs = [];
    tagError = '';
    closeTagging();
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
      actionError = errorMessage(err);
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

    <!-- 题图上的两个动作单起一行：顶栏挤着「返回 + 时间 + 删除」，再塞两个进去就点不准了。 -->
    <div class="actions">
      <button class="ghost" onclick={captureAnswer}>
        {opened.AnswerHash ? '重拍答案图' : '补拍答案图'}
      </button>
      <button class="ghost" class:on={openedTagIDs.length > 0} onclick={toggleTags}>
        标签{openedTagIDs.length > 0 ? ` ${openedTagIDs.length}` : ''}
      </button>
      <!-- 识别是主动触发的：不自动跑，用户想要才花这一次调用。 -->
      <button class="ghost" onclick={startTagging} disabled={vlmBusy}>
        {vlmBusy ? '识别中…' : '识别标签'}
      </button>
    </div>

    {#if actionError}
      <p class="banner">{actionError}</p>
    {/if}
    {#if tagError}
      <p class="banner">{tagError}</p>
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

    {#if showTags}
      <!-- 面板跟题图并排存在：打标签的时候得看得见题，否则等于闭着眼睛分类。 -->
      <div class="panel">
        <TagFilter tags={allTags} selected={openedTagIDs} onSelect={saveTags} />
      </div>
    {/if}

    {#if showTagging}
      <!-- 同样是并排：判断模型给的标签对不对，得对着题看。 -->
      <div class="panel">
        <Tagging
          suggestion={suggested}
          busy={vlmBusy}
          error={vlmError}
          problem={vlmProblem}
          raw={vlmRaw}
          vocabulary={allTags}
          onSave={saveSuggested}
          onRetry={startTagging}
          onCancel={closeTagging}
        />
      </div>
    {/if}
  {:else}
    <header class="bar">
      <span class="bar-title">题库</span>
      {#if questions.length > 0}
        <span class="count">{picked.length > 0 ? `筛出 ${questions.length} 道` : `${questions.length} 道`}</span>
      {/if}
      <button class="ghost" class:on={picked.length > 0} onclick={() => (showFilter = !showFilter)}>
        筛选{picked.length > 0 ? ` ${picked.length}` : ''}
      </button>
    </header>

    {#if tagError}
      <p class="banner">{tagError}</p>
    {/if}

    {#if showFilter}
      <div class="panel">
        <TagFilter tags={allTags} selected={picked} onSelect={pickTags} />

        <!-- 顶层学科由用户自己填（预置的四门只是初始值），所以这里得有个入口。
             章节与知识点由识别那边建，这一格只管学科。 -->
        <div class="new-subject">
          <input bind:value={newSubject} placeholder="新建学科" onkeydown={onSubjectKey} />
          <button class="ghost" onclick={createSubject} disabled={!newSubject.trim()}>添加</button>
        </div>
      </div>
    {/if}

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
              <AnswerBadge q={q} />
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
  /* 「有答案图 / 缺答案图」那一小条的样式跟着 AnswerBadge 走，这里不再留一份。 */

  /* 顶栏下面那行动作（补拍答案图 / 标签 / 识别标签）。窄屏上放不下就换行，别把字挤没。 */
  .actions {
    display: flex;
    flex-wrap: wrap;
    gap: 0.5rem;
    padding: 0.6rem 1rem 0;
  }

  /* 有一项「开着」的按钮：筛选开着、或者这道题有标签。不改变形状，只换颜色。 */
  .ghost.on {
    border-color: rgba(130, 200, 255, 0.6);
    color: #9fd4ff;
  }

  /* 筛选面板与打标签面板共用同一格。树自己会滚（TagFilter 里封了 40vh），这里不再套一层。 */
  .panel {
    flex: 0 0 auto;
    padding: 0.75rem 1rem;
    border-top: 1px solid rgba(244, 246, 251, 0.1);
  }

  .new-subject {
    display: flex;
    gap: 0.5rem;
    margin-top: 0.6rem;
  }
  .new-subject input {
    flex: 1;
    min-width: 0;
    padding: 0.4rem 0.8rem;
    border: 1px solid rgba(244, 246, 251, 0.24);
    border-radius: 999px;
    background: rgba(244, 246, 251, 0.06);
    color: #f4f6fb;
    font: inherit;
    font-size: 0.9rem;
  }
  .new-subject input::placeholder {
    color: rgba(244, 246, 251, 0.4);
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
