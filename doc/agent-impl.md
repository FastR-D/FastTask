# FastTask Agent 实现规格

> 文档状态：初版实现规格  
> 上位文档：[`doc/agent.md`](agent.md)（那份定判断，本文定落地）  
> 决策记录：[ADR-0002](adr/0002-assistant-transport.md)、[ADR-0003](adr/0003-in-process-agent-loop.md)  
> 协议基线：`@assistant-ui/react` 0.15.21 / `@assistant-ui/core` 0.3.20 / `assistant-stream` 0.3.44

## 1. 文档定位

本文是可以直接照着写代码的规格。协议部分的全部字段和语义均核对自上述版本的类型定义与运行时实现，不是根据文档推测的。**升级 assistant-ui 大版本时必须重新核对本文 §2。**

## 2. 传输协议契约

### 2.1 端点

三个传输端点全部是 `POST`，全部使用同一组请求头（前端 `headers` 选项返回的内容，含 `Authorization`）。

| 前端选项 | 路径 | 用途 |
|---|---|---|
| `api` | `POST /api/v1/agent/commands` | 提交命令并开启一次运行 |
| `resumeStateApi` | `POST /api/v1/agent/resume-state` | 取回保留的状态与 `runId` |
| `resumeApi` | `POST /api/v1/agent/resume` | 续流一次进行中的运行 |

前端必须显式设置 `protocol: "assistant-transport"`。**该选项默认值是 `"data-stream"`，漏设会静默走错协议且难以排查。**

传输协议之外还有一个恢复读，它不是 assistant-ui 的选项，由前端在挂载时自己调用：

| 路径 | 用途 |
|---|---|
| `GET /api/v1/agent/thread-state` | 取回当前用户最近一次会话的权威状态 |

响应二选一：`204`（该用户还没有 agent 会话，前端从空对话开始）或 `200` + `{"state": <§2.7 结构>}`。作用域永远是认证用户，客户端不传 `threadId`，因此没有跨用户读取的入口。assistant-ui 的会话状态只活在运行时内存里，视图被卸载（切页签、刷新、PWA 冷启动）就丢了；这个端点是重新挂载时唯一的恢复来源，返回的 `fasttask.threadId` 让下一条命令继续同一条会话，而不是又新开一条。运行尚未 checkpoint 时，状态由已持久化的线程消息重建（§2.6 渲染的同一份数据）。

### 2.2 `api` 请求体

```jsonc
{
  "commands": [ /* 见 §2.3 */ ],
  "state": { /* 客户端当前持有的状态，不可信，见下 */ },
  "system": "string | undefined",
  "tools": { /* 前端工具的 JSON Schema，本期为空 */ },
  "threadId": "string | null",
  "parentId": "string | null",
  "callSettings": { }, "config": { }
}
```

**安全规则：请求体中的 `state`、`threadId`、`system`、`tools` 全部是不可信输入。** `state` 由客户端持有，服务端**只能**把它当作乐观并发的提示，绝不能据此重建权威状态——权威状态一律从 SQLite 读取。`threadId` 必须校验归属于当前认证用户，否则返回 `404`（遵循 `arch.md` §12 的跨用户 `404` 惯例）。`user_id` 永远取自认证上下文。

**`system` 与 `tools` 一律忽略。** 这两个字段由 assistant-ui 从前端的 model context 自动填充，客户端可以任意伪造。系统提示词和工具注册表只以服务端的为准——否则前端可以注入提示词或声明服务端并未授权的工具。服务端读到这两个字段后直接丢弃，不记录、不合并。

**`threadId` 为 `null` 时**（用户的第一条消息，前端尚无 thread），服务端在同一事务内创建 `conversations` 记录，并在本次流的第一个 `update-state` 里用 `set` 操作把 `["fasttask","threadId"]` 回推给客户端。前端 `converter` 负责把它接回 thread 列表。

### 2.3 命令类型

```jsonc
{ "type": "add-message",
  "message": { "role": "user", "parts": [ {"type":"text","text":"..."} ] },
  "parentId": "string | null", "sourceId": "string | null" }

{ "type": "add-tool-result",
  "toolCallId": "...", "toolName": "...",
  "result": { }, "isError": false,
  "artifact": { }, "modelContent": [ ] }
```

`add-tool-result` 是审批回执的载体，见 §7。

### 2.4 SSE 响应格式

响应头 `Content-Type: text/event-stream`、`Cache-Control: no-cache`、`Connection: keep-alive`。

每个 chunk 一行：

```text
data: {"type":"update-state","operations":[...]}\n\n
```

流结束必须发送：

```text
data: [DONE]\n\n
```

**不得写 `event:` 行。** 解码器在 strict 模式下要求事件名为默认的 `message`，任何显式 `event:` 都会让整条流失败。这正是本端点不能用 Huma `sse` 包的原因（该包强制写 `event:`），见 §9.1。

### 2.5 Chunk 类型与 `path` 规则

`path` 是非负整数数组。规则按类型分两档：

| 类型 | `path` | 必需字段 |
|---|---|---|
| `update-state` | 可省略（默认 `[]`） | `operations`（数组） |
| `part-start` | 可省略 | `part`（对象） |
| `annotations` | 可省略 | `annotations`（数组） |
| `data` | 可省略 | `data`（数组） |
| `step-start` | 可省略 | — |
| `step-finish` | 可省略 | `finishReason`（字符串） |
| `message-finish` | 可省略 | `finishReason`（字符串） |
| `error` | 可省略 | — |
| `text-delta` | **必需** | `textDelta`（字符串） |
| `part-finish` | **必需** | — |
| `tool-call-args-text-finish` | **必需** | — |
| `result` | **必需** | `isError`（布尔，可选） |

未知类型或字段不合法的 chunk 会被丢弃（strict 模式下直接让流失败）。**Go 编码器只允许发出上表中的类型。**

### 2.6 `update-state`：权威渲染路径

这是本协议最关键的一点，做错会导致「消息不显示但流是通的」这类难排查的问题。

**客户端渲染的消息列表来自服务端状态，不是来自 part chunk。** 流程是：`update-state` 操作 → 客户端累加出状态 → 前端 `converter(state)` 映射为 `{messages, state, isRunning}` → 渲染。

操作只有两种：

```jsonc
{ "type": "set",         "path": ["messages", "3"], "value": { } }
{ "type": "append-text", "path": ["messages", "3", "parts", "0", "text"], "value": "增量文本" }
```

约束：

- `path` 是**字符串**数组（与 chunk 的 `path` 不同，那个是整数数组）。
- `append-text` 要求目标位置已存在且是字符串，否则客户端抛错。因此**必须先 `set` 建出 `{"type":"text","text":""}`，再 `append-text`**。
- 数组下标用整数形式的字符串（`"3"`）。
- `__proto__`、`constructor`、`prototype` 等路径段会被拒绝。

**流式输出的实现方式就是 `append-text`**，不是 `text-delta`。

### 2.7 服务端状态结构

> 🟢 **[`chat-features.md`](chat-features.md) 给 part 增加了 `reasoning`（§3.3）与 `image`（§4.4）两种类型，并给消息加上了 `thread_id` 归属（§2.3）；migration `000006`–`000008` 已落地。** 下方结构是这两项之前的版本，仍适用于其余字段。


```jsonc
{
  "messages": [
    { "id": "...", "role": "user|assistant",
      "parts": [
        {"type":"text","text":"..."},
        {"type":"tool-call","toolCallId":"...","toolName":"...","args":{},
         "result":{},"isError":false,
         "approval":{"status":"pending|approved|rejected","options":[...]}}
      ],
      "createdAt": "RFC3339", "status": {"type":"running|complete|incomplete"} }
  ],
  "isRunning": true,
  "fasttask": {
    "activeGoalId": "...",
    "pendingProposals": [ {"id":"...","goalId":"...","baseRevision":7,"summary":"..."} ]
  }
}
```

三条说明：

- **这是 FastTask 自己定义的结构，不是 assistant-ui 的类型。** 前端 `converter` 负责把它映射成 assistant-ui 的 `ThreadMessage`。服务端不需要知道 assistant-ui 的内部类型，也不应照抄它的字段名。
- **`messages[].status` 的取值**按 assistant-ui 的 `MessageStatus` 设计，便于 converter 直接透传：`{"type":"running"}`、`{"type":"complete","reason":"stop"}`、`{"type":"incomplete","reason":"cancelled"|"error"|...}`、`{"type":"requires-action","reason":"tool-calls"}`。等待审批时用最后一种。
- **`approval` 是 FastTask 自有字段，converter 绝不能把它映射到 `ToolCallMessagePart.approval`。** 原因见 [ADR-0002](adr/0002-assistant-transport.md) §3.1：assistant-transport 不接 `onRespondToToolApproval`，映射过去会渲染出点了没反应的审批控件。审批 UI 用 `makeAssistantToolUI` 自己画，决定用 `addToolResult` 回传。

`fasttask` 命名空间承载对话之外的业务状态，前端 `converter` 把它取出来单独渲染（提案 diff 卡片、今日计划预览）。**这是让对话与业务视图共用一条流的机制**，不要再为它另开轮询。

### 2.7.1 消息索引由服务端拥有

`messages` 是数组，`update-state` 用下标寻址。服务端是唯一的下标分配者：

```text
新建助手消息： set  ["messages","<n>"]          = {id, role:"assistant", parts:[], status:{type:"running"}, createdAt}
建立文本片段： set  ["messages","<n>","parts","0"] = {"type":"text","text":""}
流式追加文本： append-text ["messages","<n>","parts","0","text"] = "增量"
收尾：        set  ["messages","<n>","status"]   = {"type":"complete","reason":"stop"}
```

**`append-text` 之前必须先 `set` 出空字符串**，否则客户端抛错（见 §2.6）。客户端上报的 `state` 不参与下标计算。

### 2.8 续流语义

`resumeStateApi` 请求体是 `{"threadId":"..."}`，响应二选一：

- `204 No Content` —— 没有进行中的运行，客户端跳过续流。
- `200` + `{"state": <任意>, "runId": "<字符串>"}` —— 两个键都必需，`runId` 必须是字符串，否则客户端抛错。

**`threadId` 可能是 assistant-ui 自己造的临时 id。** 前端用的是内存 thread list，重新挂载的对话在第一条命令之前拿到的 `remoteId` 形如 `__LOCALID_...`，而库请求 `resumeStateApi` 时不提供改写请求体的钩子。服务端把这种 id 理解为「客户端说不出自己是哪条会话」：按认证用户解析其在途运行（§9.3 的每用户最多一个活跃运行），而不是拿它当会话 id 去查。真实会话 id 仍走归属校验，跨用户返回 `404`。

拿到之后客户端 `POST resumeApi`，请求体里 `commands` 为空数组、带 `runId`、**不带 `state`**。服务端据此重放并继续该运行。

客户端在续流时会强制 `strict: false`，服务端仍应发出合法 chunk，不要依赖这个宽容。

## 3. 数据模型（migration `000005_agent_runtime`）

`ExpectedSchemaVersion` 从 `4` 升到 `5`（`store.go:28`）。

### 3.1 复用与新增

**`conversations` 表直接复用为 Thread**，`conversations.id` 即协议中的 `threadId`。它已经有 `user_id`、`title`、`status`、可选 `goal_id`、`revision`（`models.go:224`），不需要改结构。

**`conversation_messages` 不再用于 agent 对话。** 它的 `Content` 是扁平字符串（`models.go:235`），承载不了 parts。保留该表与现有接口以兼容既有数据，新对话写入新表。

新增四张表：

| 表 | 关键字段 | 说明 |
|---|---|---|
| `agent_runs` | `id`, `user_id`, `thread_id`, `job_id`, `status`, `state_json`, `checkpoint_seq`, `parent_message_id`, `error_code`, `error_message`, `revision`, `created_at`, `updated_at`, `finished_at` | 一次运行；`state_json` 是续流用的保留快照 |
| `agent_messages` | `id`, `user_id`, `thread_id`, `run_id`, `parent_id`, `role`, `seq`, `created_at` | 消息，`seq` 保证顺序 |
| `agent_message_parts` | `id`, `message_id`, `idx`, `type`, `text`, `tool_call_id`, `tool_name`, `args_json`, `result_json`, `is_error`, `artifact_json`, `approval_status`, `proposal_id`, `created_at`, `updated_at` | 消息片段 |
| `agent_run_chunks` | `run_id`, `seq`, `chunk_json`, `created_at` | 已发出 chunk 的持久日志，续流时重放 |

索引与约束：

- `UNIQUE(agent_run_chunks.run_id, seq)`，主键即此组合。
- `INDEX(agent_messages.thread_id, seq)`。
- `UNIQUE(agent_message_parts.tool_call_id)`（非空时），用于 `add-tool-result` 定位。
- `agent_runs` 对同一 `thread_id` 最多一条 `status IN ('queued','running')`，由条件写入保证。
- 全部表带 `user_id` 并在每次查询中过滤，遵循 `arch.md` §12。

### 3.2 迁移约束

- 只新增表，不修改 `conversations`、`tasks`、`daily_plans` 等既有结构。
- 升级后既有数据不得丢失，`/health/ready` 必须正常，参照 `store_test.go` 里 v3→v4 的升级测试新增一份 v4→v5 测试。

## 4. Run 状态机

```text
queued -> running -> succeeded
                  -> awaiting_approval -> running（收到 add-tool-result）
                  -> failed
                  -> interrupted（进程重启导致租约过期）
queued/running -> cancelled（用户显式取消）
```

`awaiting_approval` 是本设计新增的状态：模型发起了提案工具调用，运行让出控制权等待用户决定。此时 HTTP 流**正常结束**（发送 `[DONE]`），不挂着连接等人。

### 4.0 Run 状态与 AgentJob 状态的映射

两者是不同的东西，必须显式对应，否则 Worker 和 HTTP 层会各说各话：

| `agent_runs.status` | 对应 `agent_jobs.status` | 说明 |
|---|---|---|
| `queued` | `queued` | 已创建，等 Worker 领取 |
| `running` | `running` | Worker 持有租约，循环执行中 |
| `awaiting_approval` | `succeeded` | **本次作业正常结束**。等待用户决定，不占租约、不消耗重试次数 |
| `succeeded` / `failed` / `cancelled` | 同名 | 一一对应 |
| `interrupted` | `failed`（`error_code=INTERRUPTED`） | 租约过期且无人续跑 |

关键点：**`awaiting_approval` 时对应的 AgentJob 已经 `succeeded`。** 等待用户审批可能长达数小时，不能让一个作业一直占着租约。用户提交决定时创建**新的 AgentJob**，但复用**同一个 `agent_runs` 记录**（见 §7）。

### 4.1 v1 明确不支持的能力

写清楚避免实现者自行发挥：

- **不支持消息编辑与分支。** `capabilities.edit` 不开启，`onEdit` 不接。`agent_messages.parent_id` 字段保留供将来使用，v1 内一条 thread 的消息恒为线性。
- **不支持重新生成（reload）。** 不实现 `onReload`。
- **不支持附件。** `adapters.attachments` 不配置。
- **一个用户同时最多一个活跃运行**（§9.3），因此不需要处理同 thread 并发运行。

### 4.2 续流覆盖范围

明确区分两种断线，不要对外宣称超出实际能力的保证：

| 情况 | 行为 |
|---|---|
| **客户端断线，服务端仍在跑**（锁屏、切网、PWA 切后台） | 完整支持。运行继续，重放 `agent_run_chunks` 后接上实时流 |
| **服务端进程重启**（部署、崩溃） | v1 **不跨进程续跑**。租约过期后运行标记为 `interrupted`，已产出的部分消息保留，用户可重试 |

第一种是移动端的常见情况，也是选择本协议的主要动机。第二种留待后续按需处理。

### 4.3 客户端断开不等于取消

HTTP 请求的 `AbortSignal` 触发时**不得取消运行**。运行由 Worker 持有，继续执行并继续写 `agent_run_chunks`。只有显式调用现有的 `PUT /api/v1/agent-jobs/{job_id}/cancellation`（`app.go:789`）才取消。

## 5. 工具注册表

### 5.1 注册方式

> **更正（2026-09-22）：本节原先描述为「通过 uber-fx 值组 `group:"agent_tools"` 注册」。该值组从未存在**，
> `agenttool.go:82` 的同名注释是过时描述，需一并删除。实际构造在 `agent.go:89`：
> `builtIn := append(NewReadonlyTools(app), NewProposalTools(app)...)`，再交给 `NewToolRegistry`。

工具在 `internal/application` 内由 `NewReadonlyTools` / `NewProposalTools` 构造，经 `NewToolRegistry`
汇总；注册表拒绝重名，并拒绝任何参数 schema 泄漏身份字段的工具（`identityArgNames`）。

工具定义至少包含：名称、描述、JSON Schema 参数、级别（`readonly` / `proposal`）、执行函数。

**自 [ADR-0005](adr/0005-libfx-agent-harness.md) 起，注册表还须经 `GET /api/v1/agent/tools` 投影给 harness 宿主**，
且该端点是宿主侧工具描述符的**唯一**来源——不得在 TypeScript 里重复声明。见 [`doc/harness.md`](harness.md) §3.4。

### 5.1.1 必须新增 Provider 能力：现有接口不支持工具调用

当前 `agent.Provider`（`openai.go:20`）只有 `TaskProposal` 和 `ConversationReply` 两个方法，**没有任何工具调用能力**，无法承载 agent 循环。需要新增一个 port，与现有接口并存（现有接口仍被旧的 `conversation` / `task_tree_*` 作业使用，见 §8.1）：

```go
// internal/agent 中新增，不替换现有 Provider
type ChatProvider interface {
    Name() string
    // 一次模型调用。messages 含完整历史，tools 是本次允许调用的工具定义。
    // 实现负责把结果流式写入 sink（文本增量、工具调用），并返回本轮的结束原因。
    Chat(ctx context.Context, req ChatRequest, sink ChatSink) (ChatResult, error)
}
```

三条要求：

- 使用 OpenAI-compatible 的 `tools` / `tool_choice` 参数和 `tool_calls` 响应字段，与现有 `complete()`（`openai.go:181` 起）共用 HTTP 客户端与鉴权。
- **必须支持流式**（`stream: true`），否则 §2.6 的 `append-text` 退化成一次性输出，流式就白做了。
- `ChatSink` 是 Go 侧接口，不暴露给领域层；它的实现负责把增量转成 §2.5 的 chunk 并写入 `agent_run_chunks` 和 hub。

**模型不支持工具调用时的行为**：Provider 校验失败应返回明确错误，运行标记 `failed`、错误码 `PROVIDER_NO_TOOL_SUPPORT`，并在界面提示管理员更换模型。**不要静默降级成单轮对话**——那会让用户以为 agent 在工作而实际什么都没做。

### 5.2 强制规则

- **参数里不得出现 `user_id`、`workspace_id` 或任何身份字段。** 身份来自执行上下文。出现即视为实现错误，需有测试覆盖。
- 每个工具的参数必须经 JSON Schema 校验后再进入领域层，模型输出按 `arch.md` §12 属于不可信输入。
- `readonly` 工具在数据库事务外执行。
- `proposal` 工具只创建 `Proposal` 记录，**不得触碰业务表**。
- 工具执行失败返回结构化错误给模型（可自我修正），不直接让整个运行失败。

### 5.3 清单

见 [`doc/agent.md`](agent.md) §5。新增工具必须同时更新那份清单和本节的测试要求。

## 6. 循环执行规则

> 🟢 **本节的「谁驱动循环」部分已由 [ADR-0005](adr/0005-libfx-agent-harness.md) 取代，取代方案已实现**（[`harness.md`](harness.md) §1.2、§5）。下方保留的是取代前的描述。
> 循环驱动改由 libfx 承担，宿主在浏览器 WASM 或 Node sidecar 中运行，见 [`doc/harness.md`](harness.md)。
>
> **仍然有效、不因换 harness 而改变的部分：**
> - 「忽略请求体里的 system / tools，一律用服务端的值」——宿主在客户端，其声明属不可信输入，
>   该规则的重要性反而**上升**（`harness.md` §4.3）。
> - 下方的限制表（轮数、墙钟、工具超时）与错误分类，由 Go 代理层继续强制。
> - 唯一的数值变更：**审批等待不计入墙钟超时**（`harness.md` §7）。
>
> 下方流程图描述的是被取代的自建循环，保留作为语义参照。

```text
载入 thread 历史 + 服务端系统提示 + 服务端工具注册表（忽略请求体里的 system / tools）
循环：
  调用模型（流式，带工具定义）
  文本增量 -> 立即以 append-text 推送
  本轮结束后按工具调用分情况：
    无工具调用                -> 运行结束（succeeded）
    只有 readonly 工具调用    -> 全部执行，结果回灌上下文，继续下一轮
    含任一 proposal 工具调用  -> 创建 Proposal、推送待审批工具片段、转 awaiting_approval、发 [DONE] 结束本次流
每轮开始前检查 cancel_requested
```

**同一轮同时返回文本和工具调用是常见情况**，不是异常：文本照常流式推送，然后按上表处理工具调用。文本不会因为有工具调用而被丢弃。

**同一轮返回多个工具调用时**：全部是 readonly 则并发执行、全部回灌；只要含一个 proposal 工具，则 readonly 的照常执行并回灌，proposal 的转入审批，本轮到此为止。

保守默认值（接真实模型后按观测调整，属 `agent.md` §11 待确认项）：

| 限制 | 默认值 | 超出时 |
|---|---|---|
| 单次运行最大模型轮数 | 8 | 结束运行并告知用户 |
| 单次运行墙钟超时 | 180 秒 | 标记 `failed`，错误码 `RUN_TIMEOUT` |
| 单次工具执行超时 | 10 秒 | 该工具返回错误，循环继续 |
| 单轮最大输出 token | 沿用 Provider 配置 | — |

错误分类复用现有 `worker.go:214` 的约定：可重试错误进 `TEMPORARY` 并回到 `queued`，不可重试进 `PROVIDER_ERROR`，revision 冲突进 `REVISION_MISMATCH`。

## 7. 审批流

> 🟢 **[ADR-0005](adr/0005-libfx-agent-harness.md) 改变了「审批后如何继续」，但不改变审批的语义与协议；新时序已实现**（同一 turn 内长轮询等待，见 [`harness.md`](harness.md) §7）。
> 决定仍然只走 `add-tool-result`（§7.1 不变），提案仍然不写业务表，`ApplyProposal` 仍在事务内重校验。
> 变化是：审批不再「结束流 → `resumeRun` 起新流」，而是**在同一个 turn 内等待**——
> 提案工具的 `execute` 长轮询到决定后才返回。见 [`doc/harness.md`](harness.md) §7。

```text
模型调用 propose_task_tree_patch
  -> 服务端校验参数 + 创建 Proposal(status=pending)，不写业务表
  -> update-state 推送 tool-call part，approval.status = "pending"，附结构化 diff
  -> 运行转 awaiting_approval，发送 [DONE] 结束本次流
  -> 用户在对话里确认或拒绝
  -> 前端发 add-tool-result 命令（result 里带决定）
  -> 确认：调用现有 ApplyProposal（app.go:991），事务内重校验所有权 / base revision / 父子归属 / 循环
     拒绝：Proposal -> rejected，拒绝理由写入工具结果
  -> 工具结果回灌模型上下文，运行回到 running 继续
```

### 7.1 审批只能走 `add-tool-result`

**不要使用 assistant-ui 的 `respondToApproval` / `hitl` / `humanTool`。** `useAssistantTransportRuntime` 没有接 `onRespondToToolApproval`，用了传不到服务端。详见 [ADR-0002](adr/0002-assistant-transport.md) §3.1。

前端用 `makeAssistantToolUI` 按工具名注册审批卡片，用户点击后调用 `addToolResult`，结果体形如：

```jsonc
{ "decision": "approve" }
{ "decision": "reject", "reason": "第二步和第三步重复了" }
```

### 7.2 审批续跑的是同一个 Run，不是新 Run

这一点最容易做错。`add-tool-result` 命令到达 `POST /agent/commands` 时：

```text
若该 toolCallId 对应的 agent_run 处于 awaiting_approval
  -> 复用该 agent_run（同 run_id、同 assistant message、同消息下标）
  -> 创建一个新的 AgentJob 承载后续轮次
  -> agent_run.status 回到 running
否则
  -> 按普通命令处理（新建 run）
```

**不要为审批回执新建 `agent_run`**，否则同一次对话会分裂成两条运行记录，续流和消息下标都会错乱。

### 7.3 提案由 HTTP 处理器同步应用

`ApplyProposal` 在**收到审批回执的 HTTP 请求里同步执行**，不交给 Worker：

- 用户需要立刻看到 `412`（base revision 已变）或 `409`，而不是等 Worker 轮询后从流里看到一个错误。
- `ApplyProposal` 是短事务，不调用外部服务，放在请求路径里不违反 `arch.md` §11.1。

顺序是：鉴权 → 同步 `ApplyProposal` → 记录工具结果 → 唤醒 Worker 续跑 → 开始 SSE 流。应用失败时不续跑运行，直接返回错误状态码。

### 7.4 其他规则

- **审批回执必须重新鉴权。** `add-tool-result` 里的 `toolCallId` 必须归属当前用户的 thread，否则 `404`。
- **base revision 变化时返回 `412`**，提案标记为冲突，不静默覆盖（`arch.md` §9.1）。
- **拒绝理由要回灌模型。** 这是对话式修正相对一次性生成的核心价值，不能只是丢弃提案。
- **重复回执要幂等。** 同一个 `toolCallId` 第二次提交决定时返回 `409`，不重复应用提案。

## 8. 与现有 Worker / Job 的关系

> ❌ **本节已被 [ADR-0005](adr/0005-libfx-agent-harness.md) 取代，见 [`harness.md`](harness.md) §1.2。**
>
> 下方描述的是**当前已实现**的行为，作为迁移起点仍然准确，但迁移后：
>
> - **WASM 模式不创建 `AgentJob`**，循环由浏览器宿主驱动。照旧创建会导致 Worker 用旧的
>   `toolLoop` 把同一个 run **再跑一遍**。
> - **sidecar 模式仍创建 job**，但 Worker 转调 sidecar 的 `POST /run`，不再调 `toolLoop`。
> - 因此下面那句「这一点决定了『关掉浏览器任务还在跑』能否成立」**只在 sidecar 模式成立**。
> - §8.2 的 fx 迁移顺序依赖已完成（`wiring.md` 全部落地），该小节仅留作历史记录。

**运行由 Worker 执行，HTTP 处理器只做订阅和转发。** 这一点决定了「关掉浏览器任务还在跑」能否成立。

```text
POST /agent/commands
  -> 一个事务内：写入用户消息 + 创建 agent_run + 创建 AgentJob(type="agent_run")
  -> 订阅内存 hub（按 run_id）
  -> 边收边写 SSE，同时 Worker 在另一侧写 agent_run_chunks
```

- Worker 侧新增 `case "agent_run"`（`worker.go:104` 的 switch），复用既有租约、fencing token、attempt 校验、重试与取消。
- Hub 是进程内的 run_id → 订阅者扇出，**SQLite 是持久日志，hub 只是实时通道**。两者缺一不可：只有 hub 则重连丢数据，只有 DB 则延迟高。
- 当前 Worker 轮询间隔 300ms（`config.go:61`）。首个 token 的延迟因此最多多 300ms，可接受；后续可加一个入队通知 channel 消除这段延迟，属优化而非必需。
- HTTP 处理器**不得持有数据库事务**跨越整个流。

### 8.1 旧对话路径的处置

`POST /api/v1/conversations/{id}/messages`（`server.go:1024`）会创建 `type="conversation"` 的单轮作业，与新的 agent 运行是两套并行系统。处置方式：

| 对象 | v1 处置 |
|---|---|
| `POST /conversations/{id}/messages` | **停止创建新作业**，返回 `410 Gone`，提示改用 `/agent/commands` |
| `GET /conversations`、`GET /conversations/{id}/messages` | 保留只读，用于展示迁移前的历史对话 |
| Worker 的 `case "conversation"` | 保留，处理迁移期间可能残留的队列作业；队列清空后由后续提交删除 |
| `agent.Provider.ConversationReply` | 随上一条一起保留，不再有新调用方 |

**不要保留两条可写路径。** 两套系统同时能写会让「这条对话在哪张表里」变成每次排查都要先确认的问题。

### 8.2 与 fx 迁移的顺序依赖

本文档与 [`doc/wiring.md`](wiring.md) 有两处交叠，必须协调，否则会互相覆盖：

| 交叠点 | 处置 |
|---|---|
| `main.go:95` 的 `WriteTimeout` 改为 `0` | **归 `wiring.md` 步骤 2**（fx 化装配时一并改）。本文档 §9.2 只描述要求，不重复实施 |
| Worker 的作业类型 `switch`（`worker.go:104`） | 若 `wiring.md` 步骤 5 已完成，**按值组注册 `agent_run` 处理器**；若尚未完成，先加 `case`，由步骤 5 一并改造 |

**推荐顺序：`wiring.md` 步骤 1–2 先行**，再并行推进两条线。步骤 1–2 只动装配不动业务逻辑，风险低、收益是给 Agent 运行时的新组件提供 lifecycle 挂载点。

## 9. HTTP 与运维落地

### 9.1 必须绕过 Huma

`/api/v1/agent/*` 的端点用裸 Gin handler 注册：三个流式端点的原因见 §2.4，恢复读 `GET /agent/thread-state` 同组注册，共用同一个 handler 级认证入口。随之而来的义务：

- 在 `doc/interface.md` 中显式记录这是「Huma 为字段级事实来源」的例外。
- 为这一组端点手工维护 OpenAPI 描述，保证其他 FastResearch 项目仍能拿到完整契约。

### 9.2 必须修掉的阻塞项

**`WriteTimeout: 60 * time.Second`（`main.go:95`）会掐断任何超过 60 秒的 SSE 流。** 处理方式：把 `http.Server.WriteTimeout` 设为 `0`，改用 `http.ResponseController` 对每个请求单独设置写截止时间——普通 JSON 端点保持 60 秒，流式端点不设或设很长。**不要简单地把全局 `WriteTimeout` 调大**，那会同时削弱所有普通端点的保护。这项改动由 `wiring.md` 步骤 2 实施，见 §8.2。

### 9.3 其他要点

- 每个 chunk 写完后必须 `Flush`，否则会被缓冲。
- 建议每 15 秒发一个 SSE 注释行（`: ping\n\n`）作为心跳，穿透反向代理的空闲超时。注释行不是 `data:` 行，不会进解码器。
- 反向代理需关闭该路径的响应缓冲（nginx：`proxy_buffering off`）。
- 现有 CSP 的 `connect-src 'self'`（`server.go:61`）允许同源 SSE，无需修改。
- **幂等中间件需要放行这一组端点。** 现有 `idempotencyMiddleware`（`server.go:1588`）会缓存响应体，对流式响应无意义且有害。
- 速率限制：按用户限制并发运行数，建议每用户同时最多 1 个活跃运行。

## 10. 测试要求

沿用仓库既有的测试密度标准（README「测试」一节的覆盖清单）。至少覆盖：

**协议层**
- chunk 编码器对每种类型产出合法 JSON，`path` 规则正确。
- `update-state` 的 `append-text` 前必有建立字符串的 `set`。
- 流以 `[DONE]` 结束；响应不含 `event:` 行。
- `resume-state` 在无活跃运行时返回 `204`，有活跃运行时返回含 `state` 与字符串 `runId` 的 `200`；`__LOCALID_` 形式的 `threadId` 解析到调用者自己的在途运行，且解析不到别人的。
- `thread-state` 在无历史时返回 `204`，有历史时返回 §2.7 结构与 `fasttask.threadId`；未 checkpoint 的运行由持久化消息重建；跨用户不可见。

**运行时**
- 客户端断开后运行继续，重连可重放并接上。
- 显式取消能终止运行；客户端断开不能。
- 轮数上限、墙钟超时、工具超时各自触发正确的错误码。
- 进程重启后运行标记为 `interrupted`，部分消息保留。

**安全与不变量**
- 跨用户 `threadId` / `toolCallId` 返回 `404`。
- 客户端伪造的 `state` 不影响服务端权威状态。
- 工具参数里带 `user_id` 会被拒绝。
- `proposal` 级工具执行后业务表零写入。
- 提案应用时 base revision 不匹配返回 `412`。
- 模型返回非法工具参数时运行不崩溃，错误回灌模型。
- 每日计划提案不能绕过「最多三个核心项」和「候选不足不补位」。

**迁移**
- v4 库升级到 v5 后 `/health/ready` 正常且既有数据完整。

## 11. 分阶段交付

每个阶段结束时 `go test ./...`、`go vet ./...` 和 `npm run build` 必须通过。

| 阶段 | 内容 | 验收 |
|---|---|---|
| A | migration `000005` + 数据模型 + 仓储 | v4→v5 升级测试通过 |
| B | chunk 编码器 + 三个端点（回声实现，不接模型） | 前端能连上并渲染出一条假消息 |
| C | 工具注册表 + 只读工具 + 循环（无提案工具） | agent 能读上下文并回答问题 |
| D | 提案工具 + 审批流 + `ApplyProposal` 对接 | 对话里能改任务树，且不变量测试全过 |
| E | 每日计划的 agent 参与 | 确定性取三不被绕过 |
| F | 续流 + 取消 + 限流 + 心跳 | 移动端断线重连可用 |

阶段 B 是关键检查点：**在写完整编码器之前，先用回声实现验证协议理解是否正确。** 本文 §2 已核对到实现级别，但一次端到端验证的成本远低于在错误理解上叠加五个阶段的代码。
