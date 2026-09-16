<script lang="ts">
  // 题库页：错题列表 + 点开看题图（答案图按需摊开）。
  // 列表分成学科页签：「未打标签」打头，接着每一门顶层学科；学科页里左侧还有一棵
  // 章节/知识点树，用来在那门学科里再收窄。
  // 排序、落库、取图、筛都在 Go 侧，这里只做取数与呈现 —— spec 说前端是薄视图、不做自动化测试，
  // 所以能往前端放的逻辑就别放。唯一留在前端的是「这些 id 交给谁筛」这类接线。

  import * as Library from '../bindings/questionbook/internal/library/service';
  import type { Question } from '../bindings/questionbook/internal/library/models';
  import * as Tags from '../bindings/questionbook/internal/tags/service';
  import type { Tag } from '../bindings/questionbook/internal/tags/models';
  import * as VLM from '../bindings/questionbook/internal/vlm/service';
  import type { ProposedTag } from '../bindings/questionbook/internal/vlm/models';
  import AnswerBadge from './AnswerBadge.svelte';
  import TagFilter from './TagFilter.svelte';
  import Tagging from './Tagging.svelte';
  import Discussion from './Discussion.svelte';
  // 「有没有答案图」的判断只写在一处（answer.ts），别在这儿再写一遍 q.AnswerHash !== ''。
  import { hasAnswer } from './answer';

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

  // 答案图在详情页是**摊开来看**的，但默认收着：它是对答案用的，不该一进详情就糊在脸上。
  // 点「看答案图」才去取；没有再点一下收起。取法与题图同一套（同一份缓存、同样的话术）。
  let showAnswer = $state(false);
  let answerUrl = $state<string | null>(null);
  let answerError = $state('');

  // 删除是两步的，且不可撤销（图片文件也不是这里删的），所以先问一句。
  let confirming = $state(false);
  let busy = $state(false);

  // ── 标签 ──
  // 词表是平的（父由 ParentID 指出），怎么画成三层树是 TagFilter 的事。
  let allTags = $state<Tag[]>([]);

  // ── 学科页签 + 左侧细分树 ──
  // 0 = 「未打标签」那一页。它不是一个标签（标签 id 从 1 起），只是与 Go 侧
  // 「ParentID 为 0 = 顶层」同一个约定的哨兵值。
  let subjectTab = $state(0);
  // 左侧树里勾中的章节/知识点；换学科时清空 —— 那些 id 只属于上一棵树，
  // 留着会跟着并集一起交上去，把别的学科的题也筛进来。
  // 详情页那份是 openedTagIDs（给单道题打标签），两者互不相干。
  let picked = $state<number[]>([]);
  let openedTagIDs = $state<number[]>([]);
  // 「新建学科」那个输入框开着没有。入口是标签栏末尾那个 ＋。
  let showNewSubject = $state(false);
  let showTags = $state(false);
  // 新学科的名字（顶层）。章节与知识点由识别那边建，这里只管用户自己填的学科。
  let newSubject = $state('');
  // 标签相关的失败都归这儿：建学科没成、打标签没写进去。
  let tagError = $state('');

  // 当前页签的学科；null = 停在「未打标签」那一页。
  const activeSubject = $derived(
    allTags.find((t) => t.ID === subjectTab && t.Level === 1) ?? null,
  );

  // 页签上的学科，顺序照 Go 侧 List() 给的顺序（它按 level, created_at, id 排，
  // 所以顶层学科正好在前，先建的在前）—— 这里不再排一次，再排会跟它打架。
  const subjects = $derived(allTags.filter((t) => t.Level === 1));

  // 左侧树只画当前这门学科：TagFilter 按 ParentID 拼树、认 ParentID 0 为根，
  // 所以把学科自己也传进去，它就是这棵树的根，章节与知识点照旧挂在下面。
  // 顺着 ParentID 一层层捞（最多三层，用队列而不是递归）。
  const subjectTree = $derived.by(() => {
    const subject = activeSubject;
    if (!subject) return [];

    const byParent = new Map<number, Tag[]>();
    for (const tag of allTags) {
      const siblings = byParent.get(tag.ParentID);
      if (siblings) siblings.push(tag);
      else byParent.set(tag.ParentID, [tag]);
    }

    const out: Tag[] = [subject];
    for (let i = 0; i < out.length; i++) out.push(...(byParent.get(out[i].ID) ?? []));
    return out;
  });

  // 交给 Go 的筛选集合：**学科本身始终在里面**（页签就是这个意思），左侧树勾的
  // 章节/知识点是在它之上再收窄 —— 它们本来就在这门学科下面，并集就等于收窄。
  const effective = $derived(activeSubject ? [activeSubject.ID, ...picked] : []);

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

  // 取回来的图按内容 hash 存一份：同一道题反复开合不必重走一次 IPC。
  // 题图与答案图共用这一份 —— 键是内容 hash，同 hash 就是同一张图，分开存没有意义。
  // 一张图撑死几百 KB，错题本量小，先不做淘汰。
  const cards = new Map<string, string>();

  // ── 左栏宽度（用户可拖）──
  //
  // 默认交给 CSS 那个 `clamp(8rem, 36vw, 11rem)`：它按屏宽算，换设备、转屏都自适应。
  // **只有用户亲手拖过之后**才钉成具体像素并存下来 —— 那之后就该听他的，不再跟着 vw 走。
  //
  // 存 localStorage：这个 WebView 的存储跟着应用数据走（重启在、卸载没），
  // 所以「上次调多宽」记得住。读失败（存储被禁之类）就当没存过、用默认。
  const SIDE_WIDTH_KEY = 'library.sideWidth';

  function readSavedSideWidth(): number | null {
    try {
      const raw = localStorage.getItem(SIDE_WIDTH_KEY);
      if (raw === null) return null;
      const n = Number(raw);
      return Number.isFinite(n) && n > 0 ? n : null;
    } catch {
      return null;
    }
  }

  let sideWidth = $state<number | null>(readSavedSideWidth());
  let browseEl = $state<HTMLElement | null>(null);
  // 拖拽中的快照。每次 move 都从它重算，不做增量累加（累加会被每一次舍入咬掉一点）。
  // 它不参与渲染，所以是普通变量而不是 $state。
  let resizeFrom: { x: number; width: number } | null = null;

  // 左右两头的边界：左栏最窄 8rem（再窄树里的名字只剩两个字、点不准），
  // 列表那边至少留 7rem（拖到底会把列表挤没）。
  function sideLimits(): { min: number; max: number } {
    const rem = parseFloat(getComputedStyle(document.documentElement).fontSize) || 16;
    const total = browseEl?.getBoundingClientRect().width ?? 0;
    const min = 8 * rem;
    return { min, max: Math.max(min, total - 7 * rem) };
  }

  // 拖分界条。三件事少一件都会坏：
  //   (1) 指针捕获 —— 手指滑出那条窄条、甚至滑出屏幕，move 事件照样回得来；
  //   (2) CSS 上的 `touch-action: none` —— 否则这根手指的横滑会被外层翻页手势抢走，
  //       变成翻到题库/复习页（与应用里选框角柄同一个道理）；
  //   (3) preventDefault。
  function startResize(e: PointerEvent) {
    const side = browseEl?.querySelector<HTMLElement>('.side');
    if (!side) return;
    e.preventDefault();
    (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId);
    resizeFrom = { x: e.clientX, width: side.getBoundingClientRect().width };
  }

  function resize(e: PointerEvent) {
    const from = resizeFrom;
    if (!from) return;
    const { min, max } = sideLimits();
    sideWidth = Math.round(Math.min(max, Math.max(min, from.width + (e.clientX - from.x))));
  }

  function endResize() {
    if (!resizeFrom) return;
    resizeFrom = null;
    try {
      if (sideWidth !== null) localStorage.setItem(SIDE_WIDTH_KEY, String(sideWidth));
    } catch {
      // 存不下就算了：这一次拖的宽度照用，只是下次打开回到默认。
    }
  }

  // 双击分界条 = 回到默认（那个跟着屏宽走的 clamp）。触屏上双击不一定触发，
  // 所以它只是鼠标/桌面上顺手的一个复位；触屏往回拖到差不多宽即可。
  function resetSideWidth() {
    sideWidth = null;
    try {
      localStorage.removeItem(SIDE_WIDTH_KEY);
    } catch {
      // 同上
    }
  }

  async function refresh() {
    loading = true;
    try {
      // 后端已按创建时间倒序排好（新的在前），这里不再排一次 —— 再排会跟它的并列规则打架。
      if (activeSubject) {
        // 学科页：学科本身 + 左侧树勾上的章节/知识点（这是并集，见 effective）。
        // 点父标签会自动带上子孙，所以筛与不筛是同一个调用。
        questions = (await Tags.QuestionsByTags(effective)) ?? [];
      } else {
        // 未打标签页：空数组在 QuestionsByTags 里是「不筛」（= 全部错题），
        // 表达不了「一条标签都没挂」，所以走单独那条查询。
        questions = (await Tags.UntaggedQuestions()) ?? [];
      }
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
    // 答案图重新收起来：换一道题就该从「只看题图」开始，上一道的答案不该跟过来。
    showAnswer = false;
    answerUrl = null;
    answerError = '';
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

  // 「看答案图 / 收起」。收起时**不**把取到的图丢掉：同一道题再点开是瞬时的，
  // 反正它就在内存缓存里（cards），下次开这道题照样便宜。
  async function toggleAnswer() {
    const q = opened;
    if (!q) return;
    if (showAnswer) {
      showAnswer = false;
      return;
    }
    showAnswer = true;
    if (answerUrl) return; // 这道题刚才已经取到过了
    if (!hasAnswer(q)) return; // 入口本来就不出现，这里是兜底

    answerError = '';
    const cached = cards.get(q.AnswerHash);
    if (cached) {
      answerUrl = cached;
      return;
    }

    try {
      // 与题图同一个读路径（答案图的读也归题库），回来的同样是 PNG 的 base64。
      const base64 = await Library.AnswerImage(q.AnswerHash);
      // 取图是异步的：这中间用户可能已经返回、或换了一道题 —— 别把旧图贴上去。
      if (opened?.ID !== q.ID) return;
      if (!base64) {
        answerError = '这道题的答案图不在，可能已被清理';
        return;
      }
      const url = `data:image/png;base64,${base64}`;
      cards.set(q.AnswerHash, url);
      answerUrl = url;
    } catch (err) {
      if (opened?.ID === q.ID) answerError = errorMessage(err);
    }
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

  // 左侧树勾选变了：先记下勾了什么，再重拉一次列表。
  function pickTags(ids: number[]) {
    picked = ids;
    void refresh();
  }

  // 换学科页签。勾过的章节/知识点属于上一棵树，必须清掉 ——
  // 否则它们会跟着并集一起交上去，把别的学科（或跨学科）的题筛进来。
  function selectTab(id: number) {
    if (id === subjectTab) return;
    subjectTab = id;
    picked = [];
    showNewSubject = false;
    void refresh();
  }

  function toggleNewSubject() {
    showNewSubject = !showNewSubject;
    tagError = ''; // 换一处动作，上一次的报错别留着
    // 收起来就把没提交的名字丢掉：它没有被记住过，留着下次会变成一个意外冒出来的输入。
    if (!showNewSubject) newSubject = '';
  }

  async function createSubject() {
    const name = newSubject.trim();
    if (!name) return;
    try {
      // 0 = 顶层：没有父的标签就是学科，层级由这一条推出来，不用自己算。
      // 这一格永远建**学科**，即使当前正停在某门学科里 —— 输入框上就写着「新建学科」。
      await Tags.Create(0, name);
      newSubject = '';
      tagError = '';
      await refreshTags();
      // 新学科是空的，切过去只会看到一页空列表；留在原地，它的页签已经出现在栏里了。
      showNewSubject = false;
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
    showAnswer = false;
    answerUrl = null;
    answerError = '';
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

    <!-- 题图上那几个动作单起一行：顶栏挤着「返回 + 时间 + 删除」，再塞进去就点不准了。
         窄屏上放不下就换行（见 .actions）。 -->
    <div class="actions">
      <button class="ghost" onclick={captureAnswer}>
        {opened.AnswerHash ? '重拍答案图' : '补拍答案图'}
      </button>
      <!-- 看答案图是个开关，**默认收着**：它是对答案用的，不该一进详情就糊在脸上。
           没有答案图的题不给这个入口 —— 上面那个按钮正写着「补拍答案图」，
           那句话说完了，再多一个不能用的按钮只会让人点空。 -->
      {#if hasAnswer(opened)}
        <button class="ghost" class:on={showAnswer} onclick={toggleAnswer}>
          {showAnswer ? '收起答案图' : '看答案图'}
        </button>
      {/if}
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

    {#if showAnswer}
      <!-- 摊在题图下面，两块各分一半高度，对着看（复习页宽屏上也是这个排法）。
           三种状态与题图一模一样：有图 / 取不到（文件被清理）/ 还在取。 -->
      <div class="stage answer">
        {#if answerUrl}
          <img src={answerUrl} alt="答案图" />
        {:else if answerError}
          <p class="hint error">{answerError}</p>
        {:else}
          <p class="hint">正在取答案图…</p>
        {/if}
      </div>
    {/if}

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

    <!-- 讨论常驻一行入口（组件自己带「展开」），不另设按钮：它在题库页是"看这道题"
         的一部分，而不是一次性的动作。 -->
    <div class="panel">
      <Discussion questionId={opened.ID} />
    </div>
  {:else}
    <header class="bar">
      <span class="bar-title">题库</span>
      {#if questions.length > 0}
        <span class="count">
          {effective.length > 0 ? `筛出 ${questions.length} 道` : `${questions.length} 道`}
        </span>
      {/if}
    </header>

    {#if tagError}
      <p class="banner">{tagError}</p>
    {/if}

    <!-- 学科页签：「未打标签」打头，接着是每一门顶层学科，顺序照 Go 侧 List() 给的顺序。
         装不下就横着滚（学科由用户自己加，没有上限）。 -->
    <nav class="tabs">
      <button class="ghost tab" class:on={activeSubject === null} onclick={() => selectTab(0)}>
        未打标签
      </button>
      {#each subjects as subject (subject.ID)}
        <button
          class="ghost tab"
          class:on={activeSubject?.ID === subject.ID}
          onclick={() => selectTab(subject.ID)}
        >
          {subject.Name}
        </button>
      {/each}

      <!-- 新建学科的入口放在末尾：顶层学科由用户自己填（预置的四门只是初始值），
           点开仍是那个输入框。章节与知识点由识别那边建，这一格只管学科。 -->
      <button
        class="ghost tab plus"
        class:on={showNewSubject}
        onclick={toggleNewSubject}
        aria-label="新建学科"
      >
        ＋
      </button>
    </nav>

    {#if showNewSubject}
      <div class="new-subject">
        <input bind:value={newSubject} placeholder="新建学科" onkeydown={onSubjectKey} />
        <button class="ghost" onclick={createSubject} disabled={!newSubject.trim()}>添加</button>
      </div>
    {/if}

    <!-- 「未打标签」那一页没有树可画（一条标签都没挂，没有可筛的），整幅宽度给列表。 -->
    <div class="browse" bind:this={browseEl}>
      {#if activeSubject}
        <!-- 宽度默认交给 CSS 那个 clamp（跟着屏宽走）；用户亲手拖过分界条之后由 style 钉住。 -->
        <aside class="side" style:width={sideWidth === null ? null : `${sideWidth}px`}>
          <!-- 与详情页打标签用的是同一棵树：章节与知识点交给它画，勾选与半选照旧。
               fill 让它撑满这一列、自己滚；标题写学科名，省得再猜这列筛的是哪一门。 -->
          <TagFilter
            tags={subjectTree}
            selected={picked}
            onSelect={pickTags}
            title={activeSubject.Name}
            fill
          />
        </aside>

        <!-- 分界条：拖它改左栏宽度，双击复位。10px 的命中区只画 1px 的线 ——
             细线手指按不住，宽的又难看。 -->
        <div
          class="splitter"
          role="separator"
          aria-orientation="vertical"
          aria-label="拖动调整分类树的宽度（双击复位）"
          onpointerdown={startResize}
          onpointermove={resize}
          onpointerup={endResize}
          onpointercancel={endResize}
          ondblclick={resetSideWidth}
        ></div>
      {/if}

      <div class="listpane">
        {#if listError}
          <p class="hint error">{listError}</p>
        {:else if loading && questions.length === 0}
          <p class="hint">正在读题库…</p>
        {:else if questions.length === 0}
          <div class="empty">
            {#if activeSubject}
              {#if picked.length > 0}
                <p class="empty-main">这些章节下还没有错题</p>
                <p class="empty-sub">
                  左侧树里勾的是「{activeSubject.Name}」下的章节或知识点，去掉几个勾可能就有题了。
                </p>
              {:else}
                <p class="empty-main">这门学科下还没有错题</p>
                <p class="empty-sub">给错题打上「{activeSubject.Name}」的标签，它就会出现在这里。</p>
              {/if}
            {:else}
              <p class="empty-main">没有未打标签的错题</p>
              <p class="empty-sub">滑到中间那页拍一道，它就会出现在这里 —— 打上标签之后才归到学科页。</p>
            {/if}
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
      </div>
    </div>
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

  /* 学科页签。横向排、装不下横着滚 —— 学科是用户自己加的，数量没有上限。 */
  .tabs {
    flex: none;
    display: flex;
    align-items: center;
    gap: 0.4rem;
    padding: 0.6rem 1rem;
    border-bottom: 1px solid rgba(244, 246, 251, 0.1);
    overflow-x: auto;
    /* 滑到头不把整页三页横滑也带走。 */
    overscroll-behavior-x: contain;
  }
  .tab {
    padding: 0.35rem 0.8rem;
    font-size: 0.9rem;
  }
  /* 选中的那一个要一眼看得出来：它决定这一页筛的是哪一门。 */
  .tab.on {
    background: rgba(122, 162, 255, 0.22);
    border-color: rgba(130, 200, 255, 0.6);
    color: #cfe2ff;
  }
  .tab.plus {
    padding: 0.35rem 0.7rem;
    font-weight: 700;
  }

  /* 左树 + 列表。树只在学科页出现，所以「未打标签」上这一层里只有列表，宽度整幅归它。 */
  .browse {
    flex: 1;
    min-height: 0;
    display: flex;
  }
  /* 左侧树的**默认**宽度：手机竖屏（主场景，360–430px 宽）下取 36vw ≈ 130–155px，
     给列表留下约 64%。行里只有「时间 + 有/缺答案图」两样，393px 的屏上排下来
     约 205px，留下 250px 是够的；知识点再缩进一档，36vw 里也还放得下四五个字。
     两头都收了口：最窄 8rem —— 再窄树里的名字就只剩两个字，等于没法点；
     最宽 11rem —— 它只是一列筛选，宽屏上再宽也没用，列表那边要放图。

     它只是个**默认值**：用户拖过边上那条分界条之后，宽度就由 style 上的具体像素接管，
     并存进 localStorage（见脚本里那一节）。默认跟着屏宽走、用户选过就听用户的 ——
     这两件事都要，所以不把默认值也写死成像素。 */
  .side {
    flex: none;
    /* 默认宽度。被用户拖过之后 style 上会有一个具体像素值把它盖掉。 */
    width: clamp(8rem, 36vw, 11rem);
    display: flex;
    flex-direction: column;
    padding: 0.6rem 0.4rem 0.6rem 0.6rem;
    /* 那条竖线改由 .splitter 画（它要盖在边上，而不是多占一列宽度）。 */
  }
  /* 分界条：10px 的命中区，只画 1px 的线。
     负外边距让它**骑在**边线上 —— 否则它自己会额外占掉 10px，把列表挤窄。
     `touch-action: none` 是必需的：不给的话这根手指的横滑会被外层翻页手势抢走，
     变成翻到题库/复习页（与应用里选框的四个角柄同一个道理，那边也是这么写的）。 */
  .splitter {
    flex: none;
    width: 10px;
    margin-left: -5px;
    position: relative;
    z-index: 1;
    cursor: col-resize;
    touch-action: none;
    -webkit-tap-highlight-color: transparent;
    --wails-draggable: no-drag;
  }
  .splitter::after {
    content: '';
    position: absolute;
    top: 0;
    bottom: 0;
    left: 50%;
    width: 1px;
    translate: -50% 0;
    background: rgba(244, 246, 251, 0.14);
  }
  /* 按住时那条线亮起来、加粗一点：告诉用户「你正在拖它」。 */
  .splitter:active::after {
    width: 2px;
    background: rgba(130, 200, 255, 0.75);
  }
  .listpane {
    flex: 1;
    /* 列表被树挤窄时，行里的内容自己截断而不是把这一栏撑出去。 */
    min-width: 0;
    display: flex;
    flex-direction: column;
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

  /* 有一项「开着」的按钮：这块面板开着、选中了哪个页签、或者这道题有标签。
     不改变形状，只换颜色。 */
  .ghost.on {
    border-color: rgba(130, 200, 255, 0.6);
    color: #9fd4ff;
  }

  /* 打标签与识别那两块面板共用同一格。树自己会滚（TagFilter 里封了 40vh），这里不再套一层。 */
  .panel {
    flex: 0 0 auto;
    padding: 0.75rem 1rem;
    border-top: 1px solid rgba(244, 246, 251, 0.1);
  }

  /* 新建学科那一行：贴在页签栏下面（它由页签末尾的 ＋ 开出来），所以自带一行的内外边距。 */
  .new-subject {
    flex: none;
    display: flex;
    gap: 0.5rem;
    padding: 0.6rem 1rem;
    border-bottom: 1px solid rgba(244, 246, 251, 0.1);
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
  /* 答案图摊开时与题图各占一半（两块都是 flex:1），中间画一条线分开。 */
  .stage.answer {
    border-top: 1px solid rgba(244, 246, 251, 0.1);
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
