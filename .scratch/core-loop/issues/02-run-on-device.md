# 02: 在真机上跑起来

**What to build:** 把 `wails3 task android:run:device` 这条路径打通，让打好的 APK 能装到那台真机上并启动，看到三页骨架。这是后面**每一张票的验证前提** —— 不先打通，03 之后的票全都"写完了但验不了"。

**Blocked by:** None (can start immediately)

**Status:** ready-for-agent

- [ ] `ADB` 与 `EMULATOR` 变量里那套 `$HOME/Android/Sdk` 的 Unix 路径回退修好（与 NDK 那处是同类问题）
- [ ] 需要的 Unix 工具（如 `awk`）在该内置 shell 里确实可用，不可用则换掉
- [ ] `wails3 task android:run:device` 在插着的真机上完成构建、安装、启动
- [ ] 手机上打开应用，看到三页骨架，默认落在中间那页（拍照）
- [ ] 左右滑动能看到 Agent 与题库两页
