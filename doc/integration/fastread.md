# FastRead 对接说明

> 项目：`/root/repo/FRD/FastRead`  
> 状态：论文导入、阅读报告、问答和核验 API 已实现；服务鉴权和可靠任务队列未实现

## 1. 定位与证据边界

FastRead 是单篇论文的证据化阅读工作台：

```text
PDF/论文 URL
  -> 分页原文和内容哈希
  -> 学术身份 Gate
  -> 关键问题阅读报告
  -> 服务端逐字校验页码引文
  -> 个人总结和持续问答
  -> 可选联网核验
```

FastRead 应作为 FastResearch 链路中的全文和页码证据来源。FastInsight 的趋势摘要、FastNews 的 LLM 摘要不能替代 FastRead 的分页原文。

当前 `academic_gate` 是身份证据分类和风险提示，不是生成阅读报告的阻断式门禁。Gate 未通过时，报告仍可能生成并在 `limitations` 中提示，因此调用方不能以“报告已生成”推导论文身份已确认。

## 2. 服务和运行

默认地址：

| 服务 | 地址 |
|---|---|
| Web | `http://127.0.0.1:3015` |
| API，直接后端/本地开发 | `http://127.0.0.1:8483/api` |
| API，Docker/Nginx | `http://127.0.0.1:3015/api` |

健康检查：

```http
GET /api/sys_check
GET /api/sys_health
```

本地 Windows 推荐入口：

```powershell
.\run.bat
```

手动后端：

```bash
cd backend
python main.py
```

Docker：

```bash
docker compose up -d --build
```

Docker Compose 默认只把 Nginx 的 `3015` 映射到宿主机，后端 `8483` 仅在 Compose 网络内暴露。宿主机调用 Docker 部署时应使用 `http://127.0.0.1:3015/api`，除非另行增加后端端口映射。

测试：

```bash
python -m pytest
```

## 3. 通用 HTTP 约定

常见成功响应：

```json
{
  "code": 0,
  "msg": "success",
  "data": {}
}
```

调用方还必须兼容：

- FastAPI/Pydantic 的 HTTP `422` 和 `detail` 数组。
- `HTTPException` 的 `{"detail":"..."}`。
- 业务错误 envelope 的 `code/msg/data`。

当前没有 Bearer Token、API Key、用户会话或 RBAC。论文业务接口主要依赖网络可达性。上传和部分管理接口只允许 loopback；不要通过 `ALLOW_NON_LOCAL_ADMIN=true` 代替正式鉴权。

## 4. 论文导入 API

### 4.1 URL 导入

```http
POST /api/papers/from_url
Content-Type: application/json
```

最小请求：

```json
{
  "url": "https://example.org/paper"
}
```

可选补充 `title`、`authors`、`venue`、`year`、`doi`、`provider_id`、`model_name`。用户补充字段不会单独通过学术身份 Gate。

响应的 `data` 关键字段：

```json
{
  "task_id": "uuid",
  "status": "SUCCESS",
  "result": {
    "paper_document": {
      "title": "...",
      "source_url": "...",
      "resolved_source_url": "...",
      "pdf_url": "...",
      "content_hash": "...",
      "source_status": "...",
      "parser": "...",
      "parser_version": "...",
      "page_count_total": 12,
      "page_count_parsed": 12,
      "pages": [
        {
          "page": 1,
          "text": "...",
          "start": 0,
          "end": 1200
        }
      ],
      "academic_gate": {}
    }
  }
}
```

### 4.2 PDF 上传

```http
POST /api/papers/upload
Content-Type: multipart/form-data
```

最小表单为 `file=<PDF>`。该接口默认只允许本机请求，文件上限默认 10 MiB。

## 5. 阅读报告 API

```http
POST /api/reading_reports
Content-Type: application/json
```

请求：

```json
{
  "task_id": "uuid",
  "provider_id": "provider-id",
  "model_name": "model-name",
  "force": false
}
```

响应的 `data` 关键结构：

```json
{
  "task_id": "uuid",
  "reading_report": {
    "executive_summary": "...",
    "key_questions": [
      {
        "question": "...",
        "answer": "...",
        "evidence": [
          {
            "source_id": "...",
            "source_url": "...",
            "page_start": 4,
            "page_end": 4,
            "exact_quote": "...",
            "verified_in_source": true,
            "verification_status": "source_only"
          }
        ]
      }
    ],
    "process": [],
    "contributions": [],
    "limitations": [],
    "source_grounded": true,
    "source_content_hash": "..."
  }
}
```

服务端会移除无法在指定页逐字匹配的模型引文。报告至少要求四个有效关键问题、方法过程、主要贡献和足够的可匹配引用。

个人总结：

```http
PUT /api/reading_reports/{task_id}/personal_summary
```

```json
{
  "summary": "不超过 300 字"
}
```

## 6. 问答和核验 API

### 6.1 论文问答

```http
POST /api/chat/ask
```

```json
{
  "task_id": "uuid",
  "scope": "task",
  "question": "论文的方法和威胁模型是什么？",
  "history": [],
  "provider_id": "provider-id",
  "model_name": "model-name"
}
```

响应的 `data`：

```json
{
  "answer": "...",
  "sources": [
    {
      "source_type": "paper_page",
      "task_id": "uuid",
      "page_start": 4,
      "page_end": 5,
      "source_url": "...",
      "text": "..."
    }
  ]
}
```

聊天答案没有阅读报告同等级的逐句后验引文校验，重要结论仍应回到分页原文确认。

### 6.2 联网核验

```http
POST /api/verification_tasks
```

最小请求：

```json
{
  "text": "待核实主张"
}
```

或：

```json
{
  "url": "https://example.org/article"
}
```

返回 `task_id` 和初始状态。结论包括：

```text
supported
refuted
mixed
insufficient
data_void
source_risk
```

### 6.3 任务轮询

```http
GET /api/task_status/{task_id}
```

终态：

```text
SUCCESS
FAILED
```

重要缺陷：未知 `task_id` 当前也可能返回 `PENDING`，FastTask 必须设置总超时，不能无限轮询。

## 7. 持久化和配置

关键变量：

| 变量 | 默认/用途 |
|---|---|
| `BACKEND_HOST` | `0.0.0.0` |
| `BACKEND_PORT` | `8483` |
| `DATABASE_URL` | `sqlite:///backend/reel_mind.db` |
| `NOTE_OUTPUT_DIR` | `backend/note_results` |
| `UPLOAD_DIR` | `backend/uploads` |
| `MAX_UPLOAD_BYTES` | 默认 10 MiB |
| `ONLINE_VERIFY_SEARCH_PROVIDER` | 默认 `brave` |
| `BRAVE_SEARCH_API_KEY` | Brave Search Key |
| `CHAT_VECTOR_INDEX_ENABLED` | 默认关闭 |
| `VECTOR_DB_DIR` | ChromaDB 目录 |

主要文件产物：

```text
note_results/{task_id}.json
note_results/{task_id}.status.json
note_results/_verification/{task_id}/claims/{claim_id}.json
```

SQLite 主要保存供应商、模型和旧视频任务索引；JSON 文件保存论文、报告和核验事实；ChromaDB 是可选派生索引。

## 8. FastTask 接入建议

### 论文阅读任务

1. FastTask 调用 `/papers/from_url` 或 `/papers/upload`。
2. 保存 FastRead `task_id`、请求哈希和来源外部 ID。
3. URL 导入通常同步返回结果；核验任务按状态轮询。
4. 成功后保存外部引用，不复制无关全文。
5. 将阅读报告中的关键问题和证据保存为外部引用或候选任务描述。
6. 用户确认后，通过现有任务完成或计划项完成接口提交合法证据；FastTask 在这些用例内部生成 ProgressEvent。

证据映射必须保留：

```text
task_id
content_hash
source_url
page_start/page_end
exact_quote
verified_in_source
verification_status
```

### FastRead 到 FastWrite

建议生成普通研究材料文件，例如：

```text
research-notes/<paper-key>.md
```

内容应包含标题、来源、内容哈希、页码引文、阅读结论和待确认项。不要把未经核验的聊天回答直接写入论文正文。

## 9. 风险和限制

1. 多数业务 API 无鉴权，网络暴露后可被任意调用。
2. 后台任务依附 API 进程，没有持久队列恢复。
3. 未知任务返回 `PENDING` 而不是 404。
4. SQLite、JSON、上传文件和 ChromaDB 没有跨介质事务。
5. 聊天答案没有阅读报告同等级的后验逐字校验。
6. 当前不支持扫描 PDF OCR、任务取消、Webhook 或 SSE。

## 10. 关键位置

- `FastRead/README.md`
- `FastRead/docs/FASTREAD_REQUIREMENTS.md`
- `FastRead/backend/main.py`
- `FastRead/backend/app/routers/note.py`
- `FastRead/backend/app/routers/chat.py`
- `FastRead/backend/app/services/paper_ingest_service.py`
- `FastRead/backend/app/services/reading_report_service.py`
- `FastRead/backend/app/services/chat_service.py`
- `FastRead/backend/app/services/note_task_service.py`
- `FastRead/backend/app/services/verification/`
- `FastRead/backend/app/repositories/note_artifacts.py`
- `FastRead/backend/app/core/settings.py`
- `FastRead/backend/tests/test_academic_reading_workflow.py`
