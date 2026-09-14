# Feature Spec: P0 Foundation

**Phase**: P0
**Milestone**: Alpha (Internal)
**Status**: Draft
**Created**: 2026-05-19
**Constitution Compliance**: v1.0.0
**Dependencies**: 无(本 phase 是项目基石)

---

## 1. 概述

P0 建立 Plotminder 的最底层结构:数据模型、本地持久化、跨平台运行骨架。本 phase 不提供完整的用户体验——它的成功标志是**一个开发者能在三个平台(macOS、iOS、Android)上启动应用,创建一个 task,关闭应用,重启后看见该 task 仍然存在,并能从用户文件夹中以可读形式找到该数据**。

P0 是后续所有 phase 的地基。任何 P0 阶段的妥协都会以利息形式在后续 phase 偿还。

---

## 2. 用户故事

由于 P0 是基础设施 phase,用户故事以**开发者**和**早期 dogfooder** 为主体:

- **US-P0-1** 作为开发者,我能在 macOS / Windows / Linux / iOS / Android 上启动 Plotminder shell,看到一个空的工作区。
- **US-P0-2** 作为 dogfooder,我能创建一个 task,关闭应用,重启后该 task 仍然存在。
- **US-P0-3** 作为关心数据主权的用户,我能在用户文件夹中找到一个**可读的、可被 Git 追踪的**数据目录,理解每个文件代表什么。
- **US-P0-4** 作为开发者,我能在没有 LLM 服务、没有网络的环境下完整使用 P0 提供的所有能力。
- **US-P0-5** 作为开发者,我能通过命令行或文件导出/导入完整的工作区数据,且导出–导入往返无损失。

---

## 3. 功能需求

### 3.1 数据模型 (Core Entities)

- **FR-P0-001** 必须定义并实现以下核心实体:
  - `Workspace` — 顶层容器,包含若干 view 与 task 集合
  - `Task` — 底层唯一对象,包含 `id`、`title`、`description`、`note`、`status`、`tags`、`created_at`、`updated_at`、`completed_at`
  - `View` — 观察 task 的视角,包含 `id`、`name`、`axes`(待 P2 扩展)、`included_task_ids` 或 filter rule
  - `TaskViewPlacement` — 每个 (task, view) 对的坐标记录,包含 `task_id`、`view_id`、`x`、`y`、`z?`
  - `Dependency` — DAG 边,包含 `from_task_id`、`to_task_id`(`to` 依赖 `from`,即 `from` 完成后 `to` 可激活)
  - `Tag`、`Project` — 标签与项目分类,跨 view 共享
- **FR-P0-002** Task 的状态枚举必须为:`todo` / `doing` / `done` / `archived`。
- **FR-P0-003** 所有实体必须使用 UUID v7(或等价的可排序 UUID)作为 `id`,避免后续云同步时的主键冲突。
- **FR-P0-004** 所有实体必须包含 `created_at` 与 `updated_at` 时间戳(UTC)。
- **FR-P0-005** Task 与 view 之间的关系必须支持:**一个 task 可同时归属任意数量的 view,每个 view 上有独立的坐标记录**(宪章 III.2)。

### 3.2 持久化

- **FR-P0-006** 数据必须双写:**SQLite 作为主索引、JSON 作为可读快照**。
- **FR-P0-007** JSON 必须存储在用户文件夹中**可见、可被 Git 版本控制**的目录(默认 `~/Plotminder/workspaces/{workspace_id}/`)。
- **FR-P0-008** JSON 文件组织建议(具体目录布局由 plan 决定):
  - 每个 task 一个 JSON 文件(便于 diff)
  - 每个 view 一个 JSON 文件(包含轴配置与 placement)
  - 一个 manifest 文件(workspace 元信息、schema 版本)
- **FR-P0-009** 当 SQLite 与 JSON 不一致时,以 **JSON 为权威源**(JSON 是用户可见可编辑的;SQLite 是缓存索引)。启动时若检测不一致,触发重建索引。
- **FR-P0-010** 必须有 schema 版本号机制,支持未来的迁移。

### 3.3 跨平台运行骨架

- **FR-P0-011** Flutter UI 层 + Go 共享核心库 (FFI/native bridge)。
- **FR-P0-012** 所有数据模型、持久化、业务逻辑在 Go 层实现;UI 层不直接操作 SQLite/JSON。
- **FR-P0-013** macOS / Windows / Linux / iOS / Android 五个平台必须都能启动 Plotminder shell,创建 task,持久化。
- **FR-P0-014** P0 不要求复杂 UI,但需要最小可演示 shell:能看到 task 列表(简单列表即可,不要求地图)、能新建 task、能切换状态。

### 3.4 基础 CRUD API

- **FR-P0-015** Go core 必须暴露以下 API 给 Flutter UI:
  - `task.create(title, view_id, [coordinate])` → returns task
  - `task.update(id, fields)`
  - `task.delete(id)`(实际为 archive,保留 30 天)
  - `task.list(filter)` → returns tasks
  - `view.create(name)`、`view.update`、`view.list`
  - `placement.upsert(task_id, view_id, x, y, [z])`
  - `dependency.add(from, to)`、`dependency.remove(from, to)`、`dependency.detect_cycle(from, to)` → bool

### 3.5 导出与导入

- **FR-P0-016** 必须支持 workspace 完整导出为单个 `.zip`(包含 JSON 树状结构)。
- **FR-P0-017** 必须支持从 `.zip` 导入,验证 schema 版本,处理冲突(默认放弃导入并报错,后续可加入合并策略)。
- **FR-P0-018** 导出–重新导入的往返必须 100% 无损失(宪章质量底线"数据导出完整性")。

### 3.6 工作区与首次启动

- **FR-P0-019** 首次启动时自动创建 default workspace 与一个空的 default view(具体 default view 配置见 P1)。
- **FR-P0-020** P0 阶段限定**单 workspace**;多 workspace 在 P2/P4 启用付费计划后扩展。

---

## 4. 非功能需求

- **NFR-P0-001** 启动到可交互界面 < 2 秒(冷启动,中型数据集 500 tasks)。
- **NFR-P0-002** P0 在完全离线状态下 100% 功能可用(宪章 V.2)。
- **NFR-P0-003** SQLite 写入操作 P95 < 50ms(中型数据集)。
- **NFR-P0-004** 所有数据访问通过 Go core,UI 层零 SQL/文件 I/O。
- **NFR-P0-005** Go core 单元测试覆盖率 ≥ 80%(数据层是核心,松不得)。
- **NFR-P0-006** 提供 CLI 工具 `plotminder-cli`,支持基础 CRUD + 导入导出,用于开发者 dogfooding。

---

## 5. 验收标准

P0 完成当且仅当以下标准全部通过:

- **AC-P0-1** 在 macOS、Windows、Linux、iOS、Android 五个平台上启动应用,均能完成"创建 task → 关闭 → 重启 → task 仍在"循环。
- **AC-P0-2** 用户能在文件系统中打开 `~/Plotminder/`,看到清晰命名的 JSON 文件,且能用任意文本编辑器查看其内容。
- **AC-P0-3** 在文本编辑器中手动编辑某个 task JSON 文件后,下一次启动 Plotminder 能识别变更(JSON 作为权威源,SQLite 重建)。
- **AC-P0-4** 导出 workspace → 删除本地数据 → 重新导入 → 数据 100% 还原。
- **AC-P0-5** 整个流程在无网络环境下可完成。
- **AC-P0-6** CLI 工具支持以下命令:`task add`、`task list`、`task done`、`workspace export`、`workspace import`。
- **AC-P0-7** 故意造成 SQLite 与 JSON 不一致(手动改 JSON),重启后系统能正确以 JSON 为权威重建。

---

## 6. 范围

### In Scope
- 数据模型、SQLite + JSON 双写、跨平台 shell、基础 CRUD、导入导出、CLI 工具。

### Out of Scope(本 phase)
- 地图渲染、拖拽、视觉编码(留给 P1)
- 自定义轴、多 view、LLM 打分(留给 P2)
- 路径推荐(留给 P3)
- 外部集成(留给 P4)
- 云同步(留给 P7)
- 团队协作(超出 v1.0)

---

## 7. 依赖

- **外部**: Flutter SDK、Go toolchain、SQLite。
- **内部**: 无。

---

## 8. Constitution Alignment

| 宪章条款 | 关系 | 处理 |
|---------|------|------|
| V.1–V.4 (Local-First & 数据主权) | **直接落地** | P0 是 Local-First 原则的物理实现,FR-P0-007/008/016/017 全部为此服务 |
| V.2 (离线核心功能) | **直接落地** | AC-P0-5 验证 |
| III.1–III.5 (多 View 单 Task) | **数据模型奠基** | FR-P0-005、FR-P0-001 中 `TaskViewPlacement` 实体显式保证 invariant |
| I.3 (永不偷偷改数据) | **基础设施层落地** | 所有写操作必须由用户或受用户授权的进程触发;CLI 工具不得有"自动整理"类命令 |
| 质量底线("数据导出完整性 100%") | **直接落地** | AC-P0-4 验证 |

**与宪章无张力。** P0 是宪章 V 节(Local-First)的物理形态。

---

## 9. 开放问题

- **OQ-P0-1** SQLite 与 JSON 双写的具体策略:同步写 vs 异步刷盘?(性能 vs 数据安全的取舍,留给 plan 阶段)
- **OQ-P0-2** JSON 目录布局:per-task 一个文件 vs 每个 view 一个文件 vs 混合?需要在 plan 阶段权衡 Git diff 友好度与文件数量爆炸。
- **OQ-P0-3** archived task 的 30 天保留期由谁清理?(后台任务 vs 启动时清理 vs 用户手动)
- **OQ-P0-4** 是否在 P0 阶段就引入 event sourcing / change log?(P7 的云同步可能需要,但 P0 引入会增加复杂度)
