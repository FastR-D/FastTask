# Agent / Harness 优化实施交接

状态：代码、自动化与真实模型评测已交付，完整人工 QA 矩阵待验收。完整设计与验收条件如下；已落地范围和未完成项见 [实施记录](implementation-progress.md)，不能据本文档推定全部验收完成。最新证据与未验收项见 [验收报告](verification-report.md)。

编写日期：2026-10-29。分析基线：`90d0af74de1077c7270fa1299ed9fecbcc8eb435`。

适用对象：负责实现、编写自动化测试、集成和人工 QA 的 agent / 开发者。

## 阅读与交付顺序

1. 阅读本文件，理解范围、不变量、接口建议及实施依赖。
2. 按 [自动化测试规范](automated-tests.md) 编写能暴露原问题的测试，再完成实现。
3. 使用 [QA 手册](qa.md) 做真实浏览器与 sidecar 联调并填写验收记录。
4. 使用 [行为评测样例](eval-cases.json) 验证科研任务质量；runner 和报告见 [评测说明](../../evals/agent/README.md)；底层回归不替代真实模型验收。

本目录中的 DTO、错误码、文件名和参数默认值，除明确标注为现状外，均是建议契约。实现者可以调整内部拆分；对外语义、测试断言或验收条件的调整必须写明理由并同步本目录，不能仅以测试通过代替需求完成。

## 1. 范围和现状

当前链路：

```text
用户命令 -> Go 写入 user message / run
                    |
          +---------+----------+
          |                    |
     浏览器 WASM           Worker -> Node sidecar
          |                    |
          +------ libfx -------+
                    |
        Go 模型代理 -> 上游模型 SSE
                    |
        Go 记录模型输出 / tool_call_id
                    |
        Go 工具执行 -> 提案 -> 用户审批
                    |
        持久化事件 / 快照 -> assistant-ui
```

保留的基础：服务端掌握模型凭据、用户身份、工具 registry、审批及业务数据库；host 是驱动者；模型输出和工具结果由服务端记录；已执行工具按稳定身份重放结果。当前默认一名用户只允许一个活跃 run，本轮继续保留这个产品约束。

本轮不引入多 agent 调度，不更换 libfx，不重写科研任务和排期业务，不增加外部科研系统操作权限，不改变“当日满足”与“底层 Task 完成”的区别。性能改造先以当前 SQLite / 单服务部署为目标；关键持久化条件不能只依靠进程内锁。

### 1.1 代码依据与问题分类

以下是静态代码确认的行为；故障的端到端复现由测试任务完成。并发和负载条目标注为待验证风险，不把它们表述为已发生的线上事故。

| ID | 分类 / 优先级 | 当前行为和影响 | 主要代码入口 |
| --- | --- | --- | --- |
| H01 | 功能缺口 / P0 | 浏览器只提取文本，空文本不覆盖全局 `lastUserText`；两种 host 的 prompt 都未携带图片。只发图可能复用旧文本，图片已显示却未进入模型输入 | `web/src/agent/runtime.ts` 的 `extractUserText` / `prepareAgentCommand`；`web/src/harness/host.ts` 的 `promptBlocks`；`internal/application/agent.go` 的 `runPromptText` |
| H02 | 恢复缺口 / P0 | checkpoint 只按存在性和 libfx 版本判断可用，缺少对话覆盖位置；中间一轮未保存会继续使用更早上下文。浏览器读取失败直接返回 null；sidecar 直接接收数据库 checkpoint | `internal/application/harness.go` 的 `instructionsFor`；`harnesslifecycle.go`；`agent.go` 的 `driveViaSidecar`；`host.ts` 的 `readCheckpoint` |
| H03 | 功能缺口 / P0 | 停止按钮调用只处理浏览器本地实例的函数，sidecar 路径无本地 active；断开显示流不取消 run。host 普通 fetch 没有贯通取消信号 | `web/src/agent/runtime.ts` 的 `onCancel`；`web/src/harness/host.ts` 的 `cancelActiveRun` / `hostFetch`；`internal/application/agentthread.go` |
| H04 | 执行边界缺口 / P0 | 模型请求入口检查 wall clock，执行期间没有同等硬截止；取消检查依赖收到上游字节。`MaxOutputTokens` 未用于真实代理请求 | `internal/application/harnessproxy.go` 的 `OpenModelProxy` / `proxyReader.Read`；`agent.go` 的 `LoopLimits` |
| H05 | 并发与持久化风险 / P1 | chunk 序号的 MAX 查询和插入不在同一事务；model_calls 在响应结束后计数；多路写入缺少统一串行提交约束；若干关键写错误被忽略 | `internal/persistence/agent.go` 的 `AppendChunk`；`application/harnessproxy.go` 的 `finish`；`harnesstools.go` |
| H06 | 成本与前置条件优化 / P1 | 任务树同时返回 tasks 与 tree；prompt 倾向多项上下文读取；提案可省略 base_revision 并采用当前版本，无法证明模型基于该版数据构造变更 | `agenttools_readonly.go` 的 `toolGetTaskTree`；`agenttools_proposal.go` 的 `buildTaskTreePatchProposal`；`agentloop.go` 的系统提示 |
| H07 | 上下文质量优化 / P1 | 降级路径是最后 12 条模型消息拼接，缺少 token 预算和长期约束记忆；历史拼入系统提示；末尾截断可能丢掉最新事实 | `application/harness.go` 的 `rebuildSummary` / `truncateInstructions`；`agentloop.go` 的 `rebuildModelContext` |
| H08 | 可维护性与可测性 / P1 | browser / sidecar 分别实现心跳、审批等待和收尾；配置下发与实际使用、usage 持久化缺少闭环验证 | `web/src/harness/host.ts`、`sidecar-driver.ts`、`shim.ts`；`harness.go` 的 RunGrant；`harnesslifecycle.go` 的 Completion |

基线已有相关 Go 包测试通过，前端 `npm --prefix web test` 的 42 项测试通过；覆盖主要集中于 Panel、业务规则和模块跳转。现有 `libfx.d.ts` 注释中提到的 contract / boundary 测试文件，在此基线的仓库测试目录中未找到，不能作为已存在的测试依赖。

## 2. 必须保持的不变量

| 编号 | 不变量 | 验证依据 |
| --- | --- | --- |
| I01 | 用户身份来自认证上下文；host 输入、模型参数不得扩大用户 / 线程 / run 范围 | 跨用户接口、附件及工具身份反例 |
| I02 | 任务树、目标、计划结构变更仍通过提案及用户审批；显示提案不等于已应用 | 审批前后业务快照 |
| I03 | 工具使用真实模型 call ID、完整参数和 run / model step 关联；部分响应中的工具不可执行 | 假模型 SSE 与执行计数器 |
| I04 | 相同工具执行身份不会因网络重试重复产生副作用；结果未知不能直接重跑 | 同 ID 并发、断线及恢复用例 |
| I05 | 取消被服务端确认后不得再开始新模型调用、抢占新工具或应用新审批；已提交动作保留证据 | 用并发屏障定义取消与动作提交的先后关系 |
| I06 | 恢复历史不能再次执行历史工具；checkpoint 是优化载体，不是业务事实来源 | 恢复前后业务及执行计数不变 |
| I07 | 同一 run 的持久化事件有确定顺序；快照游标与已提交事件一致；重连不重复 append-text | 从零回放与快照后回放得到同一状态 |
| I08 | 浏览器、sidecar 的模型输入和业务结果语义一致；展示从服务端记录派生 | 双 host 合约测试 |
| I09 | API Key、图片原始 bytes 和认证 token 不进入对话 / 事件 / 日志；模型传输中的图片 bytes 仅由服务端校验后注入 | 请求采集及持久化 / 日志扫描 |
| I10 | waiting、partial、cancelled、interrupted、failed、completed 的语义有区分；不能把未知结果描述为成功 | 错误码、UI 与数据库交叉验证 |

I04 是“稳定身份下至多一次开始执行 + 可核对结果”，不是对任意外部系统的 exactly-once 承诺。只读失败可以按独立策略重试；有副作用且执行结果未知时必须核对。

## 3. 目标结构与公共契约

### 3.1 结构拆分

建议形成五个协作单元，避免继续通过嵌入类型数量或方法数量指导业务边界：

| 单元 | 责任 | 不承担的责任 |
| --- | --- | --- |
| RunCoordinator | 接受命令、host 选择、run 状态转换、取消、预算和终态 | 模型自由文本推理 |
| ContextHydrator | 规范输入、附件引用、checkpoint 校验、基线历史和摘要 | 业务写入 |
| ModelGateway | 服务端参数约束、模型单飞、流解析、使用量和超时 | 相信 host 声明的工具结果 |
| ToolExecutor / EventWriter | 工具身份验证、执行抢占、结果持久化、审批状态与有序事件 | 在数据库事务内等待模型或用户 |
| SharedRunDriver + HostAdapter | 共用启动、心跳、审批等待、收尾逻辑；adapter 注入浏览器 / Node 能力 | 持有上游模型 Key 或直接访问 SQLite |

允许分阶段以现有文件完成修复，再抽取公共 driver；禁止先大规模搬文件而不完成 P0 行为。

### 3.2 规范运行输入（H01、H08）

由服务端从 `run.ParentMessageID` 指向的已入库消息构建，不能继续由页面内全局变量决定。建议 DTO：

```json
{
  "contract_version": 2,
  "run_id": "run-fixture-1",
  "thread_id": "thread-fixture-a",
  "host_mode": "sidecar",
  "input": {
    "message_id": "msg-fixture-1",
    "message_seq": 7,
    "parts": [
      {"type": "text", "text": "解释图中的趋势"},
      {"type": "image", "image": "/api/v1/agent/attachments/attachment-fixture-1"}
    ]
  },
  "context": {
    "strategy": "checkpoint",
    "context_revision": 4,
    "baseline_id": null,
    "checkpoint_through_message_seq": 6,
    "libfx_version": "0.0.10"
  },
  "policy": {
    "max_model_turns": 8,
    "max_model_attempts": 10,
    "active_budget_ms": 180000,
    "max_output_tokens": 4096,
    "reasoning": "provider-default"
  }
}
```

示例附件路径只是易读占位；实现必须沿用仓库现有 attachmentReference 的真实格式，不能另发明 URI。`4096` 与 `10` 是建议的可配置初值，不是已存在的生产默认值。凭据仍经独立 capability 下发，禁止把上游 Key 加入这个 DTO。

要求：

- browser grant 与 sidecar 交接调用共用构建函数；两种 host 接收同一语义的 input / context / policy。
- 纯图片输入合法；空文本不能复用之前的文本；保持 part 顺序和多行内容，不在不同文件重复拼接。
- libfx 0.0.10 本地源码明确拒绝 `{type:'image'}` prompt block，只接受 text / resource。不得仅修改自定义声明让图片“通过编译”。
- host 使用可支持的 resource 引用；ModelGateway 必须从已绑定消息的附件重新校验并向上游注入真实 image content。不能假定 resource URI 会自动被 libfx 或 SDK 变成图片。
- 在每次模型请求中绑定当前 run 的规范用户输入；校验 / 替换 host 携带的当前用户消息，避免重复追加或把附件附到历史用户消息。找不到可核对的当前消息位置时明确拒绝。
- 入库时和模型展开时分别检查附件所属用户及绑定消息；图片无法读取时明确失败或明确告知该附件不可用，不能悄悄按纯文本回答。
- 接受新命令、读取 grant、恢复页面均检查当前 owner；账号切换后旧异步响应不得启动新 host 或覆盖新账号状态。

### 3.3 展示用 run ID 与驱动权限分离（H03）

所有模式都在状态中暴露 `fasttask.runId`，用于取消和恢复。是否启动浏览器 driver 必须同时检查服务端确认的 `harnessMode === 'wasm'` 及相应驱动授权，不能仅凭 runId 存在判断。

`X-Harness-Run` 保留现有语义，仅表示浏览器需要驱动的 WASM run，不用于 sidecar 驱动；服务端已有 run ID 不等于授予模型 / 工具权限。sidecar grant 的浏览器认领限制继续保留。

UI 点击停止：先对当前 owner / runId 发用户认证的 cancellation API，再尽力中断本地 driver；sidecar / 刷新恢复场景也必须调用服务端。发送状态显示“正在停止”，收到终态后结束；取消请求失败时保留可重试操作，不得以吞掉错误方式伪装成功。

网络断开、显示流卸载、页面切换和网络异常的 SDK onCancel 不能自动当作用户停止。工具和长轮询传入 signal；收尾上报使用独立、短时、有限重试的 cleanup 请求，不能复用已经 abort 的主 signal。

### 3.4 上下文恢复策略（H02、H07）

新增 checkpoint 元数据建议包括：写入 run、覆盖到的 message_seq、context_revision、libfx_version、可选 baseline_id、更新序号。现有 `AgentRun.CheckpointSeq` 是运行事件日志游标，不能直接拿来当线程消息水位。

`through_message_seq` 仅表示完整纳入上下文的连续前缀，不能越过未落盘的工具结果。已覆盖消息内的晚到审批结果也会改变语义，因此额外使用 `context_revision`，或等价的消息 revision / 内容指纹，不能只增加 message_seq。

| 条件 | 行为 |
| --- | --- |
| 无历史 | 空上下文；不显示“已丢失上下文” |
| checkpoint 版本、覆盖位置、语义 revision 全部匹配 | 允许恢复，并应用与其配套的 baseline |
| checkpoint 旧版本、过期、损坏或无覆盖元数据 | 旧 bytes 不交给运行时；由服务端权威历史重建 |
| 读取 checkpoint / 元数据出现网络或数据库错误 | 有界重试；随后明确失败或显式请求服务端重建，不可静默当成新线程 |
| 旧 run 晚到 checkpoint，已有更新 run 或恢复 revision | 条件写拒绝，保留新上下文 |
| 未知 / 不可核对的工具结果 | 保留 unknown / pending，不虚构成功，也不为了补齐历史再次 execute |

推荐的短期安全实现：过期 checkpoint 整体放弃，从已持久化历史构造上下文；完整新鲜 checkpoint 才复用。不要向模型发送“请重做上次操作”来补历史。

历史重建放在模型代理的上下文组装层，以原 user / assistant / tool 角色提供。工具调用与结果必须成对，维持模型 step 顺序；现有 `rebuildModelContext` 把一个 assistant message 内多轮内容合并，不能未经验证直接当作可重放模型消息。

**特别注意上下文基线的持久性**：如果重建历史是代理注入的，libfx 的新 checkpoint 不会自动包含这部分历史。实现必须把注入的 baseline 作为独立版本化记录，与 checkpoint 绑定，并在以后每次调用继续提供；或者选择始终从服务端重建模型上下文。禁止“这轮代理补历史，下轮只用新 checkpoint”造成第二次丢历史。baseline 与 checkpoint 的分界要保证历史不重复。

压缩摘要建议保留：目标和验收标准、用户明确约束、已审批决定、真实工具事实、未解决问题及来源消息。最近窗口保持原文，较早内容按预算压缩；原始对话和业务证据不删除。外部材料与用户历史不提升成 system / developer 指令。

预算优先级：固定产品规则 -> 当前输入 -> 待处理审批 / 最近决定 -> 近期完整工具交互 -> 早期摘要 -> 可再次检索的材料。完整计算 UTF-8 上限，预留截断标记空间；裁剪旧摘要而不是无条件删除最新尾部。

### 3.5 预算、取消与状态机（H03、H04）

现有 8 轮 / 180 秒主动运行 / 10 秒普通工具预算继续作为初始策略。新增响应头等待和流空闲默认值建议均为 30 秒，必须可注入、可配置；慢推理模型部署可调，测试不用等待真实 30 秒。

定义：

- model turn：逻辑模型步骤；流失败后重试同一步仍为同一逻辑步骤。
- model attempt：一次上游 HTTP 尝试，含建连 / 供应商错误；单独设置上限，防止无限“未成功不计次”。
- active time：首次成功认领并进入 running 后的运行时间，排除 run 处于等待用户审批状态的区间；不是各长轮询请求等待时间之和。queued 阶段用独立的 pickup / job 等待规则，不因 Worker 队列等待消耗模型预算。
- stream idle：上游没有收到任何传输字节的时间；仅有 SSE 心跳时仍受 active time 约束，不能靠心跳永久存活。

服务端原子预留 step / attempt 后才开始外部请求。每个 run 同时最多一个模型流；重复同 attempt ID 返回状态或已记录结果，不重新请求上游；下一次 attempt 需明确分配新身份。

所有等待均受 context / signal 限制。运行预算到期、取消请求确认或 run 进入终态时，要主动关闭上游 body / 取消请求，即使 Read 正阻塞或供应商还没有响应头。数据库状态轮询需独立于流收到字节；本地通知可提速，持久化条件仍兜底。

审批等待从进入 awaiting_approval 到最后一个 pending 提案得到终态按 run 记一次；并行两个等待不双倍扣除。长轮询断开不重置 15 分钟审批期限；reaper 可独立发现超时，不能只有 host 持续轮询才超时。

状态转换：

```text
queued -> running -> succeeded / failed / interrupted
             |
             +-> awaiting_approval -> running

queued / running / awaiting_approval -> cancelling -> cancelled
```

终态不可被晚到上报改成别的状态；`CompleteRun` 的未知 stop reason 不可默认成功。映射 libfx 的已验证成功、取消和预算结束原因，并保留诊断原值；无法识别的结果用明确失败 / interrupted 原因处理。预算耗尽可保留现有可读收尾，但记录 budget 结束原因，不伪装任务完成。

取消与业务提交竞态以数据库提交顺序裁定：动作先提交则保留；取消先提交则不得开始新动作。不能要求取消撤销已完成的事务或外部副作用。新建 run 检查应统一包含 cancelling，保持当前“每用户一个活跃 run”的约束。

### 3.6 有序事件、结果与完成上报（H05）

- 序号分配与事件写入在同一事务完成，使用原子计数器或事务内 MAX；不在网络请求、工具执行或人工审批期间持有事务。
- 所有写入路径遵守相同顺序约束，不能只在模型代理加 mutex 而遗漏工具结果 / 审批 / reaper。
- 快照包含游标之前的全部事件；并发写入不能把快照推进到自己未合并的事件之后。快照后 replay 与全量 replay 的结果一致。
- 文本可按建议的 20–50ms / 约 1KiB 合并成一个持久化事件，保序；工具开始、完整参数、审批和终态是立即 flush 屏障。具体值经测量调整。
- 工具执行使用已存在的 CAS；执行完成后请求断开，结果记录使用独立的短时 cleanup context。claim 后崩溃且无结果仍标记 unknown，不能释放 claim 直接重做。
- 关键写失败必须返回错误或记入可恢复状态及结构化日志，不能忽略后继续上报成功。事件 / 结果 / run 终态的提交需要一致性设计。
- 完成上报可按 run + completion identity 重试；终态只发生一次。checkpoint 写成功但终态失败、终态成功但响应丢失，均有明确恢复路径。
- token 认领 / 轮换应原子化并绑定 host generation；两个同时 grant 不能留下两个有效 owner。旧 generation 的已认证请求也必须在实际写时被 fence。
- token 轮换响应丢失不能永久驱逐当前合法 host：使用有界重叠窗口或按旧 token 安全重放同次轮换等可验证方案；不降低到允许多个 host generation 同时执行。

### 3.7 工具与 prompt 优化（H06、H07）

建议新增 `get_planning_context`：在同一一致性读取中返回主要目标、规划设置、今日安排、相关可执行候选、阻塞原因、revision 和可继续读取入口。它是只读聚合，不允许隐藏排期或补位写入。

保留精确查询工具；简单打开工作区的请求无需读取整个任务树或周复盘。任务树结果提供单一表示、子树范围 / 分页、has_more 及 revision。所有影响决策的截断必须显式标识，不能基于局部树推断不存在同名任务或全局依赖已满足。

新版已有树变更提案要求明确读取版本；旧调用省略 revision 的兼容路径只能使用服务端记录的本 run 最近有效观察版本，无法核对则要求重读。不要自动用当前版本替代。允许 revision 0 作为真实空树版本，不能把它无条件当作“省略”。

系统提示分为稳定产品规则和任务相关指引；按请求目的减少无关工具说明。提示与实际 manifest 做一致性检查；默认没有注册的工具不能在提示中作为可调用能力出现。工具选择仍由可解释策略或模型处理，无需引入额外“意图分类模型”作为必经步骤。

### 3.8 配置、使用量与观测（H08）

`reasoning` 下发必须验证实际模型请求是否采用；不同兼容 Provider 使用的参数由适配器映射，不能假定一个字符串字段通用于所有模型。`max_output_tokens` 由服务端钳制，host 请求更大不能绕过。

usage 优先采集服务端上游流的可验证值，host report 为辅助；按 step / attempt 去重并明确费用未知的失败请求。只累计成功报告会漏掉已计费但响应丢失的调用。未知 token 数量保存 unknown，不写成 0 以制造低成本。

最小事件字段：run_id、step_id、attempt_id、host_mode、host_generation、tool_call_id（适用时）、阶段、结构化错误码、耗时、重试原因、context_strategy / revision、可用 usage、是否截断。日志不记录完整 prompt、材料正文、reasoning 原文或凭据；错误体保留既有脱敏。

## 4. 兼容与迁移

1. 先增加可选版本字段、读接口和迁移，再发布新 driver。服务端保留既有工具 / 模型 / 审批入口。
2. sidecar 仍不能由浏览器认领；老前端依据 X-Harness-Run 驱动 WASM 的路径继续工作。只加 runId 字段前，必须检查所有“有 ID 就启动 driver”的消费者。
3. 所有新增 metadata 使用允许旧数据存在的默认值；历史 checkpoint 覆盖水位未知，按需重建，不猜测已完整覆盖，不批量删除对话。
4. `migrations/` 与 `internal/persistence/migrations/` 有两份 SQL；新增迁移按当时实际最大版本分配，两处同步，并更新 `ExpectedSchemaVersion` / Ready 检查和相关文档。当前版本是 21，不预占固定 22 给并行任务。
5. 数据迁移只增加必要字段 / 表 / 索引，不改写历史业务结果。升级前后抽样比对消息、提案、进展事件和任务状态。
6. 未升级 host 的输入可在代理层从规范消息补齐；新契约必需能力缺失时返回可理解的升级提示。不得静默进入演示模式，也不将旧 token 扩权。
7. 回滚程序之前检查 schema 兼容性；新元数据可忽略时保留。不能以恢复旧数据库覆盖升级后的真实研究成果。回滚流程至少在测试副本中演练一次。

## 5. 实施任务包与依赖

以下供任务分配使用，不表示本次已启动其他 agent。共享文件修改由集成负责人协调。

| 包 | 工作内容 | 前置依赖 | 主要文件 / 交付 | 必过测试组 |
| --- | --- | --- | --- | --- |
| W0 | 建立测试 fixture、假模型、冻结契约 | 无 | 独立 test helper；错误码 / DTO 决策记录 | 当前基线及 fixture 自检 |
| W1 | 规范输入、图片实际进入模型、host 无关 runId 和取消 | W0 | agent.go / agentthread.go / runtime.ts / host.ts / sidecar-driver.ts / shim.ts | IN、CA、AU |
| W2 | checkpoint 元数据、失效判定、baseline 与角色历史恢复 | W0；与 W1 对齐 DTO | persistence / harness lifecycle / hydrator / 迁移 | CX、DB |
| W3 | model step / attempt 抢占、预算、取消硬截止、输出 cap | W0；依赖 W1 取消契约 | harnessproxy / persistence / Provider 适配 | BG、SM、TC |
| W4 | 事件事务、快照一致、结果收尾、claim 与 grant fencing | W0；与 W2/W3 对齐模型 | persistence / EventWriter / approvals | EV、TC、AP、DB |
| W5 | 聚合上下文工具、提案观察版本、输出预算、prompt 一致性 | W2/W4 基础契约稳定 | agenttools / agentloop / tool manifest | TL、CQ、评测样例 |
| W6 | 抽取共用 driver、配置 usage 闭环 | W1/W2/W3 稳定 | harness shared core + adapters | HC、OB、全部 P0 |
| W7 | 真实 host 联调、性能测量、QA、回归记录 | 所有纳入发布包 | QA 记录、命令原始结果、trace 摘要 | QA 矩阵和发布门禁 |

W1–W4 先于 W5/W6 的重构合并。每包提交说明：修复前触发条件、修复后行为、实际执行命令、失败/跳过项和未解决问题。不要把“写了测试文件”当作“测试已执行”。

## 6. 发布门禁

需求到验收的主映射如下；完整案例的优先级与断言以自动化测试规范为准。

| 需求 | 自动化测试组 | 人工 QA | 行为评测 |
| --- | --- | --- | --- |
| H01 规范输入 | IN、HC | QA01–QA03 | E18、E19 |
| H02 恢复正确性 | CX、DB | QA07–QA09、QA17 | E15 |
| H03 host 无关取消 | CA、AU、HC | QA04–QA06 | E16、E18 |
| H04 运行预算 | BG、SM、TC | QA12–QA13 | E16 |
| H05 有序提交 | EV、TC、AP、SM、AU、DB | QA09–QA11、QA14、QA17 | E03、E06、E17 |
| H06 工具与版本 | TL、AP | QA15–QA16 | E01–E13、E20 |
| H07 上下文质量 | CX、CQ | QA07–QA08、QA15–QA16 | E12–E15、E20 |
| H08 driver 与观测 | HC、OB | QA02、QA05、QA18 | E19 |

- 自动化测试文档中 P0 全部通过，覆盖 browser 和 sidecar；纯 JS fake 不能代替真实 libfx shim 验证。
- 若分期发布，仅允许明确列出未纳入的 P1；受本次变更影响的原有业务回归必须通过。
- QA 中核心路径全部通过；取消、上下文恢复、审批与重复副作用相关问题不带病发布。
- 新增观察指标能区分重试、超时、未知结果、上下文降级及明确完成；无凭据泄露。
- 性能用同机器、同 fixture、同 Provider / model 配置比较；记录输入输出 token 和成功率，不能只展示响应时间降低。

## 7. 设计问答

**Q：能否通过延长 180 秒或增加轮次解决卡住？** 不能代替执行期间的 deadline 和 attempt 上限。修复边界后，可按实测配置预算。

**Q：能否旧 checkpoint 恢复后再 prompt 一遍遗漏内容？** 不能把历史业务动作作为新请求执行。短期用完整重建并防重放；若增量补历史，需要明确无 execute 语义及能力证明。

**Q：是否承诺所有被中断工具自动恢复？** 不承诺。已知结果可以重放；执行已开始但结果未知时保留 claim 并核对，有副作用的动作不得无条件重试。

**Q：为什么不能仅比较 message_seq？** 旧消息内的审批和工具结果可以晚到，seq 不变而上下文语义变化。必须关联 revision / 指纹。

**Q：为什么 checkpoint 写上 run_id 仍不够？** 它只能说明谁写的，不能证明写入包含全部已提交结果，也不能证明代理注入的 baseline 被包含。

**Q：是否需要先迁移到 PostgreSQL、消息队列或完整事件溯源架构？** 当前不要求。SQLite 事务、条件更新和清晰事件边界可以完成本轮目标。

**Q：为什么需要真实 runtime 测试？** 本地 libfx 声明是人工维护的快照，图片 block、资源引用、iterator 消费和 checkpoint 的真实行为必须通过运行验证。

**Q：模型自己遵循 prompt 是否足够？** 身份、版本、审批、执行抢占和预算必须由程序约束；建议内容质量再由行为评测衡量。

参考阅读：[Anthropic 上下文工程](https://www.anthropic.com/research/effective-context-engineering-for-ai-agents)、[Agent 评测方法](https://www.anthropic.com/research/demystifying-evals-for-ai-agents)。设计判断以本仓库实际行为和产品不变量为依据。
