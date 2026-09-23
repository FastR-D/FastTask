# FastTask Agent Harness 实现规格（libfx 双宿主）

> 文档状态：🟢 已实现（§1–§15 全部落地；§16 是 spike 记录）
> 更新：2026-09-23
> 上位决策：[ADR-0005](adr/0005-libfx-agent-harness.md)
> 相关：[`agent.md`](agent.md)（不变量与工具分级，未变）、[`agent-impl.md`](agent-impl.md)（传输协议与数据模型，未变）、
> [`chat-features.md`](chat-features.md)（多会话 / 思考过程 / 图片附件）、[`wiring.md`](wiring.md)（新增 `SidecarModule`）

## 0. 术语

**uber-fx** = `go.uber.org/fx`（DI 容器）。**libfx** = npm 包 `libfx`（vercel-labs/fx 的嵌入 SDK）。本文不使用裸「fx」。

## 1. 文档定位

### 1.1 `agent-impl.md` 的哪些章节被取代

| `agent-impl.md` 章节 | 状态 | 说明 |
|---|---|---|
| §2 传输协议 | ✅ 继续有效 | assistant-transport 不变 |
| §3 数据模型 | ⚠️ **被扩充** | 新增 `thread_id` 归属与两种 part，见 [`chat-features.md`](chat-features.md) §5 |
| §4 Run 状态机 | ⚠️ **取值不变，驱动方变更** | 见 §11 |
| §5.1 工具注册方式 | ❌ **被取代** | 改为 §3.4 的 HTTP 投影 |
| §5.2 工具强制规则 | ✅ 继续有效 | |
| §6 循环执行规则 | ❌ **被取代** | 循环驱动交给 libfx；限制表与错误分类仍由 Go 强制 |
| §7 审批语义 | ⚠️ **语义不变，时序变更** | 同一 turn 内等待，见 §7 |
| **§8 与现有 Worker / Job 的关系** | ❌ **被取代** | **见 §1.2，这是最容易做错的一处** |
| §9 HTTP 与运维落地 | ✅ 继续有效 | |
| §10 测试要求 | ✅ 继续有效，本文 §14 追加 | |

### 1.2 §8 为什么被取代：WASM 模式下没有 AgentJob

`agent-impl.md` §8 定义的现有流程是：

```text
POST /agent/commands
  → 一个事务内：写用户消息 + 创建 agent_run + 创建 AgentJob(type="agent_run")
  → Worker 领取并执行
```

代码已落地（`agent.go:243`，审批续跑处另有一份 `agentapproval.go:217`）。
**在 WASM 模式下循环跑在浏览器，如果照旧创建 job，Worker 会用旧的 `toolLoop` 把同一个 run 再跑一遍。**

定案：

| 模式 | `/agent/commands` 的行为 |
|---|---|
| **WASM** | 一个事务内写用户消息 + 创建 `agent_run`（状态 `queued`），**不创建 `AgentJob`**；响应头带 `X-Harness-Run: <run_id>`，随后照旧订阅 hub 并流式输出。浏览器宿主凭 §10 换取令牌后驱动该 run |
| **sidecar** | 一个事务内写用户消息 + 创建 `agent_run` + 创建 `AgentJob(type="agent_run")`；Worker 领取后**不再调用 `toolLoop`**，而是转调 sidecar 的 `POST /run`（§8.3） |

请求体新增 `harness_mode: "wasm" | "sidecar"` 字段，由前端在 §3.2 探测后填入。

- 该字段**只决定谁驱动**，不影响任何不变量，因此可以来自客户端。
- 服务端仍要校验：`harness_mode: "sidecar"` 但 sidecar 不健康时，返回 `HARNESS_UNAVAILABLE` 而不是创建一个永远不会被执行的 job。
- `agentapproval.go:217` 的第二处 job 创建随 §7 的「同一 turn 内审批」一并移除——审批不再需要新 run。

**直接后果**：`agent-impl.md` §8 开头那句「**这一点决定了『关掉浏览器任务还在跑』能否成立**」
**只在 sidecar 模式成立**。WASM 模式下关闭标签页 run 置 `interrupted`（§5.2、§11）。

实现顺序见 §13。

## 2. 拓扑

```text
┌───────────────────────── 浏览器 ─────────────────────────┐
│  UI（assistant-ui）──── SSE / 命令 ────┐                 │
│    只从服务端状态渲染                   │                 │
│                                        │                 │
│  Harness 宿主（WASM 模式，默认）       │                 │
│    fx-core.wasm + JSPI                 │                 │
│      │ fetch 覆盖                      │                 │
│      ▼                                 │                 │
│    gateway shim                        │                 │
│    @ai-sdk/openai-compatible           │                 │
│      │ 工具 execute                    │                 │
└──────┼─────────────────────────────────┼─────────────────┘
       │ OpenAI 兼容请求（同源）          │
┌──────▼─────────────────────────────────▼─────────────────┐
│                        Go 后端                           │
│  ┌──────────────┐ ┌────────────┐ ┌──────────┐            │
│  │ /…/openai    │ │ 工具执行面  │ │ 运行 hub │            │
│  │ 凭据注入      │ │ToolRegistry│ │   SSE    │            │
│  │ 覆盖 sys/tools│ │            │ │          │            │
│  └──────┬───────┘ └─────┬──────┘ └──────────┘            │
│         │  ← 权威状态写入点 →                             │
│  ┌──────▼──────────────▼────────────────────────────┐    │
│  │ AgentRun / AgentMessage / AgentMessagePart        │    │
│  │ AgentThread / agent_run_chunks（SQLite）          │    │
│  └───────────────────────────────────────────────────┘    │
└──────┬───────────────────────────────────────────────────┘
       │                         ▲ 同一个 /openai 端点
       ▼ NewAPI → qwen3.8-max    │
                       ┌─────────┴────────────────────────┐
                       │ Node sidecar —— 仅 JSPI 缺失时    │
                       │   libfx N-API + 同一份 shim       │
                       └──────────────────────────────────┘
```

三条必须记住的事实：

1. **gateway shim 跑在宿主进程内**（浏览器或 sidecar），**不在 Go 里**。
   §16 的 spike 已实测确认它能在浏览器生产产物中运行。
2. **Go 只有一个模型端点，OpenAI 兼容格式，两种模式完全相同。**
   Go 不实现 LanguageModelV4 协议——那层由 shim 在宿主侧消化掉了。
3. **sidecar 是兜底，不是必需。** 仅当浏览器缺 JSPI 时需要它当 libfx 宿主。

## 3. 宿主契约（两模式共用）

宿主逻辑放在 `web/src/harness/`，编译产物同时被浏览器入口和 sidecar 入口引用。

### 3.1 硬规则

1. **libfx 的类型不得出现在 `web/src/harness/` 之外。** UI 层看不到 `createFxAgent`、`HostTool`、turn event。违反即视为实现错误，需有 lint 或测试覆盖。
2. **宿主不持久化任何东西。** 不写 localStorage、不写 IndexedDB、不写文件。checkpoint 也经服务端存取（§6）。
3. **宿主不渲染。** 它消费 libfx 的 turn event 只为驱动循环与上报诊断，UI 从服务端 SSE 渲染。
4. **宿主不持有模型凭据。** `apiKey` 传的是运行能力令牌（§10），真实凭据只在 Go 侧。

### 3.2 模式探测

```ts
// web/src/harness/backend.ts
import { getBackendInfo } from 'libfx'

export type HarnessMode = 'wasm' | 'sidecar'

export async function selectMode(forced?: HarnessMode): Promise<{
  mode: HarnessMode
  reason: string
}>
```

规则：

- 管理员或用户在设置中强制指定时，直接采用，不探测。
  🟢 **该开关已暴露给用户**：对话页状态栏右侧的「运行位置」（自动 / 本机运行 / 服务端运行，
  `web/src/agent/HarnessStatus.tsx` 的 `HarnessModeControl`）。选择会**清掉本会话的探测缓存**并重新预热，
  因此切换立即生效，不必刷新。
  偏好本身存在 `web/src/agent/modePreference.ts`（`localStorage`，键 `fasttask.harness.mode`，
  `auto` 以「不存这个键」表示），**故意不放在 `web/src/harness/` 里**：§3.1 规则 2 要求宿主层不落任何持久状态，
  `harness/boundary.test.ts` 会据此扫描该目录。存的是**用户的指令**，不是探测结论，两者不可混为一谈。
- 否则调用 `getBackendInfo({ surface: 'agent', backend: 'wasm', wasm: <自托管 URL> })`。返回 `backend: 'wasm-jspi'` 则用 WASM 模式；返回 `'unavailable'` 则降级 sidecar，并把 `attempts[].reason`（如 `LIBFX_JSPI_UNAVAILABLE`、`LIBFX_WASM_LOAD_FAILED`）带进降级原因。
- **探测每个会话做一次，结果只存内存。** 不写 localStorage——浏览器升级会改变能力，缓存会导致长期错判。
- 探测会编译 2 MB wasm。必须在首次进入对话页**之前**、而不是发送第一条消息时触发，避免首条消息多出编译延迟。
- 降级必须可见：对话页状态栏显示当前模式；降级时附一行原因。**不得静默降级。**

sidecar 不可用且 WASM 也不可用时，agent 功能置为不可用并给出明确错误，**不退化成单轮对话**——与 `agent-impl.md` §5.1.1 对 `PROVIDER_NO_TOOL_SUPPORT` 的处置一致。

### 3.3 Agent 构造

```ts
// web/src/harness/agent.ts
const agent = await createFxAgent({
  apiKey: harnessToken,                 // §10，不是模型凭据
  model: serverConfig.modelId,      // 由管理员配置下发，宿主不调用 listModels()
  backend: mode === 'wasm' ? 'wasm' : 'native',
  wasm: mode === 'wasm' ? wasmAssetUrl : undefined,   // §9，自托管
  fetch: proxyFetch,                // §4，覆盖全部网络出口
  instructions: serverConfig.instructions,             // §3.5
  tools: buildHostTools(serverConfig.tools, harnessToken), // §3.4
  checkpoint: restored,             // §6
  onEvent: reportDiagnostics,
})
```

`model` 与 `instructions` 均来自服务端下发（§10 的 `POST /agent/runs` 响应），**宿主不得内置默认值**。

### 3.4 工具描述符：服务端是唯一事实来源

**不得在 TypeScript 里重复声明工具。** 这是 `BuiltinJobHandlers` 平行清单问题的同一类陷阱，必须结构性避免。

Go 侧新增：

```
GET /api/v1/agent/tools    →  { tools: [{ name, description, input_schema }] }
```

响应直接由 `ToolRegistry`（`internal/application/agenttool.go`）序列化得到，顺序沿用 `ToolRegistry.order`。

宿主把每一项包成 `HostTool`：

```ts
function buildHostTools(defs: ToolDef[], harnessToken: string): HostTool[] {
  return defs.map(def => ({
    name: def.name,
    description: def.description,
    inputSchema: def.input_schema,
    async execute(input, { signal }) {
      return callToolEndpoint(def.name, input, harnessToken, signal)   // §5
    },
  }))
}
```

约束：

- libfx 的工具上限是 **64 个**，当前 9 个，`ToolRegistry` 超过 60 时构造应告警。
- `agent-impl.md` §5.2 的身份字段禁令由 `NewToolRegistry` 在构造期强制（`identityArgNames`），本端点只是投影，不做二次校验，但**必须有测试断言该端点的输出不含任何身份字段**。
- **删除 `agenttool.go:82` 关于 uber-fx 值组 `agent_tools` 的注释。** 该值组从未存在，实际构造在 `agent.go:89` 的 `append(NewReadonlyTools(app), NewProposalTools(app)...)`。注释与代码的这处矛盾必须在本次一并清理。

### 3.5 instructions

`agentloop.go:20` 的 `defaultSystemPrompt` 原样迁移，由 `POST /agent/runs` 下发。

- libfx 不加任何隐藏 base prompt（官方文档：「libfx adds no hidden base prompt」），因此模型看到的系统上下文与现在完全一致。
- 上限 **64 KiB UTF-8**，且该上限包含 MCP 与 skills 适配器拼接进来的文本。当前提示词远未触及，但 §6.3 的降级路径会往里塞历史摘要，**必须在服务端做长度检查并截断**，不能让 `createFxAgent` 在客户端抛错。
- 我们**不使用** libfx 的 skills 与 MCP 适配器。工具全部经 §3.4 提供。

### 3.6 一次 turn

```ts
const turn = agent.prompt(userText, { signal })
for await (const ev of turn) {
  // 仅用于诊断与取消判定；不渲染，不写库
}
const { stopReason, usage } = await turn.result
const checkpoint = await agent.checkpoint()
await postCheckpoint(harnessToken, checkpoint)   // §6
```

必须遵守的 libfx 语义：

- **一个 turn 只能有一个事件消费者**，且必须消费。只 await `turn.result` 而不读事件流会等待流自行排空。
- **跳出迭代器会取消 turn。** 取消传播到工具的 `signal`。
- 同时只能跑一个 `prompt`。并发发送由 UI 层禁止。
- libfx 在传输失败后**最多自动重试一次**，且仅在模型输出或工具副作用逸出之前。这与 `agent-impl.md` §6 的错误分类叠加，不替代它——Go 侧的 `TEMPORARY` / `PROVIDER_ERROR` 分类仍在代理层做。

### 3.7 宿主实例的生命周期（多会话下尤其重要）

libfx 文档：「libfx compiles each stable Wasm source once and **creates a separate WebAssembly
instance for every Agent**」。每个 agent 实例约 2 MB 常驻内存。

[`chat-features.md`](chat-features.md) §2.1 的多会话里 `runtimeHook` 是**按线程实例化**的，
如果宿主跟着一起实例化，10 个线程就是 20 MB+，移动端撑不住。

硬规则：

1. **同一时刻只为「活动线程」持有一个 agent 实例。**
2. 切换线程时对旧实例 `await agent.close()`，再为新线程 `createFxAgent`（带该线程的 checkpoint）。
3. 旧线程有运行中的 turn 时，切换必须先取消（§5.2），**不允许后台留一个跑着的 turn**。
   这与 §15「WASM 模式下的并发运行数 = 1」是同一条约束的两种表述。
4. sidecar 模式下同理：sidecar 每个 run 建一个 agent，`POST /run` 返回后立即 `close()`，不做实例池化。

> **实例复用是明确的非目标。** checkpoint 已经承担了「跨 turn 保留历史」的职责（§6），
> 复用实例只省一次 wasm 实例化，却要引入实例生命周期管理和跨线程串味的风险。

### 3.8 libfx 不发布任何类型声明

实测 `libfx@0.0.10` 的 `package.json` 中 `types` 与 `typings` **均不存在**，包内无任何 `.d.ts`。

因此：

- 必须在 `web/src/harness/libfx.d.ts` 自己写 ambient 声明，覆盖我们实际用到的 API
  （`createFxAgent`、`getBackendInfo`、`HostTool`、turn event）。
- **该文件是我们对 libfx 的理解快照，不是权威**。升级 libfx 时必须重新核对，
  并在 §14 的契约测试里用运行时断言兜住（例如断言 `agent.prompt` 与 `agent.checkpoint` 存在）。
- 这也是一个对外贡献机会：向 `vercel-labs/fx` 提 PR 补 `.d.ts` 是低风险高收益的第一个 PR。

## 4. 模型代理

> **本节在 2026-09-23 的 [§16](#16-phase-0-spike已完成) spike 后整体重写。**
> 原先的设计是「Go 实现 LanguageModelV4 服务端 + 转发给 sidecar」。
> spike 证明 gateway shim 可在浏览器生产产物中运行，因此该层下沉到宿主，
> **Go 侧退化为一个普通的 OpenAI 兼容代理**，实现量与风险都大幅下降。

### 4.1 为什么 Go 不再需要实现 LanguageModelV4

libfx 打的是 `https://ai-gateway.vercel.sh/v4/ai/language-model`，请求体是
`LanguageModelV4CallOptions` 原样 JSON，响应是 `LanguageModelV4StreamPart` 的 SSE
（协议细节见 [ADR-0005](adr/0005-libfx-agent-harness.md) §5.1，客户端源码 Apache-2.0 可读）。

我们用 `fetch` 覆盖把这个请求截到**同进程内的 shim**，由
`@ai-sdk/openai-compatible` 把 V4 翻译成 OpenAI 兼容格式再发出去。
因此 **Go 从头到尾只见 OpenAI 格式**，V4 协议不跨进程。

### 4.2 shim（宿主侧，两种模式共用同一份）

```ts
// web/src/harness/shim.ts（🟢 实现；下面是骨架，真实文件还处理目录请求与未知端点）
import { createOpenAICompatible } from '@ai-sdk/openai-compatible'

export function createGatewayFetch(options): typeof fetch {
  // libfx 发往 ai-gateway.vercel.sh 的请求在这里被截下
  return async (input, init) => {
    const path = gatewayPathOf(input)
    if (path === null) return globalThis.fetch(input, init)   // 不是网关请求，原样放行
    if (path === GATEWAY_MODELS_PATH) return modelCatalogResponse(options.model)
    if (!GATEWAY_CHAT_PATHS.includes(path)) return errorResponseOf(...)   // 未知端点：不放行、只报告
    // ⚠️ body 不一定是字符串：Node 交过来的是字节。用 Response 自己的读取器解码，
    //    才能同时吃下 string / Uint8Array / ReadableStream。（真机实测踩过这个坑）
    const opts = await parseCallOptions(init?.body)      // LanguageModelV4CallOptions
    const model = createOpenAICompatible({
      name: 'fasttask',
      // ⚠️ baseURL 必须是绝对 URL —— SDK 内部对它做 new URL()。
      //    同源的绝对 URL 仍是同源，不触发 CORS。（spike 实测踩过这个坑）
      baseURL: gatewayBaseURL(options.runId, options.serverOrigin),
      apiKey: currentToken(options.harnessToken),        // 占位；真实凭据由 Go 注入
    }).languageModel(opts.model ?? options.model ?? 'server-decides')
    const { stream } = await model.doStream(opts)
    return streamResponseOf(stream)                      // 编码回 V4 分片给 libfx
  }
}
```

要点：

- **`baseURL` 必须绝对。** 传相对路径会抛 `TypeError: Failed to construct 'URL': Invalid URL`。
- shim 不碰凭据。`apiKey` 传 harness token 占位，Go 用真实凭据替换（§4.3）。
  token 可以是 getter，这样心跳轮换 token 时不必重建 agent（§10.4）。
- 浏览器与 sidecar 用**同一份文件**，差别只是 `location.origin` 与注入的 `serverOrigin`。
- **请求体不是字符串。** `JSON.parse(String(init.body))` 在浏览器测试里能过、在 Node 里必然失败
  （字节 → `"[object Uint8Array]"`），后果是每次补全都被 shim 自己 502 掉，libfx 无限重试直到
  turn 预算耗尽——**外部看起来就是"模型不回答"**。用 `new Response(body).text()` 解码，
  它接受所有 `BodyInit` 形态。
- **libfx 在补全之前会先拉模型目录**（`GET /coding-agent/v1/models`），且**目录失败对 turn 是致命的**：
  它会一直等，不报错。shim 因此必须**就地应答**目录请求（用服务端下发的 model 造一条目录），
  而不是把它当"非补全请求"放行——放行等于让运行时访问 Vercel，违反 ADR-0005 §3.3。
  同理，**未知的网关端点一律不放行**，只回错误并上报 `shim.unhandled`：libfx 升级新增调用时，
  这会表现为一条日志而不是一个挂死的 run。
- **`stopReason` 用的是 libfx 自己的词表**（真机实测为 `end_turn`，取消为 `cancelled`），不是 AI SDK 的
  `stop`。Go 侧只区分 `cancelled` / `error`，其余映射为协议的 `stop`，所以对外词表不随 libfx 变化。

以上四条都由 `web/src/harness/integration.test.ts` 守着：它加载**真实的 `libfx/node`**（原生插件），
对一个脚本化的 OpenAI 兼容端点跑完整 turn，断言目录被就地应答、补全打到代理路径、
bearer 是 capability 而非 provider key、工具真的被宿主执行。

### 4.3 Go 侧：一个普通的 OpenAI 兼容代理

```
POST /api/v1/agent/runs/{run_id}/openai/chat/completions
  认证：harness token
  Body：OpenAI 兼容 chat.completions 请求（由 shim 构造）
  →    OpenAI 兼容 SSE
```

Go 做四件事，**全部在 OpenAI 格式上做**，比原先在 V4 格式上做更直接：

1. **凭据注入**：把 `Authorization` 换成管理员配置的真实 key，`baseURL` 换成
   `ModelProvider.BaseURL`（`models.go:33`），`model` 换成 `ModelProvider.ModelName`。
   宿主发来的这三项一律丢弃。
2. **覆盖 `messages[0]`（system）与 `tools`**：用服务端的值**替换**，不是合并。
   宿主声明的工具集属不可信输入（`agent-impl.md` §6 既有规则）。
3. **展开附件引用**：`attachment_id` → OpenAI `image_url`，按 `user_id` 校验归属；
   宿主直传的 base64 一律丢弃。见 [`chat-features.md`](chat-features.md) §4.4。
4. **权威状态写入**（§4.4）。

> **`internal/agent/chat.go` 的 SSE 解析在这里复用。** 它本来就是按
> OpenAI-compatible 的 `tools` / `tool_calls` + 流式设计的（`agent-impl.md` §5.1.1），
> 正好是 Go 现在需要的那一半。§12 有更正说明。

### 4.4 权威状态的写入点

```text
NewAPI 的 OpenAI SSE
  ├─→ 原样转发给 shim（shim 再翻成 V4 给 libfx）
  └─→ 解析 delta，翻译成 agent-impl.md §2.5 的 chunk
        ├─→ agent_run_chunks（续流用）
        ├─→ AgentMessagePart（权威转录：text / reasoning / tool-call / image）
        └─→ 运行 hub → UI 的 SSE 连接
```

Go 要认的 delta 字段只有四类：`content`、`reasoning_content`、`tool_calls`、`finish_reason`。

两条支路从同一个 handler 分出，**不存在额外网络往返**。
服务端**从不接收客户端提交的转录**，不提供任何「上传本轮对话」的端点。

#### 4.4.1 工具调用的写入归属与去重（必读，最容易写重）

同一次工具调用会**三次**出现在服务端视野里：

| 出现位置 | 内容 |
|---|---|
| 本轮模型流 | `tool_calls` 增量（id + name + 分片的 arguments） |
| 工具执行面（§5） | 执行结果 / 提案引用 |
| **下一轮请求的 `messages` 里** | libfx 把完整历史带上，同一条 tool_call 与 tool 结果再次出现 |

归属定死：

1. **模型代理写"调用"**：`tool_calls` 完成时创建 tool-call part，写入 `tool_call_id` / `name` / `args`。
2. **工具执行面写"结果"**：按 `tool_call_id` **更新同一个 part**，写入 result 或 `approval.status`。**不创建新 part。**
   宿主拿不到 id 时（见下）按 `(run, 工具名, 最近一个尚无结果的 part)` 关联——宿主对**同名工具串行执行**，
   所以这个关联唯一。
3. **下一轮请求 `messages` 中的历史一律不写库。** 代理只从**响应流**取权威内容，
   **从不从请求体取**。这条同时封死了客户端伪造历史的路径。
4. 去重键是 `(run_id, tool_call_id)`，数据库加唯一索引兜底。

**spike 实测确认的两件事**（2026-09-23，生产产物）：

- `tool-call` 分片**确实携带 `toolCallId`**，实测值 `{ type:'tool-call', toolCallId:'call_abc123',
  toolName:'list_goals', input:'{"status":"active"}' }`。代理侧按 id 关联的主方案成立。
- **但宿主执行工具时拿不到这个 id**：libfx 调 `HostTool.execute(input, { signal })`，只给入参与一个
  abort signal（`web/node_modules/libfx/fx-sdk.js` 的 `executeHostTool`）。所以「宿主把 id 带回来」这条
  路走不通，§5 的请求体里 `tool_call_id` 是**可选**字段，缺失时按上面第 2 条的名字关联。
  分片里的 id 仍然照写，因为**去重键与下一轮历史比对都要用它**。
- **分片是交错的，不是严格嵌套。** 实测顺序：

  ```text
  … text-delta → tool-input-start → tool-input-delta → text-end → tool-input-end → tool-call → finish
  ```

  注意 `text-end` 出现在 `tool-input-start` **之后**。
  **chunk 翻译器不得假设 part 严格顺序或嵌套**，必须按 id 维护并行的 part 状态机。

> 仍未验证：libfx 的 `HostTool.execute(input, { signal })` 第二参数是否把调用 id 传给工具。
> **phase C 的第一件事就是打印这个参数。**
> 若拿不到，退化方案：Go 按 `(run_id, tool_name, 最近一个 result 为空的同名 part)` 关联；
> 同一轮并发调用**同名**工具时会歧义，届时需让宿主串行化同名调用。**退化方案必须有测试覆盖。**

### 4.5 凭据跨进程的安全边界

`ModelProvider.APIKeyCiphertext`（`models.go:33`）在库里加密，由
`ProviderService.ActiveProviderRuntime`（`admin.go:476`）的 `decryptSecret` 解密。

新架构下明文密钥**只在 Go 进程内**——这是相对原设计的一项净改善：
shim 下沉到浏览器后，凭据不再跨进程传给 sidecar。

sidecar 模式（仅 JSPI 缺失时）下 shim 跑在 Node 里，但它打的仍是 Go 的
`/openai` 端点，**凭据依然不离开 Go**。因此本架构下**不存在明文凭据跨进程的场景**。

### 4.6 版本漂移防护与对外贡献

- 锁定精确版本：`libfx@0.0.10`、`@ai-sdk/openai-compatible@3.0.53`、`@ai-sdk/provider@4.0.17`，
  **一律 `--save-exact`，不用 `^`**。
- shim 的 V4 编解码只依赖 `@ai-sdk/provider` 的类型，不自己解释语义。
- 契约测试用**录制的真实请求/响应夹具**，不手写 JSON。测法见 §14.1。

**对外贡献机会**（两个，都低风险）：

1. **给 `vercel-labs/fx` 补 `.d.ts`** —— libfx 不发布任何类型声明（§3.8），我们无论如何要自己写。
2. **把 shim 抽成独立包** —— "把 AI Gateway 协议请求转接到任意 AI SDK provider" 在生态里是空缺，
   Portkey / LiteLLM / Bifrost 都是 OpenAI 兼容网关，不是这个协议。

## 5. 工具执行面

```
POST /api/v1/agent/runs/{run_id}/tools/{name}
Authorization: Bearer <harness token>
Body: { "input": { ... } }
```

服务端行为与现有 `executeToolCall`（`agentloop.go:442`）一致：

1. 从 harness token 解出 `ToolContext`（`UserID` / `ThreadID` / `RunID` / `ActiveGoalID`）。**`UserID` 永不从 body 取。**
2. JSON Schema 校验 + 领域校验。
3. `readonly` 级：事务外直接执行，返回结果。
4. `proposal` 级：创建 `Proposal(status=pending)`，**不写业务表**，然后进入 §7 的审批等待。
5. 写 `AgentMessagePart`（tool-call part + 结果）并推送到运行 hub。
6. 工具执行失败返回**结构化错误**给宿主，由模型自我修正，不让整个运行失败。

单次工具执行超时沿用 10 秒（`agent-impl.md` §6），**审批等待不计入**（§7）。

请求体（🟢 两个字段都可选，见 §4.4.1 的可得性说明）：

```jsonc
{ "tool_call_id": "call_...", "input": { ... } }
```

`tool_call_id` 缺失时按 `(run, 工具名, 最近一个尚无结果的 part)` 关联；**关联不到就返回结构化错误**
（`no pending "<name>" call in this run`），让模型自己纠正，而不是凭空造一个 part——
否则 transcript 里会出现一次模型从未发起的调用。

### 5.1 一个 run = 一个用户消息 = 一次 `prompt()` = 一个 turn

这条基数关系此前未定义，现在定死：

```text
用户发一条消息
  → POST /agent/commands 创建 1 个 agent_run
  → 宿主 createFxAgent(带该线程 checkpoint) + agent.prompt(userText) 一次
  → turn.result 落定 → run 终态 → agent.close()
```

推论（都是实现时会撞上的）：

- **`awaiting_approval` 是 run 内部的中间态**，不再是 run 的终态。审批完回到 `running`，
  同一个 run 继续，**不创建新 run**。`agentapproval.go:217` 的建 job 逻辑因此删除。
- **checkpoint 在 run 终态时写一次**（§3.6），不在每轮模型调用后写。
- 一个 run 的最长存活 = 墙钟 180 秒 + 审批等待（每次最多 15 分钟）。
  因此令牌有效期不能只按 30 分钟算死，必须可经心跳续期（§10.3）。
- 一个 thread 有多个 run（一问一答一个），checkpoint 是 thread 级、跨 run 累积的。

### 5.2 取消

现状靠轮询 `AgentJob.cancel_requested`（`agentloop.go:209`）。新架构下 WASM 模式没有 job，
且 sidecar 也需要一条取消通道。定案：

```
POST /api/v1/agent/runs/{run_id}/cancellation      认证：用户主 JWT（不是 harness token）
```

Go 收到后把 run 置 `cancelling`，并通过**三条通道**同时通知：

| 通道 | 作用 |
|---|---|
| 心跳响应（§10.3）带 `cancel_requested: true` | 覆盖宿主空闲等待的情况 |
| 进行中的审批长轮询（§7）**立即返回 `cancelled`** | 覆盖等审批的情况 |
| 进行中的模型代理请求**主动关闭上游流** | 覆盖正在出字的情况，宿主的 `fetch` 随之 abort |

宿主收到任一信号后 `turn.cancel()`（或跳出 turn 迭代器），libfx 把取消传播到所有工具的 `signal`。
run 最终置 `cancelled`。

sidecar 模式额外需要 Go → sidecar 的取消通道，§8.3 的接口相应新增 `POST /run/{run_id}/cancel`。

> **取消是尽力而为，不是强一致。** 已经落库的提案不会回滚（它本来就要用户审批）；
> 已经执行的只读工具不需要回滚。取消只保证**不再产生新的模型调用与工具调用**。

## 6. checkpoint

### 6.1 存取

```
GET  /api/v1/agent/threads/{thread_id}/checkpoint   → { checkpoint: <base64>, libfx_version: "0.0.10" }
PUT  /api/v1/agent/threads/{thread_id}/checkpoint   ← { checkpoint: <base64>, libfx_version: "0.0.10" }
```

存在 SQLite，随 thread 生命周期。**必须存服务端**——存浏览器会断掉跨设备续聊，而移动端 PWA 是既定目标（`pwa.md`）。

`checkpoint` 是不透明的带版本字节，只含对话历史与用量。libfx 文档明确：host 拥有持久化，并需自行重新提供 models、credentials、instructions、tools。因此每次 `createFxAgent` 都要重新传 §3.3 的全部字段。

### 6.2 恢复

`createFxAgent({ ..., checkpoint })` 只能在**新建 agent** 时传入，不能对已有 agent 恢复。

### 6.3 版本漂移降级

`libfx_version` 与当前运行版本不一致，或恢复抛错时：

1. 记 `CHECKPOINT_VERSION_SKEW`（结构化日志 + 运行诊断，可观测）。
2. 从服务端权威 `AgentMessagePart` 重建对话摘要，拼进 `instructions`。**复用现有 `rebuildModelContext()`（`agentloop.go:612`）的裁剪逻辑，不要重写。**
3. 受 §3.5 的 64 KiB 上限约束，必须截断，保留最近若干轮 + 待处理提案。
4. **这是有损降级**，要在对话里向用户显示一条系统提示，说明早期上下文已压缩。

> 因此 `rebuildModelContext()` **不删除**。它从主路径变成降级路径。

## 7. 审批流

libfx 的 turn 在工具 `execute` 返回前不会推进。审批因此实现为**一个迟迟不返回的工具调用**，而不是像现在这样结束流再起新流。

```text
模型调用 propose_task_tree_patch
  └─ 宿主 execute → POST /agent/runs/{id}/tools/propose_task_tree_patch
       ├─ Go 校验参数 + 创建 Proposal(pending)，不写业务表
       ├─ Go 推 update-state：tool-call part，approval.status = "pending"，附结构化 diff
       ├─ Go 把 run 置为 awaiting_approval
       └─ Go 返回 { status: "pending", proposal_id }
  └─ 宿主长轮询 GET /agent/runs/{id}/approvals/{proposal_id}
       （用户在 UI 里决定 → assistant-transport 的 add-tool-result 命令 → Go）
       ├─ 批准：ApplyProposal 事务内重校验所有权 / base revision / 父子归属 / 循环
       └─ 拒绝：Proposal → rejected，拒绝理由入结果
  └─ 长轮询返回决定 → execute 用该结果 resolve
  └─ Go 把 run 置回 running，模型在同一个 turn 内继续
```

规则：

- **审批决定仍然只走 `add-tool-result`。** [ADR-0002](adr/0002-assistant-transport.md) §3.1 的限制不变：不得使用 `respondToApproval` / `hitl` / `humanTool`，不得填 `ToolCallMessagePart.approval` 之外的审批控件。
- **用长轮询，不用页内事件。** 两种宿主因此共用同一份代码；sidecar 模式下本来也没有「页内」。
- 长轮询超时默认 **15 分钟**，**不计入运行墙钟超时**（`agent-impl.md` §6 的 180 秒）。超时后工具返回 `APPROVAL_TIMEOUT` 结构化错误，run 置 `failed`。
- 长轮询应分段（如每 30 秒返回一次 `pending` 由宿主续轮询），避免被中间代理切断。
- WASM 模式下等待期间关闭标签页 → run 置 `interrupted`，`Proposal` 保持 `pending`。用户下次进入可看到该提案，但**不恢复运行**（`agent-impl.md` §4.2）。

相对现状的改进：审批后模型在**同一个 turn** 内继续，不需要 `resumeRun` 重建上下文。`agentloop.go:541` 的 `resumeRun` 因此只在续流场景保留。

## 8. Node sidecar（兜底）

> **2026-09-23 §16 spike 后本节降级。** sidecar 此前被定为必需组件（因为 gateway shim
> 被认为只能跑在 Node 里）。spike 证明 shim 可在浏览器运行，**sidecar 因此只剩一个职责，
> 且只在浏览器缺 JSPI 时需要**。

### 8.1 唯一职责：JSPI 缺失时的 libfx 宿主

| 职责 | 何时 |
|---|---|
| libfx 宿主（N-API）+ 同一份 gateway shim | **仅当** `getBackendInfo()` 探测不到 JSPI |

sidecar **不参与**凭据处理、不实现任何协议、不碰数据库。它打的是和浏览器完全相同的
Go `/openai` 端点（§4.3）。

**没有 sidecar 时 agent 仍然可用**，只是缺 JSPI 的浏览器（旧版 Chromium 内核、
iOS 27 以前的 Safari、flag 未开的 Firefox）用不了。部署方可以按自己的用户构成选择是否部署。

### 8.2 形态（🟢 已实现）

- 独立 Node 进程。源码 `sidecar/src/main.ts`，构建命令 **`npm run build:sidecar`（在 `web/` 下执行）**，
  产物 `sidecar/dist/host.mjs`。构建放在 `web/` 而不是 `sidecar/`，是因为 sidecar **复用**
  `web/src/harness/` 的 shim 与工具投影，依赖必须按 `web/node_modules` 解析。
- 产物是自包含 bundle，**只有 `libfx` 保持 external**：它的原生插件要在运行时从磁盘加载，无法内联。
  sidecar 通过 `FASTTASK_WEB_ROOT` 从 web 工作区的 `node_modules` 解析 `libfx/node`，
  **不安装第二份 6 MB 原生插件**。
- **仅监听 loopback**，优先 unix socket；TCP 时必须绑 `127.0.0.1`/`::1`/`localhost` 且带端口，
  其余一律拒绝启动（`parseEndpoint`）。
- **每一条路由都要密钥**，`/healthz` 也不例外：一个未认证的探测不该知道这里有没有宿主。
  密钥默认由 Go 启动时生成并经环境变量交给子进程；外部托管时由部署方提供（§8.4）。
- 需要 Node.js 20+；原生插件在 Linux 需 glibc 2.34+，不满足时 libfx 回落到 Node 的 WASM 后端，
  而**某些 Node 版本需要 `--experimental-wasm-jspi`**，属已知坑，`doctor` 要检出，
  `/healthz` 的 `native_addon: false` 与 `detail` 也会说明。
- **诊断走 stderr**，由父进程转发进服务日志：manifest 数量、每次工具调用与其拒绝原因、
  libfx 自己的重试判定（`modelResponseRecovery`）、shim 错误。一个解释不了失败的宿主没法运维（§3.1 规则 3）。

### 8.3 接口（🟢 已实现）

```
POST /run          { run_id, harness_token, thread_id, prompt, checkpoint?, libfx_version?,
                     model, instructions }
                   → { stop_reason, usage?, checkpoint?, libfx_version?, error_message? }
POST /run/{run_id}/cancel                  ← §5.2 的取消通道
GET  /healthz      → { ok, node_version, native_addon: bool, libfx_version, detail }
```

`/run` **不向 Go 回流任何对话内容**——文本与工具调用已由 §4.4 的代理写入。

两条与浏览器宿主的差异，都是「这个进程手里只有 run capability」推出来的：

- **`model` / `instructions` / `thread_id` 随请求下发。** sidecar 不保存凭据、也没有自己的 system prompt，
  浏览器宿主从 `POST /agent/runs` 的 grant 里读到的东西，它只能从这里读到。代理仍然不信这些值：
  每次调用照旧强制自己的 model 与 system（§4.2）。
- **checkpoint 写入不带 `stop_reason`。** 带 `stop_reason` 的 checkpoint 写入就是 run 的终态信号（§5.1），
  浏览器宿主必须发，因为没有别人看见它的 turn 结束；sidecar 的 turn 结束在一次 Go 正阻塞等待的调用里，
  所以**由 Go 依据 `/run` 的返回记录终态**。两边都写就会有两个进程完成同一个 run。

`stop_reason` 用的是 libfx 自己的词表（实测为 `end_turn`、`cancelled`），Go 侧只区分
`cancelled` / `error`，其余一律映射为协议里的 `stop`（`internal/application/harnesslifecycle.go`），
因此对外词表不随 libfx 变化。

工具调用侧同样有一处必要差异：libfx 交给 `HostTool.execute()` 的只有入参与一个 abort signal，
**没有 tool call id**，所以 `POST /agent/runs/{id}/tools/{name}` 的 `tool_call_id` 是可选字段，
缺失时按 `(run, 工具名, 最近一个没有结果的 part)` 关联——宿主对同名工具串行执行，因此这个关联是唯一的（§4.4.1）。
同理 `GET /agent/tools` 接受 run capability：manifest 只是名字与 schema，不含凭据与用户数据，
而它描述的每次调用在执行时还会再鉴权一次（§5.3）。

### 8.4 生命周期（🟢 已实现）

`internal/bootstrap/sidecar.go`，按 [`wiring.md`](wiring.md) §6 的规则，支持两种托管方式，
由 `FASTTASK_SIDECAR_SPAWN` 选择（**默认 `true`**）：

| | `spawn=true`（默认，本节原文） | `spawn=false`（[`tech.md`](tech.md) §21.4 的 systemd 单元） |
|---|---|---|
| 谁启动进程 | Go 的 `OnStart` | `fasttask-sidecar.service`（`PartOf=fasttask.service`） |
| 密钥 | Go 启动时生成，经环境变量交给子进程 | 部署方提供，`FASTTASK_SIDECAR_SECRET` 两侧同值；**缺失则拒绝启动** |
| 崩溃处理 | 自动重启 + 退避；连续失败达阈值标记不可用 | 由 systemd 重启；Go 侧只做**就绪探测**，连续失败达阈值标记不可用，恢复后自动可用 |
| `OnStop` | SIGTERM → 宽限 → SIGKILL，并删除自己的 socket 文件 | 不发信号、不删 socket（不归它管），只停探测 |

两种方式共同的部分：

- 配置项 `sidecar.enabled`（**默认 `off`**，因为它是兜底而非必需）、`sidecar.node_path`、
  `sidecar.socket`、`sidecar.start_timeout`、`sidecar.spawn`、`sidecar.secret`。
- 启用时 `OnStart` 等 `/healthz`；**就绪超时应失败启动**，不带病运行。
- 注册位置早于 `HTTPModule`，使其晚于 HTTP 停止。
- 连续失败达阈值只把「sidecar 模式」标记为不可用，**不影响 WASM 模式**（§14.10）。
- `doctor`：Node 版本、glibc、`/healthz`、原生插件可用性。

### 8.5 两种模式的用户可感知差异

| | WASM 模式 | sidecar 模式 |
|---|---|---|
| 关掉标签页 | run 置 `interrupted`，不恢复 | 运行继续，重连可续流 |
| 依赖 | 浏览器 JSPI | 服务端 Node |

设置界面需说明这一条差异。

## 9. 资源自托管与 PWA

### 9.1 构建

`libfx@0.0.10` 零运行时依赖，Apache-2.0。实测体积：

| 文件 | 原始 | gzip | brotli |
|---|---|---|---|
| `fx-core.wasm` | 2.01 MB | 0.69 MB | 0.53 MB |
| `fx-sdk.js` | 69 KB | 20 KB | 13 KB |

构建时把 `fx-core.wasm` 拷进 `web/public/`，**构建期预压缩**生成 `.br` 与 `.gz`。

包内还有 `fx-term.wasm`（4.51 MB）和四个平台的 `.node` 原生插件（6–7 MB 各）。**浏览器产物不得包含它们**——`fx-term.wasm` 是交互式终端，我们不用；`.node` 只有 sidecar 需要。需有构建产物体积断言防止误打包。

### 9.2 Go 侧

`static()` 必须以 `Content-Type: application/wasm` 提供 `.wasm`，并按 `Accept-Encoding` 优先返回 `.br` / `.gz` 并设 `Content-Encoding`。当前 `static()` 未处理预压缩，需要改。

### 9.3 Service Worker

`web/vite.config.ts:40` 的 `globPatterns` **当前不含 `wasm`**，必须加：

```ts
globPatterns: ['**/*.{js,css,html,woff2,woff,png,svg,ico,wasm}'],
```

`maximumFileSizeToCacheInBytes` 已是 4 MB（`vite.config.ts:41`），2.01 MB 的 wasm 可容纳，无需改。

预缓存后 wasm 为本地资源，符合 `pwa.md` 的离线目标——但注意**离线只能加载宿主，不能跑对话**，模型调用仍需网络。离线态下对话入口应明确置灰。

## 10. 运行能力令牌（harness token）

### 10.1 为什么不叫 `run_token`

[`interface.md`](interface.md) §13 **已经有一个 `run_token`**：独立 Worker 领取 Job 时签发的高熵租约令牌，
用于回调时校验 attempt 与租约栅栏。那是既有契约，不能占用。

本文的令牌一律称 **harness token**，JSON 字段名 `harness_token`。
两者用途、签发方、校验方全不相同，**实现时不得复用同一套签发/校验代码**。

### 10.2 签发

```
POST /api/v1/agent/runs
  认证：用户主 JWT
  Body: { run_id, tools_etag? }
  →  { harness_token, model, instructions, tools_etag, expires_at, heartbeat_interval_s }
```

- `run_id` 来自 `/agent/commands` 响应头 `X-Harness-Run`（§1.2）。
- `harness_token` 不透明、绑定 `{user_id, thread_id, run_id}`，默认有效期 30 分钟，经心跳续期（§10.4）。
- 它同时作为 `createFxAgent({ apiKey })` 的取值——libfx 要求 `apiKey` 为非空字符串，我们用它占位。
  **真实模型凭据只在 Go 侧**，宿主永不可见。
- §4、§5、§6、§7 的所有端点**只接受 harness token，不接受用户主 JWT**；
  §5.2 的取消端点反过来**只接受用户主 JWT**。这样一次运行被攻陷的爆炸半径限定在该 run，
  且被攻陷的宿主无法自行取消（取消是用户的权力，不是宿主的）。
- run 进入任一终态（`succeeded` / `failed` / `cancelled` / `interrupted`）后令牌立即失效。

### 10.3 `tools_etag`：防止工具清单在运行中途变更

`GET /api/v1/agent/tools`（§3.4）的响应带 `ETag`，值为 `ToolRegistry` 内容的稳定哈希
（名称 + 描述 + schema，按 `ToolRegistry.order` 拼接后取摘要）。

- 宿主缓存工具清单及其 etag，签发令牌时把 `tools_etag` 带上。
- 服务端比对：不一致说明中途发生了部署或配置变更，返回 **`409 TOOLS_ETAG_STALE`**，
  宿主重新拉 `/agent/tools` 后重试签发。
- 不带 `tools_etag` 时服务端不校验，只在响应里回当前值（首次调用的情形）。

> 这条防的是真实故障：滚动部署期间工具 schema 变了，而宿主还拿着旧 schema 生成调用，
> 服务端按新 schema 校验失败，表现为"模型突然不会用工具了"。

### 10.4 心跳

```
POST /api/v1/agent/runs/{run_id}/heartbeat
  认证：harness token
  →  { cancel_requested: bool, expires_at, harness_token?: "<续期后的新令牌>" }
```

| 项 | 默认值 |
|---|---|
| 宿主发送间隔 | **10 秒**（由 `heartbeat_interval_s` 下发，宿主不硬编码） |
| 服务端判定失联 | **45 秒**未收到 |
| 失联处置 | run 置 `interrupted`，令牌失效，释放该 run 的待审批长轮询 |

- 心跳只在 run 处于 `running` / `awaiting_approval` 时发送。
- **审批等待期间必须继续发心跳**——否则 15 分钟的等待会被 45 秒的失联判定打断。
- 剩余有效期不足 10 分钟时，服务端在心跳响应里下发续期后的新令牌，宿主原子替换。
- 回收由现有的定期扫描承担（与 `MarkInterruptedRuns` 同一处，见 §11），不新起 goroutine。

## 11. 服务端状态机的变化

`agent-impl.md` §4 的 Run 状态机**取值不变**，变化只有两处：

| 变化点 | 原先 | 现在 |
|---|---|---|
| 谁驱动状态迁移 | `toolLoop`（`agentloop.go:351`） | 模型代理与工具执行面的 handler |
| `awaiting_approval` 后如何继续 | 发 `[DONE]` 结束流，`resumeRun` 起新流 | 同一个 turn 内等待长轮询返回，流不中断 |

`interrupted` 的语义扩大：现在还包括「WASM 宿主所在标签页消失」。

回收有**两条**路径，缺一不可：

| 触发 | 实现 | 覆盖场景 |
|---|---|---|
| 进程启动 | `MarkInterruptedRuns`（`internal/bootstrap/worker.go` 的 `OnStart`），**逻辑不变** | 服务端重启 |
| **心跳超时 45 秒**（§10.4） | 定期扫描，**新增**；复用同一个 scheduler，不新起 goroutine | 标签页关闭 / 刷新 / 休眠 / 断网 |

`awaiting_approval` 的 run 被心跳超时回收时，`Proposal` **保持 `pending`**——
用户下次进入仍能看到该提案，但运行不恢复（`agent-impl.md` §4.2）。

## 12. 现有代码处置

| 文件 / 符号 | 行数 | 处置 |
|---|---|---|
| `agentloop.go` 的 `toolLoop`、`executeToolCall` 的调度部分 | ~150 | **移除**，循环驱动交给 libfx |
| `agentloop.go` 的 `sessionStream`、`beginRun`、`succeed`、`failRun`、`cancelRun`、`recordProposal`、chunk 发射 | ~350 | **保留**，改由 §4/§5 的 handler 调用 |
| `agentloop.go:612 rebuildModelContext` | ~45 | **保留**，从主路径降为 §6.3 的降级路径 |
| `agentloop.go:541 resumeRun` | ~55 | **保留但收窄**，只服务续流，不再服务审批续跑 |
| `internal/agent/chat.go` 的 **SSE 解析部分** | 333 | **保留并复用**，成为 §4.4 权威状态提取的核心（见下方更正 2） |
| `internal/agent/openai.go` | 239 | **保留**，`Transcribe()`（`openai.go:137`）与 `Verify()`（`:59`）长期使用 |
| `internal/agent/protocol/` | 621 | **保留**，assistant-transport 状态机不变 |
| `internal/application/agenttool*.go` | 1114 | **保留**，新增 §3.4 的导出端点 |
| `agenttool.go:82` 的 `agent_tools` 值组注释 | 1 | **删除**，该值组从不存在（见 §3.4） |
| `agentapproval.go`、`agentdailyplan.go` | 510 | **保留**，审批与计划落库路径不变 |
| `web/src/harness/shim.ts` | — | **新增**，§4.2 的 gateway shim（约 80 行；浏览器与 sidecar 共用同一份） |
| `web/src/harness/` 其余 | — | **新增**，§3 |
| Go 的 `/openai` 代理 handler | — | **新增**，§4.3–4.4 |
| `internal/bootstrap/sidecar.go` | — | **新增**，§8.4。**可选模块**，`sidecar.enabled` 默认 `off` |
| `sidecar/` | — | **新增**，§8。**兜底组件，非必需**——仅支持缺 JSPI 的浏览器 |
| `AgentThread` 表与线程列表端点 | — | **新增**，见 [`chat-features.md`](chat-features.md) §2 |
| 附件存储与上传端点 | — | **新增**，见 [`chat-features.md`](chat-features.md) §4 |

> **更正 1（2026-09-22）**：本表原先写 `ChatProvider`「成为 §4.3 适配层的下游」，
> 当时的链路是 `Go → sidecar → shim → NewAPI`，`ChatProvider` 确实不在其中。
>
> **更正 2（2026-09-23，§16 spike 之后）**：shim 下沉到宿主后，
> **Go 变成了直接面对 NewAPI 的 OpenAI 兼容代理**，需要解析 OpenAI SSE 来提取权威状态。
> 而 `chat.go` 的 `ChatProvider` 本来就是按「OpenAI-compatible 的 `tools`/`tool_calls` + 流式」
> 设计的（`agent-impl.md` §5.1.1）——**它的 SSE 解析正是 Go 现在需要的那一半**。
>
> 净结论：
>
> | 部分 | 处置 |
> |---|---|
> | `chat.go` 的 **SSE 解析 / `tool_calls` 聚合** | **保留并复用**到 §4.4 的权威状态提取 |
> | `chat.go` 的**请求构造与调用入口**（`Chat()` 的调用方） | 删除——请求现在由宿主侧 shim 构造 |
> | `openai.go` 的 `Transcribe()`（`:137`）、`Verify()`（`:59`） | **长期保留**，与 agent 循环无关 |
> | `openai.go` 的 `TaskProposal` / `ConversationReply` | 按 `agent-impl.md` §8.1 随旧 job 队列清空后删除 |
>
> **不要把 `chat.go` 整个删掉，也不要整个留着。** 它要拆。

净变化远小于「换掉 agent 循环」这句话的直觉：**被替换的只是循环驱动本身，约 150 行。**

## 13. 分阶段交付

每个 phase 结束时 `go build ./...`、`go test ./...`、`npm run build`、`npm test` 必须全绿。

| Phase | 内容 | 出口判据 |
|---|---|---|
| ~~0~~ | ~~拓扑 spike~~ | ✅ **已完成（2026-09-23）**，结论见 §16 |
| **A** | `web/src/harness/shim.ts`（§4.2）+ Go 的 `/openai` 代理（§4.3，先只做凭据注入与透传） | 最小页面经 shim 跑通流式，含 `reasoning-delta` 与 `tool-call` |
| **B** | §4.4 权威状态写入 + §4.4.1 去重 + harness token（§10） | 最小 Node 脚本驱动 libfx 跑通单轮，服务端转录与现有实现等价 |
| **C** | 工具导出端点（§3.4）+ 工具执行面（§5） | 9 个工具全部可被 libfx 调用；身份字段断言测试通过。**开头先打印 `execute` 第二参数**（§4.4.1） |
| **D** | 宿主 + WASM 模式 + 模式探测（§3.2）+ 实例生命周期（§3.7） | 浏览器内完成含只读工具的多轮对话；UI 代码零改动 |
| **E** | 审批长轮询（§7）+ checkpoint（§6）+ 取消三通道（§5.2）+ 心跳（§10.4） | 提案审批在同一 turn 内继续；跨设备续聊可用 |
| **F** | Node sidecar（§8）+ uber-fx 生命周期 + `doctor` | 禁用 JSPI 后自动降级，端到端与 WASM 模式等价 |
| **G** | 资源自托管、预压缩、SW 预缓存（§9）+ 降级可见性 UI | 构建产物体积断言通过；离线态对话入口正确置灰 |
| **H** | [`chat-features.md`](chat-features.md) 的三项 | 见该文档 §6 的验收 |

**A 之所以能这么小**，是因为 §16 的 spike 已经证明 shim 可行并给出了可直接抄的代码形状。
A 的 Go 侧只是一个带凭据注入的反向代理，权威状态写入推迟到 B——这样 A 的失败面很窄。

> **sidecar（F）现在可以推后甚至跳过。** 它是兜底，不阻塞 A–E 与 G–H 的任何一步。
> 若目标用户浏览器普遍具备 JSPI，可以先不做，等有真实需求再补。

## 14. 测试要求

在 `agent-impl.md` §10 之上追加：

1. **协议往返**：录制的 OpenAI 兼容请求/响应夹具，经 shim + Go 代理往返无损。禁止手写 JSON 夹具。
   夹具用 §16 的 mock 手法录制（`reasoning_content` / `content` / `tool_calls` 三类 delta 齐全），
   并**必须覆盖 §16.3 的交错顺序**（`text-end` 晚于 `tool-input-start`）。
2. **工具清单单源**：断言 `GET /agent/tools` 的输出与 `ToolRegistry.order` 逐项一致，且不含任何 `identityArgNames` 中的字段。
3. **双宿主等价**：同一组输入分别经 WASM 与 sidecar 跑通，断言 `AgentMessagePart` 序列逐字节相同。这是 ADR-0005 §2 的核心主张，必须有测试守住。
4. **请求体 `system` / `tools` 被忽略**：构造携带恶意 `system` 与额外 `tools` 的代理请求，断言服务端使用自己的值。
5. **harness token 边界**：用户主 JWT 打 §4/§5 端点应被拒；令牌在 run 终止后立即失效；跨 run 令牌不可用。
6. **审批不计入墙钟**：模拟 5 分钟审批等待，断言 run 未因 180 秒超时失败。
7. **checkpoint 漂移降级**：伪造 `libfx_version` 不匹配，断言走 §6.3 且记录 `CHECKPOINT_VERSION_SKEW`。
8. **构建产物体积**：断言浏览器产物不含 `fx-term.wasm` 与任何 `.node`。
9. **降级可见**：探测返回 `unavailable` 时，断言 UI 显示模式与原因。
10. **sidecar 缺失不影响 WASM 模式**：不部署 sidecar 时，具备 JSPI 的浏览器**必须照常可用**；
    缺 JSPI 的浏览器才降级为不可用，且理由可见（§3.2）。这是 spike 之后语义的反转，
    旧断言「无 sidecar 则 agent 全挂」**已失效，不要照抄**。
11. **`baseURL` 绝对性**：断言 shim 构造的 `baseURL` 是绝对 URL（§16.3 踩过的坑，回归价值高）。
12. **工具调用不重复写入**：模拟含工具调用的两轮对话，断言 `(run_id, tool_call_id)` 唯一，
    且第二轮请求体里的历史 tool-call **没有**产生新 part（§4.4.1）。
13. **取消三通道**：分别在「正在出字」「等待审批」「空闲等心跳」三种时刻取消，断言 run 均在 5 秒内到 `cancelled`（§5.2）。
14. **心跳失联回收**：停止心跳 45 秒，断言 run 置 `interrupted`、令牌失效、待审批长轮询被释放、`Proposal` 仍为 `pending`（§10.4、§11）。
15. **`tools_etag` 陈旧**：携带过期 etag 签发令牌，断言返回 `409 TOOLS_ETAG_STALE`（§10.3）。
16. **令牌职责隔离**：harness token 打取消端点应被拒；用户主 JWT 打 §4/§5 端点应被拒（§10.2）。
17. **凭据不出 Go 进程**：断言宿主（浏览器与 sidecar）发出的任何请求、以及 sidecar 的日志中，
    都不含 provider 的 `baseURL` 与 `apiKey`；Go 的 `/openai` 响应也不回显它们（§4.5）。
18. **宿主单实例**：连续切换三个线程，断言任一时刻只存在一个 libfx agent 实例（§3.7）。
19. [`chat-features.md`](chat-features.md) §6 的七项验收，其中第 7 项（两种模式行为一致）由本节第 3 项的等价性测试覆盖。

### 14.1 🟢 覆盖对照（2026-09-23 逐条核对）

每一条要求对应的测试。核对方式是从测试名反查，不是从意图推断——**列在这里的名字都在仓库里存在**。

| # | 要求 | 测试 |
|---|---|---|
| 1 | 协议往返（录制夹具 + 交错顺序） | `web/src/harness/shim.test.ts`：`translates one gateway call into an OpenAI-compatible request to our own proxy`、`keeps the interleaved order the spike measured (§16.3)`、`answers an upstream failure with an error response so libfx can retry (§3.6)`；夹具在 `web/src/harness/__fixtures__/`（录制说明见其 README）。**真机补充**：`web/src/harness/integration.test.ts` 加载真实 `libfx/node` 跑完整 turn |
| 2 | 工具清单单源 | `TestAgentToolsEndpointProjectsTheRegistry`、`TestNewToolRegistryRejectsIdentityFields`、`TestReadonlyToolsRegistered` |
| 3 | 双宿主等价 | `TestBothHostsWriteTheSameTranscript`；真机侧见 §16.6 |
| 4 | `system` / `tools` 被忽略 | `TestHarnessProxyReplacesSystemAndTools`、`TestProxyReplacesHostSystemAndTools`、`TestAgentCommandsIgnoresForgedStateSystemTools` |
| 5 | harness token 边界 | `TestHarnessTokenBoundaries`（用户 JWT 打宿主端点 401、**已终止 run 的令牌 401**、跨 run 401、伪造 401） |
| 6 | 审批不计入墙钟 | `TestAgentApprovalWaitDoesNotCountAgainstWallClock`（等待时长 > 整个墙钟预算，run 仍成功） |
| 7 | checkpoint 漂移降级 | `TestCheckpointVersionSkewDegrades` |
| 8 | 构建产物 | `web/src/harness/artifacts.test.ts`：`never imports the libfx entry that drags in fx-term.wasm (§9.1)`、`self-hosts the runtime instead of pointing at a CDN`；CI 另断言 `dist/fx-core.wasm{,.br,.gz}` 存在且产物内无 `fx-term.wasm` / `*.node` |
| 9 | 降级可见 | `web/src/agent/HarnessStatus.test.tsx`：`shows the mode AND the reason when the browser cannot host the runtime (§14.9)`、`says the agent is unavailable rather than leaving the composer to fail on send`、`outranks the mode line when the device is offline (phase G)`；`web/src/harness/backend.test.ts`：`degrades to the sidecar when JSPI is missing, with the reason (§3.2)`；`web/src/harness/host.test.ts`：`publishes a status a view can render, including a degraded context (§3.2, §6.3)` |
| 10 | sidecar 缺失不影响 WASM | `TestSidecarSupervisorGivesUpWithoutTouchingWasm`、`TestSecurityPolicyAllowsWasm`、`TestSidecarModeIsRefusedWithoutASidecar`、`TestWasmRunIsNotExecutedByTheWorker`、`TestExternallyManagedSidecarIsProbedNotOwned` |
| 11 | `baseURL` 绝对性 | `web/src/harness/shim.test.ts`：`builds an absolute proxy URL, or refuses (§14.11)` |
| 12 | 工具调用不重复写入 | `TestHarnessToolCallHistoryInRequestIsNeverPersisted`、`TestToolCallIDCollisionIsRefused`、`TestReasoningAcrossModelCallsKeepsItsOwnParts`（同一 run 内多次调用的 part 不互撞） |
| 13 | 取消三通道 | 出字中：`TestCancellationStopsAModelCallThatIsAlreadyStreaming`；等审批：`TestCancellationReleasesAnApprovalWait`；空闲等心跳：`TestHarnessRunCancellation` + `web/src/harness/host.test.ts`：`cancels the turn when a heartbeat reports a cancellation (§5.2 channel 1)`；无人确认时收尾：`TestHarnessCancellationIsFinishedByTheReaper`；用户入口：`TestAgentExplicitCancelTerminatesRunOverHTTP` |
| 14 | 心跳失联回收 | `TestHarnessHeartbeatLossInterruptsRun`（run 置 interrupted、令牌 401、长轮询被释放、`Proposal` 仍 pending）、`TestSchedulerReapsLostHarnessHosts`、`TestHarnessHeartbeatKeepsARunAlive`、`TestHarnessRunNeverPickedUpIsReaped` |
| 15 | `tools_etag` 陈旧 | `TestRunGrantRejectsStaleToolsETag` |
| 16 | 令牌职责隔离 | `TestHarnessTokenBoundaries`（harness token 打取消端点 401、打 `POST /agent/runs` 401；用户 JWT 打代理/工具/心跳/checkpoint 401） |
| 17 | 凭据不出 Go 进程 | `TestHarnessCredentialsNeverReachTheHost`、`TestHarnessResponsesNeverCarryCredentials`；宿主侧 `web/src/harness/integration.test.ts`（上游收到的 bearer 是 capability 而非 provider key）与 `web/src/harness/sidecar-server.test.ts`：`drives a run and reports how it ended, without completing it`（每个调用只带 capability） |
| 18 | 宿主单实例 | `web/src/harness/host.test.ts`：`holds at most one agent instance, cancelling the run it replaces (§3.7, §14.18)` |
| 19 | [`chat-features.md`](chat-features.md) §6 七项 | ①`TestAgentThreadStateRestoresCompletedConversation`、`TestThreadCatalogueListsAndPages`、`web/src/agent/threads.test.ts` ②`TestThreadArchiveKeepsEverythingAndDeleteRemovesIt`、`TestThreadDeleteCancelsAnInFlightRun`、`TestAttachmentDeleteRemovesRowAndFile` ③`TestReasoningIsPersistedAndCanBeDisabled` + `web/src/agent/Reasoning.test.tsx`：`is folded by default, and says what it is` ④同上 + `renders nothing at all when there is no reasoning to show` ⑤`TestAttachmentUploadSniffsTypeAndStripsMetadata`、`TestAgentAttachmentRoundTripOverHTTP`、`TestAgentMessageCarriesAttachmentReferences`、`web/src/agent/attachments.test.ts` ⑥`TestAgentThreadAccessIsScopedOverHTTP`、`TestAgentAttachmentRoundTripOverHTTP`（跨用户 GET/DELETE 均 404） ⑦`TestBothHostsWriteTheSameTranscript` |

§8 的 sidecar 另有自己的契约测试：`TestSidecarRunPayloadMatchesHostContract`（**直接读 TypeScript 源码里的
`SidecarRunRequest` 字段名**与 Go 发出的 JSON 键逐一对照，任何一侧改名都会失败）、
`TestSidecarClientRefusesARoutableEndpoint` / `TestSidecarClientRefusesNonLoopbackEndpoints`（§8.2 只听 loopback）、
`TestSpawnedSidecarSecretIsGeneratedOnce`（§8.2 密钥）、以及 `web/src/harness/sidecar-server.test.ts`
（`answers nothing at all without the startup secret`、`cancels a run that is still going`、
`only listens where the supervisor can reach it`）。

§3.2 的模式开关（§15 决定暴露给用户）：`web/src/agent/modePreference.test.ts`、
`web/src/agent/HarnessStatus.test.tsx`：`stores the choice, drops the cached probe and re-warms it`、
`web/src/agent/runtime.test.ts`：`hands a stored preference to the probe instead of asking it (§3.2, §15)`。

> **一处方法论**：本表是在实现完成后逐条反查得到的，查出两个真实缺口（§14.13 的「出字中」与「等待审批」
> 两个取消时刻、§14.9 的 UI 侧可见性），已补测试。**测试要求的清单要拿来对，不能拿来读。**

## 15. 待确认

| 事项 | 默认取值 | 何时重新评估 |
|---|---|---|
| libfx 版本升级节奏 | 锁死 `0.0.10`，季度评估 | 出现影响我们的修复时 |
| 审批长轮询超时 | 15 分钟，分段 30 秒 | 观察真实审批时长分布后 |
| harness token 有效期 | 30 分钟，可续 | 长对话被迫续签过于频繁时 |
| WASM 模式下的并发运行数 | 1（UI 层禁止并发） | 用户反馈需要并行多线程对话时 |
| 是否暴露模式切换给普通用户 | 🟢 已暴露（对话页状态栏「运行位置」），默认自动 | 若发现用户乱切导致困惑，则收进管理员设置 |
| 心跳间隔 / 失联阈值 | 10 秒 / 45 秒（§10.4） | 移动端弱网下误判过多时 |
| harness token 续期策略 | 剩余不足 10 分钟时随心跳下发新令牌 | — |

## 16. Phase 0 spike（已完成）

> **执行日期：2026-09-23。结论：gateway shim 放浏览器，sidecar 降为兜底。**
> 本节保留为决策证据，不再是待办。

### 16.1 验证方法

在真实的 `web/` 工程里（vite 8.2.1 + 项目自己的 `vite.config.ts`，非另起最小工程）：

1. `npm i --save-exact @ai-sdk/openai-compatible@3.0.53`
2. 写一个 spike 模块调用 `createOpenAICompatible(...).languageModel(m).doStream(opts)`，
   并从 `main.tsx` 引入以确保**不被 tree-shake**（否则测试是空的）
3. `npm run build` → 产物用 `vite preview` 提供，**测的是压缩后的生产 bundle**
4. 模型端用一个 OpenAI 兼容 SSE mock（发 `reasoning_content` / `content` / `tool_calls`），
   经 vite proxy 转发并**在代理层注入 Authorization**，模拟 Go 注入凭据

### 16.2 结果

| 检查项 | 结果 |
|---|---|
| 生产构建 | ✅ 通过，无 `undici` / `node:dns` 解析错误 |
| 体积增量 | `index.js` 843.04 → **1009.70 kB**（+166.66），gzip 236.79 → **284.12 kB**（+47.33） |
| 浏览器运行（生产 bundle） | ✅ 流式正常，控制台无错误 |
| `reasoning_content` 映射 | ✅ `reasoning-start` / `reasoning-delta` / `reasoning-end` 齐全 |
| `tool_calls` 映射 | ✅ `tool-call` 携带 `toolCallId` / `toolName` / `input` |
| 代理层注入凭据 | ✅ 浏览器发占位符，代理覆盖后 mock 收到 `auth=present` |

关键在于产物里的这段运行时守卫——**上游有意做的浏览器兼容，不是巧合**：

```js
function isNode(){
  const p = globalThis.process
  return p?.release?.name === 'node' && p.versions?.bun == null
      && p.versions?.deno == null && p.title !== 'workerd'
      && globalThis.EdgeRuntime == null
}
async function pickFetch(){ return isNode() ? lazyNodeFetch() : globalThis.fetch }
```

浏览器里直接返回 `globalThis.fetch`，`undici` / `node:dns` 分支永不进入。

### 16.3 spike 带出的三条实现约束

1. **`baseURL` 必须是绝对 URL**，否则 `TypeError: Failed to construct 'URL': Invalid URL`。写进 §4.2。
2. **分片是交错的，不是严格嵌套**：实测 `text-end` 出现在 `tool-input-start` 之后。写进 §4.4.1。
3. **`tool-call` 携带 `toolCallId`**，代理侧按 id 关联的主方案成立。写进 §4.4.1。

### 16.4 对文档的影响

| 文档 | 改动 |
|---|---|
| 本文 §2、§4、§8、§12、§13 | 已重写 |
| [ADR-0005](adr/0005-libfx-agent-harness.md) §2、§3.5、§5.1、§6 | 已修订 |
| [`tech.md`](tech.md) §2.1、§21.4 | Node 从必需降为可选 |
| [`arch.md`](arch.md) §14 | 部署恢复为单 Go 二进制（sidecar 可选） |
| [`interface.md`](interface.md) §20 | 模型端点从 `/model`（V4）改为 `/openai`（OpenAI 兼容） |

### 16.5 环境已清理

spike 的依赖、`spike-gateway.ts`、`main.tsx` 与 `vite.config.ts` 改动**已全部回滚**，
`web/` 工作区干净，构建回到 843.04 kB 基线。
`node_modules` 中仍残留 spike 装过的包，`npm ci` 即可清除。

### 16.6 🟢 真机端到端验证（2026-09-23，实现完成后）

§16.1–§16.5 验证的是 **shim 能否在产物里跑通一次补全**。实现落地后又做了一轮**全链路真机验证**：
真实 Go 服务 + 真实 Node sidecar（原生插件）+ 真实模型（`qwen3.8-max`，经本地网关），
并在 Go 与上游之间插一个记录代理，把**每一轮模型请求的 messages 原样落盘**。

跑通的两条链路：

| 场景 | 结果 |
|---|---|
| 纯问答（`1 加 1 等于几`） | run `succeeded`，`model_calls=1`，reasoning part + text part 落库 |
| 工具调用（`调用 list_goals 并报标题`） | run `succeeded`，`model_calls=2`，reasoning → tool-call（含 result）→ reasoning → text 四个 part 按 idx 落库 |

**这一轮查出三个只靠 mock 永远查不出的缺陷**（均已修，见 §4.2、§4.4.1、§8.3）：

1. **shim 读不出请求体。** `init.body` 在 Node 里是字节不是字符串，`JSON.parse(String(...))` 抛错 →
   shim 自己回 502 → libfx 指数退避重试到 turn 预算耗尽。**外部表现是"模型不回答"，日志里什么都没有。**
2. **shim 把模型目录请求也当补全处理并失败。** libfx 补全前先 `GET /coding-agent/v1/models`，
   目录失败对 turn 是致命的：它一直等，不报错、不超时。
3. **reasoning part 的主键在一个 run 内跨模型调用冲突。** id 只按调用内序号编号，第二次调用撞第一次的键 →
   插入失败 → 转写副本中途断流 → 宿主看到被截断的响应并重试 → 8 次调用烧光预算，
   run 最后以"已达到工具调用轮次上限"**成功**收尾，而模型其实第二轮就答对了。

三条的共同点是**失败被吞掉**：宿主只看到"没有输出"，服务端只看到"预算用尽"。
因此这一轮同时补了两处可观测性（§8.2 的诊断、§4.4 代理提前结束时的一行日志），
并把 `web/src/harness/integration.test.ts`（加载真实 `libfx/node`）与
`TestReasoningAcrossModelCallsKeepsItsOwnParts`（去掉修复即失败）留作回归。

> **教训值得写进文档**：`createFxAgent` 被 mock 掉的测试只能证明"我们的代码自洽"，
> 证明不了"我们的代码和 libfx 说得上话"。任何适配层都必须有一条**加载真实依赖**的测试。
