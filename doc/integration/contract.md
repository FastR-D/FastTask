# FastResearch 通用对接契约草案

> 状态：核心导入、查询、转换和拒绝契约已实现；CLI Runner 和远端作业适配仍为建议  
> 目标：为 CLI、HTTP 和文件型工具提供最小统一边界，不替代各工具的领域 API

## 1. 设计原则

- FastTask 只保存完成任务所需的摘要、稳定引用和产物校验信息，不复制外部系统全部数据。
- `source.system + source.external_id` 是外部对象的业务幂等键。
- 原始内容仍由来源工具负责；FastTask 保存引用和必要快照。
- 导入默认进入 `candidate` 状态，不能直接修改每日计划或论文正文。
- 所有契约携带 `schema_version`，不依赖未版本化的隐式字段。
- HTTP 调用使用 `X-Request-ID`；跨阶段沿用同一 `trace_id`。
- 服务 JWT 应包含稳定 `client_id`、代表用户和最小 Scope，服务幂等键在该身份范围内生效。
- 可重试必须显式标记，避免对非幂等长任务盲目重放。

## 2. 外部引用

建议 FastTask `POST /api/v1/imports` 接受：

```json
{
  "schema_version": "1.0",
  "trace_id": "trace_01J...",
  "source": {
    "system": "fastread",
    "external_id": "paper-task-uuid",
    "url": "http://127.0.0.1:3015/?task_id=paper-task-uuid",
    "content_hash": "sha256:..."
  },
  "kind": "candidate_task",
  "title": "阅读并判断论文 X",
  "description": "重点判断方法是否适合作为当前实验基线",
  "suggested_goal_id": "goal_01J...",
  "artifacts": [
    {
      "kind": "json",
      "uri": "file:///root/repo/FRD/FastRead/backend/note_results/task.json",
      "content_hash": "sha256:..."
    }
  ],
  "metadata": {
    "direction": "ML/AI Security",
    "paper_url": "https://example.org/paper"
  }
}
```

建议 `kind` 初始集合：

| kind | 语义 |
|---|---|
| `candidate_task` | 等待用户确认的任务候选 |
| `research_material` | 阅读、趋势或引用材料 |
| `progress_evidence` | 可关联现有任务的进度证据 |
| `review_issue` | 写作审稿或质量问题 |
| `generated_report` | 新闻、顶会或研究报告引用 |

建议响应：

```json
{
  "id": "import_01J...",
  "status": "candidate",
  "source": {
    "system": "fastread",
    "external_id": "paper-task-uuid"
  },
  "task_id": null,
  "created_at": "2026-08-09T08:00:00Z"
}
```

## 3. CLI 作业描述

FastInsight 和 FastNews 没有任务 API，建议 FastTask 内部统一保存：

```json
{
  "schema_version": "1.0",
  "job_type": "fastnews.conference.summarize",
  "trace_id": "trace_01J...",
  "working_directory": "/root/repo/FRD/FastNews",
  "command": [
    "uv",
    "run",
    "python",
    "top-conf/generate_conf_summary.py",
    "usenix",
    "2026"
  ],
  "timeout_seconds": 3600,
  "inputs": [
    "top-conf/data/conferences/usenix_2026.jsonl"
  ],
  "expected_outputs": [
    "top-conf/data/summary/usenix_2026_summary.jsonl"
  ]
}
```

运行结果至少保存：

```json
{
  "status": "succeeded",
  "exit_code": 0,
  "started_at": "2026-08-09T08:00:00Z",
  "finished_at": "2026-08-09T08:04:12Z",
  "stdout_excerpt": "...",
  "stderr_excerpt": "...",
  "artifacts": [
    {
      "uri": "file:///.../usenix_2026_summary.jsonl",
      "content_hash": "sha256:...",
      "media_type": "application/x-ndjson",
      "record_count": 160
    }
  ],
  "error": null
}
```

成功不能只依赖退出码。Runner 还应验证：

- 预期产物存在且位于允许目录。
- JSON/JSONL 可解析。
- 必填字段和记录数满足最低要求。
- 日志不存在已知的“捕获异常但退出 0”信号。
- 产物修改时间和内容哈希对应本次运行。

## 4. HTTP 长任务适配

FastRead 和 FastWrite 当前没有统一异步协议，FastTask Adapter 应保存：

| 字段 | 说明 |
|---|---|
| `remote_system` | `fastread` 或 `fastwrite` |
| `remote_job_id` | FastRead `task_id`、FastWrite `run.id/plan.id` |
| `operation` | 导入、报告、核验、Agent、Review 等 |
| `request_hash` | 规范化请求摘要 |
| `status` | FastTask 映射后的状态 |
| `last_remote_status` | 远端原始状态 |
| `next_poll_at` | 下一次轮询时间 |
| `deadline_at` | 总超时 |
| `attempt_no` | FastTask 侧调用次数 |
| `result_ref` | 外部结果引用 |

统一状态建议：

```text
queued -> running -> awaiting_approval -> succeeded
                  -> failed
                  -> timed_out
queued/running/awaiting_approval -> cancelled
```

FastWrite 的 Agent Plan 和 ChangeSet 应映射为 `awaiting_approval`，不能映射为成功。

## 5. 错误契约

建议统一错误对象：

```json
{
  "code": "REMOTE_VALIDATION_FAILED",
  "message": "FastRead rejected the paper URL",
  "retryable": false,
  "remote_system": "fastread",
  "remote_status": 422,
  "remote_code": "validation_error",
  "details": {},
  "occurred_at": "2026-08-09T08:00:00Z"
}
```

建议错误分类：

| code | 默认是否重试 | 说明 |
|---|---:|---|
| `REMOTE_UNAVAILABLE` | 是 | 连接失败、服务未启动 |
| `REMOTE_TIMEOUT` | 谨慎 | 请求超时，需先查询是否已创建远端任务 |
| `REMOTE_RATE_LIMITED` | 是 | 遵守 `Retry-After` |
| `REMOTE_VALIDATION_FAILED` | 否 | 输入或领域校验失败 |
| `REMOTE_AUTH_FAILED` | 否 | 服务身份失效或 Scope 不足 |
| `REMOTE_CONFLICT` | 否 | 文件版本、项目版本或状态冲突 |
| `REMOTE_OUTPUT_INVALID` | 否 | JSON、字段或产物校验失败 |
| `REMOTE_DEPENDENCY_FAILED` | 视情况 | LLM、搜索、LaTeX、抓取站点失败 |
| `REMOTE_CANCELLED` | 否 | 用户或系统取消 |

## 6. 论文和证据映射

建议跨工具使用以下最小论文对象：

```json
{
  "schema_version": "1.0",
  "title": "...",
  "authors": ["..."],
  "year": 2026,
  "venue": "...",
  "abstract": "...",
  "url": "...",
  "pdf_url": "...",
  "doi": "...",
  "arxiv_id": "...",
  "source_system": "fastnews",
  "source_external_id": "..."
}
```

建议证据对象：

```json
{
  "source_system": "fastread",
  "source_external_id": "task-uuid",
  "source_url": "https://example.org/paper.pdf",
  "content_hash": "sha256:...",
  "page_start": 4,
  "page_end": 4,
  "exact_quote": "...",
  "verification_status": "source_only",
  "retrieved_at": "2026-08-09T08:00:00Z"
}
```

FastInsight 的 `match_score` 和 FastNews 的 LLM 摘要不能直接标记为论文事实证据。进入 FastWrite 正文前应经 FastRead 原文核对或人工确认。

## 7. 安全约束

- 服务凭据只保存在服务端配置或密钥管理中。
- FastRead/FastWrite 未增加鉴权前，不允许通过公网直接调用。
- 文件 URI 必须经过允许目录校验，禁止任意路径读取。
- CLI 参数使用参数数组执行，不拼接 Shell 字符串。
- 日志和错误详情不得包含 API Key、Cookie、飞书 open_id 名册全文或 GitHub Token。
- 外部 HTML、摘要和 Prompt 输入均视为不可信内容。

## 8. 版本与兼容

- `schema_version` 使用 `major.minor`。
- 新增可选字段只提升 minor。
- 删除字段、改变类型或改变状态语义必须提升 major。
- 适配器应记录远端仓库 Commit 或应用版本，方便定位契约漂移。
- 没有 schema version 的现有产物按 `legacy` 处理，并在适配层显式转换。
