package com.wails.app;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.util.Log;

/**
 * 开机 / 应用被更新之后重排每日汇总。
 *
 * ── 为什么必须有这个类 ──
 *
 * **闹钟不跨重启。** AlarmManager 里的待触发闹钟活在系统内存里，重启就全没了；
 * 而且它**永远不会被备份**（manifest 里的 allowBackup="true" 救不了它，
 * 备份只搬文件，搬不动一个已排的系统闹钟）。重装、清数据、恢复备份之后同理。
 *
 * 于是「排程能自愈」这件事只有两个入口：MainActivity 每次启动 arm 一次，
 * 以及这里。少了任何一个，用户都可能永远收不到提醒而完全不知道为什么。
 *
 * 收 BOOT_COMPLETED（开机）与 MY_PACKAGE_REPLACED（本应用被覆盖安装 ——
 * 覆盖安装同样会把已排的闹钟清掉）。
 *
 * 两个都是系统广播。manifest 里这个 receiver 是 exported="false"，那**不影响**
 * 收系统广播：非导出的接收器仍然收得到系统发来的消息，只是别的应用发不进来。
 */
public class DigestBootReceiver extends BroadcastReceiver {
    private static final String TAG = "DigestBoot";

    @Override
    public void onReceive(Context context, Intent intent) {
        String action = intent != null ? intent.getAction() : null;
        Log.i(TAG, "收到 " + action + "，重排每日汇总");
        try {
            // arm 是幂等的（同一个请求码会覆盖），所以「开机」与「覆盖安装」走同一条路，
            // 不必分辨也不用去重。
            DigestScheduler.arm(context);
        } catch (Exception e) {
            // 这里没有重试的机会。用户下次打开应用时 MainActivity 还会再排一次，
            // 所以失败就只记一条日志，不要让它冒出去（receiver 抛异常会被系统当成 ANR 线索）。
            Log.e(TAG, "开机后排程失败", e);
        }
    }
}
