# FastTask 组合根与依赖装配规格

> 文档状态：初版实现规格  
> 决策记录：[ADR-0001](adr/0001-fx-composition-root.md)  
> 上位约束：`doc/arch.md` §5.1、§6.2、§6.3；`doc/tech.md` §4  
> 依赖基线：`go.uber.org/fx` v1.24.0

## 1. 目标

1. 用 fx 替换 `cmd/fasttask/main.go:39-105` 的手工装配。
2. 把 `application.App` 按 `arch.md` §7 的领域模块拆成多个服务。
3. 兑现 `arch.md` §5.1 承诺但从未实现的角色化命令（`serve` / `worker` / `scheduler`）。
4. 为 Agent 运行时新增的长生命周期组件提供统一的启停位置。

**非目标**：不按 `arch.md` §6.1 重排整个 `internal/` 目录树。目录重排是独立的、风险更高的动作，留待后续单独决策。

## 2. 硬规则

违反任何一条都视为实现错误：

1. **`go.uber.org/fx` 只允许被 `internal/bootstrap` 和 `cmd` 导入。** 其余包一律不得出现该导入。建议加一条 CI 检查：`go list -deps` 或 `grep -rn "go.uber.org/fx" internal/ --include=*.go` 的结果必须只落在 `internal/bootstrap`。
2. **所有构造函数是普通 Go 函数**，签名形如 `NewGoalService(store *persistence.Store, clock Clock) *GoalService`，可以脱离 fx 直接调用。不使用 fx 的结构体标签注入参数以外的魔法。
3. **依赖方向不变**，仍遵守 `arch.md` §6.3。fx 只决定「谁调用构造函数」，不改变「谁可以依赖谁」。
4. **单元测试不使用 fx**，直接构造服务。只有跨组件集成测试用 `fxtest`。
5. **每个角色命令必须有 `fx.ValidateApp` 测试**，让装配错误在 CI 而不是生产启动时暴露。

## 3. 模块划分

```text
internal/bootstrap/
  module.go        // 汇总各 Module，定义角色组合
  persistence.go   // Store 的 Provide + Lifecycle（打开、迁移校验、关闭）
  auth.go          // platformauth.Service
  application.go   // 各领域服务的 Provide
  httpapi.go       // Gin Engine、Huma API、路由值组、http.Server 的 Lifecycle
  agentruntime.go  // Run Manager、Hub、工具注册表
  worker.go        // Worker 的 Lifecycle
  scheduler.go     // gocron 的 Lifecycle
  sidecar.go       // Node sidecar 进程的 Lifecycle（ADR-0005，见 harness.md §8.3）
```

> **术语：本文的「fx」一律指 `go.uber.org/fx`（DI 容器），即 uber-fx。**
> [ADR-0005](adr/0005-libfx-agent-harness.md) 引入的 `libfx` 是完全无关的 JS 包，
> 它只出现在 `web/src/harness/` 与 `sidecar/`，**不进 Go 依赖图**。

角色组合：

```go
var Core = fx.Options(PersistenceModule, AuthModule, ApplicationModule)

var ServeRole     = fx.Options(Core, HTTPModule, AgentRuntimeModule)
var WorkerRole    = fx.Options(Core, AgentRuntimeModule, WorkerModule)
var SchedulerRole = fx.Options(Core, SchedulerModule)
```

`serve --with-worker --with-scheduler` 按标志把 `WorkerRole` 和 `SchedulerRole` 的模块并入同一个 fx App，**保持现有默认行为不变**（两个标志默认都是 `true`）。

## 4. `application.App` 拆分

现状：`app.go`、`admin.go`、`lens.go` 共 50 余个方法挂在同一个 struct 上。目标拆分：

| 新服务 | 承接的方法 | 对应 `arch.md` |
|---|---|---|
| `GoalService` | `CreateGoal`、`UpdateGoal`、`CreateTask`、`UpdateTask`、`CompleteTask`、`ApplyProposal`、`RejectProposal`、`snapshotTaskTree`、`createTaskTx` | §7.2 |
| `PlanService` | `CreateDailyPlan`、`ReplanDailyPlan`、`CloseDailyPlan`、`AddPlanItem`、`UpdatePlanItem` | §7.3 |
| `ProgressService` | `StartSession`、`TransitionSession`、`CompletePlanItem` | §7.4 |
| `AgentService` | 对话、运行、工具注册（新增，见 [`agent-impl.md`](agent-impl.md)） | §7.5 |
| `JobService` | `CreateJob`、`RetryJob`、`CancelJob`、`AgentCallback`、`MaterializeJobResult` 的调度部分 | §7.6 |
| `DeviceService` | `RegisterDevice`、`RotateDeviceToken`、`DeviceByToken` | §7.7 |
| `IntegrationService` | `CreateExternalImport`、`ConvertExternalImport`、`RejectExternalImport`、Panel 摘要 | §7.8 |
| `AdminService` | 用户、会话、审计相关方法 | §7.1 |
| `ProviderService` | `ListModelProviders`、`CreateModelProvider`、`UpdateModelProvider`、`ActivateModelProvider`、`DeleteModelProvider`、`VerifyModelProvider`、`ActiveProviderRuntime`、密钥加解密 | §7.1 |
| `LensService` | `SetTaskCoord`、`WeeklyReview`、`GoalMap` | `lens.md` |

`ProviderService` 从 `AdminService` 里单独拆出，因为 Worker 每次执行作业前都要解析默认 Provider（`main.go:66-83`），它不是管理面专属能力。

### 4.1 跨聚合方法的处理

有三处方法天然跨聚合，必须显式处理，不能靠「放哪个服务都行」糊过去：

| 方法 | 跨越 | 处理 |
|---|---|---|
| `CompletePlanItem` | Progress 写事件 + Plan 改状态 | 归 `ProgressService`，通过 `TxManager` 在同一事务内调用 `PlanService` 的领域函数 |
| `MaterializeJobResult`（`app.go:884`） | 按作业类型分别落到 Goal / Conversation / Plan | 拆成值组，见 §5 |
| `ConvertExternalImport` | Integration 读 + Goal 写 | 归 `IntegrationService`，同事务内调用 `GoalService.createTaskTx` |

新增 `TxManager`：由 `persistence` 提供，签名为

```go
type TxManager interface {
    WithTx(ctx context.Context, fn func(tx *gorm.DB) error) error
}
```

各服务额外暴露接受 `tx` 的内部方法（如 `(*GoalService) createTaskTx(tx *gorm.DB, ...)`）供同事务组合。

**关于 `arch.md` §6.2「不向上暴露 `*gorm.DB`」**：该约束针对的是 `application` **之上**的层——HTTP 处理器和 `domain` 都不得接触 `*gorm.DB`，这一点在重构后必须继续成立。`application` 内部以 `*gorm.DB` 作为事务句柄是现状（`app.go:884` 的 `MaterializeJobResult` 已经如此），本次重构**不改变这一点**。

把 `*gorm.DB` 换成完全不透明的 `Tx` 类型是更干净的做法，但那需要同时引入按聚合划分的 Repository 接口，改动面远超本次目标。**明确列为非目标**，需要时另立 ADR。

## 5. 值组

fx 的价值主要在这里。四个值组：

| 值组 | 提供者 | 消费者 |
|---|---|---|
| `group:"routes"` | 各领域的路由注册函数 | `httpapi` 装配时统一调用 |
| `group:"agent_tools"` | 各领域服务 | Agent 工具注册表 |
| `group:"job_handlers"` | 各领域服务 | Worker 的作业类型分发 |
| `group:"job_materializers"` | 各领域服务 | `MaterializeJobResult` 的替代实现 |

`httpapi/server.go` 已有 `registerAuth`、`registerGoals`、`registerPlans`、`registerJobs` 等 13 个分组函数（`server.go:214-1386`），直接改造成值组提供者即可，不需要重写路由定义本身。

值组成员的形状统一为：

```go
type RouteRegistrar interface {
    RegisterRoutes(api huma.API)
}
```

各领域提供一个实现该接口的类型（持有它需要的服务），由 fx 以 `group:"routes"` 收集，`httpapi` 装配时遍历调用。`agent_tools` 与 `job_handlers` 同理，各自定义一个小接口，不要用裸函数——裸函数在 fx 的错误信息里无法分辨来源。

`job_handlers` 和 `job_materializers` 替换掉 `worker.go:104` 和 `app.go:884` 两处集中的 `switch`。收益是新增作业类型时不必回到中心文件——Agent 运行时正要新增 `agent_run` 类型。

## 6. Lifecycle 与启停顺序

用 `fx.Lifecycle` 声明，fx 保证停机按启动逆序执行。

| 组件 | OnStart | OnStop |
|---|---|---|
| `Store` | 打开 SQLite、执行迁移、校验 schema 版本 | `Close()` |
| `authService` | `EnsureAdmin` | — |
| Agent Hub | 启动扇出 goroutine | 关闭所有订阅者 |
| `Worker` | 启动轮询 goroutine | 取消 context 并等待当前作业收尾 |
| `Scheduler` | `Start()` | `Shutdown()` |
| `Sidecar`（ADR-0005） | 启动 Node 进程，等 `/healthz` 就绪 | SIGTERM，宽限期后 SIGKILL |
| `http.Server` | `ListenAndServe` | `Shutdown(ctx)` |

要求：

- **停机时 HTTP 必须先于 Worker 停止**，避免新请求进入正在关闭的运行时。fx 的逆序语义天然满足，前提是 HTTP 最后启动。
- **`Sidecar` 同理必须注册在 `HTTPModule` 之前**，使其晚于 HTTP 停止——HTTP 先停，才不会有新请求打到正在退出的 sidecar。
- **`Sidecar` 的 `OnStart` 就绪超时后应失败启动，不带病运行。** 配置 `sidecar.enabled=auto` 时检测不到 Node 则跳过该模块（不报错），
  此时 harness 只有 WASM 模式可用，需在 `/health/ready` 与 `doctor` 中如实反映。
- `OnStart` 必须快速返回，长循环放进 goroutine（fx 的标准做法）。
- 保留现有的 15 秒优雅关闭预算（`main.go:100`）。
- **`http.Server.WriteTimeout` 改为 `0`**，配合 `http.ResponseController` 按请求设置写截止时间。理由见 [`agent-impl.md`](agent-impl.md) §9.2；这是 SSE 能工作的前提。

## 7. 迁移步骤

**禁止一次性大爆炸式重构。**每一步结束时 `go test ./...`、`go test -race ./...`、`go vet ./...` 必须全绿，每步一个提交。

| 步骤 | 内容 | 验收 |
|---|---|---|
| 1 | 建 `internal/bootstrap`，把现有装配原样搬进去，`main.go` 只剩 Cobra 命令定义 | 行为零变化，现有测试全过 |
| 2 | 引入 fx，把步骤 1 的装配改写成 Module + Lifecycle，`App` 暂不拆 | `fx.ValidateApp` 测试通过；`serve` 行为不变 |
| 3 | 拆出 `ProviderService` 和 `LensService`（边界最清晰，依赖最少） | 单元测试不变，仅改调用点 |
| 4 | 拆出 `DeviceService`、`IntegrationService`、`AdminService` | 同上 |
| 5 | 拆出 `JobService`，同时把 `switch` 改成 `job_handlers` 值组 | Worker 行为不变 |
| 6 | 拆出 `GoalService`、`PlanService`、`ProgressService`，引入 `TxManager` 处理 §4.1 的三处跨聚合 | 事务边界测试全过 |
| 7 | 路由改造成 `routes` 值组 | OpenAPI 输出逐字节不变 |
| 8 | 补 `worker` / `scheduler` 角色命令 | 各自能独立启动并处理作业 |

### 7.1 与 Agent 工作流的联锁

`doc/agent-impl.md` 的实现与本文档有两处交叠，已在那份文档 §8.2 约定：

- **`main.go:95` 的 `WriteTimeout` 改为 `0` 归本文档步骤 2**，Agent 工作流不重复实施。这是 SSE 能工作的前提，因此**步骤 1–2 应当先于 Agent 工作流的阶段 B 完成**。
- **Worker 的作业类型 `switch` 归本文档步骤 5**。若 Agent 工作流先到，先加 `case "agent_run"`，步骤 5 再一并改造成值组。

除这两处外，两条工作流可以完全并行。

步骤 7 的验收条件值得强调：**改造前后 `GET /api/v1/openapi.json` 的输出必须完全一致**。这是证明路由重构没有意外改变对外契约的最直接手段，建议做成一个固化对比测试。

## 8. 测试

- 每个角色命令一个 `fx.ValidateApp` 测试。
- 一个 `fxtest` 集成测试：启动完整 `ServeRole`、打一次健康检查、优雅关闭，断言无泄漏 goroutine。
- 停机顺序测试：断言 HTTP 先于 Worker 停止。
- 现有全部测试保持通过，**不允许为迁移而放宽断言**。

## 9. 验收标准

- `grep -rn "go.uber.org/fx" internal/ --include=*.go` 只命中 `internal/bootstrap`。
- `application` 包内不存在超过 15 个方法的 struct。
- `fasttask serve`、`fasttask worker`、`fasttask scheduler` 三个命令均可独立运行。
- `serve` 的默认行为与重构前一致。
- `GET /api/v1/openapi.json` 与重构前一致。
