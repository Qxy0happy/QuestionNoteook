package com.wails.app;

import android.content.ContentResolver;
import android.content.ContentValues;
import android.content.Context;
import android.database.Cursor;
import android.net.Uri;
import android.os.Build;
import android.os.Environment;
import android.os.Handler;
import android.os.Looper;
import android.provider.MediaStore;
import android.util.Log;
import java.io.File;
import java.io.FileInputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import android.webkit.JavascriptInterface;
import android.webkit.WebView;
import com.wails.app.BuildConfig;
import org.json.JSONObject;

/**
 * WailsJSBridge provides the JavaScript interface that allows the web frontend
 * to communicate with the Go backend. This is exposed to JavaScript as the
 * `window.wails` object.
 *
 * Similar to iOS's WKScriptMessageHandler but using Android's addJavascriptInterface.
 */
public class WailsJSBridge {
    private static final String TAG = "WailsJSBridge";
    private static final boolean DEBUG = BuildConfig.DEBUG;
    // Pooled threads avoid unbounded thread creation under high call volume.
    private static final ExecutorService executor = Executors.newCachedThreadPool();

    private final WailsBridge bridge;
    private final WebView webView;
    // armDigest 要走主线程 —— AlarmManager 与 requestPermissions 都要求主线程。
    private static final Handler mainHandler = new Handler(Looper.getMainLooper());

    public WailsJSBridge(WailsBridge bridge, WebView webView) {
        this.bridge = bridge;
        this.webView = webView;
    }

    /**
     * Send a message to Go and return the response synchronously.
     * Called from JavaScript: wails.invoke(message)
     *
     * @param message The message to send (JSON string)
     * @return The response from Go (JSON string)
     */
    @JavascriptInterface
    public String invoke(String message) {
        if (DEBUG) Log.d(TAG, "Invoke called: " + message);
        return bridge.handleMessage(message);
    }

    /**
     * Send a message to Go asynchronously.
     * The response will be sent back via a callback.
     * Called from JavaScript: wails.invokeAsync(callbackId, message)
     *
     * @param callbackId The callback ID to use for the response
     * @param message The message to send (JSON string)
     */
    @JavascriptInterface
    public void invokeAsync(final String callbackId, final String payload) {
        if (DEBUG) Log.d(TAG, "InvokeAsync called: " + payload);

        // Handle off the JS thread so we don't block the WebView.
        executor.execute(() -> {
            try {
                String response = bridge.handleRuntimeCall(payload);
                sendCallback(callbackId, response, null);
            } catch (Exception e) {
                Log.e(TAG, "Error in async invoke", e);
                sendCallback(callbackId, null, e.getMessage());
            }
        });
    }

    /**
     * Log a message from JavaScript to Android's logcat
     * Called from JavaScript: wails.log(level, message)
     *
     * @param level The log level (debug, info, warn, error)
     * @param message The message to log
     */
    @JavascriptInterface
    public void log(String level, String message) {
        switch (level.toLowerCase()) {
            case "debug":
                Log.d(TAG + "/JS", message);
                break;
            case "info":
                Log.i(TAG + "/JS", message);
                break;
            case "warn":
                Log.w(TAG + "/JS", message);
                break;
            case "error":
                Log.e(TAG + "/JS", message);
                break;
            default:
                Log.v(TAG + "/JS", message);
                break;
        }
    }

    /**
     * Ask the host to arm the next daily digest alarm.
     * Called from JavaScript: wails.armDigest()
     *
     * 为什么需要这么一条：Go 写好排程文件之后，**只有 Java 知道**怎么把它变成一个真的闹钟；
     * 而 Go 与 Java 之间没有调用通道（WailsBridge 上那些能力都是 Go 经 JNI 调的，对应的 Go
     * 包装在 Wails 模块里）。所以由前端在「排程刚被算出来」这一刻喊一声。
     *
     * 顺带，这一次才有机会申请通知权限与引导「闹钟和提醒」—— 宿主那边两者都收敛成
     * 「只在真的排上了一条、且在 Activity 上下文里、且没问过」才问（见 DigestScheduler）。
     */
    @JavascriptInterface
    public void armDigest() {
        final Context ctx = webView.getContext();
        mainHandler.post(() -> DigestScheduler.arm(ctx));
    }

    /**
     * Get the platform name
     * Called from JavaScript: wails.platform()
     *
     * @return "android"
     */
    @JavascriptInterface
    public String platform() {
        return "android";
    }

    /**
     * Check if we're running in debug mode
     * Called from JavaScript: wails.isDebug()
     *
     * @return true if debug build, false otherwise
     */
    @JavascriptInterface
    public boolean isDebug() {
        return BuildConfig.DEBUG;
    }

    /**
     * 把应用私有目录里的一个文件拷进系统「下载」目录 —— 那里**卸载了也还在**。
     *
     * 从 JS 这样调：{@code wails.copyToDownloads(json)}
     *
     * 入参：{"path":"/data/data/<pkg>/files/questionbook/exports/xxx.zip",
     *        "name":"错题本-2026-09-16.zip", "mime":"application/zip"}
     * 出参：{"ok":true,"display":"下载/错题本-2026-09-16.zip"}
     *       {"ok":false,"error":"…"}
     *
     * ── 为什么走 MediaStore 而不是直接写 /sdcard/Download/ ──
     *
     * targetSdk 是 35，scoped storage 生效：按路径写公共下载目录会被直接拒。
     * API 29+ 上 MediaStore.Downloads 是**不需要任何存储权限**就能落进那里的唯一正路。
     *
     * ── 为什么必须校验 path ──
     *
     * 这个方法是 **JS 可调** 的：今天调它的是我们自己的前端，明天可能是任何一段
     * 跑进 WebView 的脚本。没有校验，它就是一个通用的「把任意可读文件拷到下载目录」
     * 的工具 —— 也就是一个数据外带原语。判据只有一条但必须严：真实路径
     * （canonical，软链与 ".." 都已解开）落在 getFilesDir() 之下。
     *
     * ── 同步，是取舍过的 ──
     *
     * @JavascriptInterface 方法返回之前调用它的 JS 一直阻塞，所以拷贝期间页面是卡住的。
     * 这里接受这个代价：导出包最多几十 MB（一个 SQLite 加若干降采样过的 PNG），
     * 而 MediaStore 写入是本地 IPC，不是网络往返 —— 实际耗时是零点几秒的量级。
     * 改成异步要把结果绕一圈 sendCallback() 与 Go，复杂度远大于它省掉的那点卡顿。
     * 如果导出包将来涨到几百 MB，这一行就是要重新考虑的地方。
     */
    @JavascriptInterface
    public String copyToDownloads(String json) {
        File src = null;
        Context ctx = webView != null ? webView.getContext() : null;
        if (ctx == null) {
            return errorJson("没有 Context");
        }
        try {
            JSONObject opts = new JSONObject(json != null ? json : "{}");
            String rawPath = opts.optString("path", "");
            String mime = opts.optString("mime", "");
            if (mime.isEmpty()) {
                mime = "application/octet-stream";
            }

            src = resolvePrivateFile(ctx, rawPath);
            if (src == null) {
                // 前端拿到的就是这一条：路径不在私有目录里（拼错了、或者有人在试探）。
                return errorJson("路径不在应用私有目录里");
            }

            String name = sanitizeName(opts.optString("name", ""),
                    mime.contains("zip") ? "错题本导出.zip" : "导出文件");

            if (Build.VERSION.SDK_INT < Build.VERSION_CODES.Q) {
                // 安卓 10 以下没有 MediaStore.Downloads，写公共目录得先拿
                // WRITE_EXTERNAL_STORAGE（运行时权限，还要多声明一条 manifest 权限）。
                // 这条同步 API 没有「先去申请权限再回来」的地方，所以老系统上明确报错
                // 而不是留一个静默失败的按钮。
                return errorJson("安卓 10 以下暂不支持导出到下载目录");
            }

            ContentResolver resolver = ctx.getContentResolver();
            ContentValues values = new ContentValues();
            values.put(MediaStore.MediaColumns.DISPLAY_NAME, name);
            values.put(MediaStore.MediaColumns.MIME_TYPE, mime);
            values.put(MediaStore.MediaColumns.RELATIVE_PATH, Environment.DIRECTORY_DOWNLOADS);
            // IS_PENDING：拷完之前别的应用看不到这个半成品（不然文件管理器里会出现
            // 一个 0 字节的坏包）。拷完再清掉这一位。
            values.put(MediaStore.MediaColumns.IS_PENDING, 1);

            // 重名时 MediaStore 会自己改名（"错题本-2026-09-16 (1).zip"），所以下面
            // 要把系统最终定的名字**读回来**，返回值里报给用户的必须是那个。
            Uri target = resolver.insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI, values);
            if (target == null) {
                return errorJson("系统拒绝了这次写入");
            }

            boolean copied = false;
            try {
                try (InputStream in = new FileInputStream(src);
                     OutputStream out = resolver.openOutputStream(target, "w")) {
                    if (out == null) {
                        throw new IOException("打不开输出流");
                    }
                    byte[] buf = new byte[64 * 1024];
                    int n;
                    while ((n = in.read(buf)) > 0) {
                        out.write(buf, 0, n);
                    }
                    out.flush();
                }
                ContentValues done = new ContentValues();
                done.put(MediaStore.MediaColumns.IS_PENDING, 0);
                resolver.update(target, done, null, null);
                copied = true;
            } finally {
                if (!copied) {
                    // 拷了一半失败：把半成品从下载目录里撤掉，别给用户留一个坏文件
                    //（源文件保留，他可以重试）。
                    try {
                        resolver.delete(target, null, null);
                    } catch (Exception ignored) {
                    }
                }
            }

            String display = queryDisplayName(resolver, target);
            if (display == null || display.isEmpty()) {
                display = name;
            }

            // 成功之后删掉源文件：它是 Go 写的临时包，留着既没用（它在私有目录里，
            // 卸载就没了，防不了这张票要防的丢），又多占一份同等大小的空间。
            // 删不掉不算失败 —— 文件已经落进下载目录，用户要的结果已经达成。
            if (!src.delete()) {
                Log.w(TAG, "导出成功但源文件删不掉: " + src);
            }

            return new JSONObject()
                    .put("ok", true)
                    // 落点是给人看的：Go/前端把这一句原样显示出来，就是「明确告知落点」。
                    .put("display", "下载/" + display)
                    .toString();
        } catch (Exception e) {
            Log.e(TAG, "copyToDownloads failed", e);
            return errorJson(e.getMessage() != null ? e.getMessage() : "导出失败");
        }
    }

    /**
     * 把 path 变成一个文件，**只**当它真的在应用私有目录里；否则返回 null。
     *
     * 关键是用了 getCanonicalPath() 而不是 getAbsolutePath()：前者会把 ".." 与软链
     * 一起解开再比，于是 "files/../../shared_prefs/x.xml" 与「私有目录里指向外面
     * 的软链」都过不了这一关。
     */
    private static File resolvePrivateFile(Context ctx, String path) {
        if (path == null || path.isEmpty()) {
            return null;
        }
        try {
            File filesDir = ctx.getFilesDir();
            if (filesDir == null) {
                return null;
            }
            File candidate = new File(path);
            if (!candidate.isAbsolute()) {
                return null;
            }
            String root = filesDir.getCanonicalPath();
            String real = candidate.getCanonicalPath();
            if (!real.equals(root) && !real.startsWith(root + File.separator)) {
                return null;
            }
            return candidate.isFile() ? candidate : null;
        } catch (Exception e) {
            return null;
        }
    }

    /**
     * 把 raw 洗成一个能交给 MediaStore 当 DISPLAY_NAME 的名字。
     *
     * '/' 会被某些实现当成路径分隔符，':' 在部分机型上被 MediaProvider 直接拒掉
     * （FAT 时代的保留字符），控制字符则会跑进文件名里让之后谁都处理不了。
     * 开头的点也去掉：不造隐藏文件，也不给 ".." 留活口。
     */
    private static String sanitizeName(String raw, String fallback) {
        String name = raw == null ? "" : raw.trim();
        name = name.replaceAll("[\\\\/:*?\"<>|\\x00-\\x1F\\x7F]", "_");
        name = name.replaceAll("^[.]+", "");
        if (name.length() > 120) {
            // 截断时保住扩展名：丢了 .zip 系统就认不出类型，点开也没有「用什么打开」。
            int dot = name.lastIndexOf('.');
            String ext = (dot > 0 && name.length() - dot <= 12) ? name.substring(dot) : "";
            name = name.substring(0, Math.max(1, 120 - ext.length())) + ext;
        }
        return name.isEmpty() ? fallback : name;
    }

    /** 问 MediaStore：它最后到底把文件名定成了什么。 */
    private static String queryDisplayName(ContentResolver resolver, Uri uri) {
        try (Cursor c = resolver.query(uri,
                new String[]{MediaStore.MediaColumns.DISPLAY_NAME}, null, null, null)) {
            if (c != null && c.moveToFirst() && !c.isNull(0)) {
                return c.getString(0);
            }
        } catch (Exception e) {
            Log.w(TAG, "读回显示名失败", e);
        }
        return null;
    }

    private static String errorJson(String message) {
        try {
            return new JSONObject().put("ok", false).put("error", message).toString();
        } catch (Exception e) {
            return "{\"ok\":false,\"error\":\"unknown\"}";
        }
    }

    /**
     * Send a callback response to JavaScript
     */
    private void sendCallback(String callbackId, String result, String error) {
        final String js;
        if (error != null) {
            js = String.format(
                    "window._wailsAndroidCallback && window._wailsAndroidCallback('%s', null, '%s');",
                    escapeJsString(callbackId),
                    escapeJsString(error)
            );
        } else {
            js = String.format(
                    "window._wailsAndroidCallback && window._wailsAndroidCallback('%s', '%s', null);",
                    escapeJsString(callbackId),
                    escapeJsString(result != null ? result : "")
            );
        }

        webView.post(() -> webView.evaluateJavascript(js, null));
    }

    private String escapeJsString(String str) {
        if (str == null) return "";
        return str.replace("\\", "\\\\")
                .replace("'", "\\'")
                .replace("\n", "\\n")
                .replace("\r", "\\r")
                // JS line terminators (U+2028/U+2029) must be escaped too; built via
                // (char) casts so the Java lexer does not reinterpret them as newlines.
                .replace(String.valueOf((char) 0x2028), "\\u2028")
                .replace(String.valueOf((char) 0x2029), "\\u2029");
    }
}
