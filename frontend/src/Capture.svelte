<script lang="ts">
  // 取景页。本页是三页里的默认落地页，所以挂载即开镜 —— 不需要额外的「打开相机」点击。
  // 取景框与中心十字是**纯装饰**的对准辅助：不做边缘检测，也不随画面内容变化。

  let root = $state<HTMLElement | null>(null);
  let video = $state<HTMLVideoElement | null>(null);
  // 本页是否露在屏幕上。三页是同时挂载的，只有可见时才该占着相机。
  let visible = $state(false);
  // 快门抓到的无损帧（源像素密度，已按预览裁过），交给下游的那一份。
  let frame = $state<HTMLCanvasElement | null>(null);
  // 同一个帧的 JPEG data URL，**仅供显示** —— 有损，不进图像管线。
  let frameUrl = $state<string | null>(null);
  let errorText = $state('');

  // 流只在可见期间持有，滑走就还给系统。
  let stream: MediaStream | null = null;

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
      .getUserMedia({ video: { facingMode: 'environment' }, audio: false })
      .then((s) => {
        // 拿到流时已经不可见（或组件已销毁）：直接把轨道关掉，别泄漏。
        if (disposed) {
          for (const track of s.getTracks()) track.stop();
          return;
        }
        stream = s;
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

    frame = canvas;
    frameUrl = canvas.toDataURL('image/jpeg', 0.92);
  }

  function retake() {
    frame = null;
    frameUrl = null;
  }
</script>

<div class="capture" bind:this={root}>
  <video bind:this={video} class="preview" autoplay muted playsinline></video>

  <div class="reticle" aria-hidden="true"></div>

  {#if frameUrl}
    <div class="shot">
      <img src={frameUrl} alt="刚拍下的画面" />
      <button class="pill" onclick={retake}>重拍</button>
    </div>
  {:else}
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

  /* 抓到的帧铺满整页 —— 与刚看到的取景画面同视野。 */
  .shot {
    position: absolute;
    inset: 0;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: 1.5rem;
    background: #000;
  }
  .shot img {
    max-width: 100%;
    max-height: 78%;
    object-fit: contain;
    -webkit-user-drag: none;
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
