<script lang="ts">
  // 标签筛选：把三层标签树摊开，勾选任意层级，向上抛出选中的 id 集合。
  //
  // 取数不在这里 —— 标签由外层从 Go 侧拿好传进来（前端是薄视图，spec 的测试缝只在 Go 侧）。
  // 这是个纯视图：给它标签与当前选中项，它把变化喊回去。
  //
  // 接法（在 Library.svelte 里）：
  //   <TagFilter tags={allTags} selected={selectedTagIDs} onSelect={onPick} />
  // allTags 来自 Tags.List()，selectedTagIDs 是你自己 $state 的数组，
  // onPick 里把新数组存回去、再按它刷新列表（Tags.QuestionsByTags(ids)）。
  //
  // 同一个组件也是「给一道错题打标签」的勾选面板：那边把 selected 换成那道题当前挂的标签，
  // 在 onSelect 里存一次 Tags.SetQuestionTags(id, ids) 即可 —— 两边要的是同一样东西。

  // 只用到标签的这四个字段，所以不从生成的 bindings 里 import 类型：组件是纯视图，
  // 形状对得上就够，接线的传 tags.Tag[] 进来也照样通过（字段名与 Go 侧一一对应）。
  type TagLike = { ID: number; ParentID: number; Name: string; Level: number };

  interface Props {
    /** 全量标签（平铺，来自 Tags.List()）。组件按 ParentID 自己拼树。 */
    tags: TagLike[];
    /** 当前选中的标签 id。空数组 = 不筛。 */
    selected?: number[];
    /** 选中项变化时回调。传的是**新的**整份 id 数组，不是增量。 */
    onSelect?: (ids: number[]) => void;
  }

  let { tags, selected = [], onSelect }: Props = $props();

  // 按父节点分组，一次遍历。Go 侧顶层学科的 ParentID 是 0，所以 0 就是根。
  const children = $derived.by(() => {
    const map = new Map<number, TagLike[]>();
    for (const tag of tags) {
      const siblings = map.get(tag.ParentID);
      if (siblings) siblings.push(tag);
      else map.set(tag.ParentID, [tag]);
    }
    return map;
  });
  const roots = $derived(children.get(0) ?? []);
  const picked = $derived(new Set(selected));

  function collect(tag: TagLike, out: number[] = []): number[] {
    out.push(tag.ID);
    for (const child of children.get(tag.ID) ?? []) collect(child, out);
    return out;
  }

  // 勾一个节点 = 连它整棵子树一起勾上（去勾则整棵去掉）。
  // Go 侧筛「数学」时本来就会带上它下面的章节与知识点（见 store.go 的
  // ListQuestionsTaggedWith），界面把这件事显示出来，看到的选中集合才与筛出来的题对得上。
  function toggle(tag: TagLike) {
    const next = new Set(selected);
    const on = !next.has(tag.ID);
    for (const id of collect(tag)) {
      if (on) next.add(id);
      else next.delete(id);
    }
    onSelect?.([...next]);
  }

  // 这棵子树里有没有被勾中的（含自己）。
  function anyPicked(tag: TagLike): boolean {
    if (picked.has(tag.ID)) return true;
    return (children.get(tag.ID) ?? []).some(anyPicked);
  }

  // 自己没勾、子树里有勾中的：画成「部分选中」，免得用户以为下面那些没生效。
  function partial(tag: TagLike): boolean {
    return !picked.has(tag.ID) && anyPicked(tag);
  }

  // 原生 checkbox 的半选态只能靠属性设、模板里写不了，所以用个 action。
  function indeterminate(node: HTMLInputElement, value: boolean) {
    node.indeterminate = value;
    return {
      update(v: boolean) {
        node.indeterminate = v;
      },
    };
  }

  function clear() {
    onSelect?.([]);
  }
</script>

{#snippet row(tag: TagLike)}
  <label class="row">
    <input
      type="checkbox"
      checked={picked.has(tag.ID)}
      use:indeterminate={partial(tag)}
      onchange={() => toggle(tag)}
    />
    <span class="name">{tag.Name}</span>
  </label>
{/snippet}

<section class="tag-filter">
  <header class="head">
    <span class="title">按标签筛</span>
    {#if selected.length > 0}
      <button class="clear" onclick={clear}>清空</button>
    {/if}
  </header>

  {#if tags.length === 0}
    <p class="hint">还没有标签。给错题打标签时会建出来。</p>
  {:else}
    <ul class="tree">
      {#each roots as subject (subject.ID)}
        <li class="lv1">
          {@render row(subject)}
          <ul class="kids">
            {#each children.get(subject.ID) ?? [] as chapter (chapter.ID)}
              <li class="lv2">
                {@render row(chapter)}
                <ul class="kids">
                  {#each children.get(chapter.ID) ?? [] as point (point.ID)}
                    <li class="lv3">{@render row(point)}</li>
                  {/each}
                </ul>
              </li>
            {/each}
          </ul>
        </li>
      {/each}
    </ul>
  {/if}
</section>

<style>
  .tag-filter {
    display: flex;
    flex-direction: column;
    min-height: 0;
    gap: 0.4rem;
  }

  .head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.5rem;
  }
  .title {
    font-size: 0.85rem;
    font-weight: 600;
    letter-spacing: 0.04em;
    color: rgba(244, 246, 251, 0.6);
  }
  .clear {
    padding: 0.25rem 0.7rem;
    border: 1px solid rgba(244, 246, 251, 0.24);
    border-radius: 999px;
    background: transparent;
    color: #f4f6fb;
    font: inherit;
    font-size: 0.8rem;
    cursor: pointer;
    touch-action: manipulation;
    -webkit-tap-highlight-color: transparent;
    --wails-draggable: no-drag;
  }
  .clear:active {
    background: rgba(244, 246, 251, 0.16);
  }

  .tree {
    margin: 0;
    padding: 0;
    list-style: none;
    /* 词表长了自己滚，滑到头不把整页横滑带走。 */
    max-height: 40vh;
    overflow-y: auto;
    overscroll-behavior: contain;
  }
  .kids {
    margin: 0;
    padding: 0;
    list-style: none;
  }
  /* 三层各缩进一档。层级不画线也不加符号：缩进本身就说明了一切。 */
  .lv2,
  .lv3 {
    padding-left: 0.9rem;
  }

  .row {
    display: flex;
    align-items: center;
    gap: 0.45rem;
    padding: 0.3rem 0.2rem;
    border-radius: 0.4rem;
    cursor: pointer;
    touch-action: manipulation;
    -webkit-tap-highlight-color: transparent;
    --wails-draggable: no-drag;
  }
  .row:active {
    background: rgba(244, 246, 251, 0.1);
  }
  .row input {
    flex: none;
    width: 1rem;
    height: 1rem;
    margin: 0;
    accent-color: #7aa2ff;
  }
  .name {
    font-size: 0.9rem;
    line-height: 1.3;
    color: rgba(244, 246, 251, 0.85);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  /* 顶层学科比下面两层显眼一点：它决定筛的是哪一门。 */
  .lv1 > .row .name {
    font-weight: 600;
    color: #f4f6fb;
  }

  .hint {
    margin: 0.3rem 0 0;
    font-size: 0.85rem;
    color: rgba(244, 246, 251, 0.5);
  }
</style>
