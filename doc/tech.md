# FastTask 技术方案

> 文档状态：初版设计  
> 适用范围：后端服务、CLI、HTTP API、调度器、Agent Worker 和部署运维  
> 技术基线参考：`/root/repo/zhongxin-mng-bkd`

## 1. 技术目标

FastTask 初期部署在实验室 Mini 主机，并通过阿里云反向代理提供公网访问。技术方案优先保证：

1. 单机部署简单、数据可恢复。
2. 目标、任务、计划和进度写入具备事务一致性。
3. Agent 长任务在进程重启后可恢复、可重试、可取消。
4. Claude Code、Codex 等 Agent Harness 和模型可切换。
5. API 输入、输出、校验和 OpenAPI 来自同一代码定义。
6. 业务核心不依赖具体 Web 框架、ORM 或 Agent SDK。
7. 不机械复制参考项目中的特定业务依赖和架构缺陷。

## 2. 技术选型

### 2.1 核心依赖

| 技术 | 用途 | 选择说明 |
|---|---|---|
| Go 1.25.x | 后端、CLI、Worker | 继承参考项目基线，单二进制、并发和部署友好 |
| Cobra | CLI 命令 | 继承参考项目，用于服务、迁移、备份和诊断 |
| Gin | HTTP Engine、中间件、静态资源 | 继承参考项目及其生态 |
| `gin-contrib/cors` | CORS | 配置化来源白名单 |
| Huma v2 | Operation、DTO 校验、OpenAPI、Problem | 必须新增，消除接口实现和文档漂移 |
| `humagin` | Huma 与 Gin 适配 | 保持 Gin 技术栈并使用 Huma |
| GORM | SQLite 持久化适配 | 继承参考项目，但限制在 Persistence Adapter |
| SQLite WAL | 单机事务数据库 | 适合 Mini 主机和早期规模 |
| gocron v2 | 周期触发 | 继承参考项目，但不作为可靠任务状态存储 |
| `google/uuid` | 非机密业务 ID | 优先 UUIDv7，便于按时间局部排序 |
| `pkg/errors` | 兼容错误栈包装 | 仅在必要边界使用，分类以标准 `errors.Is/As` 为主 |
| logrus | 结构化日志 | 继承参考项目，统一 JSON Formatter |
| React + TypeScript + Vite | 响应式管理界面 | 支持任务树、对话、计时和移动端；客户端由 OpenAPI 生成 |
| `go.uber.org/fx` | 组合根、依赖装配、生命周期 | 见 [ADR-0001](adr/0001-fx-composition-root.md)；只允许出现在 `internal/bootstrap` 和 `cmd` |
| `@assistant-ui/react` | Agent 对话界面与工具审批 | 无样式 primitives，见 [ADR-0002](adr/0002-assistant-transport.md)、[`doc/frontend.md`](frontend.md) |
| assistant-transport 协议 | Agent 前后端传输 | 服务端持有权威状态；Go 侧自实现编码器，见 [`doc/agent-impl.md`](agent-impl.md) §2 |
| mdui | Material You 组件与响应式布局 | 基于 Lit 的 Web Components，无官方 React 封装，依赖 React 19 的自定义元素支持 |
| `vite-plugin-pwa` | PWA manifest 与 Service Worker | 见 [`doc/pwa.md`](pwa.md) |

依赖版本由 `go.mod` 和 `go.sum` 固定，不在构建脚本中使用 `@latest`。Huma、Gin、Go 和 SQLite Driver 作为联动升级组验证。

### 2.2 Huma 能力使用

Huma v2 用于：

- 基于 Go Struct 定义 Path、Query、Header 和 Body。
- 使用 `enum`、`minimum`、`maxLength` 等标签生成 JSON Schema 并校验。
- 由 Operation 自动生成 OpenAPI 3.1 和兼容 OpenAPI 3.0.3。
- 自动提供 Docs 和 Schema。
- 定义 Bearer/API Key Security Scheme。
- 统一错误和响应模型。
- 使用 Conditional Request 工具辅助处理墨水屏 `If-None-Match`，以及资源 `If-Match`。

注意：Huma 不会自动读取数据库 Revision 或完成并发控制。应用仍需读取当前 ETag/时间、调用 Huma Conditional 工具判断前置条件，并在数据库使用条件更新。Huma 的结构校验也不替代领域校验；任务树循环、每日三个核心项和完成语义必须在 Domain/Application 层校验。

### 2.3 当前不引入的参考项目技术

#### MSSQL / SQL Server Driver

参考项目用 MSSQL 同步机器日志，FastTask 当前没有远程 SQL Server 业务需求，因此不引入 `gorm.io/driver/sqlserver` 和 `go-mssqldb`。未来若有企业数据库联动，应通过独立 Connector 和 ADR 引入。

#### Excelize

参考项目用于工资 Excel 导出，FastTask 当前无明确 Excel 导入导出需求，因此不引入。出现具体文件契约后再评估公式注入、文件大小和兼容性。

#### 微信集成

FastTask 不是微信小程序配套后端，当前无微信登录或订阅消息需求，因此不继承微信 Token、模板 ID 和 Client。未来通知通过通用 `Notifier` Port 扩展。

## 3. Go Module 与包管理

建议 Module 使用仓库路径，例如：

```go
module github.com/FastR-D/FastTask
```

不要沿用参考项目的短 Module 名 `zhongxin`。完整仓库路径更便于发布、迁移、定位和外部工具生成客户端。

依赖管理要求：

- 提交 `go.mod` 和 `go.sum`。
- 禁止 `replace` 指向个人本地路径。
- 新依赖说明用途、维护状态和许可证。
- Agent CLI 版本不进入 Go Module，而在部署清单和 `doctor` 中固定、检查。
- 定期执行 `govulncheck`。

验证命令：

```bash
go mod tidy
go mod graph
go list -m all
go test ./...
go vet ./...
```

## 4. 工程目录

```text
.
├── cmd/fasttask/main.go
├── internal/
│   ├── bootstrap/
│   ├── config/
│   ├── domain/
│   ├── application/
│   ├── ports/
│   ├── adapters/
│   │   ├── http/
│   │   ├── persistence/sqlite/
│   │   ├── agent/
│   │   ├── scheduler/
│   │   ├── outbox/
│   │   ├── panel/
│   │   └── speech/
│   └── platform/
├── migrations/
├── deployments/
│   ├── systemd/
│   └── proxy/
├── test/integration/
├── web/
├── doc/
├── go.mod
└── go.sum
```

规则：

- `domain` 只包含实体、值对象、状态机、领域服务和 Repository 接口。
- `application` 实现用例、事务和授权，不感知 HTTP。
- `adapters/http` 只包含 Huma DTO、Operation、Mapper 和错误映射。
- `adapters/persistence/sqlite` 是唯一允许直接使用 GORM 的业务包。
- `adapters/agent` 是具体 Harness 的防腐层。
- 不建立无边界的 `util`、`common` 或 `helpers` 大包。

`web` 使用 React、TypeScript 和 Vite，提供登录、今日计划、目标/任务树、对话/语音、番茄、进度和设备设置。通过 OpenAPI 生成 TypeScript Client，禁止手写另一套接口类型。构建产物可由 FastTask 静态托管，也可由反向代理独立托管。

## 5. 应用构造与生命周期

### 5.1 显式依赖注入

禁止：

```go
var DB *gorm.DB
var Conf *Config
var Scheduler gocron.Scheduler
var HTTPClient *http.Client
```

运行态对象由 Bootstrap 显式构造。推荐顺序：

```text
CLI Flags
  -> Config Load + Validate
  -> Logger
  -> SQLite + Schema Check
  -> Repositories + Transactor
  -> HTTP Clients + Agent Registry
  -> Application Services
  -> Huma API + Gin Engine
  -> Worker + Scheduler
  -> HTTP Server
```

这修复参考项目中业务 Scheduler 早于数据库初始化的隐患。

### 5.2 关闭顺序

1. Readiness 变为失败。
2. HTTP Server 停止接收新请求。
3. Scheduler 停止产生新任务。
4. Worker 停止领取新 Job。
5. 等待运行任务在宽限期内结束。
6. 取消超时的外部调用和子进程。
7. Flush 日志和指标。
8. 关闭 SQLite。

底层包返回错误，不调用 `os.Exit` 或 `log.Fatal`。由 Cobra 命令决定退出码。

## 6. Cobra 命令

```text
fasttask
├── serve
├── worker
├── scheduler
├── migrate
│   ├── up
│   ├── down
│   └── status
├── backup
├── restore
├── doctor
├── agent-job
│   ├── inspect
│   ├── retry
│   └── cancel
├── admin
├── config validate
└── version
```

要求：

- `serve` 默认不执行破坏性迁移。
- Schema 不兼容时拒绝启动。
- `doctor` 检查 DB、目录权限、磁盘、时区、代理连接、Agent CLI 路径和版本。
- `restore` 默认要求服务停止。
- CLI 正常机器可读结果输出 stdout，错误输出 stderr。

## 7. Gin 与 Huma

### 7.1 职责划分

Gin 负责：

- HTTP Engine。
- Request ID、Access Log、Recovery 和 CORS。
- Trusted Proxies。
- 静态前端资源。
- 少数必须使用底层 Gin 能力的例外端点。

Huma 负责：

- API Operation 注册。
- 输入绑定和结构校验。
- 输出 Schema。
- OpenAPI、Docs 和 JSON Schema。
- Security Scheme 声明。
- 标准化 API Error。

推荐请求链路：

```text
Gin Middleware
  -> Huma Adapter
  -> Auth Principal
  -> Huma Validation
  -> Operation Handler
  -> Application Use Case
  -> Domain/Repository
```

### 7.2 统一 Operation 注册

项目应封装 Operation 注册，统一：

- `OperationID`、Tags、Summary 和 Description。
- User、Device、Service 认证。
- Problem Details。
- Idempotency-Key。
- ETag/If-Match。
- Request ID 和审计信息。

禁止路由定义散落并遗漏 Security 或错误文档。

### 7.3 DTO、Domain、Record 分离

```text
HTTP DTO
  -> Application Command/Result
  -> Domain Entity/Value Object
  -> Persistence Record
```

DTO 可以使用 `json`、`doc`、`enum`、`format`、`minimum` 等 Huma 标签，但不包含 GORM 标签。Persistence Record 不作为 API 输出，Domain 不包含 HTTP 或数据库标签。

对于 PATCH，使用指针或专用 Optional 类型区分缺失、显式零值和 `null`，避免参考项目依赖反射零值判断而无法清空字段的问题。

### 7.4 OpenAPI 防漂移

Huma Operation 是契约事实来源。CI：

1. 构建路由。
2. 导出 OpenAPI 3.1 和 3.0。
3. 校验格式和 Security Scheme。
4. 执行 Breaking Change Diff。
5. 将结果作为客户端生成和发布制品。

手写文档只补充业务语义，不维护另一份字段级 OpenAPI。

管理前端 CI 同时执行 TypeScript 类型检查、单元测试和桌面/移动关键页面构建；核心 E2E 覆盖对话、任务树、今日计划、计时和语音确认流程。

## 8. 配置管理

### 8.1 来源优先级

```text
CLI Flag > 环境变量 > 配置文件 > 安全默认值
```

为减少依赖，可使用严格 JSON 配置。配置加载必须：

- 返回不可变值对象。
- 拒绝未知字段。
- 严格校验 URL、时区、路径、Duration 和端口。
- 出错时拒绝启动。
- 不自动重写用户配置文件。
- 输出时脱敏。

### 8.2 示例配置

```json
{
  "server": {
    "listen": "127.0.0.1",
    "port": 2233,
    "public_url": "https://task.example.com",
    "trusted_proxies": ["127.0.0.1"],
    "cors_origins": ["https://task.example.com"]
  },
  "database": {
    "path": "/var/lib/fasttask/fasttask.db",
    "busy_timeout": "5s",
    "synchronous": "NORMAL"
  },
  "worker": {
    "concurrency": 2,
    "lease_duration": "5m",
    "heartbeat_interval": "30s",
    "shutdown_grace_period": "60s"
  },
  "scheduler": {
    "timezone": "Asia/Shanghai"
  },
  "providers": {
    "claude_code": {
      "enabled": true,
      "binary": "/usr/local/bin/claude",
      "timeout": "30m"
    },
    "codex": {
      "enabled": true,
      "binary": "/usr/local/bin/codex",
      "timeout": "30m"
    }
  }
}
```

### 8.3 密钥

优先使用 systemd `EnvironmentFile` 或权限 `0600` 的 Secret 文件。密钥不得提交到 Git、写入日志、OpenAPI 示例、Agent Job Metadata 或普通配置响应。

## 9. SQLite 与 GORM

### 9.1 适用范围

SQLite 适合单机、小团队、中低写入并发和简单备份。一个主库只允许一个 Active 服务实例写入，不能通过 NFS/SMB 共享给多主机。

### 9.2 Pragma

启动时设置并验证：

```text
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;
PRAGMA synchronous = NORMAL;
```

高可靠部署可评估 `synchronous=FULL`。必须确认 Pragma 对连接池中的每个连接生效。

连接池初始值保守配置，例如：

```go
sqlDB.SetMaxOpenConns(4)
sqlDB.SetMaxIdleConns(4)
sqlDB.SetConnMaxLifetime(0)
```

SQLite 同时只有一个 Writer，增加连接数不等于增加写吞吐。出现 Busy 时优先缩短事务、调整 Worker 并发和连接数。

### 9.3 Migration

生产禁止 `AutoMigrate`。使用版本化 SQL：

```text
migrations/
  000001_init.up.sql
  000001_init.down.sql
  000002_add_agent_job_lease.up.sql
  000002_add_agent_job_lease.down.sql
```

要求：

- 已发布 Migration 不修改。
- 保存版本、名称、应用时间和校验和。
- CI 从空数据库完整执行。
- 发布前备份。
- `serve` 检查兼容版本。
- 迁移失败不继续启动。

### 9.4 Transaction

Application 层通过 Transactor 定义原子操作：

```go
type Transactor interface {
    WithinTransaction(ctx context.Context, fn func(context.Context) error) error
}
```

要求：

- 事务对应一个业务原子操作。
- 事务内所有 Repository 使用同一事务句柄。
- 业务写、幂等请求摘要和可重放响应快照在同一事务提交。
- 不在事务内执行 Agent、HTTP 或长文件操作。
- 状态更新使用 Revision/状态条件更新。
- 提交成功后才返回成功响应。

## 10. Agent Provider 与 Harness

### 10.1 抽象

需求明确要求底层 Agent/模型可切换。当前 OpenAI-compatible Adapter 支持对话、任务树提案和可选音频转写；通用 Runner 目标接口示例：

```go
type Runner interface {
    Name() string
    Validate(ctx context.Context) error
    Run(ctx context.Context, req RunRequest, sink EventSink) (RunResult, error)
    Cancel(ctx context.Context, runID string) error
}
```

`RunRequest` 至少包含 Job ID、Job Type、版本化输入、模型选择、超时、输出 Schema 和安全工作目录。`RunResult` 返回结构化输出、Provider Run ID、使用模型和时间信息。

Provider 不负责：

- 修改业务状态。
- 决定权限和重试。
- 访问 Huma DTO。
- 直接写 Repository。
- 保存或输出密钥。

Worker 在执行每个 Job 前解析当前默认 Provider；后台切换无需重启。默认 Provider 来自数据库配置，未配置时回退到 OpenAI-compatible 环境变量，最后回退到确定性本地 Provider。

### 10.2 Claude Code/Codex 适配

可以优先使用官方 SDK；需要 CLI 时：

- 使用 `exec.CommandContext`，不使用 `sh -c` 拼接。
- 固定二进制路径和已验证版本范围。
- Prompt 通过标准输入或官方安全协议传递。
- 用户输入不能成为任意 CLI 参数。
- 环境变量使用白名单。
- 工作目录必须限制在允许根目录并检查符号链接。
- 捕获并限制 stdout/stderr 大小。
- Context 取消时终止整个子进程组。
- 结构化输出先做 Schema 校验。

两个 Adapter 保留各自参数、沙箱和错误差异，不强行做通用命令模板。

### 10.3 Agent 输出规则

- 所有规划输出使用版本化 JSON Schema。
- 保存原始输出摘要和解析错误，敏感内容脱敏。
- 输出不能直接持久化为正式任务树。
- Application 检查 Subject Revision、权限和领域规则后应用。
- 不合法输出最多进行有限的修复性重试。

## 11. 可靠异步作业

### 11.1 Job 表

`agent_jobs` 保存：

```text
id, type, status, priority, retry_of_job_id,
subject_type, subject_id, subject_revision,
input_json, output_json, schema_version,
agent_name, provider_name, model_name,
attempt_count, max_attempts, run_after,
locked_by, locked_until, lease_version, run_token_hash,
idempotency_key, last_error,
created_at, started_at, finished_at, updated_at
```

### 11.2 领取与租约

SQLite 不支持 `SKIP LOCKED`，使用短 `BEGIN IMMEDIATE` 事务和条件更新领取：

```text
查找 queued 且 run_after <= now
  -> UPDATE status=running, locked_by, locked_until
  -> affected rows == 1 则领取成功
  -> COMMIT
```

每次领取递增 `lease_version` 并签发本 Attempt 专用高熵 `run_token`，数据库只保存其哈希。进度、完成和回调必须匹配 Job、Attempt、Lease Version、Run Token 和当前状态，形成 fencing barrier。外部 Agent 调用发生在事务外。Worker 定期续租；崩溃后由租约扫描恢复，旧 Worker 的迟到结果被拒绝。

### 11.3 重试分类

可重试：网络超时、Provider 429、临时模型错误、进程资源不足、可修复 Schema 错误。

不可自动重试：输入校验、权限、配置错误、未知 Provider、用户取消、版本冲突、工作目录越权。

退避：

```text
delay = min(base * 2^attempt + jitter, max_delay)
```

Jitter 可使用非安全随机数；认证 Token 必须使用 `crypto/rand`。

### 11.4 取消

取消是持久化意图：API 标记取消请求，Worker 取消 Context 和子进程，最终写入 `cancelled`。迟到结果不得覆盖已取消 Job 或新 Revision。

### 11.5 Outbox

Outbox 用于 Panel 摘要更新、未来通知和跨项目事件。业务写和 Outbox Event 同事务提交；消费者至少一次处理，按 Event ID 幂等。

## 12. HTTP Client

外部 HTTP Client 按服务注入并复用连接，不使用任意包可修改的全局 Client。

Transport 至少配置：

- Dial Timeout。
- TLS Handshake Timeout。
- Response Header Timeout。
- Idle Connection Timeout。
- Max Idle Connections。
- Request Context Deadline。

短 API 和长 Agent 请求使用不同时间预算。默认不重试非幂等请求；POST 仅在有幂等协议时重试；处理 `Retry-After`；禁止无限重试和记录含密钥的完整 URL。

## 13. 认证、安全与权限

### 13.1 Token

认证凭据使用 `crypto/rand`：

```go
buf := make([]byte, 32)
_, err := cryptorand.Read(buf)
token := base64.RawURLEncoding.EncodeToString(buf)
```

禁止使用 `math/rand`、时间戳或 UUID 直接作为长期认证 Token。

高熵不透明 Token 在 DB 保存 SHA-256 摘要。JWT 签名密钥保存在 Secret 文件或环境变量，不进入普通配置。

本地账号密码使用 Argon2id 或经 ADR 选择的同等级自适应哈希。登录按账号和来源 IP 组合限速并执行失败退避。Refresh Token 只保存哈希且每次使用后轮换；检测到旧 Token 复用时撤销会话族。JWT 验证固定算法白名单、Issuer 和 Audience。

Refresh 接口要求 `Idempotency-Key`。轮换事务同时保存旧 Token 哈希、替代 Token 关系和短期加密的响应快照；同一旧 Token、同一幂等键和相同请求在短重试窗口内重放原响应，不触发复用告警。不同幂等键、不同请求摘要或超过窗口后再次使用旧 Token，才按复用攻击撤销会话族。加密快照使用独立密钥、严格 TTL，禁止日志记录并定期清理。

### 13.2 认证主体

- User Principal：用户 Web/API。
- Device Principal：只读墨水屏。
- Service Principal：Panel 或独立 Worker，按 Scope 限权。

HTTP Middleware 负责认证和粗粒度保护，Application Service 仍执行资源级授权。

### 13.3 CORS

生产使用明确白名单，不启用 `AllowAllOrigins`。如果未来使用 Cookie，需要单独设计 CSRF、SameSite 和 Credentials；当前 Bearer Header 模式不默认允许 Credentials。

### 13.4 Agent 安全

- Agent 输出不可信。
- Prompt 中避免注入密钥和非必要私人数据。
- Provider 工作目录和工具权限遵循最小权限。
- Agent Service 不持有 FastTask 数据库直接写权限。
- 记录模型和配置版本，便于审计。

## 14. 错误模型

使用标准 `errors.Is/As` 分类，HTTP 层映射 Problem Details。

| 场景 | HTTP |
|---|---:|
| JSON/Header 格式 | 400 |
| 未认证 | 401 |
| Scope/权限不足 | 403 |
| 不存在或不可见 | 404 |
| 状态、唯一性、幂等冲突 | 409 |
| Revision 过期 | 412 |
| 字段或领域语义无效 | 422 |
| 缺少 If-Match | 428 |
| 限流 | 429 |
| 内部错误 | 500 |
| 必要依赖不可用 | 503 |
| 上游超时 | 504 |

客户端依赖稳定 `code`，不依赖中文 `detail`。数据库错误、堆栈和 Provider 原始敏感错误不返回客户端。

Recovery 使用 `fmt.Sprint(recovered)` 安全处理任意 Panic 值，只注册一层应用 Recovery，避免参考项目的重复 Recovery 和类型断言二次 Panic。

## 15. 时间与时区

- 数据库存储 UTC 时间。
- API 使用 RFC 3339。
- 每日计划保存用户 IANA 时区和 Local Date。
- 禁止依赖宿主机隐式 `time.Local`。
- 使用可注入 `Clock` 测试日界线、租约和重试。
- 静态构建内置 `time/tzdata`。
- Mini 主机启用 NTP。

## 16. 日志、指标与追踪

### 16.1 日志

logrus 使用 JSON Formatter。关键字段：

```text
service, version, instance_id,
request_id, operation_id,
goal_id, job_id, provider,
duration_ms, error_code
```

不记录 Token、密码、密钥、完整 Prompt、默认完整任务正文或音频。Provider stdout/stderr 写受控文件，日志只保留截断摘要。

### 16.2 指标

最低建议：

```text
fasttask_http_requests_total
fasttask_http_request_duration_seconds
fasttask_agent_jobs_total
fasttask_agent_job_duration_seconds
fasttask_agent_job_retries_total
fasttask_agent_queue_depth
fasttask_outbox_pending
fasttask_sqlite_busy_total
fasttask_daily_plan_generation_total
fasttask_backup_last_success_timestamp
```

指标标签使用 Method、Route Template、Status、Provider、Result 等低基数字段，不使用 User ID、Task ID、Prompt 或完整错误。

### 16.3 追踪

MVP 必须实现 Request ID、Job ID 和 Provider Run ID 贯穿日志。完整 OpenTelemetry 在观测平台确定后引入，Exporter 故障不能影响业务。

## 17. HTTP Server

必须设置：

```text
ReadHeaderTimeout
ReadTimeout
WriteTimeout
IdleTimeout
MaxHeaderBytes
```

优雅关闭宽限期不少于 10 秒，Agent Worker 使用独立、更长的关闭策略。Readiness 在关闭开始时立即失败。

健康端点：

```text
GET /health/live
GET /health/ready
GET /health/version
```

## 18. 测试策略

### 18.1 单元测试

覆盖：

- 目标、任务、计划和 Job 状态机。
- 每日候选过滤、稳定排序和最多三个规则。
- 当日项与底层任务完成语义。
- 未完成任务重新评估。
- Revision、幂等和错误映射。
- 配置、时区、重试和 Token。
- DTO/Domain/Record Mapper。

### 18.2 Repository 集成测试

使用临时目录真实 SQLite 文件，不只使用 `:memory:`。覆盖：

- 全部 Migration。
- 外键、唯一约束和事务回滚。
- WAL、Busy Timeout 和数据库重开。
- 并发 Job 领取、租约恢复和乐观锁。
- 幂等记录和 Outbox。
- 备份恢复。

### 18.3 HTTP 契约测试

使用 `httptest` 启动 Gin + Huma，覆盖：

- 输入绑定、未知字段、Query 和 Header 校验。
- User/Device/Service 认证。
- ETag/If-Match 和 304。
- 状态码和 Problem Details。
- OpenAPI 路径、Security Scheme 和 Schema。

### 18.4 Agent Adapter 测试

使用 Fake Runner 或假可执行文件模拟成功、非零退出、超时、取消、忽略 SIGTERM、输出超限和损坏 JSON。真实 Claude Code/Codex 烟雾测试只在受控环境运行，不进入默认 PR 流程。

### 18.5 E2E

至少覆盖：

1. 登录并创建目标。
2. Fake Agent 生成任务树。
3. 生成最多三个日计划项。
4. 完成最小行动，验证 Task 未完成。
5. 完成底层 Task，验证进度事件。
6. 墨水屏 Poll 和 304。
7. 服务重启后 Job、计划和历史仍存在。
8. Worker 崩溃后租约恢复。
9. 备份、恢复并检查一致性。
10. 语音上传、异步转写、用户确认文本并提交对话。
11. 桌面和移动视口完成今日计划、任务树和计时主流程。

### 18.6 CI 命令

```bash
gofmt -w .
go test ./...
go test -race ./...
go vet ./...
go build ./...
```

建议增加 `staticcheck`、`govulncheck`、OpenAPI Breaking Change、Migration From Empty 和 Backup-Restore Test。CI 检查格式差异，不静默修改提交。

## 19. 构建与 SQLite CGO

`gorm.io/driver/sqlite` 默认依赖 `mattn/go-sqlite3`，需要 CGO。不能直接用 `CGO_ENABLED=0` 得到当前驱动可用二进制。

Linux AMD64 静态构建示例：

```bash
CGO_ENABLED=1 \
GOOS=linux \
GOARCH=amd64 \
CC=x86_64-linux-musl-gcc \
CGO_LDFLAGS="-static" \
go build \
  -trimpath \
  -tags "osusergo netgo sqlite_omit_load_extension" \
  -ldflags '-linkmode external -extldflags "-static" -s -w' \
  -o bin/fasttask-linux-amd64 \
  ./cmd/fasttask
```

构建后验证：

```bash
file bin/fasttask-linux-amd64
ldd bin/fasttask-linux-amd64
```

Mini 主机若为 ARM64，应使用 `GOARCH=arm64` 和匹配的 musl Toolchain，或在 ARM64 Runner 构建。CI 必须实际打开 SQLite、迁移、写入和读取，不只检查编译。

## 20. CI/CD

### 20.1 CI

```text
格式检查
-> 依赖下载
-> 单元/集成/Race Test
-> Vet/Staticcheck/Vulnerability Scan
-> OpenAPI 导出和校验
-> Migration Test
-> 静态构建
-> 二进制 SQLite 烟雾测试
-> 制品校验和
```

### 20.2 构建信息

通过 `-ldflags` 注入 Version、Commit、BuildTime。`fasttask version` 输出 Go、OS/Arch、CGO、SQLite Driver 和 Agent CLI 版本。

### 20.3 发布

1. 生成不可变制品和 SHA-256。
2. Mini 主机验证校验和。
3. 进入维护或停止接收新写请求。
4. 执行 SQLite 一致性备份。
5. 执行 `fasttask migrate up`。
6. 切换版本并重启 systemd。
7. 检查 Health、版本、API 和 Provider 烟雾测试。

## 21. 部署

### 21.1 拓扑

```text
Internet
  -> 阿里云 ECS / 反向代理
       - TLS
       - 域名
       - 请求体限制
       - 基础限流
  -> WireGuard / frp / 受控反向隧道
  -> 实验室 Mini 主机
       - FastTask systemd
       - SQLite
       - Agent CLI/SDK
       - 数据、Artifact、日志、备份
```

FastTask 默认监听 `127.0.0.1:2233` 或专用隧道地址，不在无防火墙条件下监听公网 `0.0.0.0`。

### 21.2 systemd

建议使用专用 `fasttask` 用户。关键安全项：

```text
Restart=on-failure
TimeoutStopSec=60s
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/fasttask
```

Agent 对 Workspace 的访问需要单独测试，优先让 Agent 使用受限运行用户或沙箱，而不是扩大整个 FastTask 服务权限。

### 21.3 反向代理头

Gin 只信任明确代理地址。不得无条件信任任意 `X-Forwarded-For`、`X-Forwarded-Proto` 和 `X-Real-IP`。安全回调 URL 从配置的 Public URL 构造。

## 22. 备份与恢复

不能在运行中只复制 `fasttask.db` 而忽略 WAL。使用 SQLite Online Backup API、`VACUUM INTO` 或受控 Checkpoint 生成一致性快照。

备份流程：

1. 检查磁盘空间。
2. 生成一致性快照。
3. 执行 `PRAGMA integrity_check`。
4. 记录应用和 Schema 版本。
5. 压缩、加密并计算 SHA-256。
6. 上传 OSS 或其他异地存储。
7. 验证可读并更新成功指标。

初始建议：本地小时级保留 24 至 48 份，异地每日保留 30 天，每次迁移前额外备份。最终 RPO/RTO 由数据价值确认。

恢复必须停服务、校验备份、在临时目录恢复并做完整性检查，再原子替换 DB，清理不匹配 WAL/SHM，启动后执行租约恢复和功能验证。每月至少做一次自动恢复演练。

## 23. 性能与容量

初期假设：

```text
用户：实验室内部几十人
HTTP 并发：数十到数百
Agent Worker 并发：1 至 4
单 Agent Job：默认不超过 30 分钟，可配置
任务和计划数据：中小规模
```

性能优化顺序：

1. 增加必要索引和查询投影。
2. 缩短 SQLite 写事务。
3. 限制 Worker 并发和输出大小。
4. 对历史数据归档。
5. 只有出现真实多机、高写入或高可用需求时迁移 PostgreSQL。

不为未知规模提前引入分布式缓存和消息系统。

## 24. 技术风险

| 风险 | 缓解 |
|---|---|
| SQLite 单 Writer 竞争 | 短事务、保守连接池、限制 Worker、Busy 指标 |
| Agent 响应慢或不稳定 | 持久化 Job、租约、重试、超时、人工重试 |
| Agent 输出不合法 | 版本化 Schema、领域校验、有限修复重试 |
| Agent 覆盖用户修改 | Subject Revision 和 Proposal 确认 |
| Mini 主机断网或宕机 | 本地持久化、恢复扫描、异地备份、systemd |
| Token 泄漏 | HTTPS、哈希存储、轮换、Scope、日志脱敏 |
| OpenAPI 与实现漂移 | Huma 代码优先、CI 导出和 Breaking Diff |
| CGO 静态构建失败 | musl Toolchain、目标架构 Runner、二进制烟雾测试 |
| 时区日界线错误 | IANA 时区、UTC 存储、可注入 Clock、边界测试 |
| Panel 身份不明确 | 独立 Service Audience，协议确定前不信任自报用户 ID |

## 25. ADR 清单

实现前后应维护以下 Architecture Decision Record：

1. 模块化单体与 SQLite 的选择。
2. Gin + Huma v2 的组合方式。
3. 版本化 SQL Migration 工具。
4. 用户和 Panel 统一认证协议。
5. Claude Code/Codex 使用 SDK 还是 CLI。
6. Agent Job 租约和重试策略。
7. 日计划确定性排序算法版本。
8. 语音识别 Provider。
9. Mini 主机到阿里云的隧道方案。
10. 何时从 SQLite 迁移 PostgreSQL。

## 26. 开发完成定义

一个后端功能只有同时满足以下条件才算完成：

- Domain 规则和事务边界明确。
- Huma Operation、Schema、Security 和错误已定义。
- DTO、Domain、Record 没有混用。
- 单元、Repository 集成和 HTTP 契约测试通过。
- OpenAPI 已生成且无未评审破坏性变化。
- 日志无敏感信息，错误状态码准确。
- Migration 可从空库执行。
- 构建、Race Test、Vet 和漏洞扫描通过。
- 涉及异步操作时，重启、重试、取消和版本冲突路径已验证。
