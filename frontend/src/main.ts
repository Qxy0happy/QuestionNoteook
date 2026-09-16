import { mount } from 'svelte';
import App from './App.svelte';

// 把前端的错误送到 logcat —— **这一步是诊断能力，不是锦上添花**。
//
// 宿主在 MainActivity 里加了 `onConsoleMessage` 转发，但它在真机上**不工作**（实测：
// 同一个 WebChromeClient 里 `onPermissionRequest` 活、`onConsoleMessage` 一条都没有，
// logcat 里连 chromium 的 tag 也没有）。后果是前端在真机上一出问题就**完全静默** ——
// 没有堆栈、没有报错，只能靠猜。这不是可以放着不管的缺陷：真机上唯一能看见前端的窗口就是它。
//
// 这条路绕开了那个钩子：`window.wails.log` 是宿主自己暴露的 `@JavascriptInterface`
// （见 WailsJSBridge.log），它由 Java 直接写 logcat，不经过 WebChromeClient。
//
// 桌面端没有 `window.wails`，所以整段是静默跳过的 —— 它不该让应用在别的平台上出问题。
function report(level: string, text: string): void {
  try {
    const host = (globalThis as { wails?: { log?: (l: string, m: string) => void } }).wails;
    host?.log?.(level, text);
  } catch {
    // 报错的那条路自己不能抛错 —— 否则就是一个无限递归。
  }
}

// 没人 catch 的同步错误。
window.addEventListener('error', (event) => {
  const where = event.filename ? ` @ ${event.filename}:${event.lineno}:${event.colno}` : '';
  report('error', `[uncaught] ${event.message}${where}\n${event.error?.stack ?? ''}`);
});

// 没人 catch 的 Promise。
//
// 这一条尤其要紧：**Wails 的绑定调用就是 Promise** —— 一个没接 catch 的 IPC 失败
// 只会走到这里，别的钩子一个都碰不到。
window.addEventListener('unhandledrejection', (event) => {
  const reason: unknown = event.reason;
  const text =
    reason instanceof Error ? `${reason.name}: ${reason.message}\n${reason.stack ?? ''}` : String(reason);
  report('error', `[unhandled rejection] ${text}`);
});

mount(App, { target: document.getElementById('app')! });
