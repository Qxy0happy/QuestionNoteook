// 就地放大：双指捏合缩放 + 放大之后单指拖动平移。
//
// ── 为什么自己做，而不用浏览器那套 ──
//
// 现成的那套（viewport 的 user-scalable、WebView 自带的缩放）缩的是**整个页面**：
// 导航栏、列表、按钮一起放大。用户要的是把**这一张题图**看清楚，所以页面级的缩放被关掉了
// （见 index.html 的 viewport 与 MainActivity 里那两行），这里只对图做一次 CSS transform。
//
// ── 手势仲裁是这件事真正难的地方，三条规矩 ──
//
//  1. **两根手指才算捏合。** 一发现两指就 preventDefault —— 页面既不滚、也不翻页。
//  2. **放大之后单指拖动是平移**，同样 preventDefault（不然会一边平移一边把页翻走）。
//  3. **没放大时单指一概不碰。** 那一根手指还要能上下滚列表、左右翻页 —— 这也是为什么
//     不能用 `touch-action: none` 一了百了：那会把整页的滚动一起掐掉。只能在 touchmove
//     里按情况 preventDefault，所以监听必须是 non-passive 的。
//
// 缩回原样的路只有一条：**捏到比 1 倍还小，抬手就归位**。所以不存在「卡在放大态出不来」
// —— 那正是当初没做「复位」按钮也敢放它上线的原因。
//
// ⚠️ 第 1、2 条能不能真的抢过外层翻页手势，**只能在真机上试**。纸上推不出来（这台设备上
// 的 WebView 版本、翻页容器用的是 scroll-snap 还是 transform，都会影响）。所以这里的失败
// 方式是有意做软的：抢不到就退化成「没放大」，而不是卡住 —— touchcancel 一律收拾干净。

/** 缩放状态。平移量的单位是 CSS 像素，相对**未缩放**时图片自己的左上角。 */
export interface ZoomState {
  scale: number;
  x: number;
  y: number;
}

/** 原样：铺满容器看全。 */
export const FIT: ZoomState = { scale: 1, x: 0, y: 0 };

/** 倍率超过这个数就算「放大态」：组件的裁剪、增强图的换档都看它。 */
export const ZOOMED_AT = 1.05;

/** 捏合时允许的最小倍率。小于它算「想归位」，抬手时回到 1。 */
const MIN_SCALE = 0.6;

/** 容器与图片在**未缩放**时的位置关系。一次手势开始时量一次。 */
export interface Frame {
  /** 容器内容区（padding box）在视口里的位置与尺寸。 */
  cx: number;
  cy: number;
  cw: number;
  ch: number;
  /** 未缩放时图片在视口里的位置与尺寸。 */
  bx: number;
  by: number;
  bw: number;
  bh: number;
  /** 倍率上限（见 measure 里那个算式的理由）。 */
  maxScale: number;
}

// 一条轴上的位置：图片比容器小就居中，不然夹住不让它拖出缝来。
function axis(pos: number, size: number, view: number): number {
  if (size <= view) return (view - size) / 2;
  return Math.min(0, Math.max(view - size, pos));
}

/** 把状态夹进合法范围：倍率不过头，图片也不会被拖到容器外面去。 */
export function clampZoom(st: ZoomState, f: Frame): ZoomState {
  const scale = Math.min(Math.max(st.scale, MIN_SCALE), f.maxScale);
  // 未缩放时图片左上角相对容器左上角的位置。scale 为 1 时下面算出来正好是这个值，
  // 于是 x、y 都是 0 —— 「原样」是不动点，不会在归位时跳一下。
  const ox = f.bx - f.cx;
  const oy = f.by - f.cy;
  const left = axis(ox + st.x, f.bw * scale, f.cw);
  const top = axis(oy + st.y, f.bh * scale, f.ch);
  return { scale, x: left - ox, y: top - oy };
}

/** 平移一段距离（从**手势开始**那一刻的状态算起，不是逐帧累加，免得漂）。 */
export function panBy(st: ZoomState, dx: number, dy: number, f: Frame): ZoomState {
  return clampZoom({ scale: st.scale, x: st.x + dx, y: st.y + dy }, f);
}

/**
 * 双指捏合的一帧。
 *
 * anchor 是**手势开始时**两指中点落在图片上的那个点（图片坐标系），mid 是此刻两指中点在
 * 视口里的位置 —— 让 anchor 一直贴着 mid，就是「捏的时候想放大的地方就是那里」，而且手指
 * 整体移动时图会跟着走（真实捏合的手感）。
 */
export function pinchTo(
  startState: ZoomState,
  anchor: { x: number; y: number },
  mid: { x: number; y: number },
  p0: { x: number; y: number },
  factor: number,
  f: Frame,
): ZoomState {
  const scale = Math.min(Math.max(startState.scale * factor, MIN_SCALE), f.maxScale);
  return clampZoom(
    {
      scale,
      x: mid.x - p0.x - scale * anchor.x,
      y: mid.y - p0.y - scale * anchor.y,
    },
    f,
  );
}

/** 交给 CSS 的那一行。 */
export function cssTransform(st: ZoomState): string {
  return `translate(${st.x.toFixed(2)}px, ${st.y.toFixed(2)}px) scale(${st.scale.toFixed(4)})`;
}

export interface ZoomOptions {
  /**
   * 换一个值就让这张图归位。组件用它把「换了一道题」告诉这里 ——
   * 走 update 而不是暴露一个 reset 方法，是因为组件拿不到 action 的返回值。
   */
  resetKey?: unknown;
  /**
   * 每次倍率/平移变化都回调。组件拿它做两件事：放大时裁掉溢出、按需换更高分辨率的增强图。
   */
  onChange?: (st: ZoomState, zoomed: boolean) => void;
}

function dist(a: Touch, b: Touch): number {
  return Math.hypot(a.clientX - b.clientX, a.clientY - b.clientY);
}

function mid(a: Touch, b: Touch): { x: number; y: number } {
  return { x: (a.clientX + b.clientX) / 2, y: (a.clientY + b.clientY) / 2 };
}

/**
 * Svelte action：把手势面挂在**容器**上，缩放作用在里面的 `<img>` 上。
 *
 * 挂在容器而不是图上，是因为手指常常落在图旁边的空白处（题图窄、容器宽），
 * 那里也该算数。图还没加载出来时什么都不做，加载出来自然就能用了（每次手势开始时才去找）。
 */
export function pinchZoom(node: HTMLElement, opts: ZoomOptions = {}) {
  const surface = node;
  let st: ZoomState = { ...FIT };
  let frame: Frame | null = null;
  let mode: 'idle' | 'pinch' | 'pan' = 'idle';
  let key = opts.resetKey;
  let pinchStart: { state: ZoomState; d: number; anchor: { x: number; y: number } } | null = null;
  let panStart: { state: ZoomState; x: number; y: number } | null = null;

  const image = (): HTMLImageElement | null => surface.querySelector('img');

  function measure(): Frame | null {
    const el = image();
    if (!el) return null;
    const rect = el.getBoundingClientRect();
    const box = surface.getBoundingClientRect();
    if (rect.width <= 0 || rect.height <= 0) return null;
    // rect 是**已经被缩放过的**框：除以当前倍率还原出未缩放时的尺寸，
    // 左上角减去平移量还原出未缩放时的位置（transform-origin 是图片自己的左上角）。
    const bw = rect.width / st.scale;
    const bh = rect.height / st.scale;
    // 容器量的是 padding box（clientWidth/Height）：那里正是图片被布局居中的范围，
    // 用 border box 的话两边那点 padding 会让「原样」与夹取结果差一点，归位时看得见跳。
    const dpr = window.devicePixelRatio || 1;
    // 上限不是拍脑袋的 6：题图是原始像素，拉到「一个图片像素对一个设备像素」之后再拉，
    // 多出来的只是插值的糊。下限 2 保证总能放大到看得清。
    const maxScale = Math.min(6, Math.max(2, el.naturalWidth / Math.max(1, bw * dpr)));
    return {
      cx: box.left + surface.clientLeft,
      cy: box.top + surface.clientTop,
      cw: surface.clientWidth,
      ch: surface.clientHeight,
      bx: rect.left - st.x,
      by: rect.top - st.y,
      bw,
      bh,
      maxScale,
    };
  }

  function apply(next: ZoomState) {
    st = next;
    const el = image();
    if (el) {
      el.style.transformOrigin = '0 0';
      el.style.transform = cssTransform(st);
      el.style.willChange = 'transform';
    }
    opts.onChange?.(st, st.scale > ZOOMED_AT);
  }

  function armPan(t: Touch, f: Frame) {
    mode = 'pan';
    panStart = { state: { ...st }, x: t.clientX, y: t.clientY };
    frame = f;
  }

  function onStart(e: TouchEvent) {
    if (e.touches.length === 2) {
      const f = measure();
      if (!f) return;
      const m = mid(e.touches[0], e.touches[1]);
      pinchStart = {
        state: { ...st },
        d: Math.max(1, dist(e.touches[0], e.touches[1])),
        // 手势开始时两指中点落在图片上的哪个点（图片坐标系）。
        anchor: { x: (m.x - f.bx - st.x) / st.scale, y: (m.y - f.by - st.y) / st.scale },
      };
      frame = f;
      mode = 'pinch';
      return;
    }
    // 没放大时**一概不碰**：那一根手指是给滚列表和翻页用的。
    if (e.touches.length === 1 && st.scale > ZOOMED_AT) {
      const f = measure();
      if (f) armPan(e.touches[0], f);
    }
  }

  function onMove(e: TouchEvent) {
    if (mode === 'pinch' && pinchStart && frame && e.touches.length >= 2) {
      const [a, b] = [e.touches[0], e.touches[1]];
      const next = pinchTo(
        pinchStart.state,
        pinchStart.anchor,
        mid(a, b),
        { x: frame.bx, y: frame.by },
        dist(a, b) / pinchStart.d,
        frame,
      );
      apply(next);
      // 两指落下那一刻就该阻止滚动与翻页，所以这里必须拦住（监听是 non-passive 的）。
      e.preventDefault();
      return;
    }
    if (mode === 'pan' && panStart && frame && e.touches.length === 1) {
      const t = e.touches[0];
      apply(panBy(panStart.state, t.clientX - panStart.x, t.clientY - panStart.y, frame));
      e.preventDefault();
    }
  }

  function onEnd(e: TouchEvent) {
    if (mode === 'idle') return;
    if (e.touches.length > 0) {
      // 还有手指在屏幕上（两指抬掉一根，或者反过来）。**重新起一段手势**，不要用剩下的
      // 那根接着上一段的位移 —— 那会让图猛地跳一下。
      mode = 'idle';
      pinchStart = null;
      panStart = null;
      if (e.touches.length === 1 && st.scale > ZOOMED_AT) {
        const f = measure();
        if (f) armPan(e.touches[0], f);
      }
      return;
    }
    // 手都抬起来了。捏过头（比 1 倍还小）就归位 —— 这是「缩回原样」唯一的路。
    if (st.scale < 1) apply({ ...FIT });
    else if (frame) apply(clampZoom(st, frame));
    mode = 'idle';
    frame = null;
    pinchStart = null;
    panStart = null;
  }

  function onCancel() {
    mode = 'idle';
    frame = null;
    pinchStart = null;
    panStart = null;
    if (st.scale < 1) apply({ ...FIT });
  }

  surface.addEventListener('touchstart', onStart, { passive: true });
  surface.addEventListener('touchmove', onMove, { passive: false });
  surface.addEventListener('touchend', onEnd, { passive: true });
  surface.addEventListener('touchcancel', onCancel, { passive: true });

  return {
    update(next: ZoomOptions) {
      if (next.resetKey !== key) {
        key = next.resetKey;
        apply({ ...FIT });
      }
      opts = next;
    },
    destroy() {
      surface.removeEventListener('touchstart', onStart);
      surface.removeEventListener('touchmove', onMove);
      surface.removeEventListener('touchend', onEnd);
      surface.removeEventListener('touchcancel', onCancel);
    },
  };
}
