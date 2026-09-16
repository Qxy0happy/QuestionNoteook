<script module lang="ts">
  // 归一化选框：四个数都在 0..1，相对**帧**（不是屏幕）。
  //
  // 用帧坐标而不是屏幕坐标有两个好处：帧在屏幕上缩放了多少、有没有留黑边，
  // 都不必进状态；旋转底图时选框也能跟着内容一起转（见 Capture.svelte 的 rotate）。
  export type Box = { x: number; y: number; w: number; h: number };

  // 把 v 收进 [lo, hi]。
  //
  // 放这里跟着 Box 一起导出，是因为 Capture.svelte 那侧也要**同一个动作**：把归一化的选框
  // 换算成像素下标时，四舍五入会把右边推到帧外一两个像素，得夹回来。两份逐字相同的实现
  // 没有理由各留一份，而这两个文件已经靠 Box 共享同一套几何了。
  //
  // 注意它同时吃小数与整数 —— 两边传的其实都是 number（归一化的比例、以及取整后的下标），
  // 所以不必像以前那样在 Capture 那边叫 clampInt，那名字反而说错了。
  export function clamp(v: number, lo: number, hi: number): number {
    return v < lo ? lo : v > hi ? hi : v;
  }
</script>

<script lang="ts">
  // 手选框。四个角柄改尺寸、框内平移，结果永远是轴对齐的矩形 —— 用户要的是矩形，
  // 不是四边形（四角独立拖是 ADR-0006 之后的事，那时只需把角点逐个接出去）。
  //
  // 本组件只管手势与画法，不知道帧多大、也不碰像素：它把结果写回绑定的 Box。

  interface Props {
    // 帧在舞台上实际占的那块矩形（CSS px，相对本组件的定位祖先）。
    // 留边是调用方按自己的铺法算好传进来的 —— 选框只认这一个矩形。
    rect: { left: number; top: number; width: number; height: number };
    value: Box;
  }

  let { rect, value = $bindable() }: Props = $props();

  // 选框在屏幕上最小也得有这么大：再小手指既按不住角柄，也看不出来框了什么。
  const MIN_PX = 56;

  let host = $state<HTMLElement | null>(null);

  type Corner = 'nw' | 'ne' | 'se' | 'sw';
  type Kind = 'move' | Corner;

  // 顺序只是画出来的顺序，与角点的语义无关 —— 选框始终是轴对齐的矩形，
  // 不存在「哪个角对应哪个像素」这回事（四角独立拖是 ADR-0006 之后的事）。
  const CORNERS: Corner[] = ['nw', 'ne', 'se', 'sw'];
  const CORNER_LABELS: Record<Corner, string> = {
    nw: '左上角',
    ne: '右上角',
    se: '右下角',
    sw: '左下角',
  };

  // 拖拽中的快照：起点与**起始**选框。每次 move 都从快照重算，不做增量累加 ——
  // 增量会被每一次舍入咬掉一点，拖久了选框会漂。
  // 它不参与渲染，所以用普通变量而不是 $state。
  let drag: { kind: Kind; px: number; py: number; from: Box } | null = null;

  // 归一化 → 屏幕。只用来画。
  const screen = $derived({
    left: rect.left + value.x * rect.width,
    top: rect.top + value.y * rect.height,
    width: value.w * rect.width,
    height: value.h * rect.height,
  });

  // clamp 在 module 段里（那里跟着 Box 导出，Capture.svelte 也要它）——
  // 实例段与 module 段编译在同一个模块作用域里，这里直接就能用，别再抄一份。

  function begin(e: PointerEvent, kind: Kind) {
    // 尺寸还没量出来（首帧）时 rect 是零宽，除下去全是 NaN，干脆不动。
    if (!host || rect.width < 1 || rect.height < 1) return;
    // 指针捕获挂在容器上：手指滑出选框、滑出屏幕，move 事件照样回得来。
    host.setPointerCapture(e.pointerId);
    drag = { kind, px: e.clientX, py: e.clientY, from: { ...value } };
    e.preventDefault();
  }

  // 纯函数：起始选框 + 这次拖了多远 → 新选框。
  // 四条边各自定死或跟着走，每个分支都同时给出 x/w 与 y/h，所以结果必然还是矩形。
  function apply(b: Box, kind: Kind, dx: number, dy: number, minW: number, minH: number): Box {
    // 拖角时**对面**那条边是锚，先把锚记下来。
    const right = b.x + b.w;
    const bottom = b.y + b.h;

    switch (kind) {
      case 'move':
        // 平移：整框走，撞到帧边就停。
        return { x: clamp(b.x + dx, 0, 1 - b.w), y: clamp(b.y + dy, 0, 1 - b.h), w: b.w, h: b.h };

      case 'nw': {
        // 右下角是锚。
        const x = clamp(b.x + dx, 0, Math.max(0, right - minW));
        const y = clamp(b.y + dy, 0, Math.max(0, bottom - minH));
        return { x, y, w: right - x, h: bottom - y };
      }

      case 'ne': {
        // 左下角是锚。
        const y = clamp(b.y + dy, 0, Math.max(0, bottom - minH));
        const r = clamp(right + dx, b.x + minW, 1);
        return { x: b.x, y, w: r - b.x, h: bottom - y };
      }

      case 'se': {
        // 左上角是锚。
        const r = clamp(right + dx, b.x + minW, 1);
        const btm = clamp(bottom + dy, b.y + minH, 1);
        return { x: b.x, y: b.y, w: r - b.x, h: btm - b.y };
      }

      default: {
        // sw：右上角是锚。
        const x = clamp(b.x + dx, 0, Math.max(0, right - minW));
        const btm = clamp(bottom + dy, b.y + minH, 1);
        return { x, y: b.y, w: right - x, h: btm - b.y };
      }
    }
  }

  function move(e: PointerEvent) {
    const d = drag;
    if (!d || rect.width < 1 || rect.height < 1) return;
    // 屏幕上走了多少像素 → 归一化走了多少。
    const dx = (e.clientX - d.px) / rect.width;
    const dy = (e.clientY - d.py) / rect.height;
    const minW = Math.min(MIN_PX / rect.width, 1);
    const minH = Math.min(MIN_PX / rect.height, 1);
    value = apply(d.from, d.kind, dx, dy, minW, minH);
  }

  function end() {
    drag = null;
  }
</script>

<div
  class="crop"
  bind:this={host}
  role="application"
  aria-label="选框：拖四角改尺寸，拖中间平移"
  style="left: {screen.left}px; top: {screen.top}px; width: {screen.width}px; height: {screen.height}px;"
  onpointerdown={(e) => begin(e, 'move')}
  onpointermove={move}
  onpointerup={end}
  onpointercancel={end}
>
  <!-- 角柄是框的子元素，pointerdown 必须截住，否则会先被容器当成「拖内部平移」。 -->
  {#each CORNERS as corner (corner)}
    <button
      type="button"
      class="handle {corner}"
      aria-label={CORNER_LABELS[corner]}
      onpointerdown={(e) => {
        e.stopPropagation();
        begin(e, corner);
      }}
    ></button>
  {/each}
</div>

<style>
  .crop {
    position: absolute;
    /* 描边宽度只在这里出现一次：下面四个角柄要靠它把圆心摆到描边外沿上。 */
    --edge: 2px;
    /* 描边算进宽高里，于是**看得见的这一圈**就是裁出来的那一块。
       默认的 content-box 会把边框画在指定尺寸之外，白框与实裁就差了两像素。 */
    box-sizing: border-box;
    border: var(--edge) solid rgba(255, 255, 255, 0.92);
    /* 框外压暗：一圈无穷大的外阴影就是现成的蒙版，不必再造四个遮罩块。
       阴影画在自身内容之下，所以四个角柄不会被它盖住；
       而阴影不是元素、不拦指针 —— 框外那块仍然可以横滑翻页。 */
    box-shadow: 0 0 0 100vmax rgba(0, 0, 0, 0.5);
    /* 拖选框时不能顺手把翻页的横滑手势也触发掉。 */
    touch-action: none;
    cursor: move;
    user-select: none;
    -webkit-user-select: none;
    /* 桌面端别把这当成拖窗口。 */
    --wails-draggable: no-drag;
  }

  .handle {
    position: absolute;
    /* 命中区比看得见的点大一圈：手指的落点总比眼睛以为的偏，宁可比它大。 */
    width: 2.75rem;
    height: 2.75rem;
    /* 它是 <button> 只是为了有个可被指到的身份，样子全靠下面那圈点。 */
    padding: 0;
    border: 0;
    background: none;
    touch-action: none;
    -webkit-tap-highlight-color: transparent;
    --wails-draggable: no-drag;
  }

  .handle::after {
    content: '';
    position: absolute;
    top: 50%;
    left: 50%;
    translate: -50% -50%;
    width: 1.25rem;
    height: 1.25rem;
    border: 2px solid rgba(255, 255, 255, 0.95);
    border-radius: 50%;
    background: rgba(0, 0, 0, 0.35);
    box-shadow: 0 0 3px rgba(0, 0, 0, 0.7);
  }

  /* 四个角柄都骑在角上，一半在外一半在内。
     绝对定位的偏移量是相对**内边距盒**算的，所以要从描边外沿起步得往回让一个 --edge。 */
  .nw {
    left: calc(-1 * var(--edge));
    top: calc(-1 * var(--edge));
    translate: -50% -50%;
    cursor: nwse-resize;
  }
  .ne {
    right: calc(-1 * var(--edge));
    top: calc(-1 * var(--edge));
    translate: 50% -50%;
    cursor: nesw-resize;
  }
  .se {
    right: calc(-1 * var(--edge));
    bottom: calc(-1 * var(--edge));
    translate: 50% 50%;
    cursor: nwse-resize;
  }
  .sw {
    left: calc(-1 * var(--edge));
    bottom: calc(-1 * var(--edge));
    translate: -50% 50%;
    cursor: nesw-resize;
  }
</style>
