<script lang="ts">
  import { onMount } from 'svelte';
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
    // 这一页现在是「设置」，但它同时也是 Agent 那层（待批准、Agent 管理数据）的家，
    // 见票 11。所以 id 留着 agent，标签先叫设置 —— 免得给用户看一个他还用不上的名字。
    { id: 'agent', label: '设置' },
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
  });

  // 点导航栏：平滑滚过去。滚动本身会触发 onPagesScroll，高亮不用在这儿管。
  function goTo(index: number) {
    scroller.scrollTo({ left: scroller.clientWidth * index, behavior: 'smooth' });
  }

  // 题库页点「补拍答案图」：记下是哪道题，把人送到取景页。
  function startAnswer(q: Question) {
    answerFor = q.ID;
    goTo(CAPTURE_PAGE);
  }

  // 补拍收工（用户按了「完成」），回到拍新题。
  function answerAttached() {
    answerFor = null;
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
          <!-- 题库：错题列表 + 点开看题图。切到这一页它自己会重拉一次列表。 -->
          <Library onCaptureAnswer={startAnswer} />
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
    padding: 0.7rem 0.25rem;
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
