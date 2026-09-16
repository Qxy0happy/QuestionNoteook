<script lang="ts">
  // 复习页：今日队列 + 四档评级。
  //
  // 队列里装什么、评级往后推多久，全在 Go 侧（ADR-0002 说调度不跑前端），
  // 这里只做取数与呈现 —— spec 说前端是薄视图、不做自动化测试，所以能往前端放的逻辑就别放。
  //
  // 这一页不需要父组件给任何东西：被划到可见时它自己重拉一次队列。

  import * as Review from '../bindings/questionbook/internal/review/service';
  import { Rating } from '../bindings/questionbook/internal/review/models';
  import type { QueueItem, ReviewResult } from '../bindings/questionbook/internal/review/models';
  import * as Workload from '../bindings/questionbook/internal/workload/service';
  import * as Digest from '../bindings/questionbook/internal/digest/service';
  import type { Recommendation } from '../bindings/questionbook/internal/workload/models';
  import * as Library from '../bindings/questionbook/internal/library/service';
  import * as Tags from '../bindings/questionbook/internal/tags/service';
  import { Level } from '../bindings/questionbook/internal/tags/models';
  import type { Tag } from '../bindings/questionbook/internal/tags/models';
  import AnswerBadge from './AnswerBadge.svelte';
  import Discussion from './Discussion.svelte';
  // 四档的名字（与「数值 → 名字」那一步）只在 ratings.ts 里写一遍，
  // 设置页的四档间隔预览用的是同一份。
  import { RATINGS, ratingLabel } from './ratings';
  // 放大（捏合 + 拖动）与题库详情页共用同一份手势实现。
  import { pinchZoom, ZOOMED_AT } from './zoom';

  let root = $state<HTMLElement | null>(null);

  // 本次队列的快照。队列的长度就是「今天还剩多少道」—— 评过的那道会从这里移走，
  // 所以它不会在同一屏上再出现一次。重进本页会重拉一次，
  // 服务端那时也已经把它推到明天之后了，同样捞不回来。
  let queue = $state<QueueItem[]>([]);
  // 正在做的那一道。它留在 queue 里直到评级为止 —— 这样「还剩 N 道」是在做完的那一刻才减一。
  let current = $state<QueueItem | null>(null);

  let loading = $state(false);
  let listError = $state('');

  // 放大。手势本身在 zoom.ts 里，这里只用它的回调记一下「放大没有」—— 放大时得把溢出裁掉，
  // 否则放大的那半张会盖到另一张图上（或者底部那排评级按钮上）。
  let qZoomed = $state(false);
  let aZoomed = $state(false);

  // 今天建议再做几道（由 VLM 看着「还剩多少道」与「最近表现」给）。null = 没有推荐 ——
  // 没配 VLM、断网、模型答得不能用，或者今天压根没到期题，这几种情况在这一页上是同一件事：
  // 不显示这一条。推荐是锦上添花，它拿不到时队列照常工作。
  let rec = $state<Recommendation | null>(null);
  // 用户采纳之后的那道闸：今天只做 cap 道。**0 = 没有闸**（不采纳，或者压根没推荐）。
  //
  // 闸只存在于这一页的内存里：队列本身一个字节都没被改（见 Go 侧的 internal/workload），
  // 所以「不采纳 = 回到纯 FSRS」不是一句承诺，而是「什么都没发生」。
  let cap = $state(0);
  // 采纳之后已经评了几道。闸是「再做 cap 道」，所以数的是从采纳那一刻起的新账。
  let done = $state(0);

  // 「还剩多少道」。有闸时它取闸与队列里较小的那个 —— 队列本身不会因为闸而变短。
  const remaining = $derived(cap > 0 ? Math.min(queue.length, Math.max(0, cap - done)) : queue.length);
  // 到量了：闸卡住了，但队列里还有题（队列空的时候走的是「今天没有要复习的题」那一屏）。
  //
  // 还要求 current 为 null：做完闸里的最后一道时，那一道的答案图与标签正显示在屏幕上，
  // 不该被这一屏顶掉（要等用户按了「下一题」）。showNext 里那条同样的判断负责把它们收起来。
  const finished = $derived(cap > 0 && done >= cap && queue.length > 0 && current === null);

  let questionUrl = $state<string | null>(null);
  let questionError = $state('');

  // 这一道题的评级结果；null = 还没自评。答案图与标签都在它落地之后才取 ——
  // 自评之前不该把答案拉到眼前（先只看题图，自己先做一遍）。
  let result = $state<ReviewResult | null>(null);
  let answerUrl = $state<string | null>(null);
  let answerError = $state('');
  let tags = $state<Tag[]>([]);
  let grading = $state(false);

  function errorMessage(err: unknown): string {
    return err instanceof Error ? err.message : String(err);
  }

  // 「下次 3 天后」。间隔是 Go 侧算出来的整天数；1 天就是明天 ——
  // 队列按本地日取，所以这里说「明天」是准的。
  function nextDueText(res: ReviewResult): string {
    return res.ScheduledDays <= 1 ? '明天再复习' : `${res.ScheduledDays} 天后再复习`;
  }

  async function refresh() {
    loading = true;
    try {
      // 后端已按到期时刻排好（拖得最久的在前），这里不再排一次 —— 再排会跟它的并列规则打架。
      queue = (await Review.Queue()) ?? [];
      listError = '';
      showNext();
      // 推荐**不等**：它是另一次网络往返（Go 侧要去问模型），队列不该为它多转一会儿。
      // 拿回来时页面早就画好了，那条建议自己冒出来就行。
      void loadRecommendation();
    } catch (err) {
      listError = errorMessage(err);
    } finally {
      loading = false;
    }
  }

  // 每次切到本页都重拉一次 —— 与题库页同一个做法。
  // 刚评过的题在这一刻已经在服务端被推到明天之后了，所以重拉不会把它捞回来。
  $effect(() => {
    const el = root;
    if (!el) return;

    const io = new IntersectionObserver((entries) => {
      for (const entry of entries) {
        if (entry.isIntersecting) void refresh();
      }
    });
    io.observe(el);
    return () => io.disconnect();
  });

  // 端上队头那一道。队列空了就停在「今天做完了」。
  function showNext() {
    // 到量了就停在这儿。队列里还有题，但今天按建议已经够了 —— 想接着做，收工那一屏上
    // 有一个把闸摘掉的按钮。
    if (cap > 0 && done >= cap) {
      current = null;
      result = null;
      answerUrl = null;
      answerError = '';
      tags = [];
      questionUrl = null;
      questionError = '';
      return;
    }

    const next = queue[0] ?? null;
    current = next;
    result = null;
    answerUrl = null;
    answerError = '';
    tags = [];
    questionUrl = null;
    questionError = '';
    if (next) void loadQuestion(next);
  }

  async function grade(rating: Rating) {
    const item = current;
    if (!item || result || grading) return;

    grading = true;
    try {
      const res = await Review.Grade(item.Question.ID, rating);
      result = res;
      // 做完的从本次队列里移走：「还剩 N 道」当场减一，它也不会再被端上来。
      queue = queue.filter((q) => q.Question.ID !== item.Question.ID);
      // 采纳过推荐的话，这一道也算进今天那道闸里。
      if (cap > 0) done += 1;
      // 评完一道顺手把宿主那份排程重算一遍：评过的题被推到以后了，明天那条汇总的数字
      // 得跟着变。不重算的话，只要用户不打开设置页，排程就只在应用启动那一刻算过一次。
      //
      // 有三处机会兜着它（启动时、退到后台时、以及这里），所以漏一次是自愈的。
      // 静默失败：排程旧一点，比弹一个用户没法处理的错误好。
      void Digest.Refresh().catch(() => {});
      // 答案图与标签这时候才取 —— 评完才有可对照的东西。
      void loadAnswer(item);
      void loadTags(item.Question.ID);
    } catch (err) {
      // 没记下就别装作记下了：留在这一道上把话说出来，用户还能再评一次。
      listError = errorMessage(err);
    } finally {
      grading = false;
    }
  }

  // 取回来的图按 hash 存一份：同一道题反复看不必重走一次 IPC（与题库页同一个做法）。
  // 一张图撑死几百 KB，错题本量小，先不做淘汰。
  const images = new Map<string, string>();

  // 取一张图并包成 data URL；取不到返回 null（取不到的原因由调用方决定怎么说）。
  async function fetchImage(hash: string): Promise<string | null> {
    const cached = images.get(hash);
    if (cached) return cached;
    try {
      const base64 = await Library.QuestionImage(hash);
      if (!base64) return null;
      const url = `data:image/png;base64,${base64}`;
      images.set(hash, url);
      return url;
    } catch {
      return null;
    }
  }

  async function loadQuestion(item: QueueItem) {
    const url = await fetchImage(item.Question.QuestionHash);
    // 取图是异步的：这中间用户可能已经翻到下一道了，别把旧图贴上去。
    if (current?.Question.ID !== item.Question.ID) return;
    questionUrl = url;
    questionError = url ? '' : '这道题的题图不在，可能已被清理';
  }

  async function loadAnswer(item: QueueItem) {
    if (!item.Question.AnswerHash) return; // 还没拍答案图：界面上有 AnswerBadge 标着，不算错

    const url = await fetchImage(item.Question.AnswerHash);
    if (current?.Question.ID !== item.Question.ID) return;
    answerUrl = url;
    answerError = url ? '' : '这道题的答案图不在，可能已被清理';
  }

  async function loadTags(id: number) {
    try {
      const ts = (await Tags.TagsOfQuestion(id)) ?? [];
      // 同上：异步回来时可能已经换了一道题。
      if (current?.Question.ID === id) tags = ts;
    } catch {
      // 标签只是锦上添花，取不到就不显示，别把整页顶成错误。
    }
  }

  // 闭包里 narrowed 不住 `current`，所以判空在这儿做。
  function goNext() {
    showNext();
  }

  // 问一次「今天建议再做几道」。
  //
  // **失败是静默的**：没配 VLM、断网、模型答得不能用，都只是这一条不出现而已 ——
  // 队列、评级、FSRS 那一条链一点不受影响。所以这里刻意不写 listError：
  // 那个位置留给「复习本身坏了」，把推荐的问题混进去会让用户以为复习不能用了。
  async function loadRecommendation() {
    // 已经采纳过了：今天不再问第二次（用户已经定了，再问一次只是白花一次调用）。
    if (cap > 0) return;
    try {
      const r = await Workload.Recommend();
      // Suggest 为 0 表示没有可推荐的（队列已经空了）—— 与拿不到推荐一样，什么都不显示。
      rec = r && r.Suggest > 0 ? r : null;
    } catch {
      rec = null;
    }
  }

  // 采纳：今天只做这么多。队列一动不动 —— 这道闸从头到尾只是这一页内存里的一个数。
  function acceptRec() {
    if (!rec) return;
    cap = rec.Suggest;
    done = 0;
    rec = null;
  }

  // 不采纳：这一条不再出现。此后这一页的行为与没有推荐时逐字相同。
  function dismissRec() {
    rec = null;
  }

  // 把闸摘掉，接着做完剩下的。
  function clearCap() {
    cap = 0;
    done = 0;
    showNext();
  }
</script>

<div class="review" bind:this={root}>
  <header class="bar">
    <span class="bar-title">复习</span>
    {#if current}
      <span class="count">还剩 {remaining} 道</span>
    {/if}
  </header>

  {#if listError}
    <p class="banner">{listError}</p>
  {/if}

  <!-- 今天建议再做几道（story 25）。它是**一道闸**，不是新的到期时间：采纳只是让这一页
       在 cap 道之后停下来，库里的到期时刻一个都没动，不采纳则等于这一条不存在。 -->
  {#if cap > 0 && !finished}
    <div class="rec">
      <span class="rec-text">按建议做 {cap} 道 · 已完成 {done}</span>
      <button class="rec-btn" onclick={clearCap}>继续做完</button>
    </div>
  {:else if rec}
    <div class="rec">
      <span class="rec-text">
        今天建议再做 <strong>{rec.Suggest}</strong> 道{rec.Reason ? ` · ${rec.Reason}` : ''}
      </span>
      <button class="rec-btn" onclick={acceptRec}>就做这么多</button>
      <button class="rec-btn ghost" onclick={dismissRec}>不用</button>
    </div>
  {/if}

  {#if loading && !current}
    <p class="hint">正在读复习队列…</p>
  {:else if finished}
    <div class="empty">
      <p class="empty-main">今天做满 {cap} 道了</p>
      <p class="empty-sub">按建议收工。剩下的 {queue.length} 道还在，想接着做随时可以。</p>
      <button class="primary" onclick={clearCap}>继续做剩下的 {queue.length} 道</button>
    </div>
  {:else if !current}
    <div class="empty">
      <p class="empty-main">今天没有要复习的题</p>
      <p class="empty-sub">到期日是今天或更早的题会出现在这里。去题库页多拍几道吧。</p>
    </div>
  {:else}
    <!-- 题图占满中间，自己滚；评级条钉在下面，单手也够得着。
         宽屏上评完答案图并到右边，题图与答案对着看。 -->
    <div class="stage" class:side={answerUrl !== null}>
      <figure
        class="shot"
        class:zoomed={qZoomed}
        use:pinchZoom={{
          resetKey: current?.Question.ID,
          onChange: (st) => (qZoomed = st.scale > ZOOMED_AT),
        }}
      >
        {#if questionUrl}
          <img src={questionUrl} alt="题图" />
        {:else if questionError}
          <figcaption class="hint error">{questionError}</figcaption>
        {:else}
          <figcaption class="hint">正在取题图…</figcaption>
        {/if}
      </figure>

      {#if result && current.Question.AnswerHash}
        <!-- 答案图是评完才出现的，所以这块整个是新的 DOM —— 手势从「铺满看全」重新开始，
             不必给 resetKey。 -->
        <figure
          class="shot answer"
          class:zoomed={aZoomed}
          use:pinchZoom={{ onChange: (st) => (aZoomed = st.scale > ZOOMED_AT) }}
        >
          {#if answerUrl}
            <img src={answerUrl} alt="答案图" />
          {:else if answerError}
            <figcaption class="hint error">{answerError}</figcaption>
          {:else}
            <figcaption class="hint">正在取答案图…</figcaption>
          {/if}
        </figure>
      {/if}
    </div>

    {#if result}
      <!-- 讨论夹在图与底部栏之间：面板展开时它自己长高、题图那块让位，
           塞进底部栏会把评级条顶出屏幕。评完之后才出现 —— 就这道题提问是"做完"之后的事。 -->
      <div class="discuss">
        <Discussion questionId={current.Question.ID} />
      </div>
    {/if}

    <div class="foot">
      <!-- 缺答案图要在自评**之前**标出来（story 10）：评完才发现没答案可对，这一遍就白做了。 -->
      <div class="meta">
        <AnswerBadge q={current.Question} />
      </div>

      {#if result}
        <div class="settled">
          <span class="settled-rating">{ratingLabel(result.Rating)}</span>
          <span class="settled-next">{nextDueText(result)}</span>
        </div>

        {#if tags.length > 0}
          <!-- 自评完看到这道题在考什么（story 24）。 -->
          <div class="tags">
            {#each tags as tag (tag.ID)}
              <span class="tag" class:point={tag.Level === Level.LevelPoint}>{tag.Name}</span>
            {/each}
          </div>
        {/if}

        <button class="primary" onclick={goNext}>下一题</button>
      {:else}
        <!-- 自己先做一遍，然后在这里自评（story 21、22）—— 评完才看得到答案图。 -->
        <div class="ratings">
          {#each RATINGS as r (r.value)}
            <button class="rating" onclick={() => grade(r.value)} disabled={grading}>
              <span class="rating-label">{r.label}</span>
              <span class="rating-hint">{r.hint}</span>
            </button>
          {/each}
        </div>
      {/if}
    </div>
  {/if}
</div>

<style>
  .review {
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
  }
  .count {
    font-size: 0.85rem;
    color: rgba(244, 246, 251, 0.5);
  }

  /* 推荐条：夹在标题栏与题图之间，窄窄一条。它随时可能整个不渲染（拿不到推荐时），
     所以这里不能有任何「撑住布局」的职责。 */
  .rec {
    flex: none;
    display: flex;
    align-items: center;
    gap: 0.5rem;
    padding: 0.5rem 1rem;
    background: rgba(130, 200, 255, 0.1);
    border-bottom: 1px solid rgba(130, 200, 255, 0.2);
  }
  .rec-text {
    flex: 1;
    font-size: 0.82rem;
    line-height: 1.35;
    color: rgba(159, 212, 255, 0.95);
  }
  .rec-btn {
    flex: none;
    padding: 0.35rem 0.6rem;
    border: 1px solid rgba(130, 200, 255, 0.5);
    border-radius: 0.6rem;
    background: rgba(130, 200, 255, 0.16);
    color: #9fd4ff;
    font: inherit;
    font-size: 0.78rem;
    cursor: pointer;
    touch-action: manipulation;
    -webkit-tap-highlight-color: transparent;
    --wails-draggable: no-drag;
  }
  .rec-btn:active {
    background: rgba(130, 200, 255, 0.3);
  }
  /* 「不用」比「就做这么多」轻一档：采纳是主路，但不采纳也不必躲着。 */
  .rec-btn.ghost {
    border-color: rgba(244, 246, 251, 0.24);
    background: transparent;
    color: rgba(244, 246, 251, 0.7);
  }

  .stage {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
    gap: 0.5rem;
    padding: 0.75rem;
    /* 图比屏高时图自己滚，滑到头不把整页三页横滑也带走。 */
    overflow-y: auto;
    overscroll-behavior-y: contain;
  }

  /* 讨论收成一行时不该抢地方；展开的那份日志自己滚（组件里封了 40vh），
     所以这里是 flex:0 0 auto —— 面板长高时让 .stage 让位，而不是把底部栏顶出去。 */
  .discuss {
    flex: 0 0 auto;
    padding: 0 1rem;
  }
  /* 两张图都在、且屏够宽才分左右：只有一张时铺满整屏更清楚。 */
  @media (min-aspect-ratio: 1/1) {
    .stage.side {
      flex-direction: row;
    }
  }

  .shot {
    flex: 1;
    min-height: 0;
    margin: 0;
    display: grid;
    place-items: center;
  }
  .shot img {
    max-width: 100%;
    max-height: 100%;
    /* 整张看全：题图是原始像素，不裁。 */
    object-fit: contain;
    border-radius: 0.5rem;
    /* 垫一层白：题图可能有透明区，深色底上会看成黑洞。 */
    background: #fff;
    -webkit-user-drag: none;
  }
  /* 答案图那半张压一档亮度：对照时眼睛该落在题图上。 */
  .shot.answer img {
    opacity: 0.92;
  }
  /* 放大之后把溢出裁掉：transform 不改布局，不裁的话放大的那半张会盖到另一张图上，
     还会把 .stage 那条滚动条撑出来。 */
  .shot.zoomed {
    overflow: hidden;
  }

  .foot {
    flex: none;
    display: flex;
    flex-direction: column;
    gap: 0.6rem;
    /* 底部安全区归导航栏（它在最下面），这里只留自己的间距。 */
    padding: 0.6rem 1rem 0.75rem;
    border-top: 1px solid rgba(244, 246, 251, 0.1);
  }

  .meta {
    display: flex;
    align-items: center;
    gap: 0.75rem;
  }

  .ratings {
    display: grid;
    grid-template-columns: repeat(4, 1fr);
    gap: 0.5rem;
  }
  .rating {
    display: flex;
    flex-direction: column;
    align-items: center;
    gap: 0.15rem;
    padding: 0.7rem 0.25rem;
    border: 1px solid rgba(244, 246, 251, 0.24);
    border-radius: 0.75rem;
    background: rgba(244, 246, 251, 0.06);
    color: #f4f6fb;
    font: inherit;
    cursor: pointer;
    touch-action: manipulation;
    -webkit-tap-highlight-color: transparent;
    --wails-draggable: no-drag;
  }
  .rating:active {
    background: rgba(244, 246, 251, 0.16);
  }
  .rating:disabled {
    opacity: 0.5;
  }
  .rating-label {
    font-size: 1rem;
    font-weight: 600;
  }
  .rating-hint {
    font-size: 0.7rem;
    color: rgba(244, 246, 251, 0.55);
  }

  .settled {
    display: flex;
    align-items: baseline;
    gap: 0.6rem;
  }
  .settled-rating {
    font-size: 1.05rem;
    font-weight: 700;
  }
  .settled-next {
    font-size: 0.9rem;
    color: rgba(130, 200, 255, 0.85);
  }

  .tags {
    display: flex;
    flex-wrap: wrap;
    gap: 0.35rem;
  }
  .tag {
    padding: 0.15rem 0.55rem;
    border-radius: 999px;
    background: rgba(244, 246, 251, 0.1);
    font-size: 0.78rem;
    color: rgba(244, 246, 251, 0.75);
  }
  /* 知识点那一层换个颜色：三层里它是「这道题到底在考什么」。 */
  .tag.point {
    background: rgba(130, 200, 255, 0.16);
    color: #9fd4ff;
  }

  .primary {
    padding: 0.7rem 1rem;
    border: 1px solid rgba(130, 200, 255, 0.6);
    border-radius: 0.75rem;
    background: rgba(130, 200, 255, 0.14);
    color: #9fd4ff;
    font: inherit;
    font-size: 0.95rem;
    font-weight: 600;
    cursor: pointer;
    touch-action: manipulation;
    -webkit-tap-highlight-color: transparent;
    --wails-draggable: no-drag;
  }
  .primary:active {
    background: rgba(130, 200, 255, 0.28);
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
