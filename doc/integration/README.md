# FastResearch 工具对接总览

> 盘点时间：2026-08-09  
> 盘点范围：`/root/repo/FRD/FastTask`、`FastInsight`、`FastNews`、`FastRead`、`FastWrite`  
> 文档目标：说明各工具当前真实能力、可调用边界、推荐协作链路和待补契约  
> 事实来源：各仓库当前代码、配置、README、部署文件和测试，不把需求规划视为已实现能力

## 1. 状态标记

本文档组使用以下标记：

| 标记 | 含义 |
|---|---|
| 已实现 | 当前代码中存在，可按文档调用 |
| 可立即接入 | 目标工具已有可调用界面；FastTask 仍可能需要新增适配器或 Runner |
| 建议新增 | 为形成稳定协作链路，后续应补充的能力 |
| 未实现 | 文档或需求中出现，但当前代码没有对应实现 |

## 2. 工具定位

| 工具 | 核心职责 | 当前运行形态 | 默认地址/端口 | 推荐协作边界 |
|---|---|---|---|---|
| FastTask | 长期目标、任务树、每日计划、进度与跨工具编排 | Go HTTP 服务、Worker、Scheduler、SQLite | `http://127.0.0.1:10000` | 任务状态、审批、重试、幂等、外部引用 |
| FastInsight | 截图/链接中的论文核验、趋势补充、方向路由和飞书卡片生成 | Python Skill、CLI、JSON/YAML 文件 | 无端口 | 子进程和文件契约 |
| FastNews | 安全资讯和顶会论文抓取、LLM 分类总结、静态报告生成 | Python CLI、JSON/JSONL、GitHub Actions | 无端口 | 批处理任务和结构化产物 |
| FastRead | PDF/论文 URL 导入、分页原文、证据化阅读报告、问答和联网核验 | FastAPI、React、Tauri、SQLite/JSON | API `8483`，Web `3015` | 本地 HTTP API 和任务产物 |
| FastWrite | LaTeX Workspace、AI 修改审批、编译、Review、Memory 和 GitHub Sync | Bun HTTP 服务、React、文件/JSON/Git | API `3003`，开发 Web `3002` | 本地 HTTP API 和 Workspace 文件 |

## 3. 当前集成成熟度

| 能力 | FastInsight | FastNews | FastRead | FastWrite |
|---|---:|---:|---:|---:|
| 目标工具提供 CLI | 是 | 是 | 不建议作为业务入口 | 不建议作为业务入口 |
| 可由 FastTask 调用 HTTP API | 否 | 否 | 是 | 是 |
| 有稳定结构化产物 | JSON/YAML | JSON/JSONL | JSON、SQLite、文件 | JSON、Workspace、Git |
| 有入站鉴权 | 无 | 不适用 | 无，部分管理接口限本机 | 无，依赖 loopback |
| 有持久任务状态 | 无 | 无 | 部分有，依赖 API 进程 | 有 Run/Plan，但长调用仍为同步 HTTP |
| 有 webhook/事件回调 | 无 | 无 | 无 | 无 |
| 有自动化测试 | 无 | 无 | 有 | 有 |
| 已有 FastTask 专用适配器 | 无 | 无 | 无 | 无 |

重要现状：

- FastTask 已实现用户任务、Agent Job、外部 Worker Callback 和 Panel Summary。
- FastTask 当前没有执行 FastInsight/FastNews 命令的通用 CLI Runner，也没有对应 Agent Job 类型。
- FastTask 已实现 `POST /api/v1/imports`、查询、用户转换/拒绝，以及 `/api/v1/integrations/status`。
- 四个工具均没有面向 FastTask 的专用回调或双向同步协议。
- FastRead 和 FastWrite API 都没有应用层鉴权，只应在可信本机或受控内网调用。

## 4. 推荐端到端链路

### 4.1 主研究链路

```text
FastNews 发现论文/资讯
  -> FastInsight 识别、核验和方向路由
  -> FastTask 创建候选任务并负责审批、幂等和状态
  -> FastRead 导入论文、生成分页证据和阅读报告
  -> FastTask 记录阅读结论和下一步
  -> FastWrite 导入证据化材料并生成 ChangeSet
  -> 人工审批、编译和 Review
  -> FastTask 跟踪审稿问题和完成证据
```

### 4.2 飞书灵感链路

```text
飞书截图/链接
  -> 上层多模态 Agent 提取标题和 URL
  -> FastInsight verify/analyze/route
  -> 人工确认论文身份和负责人
  -> FastTask 保存来源消息、路由结果和投递状态
  -> FastRead 深读或 FastTask 创建最小阅读行动
```

### 4.3 写作闭环

```text
FastRead 阅读报告和精确页码证据
  -> FastWrite Workspace 中的 research-notes/*.md 或 refs/*.bib
  -> FastWrite Agent Plan
  -> 人工确认 ChangeSet
  -> Compile + CompileRecord
  -> ReviewIssue
  -> FastTask 缺陷任务
  -> 修订、复编译和复审
```

## 5. 职责边界

| 事实或状态 | 建议事实来源 |
|---|---|
| 长期目标、当前任务、每日承诺、完成证据 | FastTask |
| 新闻和顶会抓取原始记录 | FastNews |
| 论文初步身份、趋势信号和方向路由 | FastInsight |
| PDF 分页原文、页码引文、阅读报告、核验结论 | FastRead |
| LaTeX 正文、文件版本、ChangeSet、编译和 Review | FastWrite |
| 跨工具运行状态、重试、审批和幂等 | FastTask |

协作规则：

- 不直接修改其他工具的私有数据库或内部 JSON 数据库。
- 文件型工具优先消费 JSON/JSONL，不抓取 HTML 作为业务数据。
- FastWrite Workspace 必须通过 API 修改，避免绕过文件版本和项目版本。
- FastRead 的页码证据应完整保留 `content_hash`、`source_url`、页码和原文引文。
- 外部结果进入 FastTask 时默认是候选事项，不直接占用每日三个核心名额。
- Agent 或 LLM 结果必须经过结构校验和人工确认，不能直接更新最终研究正文或任务状态。

## 6. 最小接入阶段

### 阶段 A：只读发现

目标工具接口已经具备，但应先新增 FastTask CLI Runner 或使用受控的外部编排脚本：

1. Runner 执行 FastNews CLI，并保存输入输出路径、记录数、退出码和日志摘要。
2. Runner 执行 FastInsight CLI，并保存 `paper.json`、`trends.json` 和路由结果。
3. FastTask 或部署探针通过 HTTP 健康检查探测 FastRead 和 FastWrite。

成功标准：不自动写入其他工具，不自动发送飞书卡片，不自动修改论文正文。

### 阶段 B：候选任务和外部引用

FastTask 通用 `/imports` 已实现：

1. 以 `source.system + source.external_id` 去重。
2. 保存稳定 URL、产物路径、摘要和校验哈希。
3. 导入默认生成待确认候选事项。
4. 用户确认后再映射到 Goal/Task。

通用契约草案见 [`contract.md`](contract.md)。

### 阶段 C：受控写入

1. FastTask 调用 FastRead 导入论文并轮询任务。
2. FastTask 将 FastRead 报告作为证据引用，不复制无关全文。
3. FastTask 调用 FastWrite 创建项目或材料文件。
4. FastWrite Agent 只生成 Plan/ChangeSet，由用户审批。
5. 编译、Review 和复审结果映射为外部引用或缺陷任务；只有通过现有任务/计划项完成用例提交合法完成证据时，FastTask 才会生成 ProgressEvent。

### 阶段 D：可靠异步集成

建议新增：

- 各工具的 `external_id`、幂等键和 schema version。
- FastRead/FastWrite webhook 或 FastTask 轮询适配器。
- CLI 作业的独立工作目录、超时、取消和产物清单。
- 统一服务身份、最小 Scope 和密钥轮换。
- 跨工具 Trace ID、运行记录和错误分类。

## 7. 部署与网络建议

建议同机地址：

| 服务 | 地址 |
|---|---|
| FastTask | `127.0.0.1:10000` |
| FastRead API，直接后端 | `127.0.0.1:8483` |
| FastRead API，Docker/Nginx | `127.0.0.1:3015/api` |
| FastRead Web | `127.0.0.1:3015` |
| FastWrite API | `127.0.0.1:3003` |
| FastWrite Dev Web | `127.0.0.1:3002` |

安全要求：

- FastRead 和 FastWrite 在补充服务鉴权前保持 loopback 或受控 Unix/容器网络可达。
- 不把 LLM Key、飞书 Token、GitHub Token 写入任务正文、卡片、JSON 产物或日志。
- FastTask 作为唯一跨工具入口时，由 FastTask 持有服务凭据，浏览器前端不直接持有。
- 文件型 CLI 每次运行使用独立临时目录，禁止共享固定输出文件名。

## 8. 文档索引

- [`contract.md`](contract.md)：统一外部引用、作业和错误契约草案
- [`fastinsight.md`](fastinsight.md)：FastInsight CLI、文件契约和接入建议
- [`fastnews.md`](fastnews.md)：FastNews 批处理、JSONL 和报告流水线
- [`fastread.md`](fastread.md)：FastRead 论文导入、报告、问答和核验 API
- [`fastwrite.md`](fastwrite.md)：FastWrite 项目、文件、Agent、编译和 Review API

## 9. 后续协作清单

| 优先级 | 工作项 | 归属建议 |
|---|---|---|
| 已完成 | 实现 FastTask `/imports` 与 `source.external_id` 唯一约束 | FastTask |
| P0 | 为 FastRead/FastWrite 增加服务鉴权或受控反向代理 | 各服务与部署 |
| P0 | 建立 CLI 作业 Runner，支持独立目录、超时、日志和产物校验 | FastTask |
| P1 | FastNews 输出 JSON Schema、运行清单和可靠退出码 | FastNews |
| P1 | FastInsight 增加匹配阈值、统一 RouteResult 和批量模式 | FastInsight |
| P1 | FastRead 未知任务返回 404，并提供持久任务恢复或 webhook | FastRead |
| P1 | FastWrite 长任务异步化、幂等化，并绑定真实编译产物 | FastWrite |
| P1 | FastRead 证据到 FastWrite 研究材料的标准映射 | FastRead/FastWrite |
| P2 | 统一事件、Trace、错误码和版本兼容策略 | 全体 |
