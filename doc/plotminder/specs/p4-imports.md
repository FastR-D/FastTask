# Feature Spec: P4 Imports & Integrations

**Phase**: P4
**Milestone**: v1.0 Public
**Status**: Draft
**Created**: 2026-05-19
**Constitution Compliance**: v1.0.0
**Dependencies**: P0, P1, P2, P3

---

## 1. 概述

P4 把 Plotminder 从"独立应用"变成"用户现有生态的一部分"。三件事:**统一 adapter 架构、四大生态(Apple / Google / Microsoft / 第三方)双向同步、原生 capture 入口(Share Sheet / Siri / Widget)**。

P4 不创造新的核心体验,但解决一个关键问题:**用户已经在 Apple Reminders 里有 200 条 task,他不会为了试 Plotminder 重新输入一遍**。冷启动的友好度由 P4 决定。

成功标志:**用户首次启动,5 分钟内能从 Apple Reminders / Google Tasks / Todoist 中导入现有 task,LLM 自动落点,看到第一张有内容的地图**。

---

## 2. 用户故事

- **US-P4-1** 作为 macOS/iOS 用户,我能授权 Plotminder 读写我的 Apple Reminders,从中选择列表导入。
- **US-P4-2** 作为 Google 用户,我能用 OAuth 授权 Plotminder 访问 Google Calendar 和 Google Tasks。
- **US-P4-3** 作为 Microsoft 365 用户,我能授权访问 Outlook + To Do + OneNote。
- **US-P4-4** 作为 Todoist / Notion / Linear 用户,我能用 API key / OAuth 授权连接对应账户。
- **US-P4-5** 作为用户,我导入的 task 在 Plotminder 中的更改能选择性写回原平台。
- **US-P4-6** 作为用户,当原平台和 Plotminder 都修改了同一个 task,我能看到冲突并决定保留哪一边。
- **US-P4-7** 作为 iOS 用户,我能在任何应用中通过 Share Sheet 把文字、链接、邮件主题快速添加为 Plotminder task。
- **US-P4-8** 作为 iOS 用户,我能在开车路上对 Siri 说"提醒我办车险",task 自动进入 Plotminder。
- **US-P4-9** 作为 iOS / macOS 用户,我能在首屏放一个 Widget,直接看到地图缩略 + 快速 capture 入口。

---

## 3. 功能需求

### 3.1 统一 Adapter 架构

- **FR-P4-001** 每个外部平台一个 adapter,实现统一接口:
  ```
  interface PlatformAdapter {
    auth(): AuthResult
    fetch(since: Timestamp): RemoteItem[]
    push(item: LocalChange): PushResult
    subscribe(callback): Subscription  // 可选,基于 webhook 或 polling
  }
  ```
- **FR-P4-002** 每个 Plotminder task 携带 `sources: [{platform, external_id, last_synced_at, etag?}]` 字段(已在 P0 数据模型中预留)。
- **FR-P4-003** 同一 task 可来源于多个平台(例如 Google Calendar 事件同时是 Outlook 日历项);adapter 需提供 dedup hint。
- **FR-P4-004** Adapter 不直接操作 task 数据模型,而通过 Go core 的标准 API,保证一致性约束(III.5)。

### 3.2 Apple 生态 (优先级 1)

- **FR-P4-005** 通过 EventKit 集成 Reminders + Calendar。
- **FR-P4-006** 通过 AppleScript / Shortcuts 集成 Notes(Notes 没有官方 API)。
- **FR-P4-007** 双向同步:
  - 导入 Reminders → 创建 task
  - Plotminder 中改 status → 同步回 Reminders 的 done 标记(若用户开启)
  - 删除 Plotminder task → 默认**不**删除原 Reminders 项,弹 UI 询问

### 3.3 Google 生态 (优先级 2)

- **FR-P4-008** OAuth 2.0 授权(在系统浏览器中,而非 in-app webview,避免 Google policy 限制)。
- **FR-P4-009** 接入 Google Calendar Events + Google Tasks。
- **FR-P4-010** Token 刷新机制 + Token 撤销时的用户提示。

### 3.4 Microsoft 365 (优先级 3)

- **FR-P4-011** 通过 Microsoft Graph API 接入 Outlook(Calendar + Mail flag)、To Do、OneNote pages 中带 `[]` 复选框的项。
- **FR-P4-012** OAuth 与 token 管理同 Google。

### 3.5 第三方 Task 工具 (优先级 4)

- **FR-P4-013** Todoist:REST API + OAuth。
- **FR-P4-014** Notion:Integration token,扫描指定数据库,把行作为 task 导入。
- **FR-P4-015** Linear:GraphQL API + OAuth,issues 作为 task 导入。

### 3.6 冲突解决

- **FR-P4-016** 默认策略:**last-write-wins**,以 `updated_at` 比较。
- **FR-P4-017** 同步**必须**保留 change history:每个 task 维护一个 changelog,记录每次同步的 source、before、after、winner。
- **FR-P4-018** 冲突 UI:当冲突无法 last-write-wins 自动解决(例如时间戳相近但字段冲突),弹出 modal 让用户选择:
  - 保留本地版本
  - 保留远端版本
  - 手动合并
  - 推迟决定(标记为 pending)
- **FR-P4-019** 冲突 UI **永不**默认勾选某一选项,避免误点。

### 3.7 Capture 入口扩展

- **FR-P4-020** iOS Share Sheet extension:接收文本/URL/图片,作为 task 创建;沿用宪章 VI 的 5 秒原则。
- **FR-P4-021** Siri Shortcut + iOS App Intents:支持自然语言"提醒我办车险"、"加个 task 写论文摘要"。
- **FR-P4-022** iOS / macOS Widget:
  - 小尺寸:显示下一站 task + 一句话 rationale
  - 中尺寸:加上地图缩略图(当前 view 的右上 frontier)
  - 大尺寸:加上未来 3 步路径
  - Widget 上的"+ 新建"按钮一键打开 capture 浮窗
- **FR-P4-023** Android Quick Settings tile + Share Intent + Widget。

### 3.8 反向写入(可选)

- **FR-P4-024** Settings 中,用户能为每个 source 配置**是否反向写入**(默认关)。
- **FR-P4-025** 当用户接受一条推荐路径,可选地把该路径的 task 写入 Calendar(以 event 形式,带 Plotminder deeplink)。
- **FR-P4-026** 反向写入的 calendar event 标题/描述模板可定制。

### 3.9 集成失败处理

- **FR-P4-027** 任何外部 API 失败(超时、auth 过期、限流)不得阻塞 Plotminder 核心功能(宪章约束 C)。
- **FR-P4-028** 失败的同步进入 retry 队列,带指数退避;连续失败 N 次后标记 source 为 degraded,提示用户重新授权。
- **FR-P4-029** 在 Settings → Integrations 页面显示每个 source 的健康状态、最近同步时间、错误日志。

---

## 4. 非功能需求

- **NFR-P4-001** 首次连接 source(例如 Apple Reminders 有 200 条 item)的完整导入 P95 < 60 秒。
- **NFR-P4-002** 增量同步 P95 < 5 秒。
- **NFR-P4-003** Share Sheet → task 入库 P95 < 5 秒(宪章 VI.1 必须满足)。
- **NFR-P4-004** OAuth token、API key 必须存于平台安全存储(Keychain / Credential Manager)。
- **NFR-P4-005** Adapter 必须可独立单元测试,网络请求通过 mock 注入。
- **NFR-P4-006** 用户撤销授权后,Plotminder 30 秒内停止该 source 的所有同步操作。

---

## 5. 验收标准

- **AC-P4-1** 从 Apple Reminders 导入 100 条 task,在 Plotminder 中能完整看到所有内容,且每条 task 的 `sources` 字段正确记录 external_id。
- **AC-P4-2** 在 Plotminder 中把一个导入自 Reminders 的 task 标记为 done,Reminders 中对应项也变为 completed(若反向写入开启)。
- **AC-P4-3** 同时在 Plotminder 和原平台修改同一 task 不同字段,触发冲突 UI,用户选择保留任一方后双方收敛。
- **AC-P4-4** iOS Share Sheet 选择 Plotminder 添加 task,5 秒内入库,LLM 推断坐标在后台进行。
- **AC-P4-5** Widget 显示的下一站 task 与应用内 The Decide 推荐一致(无延迟超过 1 个 update 周期)。
- **AC-P4-6** 故意切断网络后,所有外部同步失败,但 Plotminder 内部所有功能(创建、拖拽、推荐路径、Reflect 雏形)仍可用。
- **AC-P4-7** 删除 Plotminder 中某 task,系统询问是否同时删除原平台对应项,默认选项为"否"。
- **AC-P4-8** OAuth token 过期后,系统弹出明确提示 + 重新授权按钮,不静默失败。

---

## 6. 范围

### In Scope
- 统一 adapter 架构;Apple / Google / Microsoft / Todoist / Notion / Linear 集成;冲突 UI;Share Sheet / Siri / Widget;反向写入(可选)。

### Out of Scope(本 phase)
- 实时协作 / 团队 workspace 同步(超出 v1.0)
- 端到端加密的多设备同步(留给 P7)
- 邮件直接转 task(Gmail/Outlook 邮件 → task,后续考虑)
- 文件附件同步

---

## 7. 依赖

- **前置 phase**: P0、P1、P2、P3
- **外部**: EventKit (Apple)、Google APIs、Microsoft Graph、Todoist API、Notion API、Linear GraphQL、iOS App Intents、Android Intent System

---

## 8. Constitution Alignment

| 宪章条款 | 关系 | 处理 |
|---------|------|------|
| 约束 C (集成层) | **直接落地** | FR-P4-001、FR-P4-016~018、FR-P4-027~029 完整对应 |
| V.2 (集成故障不阻塞核心) | **直接落地** | FR-P4-027、AC-P4-6 |
| VI.2 (capture 入口必须包含 Share Sheet / Siri / Widget) | **直接落地** | FR-P4-020~023 |
| VI.1 (Share Sheet capture P95 < 5s) | **直接落地** | NFR-P4-003 |
| I.3 (永不偷偷改数据) | **关键保护** | 反向写入默认关;删除询问;冲突 UI 不默认勾选;FR-P4-019、FR-P4-024 |
| V.4 (数据格式可往返) | **直接落地** | `sources` 字段记录完整;changelog 记录所有同步 |

**潜在张力点**

- **关于 last-write-wins**: 这条策略在大多数情况下用户无感,但偶尔会"自动覆盖用户的更改"。处理:**必须有 changelog 留痕**(FR-P4-017),用户事后可在 Settings 找到完整变更历史并回滚;同时冲突 UI 在时间戳接近时强制询问(FR-P4-018),避免在用户高频编辑时静默丢失数据。
- **关于反向写入 Calendar**: 这看起来像"把推荐路径同步成日历事件",有点接近时间块/日程工具行为。处理:**严格 opt-in**(FR-P4-024 默认关),且只是写一个 event,不影响 Plotminder 内部的路径推荐算法(算法仍纯坐标驱动)。

---

## 9. 开放问题

- **OQ-P4-1** Notion 集成的"哪些 page 当作 task"边界:Database 行 vs page 中带复选框的项?需要用户配置 UX 设计。
- **OQ-P4-2** Apple Notes 集成的可行性:无官方 API,Shortcuts 路径稳定性堪忧;是否值得做。
- **OQ-P4-3** Adapter 是否要支持用户自建的 Webhook source?(灵活但增加支持成本)
- **OQ-P4-4** Share Sheet 入库后的 task 默认归属哪个 view?(目前假设当前 active view,但用户可能想要"收件箱" view)
- **OQ-P4-5** Token 加密存储在某些 Linux 发行版无 Keychain 等价物,如何处理?
