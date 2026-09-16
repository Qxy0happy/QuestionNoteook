<script lang="ts">
  // Agent 页：配 VLM 的端点与凭据，以及每日汇总通知。
  //
  // 为什么这一页是必需的：两份配置都落在应用私有目录
  // （/data/data/<包名>/files/questionbook/ 下的 vlm.json 与 digest.json），
  // 手机上没有任何文件管理器够得着它们。没有这个界面，"识别标签"与"每日提醒"
  // 就只能靠 adb push 一个 json 才能用。
  //
  // 凭据**只出不进**：读回来的只有「设没设」与长度，值本身不进界面、不进日志。
  // （每日提醒那份没有凭据，读回来的就是真值，见 Go 侧 digest.SetConfig 的注释。）

  import { onMount } from 'svelte';

  import * as Agent from '../bindings/questionbook/internal/agent/service';
  import type { Answer } from '../bindings/questionbook/internal/agent/models';
  // 导出那一面是一个**主包**里的薄适配（bundleStager），所以它的绑定落在模块根下、
  // 文件名小写 —— 与 internal/<包>/service.ts 那套不是一个路子。
  import * as Export from '../bindings/questionbook/bundlestager';
  import Markdown from './Markdown.svelte';
  import Pending from './Pending.svelte';
  import * as VLM from '../bindings/questionbook/internal/vlm/service';
  import type { Config, ConfigView } from '../bindings/questionbook/internal/vlm/models';
  import * as Digest from '../bindings/questionbook/internal/digest/service';
  import type {
    Config as DigestConfig,
    ConfigView as DigestConfigView,
    Slot,
  } from '../bindings/questionbook/internal/digest/models';

  let root = $state<HTMLElement | null>(null);
  let view = $state<ConfigView | null>(null);
  let loading = $state(false);
  let saving = $state(false);
  let error = $state('');
  let saved = $state('');

  // ── 每日汇总通知 ──

  let digestView = $state<DigestConfigView | null>(null);
  let digestEnabled = $state(true);
  // 钟点用字符串存：<input type="time"> 的值就是 "HH:MM"，进出都对得上。
  let digestTime = $state('20:00');
  // 下一次该发的那一条：什么时候发、那一天到期几道、发什么字。
  let nextSlot = $state<Slot | null>(null);
  let digestSaving = $state(false);
  let digestError = $state('');
  let digestSaved = $state('');

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

  // ── 导出整库（票 13）──
  //
  // 分两次调用，因为**中间那一跳只能由宿主做**：Go 把包打好在应用私有目录里
  // （Stage 返回落点），再把路径交给宿主的 copyToDownloads —— Go 与 WebView 都碰不到
  // Android 的存储 API（scoped storage 下直接写 /sdcard/Download 会被拒）。
  let exporting = $state(false);
  let exportError = $state('');
  let exportDone = $state('');

  // 宿主的 JS 桥（WailsJSBridge.java，注册名就是 "wails"）。它只在这个 WebView 里存在 ——
  // 桌面上跑（比如 `wails3 dev`）时没有它，所以这里要判一下，别直接炸。
  type HostBridge = { copyToDownloads?: (json: string) => string };

  function hostCopyToDownloads(
    path: string,
    name: string,
  ): { ok: boolean; display?: string; error?: string } {
    const host = (globalThis as { wails?: HostBridge }).wails;
    if (typeof host?.copyToDownloads !== 'function') {
      return { ok: false, error: '这个平台没有宿主桥（导出到「下载」只在安卓上有）' };
    }
    try {
      const raw = host.copyToDownloads(
        JSON.stringify({ path, name, mime: 'application/zip' }),
      );
      return JSON.parse(raw) as { ok: boolean; display?: string; error?: string };
    } catch (err) {
      return { ok: false, error: errorMessage(err) };
    }
  }

  function sizeText(bytes: number): string {
    if (bytes < 1024) return `${bytes} 字节`;
    if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
    return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
  }

  async function exportLibrary() {
    if (exporting) return;
    exporting = true;
    exportError = '';
    exportDone = '';
    try {
      const b = await Export.Stage();
      const host = hostCopyToDownloads(b.Path, b.Name);
      if (!host.ok) {
        // 包**已经打好了**，所以把这件事说清楚 —— 用户能重试，也能自己去私有目录拿
        // （路径就在下面那行）。别让他以为白导了一次。
        exportError = `包已经打好了（${b.Name}），但没能放进「下载」目录：${host.error}`;
        return;
      }
      exportDone = `已导出 ${b.Questions} 道题、${b.Images} 张图（${sizeText(b.Bytes)}），在${host.display}。`;
    } catch (err) {
      exportError = errorMessage(err);
    } finally {
      exporting = false;
    }
  }

  // ── 问 agent（票 11）──
  //
  // agent 读得到一切，但**每一次写都变成一条待批准改动** —— 那是 Go 侧用反射锁与源码
  // 扫描钉死的性质（internal/agent/safety_test.go），不是这里的自觉。所以这里问一句话
  // 最坏的结果是「多出几条待批准」，正式数据一个字节都不会动。
  let askText = $state('');
  let asking = $state(false);
  let answer = $state<Answer | null>(null);
  let askError = $state('');
  // 变了就让待批准清单重读一遍：agent 刚提的新东西得当场出现。
  let pendingKey = $state(0);

  async function askAgent() {
    const q = askText.trim();
    if (!q || asking) return;
    asking = true;
    askError = '';
    try {
      answer = await Agent.Ask(q);
      askText = '';
      // 无论这一轮提没提改动都 +1 —— 重读一次是最省事的「与库对齐」。
      pendingKey += 1;
    } catch (err) {
      askError = errorMessage(err);
    } finally {
      asking = false;
    }
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

  // 四页同时挂载，切到这一页才值得读一次配置文件（两份一起）。
  $effect(() => {
    const el = root;
    if (!el) return;
    const io = new IntersectionObserver((entries) => {
      for (const entry of entries) {
        if (entry.isIntersecting) {
          void load();
          void loadDigest();
        }
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

  // ── 每日汇总通知 ──

  function pad2(n: number): string {
    return String(n).padStart(2, '0');
  }

  // 把宿主会看到的那一刻说成人话：今天/明天/9月18日 + 几点。
  //
  // FireAt 是 Go 那边给出的绝对时刻（带时区偏移的字符串），new Date 解出来的就是同一个
  // 瞬间，再按设备本地时间读几点几分 —— 与 Go 侧拿设备时区算的那一下是两回事，
  // 但落到用户眼里必须是同一个钟点（提醒设在晚上八点，看到的就得是 20:00）。
  function whenText(slot: Slot): string {
    const at = new Date(slot.FireAt);
    const now = new Date();
    const sameDay = (a: Date, b: Date) =>
      a.getFullYear() === b.getFullYear() &&
      a.getMonth() === b.getMonth() &&
      a.getDate() === b.getDate();

    let day: string;
    if (sameDay(at, now)) {
      day = '今天';
    } else if (sameDay(at, new Date(now.getTime() + 24 * 60 * 60 * 1000))) {
      day = '明天';
    } else {
      day = `${at.getMonth() + 1}月${at.getDate()}日`;
    }
    return `${day} ${pad2(at.getHours())}:${pad2(at.getMinutes())}`;
  }

  async function loadDigest() {
    try {
      const v = await Digest.Config();
      digestView = v;
      digestEnabled = v.Enabled;
      digestTime = `${pad2(v.Hour)}:${pad2(v.Minute)}`;
      // 下一次那一条要单独问：它是算出来的，不在设置里。
      nextSlot = await Digest.Next();
      digestError = '';
    } catch (err) {
      // 设置文件坏了（或者库读不出到期数）时走到这里。照实说，别把上一次的值当现值。
      digestError = errorMessage(err);
    }
  }

  async function saveDigest() {
    if (digestSaving) return;
    digestSaving = true;
    digestSaved = '';
    try {
      const [h, m] = digestTime.split(':').map((s) => Number(s));
      // 键名是小写的那三个：Go 侧 Config 带 json tag（enabled / hour / minute），
      // 生成的类型用的就是 tag 名，与 vlm 那边的 base_url 同一个规矩 —— 写 Go 的字段名
      // 编译期就过不去（ConfigView 那类没有 tag 的才用大写字段名）。
      const cfg: DigestConfig = { enabled: digestEnabled, hour: h, minute: m };
      // 整份提交，不是补丁（那边没有凭据要护着，见 Go 侧 digest.SetConfig）。
      await Digest.SetConfig(cfg);
      await loadDigest();
      digestSaved = '已保存';
    } catch (err) {
      // 坏的时刻会被后端当场退回、不落盘，所以这里照实显示，别假装存上了。
      digestError = errorMessage(err);
    } finally {
      digestSaving = false;
    }
  }

  // 重算宿主读的那份排程。
  //
  // 为什么要从**这一页**去动它：能挂的地方只有这里（App.svelte 与 Review.svelte 这一轮
  // 不归这一票改）。四页是同时挂载的，所以下面 mount 那一次等于「应用打开时重算一次」；
  // 而退到后台那一次盖的是「用户临退出去之前刚评过几道题」—— 那些新算出来的到期时刻
  // 只有在这一刻才落进排程里，不重算的话第二天那条通知报的数字就少了几道。
  //
  // 静默失败是有意的：这是后台的保养动作，不是用户按下的按钮，报错只会是噪音。
  // 真正需要用户知道的错（设置存不下）会在保存时显示出来。
  async function refreshSchedule() {
    try {
      await Digest.Refresh();
    } catch {
      // 忽略：排程旧一点，比弹一个用户没法处理的错误好
    }
    // Go 把排程文件写好之后，还得让**宿主**把它变成一个真的闹钟 —— 而 Go 与 Java 之间没有
    // 调用通道（宿主那些能力都是 Go 经 JNI 调的，包装在 Wails 模块里）。所以在这儿喊一声。
    //
    // 顺序要紧：必须**等 Refresh 落盘之后**才喊，否则宿主读到的是上一份（或者还没有）。
    // 也正是在这一次里，宿主才有机会申请通知权限、引导「闹钟和提醒」（那两件事都收敛成
    // 「只在真的排上了一条、且在 Activity 里、且没问过」才问）。
    const host = (globalThis as { wails?: { armDigest?: () => void } }).wails;
    host?.armDigest?.();
  }

  onMount(() => {
    void refreshSchedule();

    // 安卓上「退出应用」多半是退到后台，visibilitychange 是这里唯一抓得到的信号。
    // 用 pagehide 也行，但它在某些 WebView 里不触发；visibilitychange 稳一些。
    const onVisibility = () => {
      if (document.visibilityState === 'hidden') void refreshSchedule();
    };
    document.addEventListener('visibilitychange', onVisibility);
    return () => document.removeEventListener('visibilitychange', onVisibility);
  });
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

      <!-- 两块设置共用一个滚动区：各给一个 .form 会各带一条滚动条，
           手机上滑起来会分不清自己在滚哪一块。 -->
      <hr class="sep" />

      <p class="note">
        每天到点发一条汇总，形如「今天有 5 道待复习」。当天没有到期题就不发，也不会为每道题
        单独发一条。
        <br />
        安卓 13 及以上，第一次发之前系统会问一次通知权限；不给的话这条汇总就出不来。
        {#if digestView?.SchedulePath}<br /><code>{digestView.SchedulePath}</code>{/if}
      </p>

      {#if digestView?.Problem}
        <p class="banner warn">{digestView.Problem}</p>
      {/if}
      <!-- 宿主报回来的状态（通知权限被关、精确闹钟没给、上次没发成…）。
           文案在 Go 侧拼好（HostNote）—— 它要同时看设置与宿主报的那几个布尔量，
           是一段判断而不是一段文案，不该散到前端来。空串就是"没什么好说的"。 -->
      {#if digestView?.HostNote}
        <p class="banner warn">{digestView.HostNote}</p>
      {/if}
      {#if digestError}
        <p class="banner">{digestError}</p>
      {/if}
      {#if digestSaved}
        <p class="banner ok">{digestSaved}</p>
      {/if}

      <label class="row">
        <input type="checkbox" bind:checked={digestEnabled} />
        <span>每天发一条汇总</span>
      </label>

      <label>
        <span>发送时刻<span class="opt">本地时间</span></span>
        <!-- type="time" 唤起的是系统的时间选择器：手机上让用户手敲「20:00」两个数字很难受。 -->
        <input type="time" bind:value={digestTime} disabled={!digestEnabled} />
      </label>

      {#if nextSlot}
        <p class="note">
          {#if nextSlot.Count > 0}
            下一次：{whenText(nextSlot)} —— {nextSlot.Body}。
          {:else}
            下一次：{whenText(nextSlot)} —— 那天没有到期题，不发。
          {/if}
        </p>
      {/if}

      <div class="actions">
        <button class="pill go" onclick={saveDigest} disabled={digestSaving}>
          {digestSaving ? '保存中…' : '保存'}
        </button>
        <button class="pill" onclick={loadDigest} disabled={digestSaving}>重新读取</button>
      </div>

      <hr class="sep" />

      <p class="note">
        把整个错题本打成一个 zip 放进系统的「下载」目录：里面是库与全部题图答案图。
        应用一卸载私有目录就全没了，**这个包是防丢的唯一手段**。
        <br />
        题图是原始像素，包可能不小；导完会在下面告诉你它落在哪。
      </p>

      {#if exportError}
        <p class="banner">{exportError}</p>
      {/if}
      {#if exportDone}
        <p class="banner ok">{exportDone}</p>
      {/if}

      <div class="actions">
        <button class="pill go" onclick={exportLibrary} disabled={exporting}>
          {exporting ? '打包中…' : '导出整库'}
        </button>
      </div>

      <hr class="sep" />

      <p class="note">
        还可以直接问它关于全部错题的问题（比如「我数学哪块最弱」）—— 它会自己去查数据。
        它**改不动**正式数据：想改什么只能提一条待批准改动，等你逐条点头。
      </p>

      {#if askError}
        <p class="banner">{askError}</p>
      {/if}

      <label>
        <span>问一句</span>
        <input
          bind:value={askText}
          type="text"
          autocapitalize="none"
          autocorrect="off"
          spellcheck="false"
          placeholder="我数学哪块最弱"
          onkeydown={(e) => {
            if (e.key === 'Enter') void askAgent();
          }}
        />
      </label>
      <div class="actions">
        <button class="pill go" onclick={askAgent} disabled={asking || !askText.trim()}>
          {asking ? '它在查…' : '问'}
        </button>
      </div>

      {#if answer}
        <div class="answer">
          <!-- 模型吐的是同一种东西（markdown + 公式），所以与讨论那边共用一套渲染。 -->
          {#if answer.Text}
            <Markdown text={answer.Text} />
          {/if}
          {#if answer.Proposals?.length}
            <p class="note">这一轮提了 {answer.Proposals.length} 条待批准改动，在下面。</p>
          {/if}
          {#if answer.Problem}
            <p class="banner warn">{answer.Problem}</p>
          {/if}
        </div>
      {/if}

      <hr class="sep" />

      <!-- 待批准清单（票 11）：agent 提议的改动都在这儿，逐条批准或丢弃。
           批准之前不影响正式数据。 -->
      <Pending refreshKey={pendingKey} />
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

  /* 两块设置之间的分隔线。用 <hr> 而不是再加一个标题，是因为这两块在
     「agent 设置」里是平级的，一个标题反而会让人以为下面那块是子项。 */
  .sep {
    width: 100%;
    margin: 0.15rem 0;
    border: 0;
    border-top: 1px solid rgba(244, 246, 251, 0.1);
  }

  /* 勾选那一行是横着的：checkbox 加一句话。上面那条 label 的竖排规则不能套在它身上。 */
  label.row {
    flex-direction: row;
    align-items: center;
    gap: 0.55rem;
  }

  /* 上面那条 input, select, textarea 的通配规则是给文本框写的（宽度铺满、有边框底色）。
     checkbox 套上去会变成一个占满整行、带方框底的怪东西 —— 这里把它改回系统原生的小方块。
     必须写在通配规则**之后**：同为选择器 `input[type='checkbox']` 权重更高，但顺序一致更保险。 */
  input[type='checkbox'] {
    /* 不许铺满：就是那个小方块本身。1.15rem 是手指点得到的尺寸。 */
    width: 1.15rem;
    height: 1.15rem;
    flex: none;
    margin: 0;
    padding: 0;
    border: 0;
    background: none;
    accent-color: #9fd4ff;
  }

  /* 时间选择器的那个小钟表图标是深色的，深色底上几乎看不见，翻成白的。 */
  input[type='time']::-webkit-calendar-picker-indicator {
    filter: invert(1);
    opacity: 0.55;
  }

  /* agent 的回答：markdown + 公式，样式交给 Markdown 组件，这里只管这一格的外观。 */
  .answer {
    padding: 0.75rem 0.9rem;
    border: 1px solid rgba(244, 246, 251, 0.12);
    border-radius: 0.6rem;
    background: rgba(244, 246, 251, 0.04);
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
