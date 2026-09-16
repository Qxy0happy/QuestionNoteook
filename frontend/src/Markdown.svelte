<script module lang="ts">
  // marked 是个单例，扩展只能挂一次 —— 所以这段放 module 段（整个前端跑一次），
  // 不放实例段（那样每挂一个 Markdown 组件都会再挂一遍扩展）。
  import { marked } from 'marked';
  import markedKatex from 'marked-katex-extension';
  import 'katex/dist/katex.min.css';

  marked.use(
    markedKatex({
      // 解析不了的公式**不要抛**：模型偶尔会吐出半截 \left( 这类东西，抛出去整条消息
      // 就没了。让它把源码标红显示出来，至少还看得见原文。
      throwOnError: false,
      // 不放行 \href / \htmlClass 这类能带出 URL 或自定义属性的命令。
      // 这是 KaTeX 的默认值，但它与安全有关，写出来比依赖默认值强。
      trust: false,
      // 单 $ 也当行内公式。
      //
      // 默认那一档要求结尾的 $ 后面跟空白或标点，而**中文正文里经常不写空格**
      // （`$x^2$的最小值`），于是公式被当成纯文本漏掉 —— 那正好是这个组件要解决的
      // 问题没解决。代价是「$5 到 $10」这种会被误当公式；在错题本里很少见，
      // 而且解析失败只是把原文标红显示。
      nonStandard: true,
    }),
  );

  // ── 定界符归一：\[ ... \] / \( ... \) → $$ ... $$ / $ ... $ ──
  //
  // 为什么需要：模型经常按 LaTeX 的老写法吐公式 —— 独立成行的用 \[ ... \] 包起来，
  // 行内的用 \( ... \) —— 而上面那个扩展**只认 $ / $$**。于是那些公式一进 marked 就是
  // 一段纯文本，原样显示出来（用户报的「公式渲染不对」根因就在这儿：设备上的讨论记录里
  // 逐字就是这个形状，同一段里的 **加粗** 则是正常渲染的 —— 坏的只有公式）。
  // 这里在进 marked 之前把它们归一成扩展认的那两种写法。
  //
  // 为什么用 preprocess 钩子，而不是自己写一个认 \[ 的扩展：钩子拿到的是**整份原文**，
  // 替换规则一眼看得懂；扩展那条路要在词法层自己认定界符，认出来的还得自己拿去调
  // katex.renderToString —— 那意味着上面那三个选项（throwOnError / trust / nonStandard）
  // 要在第二个地方再抄一遍，两处迟早走偏。
  //
  // **代码里的 \[ 一个都不能动**（那是代码，不是公式）。所以不是一次全局 replace，
  // 而是走一遍原文：围栏代码块（``` / ~~~）与行内代码（反引号那一串）整段原样搬过去，
  // 4 格缩进的代码块也一样 —— 只有剩下的正文里成对的定界符才换。
  marked.use({ hooks: { preprocess: normalizeLatexDelimiters } });

  function normalizeLatexDelimiters(src: string): string {
    const out: string[] = [];
    // 连续几行「正文」攒在一起处理：\[ 与 \] 可以隔着好几行（模型就是这么写的），
    // 一行一行地换会漏掉这种。代码行不进来，所以这里不用管子块边界。
    let body: string[] = [];
    const flush = () => {
      if (body.length > 0) {
        out.push(...convertLatexDelimiters(body.join('\n')).split('\n'));
        body = [];
      }
    };

    // 代码状态：fence 非空 = 正在一个围栏代码块里（值是开栏那串字符）；indented = 正在一个
    // 4 格缩进的代码块里。两个状态互斥，谁先成立谁说了算。
    let fence: string | null = null;
    let indented = false;
    // 上一行（文件开头也算）是不是空行 —— 缩进代码块只可能从空行之后开始，见下面。
    let blankBefore = true;

    for (const line of src.split('\n')) {
      const blank = line.trim() === '';

      if (fence !== null) {
        out.push(line);
        // 收尾那一行：同一个字符、不短于开栏那串、后面只剩空白。
        if (new RegExp('^ {0,3}' + fence[0] + '{' + fence.length + ',}[ \\t]*$').test(line)) {
          fence = null;
        }
        blankBefore = blank;
        continue;
      }

      const indentedLine = /^( {4,}|\t)/.test(line);
      // 「空行之后」这一条是 CommonMark 的规矩，也是这里唯一要判的：紧跟在段落下面的 4 格
      // 缩进是**段落续行**、不是代码（模型把公式缩进 4 格写在「有：」下面正是这种）。
      // 少了这一条，那种公式会被当代码原样搬走 —— 而它正是这个组件要救回来的东西。
      if (indentedLine && blankBefore) indented = true;
      if (indented && (blank || indentedLine)) {
        out.push(line);
        blankBefore = blank;
        continue;
      }
      indented = false;

      // 围栏开栏：``` 或 ~~~（最多 3 格缩进）。它前面的正文先结掉。
      const open = /^ {0,3}(`{3,}|~{3,})/.exec(line);
      if (open) {
        flush();
        fence = open[1];
        out.push(line);
        blankBefore = blank;
        continue;
      }

      body.push(line);
      blankBefore = blank;
    }
    flush();
    return out.join('\n');
  }

  // convertLatexDelimiters 在一段**正文**里换定界符，行内代码原样留着。
  //
  // 成对才换：落单的 \[ 原样留着 —— 那时它多半是想写一个字面的方括号，换掉反而会把
  // 后面半篇都吞进一个公式里。
  //
  // **显示公式一律并成单行的 `$$ 正文 $$`**（行与行之间归一成一个空格），这不是顺手，
  // 是这条链上唯一稳的形状：$$ 的**块级**规则要求它出现在一个块的开头，而段落**不会**
  // 因为它断开。实测（marked 18 + 上面那个扩展）：
  //   有：\n$$\nx=1\n$$\n成立。      → 整段被当纯文本，一个字母都没渲染（就是用户报的那个）
  //   有：\n\n$$\nx=1\n$$\n\n成立。    → 渲染得出来（多了那个空行才成块）
  //   列表项里的多行 $$             → 渲染得出来（它就在块的开头）
  // 而**单行**的 $$ ... $$ 走的是行内规则那条路，实测在正文里、自己一行、列表项里都渲染
  // 得出来 —— 一条规则覆盖所有位置，比「看缩进、补空行」那套形状判断可靠得多。
  // 所以 \[ ... \] 与「自己占一行的 $$ ... $$」两种多行写法都归一到这个形状；
  // 本来就写成单行 $$ ... $$ 的不动它（它已经能渲染）。
  // 并行的代价在 TeX 里等于零：数学模式的空白本来就不表意（\text{...} 里连续空白也归一成
  // 一个）。真要挑刺只剩一处：公式里带 % 注释的，并行会把注释后面那一截并到同一行 ——
  // 模型在公式里写 % 注释基本不存在，这一版不为它加分支。
  //
  // 配对是拿 indexOf / 一条多行正则一路找下去的，**不跳过中间的代码块**：`\[` 与 `\]`
  // 之间夹一整个围栏代码块的写法基本不存在，真出现时这一版会把那一对换掉
  //（代码块本身仍原样保留）。
  //
  // 数学**内部**的 \[ 会被一起换掉：真在 $...$ 里写 \[ 的，是转义方括号（LaTeX 里的
  // `\left[` 写的是 \left[ 而不是 \[，不受影响），这种极少见；这一版不为此再写一层
  // 「数学感知」的扫描 —— 代价是那一种会被换成 $$，而公式多半还是照样渲染出来。
  function convertLatexDelimiters(text: string): string {
    let out = '';
    let i = 0;
    while (i < text.length) {
      // 行内代码：连着的几个反引号，到**同样多个**反引号为止（长一点短一点都不算收尾，
      // 与 markdown 的规矩一致），整段原样搬 —— 里面是代码。
      if (text[i] === '`') {
        const run = /^`+/.exec(text.slice(i))![0];
        const close = new RegExp('(?<!`)`{' + run.length + '}(?!`)').exec(text.slice(i + run.length));
        const stop = close ? i + run.length * 2 + close.index : i + run.length;
        out += text.slice(i, stop);
        i = stop;
        continue;
      }

      const pair = text.slice(i, i + 2);

      // 老写法之一：自己占一行的 $$ ... $$（开栏那一行只剩它、收栏那一行只剩它）。
      // 收栏那一行用多行正则找：^ + 空白 + $$ + 空白 + 行尾。
      if (pair === '$$' && restOfLine(text, i + 2).trim() === '') {
        const close = /^[ \t]*\$\$[ \t]*$/m.exec(text.slice(i + 2));
        if (close) {
          const at = i + 2 + close.index;
          const body = joinLines(text.slice(i + 2, at));
          // 中间什么都没有（`$$` 紧跟着 `$$`）就原样留着：并出来只会是四个 $。
          if (body !== '') {
            out += '$$' + body + '$$';
            i = at + close[0].length;
            continue;
          }
        }
      }

      // 老写法之二：\[ ... \]（显示）与 \( ... \)（行内）。
      if (pair === '\\[' || pair === '\\(') {
        const end = text.indexOf(pair === '\\[' ? '\\]' : '\\)', i + 2);
        // 成对、而且中间不是空的：`\[\]` 与落单的 `\[` 都原样留着（换出来只会是几个 $）。
        if (end > i + 2) {
          const mark = pair === '\\[' ? '$$' : '$';
          out += mark + joinLines(text.slice(i + 2, end)) + mark;
          i = end + 2;
          continue;
        }
      }

      out += text[i];
      i += 1;
    }
    return out;
  }

  // joinLines 把一段公式正文并成一行：行与行之间归一成一个空格，两头去掉空白。
  //
  // 只动换行，**不动行内的空格**：`\text{a  b}` 这种两个连续空格照旧留给 KaTeX 自己去处理。
  function joinLines(body: string): string {
    return body.replace(/\s*\n\s*/g, ' ').trim();
  }

  // restOfLine 取这一行剩下的部分（不含换行符）。
  function restOfLine(text: string, from: number): string {
    const at = text.indexOf('\n', from);
    return text.slice(from, at < 0 ? text.length : at);
  }
</script>

<script lang="ts">
  // 把模型吐回来的 markdown（含 LaTeX）变成 DOM。
  //
  // **先 markdown、再消毒**，顺序不能反：marked 会原样放行输入里的 HTML（它明确说
  // 消毒不是自己的事），链接还能带 javascript:，而模型的输出是**不可信**的 ——
  // 题图里完全可能藏着让它输出 <script> 的文字，而这个 WebView 里有 JS 桥
  // （能调到应用自己的服务）。所以进 {@html} 的每一个字节都先过 DOMPurify。
  //
  // 公式的 HTML 也在这条链上（扩展在解析时就已经把 KaTeX 渲染完了），所以要放行 MathML：
  // KaTeX 默认同时输出一份 MathML 给读屏软件，而 DOMPurify 的默认规则会把它当陌生标签
  // 摘掉。摘了不会花屏（看得见的那份是 HTML+CSS），但读屏就没声了。
  import DOMPurify from 'dompurify';

  interface Props {
    text: string;
  }

  let { text }: Props = $props();

  const html = $derived(
    DOMPurify.sanitize(marked.parse(text, { async: false }), {
      USE_PROFILES: { html: true, mathMl: true },
      // 下面两条是**实测**出来的，不是照抄文档 —— 把「markdown → KaTeX → DOMPurify」
      // 这条链在 Node 里配 jsdom 真跑了一遍（一次性核对，跑完就拆了）：
      //
      // 1) `style` **元素**会被默认放行 —— 实测 `# <style>body{display:none}</style>`
      //    原样活下来。样式是全局的，模型一句话就能把整个应用改样；更阴的是 CSS 能用
      //    属性选择器把值拼进 url()，一位一位读走 —— 而设置页那个 API Key 输入框的
      //    value 正好落在这个射程里。禁元素。
      //    但行内 `style` **属性必须留着**（也实测过）：KaTeX 的间距与垂直对齐全靠它，
      //    禁掉属性公式就散架了。这两件事同名不同物，别一起禁掉。
      // 2) 表单类也一并禁 —— `<form action="https://…">` 能把这个 WebView 导到别的页面，
      //    伪装成应用的样子。这些默认同样放行。
      FORBID_TAGS: [
        'style',
        'form',
        'input',
        'textarea',
        'select',
        'button',
        'iframe',
        'object',
        'embed',
      ],
      // MathML 里那份 TeX 原文（`<annotation>`）默认被当陌生标签摘掉，补回来 ——
      // 读屏与「复制成 MathML」要用它。摘了不花屏，但那是白丢的可访问性。
      ADD_TAGS: ['annotation'],
    }),
  );
</script>

<div class="md">{@html html}</div>

<style>
  .md {
    font-size: 0.95rem;
    line-height: 1.65;
    /* 长公式、长 URL 都不能把气泡撑宽。 */
    overflow-wrap: anywhere;
  }

  /* ⚠️ 注入进来的 DOM 不带 Svelte 的作用域类，普通选择器一个都命中不了 ——
     这一整段必须用 :global（这正是它的用途）。 */
  .md :global(p) {
    margin: 0 0 0.6em;
  }
  .md :global(p:last-child) {
    margin-bottom: 0;
  }

  .md :global(ul),
  .md :global(ol) {
    margin: 0 0 0.6em;
    padding-left: 1.3em;
  }
  .md :global(li) {
    margin: 0.15em 0;
  }

  .md :global(code) {
    padding: 0.1em 0.35em;
    border-radius: 0.3em;
    background: rgba(244, 246, 251, 0.12);
    font-size: 0.88em;
  }
  .md :global(pre) {
    margin: 0 0 0.6em;
    padding: 0.6em 0.75em;
    border-radius: 0.5em;
    background: rgba(6, 7, 15, 0.6);
    /* 代码块比气泡宽时自己滚，不把整页撑开。 */
    overflow-x: auto;
  }
  .md :global(pre code) {
    padding: 0;
    background: none;
  }

  .md :global(a) {
    color: #9fd4ff;
  }

  .md :global(blockquote) {
    margin: 0 0 0.6em;
    padding-left: 0.75em;
    border-left: 3px solid rgba(244, 246, 251, 0.2);
    color: rgba(244, 246, 251, 0.7);
  }

  /* 宽表格自己横滚，理由与上面 pre 那条一样：模型吐一张五六列的表，不能把气泡撑出屏幕。
     两句话缺一不可：
       - `overflow` 对 `display: table` 的盒子**不生效**（它不是块级容器），所以先把它
         变成 block —— 里面的行仍是匿名的表格盒，边框合并、列对齐都不受影响；
       - 变成 block 之后盒子宽度才跟着气泡走（block 撑满可用宽度），
         多出来的那部分在这一条里横滚。
     display 一改，表宽就由 wrap 内容决定（不再是表格那套 shrink-to-fit），
     在气泡里这正是想要的。 */
  .md :global(table) {
    display: block;
    overflow-x: auto;
    max-width: 100%;
    border-collapse: collapse;
    margin: 0 0 0.6em;
  }
  .md :global(th),
  .md :global(td) {
    padding: 0.3em 0.55em;
    border: 1px solid rgba(244, 246, 251, 0.18);
  }

  .md :global(h1),
  .md :global(h2),
  .md :global(h3),
  .md :global(h4),
  .md :global(h5),
  .md :global(h6) {
    margin: 0.6em 0 0.4em;
    font-size: 1em;
    font-weight: 700;
  }

  /* 块级公式常常比气泡宽，让它自己横滚而不是把气泡撑破。 */
  .md :global(.katex-display) {
    margin: 0.5em 0;
    overflow-x: auto;
    overflow-y: hidden;
  }
</style>
