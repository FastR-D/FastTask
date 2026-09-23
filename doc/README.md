# FastTask 文档索引

> 索引更新：2026-09-23（第五轮：Phase 0 spike 完成，拓扑定案，**文档可交付**）  
> 用途：供人和 agent 快速定位文档，并判断哪些描述**已经是代码事实**、哪些是**尚未实现的目标状态**

## 0.0 术语消歧（最高优先级，先读这条）

本仓库中「fx」有**两个互不相关**的含义，混淆会导致严重的实现错误：

| 写法 | 指代 | 出现位置 |
|---|---|---|
| **uber-fx** | `go.uber.org/fx`，依赖注入与生命周期容器 | `go.mod:13`、`internal/bootstrap/`、[ADR-0001](adr/0001-fx-composition-root.md)、[`wiring.md`](wiring.md) |
| **libfx** | `vercel-labs/fx` 的 JS 嵌入 SDK，npm 包 `libfx` | [ADR-0005](adr/0005-libfx-agent-harness.md)、[`harness.md`](harness.md)、`web/src/harness/`、`sidecar/` |

**libfx 不进 Go 依赖图，uber-fx 不进前端。** 新写的文档一律使用 `uber-fx` / `libfx` 全称，不使用裸「fx」。
ADR-0001 与 `wiring.md` 的标题沿用历史写法，其中的「fx」指 uber-fx。

## 0. 给 agent 的阅读须知

**本目录中的文档分三类，混淆它们会导致实现错误：**

| 标记 | 含义 | 动作 |
|---|---|---|
| 🟢 **已实现** | 描述的是当前代码中真实存在的行为 | 可直接作为现状依据 |
| 🟡 **待实现** | 描述的是目标状态，与当前代码存在**有意的差距** | 实现前先读对应的 ADR，不要假设代码里已有 |
| ⚪ **构想来源** | 外部构想或原始需求，未必全部采纳 | 只作背景参考，不作实现依据 |

三条通用规则：

1. **代码是现状的事实来源，文档是意图的事实来源。** 两者冲突时，先判断该文档是 🟢 还是 🟡；🟢 冲突说明文档过时，🟡 冲突说明功能还没做。
2. **字段级契约以 Huma 生成的 OpenAPI 为准**（`/api/v1/openapi.json`），唯一例外是 `/api/v1/agent/*` 这一组手写描述的端点，见 `interface.md` §1.1。
3. **动手前先看 `adr/`。** 所有跨文档的架构决策都记在那里，附带被否决的方案和理由——避免重新讨论已经定过的事。

## 1. 从任务反查文档

| 你要做的事 | 先读 | 再读 |
|---|---|---|
| 实现 Agent 运行时 | [`agent.md`](agent.md) → [`agent-impl.md`](agent-impl.md) | [ADR-0002](adr/0002-assistant-transport.md)、[ADR-0005](adr/0005-libfx-agent-harness.md) |
| 迁移到 libfx harness / 做 sidecar | [ADR-0005](adr/0005-libfx-agent-harness.md) → [`harness.md`](harness.md) | [`agent.md`](agent.md) §4（不变量未变）、[`wiring.md`](wiring.md) §6 |
| 写模型网关 / AI Gateway 适配层 | [`harness.md`](harness.md) §4 | [ADR-0005](adr/0005-libfx-agent-harness.md) §5.1。**不要手搓翻译，复用 `@ai-sdk/openai-compatible`** |
| 做多会话 / 思考过程 / 图片附件 | [`chat-features.md`](chat-features.md) | [`frontend.md`](frontend.md) §4、[`agent-impl.md`](agent-impl.md) §2.7 |
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
| [`adr/0003-in-process-agent-loop.md`](adr/0003-in-process-agent-loop.md) | **已取代**（→0005） | 2026-09-22 | Agent 循环内建，工具即受控用例。**工具三级分类与不变量论证仍有效** |
| [`adr/0004-frontend-styling-boundary.md`](adr/0004-frontend-styling-boundary.md) | 已接受 | 2026-09-22 | mdui 令牌，不引入 Tailwind |
| [`adr/0005-libfx-agent-harness.md`](adr/0005-libfx-agent-harness.md) | 已接受 | 2026-09-22 | libfx 作为 harness，WASM 默认 / Node sidecar 兼容 |

### 2.2 核心设计（长期有效）

| 文档 | 类型 | 首次 | 更新 | 说明 |
|---|---|---|---|---|
| [`arch.md`](arch.md) | 🟢🟡 混合 | 2026-08-09 | 2026-09-23 | 架构总纲。§7 领域模块、§8 数据模型、§11 事务与并发、§12 安全边界、§21 部署拓扑（含 sidecar 两种托管方式）已实现；§6.1 的目录树是**目标结构，代码未按此排列** |
| [`tech.md`](tech.md) | 🟢🟡 混合 | 2026-08-09 | 2026-09-23 | 技术选型与工程约定。§2.1 选型表的依赖均已落地（fx、assistant-ui、mdui、vite-plugin-pwa、libfx、`@ai-sdk/*`）；§18.6/§20.1 的 CI 已在 `.github/workflows/ci.yml` 实现；§4 目录同样是目标结构 |
| [`func.md`](func.md) | 🟢 已实现 | 2026-08-09 | 2026-09-15 | 产品功能语义与业务规则 |
| [`interface.md`](interface.md) | 🟢 已实现 | 2026-08-09 | 2026-09-23 | HTTP 契约分组与协议约定，含 §20 的 harness / 线程 / 附件端点。字段级以 OpenAPI 为准 |

### 2.3 决策透镜（已实现）

| 文档 | 类型 | 更新 | 说明 |
|---|---|---|---|
| [`lens.md`](lens.md) | 🟢 已实现 | 2026-09-15 | 任务坐标、周复盘、目标地图的产品判断 |
| [`lens-impl.md`](lens-impl.md) | 🟢 已实现 | 2026-09-15 | 上者的可执行实现规格 |

### 2.4 Agent / 组合根 / 前端 / PWA（已实现）

**前五份加上 ADR-0001/0002/0004 描述的能力已全部实现并通过测试（截至 2026-09-22）。** 实现细节以代码与 OpenAPI 为准；这些文档保留为设计意图与验收依据。

**`harness.md` 与 `chat-features.md` 描述 [ADR-0005](adr/0005-libfx-agent-harness.md) 的落地，已于 2026-09-23 全部实现并通过测试。** 它们与 `agent.md` / `agent-impl.md` 的关系是：那两份定不变量与协议（**未变**），这两份定循环驱动与宿主拓扑（**变了**）。

| 文档 | 类型 | 更新 | 行数 | 说明 |
|---|---|---|---|---|
| [`agent.md`](agent.md) | 🟢 已实现 | 2026-09-22 | 169 | Agent 产品判断：不变量、工具三级分类、审批边界 |
| [`agent-impl.md`](agent-impl.md) | 🟢 已实现 | 2026-09-23 | 355 | 协议契约、数据模型、Run 状态机、循环规则、六阶段交付（phase A–F 全部落地）。§6「谁驱动循环」与 §7「审批后如何继续」已被 [ADR-0005](adr/0005-libfx-agent-harness.md) 取代，**取代方案亦已实现**，见 [`harness.md`](harness.md) §1.2、§7 |
| [`wiring.md`](wiring.md) | 🟢 已实现 | 2026-09-22 | 149 | fx 组合根、`App` 拆分、八步迁移 + §9 + 三个独立角色命令 |
| [`pwa.md`](pwa.md) | 🟢 已实现 | 2026-09-22 | 120 | PWA 规格与五个阻塞项（全部修复）|
| [`frontend.md`](frontend.md) | 🟢 已实现 | 2026-09-23 | 132 | §1、§6 mdui 实测现状 + §2–§5 assistant-ui 接入（workflow A/B 均落地）。§4 的多会话接线按 [`chat-features.md`](chat-features.md) §2.1.1 的取舍实现（未用 `useRemoteThreadListRuntime`）|
| [`harness.md`](harness.md) | 🟢 已实现 | 2026-09-23 | 964 | libfx 双宿主 harness：模式探测、AI Gateway 适配层、工具导出、审批长轮询、sidecar、八阶段交付。§16.6 是真机端到端验证记录 |
| [`chat-features.md`](chat-features.md) | 🟢 已实现 | 2026-09-23 | 365 | 多会话持久化、思考过程、图片附件。§2.1.1 记录了多会话的实际接线取舍（未用 `useRemoteThreadListRuntime`）|

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

### 2.7 验证记录

| 文档 | 类型 | 更新 | 说明 |
|---|---|---|---|
| [`qa-2026-09-23.md`](qa-2026-09-23.md) | 生产 QA / 代码审查记录 | 2026-09-23 | 浏览器实测范围、修复项、自动化验证与未执行的高风险操作 |

## 3. 当前实现状态速查

截至 2026-09-22：

- **Schema 版本**：8（`migrations/000005_agent_runtime` 至 `000008_agent_attachments`，包括 Agent 运行时、多会话、harness 与图片附件）。
- **后端**：Go + Gin + Huma v2 + GORM/SQLite，由 fx 组合根装配（`internal/bootstrap`）；`serve` / `worker` / `scheduler` 三个角色可独立运行（`wiring.md` 八步迁移 + §9 已全部落地）。
- **Agent**：assistant-transport SSE 运行时已落地——多轮工具循环（`agentloop.go`）、只读工具注册表（`agenttool.go`）、任务树/计划提案与 HTTP 处理器内同步审批（`agentapproval.go`、`agenttools_proposal.go`、`agentdailyplan.go`）、断线续流与启动中断回收。六阶段（A–F）全部交付。
- **前端**：mdui 2.1.5 重写 + assistant-ui 0.15.21 对话区已落地（`web/src/agent/`：`AgentChat` 挂载点、纯 converter、`makeAssistantToolUI` 审批卡片、协商录音格式的语音输入）。
- **PWA**：可安装——manifest、injectManifest Service Worker（按用户隔离缓存键的离线只读快照）、access token 内存 + refresh token 持久化、`static()` 以正确 Content-Type 提供 `sw.js`/`manifest.webmanifest`。五个阻塞项全部修复。

### 3.1 已完成：libfx harness 迁移（🟢 2026-09-23）

[ADR-0005](adr/0005-libfx-agent-harness.md) 描述的迁移**已全部落地**：八个阶段（A–H）都进了 `main`，
`go test ./...`、`go test -race ./...`、`npm test`（含加载真实 `libfx/node` 的集成测试）、
`npm run build` 与 `npm run build:sidecar` 全绿，CI 见 `.github/workflows/ci.yml`。
真机端到端验证（真实模型 + 真实 sidecar + 记录代理）见 [`harness.md`](harness.md) §16.6。

下面的内容是迁移**开工前**的决策记录，保留为设计依据；其中"实现前必读"三条已在实现中逐项处置。

#### ✅ 没有阻塞项了

Phase 0 spike 已于 2026-09-23 完成（[`harness.md`](harness.md) §16）。**拓扑定案：**

- **gateway shim 跑在宿主进程内**（浏览器或 sidecar），用现成的 `@ai-sdk/openai-compatible`，约 80 行
- **Go 侧是一个普通的 OpenAI 兼容代理**，不实现 LanguageModelV4
- **sidecar 降为可选**，仅用于支持缺 JSPI 的浏览器；部署形态仍是单 Go 二进制

实测数据：生产产物打包通过，主 bundle +166.66 kB（**gzip +47.33 kB**），
浏览器内流式正常，`reasoning_content` 与 `tool_calls` 映射齐全，控制台无错误。

spike 带出三条写进规格的实现约束：`baseURL` 必须绝对、分片交错不嵌套、`tool-call` 携带 `toolCallId`。

#### 已经定死、可直接实现的决策

| 决策 | 位置 |
|---|---|
| WASM 模式**不创建 `AgentJob`**；sidecar 模式创建但 Worker 转调 sidecar | [`harness.md`](harness.md) §1.2 |
| 一个用户消息 = 一个 run = 一次 `prompt()` = 一个 turn | [`harness.md`](harness.md) §5.1 |
| 取消经 `POST /agent/runs/{id}/cancellation`，三条传播通道 | [`harness.md`](harness.md) §5.2 |
| 工具调用：代理写"调用"、工具面写"结果"，按 `tool_call_id` 去重；请求体历史永不写库 | [`harness.md`](harness.md) §4.5.1 |
| 心跳 10 秒 / 失联 45 秒；审批等待期间必须继续心跳 | [`harness.md`](harness.md) §10.4 |
| 令牌叫 **`harness_token`**，与 `interface.md` §13 既有的 `run_token` 是两回事 | [`harness.md`](harness.md) §10.1 |
| **gateway shim 在宿主侧，Go 只做 OpenAI 兼容代理**；sidecar 可选 | [`harness.md`](harness.md) §4、§8、§16 |
| `tools_etag` 防止工具清单在运行中途变更，陈旧则 `409 TOOLS_ETAG_STALE` | [`harness.md`](harness.md) §10.3 |
| Go 覆盖 `system`/`tools`，sidecar 的 `sanitize()` 只拒绝不修正 | [`harness.md`](harness.md) §4.4 |
| 同一时刻只为活动线程持有一个 libfx agent 实例 | [`harness.md`](harness.md) §3.7 |
| shim 的 `baseURL` 必须是绝对 URL；chunk 翻译器不得假设分片严格嵌套 | [`harness.md`](harness.md) §16.3 |
| `thread_id` 回填按现有 Conversation 分组，迁移可重入 | [`chat-features.md`](chat-features.md) §2.3 |

#### 实现前必读的三条

- **不要以为 `agentloop.go` 要整个删掉。** 被替换的只有循环驱动约 150 行；
  `sessionStream`、`beginRun`、`recordProposal`、`resumeRun`、`rebuildModelContext` 全部保留并改变职责。
  逐项处置见 [`harness.md`](harness.md) §12——**注意其中对 `ChatProvider` 的两次更正**：
  它要**拆**（SSE 解析保留复用，请求构造删除），不是整个留或整个删。
- **`agent-impl.md` §8 已被取代**（[`harness.md`](harness.md) §1.2）。照它实现会让 Worker 把同一个 run 跑两遍。
- **`agent.md` 的五条不变量一条未改。** 任何为迁就 harness 放宽不变量的实现都是错的。

#### 还剩一个要用代码验证的点

| 何时 | 验证什么 | 结果 |
|---|---|---|
| ~~Phase 0~~ | ~~`@ai-sdk/openai-compatible` 的浏览器可行性~~ | ✅ 已验证通过 |
| ~~Phase C 开头~~ | `HostTool.execute` 第二参数是否带调用 id | ✅ **已验证：不带**（`fx-sdk.js` 的 `executeHostTool` 只传 `input` 与 `{ signal }`），因此走 §4.4.1 的退化方案：`tool_call_id` 可选，缺失时按 `(run, 工具名, 最近一个尚无结果的 part)` 关联 |

### 3.2 已知的代码/文档不一致（🟢 全部已修，2026-09-23）

| 位置 | 原问题 | 处置结果 |
|---|---|---|
| `internal/application/agenttool.go` | 注释称工具来自 uber-fx 值组 `agent_tools`，**该值组从未存在** | ✅ 注释已改写为实际装配方式（`NewAgentService` 内联，单一 registry，无第二份清单），见 [`harness.md`](harness.md) §3.4 |
| `internal/application/jobdispatch.go` | `BuiltinJobHandlers()` / `BuiltinJobMaterializers()` 与 `internal/bootstrap` 的值组是两份独立清单，无一致性测试 | ✅ `internal/bootstrap/valuegroups_test.go` 的 `TestJobValueGroupsCoverTheBuiltinJobTypes` 断言两侧集合相等 |
| `internal/application/agentapproval.go` | 审批续跑会再建一个 `agent_run` job | ✅ harness 模式下同一 turn 内续跑（`continueHarnessRun`），不建 job、不开第二条流；仅"无模型配置"的旧路径仍 requeue，见 [`harness.md`](harness.md) §7 |
| `web/vite.config.ts` | `globPatterns` 不含 `wasm` | ✅ 已含 `wasm`；CI 另有断言 `dist/fx-core.wasm{,.br,.gz}` 存在且产物里没有 `fx-term.wasm` / `*.node` |
| `web/src/agent/runtime.ts` | agent 端点非线程作用域，接多会话前必须改 | ✅ 取"body 携带 `threadId`"一侧（路径不带），理由与实现见 [`chat-features.md`](chat-features.md) §2.1.1 |
| CI | 未跑 `npm run build`，**测试全绿也可能构建失败** | ✅ `.github/workflows/ci.yml`：gofmt/vet/build/test/race + 空库迁移 + HTTP 写入读回烟雾 + typecheck/test/**build**/build:sidecar + sidecar 启动烟雾 |

## 4. 文档维护约定

- 改动 🟡 文档描述的能力时，实现完成后把标记改为 🟢，并更新本索引。
- 新增跨文档的架构决策时，先加 ADR，再改设计文档，**同一次提交内完成**，不留互相矛盾的两处描述。
- 引用代码位置用 `文件名:行号`。行号会漂移，**定位不到时以符号名为准**，并顺手修正索引。
