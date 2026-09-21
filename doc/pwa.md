# FastTask 移动端 PWA 规格

> 文档状态：初版设计  
> 适用范围：Web 客户端的可安装形态与离线行为  
> 相关：[`doc/frontend.md`](frontend.md)、[`doc/agent-impl.md`](agent-impl.md)  
> 依赖基线：`vite-plugin-pwa` 1.3.0

## 1. 目标与范围

把现有 Web 界面做成可安装的 PWA，目标是**手机上随手打开就能看今天三件事、记一次推进、和 agent 说一句话**。

范围内：

- 可安装（manifest、图标、启动画面）。
- 离线时仍能查看今日计划与任务树的最近一次快照。
- 应用更新可控，不出现半新半旧的资源。
- 移动端网络不稳时对话运行可续。

**范围外**（本期不做，出现真实需求再立 ADR）：

- Web Push 通知。
- 后台同步（Background Sync）。
- 离线写入队列，理由见 §5。
- 独立的原生应用壳。

## 2. 必须先修掉的阻塞项

以下五项是查证过的现存问题，**不修则 PWA 无法工作或体验严重受损**：

| # | 问题 | 位置 | 影响 |
|---|---|---|---|
| 1 | `static()` 只挂载了 `/assets`，根路径文件落进 `NoRoute` 返回 `index.html` | `server.go:1546` | `/sw.js`、`/manifest.webmanifest` 会返回 HTML，**Service Worker 注册必然失败** |
| 2 | token 存在 `sessionStorage` | `api.ts:10-12`、`api.ts:35-52` | PWA 每次冷启动都要重新登录，安装后体验不可用 |
| 3 | 录音硬编码 `audio/webm` | `App.tsx:240` | **iOS Safari 的 MediaRecorder 只产出 mp4/aac**，语音功能在 iPhone 上必坏 |
| 4 | `http.Server.WriteTimeout: 60s` | `main.go:95` | 掐断超过 60 秒的 SSE 流；PWA 场景下长运行是常态 |
| 5 | 无 manifest、无 Service Worker、无图标 | — | 不满足可安装条件。注意 `index.html` 已有 `viewport-fit=cover`、`mobile-web-app-capable` 和 `apple-mobile-web-app-*`，且 CSS 已用 `env(safe-area-inset-*)`，这部分基础在 mdui 重写中已铺好 |

第 3 项与 PWA 无关也应当修——它现在就是个 iPhone 上的功能性缺陷。正确做法是用 `MediaRecorder.isTypeSupported` 协商容器格式，并把实际 MIME 与扩展名一起传给后端，而不是写死 `voice.webm`。

## 3. Manifest 与 Service Worker

### 3.1 Manifest

- `display: "standalone"`，`orientation: "portrait"`，`start_url: "/"`，`scope: "/"`。
- `theme_color` 与 `index.html` 现有的两条 `theme-color`（浅色 `#fef7ff` / 深色 `#141218`）保持一致。注意 manifest 只能声明单一 `theme_color`，取浅色值，深色继续由 meta 的媒体查询覆盖。
- 图标至少提供 192 与 512 两种尺寸，外加一张 `purpose: "maskable"`。
- `lang: "zh-CN"`。

### 3.2 Service Worker

用 `vite-plugin-pwa`（Workbox）生成，策略：

| 资源 | 策略 |
|---|---|
| `/assets/*`（带哈希） | CacheFirst，长期缓存。服务端已设 `immutable`（`server.go:63`） |
| `index.html` | NetworkFirst。服务端已设 `no-store`（`server.go:65`），符合要求 |
| `GET /api/v1/daily-plans/current`、`/goals`、`/goals/{id}/task-tree` | StaleWhileRevalidate，**且必须按用户隔离缓存键** |
| 其余 `/api/**` | NetworkOnly |
| `/api/v1/agent/**` | **NetworkOnly，且必须显式排除**。SSE 流绝不能进 Workbox 缓存 |

三条强制要求：

- **登出时必须清空所有缓存。** 否则下一个登录者可能看到上一个用户的数据。这是共享设备场景下的真实风险。
- 更新策略用 `autoUpdate` 配合显式的「有新版本，点击刷新」提示，不做静默强制刷新——用户可能正在对话中。
- Service Worker 的作用域是 `/`，需要阻塞项 #1 修复后才能正确注册。

## 4. 认证持久化

阻塞项 #2 的处理方案：

- access token 留在内存（不落盘），refresh token 存 `localStorage`。
- 保留现有的 refresh token 轮换机制和复用检测（README「安全边界」：旧 token 复用触发会话族撤销）。
- 登出、401 且刷新失败、以及检测到 token 复用时，一律清空 `localStorage` 与全部 Cache Storage。

**安全权衡要说清楚**：`localStorage` 相比 `sessionStorage` 扩大了 XSS 下的暴露窗口。接受这个权衡的前提是现有的 CSP（`server.go:61`）已经禁止内联脚本和外部脚本源，且 refresh token 有轮换与复用检测兜底。如果后续放宽 CSP，需要重新评估本决定。

## 5. 离线策略

**设计决策：v1 只做离线只读，不做离线写入队列。**（可由后续 ADR 调整）

离线可用：

- 今日计划与计划项的最近一次快照。
- 任务树的最近一次快照。
- 已加载的对话历史。

离线不可用（明确提示「需要联网」，而不是静默失败）：

- 任何写操作。
- Agent 对话。

理由：FastTask 的写操作全部带 `Idempotency-Key` 和 `If-Match` 乐观锁（`arch.md` §11.2、§11.3）。离线排队的写请求在重新联网时 revision 很可能已经过期，会大批量返回 `412`，产生一个需要用户逐条处理的冲突收件箱。**这个代价远大于「地铁里也能打卡」带来的收益**，尤其考虑到产品的核心动作是「每天三件事」而不是高频录入。

## 6. iOS 限制

已知需要在实现和测试中考虑的：

- MediaRecorder 只支持 mp4/aac 容器，见阻塞项 #3。
- Web Push 需要 iOS 16.4+ 且必须先添加到主屏幕。本期不做推送，记录备查。
- 安装后的 PWA 有独立的存储沙箱，Safari 里的登录态不会带过去，首次打开需要重新登录一次。
- 需要补 `apple-touch-icon`。`viewport-fit=cover` 与安全区域内边距已在 mdui 重写中处理。

## 7. 服务端改动清单

| 改动 | 位置 |
|---|---|
| 显式路由 `/manifest.webmanifest`、`/sw.js`、`/registerSW.js`、图标文件，带正确 Content-Type | `server.go:1546` `static()` |
| `WriteTimeout` 改为 `0`，改用 `http.ResponseController` 按请求设置 | `main.go:95` |
| 确认 CSP 允许 Service Worker（`worker-src` 未设，回落到 `default-src 'self'`，当前满足；若后续收紧 `default-src` 需显式补 `worker-src 'self'`） | `server.go:61` |
| manifest 与 sw.js 的 `Cache-Control` 保持 `no-store`（现有规则已覆盖） | `server.go:65` |

## 8. 验收

- Chrome Lighthouse 的 PWA 安装性检查通过。
- iPhone Safari 与 Android Chrome 均可添加到主屏幕并独立启动。
- 400px 宽度下无横向滚动，安全区域正确。
- 飞行模式下打开应用，今日计划仍可查看，写操作给出明确的联网提示。
- 登出后 Cache Storage 为空。
- 锁屏 2 分钟再回到应用，进行中的 agent 运行能续上且不重复消息。
- iPhone 上录音能成功转写（阻塞项 #3 已修）。
