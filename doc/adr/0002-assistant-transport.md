# ADR-0002：Agent 前后端采用 assistant-transport 协议

> 状态：已接受  
> 日期：2026-09-22  
> 实现文档：[`doc/agent-impl.md`](../agent-impl.md)  
> 相关：`doc/interface.md` §1；ADR-0003

## 1. 背景

前端引入 `@assistant-ui/react`（当前 0.15.21）后，需要在它和 Go 后端之间选一个协议。assistant-ui 提供三种运行时接法：

| 运行时 | 状态权威在 | 流式 | 原生工具审批 |
|---|---|---|---|
| `useExternalStoreRuntime` | 调用方自己的 store | 否 | 否 |
| `useChatRuntime`（AI SDK data-stream 协议） | 前端 | 是 | 是 |
| `useAssistantTransportRuntime`（assistant-transport 协议） | **服务端** | 是 | 是 |

FastTask 有三条已经写死的架构约束：SQLite 是事实来源（`arch.md` §11）、Agent 作业必须在进程重启后可恢复（§10.1）、Agent 不得直接修改业务数据（§9.1）。

## 2. 决策

采用 **assistant-transport** 协议，前端 `useAssistantTransportRuntime` 并显式设置 `protocol: "assistant-transport"`（该选项默认值是 `"data-stream"`，不显式设置会静默走错协议）。

协议契约、端点、chunk 编码和数据模型见 [`doc/agent-impl.md`](../agent-impl.md) §2、§3。

## 3. 理由

协议的状态模型与 FastTask 的架构约束一一对应：

| assistant-transport 机制 | 对应的 FastTask 约束 |
|---|---|
| 服务端持有权威 thread state，客户端只发命令 | `arch.md` §11：SQLite 内强一致，不变量由服务端强制 |
| `resumeStateApi` + `resumeApi` 断线续流 | §10.1：Agent 作业进程重启后可恢复；移动端 PWA 网络不稳 |
| 原生工具审批（`hitl` / `ToolApprovalOption`） | §9.1：Agent 输出必须经用户确认才能落库 |
| `update-state` 增量推送服务端自有状态 | 今日计划、提案 diff、Job 状态可与对话共用一条流 |

反过来，AI SDK data-stream 协议把状态权威放在前端，与「服务端强制不变量」的模型对抗；而且该规范由 TypeScript 生态单方面演进，Go 侧只能被动追随。

## 4. 代价

- **必须在 Go 里实现 chunk 编码器。** 协议本身简单（SSE + 每行一个 JSON chunk + `[DONE]`），工作量可控，chunk 类型表见 `agent-impl.md` §2.4。
- **该端点必须绕过 Huma。** Huma 的 `sse` 包强制写 `event: <name>` 行，而 assistant-transport 解码器在 strict 模式下要求事件名为默认的 `message`（即不带 `event:` 行）。这是对 `interface.md` §1.1「Huma 定义为字段级事实来源」的一处例外，必须在 `interface.md` 中显式记录，并为该端点手工维护 OpenAPI 描述。
- **`ConversationMessage.Content` 是扁平字符串**（`models.go:235`），承载不了 assistant-ui 的 parts 数组。需要新增数据模型，见 `agent-impl.md` §3。
- **assistant-transport 在 assistant-ui 中仍属较新的接法**，API 可能比 primitives 更容易变动。缓解：Go 侧只实现协议本身，不依赖 assistant-ui 的内部类型；前端把运行时接线收敛在单个文件内。

## 5. 被否决的方案

| 方案 | 否决理由 |
|---|---|
| AI SDK data-stream 协议 | 状态权威在前端，与服务端强制不变量的模型对抗；规范由不受控的外部生态演进 |
| `useExternalStoreRuntime` 直接适配现有 REST | 无流式、无原生工具审批，本质仍是现在的「一问一答」，达不到「真正的 agent」这个目标 |
| 先 external store 跑通再迁移 transport | 前端适配层要写两遍，而 assistant-transport 的后端工作量并不比 external store 的轮询适配大多少 |
