# FastTask 前端架构

> 文档状态：初版设计（含一项待定）  
> 决策记录：[ADR-0002](adr/0002-assistant-transport.md)、[ADR-0004](adr/0004-frontend-styling-boundary.md)（待定）  
> 相关：[`doc/agent-impl.md`](agent-impl.md)、[`doc/pwa.md`](pwa.md)

## 1. 现状

| 项 | 现状 |
|---|---|
| 规模 | 1227 行，4 个组件文件（`App.tsx` / `Admin.tsx` / `Lens.tsx` + `api.ts`） |
| 技术栈 | React 19.2.8、Vite 8.2.1、TypeScript 7.0.2、Vitest 4.1.10 |
| 样式 | 手写 CSS 三份（`styles.css` / `admin.css` / `lens.css`） |
| 状态管理 | 无，`useState` + 手写 fetch |
| 代码风格 | 单行密度极高，部分函数一行数百字符 |
| 响应式 | `.mobile-nav` + 媒体查询，是本次 mdui 重写要解决的问题 |

代码量小是有利条件：重写的成本远低于渐进改造。

## 2. 交界契约（两条工作流的唯一硬约束）

两条工作流并行：

- **工作流 A**：mdui 2.1.5 完全重写前端（在另一处进行中）。
- **工作流 B**：接入 `@assistant-ui/react` 0.15.21 与 assistant-transport 运行时（本期）。

[ADR-0004](adr/0004-frontend-styling-boundary.md) 把样式方案挂起，等 A 落地后按真实代码决定。**在此期间，以下四条是两边都必须遵守的硬约束**，目的是让 A 和 B 可以独立推进且合并时不冲突：

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

**这四条与最终选哪个样式方案无关。** 无论 ADR-0004 定案为哪种，都不会要求返工 shell 或后端。

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
- 提案审批卡片用 `makeAssistantToolUI` 实现，按工具名注册。

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
- 三个 agent 端点的 401 不能依赖 `api.ts:21` 的重试逻辑（那是 `execute` 内部的，assistant-ui 不走这条路径），必须在 `authHeaders` 里前置保证 token 有效。

**token 存储位置需要从 `sessionStorage` 改为持久存储**（`api.ts:10-12`、`api.ts:35-52`），否则 PWA 每次冷启动都要重新登录。这是 §2 文件边界的唯一例外：两条工作流都会碰 `api.ts`，需要提前协调。具体方案见 [`doc/pwa.md`](pwa.md) §4。

## 6. mdui 与 React 19 的集成注意事项

记录已知事实，供工作流 A 参考，不构成对 A 的约束：

- mdui 2.1.5 是基于 Lit 的 Web Components，**没有官方 React 封装**（`@mdui/react` 与 `mdui-react` 在 npm 上均不存在）。
- React 19 对自定义元素有原生支持，这是本方案可行的前提；React 18 下属性传递和事件绑定都要额外适配。
- **非标准名称的自定义事件仍需 `ref` + `addEventListener`。** mdui 组件派发的标准 `input` / `change` 事件可以正常冒泡，但 `open`、`close` 这类组件专有事件需要手工绑定。这一点在定案 ADR-0004 时需要确认最终处理方式。
- 主题与暗色模式的事实来源（mdui 的 `setColorScheme` 还是自定义变量层）会直接影响对话区取哪些 token，是 ADR-0004 的关键输入。

## 7. 验收

- `web/src/agent/` 之外的文件在工作流 B 的提交中零改动（`api.ts` 除外）。
- 对话区在浅色与深色主题下均正确渲染，且颜色全部来自 token 而非硬编码。
- 对话区在 400px 宽度下可用（与 `pwa.md` 的响应式要求一致）。
- 断网后重连，进行中的运行能续上，不产生重复消息。
- 前端不复制任何服务端领域规则：最多三个核心项、revision 校验、完成语义一律由后端强制（`arch.md` §6.2）。
