# FastNews 对接说明

> 项目：`/root/repo/FRD/FastNews`  
> 状态：批处理和文件产物已实现；服务 API、任务状态和自动化测试未实现

## 1. 定位与流水线

FastNews 是安全研究资讯的离线采集和报告工具。

安全资讯链路：

```text
BleepingComputer / arXiv RSS
  -> 原始 JSONL
  -> LLM 翻译、筛选和导读
  -> 每日 newspaper JSON
  -> HTML/PDF 周报
```

顶会链路：

```text
USENIX Security / IEEE S&P / NDSS / CCS 页面
  -> 会议论文 JSONL
  -> LLM 十类分类和中文摘要
  -> summary JSONL
  -> HTML/PDF 报告
```

## 2. 运行方式

- Python 3.13+。
- 使用 `uv` 和 `uv.lock`。
- 无服务端口、数据库、REST API 或消息队列。
- 依赖当前工作目录中的相对路径，命令应从 FastNews 根目录执行。
- GitHub Actions 负责定时抓取、总结、报告和 Release。

安装：

```bash
uv sync
```

环境配置：

```dotenv
OPENAI_API_KEY=...
OPENAI_BASE_URL=https://api.openai.com/v1
LLM_MODEL=gemini-3-flash-preview
```

## 3. CLI 流水线

### 3.1 安全资讯

```bash
uv run python -m secnews.update bleepingcomputer
uv run python -m secnews.update arxiv_cs_cr
uv run python -m secnews.update arxiv_cs_ai
uv run python -m secnews.generate_newspaper
uv run python -m secnews.generate_pdf 7
```

### 3.2 顶会论文

```bash
uv run python top-conf/fetch_big4.py usenix 2026
uv run python top-conf/generate_conf_summary.py usenix 2026 --batch-size 40
uv run python top-conf/generate_conf_report.py usenix 2026
```

会议标识：

```text
usenix
ieee-sp
ndss
ccs
```

刷新主页：

```bash
uv run python generate_homepage.py
```

## 4. 文件契约

### 4.1 原始安全资讯

路径：

```text
secnews/data/articles/YYYY-MM-DD.jsonl
```

每行主要字段：

```json
{
  "title": "...",
  "link": "...",
  "description": "...",
  "published": "...",
  "author": "...",
  "categories": [],
  "_id": "...",
  "source": "...",
  "fetched_at": "2026-08-09T08:00:00+00:00"
}
```

### 4.2 每日摘要

路径：

```text
secnews/data/newspapers/YYYY-MM-DD.json
```

结构：

```json
{
  "generated_at": "...",
  "article_start_at": "...",
  "articles": {
    "bleepingcomputer": [],
    "arxiv_cs_cr": [],
    "arxiv_cs_ai": []
  }
}
```

入选条目保留原始字段，并增加 `intro`。

### 4.3 顶会原始论文

路径：

```text
top-conf/data/conferences/<conference>_<year>.jsonl
```

主要字段：

```json
{
  "_id": "...",
  "title": "...",
  "link": "...",
  "description": "...",
  "published": "...",
  "author": "...",
  "source": "USENIX Security 2026",
  "fetched_at": "...",
  "pdf_link": "..."
}
```

### 4.4 顶会分类摘要

路径：

```text
top-conf/data/summary/<conference>_<year>_summary.jsonl
```

每行：

```json
{
  "category": "Network Security",
  "paper": {
    "_id": "...",
    "title": "...",
    "link": "...",
    "author": "...",
    "summary_zh": "..."
  }
}
```

### 4.5 人类可读产物

```text
secnews/data/report/*.html
top-conf/data/report/*.html
top-conf/data/report/*.pdf
index.html
```

跨工具消费应优先读取 JSON/JSONL，不解析 HTML。

## 5. FastTask 接入建议

建议作业图：

```text
secnews.fetch(三个 source，当前应串行)
  -> secnews.summarize
  -> secnews.build_report
  -> homepage.refresh
```

```text
conference.fetch
  -> conference.summarize
  -> conference.build_report
  -> homepage.refresh
```

FastTask 每次运行应记录：

| 字段 | 内容 |
|---|---|
| `job_type` | 例如 `fastnews.conference.fetch` |
| `parameters` | source、conference、year、days |
| `input_paths` | 输入 JSON/JSONL |
| `output_paths` | 输出 JSON/JSONL/HTML/PDF |
| `input_count` | 输入记录数 |
| `output_count` | 输出记录数 |
| `exit_code` | 进程退出码 |
| `content_hash` | 产物校验哈希 |
| `started_at/finished_at` | 运行时间 |

成功判定不能只看退出码，还应检查：

1. 日志中不存在 `Traceback`、`Error:` 或抓取失败信息。
2. 预期文件存在且在本次运行后更新。
3. JSON/JSONL 可完整解析。
4. `_id`、`title`、`link` 等必需字段存在。
5. 摘要输入输出覆盖率在允许范围内。
6. 要求 PDF 时 PDF 存在且非空。

三个 `secnews.update` 命令都会追加同一个当天 JSONL，当前没有跨进程锁，因此不得并行执行。若未来改为每个 source 独立输出并增加原子合并，才适合并行抓取。

## 6. 与其他工具的数据映射

### FastNews 到 FastInsight

顶会论文可转换为：

```json
{
  "title": "paper.title",
  "authors": "paper.author",
  "venue": "record.source",
  "abstract": "record.description",
  "url": "record.pdf_link 或 record.link"
}
```

已有标题、摘要和会议信息时可直接调用 `route_paper.py`，不必重复核验；身份不确定时再调用 `verify_paper.py`。

### FastNews 到 FastRead

优先传递：

```text
pdf_link -> link -> DOI/详情页
```

FastRead 导入失败时，FastTask 应保留原始记录并创建“查找可访问 PDF”的最小行动。

### FastNews 到 FastTask

建议导入 `generated_report` 或 `candidate_task`，以命名空间主键去重：

```text
fastnews:secnews:<_id>
fastnews:conference:<_id>
```

## 7. 风险和限制

1. `secnews.update` 捕获异常后可能仍退出 `0`。
2. LLM 输出仅做 JSON 解析，没有 schema、分类枚举和 `_id` 白名单校验。
3. 安全周报的 WeasyPrint 降级行为与 README 不完全一致，PDF 失败可能终止命令。
4. 顶会抓取依赖官网 HTML 结构，页面改版可能静默降低质量。
5. 文件写入缺少事务和本地锁，并发运行可能覆盖或损坏产物。
6. 顶会原始文件覆盖写、摘要追加写，可能积累已经失效的旧摘要。
7. 没有自动化测试、lint、类型检查和机器可读运行清单。

## 8. 关键位置

- `FastNews/README.md`
- `FastNews/pyproject.toml`
- `FastNews/.env.example`
- `FastNews/secnews/util.py`
- `FastNews/secnews/update.py`
- `FastNews/secnews/generate_newspaper.py`
- `FastNews/secnews/generate_pdf.py`
- `FastNews/top-conf/fetch_big4.py`
- `FastNews/top-conf/generate_conf_summary.py`
- `FastNews/top-conf/generate_conf_report.py`
- `FastNews/generate_homepage.py`
- `FastNews/.github/workflows/`
