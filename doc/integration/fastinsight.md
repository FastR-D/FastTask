# FastInsight 对接说明

> 项目：`/root/repo/FRD/FastInsight`  
> 状态：CLI 和文件接口已实现；飞书收发、持久化和服务化未实现

## 1. 定位与边界

FastInsight 是论文和研究资讯的入口分拣 Skill，负责：

```text
标题/截图提取结果
  -> 学术元数据核验
  -> 引用和相关工作信号
  -> 研究方向匹配
  -> 负责人选择
  -> 飞书卡片 JSON
```

它不负责 OCR、多模态识图、飞书 webhook、卡片实际发送、任务持久化、重试或投递回执。以上能力应由上层 Agent、Connector 或 FastTask 提供。

## 2. 运行方式

- Python 3.8+。
- 无构建步骤、无端口、无数据库。
- PyYAML 可选；未安装时使用 `scripts/mini_yaml.py`。
- 所有阶段通过 CLI、stdout、退出码和 JSON/YAML 文件协作。

完整示例：

```bash
python scripts/validate_roster.py assets/colleagues.yaml

python scripts/verify_paper.py \
  --query "Attention Is All You Need" \
  --json-out paper.json

python scripts/analyze_trends.py \
  --paper paper.json \
  --json-out trends.json

python scripts/route_paper.py \
  --paper paper.json \
  --roster assets/colleagues.yaml \
  --trends trends.json \
  --dry-run
```

正式生成卡片：

```bash
python scripts/route_paper.py \
  --paper paper.json \
  --roster assets/colleagues.yaml \
  --trends trends.json \
  --card-out feishu_card_payload.json
```

## 3. CLI 契约

### 3.1 论文核验

```text
verify_paper.py --query QUERY [--limit 5] [--json-out PATH]
```

数据源按顺序尝试 Semantic Scholar、arXiv、Crossref。首个返回候选的数据源会结束搜索，不会合并三方结果。

输出：

```json
{
  "best": {
    "title": "...",
    "authors": "...",
    "year": 2026,
    "venue": "...",
    "abstract": "...",
    "url": "...",
    "doi": "...",
    "arxiv_id": "...",
    "match_score": 0.92
  },
  "candidates": [],
  "source": "SemanticScholar",
  "error": null,
  "warnings": []
}
```

退出码：

| 退出码 | 语义 |
|---:|---|
| `0` | 至少有一个候选 |
| `1` | 所有来源无结果或失败 |
| `2` | 常见 argparse 参数错误 |

`best` 只是当前来源的最高分候选，不等于论文身份已确认。FastTask 应设置人工确认或业务阈值。

### 3.2 趋势分析

```text
analyze_trends.py --paper PATH [--json-out PATH] [--limit 5]
```

稳定消费字段：

```json
{
  "paper_url": "...",
  "citation_count": 123,
  "influential_citation_count": 12,
  "fields_of_study": ["Computer Science"],
  "related_papers": [],
  "trend_summary": "...",
  "source": "SemanticScholar",
  "error": null
}
```

外部 API 全部失败时仍可能退出 `0`，调用方必须检查 `source` 和 `error`。

### 3.3 路由和卡片

```text
route_paper.py --paper PATH --roster PATH [--trends PATH]
               [--template PATH] [--card-out PATH] [--dry-run]
```

路由输入支持扁平 Paper 对象或 `{ "best": {...} }`。分类文本为 `title + abstract + venue`。

输出：

```json
{
  "matched_direction": "网络安全 / AI安全",
  "score": 3,
  "hits": ["secure inference"],
  "fallback": false,
  "fallback_reason": "",
  "invalid_owners": [],
  "owners": [
    {
      "name": "负责人",
      "feishu_open_id": "ou_..."
    }
  ],
  "routable": true,
  "paper_url": "...",
  "trend_summary": "...",
  "card": {}
}
```

注意：

- dry-run 时 `card` 是对象。
- 正式模式中 `card` 是输出路径字符串。
- 非 dry-run 且没有有效负责人时退出码为 `2`。
- 只选择一个方向，不支持多方向路由。
- 默认输出名可能是固定的 `feishu_card_payload.json`，并发调用必须传唯一路径。

### 3.4 名册

```yaml
directions:
  - name: "网络安全 / AI安全"
    priority: 10
    keywords:
      - "secure inference"
      - "隐私计算"
    owners:
      - name: "负责人"
        feishu_open_id: "ou_real_id"

default_owner:
  name: "组长"
  feishu_open_id: "ou_real_id"
```

未安装 PyYAML 时，应使用块状 YAML，不依赖 flow style JSON/YAML。

## 4. 环境与外部依赖

| 变量 | 必填 | 用途 |
|---|---:|---|
| `S2_API_KEY` | 否 | Semantic Scholar API Key |
| `CROSSREF_MAILTO` | 否 | Crossref polite pool 联系邮箱 |

FastInsight 不加载 `.env`。飞书 `APP_ID`、`APP_SECRET` 和 tenant token 只存在于参考文档，没有代码实现。

## 5. FastTask 接入建议

建议 FastTask CLI Runner 为每次事件创建独立目录：

```text
data/integrations/fastinsight/<trace_id>/
  input.json
  paper.json
  trends.json
  route.json
  card.json
  stdout.log
  stderr.log
```

推荐步骤：

1. 以飞书 `message_id` 或上游记录 ID 作为幂等键。
2. 调用 `verify_paper.py`。
3. 检查 `best`、`source`、`error` 和 `match_score`。
4. 低置信、标题冲突或多候选时进入人工确认。
5. 可选调用 `analyze_trends.py`。
6. 先以 `--dry-run` 预览方向和负责人。
7. 人工确认后生成卡片并交给外部 Connector 发送。
8. FastTask 保存投递状态，并创建“阅读并判断论文”的候选任务。

候选任务映射：

| FastInsight | FastTask |
|---|---|
| `best.title` | Task `title` |
| `trend_summary` | Task `description` |
| `paper_url` | 外部证据 URL |
| `matched_direction` | Goal 建议或标签 |
| `hits` | 导入 metadata |
| `owners` | 建议负责人，不直接授权 |
| `warnings/error` | 导入审计和阻碍 |

## 6. 风险和限制

1. 没有最低匹配阈值，低质量候选也会成为 `best`。
2. 趋势分析按标题取第一项，未使用 DOI/arXiv ID 做严格交叉确认。
3. 路由仅按关键词计数，缺少负关键词、权重和最低得分。
4. 无持久化、幂等、并发控制、发送回执和自动重试。
5. `card` 类型随运行模式变化，适配器必须归一化。
6. 当前没有自动化测试和部署配置。

## 7. 关键位置

- `FastInsight/README.md`
- `FastInsight/SKILL.md`
- `FastInsight/scripts/verify_paper.py`
- `FastInsight/scripts/analyze_trends.py`
- `FastInsight/scripts/route_paper.py`
- `FastInsight/scripts/validate_roster.py`
- `FastInsight/assets/colleagues.yaml.example`
- `FastInsight/assets/feishu_card.json`
- `FastInsight/references/academic_search.md`
- `FastInsight/references/feishu_integration.md`
