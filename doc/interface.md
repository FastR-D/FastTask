# FastTask 外部 HTTP 接口定义

> 文档状态：初版契约  
> API 版本：v1  
> 基础路径：`/api/v1`  
> 实现：Gin + Huma v2 `humagin`  
> OpenAPI：由 Huma 代码优先生成 OpenAPI 3.1；本文件描述业务契约和接口分组，Huma 定义为字段级事实来源

除 Health 小节明确列出的根路径接口外，本文端点表中的路径均相对于基础路径 `/api/v1`。例如表中的 `/goals` 表示实际路径 `/api/v1/goals`。

## 1. 协议约定

### 1.1 基本规则

- 除本地开发外必须使用 HTTPS。
- JSON 使用 UTF-8，字段名使用 `snake_case`。
- 路径使用复数资源和 `kebab-case`。
- ID 为不透明字符串，客户端不得解析内部结构。
- 时间使用 RFC 3339；业务日期使用 `YYYY-MM-DD`；时区使用 IANA 名称。
- 普通响应 `Content-Type: application/json`。
- 错误响应 `Content-Type: application/problem+json`。
- API 输入、输出、校验、Security Scheme 和 OpenAPI 文档由 Huma Operation 定义。

### 1.2 OpenAPI 和文档

建议提供：

```text
GET /api/v1/openapi.json
GET /api/v1/openapi.yaml
GET /api/v1/openapi-3.0.json
GET /api/v1/openapi-3.0.yaml
GET /api/v1/docs
GET /api/v1/schemas/...
```

生产环境可由反向代理限制 Docs UI，但 OpenAPI 仍应作为 CI 构建产物提供给其他 FastResearch 项目。

### 1.3 请求标识

客户端可发送：

```http
X-Request-ID: req_01J...
```

服务端缺省生成，并始终在响应返回相同 Header。错误体中的 `request_id` 与该值一致。

### 1.4 分页

列表接口使用游标分页：

| 参数 | 类型 | 默认 | 规则 |
|---|---|---|---|
| `limit` | integer | 20 | 1 至 100 |
| `cursor` | string | 空 | 不透明游标 |

响应：

```json
{
  "items": [],
  "next_cursor": null,
  "has_more": false
}
```

默认稳定排序为 `created_at DESC, id DESC`，特殊接口在端点处说明。

### 1.5 PATCH 语义

- 字段缺失表示不修改。
- 可空字段显式 `null` 表示清除。
- 不可空字段传 `null` 返回 `422`。
- 数组默认整体替换，子资源增删使用独立端点。

### 1.6 幂等

以下写操作支持或要求 `Idempotency-Key`：

- 创建目标、任务、计划、Session、设备和 Agent Job。
- 完成计划项和底层任务。
- 设备 Token 轮换。
- Refresh Token 轮换。
- P1 外部事项导入。

```http
Idempotency-Key: 8f2271b8-689c-4f50-bb7d-25b24de13d84
```

规则：

- 范围为认证主体、Method、规范化 Path 和 Key。Refresh 接口的幂等主体不是 Access Token 用户，而是服务端解析旧 Refresh Token 后得到的会话 ID/会话族 ID。
- 相同请求返回首次状态码和业务结果。
- 相同 Key 对应不同请求摘要返回 `409 IDEMPOTENCY_KEY_REUSED`。
- 重放响应可返回 `Idempotency-Replayed: true`。

### 1.7 Revision、ETag 和并发

可修改资源返回整数 `revision` 和强 ETag：

```http
ETag: "task_task01_rev_7"
```

修改现有资源必须发送：

```http
If-Match: "task_task01_rev_7"
```

- 缺少 `If-Match`：`428 Precondition Required`。
- ETag 过期：`412 Precondition Failed`。
- 成功后 Revision 加一并返回新 ETag。
- Agent Job 保存提交时的 `base_revision`，执行完成时不允许覆盖更新后的资源。

### 1.8 条件读取

墨水屏 Poll 必须支持 `If-None-Match`。内容未变化：

```http
HTTP/1.1 304 Not Modified
ETag: "device-view_dev01_rev_21"
Cache-Control: private, no-cache
```

`304` 无响应体。

## 2. 认证与授权

### 2.1 用户 Bearer

```yaml
userBearer:
  type: http
  scheme: bearer
  bearerFormat: JWT
```

```http
Authorization: Bearer <access_token>
```

用户 Token 至少包含 Subject、Issuer、Audience、IssuedAt、ExpiresAt 和 Token ID。客户端不能通过 Query 参数传 Token。

### 2.2 设备 Token

```yaml
deviceToken:
  type: apiKey
  in: header
  name: X-Device-Token
```

```http
X-Device-Token: <device_token>
```

设备 Token 只可调用设备接口，只绑定一个设备和用户，服务端仅保存哈希。

### 2.3 服务间认证

```yaml
serviceBearer:
  type: http
  scheme: bearer
  bearerFormat: JWT
```

建议 Scope：

| Scope | 用途 |
|---|---|
| `panel:summary:read` | FastResearch Panel 读取用户任务摘要 |
| `agent-jobs:write` | 独立 Agent Worker 回写结构化结果 |
| `imports:write` | 通用事项导入 |
| `imports:read` | 查询导入状态 |

服务 Token 使用独立 Audience 和短有效期，不隐式继承用户权限。

### 2.4 数据隔离

- 用户默认只能访问自己的资源。
- 请求其他用户资源统一返回 `404`，避免泄露资源存在性。
- 设备只能读取绑定用户的必要投影。
- Panel 只能读取服务 Token Scope 和被代表用户允许的摘要。

### 2.5 后台管理授权

后台管理接口在用户认证之上增加实时角色检查。用户生命周期与会话治理端点：

- `GET|POST /api/v1/admin/users`
- `GET|PATCH /api/v1/admin/users/{user_id}`
- `POST /api/v1/admin/users/{user_id}/password`
- `GET /api/v1/admin/users/{user_id}/sessions`
- `POST /api/v1/admin/users/{user_id}/session-revocation`

模型 Provider 管理端点：

- `GET|POST /api/v1/admin/model-providers`
- `PATCH|DELETE /api/v1/admin/model-providers/{provider_id}`
- `POST /api/v1/admin/model-providers/{provider_id}/activation`

Provider 响应不返回明文 API Key；激活操作使被激活记录成为唯一默认 Provider。最后一名 active admin 不能被降级或禁用。后台操作记录管理审计事件。

## 3. 错误协议

错误采用 RFC 9457 Problem Details：

```json
{
  "type": "https://fastresearch.example/problems/daily-core-limit",
  "title": "Daily core item limit exceeded",
  "status": 409,
  "detail": "A daily plan can contain at most three core items.",
  "instance": "/api/v1/daily-plans/plan_01/items",
  "code": "DAILY_CORE_LIMIT",
  "request_id": "req_01J...",
  "errors": [
    {
      "field": "kind",
      "code": "max_items",
      "message": "No more core items can be added."
    }
  ]
}
```

不得返回数据库原文、堆栈、服务器路径、模型密钥、Token 或其他用户信息。

### 3.1 通用状态码

| 状态码 | 含义 |
|---|---|
| 200 | 查询或同步修改成功 |
| 201 | 创建成功 |
| 202 | Agent 等异步操作已接受 |
| 204 | 成功且无响应体 |
| 304 | 条件读取无变化 |
| 400 | JSON、Header 或参数组合格式错误 |
| 401 | 未认证或凭据失效 |
| 403 | 缺少 Scope 或能力 |
| 404 | 资源不存在或当前主体不可见 |
| 409 | 业务状态、唯一约束或幂等冲突 |
| 412 | `If-Match` 版本过期 |
| 422 | 字段或业务语义校验失败 |
| 428 | 修改请求缺少 `If-Match` |
| 429 | 超出限流 |
| 500 | 未分类内部错误 |
| 503 | 服务或必要依赖未就绪 |

### 3.2 稳定错误码

| Code | HTTP | 说明 |
|---|---:|---|
| `VALIDATION_FAILED` | 422 | 参数校验失败 |
| `UNAUTHORIZED` | 401 | 无有效认证 |
| `INSUFFICIENT_SCOPE` | 403 | 服务 Scope 不足 |
| `RESOURCE_NOT_FOUND` | 404 | 资源不存在 |
| `RESOURCE_CONFLICT` | 409 | 当前状态不允许操作 |
| `IDEMPOTENCY_KEY_REUSED` | 409 | 幂等键对应不同请求 |
| `REVISION_MISMATCH` | 412 | Revision 冲突 |
| `PRECONDITION_REQUIRED` | 428 | 缺少 `If-Match` |
| `DAILY_PLAN_EXISTS` | 409 | 日期已有计划 |
| `DAILY_CORE_LIMIT` | 409 | 核心计划项超过三个 |
| `TASK_TREE_CYCLE` | 422 | 任务树或依赖形成循环 |
| `TASK_COMPLETION_INVALID` | 422 | 底层任务完成证据不合法 |
| `DAILY_ITEM_COMPLETION_INVALID` | 422 | 当日项完成证据不合法 |
| `WORK_SESSION_ACTIVE` | 409 | 已有活动 Session |
| `AGENT_JOB_NOT_CANCELLABLE` | 409 | Job 不可取消 |
| `STALE_AGENT_ATTEMPT` | 409 | Agent Attempt 或租约栅栏已过期 |
| `CORE_ITEMS_NOT_SATISFIED` | 409 | 核心项尚未全部满足，不能生成辅助项 |
| `DEVICE_TOKEN_REVOKED` | 401 | 设备 Token 已撤销 |
| `RATE_LIMITED` | 429 | 请求过多 |

## 4. 核心资源摘要

### 4.1 Goal

```json
{
  "id": "goal_01J...",
  "title": "完成论文初稿",
  "description": "形成可进入组内评审的论文初稿",
  "success_criteria": "结构完整并完成一次组内评审",
  "status": "active",
  "target_date": "2026-09-30",
  "revision": 4,
  "created_at": "2026-08-01T02:00:00Z",
  "updated_at": "2026-08-08T01:20:00Z"
}
```

### 4.2 Task

```json
{
  "id": "task_01J...",
  "goal_id": "goal_01J...",
  "parent_id": "task_parent_01J...",
  "type": "task",
  "title": "跑通第一组基线实验",
  "description": "使用默认参数完成端到端运行并保存日志",
  "status": "in_progress",
  "priority": 80,
  "position": 2,
  "estimate_minutes": 90,
  "success_criteria": "完成一次可复现的端到端运行",
  "minimum_action": "打开实验配置，确认数据路径和启动命令",
  "revision": 6
}
```

### 4.3 DailyPlanItem

```json
{
  "id": "dpi_01J...",
  "daily_plan_id": "plan_01J...",
  "task_id": "task_01J...",
  "kind": "core",
  "title": "跑通第一组基线实验",
  "commitment": "完成一次最小数据集启动并保存错误日志",
  "minimum_action": "打开配置并执行一次启动命令",
  "completion_policy": {
    "allowed_types": ["result", "step", "time", "minimum_action"],
    "target_minutes": 50
  },
  "status": "planned",
  "position": 1,
  "revision": 1
}
```

完成 DailyPlanItem 只表示满足当天约定，不自动将关联 Task 标记为 `completed`。

### 4.4 AgentJob

```json
{
  "id": "job_01J...",
  "type": "task_tree_revision",
  "status": "queued",
  "subject_type": "goal",
  "subject_id": "goal_01J...",
  "base_revision": 12,
  "progress": null,
  "result": null,
  "error": null,
  "revision": 1,
  "created_at": "2026-08-08T01:10:00Z"
}
```

## 5. Health

| 方法 | 路径 | 认证 | 成功 | 说明 |
|---|---|---|---|---|
| GET | `/health/live` | 无 | 200 | 进程存活 |
| GET | `/health/ready` | 无 | 200/503 | DB、Schema 和必要组件就绪 |
| GET | `/health/version` | 无 | 200 | 服务版本、Commit、构建时间 |

健康接口不返回依赖地址、配置或敏感错误。

Health 是基础路径的例外，实际位于服务根路径 `/health/*`，便于反向代理和进程管理器探测。

## 6. Auth 与 Me

### 6.1 Auth 端点

| 方法 | 路径 | 认证 | 响应 | 说明 |
|---|---|---|---|---|
| POST | `/auth/login` | 无 | 200 | 账号登录 |
| POST | `/auth/refresh` | 无；要求 `Idempotency-Key` | 200 | Refresh Token 轮换 |
| POST | `/auth/logout` | 用户 | 204 | 撤销当前会话 |
| GET | `/auth/session` | 用户 | 200 | 当前认证会话 |

登录请求：

```json
{
  "identifier": "zhangsan",
  "password": "********"
}
```

响应：

```json
{
  "token_type": "Bearer",
  "access_token": "<access_token>",
  "expires_in": 3600,
  "refresh_token": "<refresh_token>",
  "refresh_expires_in": 2592000,
  "user": {
    "id": "user_01J...",
    "display_name": "张三",
    "timezone": "Asia/Shanghai",
    "revision": 3
  }
}
```

有效期示例不是固定协议值。

认证安全规则：

- 密码使用 Argon2id 或经 ADR 选择的同等级自适应哈希，禁止明文和快速哈希。
- 登录按账号和来源 IP 组合限速，连续失败执行退避并记录审计。
- Refresh Token 仅保存哈希，成功使用后立即轮换。
- 已轮换旧 Refresh Token 在不同幂等键、不同请求摘要或幂等重放窗口外再次出现时，撤销对应会话族并要求重新登录；同一幂等键的合法响应重放不视为复用攻击。
- JWT 验证固定允许的签名算法、Issuer 和 Audience，禁止根据 Token Header 任意选择算法。
- 提供管理员撤销用户全部会话的运维能力；公开管理端点在管理员接口设计时单独定义。

### 6.2 Me

| 方法 | 路径 | 认证 | 说明 |
|---|---|---|---|
| GET | `/me` | 用户 | 当前用户 |
| PATCH | `/me` | 用户 + `If-Match` | 修改显示名、时区、Locale |

修改默认时区不改写历史 DailyPlan 的日期和时区。

## 7. Goals

| 方法 | 路径 | 认证 | 状态 | 说明 |
|---|---|---|---|---|
| GET | `/goals` | 用户 | 200 | 分页查询自己的目标 |
| POST | `/goals` | 用户，幂等 | 201 | 创建目标 |
| GET | `/goals/{goal_id}` | 用户 | 200 | 获取目标 |
| PATCH | `/goals/{goal_id}` | 用户 + `If-Match` | 200 | 修改、暂停、恢复、完成或归档 |

创建：

```json
{
  "title": "完成论文初稿",
  "description": "完成多 Agent 实验编排论文",
  "success_criteria": "形成可进入组内评审的完整初稿",
  "target_date": "2026-09-30"
}
```

规则：

- 创建目标不会默认同步等待 Agent 拆解。
- 可由客户端随后创建 Task Tree Generation Job。
- 将目标完成不自动完成其任务。
- 归档不删除历史计划、Session 或 Agent Job。
- 只有 `completed` 或 `abandoned` 目标可以归档；其他状态返回 `409 RESOURCE_CONFLICT`。

## 8. Task Tree

| 方法 | 路径 | 认证 | 状态 | 说明 |
|---|---|---|---|---|
| GET | `/goals/{goal_id}/task-tree` | 用户 | 200 | 获取嵌套任务树和聚合 Revision |
| POST | `/goals/{goal_id}/task-tree/generation-jobs` | 用户，幂等 | 202 | 异步生成初始任务树 |
| POST | `/goals/{goal_id}/task-tree/revision-jobs` | 用户 + Tree `If-Match`，幂等 | 202 | 根据指令生成修改提案 |
| GET | `/goals/{goal_id}/task-tree/revisions` | 用户 | 200 | 查询修改历史 |
| POST | `/goals/{goal_id}/task-tree/proposals/{proposal_id}/application` | 用户 + Tree `If-Match`，幂等 | 200 | 确认并应用提案 |
| PUT | `/goals/{goal_id}/task-tree/proposals/{proposal_id}/rejection` | 用户 + Proposal `If-Match` | 200 | 拒绝提案 |

生成请求：

```json
{
  "instruction": "拆成未来四周可执行的里程碑和任务，每个当前叶子任务给出最小行动",
  "constraints": {
    "available_minutes_per_day": 120
  }
}
```

返回：

```http
HTTP/1.1 202 Accepted
Location: /api/v1/agent-jobs/job_01J...
Retry-After: 2
```

修订请求：

```json
{
  "instruction": "实验部分先只保留基线，把消融实验放到基线完成之后",
  "conversation_id": "conv_01J..."
}
```

规则：

- Job 记录任务树 `base_revision`。
- Agent 返回结构化 Proposal，不直接写业务表。
- 提案应用时再次检查 Revision、父子关系、循环和历史保护。
- Revision 冲突时 Job 或应用操作失败，不静默覆盖。

## 9. Tasks

| 方法 | 路径 | 认证 | 说明 |
|---|---|---|---|
| GET | `/tasks` | 用户 | 扁平分页查询，可按 Goal、Parent、Status 过滤 |
| POST | `/tasks` | 用户，幂等 | 手工创建任务 |
| GET | `/tasks/{task_id}` | 用户 | 获取任务 |
| PATCH | `/tasks/{task_id}` | 用户 + `If-Match` | 修改、移动、暂停、阻塞、恢复或归档 |
| POST | `/tasks/{task_id}/completions` | 用户 + `If-Match`，幂等 | 完成底层任务 |
| POST | `/tasks/{task_id}/reopenings` | 用户 + `If-Match`，幂等 | 撤销完成并重新打开 |
| GET | `/tasks/{task_id}/progress-events` | 用户 | 查询进度证据 |

创建请求：

```json
{
  "goal_id": "goal_01J...",
  "parent_id": "task_parent_01J...",
  "type": "task",
  "title": "跑通第一组基线实验",
  "description": "使用默认配置完成一次端到端运行",
  "success_criteria": "得到一份可复现运行日志",
  "estimate_minutes": 90,
  "minimum_action": "打开实验配置并确认启动命令",
  "position": 2
}
```

底层任务完成请求：

```json
{
  "type": "result",
  "summary": "已生成基线结果表并保存可复现配置",
  "evidence": [
    {
      "kind": "url",
      "value": "https://example.invalid/experiment/123"
    }
  ],
  "completed_at": "2026-08-08T03:20:00Z"
}
```

规则：

- 不允许通过普通 `PATCH status=completed` 绕过完成证据。
- 移动任务不能形成父子循环。
- 完成最小行动或当日时间目标不能调用该接口伪装成底层任务完成。
- 重开任务不删除历史完成事件。

## 10. Daily Plans

### 10.1 业务规则

- 核心计划项数量为 0 至 3。
- 候选不足时允许少于三个或空计划。
- 同一天已满足的核心项仍占当天三个名额，不能追加第四个核心项。
- `support` 和 `input` 不占核心名额。
- 核心计划项必须关联真实 Task；辅助输入项可选关联 Task。
- 完成计划项不自动完成底层 Task。
- 未完成项次日重新评估并创建新 ID，不原样复制。

### 10.2 端点

| 方法 | 路径 | 认证 | 说明 |
|---|---|---|---|
| GET | `/daily-plans` | 用户 | 查询历史计划 |
| POST | `/daily-plans` | 用户，幂等 | 手工创建计划 |
| GET | `/daily-plans/current` | 用户 | 获取当前本地日期计划，不隐式创建 |
| GET | `/daily-plans/{plan_id}` | 用户 | 获取计划 |
| GET | `/daily-plans/{plan_id}/revisions` | 用户 | 查询同日重规划历史快照 |
| PATCH | `/daily-plans/{plan_id}` | 用户 + `If-Match` | 确认、关闭或取消计划 |
| POST | `/daily-plans/generation-jobs` | 用户，幂等 | 异步生成/重规划 |
| POST | `/daily-plans/{plan_id}/items` | 用户 + Plan `If-Match`，幂等 | 添加计划项 |
| GET | `/daily-plans/{plan_id}/items/{item_id}` | 用户 | 获取计划项 |
| PATCH | `/daily-plans/{plan_id}/items/{item_id}` | 用户 + Item `If-Match` | 修改承诺、最低行动、完成策略、排序或跳过；不可修改 `kind` |
| POST | `/daily-plans/{plan_id}/items/{item_id}/completions` | 用户 + Item `If-Match`，幂等 | 满足当日项 |
| POST | `/daily-plans/{plan_id}/support-generation-jobs` | 用户 + Plan `If-Match`，幂等 | 核心项全部满足后生成辅助项 |

生成请求：

```json
{
  "local_date": "2026-08-08",
  "timezone": "Asia/Shanghai",
  "instruction": "上午优先实验，阅读只作为辅助输入",
  "replace_existing": false,
  "available_minutes": 120
}
```

已有计划重规划时必须提供 `base_revision`：

```json
{
  "local_date": "2026-08-08",
  "timezone": "Asia/Shanghai",
  "instruction": "保留实验任务，把第二项拆得更小",
  "replace_existing": true,
  "base_revision": 5
}
```

创建计划项：

```json
{
  "task_id": "task_01J...",
  "kind": "core",
  "title": "跑通第一组基线实验",
  "commitment": "完成一次启动并保存错误日志",
  "minimum_action": "打开配置并执行一次命令",
  "completion_policy": {
    "allowed_types": ["result", "step", "time", "minimum_action"],
    "target_minutes": 50
  },
  "position": 1
}
```

已有三个核心项时返回 `409 DAILY_CORE_LIMIT`。

计划项 `kind` 创建后不可修改。需要改变类型时必须创建新计划 Revision。当天核心名额按“已满足核心项 + 当前 Revision 中未满足的有效核心项”计算；未满足且在重规划中转为 `superseded` 的核心项不再占当前名额，已满足核心项继续占用当天名额。

任何主体通过任何写路径添加 `support/input` 项前，都必须确认当前计划全部核心项已 `satisfied`，包括手工 `POST /items`、Agent 结果应用和外部导入，否则返回 `409 CORE_ITEMS_NOT_SATISFIED`。这落实“完成核心任务后再分配学习等辅助任务”的需求。

### 10.3 完成当日项

结果型：

```json
{
  "type": "result",
  "summary": "已生成基线实验结果表",
  "completed_at": "2026-08-08T03:20:00Z"
}
```

步骤型：

```json
{
  "type": "step",
  "summary": "已定位到失败原因是数据路径错误",
  "completed_at": "2026-08-08T03:20:00Z"
}
```

时间型：

```json
{
  "type": "time",
  "work_session_ids": ["session_01J..."],
  "note": "已投入两个番茄，尚未获得最终结果",
  "completed_at": "2026-08-08T03:20:00Z"
}
```

最小行动：

```json
{
  "type": "minimum_action",
  "summary": "已执行一次启动并保存完整报错日志",
  "completed_at": "2026-08-08T03:20:00Z"
}
```

响应中的计划项变为 `satisfied`，关联 Task 可继续为 `in_progress`。

## 11. Work Sessions

| 方法 | 路径 | 认证 | 说明 |
|---|---|---|---|
| GET | `/work-sessions` | 用户 | 按 Task、Plan Item、Status 和时间过滤 |
| POST | `/work-sessions` | 用户，幂等 | 开始 Session |
| GET | `/work-sessions/{session_id}` | 用户 | 获取 Session |
| POST | `/work-sessions/{session_id}/pauses` | 用户 + `If-Match`，幂等 | 暂停 |
| POST | `/work-sessions/{session_id}/resumptions` | 用户 + `If-Match`，幂等 | 继续 |
| POST | `/work-sessions/{session_id}/completions` | 用户 + `If-Match`，幂等 | 完成并计算有效时长 |
| POST | `/work-sessions/{session_id}/stoppings` | 用户 + `If-Match`，幂等 | 提前停止并保留实际时长 |
| POST | `/work-sessions/{session_id}/invalidations` | 用户 + `If-Match`，幂等 | 标记无效 |

开始：

```json
{
  "task_id": "task_01J...",
  "daily_plan_item_id": "dpi_01J...",
  "session_type": "pomodoro",
  "target_minutes": 25,
  "started_at": "2026-08-08T01:00:00Z"
}
```

完成：

```json
{
  "ended_at": "2026-08-08T01:25:10Z",
  "outcome": "progressed",
  "note": "完成配置检查并启动了一次实验"
}
```

规则：

- 同一用户默认只允许一个 `running` 或 `paused` Session。
- 服务端计算最终 `duration_seconds`。
- 已被时间型完成记录引用的 Session 修改后必须重新校验证据。
- 完成 Session 不自动满足计划项，客户端可随后提交计划项完成，或由应用规则在同一用例中完成。

## 12. Conversations

| 方法 | 路径 | 认证 | 说明 |
|---|---|---|---|
| GET | `/conversations` | 用户 | 查询对话 |
| POST | `/conversations` | 用户，幂等 | 创建对话 |
| GET | `/conversations/{conversation_id}` | 用户 | 获取对话元数据 |
| PATCH | `/conversations/{conversation_id}` | 用户 + `If-Match` | 修改标题或归档 |
| GET | `/conversations/{conversation_id}/messages` | 用户 | 分页获取消息 |
| POST | `/conversations/{conversation_id}/messages` | 用户，幂等 | 保存用户消息并返回 202 Agent Job |
| POST | `/voice-transcription-jobs` | 用户，幂等 | 上传语音并异步转写 |

消息请求：

```json
{
  "content": "这个任务太大，拆成今天可以开始的两步，不要改其他任务。",
  "client_message_id": "msg-client-8f2271b8"
}
```

返回 `202` 和 Job。Agent 回复写成 Assistant Message；如果包含资源修改，应返回结构化 Proposal，不能通过普通消息绕过 `If-Match`。

MVP 不要求 SSE、WebSocket 或 Token 流式输出。

语音转写使用 `multipart/form-data`，至少包含 `audio` 和可选 `locale`。支持的格式和大小由 Huma Operation 明确限制。返回 `202 AgentJob`；成功结果包含 `transcript`。用户编辑并确认后，再通过消息接口提交，可携带 `transcription_job_id` 形成审计关联。原始音频保留时间由配置和隐私策略决定。

## 13. Agent Jobs

| 方法 | 路径 | 认证 | 说明 |
|---|---|---|---|
| GET | `/agent-jobs` | 用户 | 查询当前用户 Job |
| GET | `/agent-jobs/{job_id}` | 用户 | Poll Job 状态 |
| PUT | `/agent-jobs/{job_id}/cancellation` | 用户 + Job `If-Match` | 请求取消 |
| POST | `/agent-jobs/{job_id}/retries` | 用户 + Job `If-Match`，幂等 | 基于失败 Job 创建新的 queued Job |
| POST | `/agent-jobs/{job_id}/callbacks` | 服务 `agent-jobs:write`，幂等 | 独立 Worker 回写结构化进度/结果 |

运行中：

```json
{
  "id": "job_01J...",
  "type": "task_tree_revision",
  "status": "running",
  "base_revision": 12,
  "progress": {
    "phase": "planning",
    "percent": 40
  },
  "revision": 3
}
```

版本冲突失败：

```json
{
  "id": "job_01J...",
  "status": "failed",
  "error": {
    "code": "REVISION_MISMATCH",
    "message": "The task tree changed before the result could be applied.",
    "retryable": false
  },
  "revision": 4
}
```

Job 内嵌错误不是 HTTP Problem；读取失败 Job 本身仍返回 `200`。

`REVISION_MISMATCH` 不能使用原 Job 自动重试。客户端应读取最新资源并创建新的 Generation/Revision Job。

其他可重试失败的手工重试也不改变原 Job 状态，而是创建带 `retry_of_job_id` 的新 Job 并返回 `202 Accepted`。只有运行中的临时错误自动重试才复用原 Job、增加 Attempt 并回到 `queued`。

独立 Worker 回调还必须提交 `attempt_no`、`lease_version` 和领取时签发的高熵 `run_token`。服务端仅在这些值与当前运行租约完全匹配时接受进度或结果；租约过期、Job 被重新领取或取消后，旧 Worker 回调返回 `409 STALE_AGENT_ATTEMPT`。`run_token` 不写日志，不在 Job 查询响应中返回。

## 14. Devices 与墨水屏 Poll

### 14.1 设备管理

| 方法 | 路径 | 认证 | 说明 |
|---|---|---|---|
| GET | `/devices` | 用户 | 查询设备 |
| POST | `/devices` | 用户，幂等 | 注册设备并一次性返回 Token |
| GET | `/devices/{device_id}` | 用户 | 获取设备 |
| PATCH | `/devices/{device_id}` | 用户 + `If-Match` | 修改名称、时区和显示能力 |
| PUT | `/devices/{device_id}/revocation` | 用户 + `If-Match` | 撤销设备和 Token |
| POST | `/devices/{device_id}/token-rotations` | 用户 + `If-Match`，幂等 | 轮换 Token |

注册：

```json
{
  "name": "办公室墨水屏",
  "kind": "eink_panel",
  "timezone": "Asia/Shanghai",
  "capabilities": {
    "width": 800,
    "height": 480,
    "color_mode": "monochrome"
  }
}
```

响应：

```json
{
  "device": {
    "id": "device_01J...",
    "name": "办公室墨水屏",
    "kind": "eink_panel",
    "status": "active",
    "revision": 1
  },
  "device_token": "<only_shown_once>"
}
```

返回 `Cache-Control: no-store`。Token 只展示一次。

### 14.2 Poll

```http
GET /api/v1/devices/self/poll
X-Device-Token: <device_token>
If-None-Match: "device-view_dev01_rev_21"
```

响应：

```json
{
  "device_id": "device_01J...",
  "local_date": "2026-08-08",
  "timezone": "Asia/Shanghai",
  "plan_status": "active",
  "plan_revision": 5,
  "generated_at": "2026-08-08T00:10:00Z",
  "updated_at": "2026-08-08T01:20:00Z",
  "core_items": [
    {
      "id": "dpi_1",
      "title": "跑通第一组基线实验",
      "minimum_action": "打开配置并执行一次启动命令",
      "status": "planned",
      "position": 1
    }
  ],
  "next_poll_after_seconds": 300
}
```

规则：

- `core_items` 最多三个。
- 无计划或空计划也返回 `200` 和空数组，不视为错误。
- Token 失效返回 `401`。
- 轮询过频返回 `429` 和 `Retry-After`。
- 响应不包含完整任务描述、对话或敏感证据。

## 15. FastResearch Panel

| 方法 | 路径 | 认证 | 说明 |
|---|---|---|---|
| GET | `/panel/summary` | 用户，或服务 `panel:summary:read` | 当前用户 FastTask 摘要 |
| POST | `/auth/panel-exchanges` | 服务认证，幂等 | 可选：将 Panel 短期票据交换为 FastTask 会话 |

摘要：

```json
{
  "local_date": "2026-08-08",
  "timezone": "Asia/Shanghai",
  "daily_plan_id": "plan_01J...",
  "plan_status": "active",
  "core_total": 3,
  "core_satisfied": 1,
  "current_item": {
    "id": "dpi_2",
    "title": "整理实验结果",
    "minimum_action": "整理三条失败原因"
  },
  "has_blockers": true,
  "entry_url": "/today",
  "updated_at": "2026-08-08T01:20:00Z"
}
```

服务认证代表用户时，具体用户绑定应来自受信任的 Subject Claim 或明确 Header 签名协议，不能接受普通客户端任意指定 `user_id`。

## 16. 通用外部导入

FastTask 已实现来源无关的外部导入收件箱。它不直接调用或复制其他工具的全部业务数据，而是保存稳定来源引用、任务所需摘要、Artifact 描述和 Metadata，并要求用户审批后再转换或关联 Task。

| 方法 | 路径 | 认证 | 说明 |
|---|---|---|---|
| POST | `/imports` | 用户，或服务 `imports:write`，幂等 | 导入候选事项或外部结果引用 |
| GET | `/imports` | 用户，或服务 `imports:read` | 按 Status、Source、Kind 或 External ID 游标分页查询导入 |
| GET | `/imports/{import_id}` | 用户，或服务 `imports:read` | 查询导入状态 |
| POST | `/imports/{import_id}/conversion` | 用户 + `If-Match`，幂等 | 创建新 Task 或关联已有 Task |
| PUT | `/imports/{import_id}/rejection` | 用户 + `If-Match`，幂等 | 拒绝候选事项 |
| GET | `/integrations/status` | 管理员用户 | 探测配置的 FastRead/FastWrite 服务状态 |

建议请求：

```json
{
  "schema_version": "1.0",
  "trace_id": "trace_01J...",
  "source": {
    "system": "fastread",
    "external_id": "paper_123",
    "url": "http://127.0.0.1:3015/?task_id=paper_123",
    "content_hash": "sha256:..."
  },
  "kind": "candidate_task",
  "title": "阅读并总结论文 X",
  "description": "重点判断其方法是否适合作为当前实验基线",
  "suggested_goal_id": "goal_01J...",
  "artifacts": [
    {
      "kind": "json",
      "uri": "file:///srv/fastread/note_results/paper_123.json",
      "content_hash": "sha256:..."
    }
  ],
  "metadata": {
    "page_count": 12
  }
}
```

规则：

- 导入默认创建候选事项，不直接进入每日三个核心项。
- 当前支持 `fastinsight`、`fastnews`、`fastread`、`fastwrite` 四个 `source.system`。
- `source.system + source.external_id` 在同一用户范围内唯一。
- `kind` 支持 `candidate_task`、`research_material`、`progress_evidence`、`review_issue`、`generated_report`。
- 创建导入要求 `Idempotency-Key`。HTTP 幂等不替代来源业务唯一键。
- FastTask 不复制外部系统全部数据，只保存稳定引用和完成任务所需摘要。
- 服务 Token 必须代表一个受信任用户；调用方不能提交任意 `user_id`。
- 服务 Token 使用稳定 `client_id` 区分 FastInsight、FastNews、FastRead、FastWrite 等调用方；服务幂等范围包含 `client_id`、代表用户和 Scope 集合。
- 服务 Token 只能导入或读取，不能执行转换和拒绝。
- 导入列表使用通用 `limit/cursor` 分页，默认 20 条，最多 100 条。
- 转换必须提交导入 ETag。创建新 Task 时必须提供可验证的 `success_criteria` 和 `minimum_action`；未提供 Goal 时可使用 `suggested_goal_id`。
- 也可通过 `existing_task_id` 将导入关联到已有 Task，但不会自动完成 Task 或写入 ProgressEvent。
- 转换和拒绝是终态，不能互相切换或回到 `candidate`。
- Artifact URI 只作为引用保存，FastTask 不会自动读取本地文件。
- 具体项目专用字段和双向同步仍需通过独立 ADR 和接口版本定义。

转换为新 Task：

```json
{
  "mode": "create",
  "type": "task",
  "title": "阅读并判断论文 X",
  "success_criteria": "形成带原文证据的基线适用性结论",
  "minimum_action": "阅读摘要和方法部分并记录三条判断",
  "priority": 80,
  "estimate_minutes": 50,
  "decision_note": "与当前实验方向相关"
}
```

关联已有 Task：

```json
{
  "mode": "attach",
  "existing_task_id": "task_01J...",
  "decision_note": "作为该任务的外部研究材料"
}
```

FastRead/FastWrite 健康探测由服务端环境变量配置，不接受请求参数中的 URL。未配置时返回 `configured=false`；不可用不会使 `/health/ready` 失败。FastInsight/FastNews 当前返回 CLI Runner 未配置。

## 17. 接口注册与测试要求

每个 Huma Operation 必须定义：

- 稳定唯一 `OperationID`。
- Method、Path、Summary、Description 和 Tags。
- 输入输出 DTO、示例和字段约束。
- Security Requirement。
- 可能的 Problem 状态码。
- 幂等、Revision 和副作用说明。

CI 必须：

1. 启动路由并导出 OpenAPI 3.1 和 3.0。
2. 校验 OpenAPI 格式。
3. 对关键接口执行 Huma/Gin `httptest` 契约测试。
4. 检查认证 Scheme、状态码和错误 Schema。
5. 对 OpenAPI 执行破坏性变更检查。

所有使用 `Idempotency-Key` 的业务写操作必须在同一个数据库事务中提交业务结果、幂等请求摘要和可重放响应快照。不得先提交业务数据再补写幂等记录。

## 18. 待确认接口项

- Panel 单点登录采用 JWT、一次性 Ticket 还是可信反向代理身份。
- 用户账号由 FastTask 管理还是 FastResearch 统一管理。
- 当前计划不存在时 `/daily-plans/current` 返回 404，还是可配置按需创建。本设计返回 404。
- Task Tree Proposal 是否需要独立列表和过期时间。
- 墨水屏设备 Token 默认有效期和轮询频率。
- Work Session 是否允许离线补录及其审计要求。
- P1 外部导入由用户 Token 还是服务 Token 代表最终所有者。
