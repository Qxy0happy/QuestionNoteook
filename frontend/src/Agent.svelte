<script lang="ts">
  // Agent 页：目前只有一件事 —— 配 VLM 的端点与凭据。
  //
  // 为什么这一页是必需的：配置落在应用私有目录（/data/data/<包名>/files/questionbook/vlm.json），
  // 手机上没有任何文件管理器够得着它。没有这个界面，"识别标签"就只能靠 adb push 一个 json 才能用。
  //
  // 凭据**只出不进**：读回来的只有「设没设」与长度，值本身不进界面、不进日志。

  import * as VLM from '../bindings/questionbook/internal/vlm/service';
  import type { Config, ConfigView } from '../bindings/questionbook/internal/vlm/models';

  let root = $state<HTMLElement | null>(null);
  let view = $state<ConfigView | null>(null);
  let loading = $state(false);
  let saving = $state(false);
  let error = $state('');
  let saved = $state('');

  // 提交的补丁：**空串表示这一项不动**（Go 侧是补丁语义）。
  // 因为凭据读不回来，整份覆盖会把 key 抹掉，所以只能按项提交。
  let patch = $state<Config>({
    base_url: '',
    api_key: '',
    vision_model: '',
    text_model: '',
    detail: '',
    tag_prompt: '',
  });

  function errorMessage(err: unknown): string {
    return err instanceof Error ? err.message : String(err);
  }

  async function load() {
    loading = true;
    try {
      const v = await VLM.Config();
      view = v;
      // 把能回显的几项填进表单，省得用户为了改一个模型名要重打一遍地址。
      // 凭据不回填 —— 界面里永远没有它的值。
      patch = {
        ...patch,
        base_url: v.BaseURL,
        vision_model: v.VisionModel,
        text_model: v.TextModel,
        detail: v.Detail,
      };
      error = '';
    } catch (err) {
      error = errorMessage(err);
    } finally {
      loading = false;
    }
  }

  // 四页同时挂载，切到这一页才值得读一次配置文件。
  $effect(() => {
    const el = root;
    if (!el) return;
    const io = new IntersectionObserver((entries) => {
      for (const entry of entries) {
        if (entry.isIntersecting) void load();
      }
    });
    io.observe(el);
    return () => io.disconnect();
  });

  async function save() {
    if (saving) return;
    saving = true;
    saved = '';
    try {
      await VLM.SetConfig(patch);
      // 提交完就把凭据从界面上抹掉：它只该存在于那一次提交里。
      patch = { ...patch, api_key: '' };
      await load();
      saved = '已保存';
    } catch (err) {
      // 缺项会被后端当场退回、不落盘，所以这里照实显示，别假装存上了。
      error = errorMessage(err);
    } finally {
      saving = false;
    }
  }
</script>

<div class="agent" bind:this={root}>
  <header class="bar">
    <span class="bar-title">设置</span>
    {#if view}
      <span class="state" class:ok={view.Configured}>
        {view.Configured ? '已配置' : '未配置'}
      </span>
    {/if}
  </header>

  {#if loading && !view}
    <p class="hint">正在读配置…</p>
  {:else}
    <div class="form">
      <p class="note">
        识别标签要用云端模型。配置留在本机，卸载应用会一起没。
        {#if view?.Path}<br /><code>{view.Path}</code>{/if}
      </p>

      {#if view?.Problem}
        <p class="banner warn">{view.Problem}</p>
      {/if}
      {#if error}
        <p class="banner">{error}</p>
      {/if}
      {#if saved}
        <p class="banner ok">{saved}</p>
      {/if}

      <label>
        <span>接口地址</span>
        <input
          bind:value={patch.base_url}
          type="text"
          autocapitalize="none"
          autocorrect="off"
          spellcheck="false"
          placeholder="https://api.deepseek.com"
        />
      </label>

      <label>
        <span>API Key</span>
        <!-- 明文而不是 password：安卓上 password 会唤出**安全键盘**，它不让粘贴、也没有候选词，
             敲一串几十个字符的 key 非常折磨。这一页本来就是应用私有的界面，省事更要紧。 -->
        <input
          bind:value={patch.api_key}
          type="text"
          autocomplete="off"
          autocapitalize="none"
          autocorrect="off"
          spellcheck="false"
          placeholder={view?.APIKeySet
            ? `已设置（${view.APIKeyLength} 字符），留空表示不改`
            : '还没设'}
        />
      </label>

      <label>
        <span>视觉模型</span>
        <input
          bind:value={patch.vision_model}
          type="text"
          autocapitalize="none"
          autocorrect="off"
          spellcheck="false"
          placeholder="看服务方的文档，别照抄这里"
        />
      </label>

      <label>
        <span>文本模型<span class="opt">选填</span></span>
        <input
          bind:value={patch.text_model}
          type="text"
          autocapitalize="none"
          autocorrect="off"
          spellcheck="false"
          placeholder="只有纯文本的活才用它"
        />
      </label>

      <label>
        <span>图片保真<span class="opt">密集文档要用高保真</span></span>
        <select bind:value={patch.detail}>
          <option value="">默认</option>
          <option value="low">low —— 降到 512×512</option>
          <option value="high">high</option>
          <option value="original">original —— 保留原尺寸</option>
        </select>
      </label>

      <label>
        <span>打标签的提示词<span class="opt">选填</span></span>
        <textarea
          bind:value={patch.tag_prompt}
          rows="4"
          placeholder="留空就用内置那份"
        ></textarea>
      </label>

      <div class="actions">
        <button class="pill go" onclick={save} disabled={saving}>
          {saving ? '保存中…' : '保存'}
        </button>
        <button class="pill" onclick={load} disabled={saving || loading}>重新读取</button>
      </div>
    </div>
  {/if}
</div>

<style>
  .agent {
    width: 100%;
    height: 100%;
    display: flex;
    flex-direction: column;
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
  .state {
    font-size: 0.85rem;
    color: rgba(255, 200, 130, 0.85);
  }
  .state.ok {
    color: rgba(150, 230, 180, 0.85);
  }

  .form {
    flex: 1;
    min-height: 0;
    overflow-y: auto;
    display: flex;
    flex-direction: column;
    gap: 0.85rem;
    padding: 1rem;
  }

  .note {
    margin: 0;
    font-size: 0.85rem;
    line-height: 1.5;
    color: rgba(244, 246, 251, 0.55);
    /* 路径很长，得能在任意处断开，否则会把整页撑宽。 */
    overflow-wrap: anywhere;
  }
  .note code {
    font-size: 0.8rem;
    color: rgba(244, 246, 251, 0.4);
  }

  label {
    display: flex;
    flex-direction: column;
    gap: 0.35rem;
  }
  label > span {
    display: flex;
    align-items: baseline;
    gap: 0.45rem;
    font-size: 0.85rem;
    color: rgba(244, 246, 251, 0.7);
  }
  .opt {
    font-size: 0.75rem;
    color: rgba(244, 246, 251, 0.35);
  }

  input,
  select,
  textarea {
    width: 100%;
    padding: 0.5rem 0.7rem;
    border: 1px solid rgba(244, 246, 251, 0.24);
    border-radius: 0.5rem;
    background: rgba(244, 246, 251, 0.06);
    color: #f4f6fb;
    font: inherit;
    font-size: 0.9rem;
    --wails-draggable: no-drag;
  }
  /* 地址、模型名、key 都是机器读的串，别让键盘顺手改。三个属性写在每个 input 上
     （HTML 属性没法在这儿统一给）—— 尤其 autocapitalize：安卓默认会把首字母大写，
     那会直接把 https:// 敲成 Https://。 */
  input::placeholder,
  textarea::placeholder {
    color: rgba(244, 246, 251, 0.3);
  }
  textarea {
    resize: vertical;
  }
  select {
    /* 深色底上系统默认的下拉箭头几乎看不见，自己画一个。 */
    appearance: none;
    padding-right: 2rem;
    background-image: linear-gradient(45deg, transparent 50%, rgba(244, 246, 251, 0.6) 50%),
      linear-gradient(135deg, rgba(244, 246, 251, 0.6) 50%, transparent 50%);
    background-position: right 1rem center, right 0.7rem center;
    background-size: 0.35rem 0.35rem, 0.35rem 0.35rem;
    background-repeat: no-repeat;
  }

  .actions {
    display: flex;
    gap: 0.5rem;
    padding-top: 0.25rem;
  }
  .pill {
    flex: none;
    padding: 0.55rem 1.1rem;
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
  .pill:active {
    background: rgba(244, 246, 251, 0.16);
  }
  .pill:disabled {
    opacity: 0.5;
  }
  .pill.go {
    border-color: rgba(130, 200, 255, 0.6);
    color: #9fd4ff;
  }

  .banner {
    margin: 0;
    padding: 0.6rem 0.8rem;
    border-radius: 0.5rem;
    background: rgba(255, 180, 180, 0.12);
    color: #ffb4b4;
    font-size: 0.85rem;
    line-height: 1.5;
    overflow-wrap: anywhere;
  }
  .banner.ok {
    background: rgba(150, 230, 180, 0.12);
    color: #96e6b4;
  }
  .banner.warn {
    background: rgba(255, 200, 130, 0.12);
    color: rgba(255, 200, 130, 0.95);
  }

  .hint {
    margin: 1.5rem 0 0;
    font-size: 0.9rem;
    text-align: center;
    color: rgba(244, 246, 251, 0.55);
  }
</style>
