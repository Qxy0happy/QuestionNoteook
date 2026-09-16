# 03: 取景与快门

**What to build:** 打开应用就是取景画面，画面中心有一个固定的圆角矩形取景框与十字准星帮助对准；按下快门拿到一帧并显示出来。

**Blocked by:** 02（在真机上跑起来）

**Status:** ready-for-agent

- [ ] 打开应用直接进入取景，不需要额外点击
- [ ] 取景框固定居中、带中心十字，**不随画面内容变化**（不做边缘检测）
- [ ] 快门能拿到一帧并在界面上显示
- [ ] 真机上验过（前端属缝外，无自动化测试）

## Comments

**2026-09-16 · 前端完成，真机验证待做**

取景页（`frontend/src/Capture.svelte`）实现完毕：挂载即开镜、固定居中取景框与中心十字、快门抓帧。

三处要点：

- 抓帧按 `object-fit: cover` 的逆运算裁到与预览可见区一致 —— 否则手机竖屏 + 横画幅传感器下，预览是一条竖窄带、拍下来却是整张横画幅，HUD 的对准意义就没了
- 帧拆成两份：`frame`（无损 canvas，交下游）与 `frameUrl`（JPEG，只给显示）
- `IntersectionObserver` 做到不可见就停流

**已验证**：svelte-check 0 errors、vite build 通过。
**未验证**：真机渲染、IO 起停节拍、`facingMode: 'environment'` 是否被接受、前置摄像头镜像问题。
