# FastTask 文档索引

> 索引更新：2026-09-22  
> 用途：供人和 agent 快速定位文档，并判断哪些描述**已经是代码事实**、哪些是**尚未实现的目标状态**

## 0. 给 agent 的阅读须知

**本目录中的文档分三类，混淆它们会导致实现错误：**

| 标记 | 含义 | 动作 |
|---|---|---|
| 🟢 **已实现** | 描述的是当前代码中真实存在的行为 | 可直接作为现状依据 |
| 🟡 **待实现** | 描述的是目标状态，与当前代码存在**有意的差距** | 实现前先读对应的 ADR，不要假设代码里已有 |
| ⚪ **构想来源** | 外部构想或原始需求，未必全部采纳 | 只作背景参考，不作实现依据 |

三条通用规则：

1. **代码是现状的事实来源，文档是意图的事实来源。** 两者冲突时，先判断该文档是 🟢 还是 🟡；🟢 冲突说明文档过时，🟡 冲突说明功能还没做。
2. **字段级契约以 Huma 生成的 OpenAPI 为准**（`/api/v1/openapi.json`），唯一例外是 `/api/v1/agent/*` 的三个流式端点，见 `interface.md` §1.1。
3. **动手前先看 `adr/`。** 所有跨文档的架构决策都记在那里，附带被否决的方案和理由——避免重新讨论已经定过的事。

## 1. 从任务反查文档

| 你要做的事 | 先读 | 再读 |
|---|---|---|
| 实现 Agent 运行时 | [`agent.md`](agent.md) → [`agent-impl.md`](agent-impl.md) | [ADR-0002](adr/0002-assistant-transport.md)、[ADR-0003](adr/0003-in-process-agent-loop.md) |
| 引入 fx / 拆 `application.App` | [`wiring.md`](wiring.md) | [ADR-0001](adr/0001-fx-composition-root.md)、`arch.md` §6 |
| 接 assistant-ui 前端 | [`frontend.md`](frontend.md) | [ADR-0004](adr/0004-frontend-styling-boundary.md)、`agent-impl.md` §2 |
| 做 PWA | [`pwa.md`](pwa.md) | `frontend.md` |
| 改 HTTP 接口 | [`interface.md`](interface.md) | 实现中的 Huma Operation 定义 |
| 改任务坐标 / 周复盘 / 目标地图 | [`lens.md`](lens.md) → [`lens-impl.md`](lens-impl.md) | — |
| 理解产品语义与业务规则 | [`func.md`](func.md) | `req/this.md` |
| 理解架构边界与分层 | [`arch.md`](arch.md) | `tech.md` |
| 对接 FastResearch 其他工具 | [`integration/README.md`](integration/README.md) | `integration/contract.md` |

## 2. 完整索引

### 2.1 架构决策记录

决策的正式记录，含被否决方案。**改变任何一条前先读它。**

| 文档 | 状态 | 更新 | 说明 |
|---|---|---|---|
| [`adr/README.md`](adr/README.md) | 索引 | 2026-09-22 | ADR 约定与依赖关系 |
| [`adr/0001-fx-composition-root.md`](adr/0001-fx-composition-root.md) | 已接受 | 2026-09-22 | fx 组合根 + 按聚合拆 `App` |
| [`adr/0002-assistant-transport.md`](adr/0002-assistant-transport.md) | 已接受 | 2026-09-22 | Agent 前后端传输协议 |
| [`adr/0003-in-process-agent-loop.md`](adr/0003-in-process-agent-loop.md) | 已接受 | 2026-09-22 | Agent 循环内建，工具即受控用例 |
| [`adr/0004-frontend-styling-boundary.md`](adr/0004-frontend-styling-boundary.md) | 已接受 | 2026-09-22 | mdui 令牌，不引入 Tailwind |

### 2.2 核心设计（长期有效）

| 文档 | 类型 | 首次 | 更新 | 说明 |
|---|---|---|---|---|
| [`arch.md`](arch.md) | 🟢🟡 混合 | 2026-08-09 | 2026-09-22 | 架构总纲。§7 领域模块、§8 数据模型、§11 事务与并发、§12 安全边界已实现；§6.1 的目录树是**目标结构，代码未按此排列** |
| [`tech.md`](tech.md) | 🟢🟡 混合 | 2026-08-09 | 2026-09-22 | 技术选型与工程约定。§2.1 选型表含尚未引入的依赖（fx、assistant-ui、mdui 已落地、vite-plugin-pwa 未落地）；§4 目录同样是目标结构 |
| [`func.md`](func.md) | 🟢 已实现 | 2026-08-09 | 2026-09-15 | 产品功能语义与业务规则 |
| [`interface.md`](interface.md) | 🟢 已实现 | 2026-08-09 | 2026-09-22 | HTTP 契约分组与协议约定。字段级以 OpenAPI 为准 |

### 2.3 决策透镜（已实现）

| 文档 | 类型 | 更新 | 说明 |
|---|---|---|---|
| [`lens.md`](lens.md) | 🟢 已实现 | 2026-09-15 | 任务坐标、周复盘、目标地图的产品判断 |
| [`lens-impl.md`](lens-impl.md) | 🟢 已实现 | 2026-09-15 | 上者的可执行实现规格 |

### 2.4 规划中的能力（待实现）

**这四份加上 ADR 是当前主要的实现待办。**

| 文档 | 类型 | 更新 | 行数 | 说明 |
|---|---|---|---|---|
| [`agent.md`](agent.md) | 🟡 待实现 | 2026-09-22 | 169 | Agent 产品判断：不变量、工具三级分类、审批边界 |
| [`agent-impl.md`](agent-impl.md) | 🟡 待实现 | 2026-09-22 | 355 | 协议契约、数据模型、Run 状态机、循环规则、六阶段交付 |
| [`wiring.md`](wiring.md) | 🟡 待实现 | 2026-09-22 | 149 | fx 组合根、`App` 拆分、八步迁移 |
| [`pwa.md`](pwa.md) | 🟡 待实现 | 2026-09-22 | 120 | PWA 规格与五个阻塞项 |
| [`frontend.md`](frontend.md) | 🟢🟡 混合 | 2026-09-22 | 132 | §1、§6 是 mdui 重写后的**实测现状**；§2–§5 是待实现的接入规格 |

### 2.5 外部工具对接（已实现的部分）

| 文档 | 类型 | 更新 | 说明 |
|---|---|---|---|
| [`integration/README.md`](integration/README.md) | 🟢 已实现 | 2026-08-10 | 各工具真实能力与协作边界总览 |
| [`integration/contract.md`](integration/contract.md) | 🟢 已实现 | 2026-08-10 | 通用导入收件箱契约 |
| [`integration/fastinsight.md`](integration/fastinsight.md) | ⚪ 待接入 | 2026-08-10 | CLI Runner 尚未实现 |
| [`integration/fastnews.md`](integration/fastnews.md) | ⚪ 待接入 | 2026-08-10 | CLI Runner 尚未实现 |
| [`integration/fastread.md`](integration/fastread.md) | 🟢 可健康探测 | 2026-08-10 | 业务适配器未实现 |
| [`integration/fastwrite.md`](integration/fastwrite.md) | 🟢 可健康探测 | 2026-08-10 | 业务适配器未实现 |

### 2.6 需求与构想来源

| 文档 | 类型 | 更新 | 说明 |
|---|---|---|---|
| [`req/this.md`](req/this.md) | ⚪ 原始需求 | 2026-08-09 | **产品的原始出发点，遇到产品判断分歧时回到这里** |
| [`req/overall.md`](req/overall.md) | ⚪ 原始需求 | 2026-08-09 | FastResearch 整体设想 |
| [`plotminder/`](plotminder/) | ⚪ 构想来源 | 2026-09-15 | 决策地图构想。**大部分未被采纳**，采纳与否决的逐条裁决见 `lens.md` §3。不要据此实现任何东西 |

## 3. 当前实现状态速查

截至 2026-09-22：

- **Schema 版本**：4（`migrations/000004_task_coords`）。`agent-impl.md` §3 要求升到 5。
- **后端**：Go + Gin + Huma v2 + GORM/SQLite，装配手写在 `cmd/fasttask/main.go`（fx 尚未引入）。
- **Agent**：只有单轮无工具的 LLM 调用，**不存在 agent 循环**。现状盘点见 `agent.md` §2。
- **前端**：mdui 2.1.5 完全重写已落地，assistant-ui 尚未接入。
- **PWA**：无 manifest、无 Service Worker。`index.html` 已有安全区域与 `apple-mobile-web-app-*` 基础。

## 4. 文档维护约定

- 改动 🟡 文档描述的能力时，实现完成后把标记改为 🟢，并更新本索引。
- 新增跨文档的架构决策时，先加 ADR，再改设计文档，**同一次提交内完成**，不留互相矛盾的两处描述。
- 引用代码位置用 `文件名:行号`。行号会漂移，**定位不到时以符号名为准**，并顺手修正索引。
