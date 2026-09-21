# FastTask Agent 实现规格

> 文档状态：初版实现规格  
> 上位文档：[`doc/agent.md`](agent.md)（那份定判断，本文定落地）  
> 决策记录：[ADR-0002](adr/0002-assistant-transport.md)、[ADR-0003](adr/0003-in-process-agent-loop.md)  
> 协议基线：`@assistant-ui/react` 0.15.21 / `@assistant-ui/core` 0.3.20 / `assistant-stream` 0.3.44

## 1. 文档定位

本文是可以直接照着写代码的规格。协议部分的全部字段和语义均核对自上述版本的类型定义与运行时实现，不是根据文档推测的。**升级 assistant-ui 大版本时必须重新核对本文 §2。**

## 2. 传输协议契约

### 2.1 端点

三个端点全部是 `POST`，全部使用同一组请求头（前端 `headers` 选项返回的内容，含 `Authorization`）。

| 前端选项 | 路径 | 用途 |
|---|---|---|
| `api` | `POST /api/v1/agent/commands` | 提交命令并开启一次运行 |
| `resumeStateApi` | `POST /api/v1/agent/resume-state` | 取回保留的状态与 `runId` |
| `resumeApi` | `POST /api/v1/agent/resume` | 续流一次进行中的运行 |

前端必须显式设置 `protocol: "assistant-transport"`。**该选项默认值是 `"data-stream"`，漏设会静默走错协议且难以排查。**

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

**安全规则：`state` 和 `threadId` 是不可信输入。** `state` 由客户端持有，服务端**只能**把它当作乐观并发的提示，绝不能据此重建权威状态——权威状态一律从 SQLite 读取。`threadId` 必须校验归属于当前认证用户，否则返回 `404`（遵循 `arch.md` §12 的跨用户 `404` 惯例）。`user_id` 永远取自认证上下文。

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

`fasttask` 命名空间承载对话之外的业务状态，前端 `converter` 把它取出来单独渲染（提案 diff 卡片、今日计划预览）。**这是让对话与业务视图共用一条流的机制**，不要再为它另开轮询。

### 2.8 续流语义

`resumeStateApi` 请求体是 `{"threadId":"..."}`，响应二选一：

- `204 No Content` —— 没有进行中的运行，客户端跳过续流。
- `200` + `{"state": <任意>, "runId": "<字符串>"}` —— 两个键都必需，`runId` 必须是字符串，否则客户端抛错。

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

### 4.1 续流覆盖范围

明确区分两种断线，不要对外宣称超出实际能力的保证：

| 情况 | 行为 |
|---|---|
| **客户端断线，服务端仍在跑**（锁屏、切网、PWA 切后台） | 完整支持。运行继续，重放 `agent_run_chunks` 后接上实时流 |
| **服务端进程重启**（部署、崩溃） | v1 **不跨进程续跑**。租约过期后运行标记为 `interrupted`，已产出的部分消息保留，用户可重试 |

第一种是移动端的常见情况，也是选择本协议的主要动机。第二种留待后续按需处理。

### 4.2 客户端断开不等于取消

HTTP 请求的 `AbortSignal` 触发时**不得取消运行**。运行由 Worker 持有，继续执行并继续写 `agent_run_chunks`。只有显式调用现有的 `PUT /api/v1/agent-jobs/{job_id}/cancellation`（`app.go:789`）才取消。

## 5. 工具注册表

### 5.1 注册方式

每个 application 服务通过 fx 值组 `group:"agent_tools"` 注册自己的工具（见 [`doc/wiring.md`](wiring.md) §5），不在中心文件枚举。

工具定义至少包含：名称、描述、JSON Schema 参数、级别（`readonly` / `proposal`）、执行函数。

### 5.2 强制规则

- **参数里不得出现 `user_id`、`workspace_id` 或任何身份字段。** 身份来自执行上下文。出现即视为实现错误，需有测试覆盖。
- 每个工具的参数必须经 JSON Schema 校验后再进入领域层，模型输出按 `arch.md` §12 属于不可信输入。
- `readonly` 工具在数据库事务外执行。
- `proposal` 工具只创建 `Proposal` 记录，**不得触碰业务表**。
- 工具执行失败返回结构化错误给模型（可自我修正），不直接让整个运行失败。

### 5.3 清单

见 [`doc/agent.md`](agent.md) §5。新增工具必须同时更新那份清单和本节的测试要求。

## 6. 循环执行规则

```text
载入 thread 历史 + 系统提示 + 工具注册表
循环：
  调用模型（带工具定义）
  若返回文本 -> 以 append-text 流式推送
  若返回 readonly 工具调用 -> 执行、结果回灌上下文、继续循环
  若返回 proposal 工具调用 -> 创建 Proposal、推送待审批工具调用、转 awaiting_approval、结束本次流
  若无工具调用且有完整回复 -> 结束运行
每轮开始前检查 cancel_requested
```

保守默认值（接真实模型后按观测调整，属 `agent.md` §11 待确认项）：

| 限制 | 默认值 | 超出时 |
|---|---|---|
| 单次运行最大模型轮数 | 8 | 结束运行并告知用户 |
| 单次运行墙钟超时 | 180 秒 | 标记 `failed`，错误码 `RUN_TIMEOUT` |
| 单次工具执行超时 | 10 秒 | 该工具返回错误，循环继续 |
| 单轮最大输出 token | 沿用 Provider 配置 | — |

错误分类复用现有 `worker.go:214` 的约定：可重试错误进 `TEMPORARY` 并回到 `queued`，不可重试进 `PROVIDER_ERROR`，revision 冲突进 `REVISION_MISMATCH`。

## 7. 审批流

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

三条规则：

- **审批回执必须重新鉴权。** `add-tool-result` 里的 `toolCallId` 必须归属当前用户的 thread，否则 `404`。
- **base revision 变化时返回 `412`**，提案标记为冲突，不静默覆盖（`arch.md` §9.1）。
- **拒绝理由要回灌模型。** 这是对话式修正相对一次性生成的核心价值，不能只是丢弃提案。

## 8. 与现有 Worker / Job 的关系

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

## 9. HTTP 与运维落地

### 9.1 必须绕过 Huma

三个 agent 端点用裸 Gin handler 注册，原因见 §2.4。随之而来的义务：

- 在 `doc/interface.md` 中显式记录这是「Huma 为字段级事实来源」的例外。
- 为这三个端点手工维护 OpenAPI 描述，保证其他 FastResearch 项目仍能拿到完整契约。

### 9.2 必须修掉的阻塞项

**`WriteTimeout: 60 * time.Second`（`main.go:95`）会掐断任何超过 60 秒的 SSE 流。** 处理方式：把 `http.Server.WriteTimeout` 设为 `0`，改用 `http.ResponseController` 对每个请求单独设置写截止时间——普通 JSON 端点保持 60 秒，流式端点不设或设很长。**不要简单地把全局 `WriteTimeout` 调大**，那会同时削弱所有普通端点的保护。

### 9.3 其他要点

- 每个 chunk 写完后必须 `Flush`，否则会被缓冲。
- 建议每 15 秒发一个 SSE 注释行（`: ping\n\n`）作为心跳，穿透反向代理的空闲超时。注释行不是 `data:` 行，不会进解码器。
- 反向代理需关闭该路径的响应缓冲（nginx：`proxy_buffering off`）。
- 现有 CSP 的 `connect-src 'self'`（`server.go:61`）允许同源 SSE，无需修改。
- **幂等中间件需要放行这三个端点。** 现有 `idempotencyMiddleware`（`server.go:1588`）会缓存响应体，对流式响应无意义且有害。
- 速率限制：按用户限制并发运行数，建议每用户同时最多 1 个活跃运行。

## 10. 测试要求

沿用仓库既有的测试密度标准（README「测试」一节的覆盖清单）。至少覆盖：

**协议层**
- chunk 编码器对每种类型产出合法 JSON，`path` 规则正确。
- `update-state` 的 `append-text` 前必有建立字符串的 `set`。
- 流以 `[DONE]` 结束；响应不含 `event:` 行。
- `resume-state` 在无活跃运行时返回 `204`，有活跃运行时返回含 `state` 与字符串 `runId` 的 `200`。

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
