package com.wails.app;

import android.app.Activity;
import android.app.AlarmManager;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.content.Context;
import android.content.Intent;
import android.content.SharedPreferences;
import android.content.pm.PackageManager;
import android.net.Uri;
import android.os.Build;
import android.provider.Settings;
import android.util.Log;

import org.json.JSONArray;
import org.json.JSONObject;

import java.io.ByteArrayOutputStream;
import java.io.File;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.text.SimpleDateFormat;
import java.util.Date;
import java.util.Locale;

/**
 * DigestScheduler 是「每日汇总通知」的**宿主那一半**：读 Go 算好的排程文件，
 * 把下一条汇总交给 AlarmManager，并把宿主自己的状态回写给 Go 读。
 *
 * ── 为什么调度在 Java，内容却在 Go ──
 *
 * 「应用没打开也要发」在安卓上只有后台调度做得到，而调度被系统拉起的那一刻 Go 进程
 * 并不存在（Go 是 libwails.so，由这个 Java 宿主拉起来的）。反过来，判断「今天到期多少」
 * 需要 FSRS 的到期时刻与库里的数据，宿主去读 SQLite 等于把 review 的判据抄第二遍、
 * 还要再引一套 SQLite。所以分工是：**Go 在应用打开时把未来 14 天各自发什么算好落盘，
 * 宿主只负责到点念出来**（见 internal/digest 的包注释）。
 *
 * 为什么不是 WorkManager：官方 Doze 文档逐字写着「WorkManager 内部用 JobScheduler，
 * 所以它的任务在 Doze 下不跑」——那不是「会晚一点」，是静默地永不触发。
 * 所以这里用的是 {@code setExactAndAllowWhileIdle}（Doze 里不被推迟，限制是每应用
 * 9 分钟最多一次，对一天一次毫无影响）。
 *
 * ── 这个类只依赖 android.* 与 org.json ──
 *
 * 闹钟是由系统在**冷进程**里拉起 receiver 的。这条路径上绝不能碰 WailsBridge ——
 * 它的静态初始化会 System.loadLibrary("wails")，为了发一条通知去把整个 Go 运行时
 * 拉起来是不必要的，而且那条路径上没人管它的生命周期。所以文件读写、JSON 解析、
 * 建渠道全部自备，只有 org.json 是安卓自带的（不引新依赖）。
 */
final class DigestScheduler {
    private static final String TAG = "DigestScheduler";

    /**
     * 数据目录下的子目录名，必须与 Go 的 dataDir() 拼出来的一致。
     *
     * Go 那边是 {@code /data/data/<包名>/files/questionbook}（见 main.go 的 androidDataDir），
     * 而 {@code /data/data/<包名>/files} 就是 {@code getFilesDir()}。
     * 两边是**同一份路径的两种写法**，所以这里用 getFilesDir() 拼而不是写死 /data/data。
     */
    private static final String DATA_SUBDIR = "questionbook";
    private static final String SCHEDULE_FILE = "digest-schedule.json";
    private static final String HOST_STATE_FILE = "digest-host.json";

    /** 闹钟的 Intent action。显式组件已经足够定位 receiver，action 只是给日志与调试看。 */
    static final String ACTION_FIRE = "com.wails.app.action.DIGEST_FIRE";

    /** extras 里放的是**线索**不是命令：receiver 拿到后会去文件里复核（见 DigestAlarmReceiver）。 */
    static final String EXTRA_FIRE_AT_MS = "fire_at_ms";
    static final String EXTRA_DATE = "date";
    static final String EXTRA_COUNT = "count";
    static final String EXTRA_TITLE = "title";
    static final String EXTRA_BODY = "body";

    /**
     * 通知渠道单开一个，**不混进脚手架的 wails_default**。
     *
     * 两个理由：用户想单独关掉每日提醒时，系统设置里能按渠道关（关掉它不该连带关掉
     * 应用里其它即时通知）；而且渠道的重要性一旦被用户改过就改不回来了，混用会让
     * 以后想调每日提醒的优先级变成不可能。
     */
    static final String CHANNEL_ID = "digest";

    /** 通知 id **固定**：每天一条，重发要覆盖上一条，不能叠成一堆。 */
    static final int NOTIFICATION_ID = 12101;

    /** PendingIntent 的请求码固定，于是新排的一次覆盖旧的那次（不会攒出多个闹钟）。 */
    private static final int ALARM_REQUEST_CODE = 12100;

    /**
     * 复核时的宽限：闹钟响的时刻比排定的时刻晚超过 6 小时就不发了。
     *
     * 挡的是「系统把闹钟推到了第二天」——那时候这条汇总上写的「今天有 5 道」已经是
     * 昨天的数字了。宁可那天不发，也不要发一条数字错的。
     */
    static final long GRACE_MS = 6L * 60 * 60 * 1000;

    /**
     * 拿不到精确闹钟权限时降级用的窗口宽度。
     *
     * 取 10 分钟：提醒晚十分钟没有代价，而窗口开得太宽（比如几小时）就等于把「几点发」
     * 这个用户设过的值也弄丢了。降级真正的代价是**可能被 Doze 推迟到维护窗口**，
     * 那一条由 receiver 的宽限复核兜着（晚超过 6 小时就不发了）。
     */
    private static final long INEXACT_WINDOW_MS = 10L * 60 * 1000;

    /** 引导去系统设置只做一次，记在这里（不写进 digest-host.json：那个文件的形状是 Go 定的）。 */
    private static final String PREFS = "digest_host";
    private static final String PREF_EXACT_PROMPTED = "exact_alarm_prompted";
    private static final String PREF_NOTIF_PROMPTED = "notif_permission_prompted";

    /**
     * 申请通知权限用的请求码。**必须挑一个别的代码不用的** —— 脚手架里 1001（JS 主动弹通知）
     * 与 1003（前台服务）各有主，撞上就会被对面的回调当成自己的结果处理。
     */
    private static final int REQ_NOTIFICATIONS = 4101;

    private DigestScheduler() {
    }

    // ---- 路径 -----------------------------------------------------------

    private static File scheduleFile(Context ctx) {
        return new File(new File(ctx.getFilesDir(), DATA_SUBDIR), SCHEDULE_FILE);
    }

    private static File hostStateFile(Context ctx) {
        return new File(new File(ctx.getFilesDir(), DATA_SUBDIR), HOST_STATE_FILE);
    }

    /**
     * 读排程文件。读不出来（不在 / 坏了 / 权限不对）一律返回 null —— 宿主对这一份
     * 文件的态度是「读不懂就当没有可发的」，绝不拿默认值顶替。
     *
     * 这与 Go 那边对**设置**文件的态度是同一条（文件在但坏了 = 报错，不悄悄顶替），
     * 只是宿主没有报错的地方，于是它退化成「不发」。
     */
    static JSONObject readSchedule(Context ctx) {
        File f = scheduleFile(ctx);
        if (!f.isFile()) {
            return null;
        }
        try {
            return new JSONObject(readAllUtf8(f));
        } catch (Exception e) {
            // 并发写窗口理论上不存在（Go 是临时文件 + 改名），真读到半截就是坏文件。
            Log.w(TAG, "排程文件读不出来，当作没有可发的: " + f, e);
            return null;
        }
    }

    /** 本地日的字符串形状，与 Go 的 dateLayout（"2006-01-02"）逐字对齐。 */
    static String today() {
        // Locale.US 不是随手写的：SimpleDateFormat 的年份按**locale 的日历**算，
        // 在 th-TH 或 ja-JP-u-ca-japanese 下 "yyyy" 会给出佛历/和历年份，
        // 于是与 Go 写下的 "2026-09-16" 永远对不上。日期是给机器比大小的，必须是公历。
        return new SimpleDateFormat("yyyy-MM-dd", Locale.US).format(new Date());
    }

    // ---- 排程 -----------------------------------------------------------

    /**
     * 排下一次汇总。没有可排的就撤掉闹钟。
     *
     * 这是**幂等**的：同一个请求码 + 同一个 action 的 PendingIntent 会覆盖上一次，
     * 所以每次应用启动都调用它是安全的，也正是靠它自愈（见下面的注释与 MainActivity）。
     */
    static synchronized void arm(Context ctx) {
        HostState state = readState(ctx);
        boolean exact = canScheduleExactAlarms(ctx);
        state.canScheduleExact = exact;
        state.notificationsEnabled = notificationsEnabled(ctx);

        try {
            JSONObject schedule = readSchedule(ctx);
            if (schedule == null) {
                // 还没有排程（刚装好、Go 还没算过），或者文件坏了。宿主对这一份的态度
                // 与 Go 对设置文件的态度是同一条：读不懂就当没有可发的，绝不拿默认值顶替。
                // 结果码归到 skipped_no_due —— 六个取值里只有它表达「当下没有可发的」。
                cancelAlarm(ctx);
                state.lastResult = "skipped_no_due";
                writeState(ctx, state);
                return;
            }
            if (!schedule.optBoolean("enabled", false)) {
                // 用户关掉了。Go 在写 enabled:false 时同时写了空 slots（两处都说），
                // 这里只判 enabled 也够 —— 判据少一条就少一次误发。
                cancelAlarm(ctx);
                state.lastResult = "skipped_disabled";
                writeState(ctx, state);
                return;
            }

            Slot slot = firstFutureSlot(schedule);
            if (slot == null) {
                // 未来 14 天里没有一天是「有题到期」的，或者这份排程已经整份过期了。
                cancelAlarm(ctx);
                state.lastResult = "skipped_no_due";
                writeState(ctx, state);
                return;
            }

            setAlarm(ctx, slot, exact);
            state.lastArmAtMs = System.currentTimeMillis();
            // 排程成功**不改 last_result**：那个字段记的是「上一次到点时的结果」，
            // 在这里写 "posted" 会让设置页在什么都没发出去时说「已发送」，
            // 写 skipped_* 更是无中生有。第一次 arm 时它是空串（Go 侧读到的空串
            // 就是「还没有到过点」，见 writeState 的注释）。
            writeState(ctx, state);

            if (!exact) {
                // 只在**真的排上了一条**之后才引导：没东西可发的时候把人丢进系统设置是骚扰。
                maybePromptForExactAlarm(ctx);
            }
            // 通知权限也要主动要一次。**不主动要就永远拿不到**：系统不会自己问，而安卓 13 起
            // 通知默认是关的（官方原话 "notifications are off by default"）。脚手架里唯一会申请
            // 它的地方是 WailsBridge.postNotification，而那条路是给 JS 主动弹通知用的，我们不走。
            // 少了这一步，这条提醒会永远发不出来、状态文件只会一直记 skipped_no_permission。
            maybeRequestNotificationPermission(ctx);
        } catch (Exception e) {
            Log.e(TAG, "排下一次汇总失败", e);
            state.lastResult = "failed";
            writeState(ctx, state);
        }
    }

    /** 撤掉已排的闹钟（用户关掉提醒、或排程里已经没有可发的条目时）。 */
    static synchronized void cancel(Context ctx) {
        cancelAlarm(ctx);
    }

    private static void cancelAlarm(Context ctx) {
        try {
            AlarmManager am = (AlarmManager) ctx.getSystemService(Context.ALARM_SERVICE);
            if (am == null) {
                return;
            }
            // 用与排程时**完全一样**的 flags，而不是 FLAG_NO_CREATE：PendingIntent 的
            // 可变性也是匹配条件之一，两边对不上就取不到那一个，于是取消会静默失败
            //（闹钟照响）。代价是最多造一个立刻被销毁的 PendingIntent，可以忽略。
            PendingIntent pi = PendingIntent.getBroadcast(
                    ctx, ALARM_REQUEST_CODE, fireIntent(ctx), pendingFlags());
            am.cancel(pi);
            pi.cancel();
        } catch (Exception e) {
            Log.w(TAG, "撤闹钟失败", e);
        }
    }

    /**
     * 挑出下一条要排的汇总：**第一条** count &gt; 0、日期不早于今天、时刻还没到的。
     *
     * 「日期不早于今天」的字符串比较是成立的：形态固定为 yyyy-MM-dd，字典序就是时间序。
     * 这条同时兜住「排程是几天前算的，slots 已经整份烂在过去」——一条都挑不出来。
     *
     * **「时刻还没到」这条不能省。** 已经过去的时刻排下去 AlarmManager 会**立刻**
     * 触发，而那时复核看到的是「日期就是今天、距 fire_at 只过了几秒」（在宽限内），
     * 于是这条会被发出去；receiver 发完还要 arm 下一次，arm 又挑中同一条 → 立刻再触发
     * → 再发一遍。那是个死循环，一次能刷出几百条通知。
     */
    private static Slot firstFutureSlot(JSONObject schedule) {
        JSONArray slots = schedule.optJSONArray("slots");
        if (slots == null) {
            return null;
        }
        long now = System.currentTimeMillis();
        String today = today();
        for (int i = 0; i < slots.length(); i++) {
            JSONObject o = slots.optJSONObject(i);
            if (o == null) {
                continue;
            }
            int count = o.optInt("count", 0);
            if (count <= 0) {
                continue; // count == 0 就是「那天不发」
            }
            String date = o.optString("date", "");
            if (date.isEmpty() || date.compareTo(today) < 0) {
                continue;
            }
            long fireAt = o.optLong("fire_at_ms", 0L);
            if (fireAt <= now) {
                continue;
            }
            return new Slot(fireAt, date, count, o.optString("title", ""), o.optString("body", ""));
        }
        return null;
    }

    private static void setAlarm(Context ctx, Slot slot, boolean exactAllowed) {
        AlarmManager am = (AlarmManager) ctx.getSystemService(Context.ALARM_SERVICE);
        if (am == null) {
            throw new IllegalStateException("没有 AlarmManager");
        }
        PendingIntent pi = PendingIntent.getBroadcast(ctx, ALARM_REQUEST_CODE, fireIntent(ctx, slot), pendingFlags());

        if (exactAllowed) {
            try {
                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
                    // Doze 里 setExact() / setWindow() 会被推到维护窗口，这两个带
                    // AllowWhileIdle 的不会 —— 一天一次，用不到它 9 分钟一次的配额。
                    am.setExactAndAllowWhileIdle(AlarmManager.RTC_WAKEUP, slot.fireAtMs, pi);
                } else {
                    // API 21/22 上没有 Doze（M 才引入），setExact 就是最精确的那个。
                    am.setExact(AlarmManager.RTC_WAKEUP, slot.fireAtMs, pi);
                }
                Log.i(TAG, "已排下一次汇总（精确）: " + slot.date + " @ " + slot.fireAtMs);
                return;
            } catch (SecurityException e) {
                // 权限在 canScheduleExactAlarms() 之后、这一句之前被撤销（用户正好在系统
                // 设置里关掉）。降级而不是把这次排程整个丢掉 —— 用户要的是「收到提醒」，
                // 不是「精确到秒收到提醒」。
                Log.w(TAG, "精确闹钟被拒，降级为窗口", e);
            }
        }

        am.setWindow(AlarmManager.RTC_WAKEUP, slot.fireAtMs, INEXACT_WINDOW_MS, pi);
        Log.i(TAG, "已排下一次汇总（降级窗口 " + (INEXACT_WINDOW_MS / 60000) + " 分钟）: "
                + slot.date + " @ " + slot.fireAtMs);
    }

    private static Intent fireIntent(Context ctx) {
        Intent i = new Intent(ctx, DigestAlarmReceiver.class);
        i.setAction(ACTION_FIRE);
        return i;
    }

    /**
     * 把这一条塞进 Intent 的 extras。
     *
     * 里面每一项 receiver 都要**拿回文件里复核**才用（见 DigestAlarmReceiver.deliver），
     * 所以 count/title/body 三项实际上只用来排查问题（dumpsys alarm / logcat 里看得见
     * 「这次闹钟自以为是哪一条」）。真正显示什么、发不发，一律由文件那一份当下的内容决定。
     */
    private static Intent fireIntent(Context ctx, Slot slot) {
        Intent i = fireIntent(ctx);
        i.putExtra(EXTRA_FIRE_AT_MS, slot.fireAtMs);
        i.putExtra(EXTRA_DATE, slot.date);
        i.putExtra(EXTRA_COUNT, slot.count);
        i.putExtra(EXTRA_TITLE, slot.title);
        i.putExtra(EXTRA_BODY, slot.body);
        return i;
    }

    private static int pendingFlags() {
        // FLAG_UPDATE_CURRENT：每次都把 extras 换成新的。复用同一个请求码是有意的
        //（一次只有一个闹钟），但 extras 必须跟着最新那条走。
        int flags = PendingIntent.FLAG_UPDATE_CURRENT;
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
            // API 31+ 要求可变性显式声明；我们不需要接收方改它，所以是不可变的。
            flags |= PendingIntent.FLAG_IMMUTABLE;
        }
        return flags;
    }

    // ---- 权限相关的两处 ---------------------------------------------

    /**
     * 现在能不能排**精确**闹钟。
     *
     * Android 14 起 SCHEDULE_EXACT_ALARM 对新装应用**默认被拒**（官方原文：
     * "no longer being pre-granted to most newly installed apps"），所以要问系统而不是
     * 假定自己拿到了。API 31 以下没有这个开关，恒为 true。
     *
     * 没有走 USE_EXACT_ALARM：Google Play 把它列为受限权限，只给「核心功能是日历或闹钟」
     * 的应用，错题本不够格。
     */
    static boolean canScheduleExactAlarms(Context ctx) {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.S) {
            return true;
        }
        try {
            AlarmManager am = (AlarmManager) ctx.getSystemService(Context.ALARM_SERVICE);
            return am != null && am.canScheduleExactAlarms();
        } catch (Exception e) {
            return false;
        }
    }

    /**
     * 通知到底发不发得出去。三种「关」都要认，否则状态文件会说「已发送」而用户什么都没看到：
     *
     *   - 应用级通知被关（API 24+ 的 areNotificationsEnabled，33+ 上 POST_NOTIFICATIONS
     *     被拒也走这一条）；
     *   - 运行时权限 POST_NOTIFICATIONS 没给（33+，上面那条通常已经覆盖，这里显式再判一次
     *     是为了不受各厂商实现差异影响）；
     *   - 单单 digest 这个渠道被用户关掉（那是「通知权限没给」在这条提醒上的实际形态）。
     */
    static boolean notificationsEnabled(Context ctx) {
        try {
            NotificationManager nm =
                    (NotificationManager) ctx.getSystemService(Context.NOTIFICATION_SERVICE);
            if (nm == null) {
                return false;
            }
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.N && !nm.areNotificationsEnabled()) {
                return false;
            }
            if (Build.VERSION.SDK_INT >= 33 && ctx.checkSelfPermission(
                    "android.permission.POST_NOTIFICATIONS") != PackageManager.PERMISSION_GRANTED) {
                return false;
            }
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                NotificationChannel ch = nm.getNotificationChannel(CHANNEL_ID);
                if (ch != null && ch.getImportance() == NotificationManager.IMPORTANCE_NONE) {
                    return false;
                }
            }
            return true;
        } catch (Exception e) {
            Log.w(TAG, "查通知开关失败", e);
            return false;
        }
    }

    /**
     * 申请通知权限（33+）。与精确闹钟那次引导同一套收敛条件，理由也一样：
     *
     *   - **只在 Activity 上下文里问** —— arm 也会被 receiver 调到，那种上下文里弹不出权限框，
     *     白烧掉「只问一次」的那一次机会；
     *   - **只问一次**（SharedPreferences 标志）。被拒之后再问系统也不会弹，只是骚扰；
     *   - 调用点只在**真的排上了一条**之后（见 arm），没东西可发时弹框是骚扰。
     *
     * 被拒之后的出路是设置页那一行（HostStatus.Note 会说「去系统设置里给错题本打开通知」）——
     * 通知权限被拒**不影响**其他任何功能。
     */
    private static void maybeRequestNotificationPermission(Context ctx) {
        if (Build.VERSION.SDK_INT < 33) {
            return; // 13 以下通知默认开着，没有这个权限可申请
        }
        if (!(ctx instanceof Activity)) {
            return;
        }
        if (ctx.checkSelfPermission("android.permission.POST_NOTIFICATIONS")
                == PackageManager.PERMISSION_GRANTED) {
            return;
        }
        SharedPreferences prefs = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE);
        if (prefs.getBoolean(PREF_NOTIF_PROMPTED, false)) {
            return;
        }
        prefs.edit().putBoolean(PREF_NOTIF_PROMPTED, true).apply();
        try {
            ((Activity) ctx).requestPermissions(
                    new String[]{"android.permission.POST_NOTIFICATIONS"}, REQ_NOTIFICATIONS);
        } catch (Exception e) {
            Log.w(TAG, "申请通知权限失败", e);
        }
    }

    /**
     * 建 digest 渠道。发之前调，重复调是安全的（同 id 已存在就什么都不做——
     * 再 createNotificationChannel 一次不会改回用户手动调过的设置，但也没必要）。
     */
    static void ensureChannel(Context ctx) {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) {
            return; // 26 以下没有渠道这回事
        }
        try {
            NotificationManager nm =
                    (NotificationManager) ctx.getSystemService(Context.NOTIFICATION_SERVICE);
            if (nm == null || nm.getNotificationChannel(CHANNEL_ID) != null) {
                return;
            }
            NotificationChannel ch = new NotificationChannel(
                    CHANNEL_ID, "每日提醒", NotificationManager.IMPORTANCE_DEFAULT);
            ch.setDescription("每天一条待复习汇总");
            nm.createNotificationChannel(ch);
        } catch (Exception e) {
            Log.w(TAG, "建通知渠道失败", e);
        }
    }

    /**
     * 拿不到精确闹钟权限时，**只引导一次**去系统设置里开。
     *
     * 为什么只一次：arm() 每次应用启动都会调，而系统设置页是一整屏抢焦点的东西 ——
     * 每次进来都被丢过去是骚扰。长期的入口是设置页那一行（Go 读 digest-host.json 的
     * can_schedule_exact 之后露出来的「去开启」），所以这里只要提过一回就够。
     *
     * **只在有 Activity 时引导**：arm() 也会被 receiver 调到（到点之后重排、开机重排），
     * 那种上下文里 startActivity 会被「后台启动 Activity」的限制挡掉，多半只是白烧掉
     * 那一次引导机会（标志位已经写下去了）。
     *
     * 官方对这个权限要求的做法就是「canScheduleExactAlarms() 为 false 时用
     * ACTION_REQUEST_SCHEDULE_EXACT_ALARM 引导」。
     */
    private static void maybePromptForExactAlarm(Context ctx) {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.S) {
            return;
        }
        if (!(ctx instanceof Activity)) {
            return;
        }
        SharedPreferences prefs = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE);
        if (prefs.getBoolean(PREF_EXACT_PROMPTED, false)) {
            return;
        }
        prefs.edit().putBoolean(PREF_EXACT_PROMPTED, true).apply();

        try {
            Intent i = new Intent(Settings.ACTION_REQUEST_SCHEDULE_EXACT_ALARM);
            // 带上包名让系统直接开到这个应用那一项，而不是整个列表。
            i.setData(Uri.parse("package:" + ctx.getPackageName()));
            ctx.startActivity(i);
        } catch (Exception e) {
            // 有的 ROM 没有这一屏。引导失败不该影响排程本身。
            Log.w(TAG, "打不开「闹钟和提醒」设置页", e);
        }
    }

    // ---- 状态回写 -------------------------------------------------------

    /**
     * digest-host.json 的形状。**只由宿主写，Go 只读** —— 这是宿主唯一能说话的地方：
     * 「精确闹钟权限被拒」「通知权限被关」这两件事只有 Java 知道，Go 没法问系统。
     */
    private static final class HostState {
        boolean canScheduleExact;
        boolean notificationsEnabled;
        long lastArmAtMs;
        long lastFireAtMs;
        /**
         * 上一次**到点**的结果。取值固定为这几个，Go 侧按它露设置页那一行：
         * posted / skipped_disabled / skipped_no_due / skipped_stale /
         * skipped_no_permission / failed。
         *
         * 空串 = 还没有到过点（文件刚被 arm 建出来）。arm 成功时**保留**这个值，
         * 因为「排上了」不是这六个取值里的任何一个。
         */
        String lastResult = "";
    }

    private static HostState readState(Context ctx) {
        HostState s = new HostState();
        File f = hostStateFile(ctx);
        if (!f.isFile()) {
            return s;
        }
        try {
            JSONObject o = new JSONObject(readAllUtf8(f));
            s.canScheduleExact = o.optBoolean("can_schedule_exact", false);
            s.notificationsEnabled = o.optBoolean("notifications_enabled", true);
            s.lastArmAtMs = o.optLong("last_arm_at_ms", 0L);
            s.lastFireAtMs = o.optLong("last_fire_at_ms", 0L);
            s.lastResult = o.optString("last_result", "");
        } catch (Exception e) {
            Log.w(TAG, "宿主状态文件读不出来，按空的重建: " + f, e);
        }
        return s;
    }

    /**
     * 记一次「到点」。{@code result} 只能取上面那六个值。
     *
     * last_fire_at_ms 只在**真的发出去**时更新：它记的是「上一次发送」，把一次跳过
     * 写进去会让设置页把「今天没发」说成「今天发了」。
     */
    static synchronized void recordFire(Context ctx, String result) {
        HostState s = readState(ctx);
        s.canScheduleExact = canScheduleExactAlarms(ctx);
        s.notificationsEnabled = notificationsEnabled(ctx);
        if ("posted".equals(result)) {
            s.lastFireAtMs = System.currentTimeMillis();
        }
        s.lastResult = result;
        writeState(ctx, s);
    }

    /**
     * 原子地写状态文件：临时文件 + 改名。
     *
     * 读者是**另一个进程**的 Go（它在设置页刷新那一刻读），写一半被读到就等于读到一份
     * 字段缺失的 JSON。Go 那边对排程文件也是这么写的（writeSchedule），理由逐字相同。
     * 顺带 flush + fsync：这个文件小得可以忽略，而「Go 读到半截」的代价是一次错误的
     * 设置页文案，不值得为省一次 fsync 去赌。
     */
    private static void writeState(Context ctx, HostState s) {
        try {
            JSONObject o = new JSONObject();
            o.put("can_schedule_exact", s.canScheduleExact);
            o.put("notifications_enabled", s.notificationsEnabled);
            o.put("last_arm_at_ms", s.lastArmAtMs);
            o.put("last_fire_at_ms", s.lastFireAtMs);
            o.put("last_result", s.lastResult);
            writeAtomic(hostStateFile(ctx), o.toString());
        } catch (Exception e) {
            Log.e(TAG, "写宿主状态失败", e);
        }
    }

    private static void writeAtomic(File target, String content) throws IOException {
        File dir = target.getParentFile();
        if (dir != null && !dir.exists() && !dir.mkdirs()) {
            throw new IOException("建目录失败: " + dir);
        }
        File tmp = new File(dir, target.getName() + "." + System.nanoTime() + ".tmp");
        boolean renamed = false;
        try {
            try (FileOutputStream out = new FileOutputStream(tmp)) {
                out.write(content.getBytes("UTF-8"));
                out.flush();
                try {
                    out.getFD().sync();
                } catch (Exception ignored) {
                    // 有的文件系统不支持 sync，那不是致命错误。
                }
            }
            if (!tmp.renameTo(target)) {
                throw new IOException("改名失败: " + tmp + " -> " + target);
            }
            renamed = true;
        } finally {
            if (!renamed) {
                //noinspection ResultOfMethodCallIgnored
                tmp.delete();
            }
        }
    }

    private static String readAllUtf8(File f) throws IOException {
        try (InputStream in = new FileInputStream(f)) {
            ByteArrayOutputStream bos = new ByteArrayOutputStream();
            byte[] buf = new byte[8192];
            int n;
            while ((n = in.read(buf)) > 0) {
                bos.write(buf, 0, n);
            }
            return new String(bos.toByteArray(), "UTF-8");
        }
    }

    /** 一条排程。字段与 internal/digest 的 slotFile 逐字对应。 */
    private static final class Slot {
        final long fireAtMs;
        final String date;
        final int count;
        final String title;
        final String body;

        Slot(long fireAtMs, String date, int count, String title, String body) {
            this.fireAtMs = fireAtMs;
            this.date = date;
            this.count = count;
            this.title = title;
            this.body = body;
        }
    }
}
