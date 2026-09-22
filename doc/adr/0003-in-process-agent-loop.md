# ADR-0003：Agent 循环内建于 Go 进程，工具即受控用例

> 状态：**已取代**（由 [ADR-0005](0005-libfx-agent-harness.md)，2026-09-22）  
> 日期：2026-09-22  
> 取代说明：§2 的**工具三级分类**与 §3 的不变量论证**继续有效**，ADR-0005 完整保留了它们。被取代的只有「循环在 Go 进程内实现」这一条——循环驱动改由 libfx 承担，宿主可在浏览器 WASM 或 Node sidecar。§4「能力上限低于成熟 harness」这条代价即为本次取代的动因。  
> 实现文档：[`doc/agent.md`](../agent.md)、[`doc/agent-impl.md`](../agent-impl.md)  
> 相关：`arch.md` §9.1、§9.2、§9.4、§10.1

## 1. 背景

当前实现里不存在 agent 循环：

- 对话是单轮无状态的。`worker.go:181` 的 `case "conversation"` 只把一条 `content` 交给 `openai.go:122` 的 `ConversationReply`，不带历史、不带工具、不做第二轮。
- 任务树生成是一次性 JSON。`openai.go:69` 的 `TaskProposal` 靠 prompt 约束字段，模型只被调用一次。
- 每日计划**完全没有模型参与**。`worker.go:155` 直接转调确定性算法 `app.CreateDailyPlan`。

「帮忙做详细计划」要求模型能先读上下文（历史进度、阻碍、周复盘、任务坐标）、再提方案、再根据用户反馈修正——这需要多轮工具调用循环。

`arch.md` §2 原本设想 Agent Harness 是可切换的外部组件（Claude Code / Codex），`/api/v1/agent-jobs/{job_id}/callbacks`（`server.go:1140`）就是为外部 Worker 预留的。

## 2. 决策

**Agent 循环在 FastTask 的 Go 进程内实现**，工具是 application 层用例的受控投影。

工具分三级，这是本 ADR 的核心：

| 级别 | 行为 | 例子 |
|---|---|---|
| **只读** | Agent 直接执行，结果进模型上下文。强制按 `user_id` 过滤 | `list_goals`、`get_task_tree`、`get_daily_plan`、`get_weekly_review`、`list_progress_events` |
| **提案** | **不写业务表**。产出 `Proposal` 并以工具审批形式呈现给用户；用户确认后才由现有 `ApplyProposal` 落库 | `propose_task_tree_patch`、`propose_daily_plan`、`propose_task_coords` |
| **禁止** | 不作为工具暴露 | 任何管理员操作、Provider 配置、设备 Token、用户与会话管理、直接改 `daily_plan_items.status` |

保留 `AgentRunner` port，内建循环是默认实现，外部 harness 作为后续可选实现，不在本期交付。

## 3. 理由

- **不变量必须由服务端强制，而工具就是不变量的边界。** `ApplyProposal`（`app.go:991`）在事务内重新校验所有权、tree revision、父子归属和循环；`CreateDailyPlan` 强制「最多三个核心项」。把写操作全部收敛成提案，意味着 agent 无论怎么发挥都不可能绕过这些校验——安全性来自结构，而不是来自 prompt。
- **`arch.md` §9.1「Agent 调用发生在数据库事务之外」原样保留。** 工具的只读查询在事务外执行，写入只发生在用户确认后的独立事务里。
- **外部 harness 反而更难保证边界。** 把循环放到进程外，`user_id` 隔离、事务边界和工具白名单都要靠跨进程协议重新保证一遍，收益不抵成本。
- **现有 Job 骨架可以直接复用。** 租约、fencing token、attempt 校验、重试、取消（`worker.go:70-102`、`app.go:765-856`）本来就是为长任务设计的，agent run 是它的一个新 job 类型，不是另起炉灶。

## 4. 代价

- **多轮循环的成本和延迟由 FastTask 自己承担**，需要显式的轮数上限、token 预算和超时，见 `agent-impl.md` §6。
- **能力上限低于成熟 harness。** 没有文件系统访问、没有代码执行、没有子 agent。对「拆解科研目标」这个场景够用；需要读 PDF 或改 LaTeX 时走已有的 FastRead / FastWrite 集成边界（`doc/integration/`），不由 agent 直接做。
- **工具 schema 要手工维护并与 application 层签名保持同步。** 由 `agent-impl.md` §5 的测试要求兜底。

## 5. 被否决的方案

| 方案 | 否决理由 |
|---|---|
| 外接 Claude Agent SDK / Claude Code headless | 事务边界、`user_id` 隔离和工具白名单都要跨进程重新保证；本期不做，port 保留 |
| port 双实现（内建 + 外部同时交付） | 工作量接近翻倍，且两条路径的不变量都要各自验证。等内建实现稳定、且出现真实需求后再评估 |
| 让 agent 直接写业务表，靠 prompt 约束 | 直接违反 `arch.md` §9.1，且模型输出按 §12 属于不可信输入 |
