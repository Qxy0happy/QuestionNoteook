// 「增强」（去阴影）那一层的状态：算多大一张、什么时候重算、blob 什么时候放掉。
//
// 为什么单独一个模块：题库详情与复习页都要这个开关，而这里面有一批**很容易抄走样**的规矩 ——
// 按显示尺寸算（票据 17 省时间的全部来源）、放大之后换一版按原图算的、异步回来对不上号就丢掉、
// 旧的那张要在 DOM 换过 src **之后**才 revoke。抄一份就是两处会走样，而且这些走样都不会报错。
//
// 纯算法在 enhance.ts（那边不碰 DOM、不碰 canvas，能脱离浏览器单独量）；这里管的是
// 「一张图变成一个看法」的那条链：原生解码 → 缩到目标尺寸的画布 → enhance.ts 改像素 →
// 编码回 PNG → 交给 <img>。
//
// 一个实例管**两张图**（题图与答案图）：界面上是一个开关管两张（「对着看的两张该是同一个看法」），
// 毫秒数也只有一个。
//
// 库里那张 PNG 一个字节都没动过（ADR-0007 说原图处理完就丢了，spec 的 Out of Scope 写着
// 不做不可逆增强）。所以「关掉立刻回到原图」不是「反过来再算一遍」，而是根本不碰它 ——
// <img> 的 src 换回原图那个 URL，逐字节相同。
import { tick } from 'svelte';
import { enhanceInPlace } from './enhance';

/** 两个槽位：题图与答案图。 */
export type Slot = 'q' | 'a';

export interface ShotSource {
  /** 原图的 URL（data URL 或 blob URL）。 */
  url: string;
  /** 这块图在屏幕上占的 CSS 像素尺寸。决定这一版算多大（见 capFor）。 */
  boxW: number;
  boxH: number;
  /** 读原图尺寸用的 <img>：放大换档时要按原图算一版。没有就退回 box。 */
  el?: HTMLImageElement | null;
}

// 画布像素总量的兜底闸。正常路径由块的尺寸封顶（手机上一千出头宽，几十万像素），
// 这条只防「块报了个很大的尺寸」这种边角情况 —— 画布与光照缓冲都是按像素走的。
const MAX_PIXELS = 4_000_000;

// 目标画布尺寸。上限是「这张图在屏幕上真正占的设备像素」：手机上一块图大约 350 CSS px 宽、
// DPR 3，也就是一千出头的设备像素 —— 而题图原始有 2903 宽，按原图算等于把四分之三的像素
// 算完再丢掉，屏幕上一点看不出差别。块还没量出来（首帧为 0）就退回原图尺寸：宁可多算，
// 也别缩成一张 1px 的图。
function capFor(nw: number, nh: number, boxW: number, boxH: number) {
  const dpr = window.devicePixelRatio || 1;
  const capW = boxW > 0 ? boxW * dpr : nw;
  const capH = boxH > 0 ? boxH * dpr : nh;
  const scale = Math.min(1, capW / nw, capH / nh, Math.sqrt(MAX_PIXELS / (nw * nh)));
  return { w: Math.max(1, Math.round(nw * scale)), h: Math.max(1, Math.round(nh * scale)) };
}

// 这一版该按哪个尺寸算：full 为真就按**原图**（用户放大过，屏幕那么大的一版不够看了）。
//
// 注意传的是「框」而不是「尺寸」：capFor 会把框乘上 DPR 再与原图比，所以传原图尺寸进去
// 等于要 1:1 —— 上限仍然由 MAX_PIXELS 兜着。
function boxFor(full: boolean, el: HTMLImageElement | null | undefined, w: number, h: number): [number, number] {
  if (full && el && el.naturalWidth > 0) return [el.naturalWidth, el.naturalHeight];
  return [w, h];
}

// 把一张图算成增强版，返回能直接塞进 <img> 的 blob URL 与这一趟的毫秒数。
//
// 最后那个 PNG 编码是这条链上**唯一躲不掉又没法在 Node 里量的一步**：toBlob 在 Blink 上走
// 后台线程，主线程不会被它卡住，但手机上到底多少毫秒只能真机测。编码本身不能省 —— 要把一块
// 像素交给 <img> 显示，只有 data URL / blob URL 两条路，两条都得先编码。
async function renderEnhanced(
  src: ShotSource,
  full: boolean,
): Promise<{ url: string; ms: number; algoMs: number }> {
  // 从「点下去」到「能显示」的整段，与里面的算法那一趟分开计。用户问的是「100ms 内做得到吗」，
  // 那问的是整段；而这个数会跟着设备变，只能真机上读。
  const t0 = performance.now();
  const im = new Image();
  im.src = src.url;
  // decode() 等的是「解码真的完成」。src 是刚取回来的，浏览器内存缓存里通常已经有这一份
  // （原图那张 <img> 刚显示过），所以这一步多数时候是白拿的。
  await im.decode();

  const [boxW, boxH] = boxFor(full, src.el, src.boxW, src.boxH);
  const { w, h } = capFor(im.naturalWidth, im.naturalHeight, boxW, boxH);
  const canvas = document.createElement('canvas');
  canvas.width = w;
  canvas.height = h;
  // willReadFrequently：下面就是一次 getImageData + 一次 putImageData。
  // 让画布落在 CPU 侧，省掉 GPU 回读那一趟停顿（一次性的读写不值得走 GPU）。
  const ctx = canvas.getContext('2d', { willReadFrequently: true });
  if (!ctx) throw new Error('拿不到画布上下文');
  ctx.drawImage(im, 0, 0, w, h);

  const pixels = ctx.getImageData(0, 0, w, h);
  const t1 = performance.now();
  enhanceInPlace(pixels);
  const algoMs = performance.now() - t1;
  ctx.putImageData(pixels, 0, 0);

  const url = await new Promise<string>((resolve, reject) => {
    canvas.toBlob(
      (b) => (b ? resolve(URL.createObjectURL(b)) : reject(new Error('增强后的图编码失败'))),
      'image/png',
    );
  });
  return { url, ms: performance.now() - t0, algoMs };
}

/** 一对图的「增强后的那一版」。用法：`const shots = new EnhancedShots()`。 */
export class EnhancedShots {
  /** 开关。题库详情每换一道题就关掉（原图是基准），复习页整段复习一直开着。 */
  enabled = $state(false);
  /** 增强后的题图 / 答案图。null = 还没算好，这时 <img> 显示原图（不闪白）。 */
  q = $state<string | null>(null);
  a = $state<string | null>(null);
  /** 这一趟量到的毫秒数：整段，与其中的纯算法那一趟（设备无关的那个数）。 */
  ms = $state(0);
  algoMs = $state(0);
  /** 算不出来时的一句话（显示原图，但别让按钮亮着却什么都没变）。 */
  error = $state('');

  /** 每个槽位单独记「这一版是按原图算的」——放大换档用，换过就不降级。 */
  #full: Record<Slot, boolean> = { q: false, a: false };
  /** 每次重算换一个号；异步回来的结果对不上号就丢掉（用户可能在这中间关了开关、换了题）。 */
  #token = 0;

  /** 拨一下开关。关掉立刻把两张放掉（显示回到原图），档位也退回默认。 */
  toggle(): void {
    this.enabled = !this.enabled;
    this.error = '';
    if (!this.enabled) {
      this.ms = 0;
      this.algoMs = 0;
      this.#release();
    }
  }

  /** 换一道题、且**关掉**开关：题库详情用这个（原图是基准，增强是这一次临时打开的看法）。 */
  resetForNewQuestion(): void {
    this.enabled = false;
    this.ms = 0;
    this.algoMs = 0;
    this.error = '';
    this.#release();
  }

  /** 换一道题、但**保留**开关：复习页用这个（一口气做十几道，每题重按一次会很烦）。 */
  resetKeepingEnabled(): void {
    this.error = '';
    this.#release();
  }

  /**
   * 放大之后把某个槽位换成按**原图**算的那一版，返回「调用方要不要重算一遍」。
   *
   * 为什么非换不可：按显示尺寸算的那版**只有屏幕那么大**（那是省时间的全部来源），
   * 放大两三倍看到的就是插值的糊 —— 而那正是用户放大要避开的东西。
   * 每个槽位只换一次，换过不降级（缩回去看只会更清楚，也省一次重算）。
   *
   * 开关关着时返回 false，但**记住**这一档 —— 等他真打开时就直接按原图算。
   */
  upgrade(slot: Slot): boolean {
    if (this.#full[slot]) return false;
    this.#full[slot] = true;
    return this.enabled;
  }

  /**
   * 按当前状态把两张图重算一遍。sources 里给了哪个槽位就算哪个，没给（或给 null）的那个
   * 会被放掉。触发点：拨开关、开一道题、摊开答案图、放大换档。
   */
  async refresh(sources: Partial<Record<Slot, ShotSource | null>>): Promise<void> {
    const token = ++this.#token;

    if (!this.enabled) {
      // 关着的时候什么都不算，只把算过的那两张放掉 —— 显示立刻回到原图。
      this.#release();
      return;
    }

    for (const slot of ['q', 'a'] as const) {
      const src = sources[slot];
      if (!src) {
        // 这一槽没了（收起答案图、换了题）：放掉，并把「按原图算」那一档也退回默认 ——
        // 换一道题就是全新的一次查看，没道理沿用上一张的档位（那会白算一遍大图）。
        this.#full[slot] = false;
        await this.#swap(slot, null);
        continue;
      }
      try {
        const r = await renderEnhanced(src, this.#full[slot]);
        if (token !== this.#token) {
          URL.revokeObjectURL(r.url); // 这次的结果已经过时，别留在内存里
          return;
        }
        // 毫秒数只报题图那一趟：两张各报一个的话，后一个会把前一个盖掉，
        // 而用户问的「100ms 内做得到吗」问的就是这条路。
        if (slot === 'q') {
          this.ms = r.ms;
          this.algoMs = r.algoMs;
        }
        this.error = '';
        await this.#swap(slot, r.url);
      } catch (err) {
        if (token === this.#token) {
          // 算不出来就照旧显示原图，把话说出来 —— 别让按钮亮着却什么都没变。
          this.error = err instanceof Error ? err.message : String(err);
          await this.#swap(slot, null);
        }
      }
    }
  }

  /** 放掉两张、档位退回默认。不碰开关（那个由调用方按场景决定）。 */
  #release(): void {
    void this.#swap('q', null);
    void this.#swap('a', null);
    this.#full = { q: false, a: false };
  }

  // 换掉一个槽位里的 blob URL，并**在 DOM 换过 src 之后**放掉旧的那张。
  // 顺序不能反：提前 revoke，当前那张 <img> 指着的资源会被抽掉。
  // 不放也不行 —— 一张增强图是几百 KB 的 PNG，开关来回拨几十次就是几十 MB。
  async #swap(slot: Slot, url: string | null): Promise<void> {
    const old = slot === 'q' ? this.q : this.a;
    if (old === url) return;
    if (slot === 'q') this.q = url;
    else this.a = url;
    await tick();
    if (old) URL.revokeObjectURL(old);
  }
}
