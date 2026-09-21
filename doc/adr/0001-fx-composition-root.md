# ADR-0001：采用 fx 作为组合根并按聚合拆分 `application.App`

> 状态：已接受  
> 日期：2026-09-22  
> 实现文档：[`doc/wiring.md`](../wiring.md)  
> 相关：`doc/arch.md` §5.1、§6.2、§6.3；`doc/tech.md` §4

## 1. 背景

当前全部装配写在 `cmd/fasttask/main.go:39-105` 的 `serveCommand` 闭包里：顺序创建 `Store`、`auth.Service`、`application.App`、`httpapi.Server`，然后用裸 `go worker.Run(workerCtx)`（`main.go:84`）启动 Worker，用 `defer` 关闭 Scheduler。

这带来三个具体问题：

1. **`arch.md` §5.1 承诺的角色化命令没有兑现。** 文档列出 `fasttask worker` 和 `fasttask scheduler` 可独立启动，实际只有 `serve --with-worker --with-scheduler`。要做角色拆分就得把装配复制一遍。
2. **启停顺序是隐式的。** Worker 的取消、Scheduler 的 `Shutdown`、HTTP 的 `Shutdown` 分散在 `defer` 和 `select` 里，新增一个长生命周期组件就要重新推一遍顺序。
3. **`application.App` 是上帝对象。** `app.go` + `admin.go` + `lens.go` 共 50 余个方法挂在同一个 struct 上，横跨 `arch.md` §7 划分的全部八个领域模块。

第 3 点是关键：**如果不拆 `App`，引入 fx 的收益接近于零**——那只是把 `NewApp` 包成一个 provider，再为此付出反射式依赖图和运行期装配错误的代价。

Agent 运行时（见 [ADR-0003](0003-in-process-agent-loop.md)）会新增一批长生命周期组件：Run Manager、SSE 广播、工具注册表、Provider 解析器。这些组件把上面三个问题从「可以忍」推到「必须解决」。

## 2. 决策

采用 `go.uber.org/fx` v1.24.0 作为组合根，**并在同一次重构中按 `arch.md` §7 的领域模块拆分 `application.App`**。

四条硬规则：

1. **fx 只允许出现在 `internal/bootstrap` 和 `cmd`。** `domain`、`application`、`httpapi`、`persistence`、`agent` 一律不得 `import go.uber.org/fx`。所有构造函数保持普通 Go 函数签名 `NewXxx(deps...) (*Xxx, error)`，可以脱离 fx 直接调用。
2. **`domain` 和 `application` 的依赖方向不变**，仍遵守 `arch.md` §6.3。fx 改变的是「谁来调用构造函数」，不改变「谁可以依赖谁」。
3. **单元测试不使用 fx。** 直接构造服务。只有跨组件的集成测试用 `fxtest`。
4. **依赖图必须在启动时完整校验。** 每个角色命令都要有一个 `fx.ValidateApp` 测试，保证装配错误在 CI 而不是生产启动时暴露。

## 3. 理由

fx 在这里换来三件具体的东西，都是手写装配做起来别扭的：

- **`fx.Lifecycle`**：每个组件自己声明 `OnStart`/`OnStop`，fx 保证停机按启动的逆序执行。新增 Agent 运行时组件不需要回去改 `main.go` 的 `defer` 顺序。
- **`fx.Module` 组合角色进程**：`serve`、`worker`、`scheduler` 由同一批模块按需组合，不复制装配代码，兑现 `arch.md` §5.1。
- **值组（value group）**：HTTP 路由、Agent 工具、Job 处理器都可以由各自的领域模块自行注册，而不是在一个中心文件里枚举。`httpapi` 已经有 `registerGoals` / `registerPlans` / `registerJobs` 等 13 个分组函数（`server.go:214-1386`），天然对应值组。

## 4. 代价与反对意见

诚实记录反对理由，不掩饰：

- **反射式依赖解析与 `arch.md` §3.2「使用显式依赖注入」的字面表述有张力。** 裁决：该条的实际意图是「禁止包级全局数据库和客户端」，fx 完全满足这一点，且比手写装配更难退化出全局变量。文档措辞按本 ADR 同步调整。
- **装配错误从编译期移到启动期。** 由硬规则 4 的 `fx.ValidateApp` 测试兜底。
- **`App` 拆分是一次跨越 2500 余行的重构，有回归风险。** 由 [`doc/wiring.md`](../wiring.md) §7 的分阶段迁移约束：每搬一个服务跑一次 `go test ./...`，禁止一次性大爆炸式重构。
- **fx 的错误信息在依赖图复杂时可读性一般。** 可接受，规模不大。

## 5. 被否决的方案

| 方案 | 否决理由 |
|---|---|
| 只换组合根，不拆 `App` | 收益接近零，只付代价。见 §1 |
| 不引入 fx，手写 `bootstrap` 包 + 显式 lifecycle | 可行且更轻。否决理由是 Agent 运行时会持续新增长生命周期组件和需要自注册的工具，手写值组和启停顺序会重复造 fx 已有的轮子 |
| 一次性按 `arch.md` §6.1 重排整个 `internal/` 目录 | 改动面过大，回归风险与本次目标（装配 + 拆分）不成比例。目录重排留待后续独立 ADR |
