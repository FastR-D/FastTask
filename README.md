# FastTask / 3Signals

FastTask 是一个面向研究生和科研人员的长期目标推进系统。它把抽象目标持续拆成可执行任务树，并从活跃任务中为每天选择最多三个核心推进项，提供最小行动、专注计时、进度证据、对话式修正、墨水屏视图和 FastResearch Panel 摘要。

本仓库包含可直接运行的 Go 服务、React 管理界面、SQLite Migration、持久化 Agent Worker、调度维护任务、测试和部署样例。

## 核心能力

- 长期目标的创建、状态管理和 Revision 保护。
- 里程碑、任务、行动组成的多层任务树。
- 手工任务管理和 LLM 结构化任务树提案。
- 每日 `0..3` 个核心推进项，候选不足时不填充占位任务。
- `result`、`step`、`time`、`minimum_action` 四类当日推进证据。
- 当日计划项完成与底层任务完成严格分离。
- 番茄和自由专注 Session，时长由服务端计算。
- 文本对话、浏览器录音和异步转写作业。
- 持久化 Agent Job、Attempt、Lease、Fencing Token、重试和取消。
- 墨水屏独立只读 Token、ETag 和 `304 Not Modified`。
- FastResearch Panel 只读摘要接口。
- FastInsight、FastNews、FastRead、FastWrite 通用外部导入收件箱，支持来源去重、用户审批和任务转换。
- 统一后台管理平台：管理员用户、活跃会话、OpenAI-compatible 模型 Provider 和后续管理模块入口。
- 可选 FastRead/FastWrite 健康探测，不影响 FastTask Readiness。
- Gin + Huma v2，自动生成 OpenAPI 和 API 文档。
- SQLite WAL、版本化 SQL Migration、一致性备份和恢复验证。
- 响应式桌面/移动 Web 管理界面。

## 技术栈

- Go 1.24.1+
- Gin 1.11 + Huma v2.36
- GORM + SQLite WAL
- gocron v2
- JWT + Argon2id
- React + TypeScript + Vite
- Vitest + Go `testing` + `httptest`

## 目录

```text
cmd/fasttask/              CLI 和服务入口
internal/agent/            OpenAI-compatible LLM Adapter
internal/application/      用例、领域编排、Worker
internal/domain/           核心业务规则
internal/httpapi/          Gin + Huma HTTP 契约
internal/persistence/      GORM Record、SQLite、Migration
internal/platform/auth/    密码、JWT、Session 和 Refresh Token
internal/scheduler/        租约、Outbox 和清理维护任务
migrations/                发布用 SQL Migration
web/                       React 管理界面
scripts/e2e.mjs            真实 HTTP 用户场景测试
deployments/systemd/       systemd 样例
doc/                       架构、功能、接口和技术设计
```

## 快速开始

### 1. 安装依赖并构建前端

```bash
cd web
npm install
npm run build
cd ..
```

### 2. 启动服务

```bash
go run ./cmd/fasttask serve --with-worker --with-scheduler
```

默认监听：`http://127.0.0.1:10000`

首次启动会创建本地开发管理员：

```text
账号：admin
密码：fasttask-admin
```

该默认账号只用于本地开发。生产环境必须设置 `FASTTASK_ADMIN_PASSWORD` 和高强度 `FASTTASK_JWT_SECRET`。

### 3. 打开界面和 API 文档

- 管理界面：`http://127.0.0.1:10000/`
- API Docs：`http://127.0.0.1:10000/api/v1/docs`
- OpenAPI JSON：`http://127.0.0.1:10000/api/v1/openapi.json`
- Readiness：`http://127.0.0.1:10000/health/ready`

## 配置

服务会从环境变量读取配置，并在仓库根目录存在 `.env` 时加载尚未由进程环境设置的变量。`.env` 已被 `.gitignore` 排除。

常用变量：

| 变量 | 默认值 | 说明 |
|---|---|---|
| `FASTTASK_LISTEN` | `127.0.0.1` | 监听地址 |
| `FASTTASK_PORT` | `10000` | HTTP 端口 |
| `FASTTASK_PUBLIC_URL` | `http://127.0.0.1:10000` | OpenAPI 和 CORS 公共地址 |
| `FASTTASK_TRUSTED_PROXIES` | `10.22.33.0/24` | 逗号分隔的可信反向代理 IP/CIDR，仅这些来源可提供转发头 |
| `FASTTASK_DATABASE` | `data/fasttask.db` | SQLite 文件 |
| `FASTTASK_WEB_DIST` | `web/dist` | 前端生产构建目录 |
| `FASTTASK_AUDIO_DIR` | `data/audio` | 待转写音频的受限临时目录，作业完成后删除 |
| `FASTTASK_ENV` | `development` | 环境；`production` 会拒绝默认密钥 |
| `FASTTASK_JWT_SECRET` | 本地开发值 | JWT HMAC 密钥，生产至少 24 字符且不可使用默认值 |
| `FASTTASK_ADMIN_USER` | `admin` | 首次启动管理员账号 |
| `FASTTASK_ADMIN_PASSWORD` | `fasttask-admin` | 首次启动管理员密码 |
| `FASTTASK_ADMIN_NAME` | `FastTask Admin` | 管理员显示名 |
| `FASTTASK_PANEL_JWT_SECRET` | 与 JWT Secret 相同 | Panel/外部 Worker 短期 Service JWT 签名密钥 |
| `FASTTASK_FASTREAD_URL` | 空 | 可选 FastRead Base URL，例如直接后端 `http://127.0.0.1:8483` 或 Docker/Nginx `http://127.0.0.1:3015` |
| `FASTTASK_FASTWRITE_URL` | 空 | 可选 FastWrite Base URL，例如 `http://127.0.0.1:3003` |
| `FASTTASK_INTEGRATION_TIMEOUT_MS` | `2000` | 外部健康探测超时，范围 100 至 10000 毫秒 |
| `OPENAI_API_BASE_URL` | 空 | OpenAI-compatible `/v1` Base URL |
| `OPENAI_MODEL` | 空 | 模型名 |
| `OPENAI_API_KEY` | 空 | API Key |
| `OPENAI_TRANSCRIPTION_MODEL` | 空 | OpenAI-compatible 音频转写模型；为空时明确使用演示转写 |

LLM 未配置时，Worker 使用确定性本地 Provider，所有手工功能和测试仍可运行。LLM 配置完整时，任务树提案和对话回复使用真实模型；模型输出仍会经过 JSON 和领域校验，且任务树变更必须由用户确认。设置 `OPENAI_TRANSCRIPTION_MODEL` 后，语音作业会把真实音频发送到兼容的 `/audio/transcriptions` 接口；未设置时结果会明确标注为演示转写，不冒充真实识别。

环境变量只是本地启动和兜底配置。管理员可以在后台创建多个 OpenAI-compatible Provider，设置其中一个为默认；Worker 在每个 Agent Job 执行前解析默认配置，切换后无需重启。Provider API Key 使用服务端密钥通过 AES-GCM 加密入库，API 响应只返回掩码。

生产配置示例：

```bash
FASTTASK_ENV=production
FASTTASK_LISTEN=127.0.0.1
FASTTASK_PORT=10000
FASTTASK_PUBLIC_URL=https://task.example.com
FASTTASK_DATABASE=/var/lib/fasttask/fasttask.db
FASTTASK_WEB_DIST=/opt/fasttask/web/dist
FASTTASK_JWT_SECRET=<至少 32 字节随机密钥>
FASTTASK_ADMIN_PASSWORD=<高强度初始密码>
OPENAI_API_BASE_URL=https://provider.example/v1
OPENAI_MODEL=<model-name>
OPENAI_API_KEY=<secret>
OPENAI_TRANSCRIPTION_MODEL=<speech-to-text-model>
```

不要把真实密钥提交到 Git、写入日志或放入前端环境变量。

## CLI

```bash
go run ./cmd/fasttask serve
go run ./cmd/fasttask migrate
go run ./cmd/fasttask doctor
go run ./cmd/fasttask harden
go run ./cmd/fasttask backup --output backups/manual.db
go run ./cmd/fasttask version
```

`harden` 会生成新的生产 JWT/Panel Secret 和管理员密码，写入权限为 `0600` 的 `.env`，同步更新数据库管理员密码并撤销旧会话。命令不会把密钥或密码输出到终端；管理员密码可在服务器本机 `.env` 的 `FASTTASK_ADMIN_PASSWORD` 中查看。

生产构建：

```bash
mkdir -p bin
go build -trimpath -o bin/fasttask ./cmd/fasttask
```

## 测试

### 后端单元、集成和 HTTP 契约测试

```bash
go test ./...
go test -race ./...
go vet ./...
```

覆盖内容包括：

- 每日候选稳定排序和最多三个规则。
- 目标和任务状态转换。
- 任务树循环检测。
- 最小行动和时间完成不自动完成底层任务。
- 辅助任务必须在全部核心项满足后创建。
- 服务端 Session 时长和单活动 Session 约束。
- Agent Job 持久化、Lease 和提案保存。
- 真实 SQLite Migration、WAL、外键、事务回滚和数据库重开。
- SQLite 一致性备份和恢复读取。
- User、Device、Panel 认证边界和跨用户 `404`。
- ETag、`If-Match`、幂等重放、幂等键冲突和设备 `304`。
- 同日重规划 Revision、已满足核心名额保留和旧项 `superseded`。
- 计划关闭时未满足项转为 `not_completed`。
- 跨用户 Task/PlanItem 引用拒绝和直接伪造 `satisfied` 拒绝。
- Panel Service JWT Audience/Scope/代表用户 Claim 和 Agent Callback 租约栅栏。
- 真实音频内容传给 STT Adapter，并在作业完成后清理临时文件。
- 登录、目标、任务、计划、对话、语音、设备和 Panel 的 HTTP 闭环。
- OpenAPI 路径和 Security Scheme。

### 前端测试和生产构建

```bash
cd web
npm run test
npm run build
```

### 真实 LLM 测试

真实模型测试默认跳过，避免普通测试产生外部请求或费用。配置好 OpenAI-compatible 变量后执行：

```bash
set -a
source .env
set +a
FASTTASK_REAL_LLM_TEST=1 go test ./internal/agent -run TestRealConfiguredModel -count=1 -v
```

该测试要求模型返回合法的科研任务 JSON，并检查所有必需字段。

### 真实 HTTP 用户场景

服务运行后执行：

```bash
node scripts/e2e.mjs
```

脚本实际完成：登录、目标创建和幂等重放、LLM 任务树作业、提案确认、每日计划、最小行动完成、底层任务状态校验、同日重规划 Revision 2、对话 Agent、设备 Poll/304 和 Panel 摘要。

若测试服务不在 `10000` 端口：

```bash
FASTTASK_E2E_URL=http://127.0.0.1:10001 node scripts/e2e.mjs
```

## 关键业务语义

### 每日三个任务是上限

计划可以有零个、一个、两个或三个核心项。候选不足、任务阻塞或没有合法最小行动时，不创建无关占位任务。

### 当日满足不等于任务完成

完成最小行动、一个关键步骤或约定专注时间，只把 `DailyPlanItem` 标记为 `satisfied`。底层 `Task` 只有通过独立完成接口提交最终结果证据后才进入 `completed`。

### Agent 不直接修改业务数据

任务树生成保存为 Proposal。用户确认时，服务重新检查所有权、Tree Revision 和字段约束，然后在一个事务中应用。迟到或过期 Agent 结果不能覆盖新版本。

### 设备是独立只读身份

墨水屏 Token 仅在创建或轮换时返回一次，数据库只保存 SHA-256 摘要。设备只能读取当天最多三个核心项，不能调用用户写接口。

### 后台管理有独立授权边界

`/api/v1/admin/*` 只允许数据库中当前状态为 active、role 为 admin 的用户访问。用户禁用、密码重置和会话撤销会立即撤销刷新与访问会话；最后一名活跃管理员不能被降级或禁用。用户变更、Provider 变更和默认模型切换写入 `admin_audit_events`。

## 数据库和备份

SQLite 启用：

```text
PRAGMA journal_mode=WAL
PRAGMA foreign_keys=ON
PRAGMA busy_timeout=5000
PRAGMA synchronous=NORMAL
```

不要在服务运行时只复制主 DB 文件并忽略 WAL。请使用：

```bash
bin/fasttask backup --output backups/fasttask.db
```

备份命令使用 SQLite `VACUUM INTO` 生成一致性文件并执行 `PRAGMA integrity_check`。

## tmux 运行

```bash
tmux new-session -d -s fasttask \
  'cd /root/repo/FastTask && ./bin/fasttask serve --with-worker --with-scheduler'

tmux attach -t fasttask
tmux capture-pane -pt fasttask
tmux kill-session -t fasttask
```

## systemd

样例位于 `deployments/systemd/fasttask.service`。生产部署建议使用专用 `fasttask` 用户，配置 `EnvironmentFile`、最小文件权限、反向代理 TLS 和单实例写入约束。

## 当前外部边界

- LLM：已实现 OpenAI-compatible Chat Completions Adapter。
- 语音：MVP 已实现录音上传、受限临时存储、持久化转写 Job、OpenAI-compatible STT Adapter 和作业完成清理；没有 STT 模型配置时使用明确标注的演示转写。
- FastResearch Panel：提供只读摘要。服务身份使用短期 JWT，必须包含 `fasttask-panel-api` Audience、`panel:summary:read` Scope 和签名的代表用户 Claim。
- 墨水屏：后端 Poll 契约已完成，真实硬件固件不在本仓库范围内，可按 OpenAPI 接入。
- FastResearch 工具：已实现 `/api/v1/imports` 通用收件箱。用户或带 `imports:write` Scope 的服务可导入候选事项；用户可通过 ETag 审批后创建或关联 Task。带 `imports:read` Scope 的服务只能读取被代表用户的导入事项。
- 集成状态：管理员可调用 `GET /api/v1/integrations/status` 查看 FastRead/FastWrite 可选健康探测结果，并明确 FastInsight/FastNews CLI Runner 尚未实现。

## 设计文档

- `doc/arch.md`
- `doc/func.md`
- `doc/interface.md`
- `doc/tech.md`
- [`doc/integration/README.md`](doc/integration/README.md)：FastInsight、FastNews、FastRead、FastWrite 对接与协作总览

实现中的 HTTP DTO 和 OpenAPI 是字段级事实来源；文档用于解释产品语义、架构边界和演进决策。
