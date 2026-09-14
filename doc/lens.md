# FastTask 决策透镜设计（Lens / Reflect / Map）

> 文档状态：初版设计
> 想法来源：`doc/plotminder/`（Plotminder 产品构想）
> 依据文档：`doc/func.md`、`doc/arch.md`、`doc/req/overall.md`
> 实现规格：`doc/lens-impl.md`（本文档定判断，那份定落地细节）

## 1. 文档定位

本文档定义 FastTask 的一组新能力：给任务附加**可解释的二维坐标**，并在此之上提供**周复盘**与**可选的目标地图**。

设计起点是 `doc/plotminder/` 中的决策地图构想。该构想不能整体移植——它与 FastTask 的多条产品原则硬冲突（见 §3）。本文档只吸收其中与 FastTask 真实缺口吻合的部分，并按科研场景重新定义语义。

被解决的缺口：FastTask 现在能很好地回答「今天做什么」（每日 0..3 核心项），但几乎不回答两个问题——

1. **我整体在哪？** 长期目标要推数周到数月，用户只能看树形列表，没有全局感。
2. **我这几周把时间花到哪去了？** `func.md` §3.2 把「更完整的周报、复盘和长期趋势分析」列为待办，至今为空。

## 2. 设计结论

| 能力 | 结论 | 优先级 |
|---|---|---|
| 任务坐标（透镜） | 做，单一预设透镜，不开放自定义 | L1 |
| 周复盘（Reflect） | 做，**本设计的核心价值点** | L2 |
| 目标地图（Map） | 做，**可选视图**，不进入主流程 | L3 |
| 第三轴 / 3D | 不做 | — |
| 用户自建无限 view | 不做 | — |
| 通用 Capture 浮窗 | 不做 | — |
| 外部待办导入（Apple / Google / Todoist） | 不做 | — |
| 路径导航推荐 | 只保留里程碑剩余步骤预览，不做每日路径 | 待定 |

地图是手段，周复盘是目的。若只能交付一项，交付周复盘。

## 3. 与现有产品原则的冲突裁决

Plotminder 构想中与 FastTask 冲突的部分，逐条裁决：

| Plotminder 主张 | FastTask 约束 | 裁决 |
|---|---|---|
| 时间是属性而非轴，不按 deadline 排序 | §10.3 排序链第 2、4 位是目标日期风险与关键路径 | **不引入**。时间继续留在确定性排序中；坐标只承载判断维度，时间压力以视觉标记呈现，不占轴 |
| 事项越多地图越有用 | 原则 10：不做无限增长的待办箱 | **改变地图单位**。地图画的是单个 goal 的全部叶子任务（数十个），不是当日任务，也不是跨 goal 全量 |
| 用户自定义任意坐标轴 | 目标用户是科研人员，不会花 30 分钟配坐标系 | **固定预设透镜**，v1 仅一个，`lens` 字段预留扩展 |
| LLM 自动打分后可直接生效 | §8.3：结构性变更必须用户确认；Agent 不得静默覆盖 | **坐标为提案**。用户覆盖后置 `pinned`，Agent 不得再改写 |
| Pareto frontier + ideal corner 推荐下一步 | §10.1：候选过滤、排序、取三必须由确定性程序完成 | **不接入每日选择**。坐标只做展示与复盘，不改动现有排序算法 |
| Capture 浮窗 + 外部待办导入 | 非目标：通用个人生活待办 | **不做**。外部输入继续走 FastInsight / FastNews / FastRead / FastWrite 收件箱 |

## 4. 核心概念：透镜与坐标

**透镜（Lens）** 是一组坐标轴定义。v1 只有一个预设透镜 `research_risk`：

| 轴 | 字段 | 取值 | 含义 |
|---|---|---|---|
| X | `uncertainty` | 0–100 | 不确定性：0 = 完全知道怎么做，100 = 方法未知，需要探索或验证 |
| Y | `contribution` | 0–100 | 贡献度：该任务对所属目标验收条件的直接贡献 |

选择这两个维度而非「重要 × 紧急」的理由：对研究生而言，导师交办的事都重要、deadline 都紧，Eisenhower 轴没有区分度。而**不确定性**在 FastTask 里本来就是一等概念（`func.md` §7.1：「未知性高的工作应拆成探索或验证任务」），只是此前没有被量化、没有被看见。

## 5. 四区语义（取代 ideal corner）

Plotminder 用 Pareto frontier + 用户声明的 ideal corner 排序。该模型在此不适用：不确定性**不是越低越好**，高不确定 + 高贡献的任务恰恰是最该优先做的。

改用四区语义，每区有明确的行动含义：

```text
贡献度 ↑
100 ┌─────────────────┬─────────────────┐
    │  B 主推进区      │  A 关键风险区    │
    │  低不确定 高贡献  │  高不确定 高贡献  │
    │  稳定产出        │  尽早验证，拖延   │
    │                 │  成本最高        │
 50 ├─────────────────┼─────────────────┤
    │  D 消耗区        │  C 时间黑洞      │
    │  低不确定 低贡献  │  高不确定 低贡献  │
    │  必要但不应占     │  应降级、拆小或   │
    │  主要时间        │  砍掉            │
  0 └─────────────────┴─────────────────┘
    0                50               100  不确定性 →
```

分区边界固定为 50/50，不可配置。分区只用于复盘归类与地图着色，**不进入每日计划的排序逻辑**。

## 6. 数据模型

新增一张表，不修改 `tasks` 表结构。

```sql
-- migrations/000004_task_coords.up.sql
CREATE TABLE task_coords (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id),
    task_id TEXT NOT NULL REFERENCES tasks(id),
    lens TEXT NOT NULL DEFAULT 'research_risk',
    x INTEGER NOT NULL,
    y INTEGER NOT NULL,
    source TEXT NOT NULL DEFAULT 'agent',
    pinned INTEGER NOT NULL DEFAULT 0,
    rationale TEXT NOT NULL DEFAULT '',
    revision INTEGER NOT NULL DEFAULT 1,
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL,
    UNIQUE(task_id, lens)
);

CREATE INDEX idx_task_coords_user_lens ON task_coords(user_id, lens);
```

| 字段 | 说明 |
|---|---|
| `lens` | 透镜标识，v1 恒为 `research_risk` |
| `x` / `y` | 0–100 整数，越界拒绝写入 |
| `source` | `agent` / `user` / `default` |
| `pinned` | 用户手工设定过则为 1，Agent 不得覆盖 |
| `rationale` | 打分理由，一句话，Agent 产出时填写；用户覆盖时可留空 |

Go 侧对应 `persistence.TaskCoord`，字段与列一一对应，`user_id` 打 `json:"-"`，与现有 Record 约定一致。

## 7. 坐标来源与写入规则

### 7.1 Agent 产出

在 `agent.OpenAI.TaskProposal` 的输出 schema 中，为 `op=create` 节点增加两个可选字段：

```text
uncertainty（0-100）、contribution（0-100）、coord_rationale（一句话，不超过 40 字）
```

字段缺失或越界时不报错，落到 `source='default'` 的中心点 `(50, 50)`，理由留空。坐标不是任务树的结构性内容，缺失不应导致整份提案被拒。

坐标随提案一起进入 `proposals.patch_json`，在 `App.ApplyProposal` 中与任务节点一并落库，**共享同一次用户确认**，不额外增加确认步骤。

### 7.2 用户覆盖

用户在地图上拖动，或在任务详情中直接填写，写入 `source='user'`、`pinned=1`。

一旦 `pinned=1`，后续 Agent 提案中该任务的坐标字段被忽略，只记录在提案详情中供用户查看，不自动生效。这是 §8.3「Agent 不得静默覆盖用户数据」在坐标上的落实。

### 7.3 反馈回路

用户覆盖记录（任务标题 + Agent 原坐标 + 用户最终坐标）作为 few-shot 样例注入后续 `TaskProposal` 的 prompt，上限取最近 10 条。模型越用越贴近该用户的判断尺度。

### 7.4 缺省

未打分的任务一律视为 `(50, 50)`、`source='default'`，在地图上以空心节点呈现，在复盘中单独计为「未标注」，不混入四区统计。

## 8. 有效投入的口径

复盘和地图都要用到「真实专注时长」。口径统一定义如下，避免各处不一致：

```text
有效专注分钟(task) = SUM(work_sessions.duration_seconds) / 60
  WHERE task_id = task
    AND status IN ('completed', 'stopped')
    AND duration_seconds > 0
```

`status='invalidated'` 的 Session 一律排除（用户已明确标记无效）；`stopped`（提前结束）计入，因为那是真实发生的投入。

「实质推进」用于停滞检测，定义为：

```text
实质推进(task) = 存在 progress_events.type IN ('result', 'step') AND task_id = task
```

`time` 与 `minimum_action` 不算实质推进——这正是复盘要暴露的问题：一个任务可以连续两周只有最小行动，看起来天天在动，实际没有产出。

## 9. 周复盘（Reflect）

### 9.1 定位

周复盘是本设计的核心。它做的事是把**用户的判断**（坐标）与**用户的实际投入**（番茄时长与证据）叠在一起，暴露两者的错位。

这是 FastTask 独有的能力：Plotminder 只有判断这一个平面，没有真实投入数据；通用待办工具有时间统计，但没有语义坐标。两者同时具备时，才能得出这类结论：

> 本周有效专注 11.2 小时。其中 8.1 小时（72%）落在 D 消耗区；你标为 A 关键风险区的「跑通基线实验」已 19 天没有实质推进。

### 9.2 周窗口

按用户时区的自然周，周一 00:00 至周日 24:00。时区取 `users.timezone`，请求可用 `timezone` 参数覆盖，两者都无效时回落 `Asia/Shanghai`。周标识用 ISO 周格式 `2026-W37`。

### 9.3 指标

| 指标 | 计算 |
|---|---|
| 四区投入分布 | 本周有效专注分钟按任务所在分区聚合，含占比 |
| 环比变化 | 同上，与上一周对比，给出各区分钟差值 |
| 停滞任务 | A 区中 `status` 未完成且距上次实质推进 ≥ 14 天的任务，按天数倒序 |
| 证据构成 | 本周已满足的当日计划项按 `completion_type` 分布（result / step / time / minimum_action） |
| 最小行动依赖度 | `minimum_action` 类完成数 ÷ 已满足核心项总数 |
| 未标注占比 | `source='default'` 的任务所占投入分钟比例 |

所有指标由确定性 SQL 聚合产出，可复算、可解释。

### 9.4 总结句

一句话总结**优先由确定性模板生成**，规则按顺序命中第一条：

1. A 区存在停滞任务 → 「你标为关键风险的 {title} 已 {n} 天没有实质推进」
2. D 区投入占比 > 50% → 「本周 {pct}% 的专注时间落在低不确定低贡献的工作上」
3. 最小行动依赖度 > 50% → 「本周 {n} 个核心项中有 {m} 个只完成了最小行动」
4. A 区投入占比环比提升 → 「本周在关键风险区投入 {n} 分钟，比上周多 {d} 分钟」
5. 兜底 → 陈述四区分布，不作评价

复盘接口是只读 GET，**不得在其中同步调用 LLM**——FastTask 的全部 Agent 工作都走 Job + Worker，同步外部调用会把一个读接口的延迟绑定到模型可用性上。因此：

- L2 只交付模板句，响应中的 `llm_note` 字段保留但恒为空字符串。
- LLM 润色作为 L2.5 单独交付：新增 `weekly_review_note` 作业类型，复用现有 Job 机制异步生成，结果写入后由前端二次拉取。

无论 LLM 是否配置，复盘的全部确定性内容都完整可用，与 §8.4 的降级要求一致。

总结句一律为陈述与建议口吻，不做评分、不做排名，与 §11.2「不用于用户绩效排名」一致。

### 9.5 触发

MVP 为拉取式：用户打开复盘页即计算当周数据，不做定时推送。周复盘结果不落库，每次实时聚合——数据量在单用户单周的量级，无须缓存。

## 10. 目标地图（Map，可选视图）

### 10.1 可选性

地图是 goal 详情页下的一个可切换视图，与现有树形列表并列。**不替换任何现有界面，不进入每日计划流程**。用户完全不打开它，FastTask 的全部功能不受影响。

### 10.2 单位

一次渲染一个 goal 的全部叶子任务（`type` 为 `task`/`action` 且无子节点）。典型数量数十个，密度合适。不做跨 goal 的全量地图——那会把 FastTask 变成待办箱。

### 10.3 视觉编码

| 视觉属性 | 编码内容 |
|---|---|
| 位置 (x, y) | 当前透镜下的坐标 |
| 节点大小 | 累计有效专注分钟（§8 口径） |
| 节点填充色 | 任务状态（ready / doing / done / blocked） |
| 节点边框 | 目标日期风险：goal 的 `target_date` 临近且任务未完成时高亮 |
| 透明度 | 距上次实质推进的天数，越久越淡 |
| 空心节点 | `source='default'`，尚未标注坐标 |

位置是判断，大小是实际投入。两者的错位在图上直接可见——大而偏左下的节点，就是「嘴上说不重要、身体很诚实」的时间去向。

### 10.4 交互

MVP 只做三件事：拖动节点改坐标（写入 §7.2）、点击节点打开任务详情、四区底色与图例。不做框选、不做缩放 LOD、不做路径线绘制。

### 10.5 墨水屏

地图是静态、低刷新、高信息密度的呈现，天生适合墨水屏。但 MVP 不做——墨水屏当前的只读契约（Token + ETag + 304）只覆盖当日三项，扩展需要单独的版本协商设计。留作后续。

## 11. 接口草案

沿用现有 Gin + Huma v2 契约风格，全部挂在 `/api/v1` 下，用户态鉴权。

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/lenses` | 返回预设透镜定义（轴名、取值域、分区边界），前端不硬编码 |
| `GET` | `/goals/{goal_id}/map` | 地图数据：叶子任务 + 坐标 + 有效专注分钟 + 距上次实质推进天数。可选 `lens` 查询参数 |
| `PUT` | `/tasks/{task_id}/coords` | 用户覆盖坐标。Body 含 `lens`、`x`、`y`；带 `If-Match` 式 `expected_revision`，与现有乐观锁约定一致 |
| `GET` | `/reviews/weekly` | 周复盘。可选 `week`（ISO 周，缺省当周）与 `timezone` |

坐标写入返回 409 的条件：`revision` 不匹配。坐标越界返回 422。

## 12. 不变式与测试

落在 `internal/domain/rules.go` 的纯函数，配单元测试：

1. 坐标必须在 0–100 闭区间，越界拒绝。
2. `(task_id, lens)` 唯一。
3. `pinned=1` 的坐标不被 Agent 提案覆盖。
4. 分区判定在边界值 50 上稳定（50 归入高侧，即 `>= 50` 为高）。
5. `invalidated` Session 不计入有效专注分钟。
6. `time` 与 `minimum_action` 类证据不构成实质推进。
7. 复盘在零数据周返回合法空结果，不报错、不编造。
8. LLM 不可用时复盘返回完整的确定性部分。

## 13. 分期

| 阶段 | 内容 | 交付物 |
|---|---|---|
| **L1** | 数据层 + Agent 产出坐标 + 手工覆盖接口 | migration 000004、`TaskCoord` Record、domain 规则与测试、`PUT /tasks/{id}/coords`、`GET /lenses`、prompt schema 扩展 |
| **L2** | 周复盘 | 聚合查询、`GET /reviews/weekly`、模板总结句、LLM 润色与降级、前端复盘页 |
| **L3** | 目标地图 | `GET /goals/{id}/map`、前端 SVG 地图视图、拖动改坐标 |

L1 无 UI，可独立验证。L2 是价值交付点。L3 可延后或不做。

## 14. 对现有模块的改动清单

| 模块 | 改动 | 风险 |
|---|---|---|
| `migrations/` | 新增 000004，仅建表 | 低 |
| `internal/persistence/models.go` | 新增 `TaskCoord` | 低 |
| `internal/domain/rules.go` | 新增坐标校验与分区判定纯函数 | 低 |
| `internal/agent/openai.go` | `TaskProposal` prompt 增加三个可选字段，校验保持宽容 | 低，字段缺失不影响现有行为 |
| `internal/application/app.go` | `ApplyProposal` 落坐标；新增坐标写入、地图与复盘用例 | 中，`ApplyProposal` 是核心路径，需补测试 |
| `internal/httpapi/server.go` | 新增 4 个路由 | 低 |
| `web/src/` | 新增复盘页与地图视图 | 中 |

**不改动**：每日计划生成与确定性排序（§10.3）、墨水屏契约、Panel 摘要接口、外部导入收件箱。坐标能力对这些路径完全是旁路。
