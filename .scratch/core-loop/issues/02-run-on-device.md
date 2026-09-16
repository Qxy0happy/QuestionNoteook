# 02: 在真机上跑起来

**What to build:** 把 `wails3 task android:run:device` 这条路径打通，让打好的 APK 能装到那台真机上并启动，看到三页骨架。这是后面**每一张票的验证前提** —— 不先打通，03 之后的票全都"写完了但验不了"。

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [x] `ADB` 与 `EMULATOR` 变量里那套 `$HOME/Android/Sdk` 的 Unix 路径回退修好（与 NDK 那处是同类问题）
- [x] 需要的 Unix 工具（如 `awk`）在该内置 shell 里确实可用，不可用则换掉
- [ ] `wails3 task android:run:device` 在插着的真机上完成构建、安装、启动
- [ ] 手机上打开应用，看到三页骨架，默认落在中间那页（拍照）
- [ ] 左右滑动能看到 Agent 与题库两页

## Comments

**2026-09-16 · 代码部分完成，真机验证待做**

变量与脚本部分已改完并验证：

- `SDK_ROOT` 归一化反斜杠；`ADB` / `EMULATOR` / `AVDMANAGER` 三个变量都从它派生
- 新增 `EXE_SUFFIX` / `BAT_SUFFIX`（SDK 是 `.exe`，cmdline-tools 是 `.bat`）
- `AVDMANAGER` 去掉 `sort -V`（该内置 shell 里 sort 落到 Windows 的 sort，静默吐空）
- `run:device` / `deploy-device` 去掉 `awk`（该 shell 没有），改用 tail/tr + read

**已验证**：`device:list` 能调起 `D:/Cache/Android/sdk/platform-tools/adb.exe`；
`assemble:apk` BUILD SUCCESSFUL；取设备那段对 5 种输入验过；三个拼出的路径确认存在。

**仍未验**：真机上安装并启动那三步 —— 需要手机插着。
