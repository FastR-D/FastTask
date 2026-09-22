# FastTask 前端架构

> 文档状态：初版设计  
> 决策记录：[ADR-0002](adr/0002-assistant-transport.md)、[ADR-0004](adr/0004-frontend-styling-boundary.md)  
> 相关：[`doc/agent-impl.md`](agent-impl.md)、[`doc/pwa.md`](pwa.md)

## 1. 现状

mdui 完全重写已落地（提交 `9456e5f`）。当前事实：

| 项 | 现状 |
|---|---|
| 规模 | 1810 行，`App.tsx` / `Admin.tsx` / `Lens.tsx` + `api.ts` + `mdui.ts` + `mdui-react.ts` |
| 技术栈 | React 19.2.8、Vite 8.2.1、TypeScript 7.0.2、Vitest 4.1.10、mdui 2.1.5 |
| 样式 | 纯 CSS 三份，**全部颜色走 `rgb(var(--mdui-color-*))`，零硬编码色值** |
| 主题 | `<html class="mdui-theme-auto">`，跟随 `prefers-color-scheme`，无 JS 主题 API |
| 组件注册 | `src/mdui.ts` 逐个 import，**只被 `main.tsx` 引入**，测试中自定义元素保持惰性 |
| 自定义元素适配 | `src/mdui-react.ts` 的 `useMduiEvent`（ref + addEventListener）与 `fieldValue` |
| JSX 类型 | `vite-env.d.ts` 引用 `mdui/jsx.en.d.ts` |
| 响应式 | 840px 断点：桌面 `mdui-navigation-rail`，移动端抽屉 + 顶栏 + 底部导航；已用 `env(safe-area-inset-*)` |
| 构建与测试 | `npm run build` 与 `npm run test`（13 项）均通过 |

**注意**：`src/mdui.ts` 的导入在本地 `node_modules` 缺失 mdui 时会让 `tsc -b` 报 31 个 `TS2882`，而 `npm run test` 仍然全绿——因为测试不加载 `main.tsx`。**测试通过不代表可构建**，CI 必须同时跑 `npm run build`。

## 2. 交界契约（两条工作流的唯一硬约束）

两条工作流并行：

- **工作流 A**：mdui 2.1.5 完全重写前端（**已完成**，提交 `9456e5f`）。
- **工作流 B**：接入 `@assistant-ui/react` 0.15.21 与 assistant-transport 运行时（本期）。

[ADR-0004](adr/0004-frontend-styling-boundary.md) 已依据 A 的真实代码定案为「只用 mdui 令牌」。以下四条边界继续有效，约束工作流 B 的改动范围：

1. **职责边界**：A 负责 app shell、导航、路由、主题和全部非对话页面；**A 不实现对话页内部**，只提供一个挂载点。B 只在挂载点内部工作。
2. **文件边界**：B 的代码全部落在 `web/src/agent/` 目录下。A 不修改该目录，B 不修改该目录之外的文件（`api.ts` 的认证部分除外，见 §5）。
3. **接口边界**：两者只通过两样东西交界——一个 React Provider，以及 mdui 暴露的 CSS 自定义属性。
4. **禁止事项**：对话区不得依赖 mdui 组件的内部 DOM 结构或私有类名，只读 token。A 不得假设对话区的内部实现。

挂载点的形状约定为：

```tsx
// 由工作流 A 在布局中放置，内部实现由工作流 B 提供
import { AgentChat } from './agent'

<AgentChat goalId={activeGoalId} />
```

`AgentChat` 自带 `AssistantRuntimeProvider`，不要求 A 在外层包裹任何东西。

现有的 `Dialogue` 组件（`App.tsx:234`）就是待替换的挂载点，当前实现是轮询式的简单聊天界面。

## 3. 目录约定

```text
web/src/
  agent/              # 工作流 B 独占
    index.tsx         # 导出 AgentChat
    runtime.ts        # assistant-transport 接线
    converter.ts      # 服务端状态 -> assistant-ui 消息
    state.ts          # 服务端状态的类型定义
    tools/            # 提案审批卡片等工具 UI
  ...                 # 其余由工作流 A 组织
```

样式写进 `web/src/agent.css`（与 `lens.css` / `admin.css` 的既有惯例一致），由 `agent/index.tsx` 引入。按 [ADR-0004](adr/0004-frontend-styling-boundary.md) §4：不引入 Tailwind，颜色一律 `rgb(var(--mdui-color-*))`，字号一律 `var(--mdui-typescale-*)`，圆角一律 `var(--mdui-shape-corner-*)`，**提交中不得出现字面色值**。

`web/src/agent/state.ts` 里的类型必须与 [`agent-impl.md`](agent-impl.md) §2.7 的服务端状态结构一一对应。**该结构是前后端契约，改动需要同时改两边并更新那份文档。**

## 4. 运行时接线

```ts
const runtime = useAssistantTransportRuntime({
  protocol: "assistant-transport",     // 必须显式设置，默认值是 "data-stream"
  api:            "/api/v1/agent/commands",
  resumeApi:      "/api/v1/agent/resume",
  resumeStateApi: "/api/v1/agent/resume-state",
  initialState:   { messages: [], isRunning: false, fasttask: {} },
  converter:      convertState,
  headers:        authHeaders,          // 见 §5
})
```

四条要点：

- **`protocol` 漏设会静默走错协议**，表现为流看起来正常但消息不渲染。这是本接线最容易出的错。
- `converter` 把服务端状态映射为 `{ messages, state, isRunning }`。消息列表来自服务端状态，不是来自流 chunk，原因见 [`agent-impl.md`](agent-impl.md) §2.6。
- `fasttask` 命名空间里的业务状态（待审批提案、今日计划预览）由 `converter` 取出单独渲染。**不要为这些数据另开轮询**——它们和对话共用同一条流。
- 提案审批卡片用 `makeAssistantToolUI` 按工具名注册。

**审批有一个必须避开的陷阱**：不要使用 `respondToApproval`、`hitl`、`humanTool`，也不要在 `converter` 里填充 `ToolCallMessagePart.approval`。`useAssistantTransportRuntime` 没有接 `onRespondToToolApproval`，走那条路用户点了没有任何反应。审批决定一律用 `addToolResult` 回传，结果体形如 `{"decision":"approve"}` / `{"decision":"reject","reason":"..."}`。理由见 [ADR-0002](adr/0002-assistant-transport.md) §3.1，服务端行为见 [`agent-impl.md`](agent-impl.md) §7。

### 4.1 converter 的职责

`converter(state, connectionMetadata)` 必须返回 `{ messages, state, isRunning }`：

- `messages`：把服务端 `state.messages` 映射为 assistant-ui 的 `ThreadMessage`。服务端的 `status` 已按 assistant-ui 的 `MessageStatus` 形状设计，可直接透传。
- `state`：把 `state.fasttask` 原样传出，供页面其他部分读取。
- `isRunning`：取服务端的 `state.isRunning`，并与 `connectionMetadata.isSending` 取或——命令还在路上时界面就该显示忙碌。

**converter 必须是纯函数且能处理不完整状态**：流式过程中它会被反复调用，`parts` 可能为空、`text` 可能是半句话。

### 4.2 语音输入

保持现有交互：录音 → 上传 → 轮询转写作业 → **把结果填进 composer 由用户确认后发送**，不直接触发运行（`agent.md` §3）。

实现上不要用 assistant-ui 的 `DictationAdapter`——它面向的是浏览器内实时听写，而 FastTask 的转写走服务端作业，两者模型不同。用普通按钮加 `composer.setText()` 即可。

### 4.3 重新挂载时恢复对话

assistant-ui 的会话状态只存在于运行时内存里。对话视图被卸载（切到其他页签、刷新、PWA 冷启动）再挂载时，用户看到什么完全由 `initialState` 决定：从空状态开始，就等于把刚才那段对话抹掉了。

所以 `AgentChat` 在创建运行时**之前**先读一次 `GET /api/v1/agent/thread-state`（[`agent-impl.md`](agent-impl.md) §2.1），用返回的 §2.7 结构当 `initialState`；`204` 表示该用户还没有会话，从空对话开始。这次读取走 `api.ts` 的 `request`，因此自动享有 401 刷新重试（与 §5 里三个传输端点的手工 `authHeaders` 不同）。读取失败时退回空对话并提示，**不要停在加载态**——恢复不了历史不该让对话变成不可用。

恢复出来的状态若带 `isRunning`，说明服务端那次运行还在跑（客户端断开不会终止运行，[`agent-impl.md`](agent-impl.md) §8），挂载后调用一次 `runtime.thread.resumeRun({ parentId: null })` 接回流。该方法在类型上返回 `void`，传输失败经由运行时的 `onError` 上报，这里只需要捕获同步抛出。

**顺带修掉 `App.tsx:240` 的 `audio/webm` 硬编码**：用 `MediaRecorder.isTypeSupported` 协商容器格式，并把实际 MIME 与扩展名一起传给后端，否则 iOS 上必坏（见 [`pwa.md`](pwa.md) §2）。

## 5. 认证

`headers` 选项接受一个异步函数，这是唯一能在 assistant-ui 发起请求前刷新 token 的位置：

```ts
async function authHeaders() {
  await ensureFreshAccessToken()        // 复用 api.ts 的刷新逻辑
  return { Authorization: `Bearer ${token.get()}` }
}
```

要求：

- 刷新逻辑必须与 `api.ts` 现有的 `refreshInFlight` 单飞机制共用，避免并发刷新导致 refresh token 轮换冲突。
- 三个传输端点的 401 不能依赖 `api.ts:21` 的重试逻辑（那是 `execute` 内部的，assistant-ui 不走这条路径），必须在 `authHeaders` 里前置保证 token 有效。§4.3 的恢复读走 `request`，本身就有这条重试。

**token 存储位置需要从 `sessionStorage` 改为持久存储**（`api.ts:10-12`、`api.ts:35-52`），否则 PWA 每次冷启动都要重新登录。这是 §2 文件边界的唯一例外：两条工作流都会碰 `api.ts`，需要提前协调。具体方案见 [`doc/pwa.md`](pwa.md) §4。

## 6. mdui 集成的既定解法

以下是重写中已经确立的做法，工作流 B 直接沿用，不要另造一套：

| 场景 | 解法 |
|---|---|
| 标准名称事件（`click`、`change`） | 直接用 JSX 的 `onClick` / `onChange`，React 19 的自定义元素支持已覆盖 |
| 非标准事件（drawer/snackbar 的 `close` 等） | `useMduiEvent(ref, 'close', handler)` |
| 读取 `mdui-text-field` 的值 | `fieldValue(event)` |
| 用到新的 mdui 组件 | 加进 `src/mdui.ts`——唯一注册点 |
| 暗色模式 | 无需处理。只要不硬编码颜色即自动跟随 `mdui-theme-auto` |

测试相关的一条要点：`src/mdui.ts` 不在测试中加载，因此 mdui 元素**在 DOM 里但没有行为**。既有测试用 `closest('mdui-button')`、`toBeEnabled()` 做断言并能通过（属性是反射的），但不要断言 mdui 组件的交互行为。

### 对话页需要改的一处

`.chat-shell` 当前是 `height: calc(100dvh - 20rem)`（`styles.css:233`，移动端 `styles.css:250`）。这个魔数依赖页头的确切高度，而 assistant-ui 的 Thread viewport 需要由 flex/grid 约束、自行管理滚动的容器。接入时改成弹性约束（`minmax(0,1fr)`），**不要继续加魔数**。

## 7. 验收

- `web/src/agent/` 之外的文件在工作流 B 的提交中零改动（`api.ts` 除外）。
- 对话区在浅色与深色主题下均正确渲染，且颜色全部来自 token 而非硬编码。
- 对话区在 400px 宽度下可用（与 `pwa.md` 的响应式要求一致）。
- 断网后重连，进行中的运行能续上，不产生重复消息。
- 前端不复制任何服务端领域规则：最多三个核心项、revision 校验、完成语义一律由后端强制（`arch.md` §6.2）。
