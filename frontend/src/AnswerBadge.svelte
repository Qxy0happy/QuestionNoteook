<script lang="ts">
  // 「有答案图 / 缺答案图」那一小条标记。错题列表与复习界面都用它 ——
  // 样式跟着组件走，免得两处各写一遍那个琥珀色。
  //
  // 两种用法：
  //   <AnswerBadge q={q} />                        两种状态都显示
  //   {#if !hasAnswer(q)}<AnswerBadge q={q} />{/if} 只在缺的时候显示（判断从 answer.ts 来）
  import type { Question } from '../bindings/questionbook/internal/library/models.js';
  import { hasAnswer } from './answer.js';

  interface Props {
    q: Question | null | undefined;
  }

  let { q }: Props = $props();

  const ok = $derived(hasAnswer(q));
</script>

<span class="badge" class:missing={!ok}>{ok ? '有答案图' : '缺答案图'}</span>

<style>
  .badge {
    /* 它在列表行里是个不参与伸缩的那一格。 */
    flex: none;
    font-size: 0.8rem;
    color: rgba(244, 246, 251, 0.55);
  }
  /* 缺答案的题标记出来，这样知道该去补（story 10）。 */
  .badge.missing {
    color: rgba(255, 200, 130, 0.8);
  }
</style>
