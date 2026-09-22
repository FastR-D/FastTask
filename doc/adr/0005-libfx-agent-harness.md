# ADR-0005：采用 libfx 作为 Agent Harness，双宿主（WASM 默认 / Node sidecar 兼容）

> 状态：已接受
> 日期：2026-09-22
> 修订 2：2026-09-23。Phase 0 spike 完成（[`harness.md`](../harness.md) §16），
> 实测 gateway shim 可在浏览器生产产物中运行。据此 **§3.5 的「sidecar 必需」结论被推翻**，
> sidecar 降为兜底，Go 侧退化为 OpenAI 兼容代理。§2、§3.5、§5.1、§6 相应改写。
> 修订 1：2026-09-22（**同日、提交前修订**）。发现 `@ai-sdk/gateway` 为 Apache-2.0 且随包发布完整 `src/`，
> 协议远比预估简单，且 `@ai-sdk/openai-compatible` 已是现成的转换实现。据此改写 §3.5 与 §5.1，并新增 §6 的两条否决项。
> 本 ADR 尚未进入版本历史，因此直接修订正文而非另立 ADR——`adr/README.md` §1 的「不改写已接受正文」适用于已提交的记录。
> 取代：[ADR-0003](0003-in-process-agent-loop.md)
> 实现文档：[`doc/harness.md`](../harness.md)
> 相关：[ADR-0002](0002-assistant-transport.md)（不受影响，理由见 §4）、[ADR-0001](0001-fx-composition-root.md)（新增 `SidecarModule`）

## 0. 术语消歧（必读）

本仓库中「fx」有两个互不相关的含义，**混淆会导致严重的实现错误**：

| 写法 | 指代 | 位置 |
|---|---|---|
| **uber-fx** | `go.uber.org/fx`，依赖注入与生命周期容器 | `go.mod:13`、`internal/bootstrap/` |
| **libfx** | `vercel-labs/fx` 的 JS 嵌入 SDK，npm 包 `libfx` | 本 ADR 的主题，`web/` 与 sidecar |

本文档及 `harness.md` 中，**uber-fx 一律写作 uber-fx，libfx 一律写作 libfx**，不使用裸「fx」。ADR-0001 的标题沿用历史写法，指的是 uber-fx。

## 1. 背景

[ADR-0003](0003-in-process-agent-loop.md) 决定在 Go 进程内自建 agent 循环，已经落地：`agentloop.go` 714 行，配套测试 541 行，九个工具、审批流、续流、中断回收全部可用。

但这个循环是自研的，其成熟度上限就是我们自己的投入：轮数控制、上下文裁剪、重试语义、工具并发、取消传播、token 预算，每一项都要自己迭代。ADR-0003 §4 当时就把「能力上限低于成熟 harness」列为已知代价。

`vercel-labs/fx` 的 agent 内核作为 npm 包 `libfx` 发布，可嵌入 JS 宿主，并且有两条关键性质使它适配 FastTask：

- **`tools` 选项之外不启用任何工具。** 文档原文：「No CLI tools are enabled automatically.」浏览器 WASM 构建更进一步，运行时层面就不具备文件系统、shell 和 keychain 能力。ADR-0003 §2 的「禁止级工具结构性不存在」这条性质被完整保留。
- **`instructions` 是唯一系统上下文。** 文档原文：「libfx adds no hidden base prompt」。现有系统提示词原样搬运，模型行为不会被 harness 的隐藏 prompt 污染。

同时，产品的部署场景有一条硬约束：**目标用户无法便捷访问国际互联网**。任何在运行时回源到 Vercel 网络的设计都不可接受。

## 2. 决策

**用 libfx 替换自建的 agent 循环驱动，采用双宿主拓扑：**

| 宿主 | 后端 | 角色 | 触发条件 |
|---|---|---|---|
| **浏览器 WASM** | `fx-core.wasm` + JSPI | **默认** | `getBackendInfo()` 探测到 JSPI 可用 |
| **Node sidecar** | `libfx` N-API 原生插件 | **兼容** | JSPI 不可用，或管理员强制 |

**sidecar 只在浏览器缺 JSPI 时需要**（2026-09-23 spike 后的结论）。
gateway shim 跑在宿主进程内，两种宿主共用同一份；Go 侧只有一个 OpenAI 兼容代理端点。

两种宿主共享同一套服务端契约，这是本决策成立的前提：

**宿主只驱动循环，不持有任何权威状态。** 模型调用与工具执行一律经由 Go 后端的两个 HTTP 面，因此：

- 服务端仍然看得到完整的模型输出与全部工具调用，`AgentRun` / `AgentMessage` / `AgentMessagePart` 的写入点不变；
- 两种宿主产生**逐字节等价**的服务端状态；
- 前端 UI 代码在两种模式下**完全相同**——它始终从服务端状态渲染，从不消费 libfx 的事件。

所有 libfx 资源自托管，运行时零 Vercel 访问（§3.3）。

## 3. 理由

### 3.1 五条不变量全部保留

`agent.md` §4 的五条不变量不依赖「循环在哪里跑」，而依赖「写操作必须经提案与服务端重校验」。把循环驱动移出进程后：

| 不变量 | 为何仍然成立 |
|---|---|
| 1. 不直接改业务数据 | 提案工具的 `execute` 打的是 Go 的工具执行端点，`ApplyProposal` 仍在服务端事务内重校验 |
| 2. 核心项最多三个由程序强制 | `CreateDailyPlan` 未被触碰 |
| 3. 满足 ≠ 任务完成 | 注册表里不存在标记完成的工具，与宿主无关 |
| 4. 模型输出不可信 | 工具参数在 Go 侧做 JSON Schema 与领域校验，宿主传来的参数一律当不可信输入 |
| 5. 跨用户数据不可达 | `ToolContext.UserID` 来自 Go 的认证上下文，宿主无法影响 |

关键推论：**宿主即使被完全攻陷，也不能越过任何一条不变量**，因为它唯一能做的是向 Go 发起带用户凭据的工具调用，而那正是用户本人已被授权的操作。

### 3.2 服务端权威状态由代理流量重建，不接受客户端提交的转录

宿主在浏览器里时，天真的做法是让浏览器把生成好的对话提交上来——那等于让不可信客户端写权威状态。

本决策不这么做：**模型代理端点本身就是持久化点**。Go 在把 provider 的流转发给宿主的同时，把文本增量与工具调用写进 `agent_run_chunks` 与运行 hub。服务端从不信任、也从不接收客户端提交的转录。

这条同时保证了 ADR-0002 的「服务端持有权威 thread state」在 WASM 模式下不被破坏。

### 3.3 运行时零 Vercel 访问

| 流量 | 处置 |
|---|---|
| `fx-core.wasm`、`fx-sdk.js` | 构建期 `npm install` 取得，由 Go `static()` 自托管；`createFxAgent({ wasm })` 显式指定本地 URL |
| 模型请求（`ai-gateway.vercel.sh/v4/ai/language-model`） | `fetch` 选项覆盖。文档明确：自定义 `fetch` 捕获 agent 的**全部**网络出口 |
| 模型列表（`coding-agent/v1/models`） | 同上；且我们不调用 `listModels()`，模型由管理员配置 |

`libfx` 零运行时依赖，`fx-core.wasm` 2.01 MB（brotli 0.53 MB），可被 Service Worker 预缓存，首屏后为本地资源。

### 3.4 双宿主的增量成本低

两种宿主的 host 契约相同——libfx 文档：「The same descriptors, schemas, cancellation, results, and events are used by N-API and WebAssembly.」工具描述符、instructions、checkpoint 处理、审批长轮询全部共用一份 TypeScript 实现，差异只有 `createFxAgent` 的 `backend` 与资源定位。

因此「两套实现」实际是一套宿主逻辑 + 两个薄入口，不是 ADR-0003 §5 否决的那种「port 双实现工作量翻倍」。

### 3.5 为什么默认 WASM

> **本节在 2026-09-23 的 spike 后第二次改写。**
> 修订 1 曾因「适配层必须跑在 JS 运行时里」把 sidecar 定为必需；
> spike 证明那个运行时**可以是浏览器**，该结论随之作废。

- **循环驱动跑在用户设备上**：服务端只做代理与工具执行，不承担 agent 循环的内存与调度开销，
  单机可承载的并发对话数显著上升。
- **模型流量无论如何都要过 Go 代理**，WASM 模式不增加任何额外网络跳数。
- **部署保持单 Go 二进制**：sidecar 只在浏览器缺 JSPI 时才需要，多数部署可以不装 Node。
- **凭据不跨进程**：shim 在浏览器侧构造请求，真实凭据只在 Go 进程内注入，
  相比修订 1 的「明文密钥传给 sidecar」是一项净改善。

## 4. ADR-0002 不受影响

assistant-transport 协议、`useAssistantTransportRuntime`、`update-state` 增量、`add-tool-result` 审批路径、续流语义全部保留。原因在 §2 已说明：宿主不参与 UI 渲染，前端仍然只从服务端状态渲染。

[ADR-0002](0002-assistant-transport.md) §3.1 的限制（不得使用 `respondToApproval` / `hitl` / `humanTool`）继续有效。

## 5. 代价

### 5.1 需要一层 gateway shim——但有现成模块，且跑在宿主侧

libfx 打的是 `https://ai-gateway.vercel.sh/v4/ai/language-model`，带
`ai-gateway-protocol-version: 0.0.1`。该协议的客户端 `@ai-sdk/gateway` 是 **Apache-2.0 且随包发布
完整 TypeScript `src/`**，读 `src/gateway-language-model.ts` 可知协议极简：
**请求体就是 `LanguageModelV4CallOptions` 原样 JSON，响应是 `LanguageModelV4StreamPart` 的 SSE**，
没有自定义信封。

我们用 `fetch` 覆盖把它截到同进程内的 shim，由 `@ai-sdk/openai-compatible`（同样 Apache-2.0）
翻译成 OpenAI 兼容格式。该包已实测包含 `reasoning_content` 与 `image_url` 支持。

**2026-09-23 的 spike 证明这层可以跑在浏览器里**（[`harness.md`](../harness.md) §16）：
生产产物打包通过、流式正常、`reasoning-*` 与 `tool-call` 分片齐全，
产物中 `isNode()` 守卫使 `undici` / `node:dns` 分支永不进入。

代价因此降到很低：

| | 原估计（修订 1 前） | 实际 |
|---|---|---|
| Go 侧 | 手搓 ~1000 行 LanguageModelV4 服务端 | 一个普通 OpenAI 兼容代理 |
| 宿主侧 | — | ~80 行 shim |
| 新增部署单元 | Node（必需） | 无（sidecar 仅兜底） |
| 前端体积 | — | +166.66 kB / **gzip +47.33 kB** |

### 5.2 WASM 模式下运行绑定标签页

浏览器宿主随页面存活。标签页关闭、刷新或休眠会中断运行，服务端将其标记为 `interrupted`。与现有 `MarkInterruptedRuns` 的处置一致，v1 不恢复中断运行（`agent-impl.md` §4.2）。

sidecar 模式没有这个问题。这是一条**真实的能力差异**，必须在设置界面向用户说明，并允许手动切换到兼容模式。

### 5.3 JSPI 的浏览器覆盖率在 2026 年仍不完整

Chrome 137+ 已稳定；Safari 27 刚支持，iOS 存量设备覆盖率低；Firefox 139 仍在 flag 后。国内大量使用的微信内置浏览器、UC、QQ 浏览器等旧版 Chromium 内核不可假设具备 JSPI。

这正是 sidecar 必须同时交付、而不是「以后再说」的原因。**探测失败必须自动降级，且降级必须可见**（状态栏提示当前模式），不得静默。

### 5.4 libfx 处于 0.0.x

`libfx@0.0.10`。agent 内核本身是 Vercel 内部用出来的，但**嵌入 SDK 是新的**，API 会变。缓解：宿主逻辑收敛在 `web/src/harness/` 单一目录，libfx 类型不得泄漏进 `web/src/agent/` 或任何 UI 组件。

### 5.5 checkpoint 存在版本漂移

`agent.checkpoint()` 返回不透明的带版本字节，libfx 升级后旧 checkpoint 可能无法恢复。降级路径见 `harness.md` §6.3：回退到从服务端权威消息重建摘要，记 `CHECKPOINT_VERSION_SKEW`。这是**有损降级**，必须记录并可观测。

## 6. 被否决的方案

| 方案 | 否决理由 |
|---|---|
| 保留自建 Go 循环，不引入 libfx | 是 ADR-0003 的现状。循环质量的天花板等于自研投入，而这不是 FastTask 的差异化所在 |
| 只做 sidecar，不做 WASM | 放弃循环驱动下沉到设备的伸缩收益，且强制每个部署新增 Node 运行时 |
| **在 Go 里实现 gateway 协议的服务端** | 2026-09-23 spike 后已无必要：shim 跑在宿主侧，Go 只见 OpenAI 格式。手搓 V4 服务端等于凭空引入一层对外部规范的追随成本 |
| 只做 WASM，不做 sidecar | §5.3 的覆盖率问题会让相当比例的移动端用户完全用不了 agent |
| Go 用 wazero 跑 `fx-core.wasm` | 不可行。JSPI 是 JS 引擎特性，不是 wasm 标准导入；wazero 无法提供 |
| Go 起 `fx acp` 子进程，走 ACP | ACP 模式使用与交互式 fx **完全相同的工具集与权限**——`shell`、`write_file`、`install_skill` 全部在列，需逐条用 permission 规则关闭。相比 libfx 的「默认什么都没有，由 host 提供」，安全姿态是「默认不安全，靠配置补」，差一个量级 |
| 让浏览器宿主直接提交生成好的对话转录 | 等于让不可信客户端写权威状态，违反 `arch.md` §12 与 ADR-0002 的状态模型 |
| 浏览器直连 provider，不过 Go 代理 | 凭据落入客户端；且服务端失去权威状态的写入点，§3.2 的整个设计前提消失 |
| **在 Go 里手搓 LanguageModelV4 ↔ OpenAI 转换** | 已有 Apache-2.0 的 `@ai-sdk/openai-compatible` 做同一件事，且由定义该规范的同一批人维护，不会与规范漂移。手搓等于自造轮子并承担长期追随成本 |
| 让 sidecar 只在 JSPI 缺失时才启动 | gateway 适配层两种模式都要用，sidecar 必须常驻。「按需启动」会让 WASM 模式在首次对话时多一次冷启动，且故障面更难推理 |
| 用 Portkey / LiteLLM / Bifrost 之类现成网关承担适配层 | 它们暴露的是 OpenAI 兼容协议，不是 gateway 的 LanguageModelV4 协议。放在链路里只能替代 NewAPI 的位置，替代不了适配层 |
| 用 `gatewayChatUrl` 选项替代 `fetch` 覆盖 | 该选项未出现在 `AgentOptions` 公开文档中，被描述为仅供对 localhost 测试使用。依赖未公开选项在 0.0.x 包上风险过高 |
