<script lang="ts">
  // Agent 页：上面一排子标签 ——「对话」与「设置」，默认停在对话。
  //
  // 「对话」是与 agent 聊天（整库范围的问题），「设置」是三块配置：VLM 的端点与凭据、
  // 每日汇总通知、导出整库。为什么这么分：聊天是天天要做的事，配置是一次性的。
  // 原先两者挤在同一张滚动表单里，一次性的东西占着最显眼的位置，聊天反倒缩在页面最下面
  // （用户 2026-09-16：「这个我建议做成一个子标签页，现在占据一块的模式，一来不好管理，
  // 二来看的也难受」）。
  //
  // 为什么这一页是必需的：两份配置都落在应用私有目录
  // （/data/data/<包名>/files/questionbook/ 下的 vlm.json 与 digest.json），
  // 手机上没有任何文件管理器够得着它们。没有这个界面，"识别标签"与"每日提醒"
  // 就只能靠 adb push 一个 json 才能用。
  //
  // 凭据**只出不进**：读回来的只有「设没设」与长度，值本身不进界面、不进日志。
  // （每日提醒那份没有凭据，读回来的就是真值，见 Go 侧 digest.SetConfig 的注释。）

  import { onMount } from 'svelte';

  // 导出那一面是一个**主包**里的薄适配（bundleStager），所以它的绑定落在模块根下、
  // 文件名小写 —— 与 internal/<包>/service.ts 那套不是一个路子。
  import * as Export from '../bindings/questionbook/bundlestager';
  import AgentChat from './AgentChat.svelte';
  import * as VLM from '../bindings/questionbook/internal/vlm/service';
  import type { Config, ConfigView } from '../bindings/questionbook/internal/vlm/models';
  import * as Digest from '../bindings/questionbook/internal/digest/service';
  import type {
    Config as DigestConfig,
    ConfigView as DigestConfigView,
    Slot,
  } from '../bindings/questionbook/internal/digest/models';
  import * as Review from '../bindings/questionbook/internal/review/service';
  import type {
    Config as ReviewConfig,
    ConfigView as ReviewConfigView,
    RatingInterval,
  } from '../bindings/questionbook/internal/review/models';
  // 四档的名字与复习页共用一份（设置页这里要说「四档各推到多少天之后」）。
  import { ratingLabel } from './ratings';

  // 默认停在「对话」：问 agent 是常用的那件事，配模型不是。
  let tab = $state<'chat' | 'settings'>('chat');

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

  // ── 复习参数（票 18）──
  //
  // 只有两个旋钮：间隔模糊、考试日期。**没有**「最大间隔」那一栏 —— 上限是从考试日期
  // 推出来的（ADR-0008），输入框越多，用户越不知道该填哪个。
  let reviewView = $state<ReviewConfigView | null>(null);
  // 表单里**还没保存**那份算出来的视图。它是「改一下就能看见间隔怎么变」的全部实现。
  let reviewPreview = $state<ReviewConfigView | null>(null);
  let reviewFuzz = $state(false);
  // 期望保留率。滑块绑的是**数字**（Svelte 对 type="range" 会转成 number）。
  let reviewRetention = $state(0.95);
  let reviewExamDate = $state(''); // "YYYY-MM-DD"，空 = 不设
  let reviewSaving = $state(false);
  let reviewError = $state('');
  let reviewSaved = $state('');

  // 显示用哪一份：改了还没保存时按表单那份算出来的，否则按已保存那份。
  // 分开存是因为 `reviewView` 还带着「设置文件坏了」那句 Problem，而预览那份没有。
  const reviewShown = $derived(reviewPreview ?? reviewView);

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

  // ── 常用端点 ──
  //
  // 现在只有一条（点一下就把它填进「接口地址」），因为这是已知的那一个。
  // 这一块是给以后留的位置：再添一家就在这个数组里加一条，别的代码一行都不用动。
  // 但**不为一个还不存在的第二家先造抽象** —— 一份配置表就是它需要的全部形状。
  const ENDPOINT_PRESETS: { name: string; base_url: string }[] = [
    { name: 'DeepSeek', base_url: 'https://api.deepseek.com' },
  ];

  // ── 模型清单（服务方暴露的那个接口）──
  //
  // 模型名**不硬编码**（ADR-0005）：能问服务方要清单就问它要，问不到就手填。
  let models = $state<string[]>([]);
  // 手填开关：清单拉到了也可以改成手打。它挡的是「清单不全」那件事 ——
  // 服务方那边列出来的名字未必是你想填的那个，锁死在清单上就成了另一种硬编码。
  let manualModels = $state(false);
  let checking = $state(false);
  // 校验的结果：ok 为真时是一句「通了」，假时是服务方的原话（或者本地那句缺项的报错）。
  let checkResult = $state<{ ok: boolean; text: string } | null>(null);

  function errorMessage(err: unknown): string {
    return err instanceof Error ? err.message : String(err);
  }

  // fetchModels 顺手把清单拉来填下拉。失败**不留痕迹**：拉不到就退回手填，
  // 而模型名本来就能手打（见下面那两个字段的写法）。它不是用户按下的那一次 ——
  // 用户按下的那一次是 checkModel，那次会照实说结果。
  async function fetchModels(): Promise<void> {
    try {
      models = (await VLM.Models(patch)) ?? [];
    } catch {
      models = [];
    }
  }

  // checkModel 用**当前这份配置**真发一次请求，把结果说给用户听。
  //
  // 走的是 GET /models（服务方暴露的那个接口，见 Go 侧 vlm.Service.Models）：
  // 这是最省的一次 —— 它一个 token 都不生成。顺带把清单收下来填上面那两个下拉。
  //
  // 它验到了什么、没验到什么，下面那行字里写着（那段话与 Go 侧 deepseek.go 的 Models
  // 是同一份），别在这里把它说成「配置没问题」—— 它验不了那个。
  async function checkModel() {
    if (checking) return;
    checking = true;
    checkResult = null;
    try {
      const list = (await VLM.Models(patch)) ?? [];
      models = list;
      checkResult = {
        ok: true,
        text: `通了：这个地址、这份凭据服务方认。它报了 ${list.length} 个模型名。`,
      };
    } catch (err) {
      // 凭据**不会**出现在这句话里：它只进不出（Go 侧报错里也不带它，见 vlm 的 authorize）。
      models = [];
      checkResult = { ok: false, text: errorMessage(err) };
    } finally {
      checking = false;
    }
  }

  // modelChoices 给下拉用的选项：清单里的全部 + **当前填的那个**。
  //
  // 为什么非要带上当前值：清单是服务方**那一刻**的样子，而配置里存的名字可能已经不在里面了
  // （也正是 ADR-0005 不许硬编码模型 ID 的那个理由）。不带上它，一个绑着 value 的下拉
  // 会当场把用户原来填的名字换成清单里的第一条 —— 他什么都没动，配置却变了。
  function modelChoices(current: string): string[] {
    const cur = current.trim();
    if (cur === '' || models.includes(cur)) return models;
    return [cur, ...models];
  }

  // 用下拉还是手填：清单拉到了才给下拉（没拉到就退回手填——不能把用户锁在一个空清单上）。
  const pickFromList = $derived(models.length > 0 && !manualModels);

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
      // 已经配好了就顺手把模型清单拉一次，好让下面两个模型名一进来就是可挑的下拉
      // （用户来这一页十有八九是为了换模型）。**静默**：失败就是手填，不说一句废话 ——
      // 想验配置请按「校验模型」，那一次会把结果说清楚。
      if (v.Configured) void fetchModels();
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
          void loadReview();
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
    // 上一次那句「通了」是对**旧配置**说的：这份一改，它就不再是这句话的意思了。
    checkResult = null;
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

  // ── 复习参数 ──

  async function loadReview() {
    try {
      const v = await Review.Config();
      reviewView = v;
      reviewPreview = null;
      reviewFuzz = v.Fuzz;
      // 读回来的是**实际生效**的那个值：设置文件里没这一项（旧版写的）时服务端会给默认值，
      // 所以界面上不会出现一个 0。
      reviewRetention = v.RequestRetention;
      reviewExamDate = v.ExamDate;
      reviewError = '';
    } catch (err) {
      reviewError = errorMessage(err);
    }
  }

  // previewReview 按**表单里现在这份**算一遍间隔预览。
  //
  // 不等保存就显示，是因为这一页要回答的就是「这么设之后间隔变多长」—— 那个答案要在按下
  // 保存之前看到，否则每试一个日期都是一次真的写入（还会顺手拉一批到期日回来）。
  // 算不出来（日期正打到一半）就退回按已保存那份显示，**不弹错**：他还在打字。
  async function previewReview() {
    try {
      reviewPreview = await Review.Preview({
        request_retention: reviewRetention,
        fuzz: reviewFuzz,
        exam_date: reviewExamDate,
      });
    } catch {
      reviewPreview = null;
    }
  }

  // 预览那行字：四档各多少天。
  //
  // 开着模糊时这几个数每次读都差几个百分点 —— 那是「模糊」的定义（Go 侧 config.go 里
  // 写着），不是界面在抽风，所以下面那行字里要说明一句。
  function intervalsText(list: RatingInterval[] | null): string {
    if (!list || list.length === 0) return '算不出来';
    return list.map((ri) => `${ratingLabel(ri.Rating)} ${ri.Days} 天`).join(' · ');
  }

  async function saveReview() {
    if (reviewSaving) return;
    reviewSaving = true;
    reviewSaved = '';
    try {
      // 键名是小写的那两个：Go 侧 Config 带 json tag（fuzz / exam_date），生成的类型用的
      // 就是 tag 名 —— 写 Go 的字段名编译期就过不去（与 digest / vlm 同一个规矩）。
      const cfg: ReviewConfig = {
        request_retention: reviewRetention,
        fuzz: reviewFuzz,
        exam_date: reviewExamDate,
      };
      // 整份提交，不是补丁：这一份没有凭据要护着。
      const res = await Review.SetConfig(cfg);
      reviewView = res.View;
      reviewPreview = null;
      reviewFuzz = res.View.Fuzz;
      reviewExamDate = res.View.ExamDate;
      reviewError = '';
      // 「拉回来几道」必须说出来：用户改一个日期，库里一批题的到期日刚刚被动了，
      // 那不该悄悄发生。
      reviewSaved =
        res.PulledBack > 0
          ? `已保存。有 ${res.PulledBack} 道题的到期日排在这个上限之外，已经拉回来。`
          : '已保存';
    } catch (err) {
      // 日期写坏了会被后端当场退回、不落盘，所以照实显示，别假装存上了。
      reviewError = errorMessage(err);
    } finally {
      reviewSaving = false;
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

  <!-- 子标签。用一排按钮而不是靠横滑：横滑在 App 那一层是**翻页**（切「拍照/题库/复习/设置」），
       这里再套一层横滑，同一个手势就会有两种意思。 -->
  <nav class="tabs" aria-label="这一页的分区">
    <button
      class="tab"
      class:on={tab === 'chat'}
      aria-current={tab === 'chat' ? 'page' : undefined}
      onclick={() => (tab = 'chat')}
    >
      对话
    </button>
    <button
      class="tab"
      class:on={tab === 'settings'}
      aria-current={tab === 'settings' ? 'page' : undefined}
      onclick={() => (tab = 'settings')}
    >
      设置
    </button>
  </nav>

  <!-- 两块都留在 DOM 里，只是把不显示的那块藏起来。
       对话那块**不能**用 {#if} 卸掉：它会话不落库（见 AgentChat.svelte 的文件头），
       去设置页填个 Key 再回来就会把刚聊的一整段丢掉。 -->
  <div class="pane" class:hidden={tab !== 'chat'}>
    <!-- configured 由这里喂进去：这一页要判断「模型配没配」，而那份配置是这一层读的
         （它同时还要给上面那个「已配置/未配置」的角标用）。 -->
    <AgentChat configured={view ? view.Configured : null} />
  </div>

  <!-- loading 那句只在「设置」这一页说：它是这张表单在等的配置。停在对话页时不该冒出来 ——
       它会是 .agent 这一列的第二个孩子，在聊天区下面露出一行。 -->
  {#if tab === 'settings' && loading && !view}
    <p class="hint">正在读配置…</p>
  {:else}
    <div class="form" class:hidden={tab !== 'settings'}>
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

      <!-- 常用端点：点一下就填进上面那一栏（手打一个地址很容易少一个字母）。
           现在只有一条，见脚本里 ENDPOINT_PRESETS 那段注释。 -->
      <div class="chips">
        <span class="chips-label">常用端点</span>
        {#each ENDPOINT_PRESETS as p (p.base_url)}
          <button
            class="chip"
            type="button"
            onclick={() => (patch.base_url = p.base_url)}
            title={p.base_url}
          >
            {p.name}
          </button>
        {/each}
      </div>

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

      <!-- 两个模型名：拉到清单就是下拉（挑一个，不必手打），拉不到就是文本框。
           下拉的选项里**一定带上当前填的那个**，见脚本里 modelChoices 那段注释。 -->
      <label>
        <span>视觉模型</span>
        {#if pickFromList}
          <select bind:value={patch.vision_model}>
            {#each modelChoices(patch.vision_model) as id (id)}
              <option value={id}>{id}{models.includes(id) ? '' : '（当前填的，清单里没有）'}</option>
            {/each}
          </select>
        {:else}
          <input
            bind:value={patch.vision_model}
            type="text"
            autocapitalize="none"
            autocorrect="off"
            spellcheck="false"
            placeholder="看服务方的文档，别照抄这里"
          />
        {/if}
      </label>

      <label>
        <span>文本模型<span class="opt">选填</span></span>
        {#if pickFromList}
          <select bind:value={patch.text_model}>
            {#each modelChoices(patch.text_model ?? '') as id (id)}
              <option value={id}>{id}{models.includes(id) ? '' : '（当前填的，清单里没有）'}</option>
            {/each}
          </select>
        {:else}
          <input
            bind:value={patch.text_model}
            type="text"
            autocapitalize="none"
            autocorrect="off"
            spellcheck="false"
            placeholder="只有纯文本的活才用它"
          />
        {/if}
      </label>

      <!-- 校验模型：拿**上面这份配置**真发一次请求，把结果说给用户听。
           它走的是 GET /models（服务方暴露的那个接口），一个 token 都不生成 ——
           这是能问出「端点与凭据认不认」的最省的一次调用。
           下面那行字是**这个按钮的全部意思**，别把它读成「配置没问题」：它验不到的
           是「这个视觉模型能不能读图」—— 那要真发一张图过去才行。 -->
      <div class="actions">
        <button class="pill" onclick={checkModel} disabled={checking}>
          {checking ? '正在问服务方…' : '校验模型'}
        </button>
        {#if models.length > 0}
          <label class="row inline">
            <input type="checkbox" bind:checked={manualModels} />
            <span class="opt">手填模型名</span>
          </label>
        {/if}
      </div>
      <p class="note">
        校验走 GET /models，不生成 token、不花额度。它验的是**这个地址通、这份凭据被认**；
        验不了「这个视觉模型能不能读图」（那得真发一张图过去），也验不了某个模型名一定存在 ——
        清单是服务方那一刻的样子。校验不会保存：改了哪一项要按下面的「保存」才落盘。
      </p>
      {#if checkResult}
        <p class="banner" class:ok={checkResult.ok}>{checkResult.text}</p>
      {/if}
      {#if models.length === 0 && checkResult?.ok}
        <!-- 通了、但一个模型名都没报回来：清单这条路走不通，就手填（见上面那段）。
             不把它说成失败 —— 端点与凭据确实是通的。 -->
        <p class="banner warn">
          它没报出任何模型名，上面两个名字请照服务方的文档手填。
        </p>
      {/if}

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

      <!-- 这几块设置共用一个滚动区：各给一个 .form 会各带一条滚动条，
           手机上滑起来会分不清自己在滚哪一块。 -->
      <hr class="sep" />

      <!-- ── 复习参数（票 18）──
           只有两个旋钮：考试日期、间隔模糊。**没有**「最大间隔」那一栏 —— 上限是从考试
           日期推出来的（ADR-0008），多一个要手填的天数只会让人不知道该填哪个。 -->
      <p class="note">
        间隔由 FSRS 算。要说的其实是「考试之前都得再过一遍」，所以上限是从考试日期推出来的，
        不用你手填一个天数；而「多久复习一次」由期望保留率定 —— 调高它复习得更密，间隔也就
        不会全挤到上限那几天去。
      </p>

      {#if reviewView?.Problem}
        <p class="banner warn">{reviewView.Problem}</p>
      {/if}
      {#if reviewError}
        <p class="banner">{reviewError}</p>
      {/if}
      {#if reviewSaved}
        <p class="banner ok">{reviewSaved}</p>
      {/if}

      <label>
        <span>
          期望保留率<span class="opt">调高 → 间隔更短、复习更密</span>
        </span>
        <!-- 滑块而不是数字框：手机上敲「0.95」费劲，而这个值要的是看得见它在动 ——
             下面那行四档间隔会跟着实时变（previewReview），所以「调高会怎样」不必猜。 -->
        <div class="slider">
          <input
            type="range"
            min="0.70"
            max="0.99"
            step="0.01"
            bind:value={reviewRetention}
            oninput={previewReview}
          />
          <span class="val">{reviewRetention.toFixed(2)}</span>
        </div>
      </label>

      <label>
        <span>考试日期<span class="opt">空着就不限</span></span>
        <!-- type="date" 唤起系统日期选择器：手机上手敲「2026-12-19」很容易错一位。
             它的值就是 "YYYY-MM-DD"，与 Go 侧那个格式一字不差。 -->
        <input type="date" bind:value={reviewExamDate} oninput={previewReview} />
      </label>

      {#if reviewShown}
        <p class="note">
          {#if reviewShown.ExamDate === ''}
            现在没有上限：四档最长能排到 {reviewShown.MaxIntervalDays} 天之后。
            填上考试日期，间隔就会被压到那一天之内。
          {:else if reviewShown.ExamActive}
            距考试 {reviewShown.DaysToExam} 天：任何评级都不会把题排到
            {reviewShown.MaxIntervalDays} 天以后，也就是考试那天之前。
          {:else}
            这个日期已经过去了，上限不再生效 —— 现在按默认的不限
            （{reviewShown.MaxIntervalDays} 天）排。
          {/if}
        </p>

        <p class="note">
          按这一套参数的间隔：新卡 {intervalsText(reviewShown.Preview.New)}；
          复习过的卡 {intervalsText(reviewShown.Preview.Reviewed)}。
          {#if reviewShown.Fuzz}
            开着模糊，这几个数每次看会差几个百分点 —— 也正因如此，同一天评的题不会永远
            撞在同一天到期。
          {/if}
        </p>
      {/if}

      <label class="row">
        <input type="checkbox" bind:checked={reviewFuzz} onchange={previewReview} />
        <span>间隔模糊<span class="opt">把同一天到期的题散开</span></span>
      </label>

      <div class="actions">
        <button class="pill go" onclick={saveReview} disabled={reviewSaving}>
          {reviewSaving ? '保存中…' : '保存'}
        </button>
        <button class="pill" onclick={loadReview} disabled={reviewSaving}>重新读取</button>
      </div>

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
        应用一卸载私有目录就全没了 —— 这个包是防丢的唯一手段。
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

  /* 子标签那一排：两个平级的按钮，当前那个亮起来。 */
  .tabs {
    flex: none;
    display: flex;
    gap: 0.4rem;
    padding: 0.6rem 1rem 0;
  }
  .tab {
    flex: none;
    padding: 0.4rem 1rem;
    border: 1px solid transparent;
    border-radius: 999px;
    background: transparent;
    color: rgba(244, 246, 251, 0.6);
    font: inherit;
    font-size: 0.9rem;
    cursor: pointer;
    touch-action: manipulation;
    -webkit-tap-highlight-color: transparent;
    --wails-draggable: no-drag;
  }
  .tab.on {
    border-color: rgba(130, 200, 255, 0.6);
    background: rgba(130, 200, 255, 0.14);
    color: #9fd4ff;
    font-weight: 600;
  }

  /* 对话那一块：flex:1 + min-height:0，让它按**剩下的**高度定尺寸，
     内部（记录区 / 输入框）再各自安排 —— 键盘压矮窗口时它跟着矮，输入框就浮上去了。 */
  .pane {
    flex: 1;
    min-height: 0;
    display: flex;
    flex-direction: column;
  }
  /* 不显示的那一块整个移出布局（不是 visibility）：它不该占着高度，
     也不该被别处的高度计算算进去。 */
  .pane.hidden {
    display: none;
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
  /* 「设置」那一块靠这个藏起来：.form.hidden 比 .form 多一个类，权重够，与先后顺序无关。 */
  .form.hidden {
    display: none;
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

  /* 常用端点那一排：一行小标签 + 几个可点的胶囊。
     与讨论页那排预设追问同一个长相（同一个作者、同一套配色）。 */
  .chips {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.35rem;
    /* 上面那条 label 是竖排的（label 的通配规则），这一排不是 label，但也要点得着。 */
    margin-top: -0.3rem;
  }
  .chips-label {
    font-size: 0.75rem;
    color: rgba(244, 246, 251, 0.35);
  }
  .chip {
    padding: 0.25rem 0.6rem;
    border: 1px solid rgba(130, 200, 255, 0.4);
    border-radius: 999px;
    background: rgba(130, 200, 255, 0.1);
    color: #9fd4ff;
    font: inherit;
    font-size: 0.8rem;
    cursor: pointer;
    touch-action: manipulation;
    -webkit-tap-highlight-color: transparent;
    --wails-draggable: no-drag;
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
  /* 挤在 .actions 那一排按钮里的那个勾选框（「手填模型名」）：宽度按内容来，
     不能像别的 label 那样把整行占满，否则会把旁边的按钮挤扁。 */
  label.row.inline {
    flex: none;
    gap: 0.35rem;
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

  /* 滑块那一行：滑块占满，右边跟着一个两位小数的读数。 */
  .slider {
    display: flex;
    align-items: center;
    gap: 0.6rem;
  }
  .val {
    flex: none;
    min-width: 2.6rem;
    text-align: right;
    font-size: 0.9rem;
    font-variant-numeric: tabular-nums;
    color: rgba(244, 246, 251, 0.85);
  }
  /* 上面那条 input, select, textarea 的通配规则是给文本框写的；滑块套上去会变成一个
     带边框底色的怪盒子，这里把它改回一根轨道（accent-color 与勾选框同一套配色）。 */
  input[type='range'] {
    flex: 1;
    width: auto;
    padding: 0;
    border: 0;
    background: none;
    accent-color: #9fd4ff;
  }

  /* 时间与日期选择器那个图标是深色的，深色底上几乎看不见，翻成白的。 */
  input[type='time']::-webkit-calendar-picker-indicator,
  input[type='date']::-webkit-calendar-picker-indicator {
    filter: invert(1);
    opacity: 0.55;
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
