# 架构决策记录（ADR）

> 文档状态：索引  
> 适用范围：FastTask 后端、前端、Agent 运行时和客户端形态

`doc/arch.md` §1 把一部分方案标记为「设计决策」，并说明「后续可通过 ADR 调整」。本目录保存这些调整的正式记录。

## 1. 约定

- 一条 ADR 记录一个决策，文件名为 `NNNN-短标题.md`。
- 状态取值：`提议`、`已接受`、`待定`、`已取代`。
- ADR 一旦进入 `已接受` 就不再改写正文。改变主意时新增一条 ADR，并把旧条状态改为 `已取代`，注明取代者编号。
- ADR 只记录**为什么这样选**和**边界在哪**。实现细节写进对应的设计文档，由 ADR 链接过去。
- 与 ADR 冲突的既有文档段落必须在同一次提交中更新，不允许留下互相矛盾的两处描述。

## 2. 索引

| 编号 | 标题 | 状态 | 影响范围 | 实现文档 |
|---|---|---|---|---|
| [0001](0001-fx-composition-root.md) | 采用 fx 作为组合根并按聚合拆分 `application.App` | 已接受 | 后端装配、生命周期、角色进程 | [`doc/wiring.md`](../wiring.md) |
| [0002](0002-assistant-transport.md) | Agent 前后端采用 assistant-transport 协议 | 已接受 | HTTP 契约、前端运行时、数据模型 | [`doc/agent-impl.md`](../agent-impl.md) |
| [0003](0003-in-process-agent-loop.md) | Agent 循环内建于 Go 进程，工具即受控用例 | **已取代**（→0005） | Agent 运行时、权限与事务边界 | [`doc/agent.md`](../agent.md) |
| [0004](0004-frontend-styling-boundary.md) | mdui 与 assistant-ui 的样式与职责边界 | 已接受 | 前端目录、主题、两条并行工作流 | [`doc/frontend.md`](../frontend.md) |
| [0005](0005-libfx-agent-harness.md) | 采用 libfx 作为 Agent Harness，双宿主（WASM 默认 / Node sidecar 兼容） | 已接受 | Agent 运行时、模型代理、前端宿主、部署形态 | [`doc/harness.md`](../harness.md) |

## 3. 决策依赖关系

```text
0003 Agent 形态（工具即受控用例）          [已取代]
  │     └─ 工具三级分类与不变量论证被 0005 完整继承
  └─> 0002 传输协议（需要服务端持有权威状态 + 工具审批）
        │     └─ 0005 不改动本条：宿主不参与渲染，UI 仍从服务端状态渲染
        └─> 0001 组合根（新增多个长生命周期组件，需要统一装配与启停）
              └─ 0005 新增 SidecarModule，沿用同一套 Lifecycle 规则

0005 Agent Harness（libfx，双宿主）
  └─> 取代 0003 的「循环在 Go 进程内」，保留其余全部结论

0004 前端样式边界（独立于其余各条，不影响后端）
```

0004 曾被有意挂起，等 mdui 完全重写落地后依据真实代码定案，现已接受。

**术语警告：** 0001 标题中的「fx」指 `go.uber.org/fx`（DI 容器），0005 中的「libfx」指 `vercel-labs/fx` 的嵌入 SDK。
两者毫无关系。新文档中一律写 **uber-fx** 与 **libfx**，不使用裸「fx」。见 [ADR-0005](0005-libfx-agent-harness.md) §0。
