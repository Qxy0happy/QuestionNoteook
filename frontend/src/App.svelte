<script lang="ts">
  import { onMount } from 'svelte';
  // 主包里的薄服务（与 bundleStager 一样），所以绑定落在模块根下、文件名小写。
  import * as DeviceTime from '../bindings/questionbook/devicetime';
  import Capture from './Capture.svelte';
  import Library from './Library.svelte';
  import Review from './Review.svelte';
  import Agent from './Agent.svelte';
  import type { Question } from '../bindings/questionbook/internal/library/models';

  // 主界面四页，横向排列，底部导航栏是第二条入口（横滑照旧能用）。
  // 顺序按**工作流**从左到右：拍下来 → 翻题库 → 复习，设置放最后。
  const PAGES = [
    { id: 'capture', label: '拍照' },
    { id: 'library', label: '题库' },
    { id: 'review', label: '复习' },
    // 这一页是 Agent 那层（票 11）的家：里面两个子标签 —— 与 agent 对话、以及全部设置。
    // 早先它只有设置，所以标签叫过一阵「设置」；票 11 落地之后叫回 Agent 才名实相符。
    { id: 'agent', label: 'Agent' },
  ] as const;

  // 用查出来的下标而不是写死的数字：改上面的顺序时不会有一处悄悄指错页。
  const DEFAULT_PAGE = PAGES.findIndex((p) => p.id === 'review');
  const CAPTURE_PAGE = PAGES.findIndex((p) => p.id === 'capture');

  let scroller: HTMLElement;

  // 非 null 表示取景页正处于「补拍答案图」：值是那道题的 ID。null 就是拍新题。
  let answerFor = $state<number | null>(null);
  // 当前停在哪一页，底部导航据此高亮。
  let currentPage = $state(DEFAULT_PAGE);

  onMount(() => {
    // 直接落位，不用平滑滚动 —— 否则打开时会看到一次横向滑动。
    scroller.scrollLeft = scroller.clientWidth * DEFAULT_PAGE;

    void reportTimeZone();

    // 用户可能在后台那段时间改了时区（或者跨了夏令时），回前台时补报一次。
    const onVisible = () => {
      if (document.visibilityState === 'visible') void reportTimeZone();
    };
    document.addEventListener('visibilitychange', onVisible);
    return () => document.removeEventListener('visibilitychange', onVisible);
  });

  // 把**设备时区**告诉 Go。
  //
  // 必须做：Go 在安卓上拿不到系统时区（没有 /etc/localtime、没有 $TZ、也没有 GOROOT 里的
  // zoneinfo.zip），于是 time.Local 就是 UTC。后果不是「显示差八小时」那么轻 ——
  // 「今天」的边界会整个错一个偏移：复习队列的「今日到期」按 UTC 日切（本地早上 8 点才换日），
  // 每日汇总设的「晚上 8 点」在本地次日凌晨 4 点才响（实测就是这么响的）。
  // WebView 这边知道得清清楚楚，所以由它告诉 Go。见 internal/devtz 的包注释。
  async function reportTimeZone() {
    try {
      const zone = Intl.DateTimeFormat().resolvedOptions().timeZone;
      if (zone) await DeviceTime.Set(zone);
    } catch {
      // 报不上去就算了：Go 那边继续用 time.Local（桌面上是对的，安卓上是 UTC）。
      // 这是一次尽力而为的修正，不该让启动或切回前台因此出问题。
    }
  }

  // 点导航栏：平滑滚过去。滚动本身会触发 onPagesScroll，高亮不用在这儿管。
  function goTo(index: number) {
    scroller.scrollTo({ left: scroller.clientWidth * index, behavior: 'smooth' });
  }

  // 题库页点「补拍答案图」：记下是哪道题，把人送到取景页。
  function startAnswer(q: Question) {
    answerFor = q.ID;
    goTo(CAPTURE_PAGE);
  }

  // 补拍刚交回来的那道题（**更新过的**：answer_hash 已经写进去了）。
  //
  // 它要交给题库页去替换详情页手里那份快照 —— 不交的话详情页还停在补拍前的样子：
  // AnswerHash 仍是空的，于是「重拍/补拍答案图」那个标签没变、**「看答案图」整个不出现**，
  // 看着就像补拍没存上（用户报过两次）。
  //
  // 为什么不靠「切回题库页时重拉列表」那条路：那一条依赖翻页的可见性回调 —— 而这件事
  // 必须在用户回来**之前**就已经是对的，他回到那一页时按钮就该在了。
  let attachedQuestion = $state<Question | null>(null);

  // 补拍收工（用户按了「完成」），回到拍新题。
  function answerAttached(q: Question) {
    answerFor = null;
    attachedQuestion = q;
  }

  let settle: ReturnType<typeof setTimeout> | undefined;

  function onPagesScroll() {
    currentPage = Math.round(scroller.scrollLeft / scroller.clientWidth);
    clearTimeout(settle);
    // 下面这半截**要等停稳**再判，上面那半截不用，因为两件事的性质不同：
    // 高亮跟着手指走才对（滑到一半也该亮起目标页），而补拍态是「用户到底停在哪」的问题 ——
    // startAnswer 走的是平滑滚动，起手那几帧还停在题库页上，那时就判会把刚设上的补拍态立刻抹掉。
    settle = setTimeout(() => {
      // 补拍态只在取景页看得见的时候算数。Capture 只在按「完成」时回调，用户滑走就算取消 ——
      // 不在这儿清掉的话，那个 ID 会一直挂着，几十秒后对着另一道题按下快门，
      // 答案图就静默挂到了之前那道题上。
      if (currentPage !== CAPTURE_PAGE) answerFor = null;
    }, 150);
  }
</script>

<div class="shell">
  <main class="pages" bind:this={scroller} onscroll={onPagesScroll}>
    {#each PAGES as page (page.id)}
      <section
        class="page"
        class:page-capture={page.id === 'capture'}
        data-page={page.id}
        aria-label={page.label}
      >
        {#if page.id === 'capture'}
          <!-- 取景：挂载即开镜。上面没有引导层。 -->
          <Capture {answerFor} onAnswerAttached={answerAttached} />
        {:else if page.id === 'library'}
          <!-- 题库：错题列表 + 点开看题图。切到这一页它自己会重拉一次列表。
               attachedQuestion 是「刚补拍完的那道题」，由它替换详情页手里那份快照。 -->
          <Library onCaptureAnswer={startAnswer} attached={attachedQuestion} />
        {:else if page.id === 'review'}
          <!-- 复习：今日到期的队列 + 四档自评。同样是自己发现被划到可见时才拉队列。 -->
          <Review />
        {:else}
          <!-- 设置：VLM 的端点与凭据。 -->
          <Agent />
        {/if}
      </section>
    {/each}
  </main>

  <nav class="nav" aria-label="主导航">
    {#each PAGES as page, i (page.id)}
      <button
        class="tab"
        class:on={currentPage === i}
        aria-current={currentPage === i ? 'page' : undefined}
        onclick={() => goTo(i)}
      >
        {page.label}
      </button>
    {/each}
  </nav>
</div>

<style>
  .shell {
    display: flex;
    flex-direction: column;
    height: 100dvh;
    overflow: hidden;
  }

  .pages {
    /* 导航栏占掉底下那一条，剩下的都给页面 —— 所以这里是 flex:1 而不是 100dvh。 */
    flex: 1;
    min-height: 0;
    display: flex;
    overflow-x: auto;
    overflow-y: hidden;
    scroll-snap-type: x mandatory;
    /* 不让横滑手势冒泡给外层（安卓 WebView 里尤其重要）。 */
    overscroll-behavior-x: contain;
    scrollbar-width: none;
  }
  .pages::-webkit-scrollbar {
    display: none;
  }

  .page {
    flex: 0 0 100%;
    scroll-snap-align: start;
    /* 一次只能翻过一页。默认行为是「甩多快就滚多远」—— 快速一划能连跳两三页，
       而这一页刚划走、用户还没看清就到了别处。always 让吸附点变成**必须停**的站。 */
    scroll-snap-stop: always;
    display: grid;
    place-items: center;
  }

  /* 取景页整页铺满，不参与居中 —— 网格居中会让视频退到它的固有尺寸。 */
  .page-capture {
    display: block;
  }

  .nav {
    flex: none;
    display: flex;
    border-top: 1px solid rgba(244, 246, 251, 0.1);
    background: rgba(10, 12, 22, 0.96);
    /* 手势条那一条得留出来，否则最下面一排字会被系统手势区盖住。 */
    padding-bottom: env(safe-area-inset-bottom);
  }
  .tab {
    flex: 1;
    /* 定高而不是靠内边距撑：行高一变按钮就跟着变，而底栏是最不该自己改高度的地方。
       3.75rem 是原先那条约 2.5rem 的 1.5 倍。 */
    height: 3.75rem;
    display: flex;
    align-items: center;
    justify-content: center;
    border: 0;
    background: transparent;
    color: rgba(244, 246, 251, 0.5);
    font: inherit;
    font-size: 0.85rem;
    letter-spacing: 0.04em;
    cursor: pointer;
    touch-action: manipulation;
    -webkit-tap-highlight-color: transparent;
    --wails-draggable: no-drag;
  }
  .tab:active {
    background: rgba(244, 246, 251, 0.08);
  }
  .tab.on {
    color: #9fd4ff;
    font-weight: 600;
  }
</style>
