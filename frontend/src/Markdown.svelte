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

  .md :global(table) {
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
