<script lang="ts">
  // 打标签：VLM 给了这些标签，你改完再保存。
  //
  // 跟 TagFilter 一样是个**纯视图**：取数不在这里（前端是薄视图，spec 的测试缝只在 Go 侧）。
  // 外层这样接 ——
  //
  //   let suggestion = $state<TagSuggestion | null>(null);   // VLM.SuggestTags(id) 的返回
  //   let tagging = $state(false);
  //   let tagError = $state('');
  //
  //   async function runTagging() {
  //     tagging = true;
  //     try { suggestion = await VLM.SuggestTags(id); tagError = ''; }
  //     catch (err) { tagError = String(err); }
  //     finally { tagging = false; }
  //   }
  //
  //   <Tagging
  //     suggestion={suggestion?.Tags ?? null}
  //     busy={tagging}
  //     error={tagError}
  //     problem={suggestion?.Problem}
  //     raw={suggestion?.Raw}
  //     vocabulary={allTags}
  //     onSave={(tags) => save(tags)}
  //     onRetry={runTagging}
  //     onCancel={() => (showTagging = false)}
  //   />
  //
  // 保存之前什么都不生效：VLM 给的只是建议，用户改完点了「保存」才落库（票据的硬要求）。
  // 「保存」交回去的是**整份**改完的结果，由外层调 VLM.SaveTags(id, tags) ——
  // 与手工打标签那条路一样，不在这里维护增量。

  // 只用到这三个字段，所以不从生成的 bindings 里 import 类型：
  // 组件是纯视图，形状对得上就够（字段名与 Go 侧的 vlm.ProposedTag 一一对应）。
  type ProposedTag = { Subject: string; Chapter: string; Point: string };
  type TagLike = { ID: number; ParentID: number; Name: string; Level: number };

  interface Props {
    /** VLM 给的建议。null = 还没拿到。 */
    suggestion?: ProposedTag[] | null;
    /** 正在识别。 */
    busy?: boolean;
    /** 这次调用失败了（没配置、断网、模型报错）—— 一句话原样显示。 */
    error?: string;
    /** 调用成功但结果不能用（模型没按格式回、或者一条都没给）。 */
    problem?: string;
    /** 模型的原话。problem 非空时显示出来 —— 那是判断「它到底说了什么」的唯一线索。 */
    raw?: string;
    /** 全量标签词表（平铺，来自 Tags.List()）：给输入框当候选，并标出哪条会新建标签。 */
    vocabulary?: TagLike[];
    /** 保存：交回**整份**改完的建议。 */
    onSave?: (tags: ProposedTag[]) => void;
    /** 重新识别一次（识别是只读的，随便重试）。 */
    onRetry?: () => void;
    /** 收起这块。 */
    onCancel?: () => void;
  }

  let {
    suggestion = null,
    busy = false,
    error = '',
    problem = '',
    raw = '',
    vocabulary = [],
    onSave,
    onRetry,
    onCancel,
  }: Props = $props();

  // 编辑中的这一份。它是**本地状态**而不是 suggestion 的镜像：
  // 用户改了半天又没保存，改的东西该丢，不该留在界面上冒充已生效。
  let rows = $state<ProposedTag[]>([]);
  let seeded: ProposedTag[] | null = null;

  // 新建议到手就重铺一次。比的是数组的**引用**：外层每识别一次都会给一个新数组，
  // 而同一次识别引起的重复渲染给的是同一个引用，用户的编辑因此不会被冲掉。
  $effect(() => {
    if (suggestion === seeded) return;
    seeded = suggestion;
    rows = (suggestion ?? []).map((t) => ({ ...t }));
  });

  // 词表按「父 → 名字」索引。两处用它：输入框的候选，以及「这条会不会新建标签」。
  // 按父索引而不是只按名字：不同学科下同名是合法的（两条不同的知识点）。
  const byParent = $derived.by(() => {
    const m = new Map<number, Map<string, number>>();
    for (const t of vocabulary) {
      let kids = m.get(t.ParentID);
      if (!kids) m.set(t.ParentID, (kids = new Map()));
      kids.set(t.Name, t.ID);
    }
    return m;
  });

  const subjects = $derived([...(byParent.get(0)?.keys() ?? [])]);

  function optionsFor(parentID: number | undefined): string[] {
    if (parentID === undefined) return [];
    return [...(byParent.get(parentID)?.keys() ?? [])];
  }

  /** 词表里 (父, 名字) 对应的标签 id；没有就是 undefined。 */
  function childId(parentID: number, name: string): number | undefined {
    const n = name.trim();
    return n ? byParent.get(parentID)?.get(n) : undefined;
  }

  /** 这一条里有没有词表里还不存在的层 —— 有的话保存时会现建。 */
  function isFresh(row: ProposedTag): boolean {
    const subjectID = childId(0, row.Subject);
    if (subjectID === undefined) return true;
    const chapterID = childId(subjectID, row.Chapter);
    if (row.Chapter.trim() === '') return false;
    if (chapterID === undefined) return true;
    if (row.Point.trim() === '') return false;
    return childId(chapterID, row.Point) === undefined;
  }

  // 学科空着的那条保存时会被 Go 侧整条丢掉（没学科就不知道往哪棵树上挂），
  // 所以这里就不把它算进「能保存」的条数里。
  const usable = $derived(rows.filter((r) => r.Subject.trim() !== ''));

  function addRow() {
    rows = [...rows, { Subject: '', Chapter: '', Point: '' }];
  }

  function removeRow(i: number) {
    rows = rows.filter((_, k) => k !== i);
  }

  function save() {
    onSave?.(
      usable.map((r) => ({
        Subject: r.Subject.trim(),
        Chapter: r.Chapter.trim(),
        Point: r.Point.trim(),
      })),
    );
  }
</script>

<section class="tagging">
  <header class="head">
    <span class="title">VLM 给的标签</span>
    {#if onCancel}
      <button class="ghost" onclick={() => onCancel?.()}>收起</button>
    {/if}
  </header>

  {#if error}
    <p class="banner">{error}</p>
    {#if onRetry}
      <button class="ghost wide" onclick={() => onRetry?.()} disabled={busy}>再试一次</button>
    {/if}
  {:else if busy}
    <p class="hint">正在识别这道题…</p>
  {:else if problem}
    <!-- 调用是成功的，只是结果不能用。把模型的原话摆出来：调提示词全靠它。 -->
    <p class="banner">{problem}</p>
    {#if raw}<pre class="raw">{raw}</pre>{/if}
    {#if onRetry}
      <button class="ghost wide" onclick={() => onRetry?.()} disabled={busy}>再试一次</button>
    {/if}
  {:else if suggestion === null}
    <p class="hint">还没有识别结果。点一下「让 VLM 打标签」就会给出建议。</p>
  {:else if rows.length === 0}
    <p class="hint">这里空了。可以「加一条」自己写，或者重新识别一次。</p>
  {/if}

  {#if rows.length > 0 && !busy && !problem && !error}
    <ul class="rows">
      {#each rows as _, i (i)}
        {@const subjectID = childId(0, rows[i].Subject)}
        <li class="row">
          <div class="fields">
            <label class="field">
              <span class="lv">学科</span>
              <input list={`subjects-${i}`} bind:value={rows[i].Subject} placeholder="数学" />
            </label>
            <label class="field">
              <span class="lv">章节</span>
              <input list={`chapters-${i}`} bind:value={rows[i].Chapter} placeholder="高等数学" />
            </label>
            <label class="field">
              <span class="lv">知识点</span>
              <input list={`points-${i}`} bind:value={rows[i].Point} placeholder="中值定理" />
            </label>
          </div>

          <div class="row-foot">
            {#if isFresh(rows[i])}
              <span class="badge">保存时会新建</span>
            {/if}
            <button class="ghost small" onclick={() => removeRow(i)}>删掉这条</button>
          </div>

          <!-- 候选来自用户自己的词表：照着已有的名字填，词表才不会越长越散（用户故事 17）。
               候选只是建议，输入框收任何名字 —— 顶层学科由用户自由填写（用户故事 14）。 -->
          <datalist id={`subjects-${i}`}>
            {#each subjects as name}<option value={name}></option>{/each}
          </datalist>
          <datalist id={`chapters-${i}`}>
            {#each optionsFor(subjectID) as name}<option value={name}></option>{/each}
          </datalist>
          <datalist id={`points-${i}`}>
            {#each optionsFor(subjectID === undefined ? undefined : childId(subjectID, rows[i].Chapter)) as name}
              <option value={name}></option>
            {/each}
          </datalist>
        </li>
      {/each}
    </ul>

    <div class="foot">
      <button class="ghost" onclick={addRow}>加一条</button>
      <button class="ghost primary" onclick={save} disabled={usable.length === 0}>
        保存{usable.length > 0 ? ` ${usable.length} 条` : ''}
      </button>
    </div>
    {#if usable.length === 0}
      <p class="hint">至少给一条填上学科。</p>
    {/if}
  {/if}
</section>

<style>
  .tagging {
    display: flex;
    flex-direction: column;
    gap: 0.6rem;
    min-height: 0;
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

  .rows {
    margin: 0;
    padding: 0;
    list-style: none;
    display: flex;
    flex-direction: column;
    gap: 0.6rem;
    /* 建议条数不多（上限 3 条），但手机上键盘一起来就没地方了，让它自己滚。 */
    max-height: 46vh;
    overflow-y: auto;
    overscroll-behavior: contain;
  }

  .row {
    padding: 0.6rem;
    border: 1px solid rgba(244, 246, 251, 0.12);
    border-radius: 0.75rem;
    background: rgba(244, 246, 251, 0.06);
  }

  /* 三层横排：手机上窄，让它自己挤，不换行的层级比换行好读。 */
  .fields {
    display: flex;
    gap: 0.4rem;
  }
  .field {
    flex: 1;
    min-width: 0;
    display: flex;
    flex-direction: column;
    gap: 0.2rem;
  }
  .lv {
    font-size: 0.7rem;
    color: rgba(244, 246, 251, 0.45);
  }
  .field input {
    width: 100%;
    min-width: 0;
    padding: 0.35rem 0.6rem;
    border: 1px solid rgba(244, 246, 251, 0.24);
    border-radius: 0.5rem;
    background: rgba(6, 7, 15, 0.5);
    color: #f4f6fb;
    font: inherit;
    font-size: 0.9rem;
  }
  .field input::placeholder {
    color: rgba(244, 246, 251, 0.3);
  }

  .row-foot {
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: 0.5rem;
    margin-top: 0.5rem;
  }
  .badge {
    margin-right: auto;
    font-size: 0.7rem;
    padding: 0.1rem 0.45rem;
    border-radius: 999px;
    border: 1px solid rgba(130, 200, 255, 0.4);
    color: #9fd4ff;
  }

  .foot {
    display: flex;
    gap: 0.5rem;
    justify-content: flex-end;
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
  .ghost.small {
    padding: 0.2rem 0.6rem;
    font-size: 0.8rem;
  }
  .ghost.wide {
    align-self: flex-start;
  }
  /* 主按钮：保存是这块界面里唯一会写库的动作，让它看得出跟别的不一样。 */
  .ghost.primary {
    border-color: rgba(130, 200, 255, 0.6);
    color: #9fd4ff;
  }

  .hint {
    margin: 0;
    font-size: 0.85rem;
    color: rgba(244, 246, 251, 0.5);
  }

  .banner {
    margin: 0;
    padding: 0.5rem 0.75rem;
    border-radius: 0.5rem;
    background: rgba(255, 180, 180, 0.12);
    color: #ffb4b4;
    font-size: 0.85rem;
  }

  /* 模型的原话：原样摆出来，不折行排版也不高亮 —— 它就是要拿来读的。 */
  .raw {
    margin: 0;
    padding: 0.5rem 0.75rem;
    max-height: 8rem;
    overflow: auto;
    border-radius: 0.5rem;
    background: rgba(6, 7, 15, 0.5);
    color: rgba(244, 246, 251, 0.7);
    font-size: 0.8rem;
    white-space: pre-wrap;
    word-break: break-word;
  }
</style>
