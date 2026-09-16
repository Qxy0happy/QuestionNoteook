<script lang="ts">
  import { onMount } from 'svelte';
  import Capture from './Capture.svelte';
  import Library from './Library.svelte';
  import Review from './Review.svelte';
  import Agent from './Agent.svelte';
  import type { Question } from '../bindings/questionbook/internal/library/models';

  // 主界面四页，横向排列。拍照在第二位且是默认页 —— 打开 app 看到的第一个画面就是取景；
  // 往左滑是 Agent，往右依次是题库与复习。
  const PAGES = [
    { id: 'agent', label: 'Agent' },
    { id: 'capture', label: '拍照' },
    { id: 'library', label: '题库' },
    { id: 'review', label: '复习' },
  ] as const;
  const DEFAULT_PAGE = 1;
  const CAPTURE_PAGE = 1;

  let scroller: HTMLElement;

  // 非 null 表示取景页正处于「补拍答案图」：值是那道题的 ID。null 就是拍新题。
  let answerFor = $state<number | null>(null);

  onMount(() => {
    // 直接落位，不用平滑滚动 —— 否则打开时会看到一次横向滑动。
    scroller.scrollLeft = scroller.clientWidth * DEFAULT_PAGE;
  });

  // 题库页点「补拍答案图」：记下是哪道题，把人送到取景页。
  function startAnswer(q: Question) {
    answerFor = q.ID;
    scroller.scrollTo({ left: scroller.clientWidth * CAPTURE_PAGE, behavior: 'smooth' });
  }

  // 补拍收工（用户按了「完成」），回到拍新题。
  function answerAttached() {
    answerFor = null;
  }

  // 补拍态只在取景页看得见的时候算数。
  //
  // Capture 只在按「完成」时回调，用户滑走就算取消 —— 不在这儿清掉的话，那个 ID 会一直挂着，
  // 几十秒后对着另一道题按下快门，答案图就静默挂到了之前那道题上。
  let settle: ReturnType<typeof setTimeout> | undefined;

  function onPagesScroll() {
    clearTimeout(settle);
    // 停稳了再判：startAnswer 是平滑滚动，起手那几帧还在题库页上，边滚边判会把刚设上的补拍态立刻抹掉。
    settle = setTimeout(() => {
      if (Math.round(scroller.scrollLeft / scroller.clientWidth) !== CAPTURE_PAGE) answerFor = null;
    }, 150);
  }
</script>

<main class="pages" bind:this={scroller} onscroll={onPagesScroll}>
  {#each PAGES as page (page.id)}
    <section class="page" class:page-capture={page.id === 'capture'} data-page={page.id} aria-label={page.label}>
      {#if page.id === 'capture'}
        <!-- 中间这页是取景，挂载即开镜。上面没有引导层，打开 app 直接就在取景。 -->
        <Capture {answerFor} onAnswerAttached={answerAttached} />
      {:else if page.id === 'library'}
        <!-- 题库：错题列表 + 点开看题图。切到这一页它自己会重拉一次列表。 -->
        <Library onCaptureAnswer={startAnswer} />
      {:else if page.id === 'review'}
        <!-- 复习：今日到期的队列 + 四档自评。同样是自己发现被划到可见时才拉队列。 -->
        <Review />
      {:else}
        <!-- Agent：目前就一件事 —— 配 VLM 的端点与凭据。 -->
        <Agent />
      {/if}
    </section>
  {/each}
</main>

<style>
  .pages {
    display: flex;
    height: 100dvh;
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
</style>
