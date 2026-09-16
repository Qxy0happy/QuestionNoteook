<script lang="ts">
  // 取景页。本页是三页里的默认落地页，所以挂载即开镜 —— 不需要额外的「打开相机」点击。
  // 取景框与中心十字是**纯装饰**的对准辅助：不做边缘检测，也不随画面内容变化。
  //
  // 快门之后本页进入「框选」态：底图铺满 + 居中可变边长的矩形选框。
  // 确认那一刻定稿 —— 裁出的子图交给 Go 侧拉正、落盘、入库，帧随即丢掉（ADR-0007）。

  import * as capture from '../bindings/questionbook/internal/capture/service.js';
  import * as library from '../bindings/questionbook/internal/library/service.js';
  import type { Quad } from '../bindings/questionbook/internal/capture/models.js';
  import CropBox, { type Box } from './CropBox.svelte';

  // 选框初值：居中、占七成。留出的那一圈不只是好看 —— 看得见框外，才判断得出框歪没歪。
  const DEFAULT_BOX: Box = { x: 0.15, y: 0.15, w: 0.7, h: 0.7 };

  let root = $state<HTMLElement | null>(null);
  let video = $state<HTMLVideoElement | null>(null);
  // 框选态的舞台。帧在它里面按 contain 铺开，选框也以它为坐标系。
  let stage = $state<HTMLElement | null>(null);
  // 本页是否露在屏幕上。三页是同时挂载的，只有可见时才该占着相机。
  let visible = $state(false);
  // 快门抓到的无损帧（源像素密度，已按预览裁过），交给下游的那一份。
  let frame = $state<HTMLCanvasElement | null>(null);
  // 同一个帧的 JPEG data URL，**仅供显示** —— 有损，不进图像管线。
  let frameUrl = $state<string | null>(null);
  // 舞台的像素尺寸。帧铺多大、选框换算成屏幕上的哪一块，都从它来。
  let stageW = $state(0);
  let stageH = $state(0);
  // 选框，归一化到帧上（定义见 CropBox 的 Box）。
  let crop = $state<Box>({ ...DEFAULT_BOX });
  // 入库后按 hash 取回的成品卡片图（PNG data URL）。
  let cardUrl = $state<string | null>(null);
  // 交给 Go 的那一趟还没回来。期间锁住三个按钮，免得重复提交。
  let busy = $state(false);
  let errorText = $state('');

  // 流只在可见期间持有，滑走就还给系统。
  let stream: MediaStream | null = null;

  // 帧在舞台上实际占住的那块矩形 —— 就是屏幕上「帧的 (0,0)」与右下角所在的位置。
  // 选框的归一化坐标靠它换算，所以它必须与 .base 的定位用**同一个**算出来的值，
  // 不能指望 CSS 那边再算一遍（两边差一个像素，框出来的就不是用户看到的那块）。
  const baseRect = $derived.by(() => {
    const src = frame;
    if (!src || stageW < 1 || stageH < 1) return { left: 0, top: 0, width: 0, height: 0 };
    const s = Math.min(stageW / src.width, stageH / src.height);
    const width = src.width * s;
    const height = src.height * s;
    return { left: (stageW - width) / 2, top: (stageH - height) / 2, width, height };
  });

  $effect(() => {
    const el = stage;
    if (!el) return;
    // 舞台只在框选态存在，所以这个观察器跟着框选态生灭；窗口尺寸一变它自己会报。
    const ro = new ResizeObserver(() => {
      stageW = el.clientWidth;
      stageH = el.clientHeight;
    });
    ro.observe(el);
    stageW = el.clientWidth;
    stageH = el.clientHeight;
    return () => ro.disconnect();
  });

  $effect(() => {
    const el = root;
    if (!el) return;

    // 默认 root（视口）已经算上祖先滚动容器的裁剪，滑出 .pages 时比率就是 0。
    const io = new IntersectionObserver((entries) => {
      for (const entry of entries) visible = entry.isIntersecting;
    });
    io.observe(el);
    return () => io.disconnect();
  });

  $effect(() => {
    const el = video;
    // 不可见时不起流：滑到 Agent / 题库页就把相机让出去。
    if (!el || !visible) return;

    let disposed = false;
    navigator.mediaDevices
      // 必须显式要分辨率。不给约束时 WebView 会挑个默认档（常见 640×480），
      // 而预览是 cover 裁过的，再经选框一裁，成品就只剩两三百像素宽 —— 字都看不清。
      // 用 ideal 而不是 exact：请求不到理想值也要拿到次好的，别把整条路堵死。
      .getUserMedia({
        video: {
          facingMode: 'environment',
          width: { ideal: 4096 },
          height: { ideal: 4096 },
        },
        audio: false,
      })
      .then((s) => {
        // 拿到流时已经不可见（或组件已销毁）：直接把轨道关掉，别泄漏。
        if (disposed) {
          for (const track of s.getTracks()) track.stop();
          return;
        }
        stream = s;
        // 打到 console → 宿主的 onConsoleMessage → logcat。真机上量分辨率只能靠这条。
        const settings = s.getVideoTracks()[0]?.getSettings();
        console.log(
          `相机流实际拿到: ${settings?.width}x${settings?.height} @${settings?.frameRate ?? '?'}fps`,
        );
        el.srcObject = s;
        return el.play();
      })
      .catch((err: unknown) => {
        errorText = err instanceof Error ? err.message : String(err);
      });

    return () => {
      disposed = true;
      el.srcObject = null;
      if (stream) {
        for (const track of stream.getTracks()) track.stop();
        stream = null;
      }
    };
  });

  // 快门：按预览的 cover 裁出可见区域，再按**源像素密度**落到 canvas 上。
  // 不裁切会让预览与成片视野不一致（竖屏看的是窄带，拍下来却是一整张横画幅），
  // 而那个不一致正好毁掉取景框的对准意义。不降采样则是因为下游要读手写题。
  function shutter() {
    const el = video;
    if (!el || el.videoWidth === 0) return;

    const vw = el.videoWidth;
    const vh = el.videoHeight;
    // 每次按下现算，所以窗口尺寸变了自然跟着变，不需要缓存或 resize 监听。
    const box = el.getBoundingClientRect();
    if (box.width === 0 || box.height === 0) return;

    // object-fit: cover 的逆运算：s = max(cw/vw, ch/vh)，可见源区域居中。
    const s = Math.max(box.width / vw, box.height / vh);
    const w = Math.round(box.width / s);
    const h = Math.round(box.height / s);
    const sx = Math.round((vw - w) / 2);
    const sy = Math.round((vh - h) / 2);

    const canvas = document.createElement('canvas');
    canvas.width = w;
    canvas.height = h;
    const ctx = canvas.getContext('2d');
    if (!ctx) return;

    // 源矩形与目标同尺寸 —— 逐像素搬运，不重采样。
    ctx.drawImage(el, sx, sy, w, h, 0, 0, w, h);

    crop = { ...DEFAULT_BOX };
    cardUrl = null;
    frame = canvas;
    frameUrl = canvas.toDataURL('image/jpeg', 0.92);
  }

  // 旋转底图（顺时针 90°）。转的是**无损帧**本身，不是显示用的那张 img：
  // 90° 整倍数的旋转是像素的置换，没有重采样，所以转几次都不掉画质。
  function rotate() {
    const src = frame;
    if (!src || busy) return;

    const out = document.createElement('canvas');
    out.width = src.height;
    out.height = src.width;
    const ctx = out.getContext('2d');
    if (!ctx) return;

    // 用整数矩阵而不是 rotate(π/2)：那个 π/2 的余弦是 6e-17 而不是 0，
    // 亚像素的偏移会让整张图过一次插值。(a,b,c,d,e,f) = (0,1,-1,0,新宽,0) 把源的
    // (x,y) 送到目标的 (新宽-y, x)，而新宽正是源的高 —— 这就是顺时针 90°。
    ctx.imageSmoothingEnabled = false;
    ctx.setTransform(0, 1, -1, 0, out.width, 0);
    ctx.drawImage(src, 0, 0);

    frame = out;
    frameUrl = out.toDataURL('image/jpeg', 0.92);

    // 选框是归一化到帧上的，帧一转它的坐标系也换了，得跟着内容一起挪 ——
    // 否则用户先框好了再旋转，框住的那块就跑掉了。顺时针 90° 后新图的 x 轴是旧图的 -y 轴：
    // x' = 1 - (y + h)，y' = x，宽高互换。
    crop = { x: 1 - (crop.y + crop.h), y: crop.x, w: crop.h, h: crop.w };
  }

  function clampInt(v: number, lo: number, hi: number): number {
    return v < lo ? lo : v > hi ? hi : v;
  }

  // 确认框选。到这一步就定稿：帧被丢掉，框歪了只能重拍。
  async function confirm() {
    const src = frame;
    if (!src || busy) return;
    busy = true;
    errorText = '';

    try {
      // 1) 按选框在**无损帧**上裁子图。归一化 → 像素先在同步段里定下来：
      //    后面 await 会让出线程，那时用户再拖选框也不该改变这一次提交的内容。
      //    夹一次边界：四舍五入可能把右边推到帧外一个像素。
      const px = clampInt(Math.round(crop.x * src.width), 0, src.width - 1);
      const py = clampInt(Math.round(crop.y * src.height), 0, src.height - 1);
      const pw = clampInt(Math.round(crop.w * src.width), 1, src.width - px);
      const ph = clampInt(Math.round(crop.h * src.height), 1, src.height - py);

      const cut = document.createElement('canvas');
      cut.width = pw;
      cut.height = ph;
      const ctx = cut.getContext('2d');
      if (!ctx) throw new Error('裁图：拿不到 2d 上下文');
      // 源矩形与目标同尺寸 —— 逐像素搬运，不重采样。
      ctx.drawImage(src, px, py, pw, ph, 0, 0, pw, ph);

      // PNG 而不是 JPEG：卡片图存的是原始像素，过一次有损编码就再也回不来了
      // （spec 的 Out of Scope：不做不可逆增强）。
      const base64 = cut.toDataURL('image/png').split(',')[1];
      if (!base64) throw new Error('裁图：PNG 编码失败');

      // 2) 四个角点是选框在**裁后那张图**里的位置，顺序固定 左上 → 右上 → 右下 → 左下。
      //
      //    选框是轴对齐的矩形，所以这一步今天在几何上是一次**恒等变换** —— 看着像白跑
      //    一趟，但**不能省**：它是 Go 侧唯一被 fixture 测试覆盖的入口，也是将来改成
      //    四角可独立拖时前端唯一要动的地方（那时把四个值换成各自的拖动结果即可，
      //    Go 侧一行都不用改）。
      const quad: Quad = [
        { X: 0, Y: 0 }, // 左上
        { X: pw, Y: 0 }, // 右上
        { X: pw, Y: ph }, // 右下
        { X: 0, Y: ph }, // 左下
      ];
      const hash = await capture.Rectify(base64, quad);

      // 3) 先把成品取回来，**最后才入库**。这个顺序是为了可重入：
      //    只有最后一步可能失败，此时用户再按一次「确认」是安全的 ——
      //    Rectify 是内容寻址的，返回同一个 hash，走的是同一条路径。
      //
      //    反过来（Add 成功但取图失败）就会出事：catch 里 frame 不清、busy 已释放，
      //    用户再按一次「确认」时 Add 会为同一张卡片图**再建一道错题**，题号翻倍。
      //
      //    另：先 Add 并不能避免孤儿文件 —— 卡片图是 Rectify 内部落盘的，
      //    走到 Add 这一步时文件早就写下去了，两种顺序的孤儿窗口一样大。
      const card = await capture.Card(hash);

      // 4) 入库。答案图还没有，第二个参数传空串（spec：手边没答案也先存题图）。
      await library.Add(hash, '');

      cardUrl = `data:image/png;base64,${card}`;
      // 定稿了：帧与它的显示副本一起丢掉。原图不再保留是刻意的（ADR-0007）。
      frame = null;
      frameUrl = null;
    } catch (err: unknown) {
      errorText = err instanceof Error ? err.message : String(err);
    } finally {
      busy = false;
    }
  }

  // 重拍 / 拍下一张：三条状态一起清掉，回到取景。
  function retake() {
    if (busy) return;
    frame = null;
    frameUrl = null;
    cardUrl = null;
    crop = { ...DEFAULT_BOX };
    errorText = '';
  }
</script>

<div class="capture" bind:this={root}>
  <video bind:this={video} class="preview" autoplay muted playsinline></video>

  {#if cardUrl}
    <!-- 成品。原图已经没了，这里能做的只有继续拍下一道。 -->
    <div class="shot">
      <img class="card" src={cardUrl} alt="刚入库的题图" />
      <button class="pill" onclick={retake}>拍下一张</button>
    </div>
  {:else if frameUrl}
    <div class="shot" bind:this={stage}>
      <!-- 底图铺满屏幕：占满整页，只按帧的比例留边（黑底由 .shot 出）。
           位置由 baseRect 明确给出，与选框共用同一份几何 —— 两边各算一遍迟早会差一点。 -->
      <img
        class="base"
        src={frameUrl}
        alt="刚拍下的画面"
        style="left: {baseRect.left}px; top: {baseRect.top}px; width: {baseRect.width}px; height: {baseRect.height}px;"
      />

      <CropBox rect={baseRect} bind:value={crop} />

      <div class="tools">
        <button class="pill" onclick={retake} disabled={busy}>重拍</button>
        <button class="pill go" onclick={confirm} disabled={busy}>{busy ? '处理中…' : '确认'}</button>
        <button class="pill" onclick={rotate} disabled={busy}>旋转</button>
      </div>
    </div>
  {:else}
    <div class="reticle" aria-hidden="true"></div>
    <button class="shutter" onclick={shutter} aria-label="快门"></button>
  {/if}

  {#if errorText}
    <p class="error">{errorText}</p>
  {/if}
</div>

<style>
  .capture {
    position: relative;
    width: 100%;
    height: 100%;
    overflow: hidden;
    background: #000;
  }

  .preview {
    display: block;
    width: 100%;
    height: 100%;
    /* 预览铺满整页；取景框只是压在上面的辅助线。 */
    object-fit: cover;
  }

  /* 固定居中：不随画面内容移动，也不随手指拖动。 */
  .reticle {
    position: absolute;
    top: 50%;
    left: 50%;
    translate: -50% -50%;
    /* 三个约束取最小，窄屏、宽屏、矮窗都不会溢出；比例始终 3:4。 */
    width: min(72vw, 26rem, 46vh);
    aspect-ratio: 3 / 4;
    border: 2px solid rgba(255, 255, 255, 0.9);
    border-radius: 1rem;
    /* 内一圈暗描边：亮场景下白框也不会糊掉。 */
    box-shadow: inset 0 0 0 1px rgba(0, 0, 0, 0.35);
    pointer-events: none;
  }

  /* 中心十字准星。 */
  .reticle::before,
  .reticle::after {
    content: '';
    position: absolute;
    top: 50%;
    left: 50%;
    translate: -50% -50%;
    background: rgba(255, 255, 255, 0.9);
    box-shadow: 0 0 2px rgba(0, 0, 0, 0.6);
  }
  .reticle::before {
    width: 2rem;
    height: 1px;
  }
  .reticle::after {
    width: 1px;
    height: 2rem;
  }

  .shutter {
    position: absolute;
    left: 50%;
    bottom: max(1.75rem, env(safe-area-inset-bottom));
    translate: -50% 0;
    width: 4.5rem;
    height: 4.5rem;
    padding: 0;
    border: 3px solid rgba(255, 255, 255, 0.9);
    border-radius: 50%;
    background: rgba(255, 255, 255, 0.25);
    cursor: pointer;
    touch-action: manipulation;
    -webkit-tap-highlight-color: transparent;
    /* 桌面端按钮不拖着窗口走。 */
    --wails-draggable: no-drag;
  }
  .shutter:active {
    background: rgba(255, 255, 255, 0.55);
  }

  /* 拍完之后的两态（框选 / 成品）都铺满整页，底子全黑。 */
  .shot {
    position: absolute;
    inset: 0;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: 1.5rem;
    background: #000;
    user-select: none;
    -webkit-user-select: none;
  }

  /* 帧的位置尺寸由内联的 baseRect 给，这里只管不参与重采样与非拖拽。 */
  .base {
    position: absolute;
    -webkit-user-drag: none;
  }

  /* 成品卡片图。已经拉正了，按原比例整个装进来即可。 */
  .card {
    max-width: 100%;
    max-height: 78%;
    object-fit: contain;
    -webkit-user-drag: none;
  }

  /* 三个控件压在选框蒙版之上（DOM 里在 CropBox 之后，同在 .shot 这个层叠上下文里）。 */
  .tools {
    position: absolute;
    left: 50%;
    bottom: max(1.5rem, env(safe-area-inset-bottom));
    translate: -50% 0;
    /* 整行定宽、三格均分：确认在忙碌时换文案，按钮和整行都不跟着抽一下。 */
    width: min(calc(100% - 2rem), 24rem);
    display: flex;
    align-items: center;
    gap: 0.75rem;
  }
  .tools .pill {
    flex: 1;
    padding-inline: 0;
  }

  .pill {
    padding: 0.7rem 2rem;
    border: 1px solid rgba(244, 246, 251, 0.35);
    border-radius: 999px;
    background: rgba(244, 246, 251, 0.12);
    color: #f4f6fb;
    font: inherit;
    font-size: 1rem;
    cursor: pointer;
    touch-action: manipulation;
    -webkit-tap-highlight-color: transparent;
    --wails-draggable: no-drag;
  }
  .pill:active {
    background: rgba(244, 246, 251, 0.24);
  }
  /* 三个都不可用时要看得出来，否则会以为界面卡死了。 */
  .pill:disabled {
    opacity: 0.45;
    cursor: default;
  }

  /* 确认是这一步的主动作，给它一点重量。 */
  .go {
    border-color: rgba(244, 246, 251, 0.7);
    background: rgba(244, 246, 251, 0.85);
    color: #14161c;
    font-weight: 600;
  }
  .go:active:not(:disabled) {
    background: #f4f6fb;
  }

  .error {
    position: absolute;
    left: 1.5rem;
    right: 1.5rem;
    top: 1.5rem;
    margin: 0;
    padding: 0.75rem 1rem;
    border-radius: 0.75rem;
    background: rgba(0, 0, 0, 0.7);
    color: #ffb4b4;
    font-size: 0.85rem;
    text-align: center;
  }
</style>
