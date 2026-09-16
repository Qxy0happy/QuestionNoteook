// 查看题图时的去阴影（平场校正 / flat-field correction）。
//
// 想法只有一句：**纸面本该是均匀的，某处发暗就说明那里光照弱**。于是
//     光照 ≈ 大半径模糊（图里的低频成分）
//     反射率 = 原图 / 光照
// 阴影、手挡出来的暗角、灯光不均，全都在这一个除法里被约掉，剩下的是纸和字本身。
//
// 为什么放在前端而不是 Go：票据 17 用真实题图（2903×470，136 万像素）实测过，
// Go 侧光是 PNG 解码中位就 91ms、最慢 112ms —— 一条 100ms 的预算，解码先吃掉九成。
// 而这条链上的解码**本来就不必由 Go 做**：QuestionImage 回的是图片原样的字节，
// 浏览器解 PNG 是原生实现。这里拿到的像素已经解码好了，这一趟只剩乘除。
//
// 为什么是「看法」不是「改图」：spec 的 Out of Scope 写着不做不可逆增强，
// ADR-0007 说原图处理完就丢。所以这里只改写调用方递进来的这一份像素副本
// （画布上的），库里那张 PNG 一个字节都没动过 —— 关掉开关就是原图。
//
// 这个模块刻意**不碰 DOM、不碰 canvas**：入参只是一块 RGBA 像素。所以它能在
// Node 里用同样的代码量出耗时（见票据 17 的汇报），不必真机、不必浏览器。

/** 一块 RGBA 像素。`ImageData` 天然满足它；Node 里手搓一个 {data,width,height} 也行。 */
export interface Raster {
  data: Uint8ClampedArray;
  width: number;
  height: number;
}

export interface EnhanceOptions {
  /** 光照估计的下采样倍数：8 = 缩到 1/8 再估。越小越准、越慢。 */
  downscale?: number;
  /**
   * 模糊半径占**小图**短边的比例。注意这是缩过之后的比例，不是原图的：0.12 在
   * 2903×470 上算出半径 7（小图像素），乘回 8 倍 = 56 个全图像素，约等于全图短边的 12%。
   */
  radiusRatio?: number;
  /**
   * 纸面归一化到的亮度（0-255）。**不取 255**：光照是估出来的，总有百分之几的误差，
   * 目标定在 255 就会把纸面的纹理一路削平成纯白。实测（合成图，纸纹 ±6）232 在
   * 136 万像素上削顶 0%、在手机工作尺寸（1008×163）上 0.4%；定到 244 时后者削顶 22%。
   */
  paper?: number;
  /**
   * 增益上限。暗到一定程度就不再提亮，宁可留一点阴影也别把纸纹噪点放大成一片麻子。
   * 3.2 意味着光照低于 paper/3.2 ≈ 72（约比纸面暗 3.3 倍）的地方只做部分校正 ——
   * 这是**故意留着的上限**，不是没做完：再往下的除法会逼近「除以接近 0 的数」。
   */
  maxGain?: number;
}

/**
 * 默认参数。这几个数就是下面「参数为什么这么取」那一串注释的落点，
 * 单独导出来是为了汇报和复现实验时不用把数字抄第二遍。
 */
export const DEFAULTS = {
  downscale: 8,
  radiusRatio: 0.12,
  paper: 232,
  maxGain: 3.2,
} as const;

/** 半径的夹取范围（小图像素）。下限 1 是因为半径 0 等于没有光照估计；上限是防超大图把
 *  小图模糊成一块常数（那样整幅图会被拉成纯白）。 */
const MIN_RADIUS = 1;
const MAX_RADIUS = 32;

/**
 * 就地增强一块 RGBA 像素。
 *
 * 就地而不是返回新数组：调用方拿到的是画布上那份 `ImageData`，原图另有一份
 * （data URL）在，改这份副本不会污染原图，省掉一次 4 字节/像素的分配。
 */
export function enhanceInPlace(img: Raster, options: EnhanceOptions = {}): void {
  const { data, width: w, height: h } = img;
  // 太小的图没有「低频」可言：估出来的光照就是它自己，除完只剩一片白。
  if (w < 16 || h < 16) return;

  const downscale = Math.max(2, Math.round(options.downscale ?? DEFAULTS.downscale));
  const paper = options.paper ?? DEFAULTS.paper;
  const maxGain = options.maxGain ?? DEFAULTS.maxGain;

  const sw = Math.max(1, Math.round(w / downscale));
  const sh = Math.max(1, Math.round(h / downscale));

  // ── 1) 缩到小图 ──
  // 用块平均而不是隔点抽样：题图是有纸纹和高频噪点的，隔点抽会把它们带进光照估计，
  // 除完就在白纸上留下一层斑。块平均本身就是低通，顺带把这一步做了。
  const small = new Float32Array(sw * sh * 3);
  downscaleRGB(data, w, h, small, sw, sh);

  // ── 2) 在小图上膨胀 + 模糊，估出「这一带的纸有多亮」 ──
  //
  // 半径按小图短边取比例而不是按长边：题图常见 2903×470 这种长条，按长边算出来的
  // 半径在高这个方向上大得离谱（比整幅图还高），光照会被抹成常数。
  const rawRadius = Math.round(Math.min(sw, sh) * (options.radiusRatio ?? DEFAULTS.radiusRatio));
  const radius = Math.min(MAX_RADIUS, Math.max(MIN_RADIUS, rawRadius));
  // 膨胀半径：至少 1（不然等于没做），封顶 4 —— 再大就轮到模糊去管了。
  const grow = Math.min(4, Math.max(1, Math.round(radius * 0.3)));

  const illum = new Uint8Array(sw * sh * 3);
  const tmp = new Float32Array(sw * sh);
  const tmp2 = new Float32Array(sw * sh);
  for (let c = 0; c < 3; c++) illumPlane(small, c, sw, sh, radius, grow, tmp, tmp2, illum);

  // ── 3) 查表做逐像素那一趟 ──
  //
  // 增益只取决于光照值（out = in × paper / illum），而光照量化成 8 位绰绰有余 ——
  // 1/255 的光照误差换到增益上是看不出来的。于是把两个 8 位量拍成一张 64KB 的表，
  // 内层循环就退化成「移位 + 或 + 查表 + 存」，一次除法、一次分配都没有。
  // 这比 `in * (paper / illum)` 快，不只是因为省了除法，还因为不用在循环里做夹取 ——
  // 表里的值本来就是合法字节。
  const lut = new Uint8Array(256 * 256);
  for (let l = 0; l < 256; l++) {
    // illum = 0（全黑区域）会让增益发散，所以先夹到 1；真到了那一步，maxGain 会兜住。
    let gain = paper / (l < 1 ? 1 : l);
    if (gain > maxGain) gain = maxGain;
    const base = l << 8;
    for (let v = 0; v < 256; v++) {
      const o = Math.round(v * gain);
      lut[base + v] = o > 255 ? 255 : o;
    }
  }

  // 放大用的采样表：每个输出列/行落在小图的哪两格、权重多少（0-256 的定点整数，
  // 循环里不出现浮点）。+0.5 / -0.5 是把「格子中心」对齐，否则整幅光照会偏半格，
  // 边缘会出现一条假暗边。
  const xs = buildAxis(w, sw);
  const ys = buildAxis(h, sh);

  // 放大**分两段**做，而不是每个输出像素现插一次。双线性是可分离的，而横向插值的结果
  // 与 y 无关 —— 小图第 j 行放大到整宽之后，会被下面「一个格高」的那几个输出行共用。
  // 先把它算一次（工作量是 sh × w，只有全图的 1/8 行），输出行那一趟就只剩纵向一插。
  //
  // 这是整条链上最值钱的一处优化，因为瓶颈原本就在放大、不在查表应用：
  // 单独量过（136 万像素、桌面 Node），「双线性放大三通道光照」那一趟要 19.3ms，
  // 而「查表应用」那一趟只要 4.5ms。改成两段之后，整条 enhanceInPlace 在同样大小的
  // 合成图上从 33.4ms 降到 12.1ms（中位，同一台机器、同一份数据）。
  //
  // 三个通道分开存：输出行那一趟就能顺序读，不必跨步。
  const rowsR = new Uint8Array(sh * w);
  const rowsG = new Uint8Array(sh * w);
  const rowsB = new Uint8Array(sh * w);
  for (let j = 0; j < sh; j++) {
    const s0 = j * sw;
    const d0 = j * w;
    for (let x = 0; x < w; x++) {
      const ax = xs.i0[x];
      const bx = xs.i1[x];
      const t = xs.t[x];
      const it = 256 - t;
      const p = (s0 + ax) * 3;
      const q = (s0 + bx) * 3;
      rowsR[d0 + x] = (illum[p] * it + illum[q] * t) >> 8;
      rowsG[d0 + x] = (illum[p + 1] * it + illum[q + 1] * t) >> 8;
      rowsB[d0 + x] = (illum[p + 2] * it + illum[q + 2] * t) >> 8;
    }
  }

  // 第二段：纵向插 + 立即查表应用。中间不再落一份「整行光照」，省一次往返。
  // 插值结果必落在两个端点之间（权重和为 256），所以恒在 0-255，表索引不会越界；
  // `>>` 对负差是算术移位，端点谁大谁小都对。
  for (let y = 0; y < h; y++) {
    const ty = ys.t[y];
    const a0 = ys.i0[y] * w;
    const b0 = ys.i1[y] * w;
    let o = y * w * 4;
    for (let x = 0; x < w; x++, o += 4) {
      const ar = rowsR[a0 + x];
      const br = rowsR[b0 + x];
      const ag = rowsG[a0 + x];
      const bg = rowsG[b0 + x];
      const ab = rowsB[a0 + x];
      const bb = rowsB[b0 + x];
      // 题图是 PNG，可能是带透明区的：alpha 原样带过去 —— 想当然写成 255
      // 会把透明底变成白底，在深色界面上看着像换了一张图。
      const alpha = data[o + 3];
      data[o] = lut[((ar + (((br - ar) * ty) >> 8)) << 8) | data[o]];
      data[o + 1] = lut[((ag + (((bg - ag) * ty) >> 8)) << 8) | data[o + 1]];
      data[o + 2] = lut[((ab + (((bb - ab) * ty) >> 8)) << 8) | data[o + 2]];
      data[o + 3] = alpha;
    }
  }
}

/** 每个输出坐标对应小图里的哪两格、权重多少。 */
interface Axis {
  i0: Int32Array;
  i1: Int32Array;
  /** 0-256 的定点权重：`(a*(256-t) + b*t) >> 8`。 */
  t: Int32Array;
}

function buildAxis(outLen: number, smallLen: number): Axis {
  const i0 = new Int32Array(outLen);
  const i1 = new Int32Array(outLen);
  const t = new Int32Array(outLen);
  const scale = smallLen / outLen;
  for (let i = 0; i < outLen; i++) {
    const u = (i + 0.5) * scale - 0.5;
    const f = Math.floor(u);
    const frac = u - f;
    const a = f < 0 ? 0 : f > smallLen - 1 ? smallLen - 1 : f;
    const b = f + 1 < 0 ? 0 : f + 1 > smallLen - 1 ? smallLen - 1 : f + 1;
    i0[i] = a;
    i1[i] = b;
    t[i] = Math.round(frac * 256);
  }
  return { i0, i1, t };
}

/**
 * 缩到小图，块平均。结果是**交错**的三通道 Float32（不是 RGBA）——
 * 光照估计只需要颜色，alpha 在这儿没有意义。
 */
function downscaleRGB(
  src: Uint8ClampedArray,
  w: number,
  h: number,
  dst: Float32Array,
  sw: number,
  sh: number,
): void {
  const xw = w / sw;
  const yh = h / sh;
  for (let sy = 0; sy < sh; sy++) {
    const y0 = Math.floor(sy * yh);
    // 边界那一格可能不满，按实际覆盖到的像素数平均；也保证至少有 1 行，
    // 免得 sh > h（图极小时）除出 0 来。
    const y1 = Math.min(h, Math.max(y0 + 1, Math.floor((sy + 1) * yh)));
    for (let sx = 0; sx < sw; sx++) {
      const x0 = Math.floor(sx * xw);
      const x1 = Math.min(w, Math.max(x0 + 1, Math.floor((sx + 1) * xw)));
      let r = 0;
      let g = 0;
      let b = 0;
      let n = 0;
      for (let y = y0; y < y1; y++) {
        let o = (y * w + x0) << 2;
        for (let x = x0; x < x1; x++, o += 4) {
          r += src[o];
          g += src[o + 1];
          b += src[o + 2];
          n++;
        }
      }
      const d = (sy * sw + sx) * 3;
      const inv = 1 / n;
      dst[d] = r * inv;
      dst[d + 1] = g * inv;
      dst[d + 2] = b * inv;
    }
  }
}

/**
 * 把 `small` 的第 c 个通道估成一张 8 位光照平面：先膨胀（取局部最大），再模糊。
 *
 * **膨胀这一步不能省。** 直接拿局部平均当光照，字就会把平均值拉低 —— 而后面是拿
 * 它做**分母**的，有字的地方分母偏小、就被多提亮。实测（合成图 2903×470、正文覆盖率 10%）
 * 局部平均那一版会把 87% 的纸面顶到 255 削平：纸面一丝纹理不剩，浅灰的铅笔字迹也一起没了；
 * 加膨胀之后同一张图削顶 0%（纸面 sd 从 33.7 降到 6.5）。
 * 墨总是比纸暗，所以「这一带最亮的东西」才是纸有多亮 —— 膨胀就是把字从估计里抹掉。
 *
 * 模糊分两趟（先横后纵），每趟都是滑动窗口的加减法 —— 复杂度与半径无关，
 * 「半径取大一点」在这里几乎是免费的（票据 17 在 Go 侧量到同一件事：2% 与 8% 半径
 * 的耗时只差 2ms）。
 */
function illumPlane(
  small: Float32Array,
  c: number,
  sw: number,
  sh: number,
  radius: number,
  grow: number,
  tmp: Float32Array,
  tmp2: Float32Array,
  out: Uint8Array,
): void {
  // 先把第 c 个通道抽成一张单通道平面。交错着做滑动窗口要跨步读，慢且没必要 ——
  // 这只是 1/64 大小的图，抽一次的开销可以忽略。
  const plane = new Float32Array(sw * sh);
  for (let i = 0, j = c; i < plane.length; i++, j += 3) plane[i] = small[j];

  dilateX(plane, tmp, sw, sh, grow);
  dilateY(tmp, tmp2, sw, sh, grow);
  blurX(tmp2, tmp, sw, sh, radius);
  blurY(tmp, plane, sw, sh, radius);

  // 量化回 8 位：下游的查表要整数光照，而 1/255 的误差在增益上不可见。
  for (let i = 0; i < plane.length; i++) {
    const v = Math.round(plane[i]);
    out[i * 3 + c] = v < 0 ? 0 : v > 255 ? 255 : v;
  }
}

/**
 * 横向一维膨胀（局部取最大）。朴素实现、O(N·r) —— 但 N 是缩过 8 倍的小图，
 * r 又只有 1-4，这点开销在整条链上量不出来，不值得为它上单调队列。
 */
function dilateX(src: Float32Array, dst: Float32Array, w: number, h: number, r: number): void {
  for (let y = 0; y < h; y++) {
    const base = y * w;
    for (let x = 0; x < w; x++) {
      const lo = x - r > 0 ? x - r : 0;
      const hi = x + r < w ? x + r : w - 1;
      let m = src[base + lo];
      for (let i = lo + 1; i <= hi; i++) {
        const v = src[base + i];
        if (v > m) m = v;
      }
      dst[base + x] = m;
    }
  }
}

/** 纵向一维膨胀。 */
function dilateY(src: Float32Array, dst: Float32Array, w: number, h: number, r: number): void {
  for (let x = 0; x < w; x++) {
    for (let y = 0; y < h; y++) {
      const lo = y - r > 0 ? y - r : 0;
      const hi = y + r < h ? y + r : h - 1;
      let m = src[lo * w + x];
      for (let i = lo + 1; i <= hi; i++) {
        const v = src[i * w + x];
        if (v > m) m = v;
      }
      dst[y * w + x] = m;
    }
  }
}

/** 横向一维滑动窗口均值。边界按「复制边缘」处理。 */
function blurX(src: Float32Array, dst: Float32Array, w: number, h: number, r: number): void {
  const inv = 1 / (2 * r + 1);
  for (let y = 0; y < h; y++) {
    const base = y * w;
    // 窗口在 x=0 处是 [-r, r]，越界的一律用边缘像素顶上。**这一步不能省**：
    // 若把越界当成 0，图像四周会凭空多出一圈暗边，而它会被后面的除法
    // 当成「那里光照弱」放大成一条明显的亮边 —— 模板自己造的假阴影。
    let sum = src[base] * (r + 1);
    for (let i = 1; i <= r; i++) sum += src[base + (i < w ? i : w - 1)];
    for (let x = 0; x < w; x++) {
      dst[base + x] = sum * inv;
      const drop = x - r > 0 ? x - r : 0;
      const add = x + r + 1 < w ? x + r + 1 : w - 1;
      sum += src[base + add] - src[base + drop];
    }
  }
}

/** 纵向一维滑动窗口均值。与 blurX 同一套，只是跨行取。 */
function blurY(src: Float32Array, dst: Float32Array, w: number, h: number, r: number): void {
  const inv = 1 / (2 * r + 1);
  for (let x = 0; x < w; x++) {
    let sum = src[x] * (r + 1);
    for (let i = 1; i <= r; i++) sum += src[(i < h ? i : h - 1) * w + x];
    for (let y = 0; y < h; y++) {
      dst[y * w + x] = sum * inv;
      const drop = y - r > 0 ? y - r : 0;
      const add = y + r + 1 < h ? y + r + 1 : h - 1;
      sum += src[add * w + x] - src[drop * w + x];
    }
  }
}
