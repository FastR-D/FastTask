# 决策透镜实现规格（面向实现 Agent）

> 文档状态：可执行规格
> 设计依据：`doc/lens.md`（产品判断与裁决，先读它）
> 契约约定：`doc/interface.md`、`doc/arch.md`、`doc/tech.md`

本文档给出可直接落地的实现细节。**设计判断已经定稿，实现时不要重新设计**；如果发现某条规格与现有代码冲突，停下来说明冲突，不要自行改变语义。

---

## 0. 动手前必须知道的仓库约定

这几条是本仓库特有的，写错会导致整套测试挂掉：

1. **Migration 有两份拷贝**。`migrations/` 是发布用副本，`internal/persistence/migrations/` 是 `//go:embed all:migrations/*.sql` 真正加载的那份。**新增 migration 必须同时写入两个目录，且内容逐字节相同**。
2. **`persistence.ExpectedSchemaVersion` 必须同步 +1**（`internal/persistence/store.go:29`）。它不是注释，`/health/ready` 会用 `version != ExpectedSchemaVersion` 判 503。忘记改，所有 HTTP 测试会因为 readiness 失败。
3. **Migration 版本号必须恰好是 `当前版本 + 1`**，`RunMigrations` 有显式的 gap 检查会 fail。
4. **每个 migration 文件结尾必须 `INSERT INTO schema_migrations(version, name, applied_at)`**，参考 `000002_external_imports.up.sql`。
5. **路由通过泛型 helper 注册**：`register[I, O any](api, id, method, path, summary, security, handler)`（`server.go:194`）。新增分组函数要加进 `(*Server).register()` 的列表里（`server.go:177`）。
6. **响应包装类型已有**：`itemResponse[T]`、`listResponse[T]`、`resourceResponse[T]`（带 ETag header）。不要新造。
7. **ETag 用 `application.StrongETag(kind, id, revision)`**，解析用 `revisionFromETag`。格式是 `"kind_id_rev_N"`。
8. **错误必须走 `mapError`**。新增的 sentinel error 如果不加进 `mapError` 的 switch，会被当成 500。现有映射：`ErrNotFound`→404、`ErrRevision`→412、`ErrPrecondition`→428、`ErrConflict`→409、`ErrValidation`→422。
9. **Record 的 `user_id` 一律 `json:"-"`**，不得随响应外泄。
10. **写操作用 `Store.Transaction`**（带 SQLite busy 重试），不要直接 `DB.Transaction`。
11. **乐观锁一律 `WHERE id = ? AND revision = ?` + 检查 `RowsAffected == 1`**，不匹配返回 `ErrRevision`。
12. **时间一律 `persistence.Now()`（UTC）**，ID 一律 `persistence.NewID(prefix)`。

验收命令：

```bash
go build ./... && go vet ./... && go test ./...
cd web && npm run build && npm test
```

---

## L1 · 坐标数据层

### L1.1 Migration

新文件，**两份**：`migrations/000004_task_coords.up.sql` 和 `internal/persistence/migrations/000004_task_coords.up.sql`。

```sql
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

INSERT INTO schema_migrations(version, name, applied_at)
VALUES (4, 'task_coords', CURRENT_TIMESTAMP);
```

同时把 `internal/persistence/store.go` 的 `ExpectedSchemaVersion` 从 `3` 改成 `4`。

> 升级影响：旧数据库在新二进制下必须先跑 `go run ./cmd/fasttask migrate`，否则 readiness 返回 503。这是现有机制的既定行为，不需要额外兼容代码，但发布说明里要提一句。

### L1.2 Record

追加到 `internal/persistence/models.go`（放在 `Task` 之后）：

```go
type TaskCoord struct {
	ID        string    `json:"id"`
	UserID    string    `json:"-"`
	TaskID    string    `json:"task_id"`
	Lens      string    `json:"lens"`
	X         int       `json:"x"`
	Y         int       `json:"y"`
	Source    string    `json:"source"`
	Pinned    bool      `json:"pinned"`
	Rationale string    `json:"rationale"`
	Revision  int       `json:"revision"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
```

GORM 默认表名推导为 `task_coords`，与 migration 一致，无需 `TableName()`。

### L1.3 Domain 纯函数

新文件 `internal/domain/lens.go`。这里只放无依赖的纯逻辑，便于单测。

```go
package domain

const (
	LensResearchRisk = "research_risk"
	CoordThreshold   = 50
	CoordMin         = 0
	CoordMax         = 100
)

var ErrCoord = errors.New("coordinate out of range")
var ErrLens = errors.New("unknown lens")

// ValidLens 目前只认 research_risk；新增透镜时在此扩展。
func ValidLens(lens string) bool

// ValidateCoord 校验 x、y 落在 [0,100]，越界返回 ErrCoord。
func ValidateCoord(x, y int) error

// Quadrant 返回 "A" / "B" / "C" / "D"。
// x = 不确定性，y = 贡献度，边界值 50 归入“高”侧（>= 50 为高）。
//   A 关键风险区：x >= 50 && y >= 50
//   B 主推进区：  x <  50 && y >= 50
//   C 时间黑洞：  x >= 50 && y <  50
//   D 消耗区：    x <  50 && y <  50
func Quadrant(x, y int) string

// QuadrantLabel 返回中文标签，供后端组装响应，前端不硬编码。
func QuadrantLabel(q string) string
```

`ErrCoord` 与 `ErrLens` 都要加进 `httpapi.mapError` 的 `ErrValidation` 分支，映射 422：

```go
case errors.Is(err, application.ErrValidation), errors.Is(err, domain.ErrCycle),
     errors.Is(err, domain.ErrCoord), errors.Is(err, domain.ErrLens):
	return huma.Error422UnprocessableEntity(err.Error())
```

### L1.4 Application：坐标写入

新文件 `internal/application/lens.go`。

```go
// SetTaskCoord 是 (task_id, lens) 上的 upsert。
//
// expected 语义（对齐 interface.md §1.7）：
//   - expected < 0  表示调用方没有提供 If-Match。
//       · 该 (task_id, lens) 尚无坐标 → 创建，revision = 1。
//       · 已存在坐标 → 返回 ErrPrecondition（HTTP 428）。
//   - expected >= 0 表示提供了 If-Match。
//       · 必须与现有 revision 相等，否则 ErrRevision（HTTP 412）。
//       · 不存在坐标时一律 ErrNotFound。
//
// 用户写入一律 source = "user"、pinned = true。
// 校验：任务必须属于该用户（否则 ErrNotFound）；坐标越界返回 domain.ErrCoord；
// 未知 lens 返回 domain.ErrLens。
func (a *App) SetTaskCoord(ctx context.Context, userID, taskID, lens string, x, y int, rationale string, expected int) (*persistence.TaskCoord, error)
```

实现落在一个 `Store.Transaction` 里：先按 `task_id + user_id` 查 `tasks` 确认归属，再查 `task_coords`，然后 `Create` 或 `Updates(... revision+1 ...)`，更新走乐观锁 `WHERE id = ? AND revision = ?` 并检查 `RowsAffected == 1`。

### L1.5 Application：提案落坐标

改 `App.ApplyProposal`（`internal/application/app.go:991`）。

**`op == "create"` 分支**：在 `tx.Create(&task)` 成功、`byID[task.ID] = task` 之后，插入坐标。新建任务不可能已有坐标，直接 create：

```go
if coord, ok := coordFromPatch(patch); ok {
	record := persistence.TaskCoord{
		ID: persistence.NewID("coord"), UserID: userID, TaskID: task.ID,
		Lens: domain.LensResearchRisk, X: coord.X, Y: coord.Y,
		Source: "agent", Pinned: false, Rationale: coord.Rationale,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := tx.Create(&record).Error; err != nil {
		return err
	}
}
```

**`op == "update"` 分支**（`move` / `supersede` 不处理坐标）：若 patch 带坐标且目标任务的坐标 **未 pinned**，则更新；**已 pinned 则静默跳过**，这是 §7.2 的核心不变式。不存在坐标行时按 `source='agent'` 创建。

**`coordFromPatch` 的宽容规则**（放在 `internal/application/lens.go`）：

```go
// coordFromPatch 从 Agent patch 中提取坐标。
// 返回 ok=false 的情形：两个字段都缺失。
// 任一字段缺失或越界 → 该值回落 50，另一值保留，ok=true。
// rationale 取 patch["coord_rationale"]，截断到 120 字节。
//
// 绝不因为坐标字段有问题而让整份提案失败——坐标不是任务树的结构性内容。
func coordFromPatch(patch map[string]any) (struct{ X, Y int; Rationale string }, bool)
```

用已有的 `intValue` / `textValue` 解析，不要新写类型转换。

### L1.6 Agent prompt 扩展

改 `internal/agent/openai.go` 的 `TaskProposal`。在描述 `op=create` 字段的那一行末尾追加：

```
可选坐标字段：uncertainty（0-100，0=完全知道怎么做，100=方法未知需要探索）、contribution（0-100，对目标验收标准的直接贡献）、coord_rationale（一句话说明打分依据，不超过 40 字）。坐标字段可以省略，省略时不要填 0。
```

**校验逻辑不变**：`presentText` 的必填检查里**不要**加坐标字段。坐标缺失是合法输出。

`worker.go:132` 的本地确定性 fallback patch 也补上坐标，让无 LLM 环境下这条链路同样可测：

| 节点 | uncertainty | contribution |
|---|---|---|
| 明确验收路径（milestone） | 40 | 90 |
| 完成第一个可验证推进（task） | 70 | 85 |
| 复盘结果并调整路线（task） | 30 | 60 |

### L1.7 HTTP 路由

新文件 `internal/httpapi/lens.go`，函数 `(*Server) registerLens()`，并加进 `register()` 列表。

**`GET /lenses`** — `operation id: list-lenses`，`userSecurity()`。返回静态定义，前端据此渲染轴标签与分区，不得硬编码：

```json
{
  "items": [{
    "id": "research_risk",
    "title": "科研风险透镜",
    "threshold": 50,
    "x": {"key":"uncertainty","label":"不确定性","min":0,"max":100,"low_label":"知道怎么做","high_label":"方法未知"},
    "y": {"key":"contribution","label":"贡献度","min":0,"max":100,"low_label":"间接","high_label":"直接决定验收"},
    "quadrants": [
      {"key":"A","label":"关键风险区","advice":"尽早验证，拖延成本最高"},
      {"key":"B","label":"主推进区","advice":"稳定产出"},
      {"key":"C","label":"时间黑洞","advice":"降级、拆小或砍掉"},
      {"key":"D","label":"消耗区","advice":"必要，但不应占主要时间"}
    ]
  }]
}
```

**`PUT /tasks/{task_id}/coords`** — `operation id: put-task-coord`，`userSecurity()`，返回 `resourceResponse[persistence.TaskCoord]`，ETag 用 `application.StrongETag("coord", coord.ID, coord.Revision)`。

```go
type putCoordInput struct {
	TaskID  string `path:"task_id"`
	IfMatch string `header:"If-Match"`          // 注意：不加 required:"true"
	Body    struct {
		Lens      string `json:"lens,omitempty"`
		X         int    `json:"x" minimum:"0" maximum:"100"`
		Y         int    `json:"y" minimum:"0" maximum:"100"`
		Rationale string `json:"rationale,omitempty" maxLength:"120"`
	}
}
```

`If-Match` 为空时传 `expected = -1` 给 `SetTaskCoord`；非空时用 `revisionFromETag` 解析。`Lens` 为空默认 `research_risk`。

### L1.8 L1 测试

`internal/domain/lens_test.go`：

| 用例 | 断言 |
|---|---|
| `TestValidateCoordRange` | `0,0` 与 `100,100` 合法；`-1` / `101` 返回 `ErrCoord` |
| `TestQuadrantBoundary` | `(50,50)`→A、`(49,50)`→B、`(50,49)`→C、`(49,49)`→D。边界必须稳定 |
| `TestValidLens` | `research_risk` 为真，其余为假 |

`internal/application/lens_test.go`（复用 `newFixture`）：

| 用例 | 断言 |
|---|---|
| `TestSetTaskCoordCreatesThenRequiresIfMatch` | 首次无 If-Match 创建成功且 `revision==1`、`pinned==true`、`source=="user"`；再次无 If-Match 返回 `ErrPrecondition` |
| `TestSetTaskCoordRevisionMismatch` | 用过期 revision 返回 `ErrRevision` |
| `TestSetTaskCoordRejectsOtherUsersTask` | 返回 `ErrNotFound` |
| `TestSetTaskCoordRejectsOutOfRange` | `x=101` 返回 `domain.ErrCoord` |
| `TestApplyProposalPersistsCoords` | 带坐标的 create patch 应用后，`task_coords` 有对应行，`source=="agent"`、`pinned==false` |
| `TestApplyProposalToleratesMissingCoords` | 不带坐标字段的 patch 仍然应用成功，且不写坐标行 |
| `TestApplyProposalDoesNotOverwritePinnedCoord` | 先用户写入 `(10,90)` 置 pinned，再用 update patch 带 `(80,20)` 应用，坐标仍为 `(10,90)`、revision 不变 |

`internal/httpapi/server_test.go` 追加 `TestLensContract`：`GET /lenses` 返回 200 且含 `research_risk`；`PUT /tasks/{id}/coords` 无 If-Match 首次 200、二次 428、带错误 ETag 412、`x=101` 422；OpenAPI 文本包含 `/tasks/{task_id}/coords` 与 `/lenses`。

---

## L2 · 周复盘

### L2.1 周窗口计算（纯函数）

放在 `internal/domain/lens.go`：

```go
// ISOWeekRange 把 "2026-W37" 解析成该 ISO 周在 loc 时区下的区间。
//   startUTC  = 周一 00:00 本地时间对应的 UTC 瞬时
//   endUTC    = 下周一 00:00 本地时间对应的 UTC 瞬时（半开区间，不含）
//   startDate = 周一的本地日期字符串 "2006-01-02"
//   endDate   = 周日的本地日期字符串
//
// 算法：取该年 1 月 4 日（ISO 定义中必属第 1 周），回退到它所在周的周一，
// 再加 (week-1)*7 天。week 超出 1..53 或格式非法返回 ErrValidation。
func ISOWeekRange(week string, loc *time.Location) (startUTC, endUTC time.Time, startDate, endDate string, err error)

// CurrentISOWeek 返回 t 在 loc 下的 ISO 周标识，格式 "2006-W01"（周号补零到两位）。
func CurrentISOWeek(t time.Time, loc *time.Location) string
```

跨年边界必须测：`2026-W01` 的周一应落在 2025 年 12 月。

### L2.2 聚合

放在 `internal/application/lens.go`。时区解析顺序：请求参数 → `users.timezone` → `Asia/Shanghai`；`time.LoadLocation` 失败时回落 `Asia/Shanghai`，**不报错**。

**本周有效专注分钟（按 task）**：

```sql
SELECT task_id, SUM(duration_seconds) AS seconds
FROM work_sessions
WHERE user_id = ?
  AND status IN ('completed', 'stopped')
  AND duration_seconds > 0
  AND ended_at >= ? AND ended_at < ?
GROUP BY task_id
```

上周同理，换窗口。分钟数一律 `seconds / 60` 整除，不四舍五入。

**距上次实质推进天数**：

```sql
SELECT task_id, MAX(occurred_at) AS last_at
FROM progress_events
WHERE user_id = ? AND task_id IS NOT NULL AND type IN ('result', 'step')
GROUP BY task_id
```

无记录的任务用 `tasks.created_at` 作为起点。天数按本地日期相减，不按 24 小时整除。

**本周证据构成**：

```sql
SELECT dpi.completion_type, COUNT(*) AS count
FROM daily_plan_items dpi
JOIN daily_plans dp ON dp.id = dpi.daily_plan_id
WHERE dpi.user_id = ?
  AND dpi.kind = 'core'
  AND dpi.status = 'satisfied'
  AND dp.local_date >= ? AND dp.local_date <= ?
GROUP BY dpi.completion_type
```

计划项用 `local_date` 字符串比较（它本来就是本地日期），不要换算成 UTC 瞬时。

**坐标与分区**：一次性取回涉及到的 `task_coords`（按 `user_id + lens`）。没有坐标行的任务归入 `unplotted`，其投入分钟计入 `unplotted_minutes`，**不摊进四区**。

### L2.3 响应结构

`GET /reviews/weekly`，`operation id: get-weekly-review`，`userSecurity()`，查询参数 `week`（缺省当周）、`timezone`（可选），返回 `itemResponse[WeeklyReview]`。

```json
{
  "week": "2026-W37",
  "timezone": "Asia/Shanghai",
  "start_date": "2026-09-07",
  "end_date": "2026-09-13",
  "focus": {
    "total_minutes": 672,
    "unplotted_minutes": 30,
    "quadrants": [
      {"key":"A","label":"关键风险区","minutes":95,"share":0.141,"previous_minutes":135,"delta_minutes":-40}
    ]
  },
  "stalled": [
    {"task_id":"task_...","title":"跑通基线实验","goal_id":"goal_...","quadrant":"A","days_since_progress":19}
  ],
  "evidence": {
    "result": 3, "step": 5, "time": 2, "minimum_action": 8,
    "total": 18, "minimum_action_share": 0.444
  },
  "summary": {"source":"template","rule":"stalled_risk","text":"…"},
  "llm_note": ""
}
```

- `share` 与 `minimum_action_share` 为 0–1 浮点，保留三位小数；分母为 0 时一律 0，**不得出现 NaN**。
- `quadrants` 恒定输出 A、B、C、D 四项，零值也要出现，前端不做补齐。
- `stalled` 取 A 区、任务状态不在 `{done, cancelled, superseded}`、且 `days_since_progress >= 14`，按天数倒序，最多 5 条。
- `llm_note` 在 L2 恒为 `""`（见 `lens.md` §9.4）。

### L2.4 总结句模板

纯函数，放 `internal/domain/lens.go`，输入是已经算好的指标结构体，输出 `(rule, text)`。按顺序命中第一条：

| 顺序 | rule | 条件 | 文案 |
|---|---|---|---|
| 1 | `stalled_risk` | `len(stalled) > 0` | `你标为关键风险的「{title}」已 {n} 天没有实质推进。` |
| 2 | `drain_dominant` | D 区占比 > 0.5 | `本周 {pct}% 的专注时间落在低不确定、低贡献的工作上。` |
| 3 | `minimum_action_heavy` | `minimum_action_share > 0.5` | `本周 {total} 个核心项中有 {n} 个只完成了最小行动。` |
| 4 | `risk_improving` | A 区 `delta_minutes > 0` | `本周在关键风险区投入 {n} 分钟，比上周多 {d} 分钟。` |
| 5 | `neutral` | 兜底 | `本周有效专注 {n} 分钟，四区分布：关键风险 {a}、主推进 {b}、时间黑洞 {c}、消耗 {d} 分钟。` |

零数据周（`total_minutes == 0` 且无 stalled）单独走 `rule = "empty"`，文案 `本周没有记录到有效专注时间。`。**不得编造**，不要输出鼓励性评价，不做评分或排名。

### L2.5 前端

`web/src/types.ts` 增加 `WeeklyReview` 及其子结构；`web/src/App.tsx`：

- `Tab` 增加 `'review'`，`navItems` 插入 `['review','复盘']`，排在「设备」之前。
- 新组件 `Review({ onNotice })`：加载 `GET /reviews/weekly`，渲染四区横向条形（用已有 CSS 变量，不引第三方图表库）、环比增量、停滞任务列表、证据构成、总结句。
- 上一周 / 下一周切换按钮，改 `week` 参数重新拉取；不允许跳到未来周。
- 加载中与空周都要有明确文案，空周复用 `empty-state` 样式。

不要引入任何新的 npm 依赖。

### L2.6 L2 测试

`internal/domain/lens_test.go` 追加：`TestISOWeekRangeAcrossYearBoundary`（`2026-W01` 周一在 2025 年 12 月）、`TestISOWeekRangeRejectsBadInput`、`TestSummaryRulePriority`（构造同时命中规则 1 和 2 的输入，断言取规则 1）、`TestSummaryEmptyWeek`。

`internal/application/lens_test.go` 追加：

| 用例 | 断言 |
|---|---|
| `TestWeeklyReviewAggregatesByQuadrant` | 两个不同象限的 session 分别计入对应区 |
| `TestWeeklyReviewExcludesInvalidatedSessions` | `invalidated` 的 session 不计入 `total_minutes` |
| `TestWeeklyReviewIncludesStoppedSessions` | `stopped` 计入 |
| `TestWeeklyReviewUnplottedNotInQuadrants` | 无坐标任务的分钟只进 `unplotted_minutes` |
| `TestWeeklyReviewEmptyWeekIsValid` | 空周返回四个零值象限、`rule == "empty"`、无 error |
| `TestWeeklyReviewTimeEvidenceIsNotProgress` | 只有 `time`/`minimum_action` 事件的任务仍被判为停滞 |

`internal/httpapi/server_test.go` 追加 `TestWeeklyReviewContract`：默认参数 200；非法 `week=2026-W99` 返回 422；OpenAPI 含 `/reviews/weekly`。

---

## L3 · 目标地图

### L3.1 接口

`GET /goals/{goal_id}/map`，`operation id: get-goal-map`，`userSecurity()`，查询参数 `lens`（缺省 `research_risk`）。goal 不属于当前用户返回 404。

```json
{
  "goal_id": "goal_...",
  "lens": "research_risk",
  "threshold": 50,
  "target_date": "2026-12-31",
  "unplotted": 4,
  "nodes": [{
    "task_id": "task_...", "title": "跑通基线实验", "type": "task",
    "status": "ready", "revision": 3,
    "x": 70, "y": 85, "quadrant": "A",
    "source": "agent", "pinned": false, "rationale": "方法未定，直接决定验收",
    "coord_id": "coord_...", "coord_revision": 1,
    "focus_minutes": 125, "days_since_progress": 19
  }]
}
```

节点集合 = 该 goal 下**叶子任务**（`type` 为 `task` 或 `action`，且不是任何任务的 `parent_id`），状态排除 `cancelled` 与 `superseded`。无坐标的任务也要返回，`x`/`y` 给 `50`、`source` 给 `"default"`、`coord_id` 为空字符串；`unplotted` 计其数量。

`focus_minutes` 用 §L2.2 的口径，**全时段累计**（不限本周）。

`coord_revision` 用于前端拖拽后拼 If-Match；`coord_id` 为空时前端不发 If-Match。

### L3.2 前端

在 `Goals` 组件的 `tree-panel` 内加一个「列表 / 地图」切换，默认列表。**默认视图不得改变**。

地图用内联 SVG，不引图表库：

- 视口 `viewBox="0 0 100 100"`，X 直接映射 `uncertainty`，Y 映射 `100 - contribution`（SVG 原点在左上）。
- 四区底色用四块半透明 `rect`，配 `threshold` 分割线与轴标签（标签文案来自 `GET /lenses`）。
- 节点半径 `1.6 + Math.min(focus_minutes, 600) / 150`，即 1.6–5.6。
- 填充色按 `status` 取已有 CSS 变量；`source === 'default'` 用 `fill="none"` + 描边（空心）。
- 透明度 `Math.max(0.35, 1 - days_since_progress / 60)`。
- goal 有 `target_date` 且距今 ≤ 14 天、任务未完成时，加高亮描边。
- 指针拖拽（`onPointerDown/Move/Up`）更新坐标，松手时 `PUT /tasks/{id}/coords`，带 `If-Match: "coord_{coord_id}_rev_{coord_revision}"`（`coord_id` 为空则不带）。失败回滚到原位置并 `onNotice` 报错。
- 点击（未拖动）打开该任务详情。

移动端：SVG 宽度 100%，`touch-action: none` 只加在 SVG 上，避免影响页面滚动。

### L3.3 L3 测试

`internal/httpapi/server_test.go` 追加 `TestGoalMapContract`：创建 goal + 两个任务，其一写坐标；断言 `nodes` 长度、`unplotted == 1`、无坐标节点的 `x/y` 为 50 且 `source == "default"`；他人 goal 返回 404。

---

## 文档同步（实现完成前必须做）

1. **`doc/interface.md`**：新增一节「决策透镜与周复盘」，按 §17 的要求登记 4 个 Operation 的 ID、Method、Path、Security、状态码、幂等与 Revision 说明。放在 §16 之后、§17 之前，后续章节序号顺延。
2. **`doc/func.md` §3.2**：把「更完整的周报、复盘和长期趋势分析」从 P1 待办改为已交付，并链接 `doc/lens.md`。
3. **`README.md` 核心能力**：加一条「任务坐标透镜、周复盘与可选的目标地图」。
4. **`doc/arch.md` 数据模型章节**：登记 `task_coords` 表。

---

## 完成定义（DoD）

- [ ] `go build ./... && go vet ./... && go test ./...` 全绿。
- [ ] `cd web && npm run build && npm test` 全绿。
- [ ] 两份 migration 内容一致（`diff migrations/000004_task_coords.up.sql internal/persistence/migrations/000004_task_coords.up.sql` 无输出）。
- [ ] `ExpectedSchemaVersion == 4`，`/health/ready` 在迁移后返回 200。
- [ ] 旧库升级路径验证过：用 v3 库跑 `migrate` 后 readiness 正常，既有数据未丢。
- [ ] 每日计划相关测试**一个都没改动**（见下方禁止清单）。
- [ ] `doc/interface.md` 已登记新接口。

---

## 禁止清单

实现过程中**不得修改**下列内容。若认为必须改，停下来说明原因，不要自行决定：

- `domain.SelectDailyCandidates` 及每日计划的确定性排序逻辑。
- `App.CreateDailyPlan` / `ReplanDailyPlan` / `CompletePlanItem` 的既有语义。
- 墨水屏 Poll 的 Token / ETag / 304 契约。
- Panel 摘要接口。
- 外部导入收件箱（`external_imports`）的任何行为。
- `tasks` 表结构。坐标一律落在 `task_coords`，**不要往 `tasks` 加列**。
- 现有 migration 文件 000001–000003 的内容。

坐标能力对以上路径全部是旁路。任何让它们产生行为差异的改动都说明实现跑偏了。
