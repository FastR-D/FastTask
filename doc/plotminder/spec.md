**P1 · Foundation**
搭建 Flutter 项目骨架,集成本地 SQLite (drift 或 sqflite),实现 Task / View / Axis / Edge 四个核心数据模型,确保"多 view 单 task"不变式在数据层强制成立(同一 task 在不同 view 上坐标独立、其余字段共享)。提供 task 的本地 CRUD、view 的创建与切换、JSON 双写导出,同时跑通单元测试覆盖宪章 III.5 的不变式。此阶段无完整 UI,仅有最简调试页验证数据流。

---

**P2 · MapView**
实现产品的视觉灵魂:一张可缩放、可拖拽的 2D 决策地图。默认载入 Eisenhower view (重要 × 紧急),task 渲染为节点(位置=坐标、大小=duration、颜色=状态、透明度=陈旧度),并实现 zoom-aware LOD 渲染——缩小时节点退化为色点,中等比例尺显示标题,放大后展开描述与元数据,过渡使用淡入淡出动画。支持单节点连续拖拽、Shift / Cmd 限制单轴拖动、框选批量调整、双指缩放、双击进 detail panel、勾选完成时的褪色动画。此阶段交付 The Open / The Sort / The Complete 三个用户时刻,确保拖拽与 LOD 切换延迟稳定在 16ms 内。

---

**P3 · Smart Axes & Capture**
开放 view 配置:用户可自定义两条轴的名称、值域、ideal corner,并保存为预设 (Eisenhower / Impact-Effort / 兴趣×商业 等)。同时实现 Capture 浮窗——desktop 走全局快捷键 (Cmd / Ctrl+Space)、iOS 走 Share Sheet + Siri Shortcut + Widget、Android 走 Share Intent,自然语言输入后调用 LLM 异步推断当前 view 的 (x, y) 坐标并回填,LLM 不可用时落到地图中心。此阶段交付 The Capture 时刻,P95 全链路必须 ≤ 5 秒(宪章 VI)。

---

**P4 · Path View**
在 MapView 上叠加"下一站"高亮与 3–5 步推荐路径线,每段路径附一句 reasoning label。提供 DAG 依赖关系编辑 UI(从一个节点拖出箭头到另一个节点),路径计算严格遵循宪章 II.3 的优先级链(依赖 → Pareto frontier → 距 ideal corner 距离 → 用户权重),完全不依赖时间。此阶段交付 The Decide 这个 killer moment,demo 视频围绕这一帧拍摄。

---

**P5 · Imports & Sync**
Flutter 端通过 platform channel 接入四套外部数据源:Apple EventKit (Reminders + Calendar + Notes)、Google OAuth (Calendar + Tasks)、Microsoft Graph (Outlook + To Do + OneNote)、Todoist / Notion / Linear REST API。每个 task 的 `sources` 字段在 detail panel 中可见,冲突时弹 UI 让用户决定(不静默 last-write-wins),change history 可查。Onboarding 流程接入此能力,让用户首次进入即可一键导入既有数据。

---

**P6 · Live Mode & Reflect**
实现两个互补功能:一是 Live mode,在路径执行中提供被动的时间提醒与可选番茄追踪 UI(纯被动呈现,不重排路径,符合宪章 II.5);二是 Reflect,每周日 20:00(用户可配置)自动弹出周复盘,包含本周地图 vs 上周地图对比、7 天演变动画、已完成节点在 axis-space 的分布热力图、AI 一句话趋势总结(建议口吻)。此阶段交付 The Reflect 时刻,是产品长期留存的核心钩子。

---

**P7 · 3D View**
扩展 MapView 支持可选的第三条轴,用户可在 view 配置中开启 3D 模式。Flutter 端实现 3D 节点渲染、轨道相机、旋转与缩放手势、3D 下的路径线绘制、节点的视觉编码在新维度上的延展。提供 2D ↔ 3D 平滑过渡动画,在 2D 模式下第三轴的值作为隐藏属性保留。

---

**P8 · Cloud Sync**
Flutter 端接入云同步层(通过宪章预留的 `SyncProvider` interface),实现登录 / 设备管理 / 同步状态指示 / 冲突解决 UI,默认 opt-in、可随时退出且不影响本地核心功能(宪章 V.3)。同步策略为 last-write-wins + 完整 change history,跨设备冲突时弹 UI 让用户裁决。此阶段为未来团队协作(v2)做架构铺垫,但本身不引入多人编辑。
