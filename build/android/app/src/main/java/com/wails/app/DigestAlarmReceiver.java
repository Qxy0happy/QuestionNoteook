package com.wails.app;

import android.app.Notification;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.os.Build;
import android.util.Log;

import androidx.core.app.NotificationCompat;

import org.json.JSONArray;
import org.json.JSONObject;

/**
 * 闹钟到点时被系统拉起来的那一段：**复核之后才发**，发完（或者不发）都重排下一次。
 *
 * 不导出（manifest 里 android:exported="false"）：这个 receiver 只会由我们自己的
 * PendingIntent 触发，其它应用没有理由给它发广播。
 *
 * 这个类（以及它用到的 DigestScheduler）刻意只依赖 android.* 与 org.json：
 * 系统可能为了投递这个广播**冷启动进程**，那条路径上绝不能碰 WailsBridge ——
 * 它的静态初始化会 System.loadLibrary("wails")，为了念一条通知把整个 Go 运行时
 * 拉起来既没必要，也没人管它的生命周期。
 */
public class DigestAlarmReceiver extends BroadcastReceiver {
    private static final String TAG = "DigestAlarm";

    /**
     * 从 extras 里拿到的触发时刻**比现在晚了**这么多以上就当作不可信。
     *
     * AlarmManager 不会提前触发，所以正常情况下 fire_at_ms 一定 <= now。留 5 分钟
     * 是给「系统时钟被改过」这种事的余量：真的是篡改（adb / root 发的广播）才拦得住。
     */
    private static final long EARLY_TOLERANCE_MS = 5L * 60 * 1000;

    @Override
    public void onReceive(Context context, Intent intent) {
        String result;
        try {
            result = deliver(context, intent);
        } catch (Exception e) {
            Log.e(TAG, "发每日汇总时出错", e);
            result = "failed";
        }

        // 先记这一趟的结果，再重排。顺序是有意的：arm() 排上了就不动 last_result
        // （所以它盖不掉刚才那一条），而它排不上时会写下自己的判定 —— 那是后发生的事，
        // 让它留在文件里才对。
        try {
            DigestScheduler.recordFire(context, result);
        } catch (Exception e) {
            Log.e(TAG, "回写宿主状态失败", e);
        }

        // **无论如何重排下一次。**
        //
        // 一是因为这里的闹钟不是重复闹钟：setExactAndAllowWhileIdle / setWindow 都只响
        // 一次，不重排就只发这一回。
        // 二是因为这是唯一能自愈的地方：设备重启、应用被强停、用户手动清后台之后，
        // 已排的闹钟都没了（闹钟永远不会被备份），而 Go 与宿主之间没有通道 ——
        // 宿主不自己接上，就再也不会响了。
        try {
            DigestScheduler.arm(context);
        } catch (Exception e) {
            Log.e(TAG, "重排下一次汇总失败", e);
        }
    }

    /**
     * 复核 + 发送，返回结果码（DigestScheduler.recordFire 认的那六个值之一）。
     *
     * ── 为什么不信任 extras ──
     *
     * extras 里那几项**不是命令，只是线索**。原因有三条，每条单独都成立：
     *
     *   - 排下闹钟之后，文件可能已经被 Go 重写过（用户改了时间、关掉了提醒、
     *     排程又算了一遍）—— 闹钟手里那份是旧的。
     *   - 系统可能把闹钟推迟得很久（Doze、厂商省电策略），那时候 extras 里的日期
     *     已经不是今天了，而「今天有 5 道」这句话只在它自己那一天成立。
     *   - 这个 receiver 虽然不导出，但 adb / root 能发广播进来。
     *
     * 所以做法是：拿 extras 去**文件里**找出那一条，再按当下的条件重判一遍。
     * 最坏结果是「那天不发」，而不是「发一条数字错的」—— 后者用户没法察觉。
     */
    private String deliver(Context context, Intent intent) {
        long fireAtMs = intent.getLongExtra(DigestScheduler.EXTRA_FIRE_AT_MS, 0L);
        String extraDate = intent.getStringExtra(DigestScheduler.EXTRA_DATE);
        long now = System.currentTimeMillis();

        if (fireAtMs <= 0) {
            // 不是我们排的那次（或者旧版本留下的、没有 extras 的 intent）。
            return "failed";
        }
        if (fireAtMs > now + EARLY_TOLERANCE_MS) {
            Log.w(TAG, "触发时刻在未来，不可信: " + fireAtMs);
            return "skipped_stale";
        }
        if (now - fireAtMs > DigestScheduler.GRACE_MS) {
            // 被推到第二天了。宽限之外这条汇总上写的数字已经是过去那天。
            Log.i(TAG, "超过宽限 " + (DigestScheduler.GRACE_MS / 3600000) + " 小时，不发");
            return "skipped_stale";
        }

        JSONObject schedule = DigestScheduler.readSchedule(context);
        if (schedule == null) {
            return "failed";
        }
        if (!schedule.optBoolean("enabled", false)) {
            return "skipped_disabled";
        }

        JSONObject slot = findSlot(schedule, fireAtMs, extraDate);
        if (slot == null) {
            // 排程已经重算过，这一条不存在了 —— 那就不该再发。
            return "skipped_stale";
        }
        if (!DigestScheduler.today().equals(slot.optString("date", ""))) {
            // 「今天有 N 道」只在 slot 自己那一天成立。
            return "skipped_stale";
        }
        if (slot.optInt("count", 0) <= 0) {
            // 当天没有到期题就不发（验收项）。
            return "skipped_no_due";
        }
        if (!DigestScheduler.notificationsEnabled(context)) {
            // 通知权限没给 / 应用级通知被关 / 这个渠道被关。用户要在设置页看到这一条，
            // 所以不能静默地什么都不做。
            return "skipped_no_permission";
        }

        // 字用**文件里的**而不是 extras 里的：extras 可能是几天前塞进去的旧数值。
        String title = slot.optString("title", "");
        String body = slot.optString("body", "");
        if (title.isEmpty()) {
            title = "错题本";
        }
        postNotification(context, title, body);
        return "posted";
    }

    /**
     * 按 extras 里的时刻与日期，在排程文件里找回那一条。
     *
     * 两个字段都要对上：时刻是身份（同一天重算后时刻会变），日期是防呆
     * （时刻相同但日期不同这种事理论上不会发生，对上了才用它）。
     */
    private static JSONObject findSlot(JSONObject schedule, long fireAtMs, String date) {
        JSONArray slots = schedule.optJSONArray("slots");
        if (slots == null) {
            return null;
        }
        for (int i = 0; i < slots.length(); i++) {
            JSONObject o = slots.optJSONObject(i);
            if (o == null || o.optLong("fire_at_ms", 0L) != fireAtMs) {
                continue;
            }
            if (date != null && !date.equals(o.optString("date", ""))) {
                continue;
            }
            return o;
        }
        return null;
    }

    private void postNotification(Context context, String title, String body) {
        DigestScheduler.ensureChannel(context);

        // 点一下要能进应用。一条点不动的提醒到点响完就没了，用户还得自己去桌面找图标。
        Intent open = new Intent(context, MainActivity.class);
        open.setFlags(Intent.FLAG_ACTIVITY_NEW_TASK | Intent.FLAG_ACTIVITY_CLEAR_TOP);
        int piFlags = PendingIntent.FLAG_UPDATE_CURRENT;
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
            piFlags |= PendingIntent.FLAG_IMMUTABLE;
        }
        PendingIntent contentIntent =
                PendingIntent.getActivity(context, 0, open, piFlags);

        Notification n = new NotificationCompat.Builder(context, DigestScheduler.CHANNEL_ID)
                // 没有自己的图标资源（res 不在这一轮的改动范围里），用系统那个，
                // 与脚手架 postNotification 的选择一致。
                .setSmallIcon(android.R.drawable.ic_dialog_info)
                .setContentTitle(title)
                .setContentText(body)
                .setContentIntent(contentIntent)
                .setAutoCancel(true)
                .setPriority(NotificationCompat.PRIORITY_DEFAULT)
                .build();

        NotificationManager nm =
                (NotificationManager) context.getSystemService(Context.NOTIFICATION_SERVICE);
        if (nm == null) {
            throw new IllegalStateException("没有 NotificationManager");
        }
        // id 固定：同一天被触发两次（比如闹钟被推迟后又响了一次）也是覆盖，不叠加。
        nm.notify(DigestScheduler.NOTIFICATION_ID, n);
    }
}
