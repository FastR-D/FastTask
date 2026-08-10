# FastWrite 对接说明

> 项目：`/root/repo/FRD/FastWrite`  
> 状态：项目、文件、Agent、编译和源码 Review API 已实现；鉴权、异步回调和真实 PDF Review 未实现

## 1. 定位与事实来源

FastWrite 是本地优先的 LaTeX AI 写作工作台。核心原则：

- Workspace 文件是论文正文真相。
- Completion、Agent、Revise、Review、Memory 和 CompileRecord 是派生状态。
- Agent 不应静默修改正文，而是先生成 Plan 和 ChangeSet。
- 用户应逐 hunk 接受、拒绝或编辑变更。
- 文件通过 `baseVersion` 做乐观并发控制。

## 2. 服务和运行

安装与开发：

```bash
bun install
bun run dev
```

默认地址：

| 服务 | 地址 |
|---|---|
| 开发 Web | `http://127.0.0.1:3002` |
| API/生产 Web | `http://127.0.0.1:3003` |

健康检查：

```http
GET /api/health
```

```json
{
  "status": "ok"
}
```

构建和测试：

```bash
bun run typecheck
bun run test
bun run build
bun run e2e:smoke
```

默认仅监听 `127.0.0.1`。当前 API 无 Token、会话或 RBAC，不应直接暴露公网。

## 3. HTTP 约定

JSON 请求必须发送：

```http
Content-Type: application/json
```

错误格式：

```json
{
  "error": {
    "code": "version_conflict",
    "message": "The file changed since it was opened",
    "details": {}
  }
}
```

常见状态：

| 状态 | 语义 |
|---:|---|
| `400` | 字段、路径或状态无效 |
| `404` | 项目、文件、Plan、ChangeSet 不存在 |
| `409` | 文件版本、状态机或编译门禁冲突 |
| `413` | 文件、上传或候选过大 |
| `499` | 客户端取消 AI 请求 |
| `502` | AI、GitHub 或 TeX 上游失败 |
| `503` | AI 或本地 LaTeX 未配置 |
| `504` | AI 超时 |

## 4. 项目和文件 API

### 4.1 创建项目

```http
POST /api/projects
```

```json
{
  "name": "论文名称",
  "mainDocument": "main.tex",
  "venue": "security-top4"
}
```

响应 `201 PaperProject`，关键字段包括 `id`、`mainDocument`、`version`、`skill` 和 `source`。

### 4.2 查询项目和文件

```http
GET /api/projects/{projectId}
GET /api/projects/{projectId}/files
GET /api/projects/{projectId}/file?path=main.tex
```

文本文件响应：

```json
{
  "file": {
    "path": "main.tex",
    "version": 3,
    "kind": "text"
  },
  "content": "..."
}
```

### 4.3 保存文件

```http
PUT /api/projects/{projectId}/file?path=main.tex
```

```json
{
  "content": "...",
  "baseVersion": 3
}
```

响应：

```json
{
  "file": {
    "path": "main.tex",
    "version": 4
  },
  "projectVersion": 8
}
```

版本冲突返回 `409 version_conflict`。FastTask 不能无条件覆盖，必须重新读取并进入人工合并。

### 4.4 创建研究材料文件

```http
POST /api/projects/{projectId}/files
```

```json
{
  "path": "research-notes/paper-x.md",
  "content": "..."
}
```

建议 FastRead/FastNews 产物先写入普通 `.md`、`.txt` 或 `.bib` 文件，不直接写 `main.tex`。

## 5. Agent 和审批 API

### 5.1 创建 Plan

```http
POST /api/projects/{projectId}/agent-tasks
```

```json
{
  "objective": "/revise 明确威胁模型并补充证据边界",
  "scope": {
    "type": "project"
  },
  "issueIds": []
}
```

响应：

```json
{
  "run": {},
  "plan": {},
  "resolution": null
}
```

此时尚未修改 Workspace。

### 5.2 确认 Plan

```http
POST /api/projects/{projectId}/agent-tasks/{planId}/confirm
```

响应包含 `changeSet`。生成候选仍不代表全部变更已批准。

### 5.3 决定 hunk

```http
POST /api/projects/{projectId}/change-sets/{changeSetId}/decide
```

```json
{
  "decisions": [
    {
      "path": "main.tex",
      "hunkIds": ["hunk-1"],
      "status": "accepted"
    }
  ]
}
```

所有 hunk 已决定后：

```http
POST /api/projects/{projectId}/change-sets/{changeSetId}/finish
```

并发冲突返回 `409 changeset_conflict_review_required`，必须人工确认覆盖或重新生成。

运行状态可读取：

```http
GET /api/projects/{projectId}/agent-runs
GET /api/projects/{projectId}/agent-tasks
```

## 6. 编译和 Review API

### 6.1 服务端编译

```http
POST /api/projects/{projectId}/compile
```

响应：

```json
{
  "success": true,
  "engine": "server",
  "log": "...",
  "pdfBase64": "...",
  "syncTexData": "...",
  "workspacePaths": ["main.tex"]
}
```

依赖宿主机 `latexmk` 或 `pdflatex`。该接口不会自动创建 CompileRecord。

### 6.2 登记编译结果

```http
POST /api/projects/{projectId}/compile-results
```

```json
{
  "projectVersion": 8,
  "status": "success",
  "summary": "Compiled successfully"
}
```

调用方必须确保 `projectVersion` 对应实际编译输入。当前接口不要求 PDF 哈希或产物证明，不能把 CompileRecord 当作强审计证据。

查询：

```http
GET /api/projects/{projectId}/compile-results/latest
```

### 6.3 Review

```http
POST /api/projects/{projectId}/reviews
```

```json
{
  "sourceOnly": false
}
```

响应包含 `run`、`snapshot` 和结构化 `report.issues[]`。当前项目版本没有成功 CompileRecord 时，通常返回 `409 compile_required`。

重要现状：Review Provider 当前读取源码，不读取固定 PDF，也没有 PDF 页码证据。`sourceOnly=false` 不能改变这一实现边界。

## 7. 配置与持久化

关键变量：

| 变量 | 用途 |
|---|---|
| `FASTWRITE_PORT` | API/生产 Web 端口，默认 `3003` |
| `FASTWRITE_DATA_DIR` | 数据目录 |
| `FASTWRITE_WEB_PORT` | 开发 Web 端口，默认 `3002` |
| `OPENAI_API_KEY` | 全局 AI Key |
| `OPENAI_BASE_URL` | OpenAI-compatible Base URL |
| `FASTWRITE_OPENAI_MODEL` | 全局模型 |
| `FASTWRITE_<WORKFLOW>_*` | Completion/Agent/Revise/Review/Memory 独立配置 |
| `FASTWRITE_AGENT_TIMEOUT_MS` | AI 操作超时 |
| `FASTWRITE_REVIEW_TIMEOUT_MS` | Review 超时 |
| `FASTWRITE_GITHUB_TOKEN` | GitHub 导入和同步 |

默认数据：

```text
.fastwrite-data/database.json
.fastwrite-data/projects/<projectId>/workspace/
.fastwrite-data/projects/<projectId>/history.git/
.fastwrite-data/projects/<projectId>/trash/
```

发布包默认使用 `paperdata/`。

外部工具不得直接修改 `database.json` 或 Workspace 文件。直接写 Workspace 会绕过文件版本、项目版本、Memory freshness 和内部 checkpoint。

## 8. FastTask 接入建议

推荐状态映射：

| FastWrite | FastTask |
|---|---|
| Project | Goal 的外部项目引用 |
| AgentTaskPlan proposed | 待审批事项 |
| ChangeSet reviewing | 待审批事项 |
| ChangeSet accepted/finished | 外部结果引用；需要任务进度时通过现有完成用例提交证据 |
| CompileRecord success | 编译证据引用 |
| ReviewIssue | 缺陷或修订 Task |
| IssueResolution | 修订任务进度 |

推荐流程：

1. FastTask 创建或关联 FastWrite Project。
2. 将 FastRead 证据写入 `research-notes/`，将规范引用写入 `.bib`。
3. 创建 AgentTaskPlan，将 `planId` 保存为外部引用。
4. 用户确认后调用 `/confirm`。
5. 将 `changeSetId` 映射为 FastTask 审批项。
6. 逐 hunk 决策并 `/finish`。
7. 读取当前 `projectVersion`，执行编译并登记 CompileRecord。
8. 创建 Review，将 ReviewIssue 转成 FastTask 修订任务。

FastTask 不应自动接受 ChangeSet，也不应把 HTTP `201` 误判为写作任务完成。

## 9. 风险和限制

1. API 无鉴权，端口暴露后可读写论文并消耗 LLM/GitHub 凭据。
2. `/compile-results` 缺少真实产物证明，可登记虚假成功。
3. 多文件 ChangeSet 缺少完整事务，失败时可能部分应用。
4. `database.json` 无跨进程锁，多个实例共享目录会产生竞争。
5. 长 AI 操作使用同步 HTTP 且没有幂等键，超时重试可能重复执行。
6. Review 和 targeted re-review 当前是源码审查，不是 PDF 审查。
7. 当前没有 OpenAPI、API 版本、Webhook 或跨工具 `external_id`。

## 10. 关键位置

- `FastWrite/README.md`
- `FastWrite/docs/RDA.md`
- `FastWrite/docs/DESIGN.md`
- `FastWrite/docs/DEV.md`
- `FastWrite/apps/server/src/app.ts`
- `FastWrite/apps/server/src/server.ts`
- `FastWrite/apps/server/src/config.ts`
- `FastWrite/apps/server/src/http.ts`
- `FastWrite/apps/server/src/workspace/workspace-service.ts`
- `FastWrite/apps/server/src/agent/agent-task-service.ts`
- `FastWrite/apps/server/src/agent/revise-service.ts`
- `FastWrite/apps/server/src/agent/review-service.ts`
- `FastWrite/apps/server/src/storage/database.ts`
- `FastWrite/packages/shared/src/models.ts`
