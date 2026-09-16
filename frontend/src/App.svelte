<script lang="ts">
  import { onMount } from 'svelte';

  // 主界面三页，横向排列。拍照居中且是默认页 —— 打开 app 看到的第一个画面就是取景。
  const PAGES = [
    { id: 'agent', label: 'Agent' },
    { id: 'capture', label: '拍照' },
    { id: 'library', label: '题库' },
  ] as const;
  const DEFAULT_PAGE = 1;

  let scroller: HTMLElement;

  onMount(() => {
    // 直接落位，不用平滑滚动 —— 否则打开时会看到一次横向滑动。
    scroller.scrollLeft = scroller.clientWidth * DEFAULT_PAGE;
  });
</script>

<main class="pages" bind:this={scroller}>
  {#each PAGES as page (page.id)}
    <section class="page" data-page={page.id} aria-label={page.label}>
      <span class="page-label">{page.label}</span>
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

  .page-label {
    font-size: 1.5rem;
    font-weight: 700;
    letter-spacing: 0.1em;
    color: rgba(244, 246, 251, 0.35);
  }
</style>
