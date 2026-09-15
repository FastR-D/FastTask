package application

import (
	"context"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/FastR-D/FastTask/internal/domain"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

const (
	coordRationaleMaxBytes = 120
	defaultTimezone        = "Asia/Shanghai"
	maxStalledTasks        = 5
)

// SetTaskCoord 是 (task_id, lens) 上的 upsert，用户写入一律 source=user、pinned=true。
//
// expected < 0 表示调用方没有提供 If-Match：无坐标时创建（revision=1），已有坐标返回 ErrPrecondition。
// expected >= 0 表示提供了 If-Match：必须与现有 revision 相等，坐标不存在返回 ErrNotFound。
func (a *App) SetTaskCoord(ctx context.Context, userID, taskID, lens string, x, y int, rationale string, expected int) (*persistence.TaskCoord, error) {
	lens = normalizeLens(lens)
	if !domain.ValidLens(lens) {
		return nil, domain.ErrLens
	}
	if err := domain.ValidateCoord(x, y); err != nil {
		return nil, err
	}
	rationale = truncateRationale(rationale)
	coord := persistence.TaskCoord{}
	err := a.Store.Transaction(ctx, func(tx *gorm.DB) error {
		var task persistence.Task
		if err := tx.Where("id = ? AND user_id = ?", taskID, userID).First(&task).Error; err != nil {
			return notFound(err)
		}
		existing, found, err := findCoord(tx, userID, taskID, lens)
		if err != nil {
			return err
		}
		now := persistence.Now()
		if !found {
			if expected >= 0 {
				return ErrNotFound
			}
			coord = persistence.TaskCoord{ID: persistence.NewID("coord"), UserID: userID, TaskID: taskID, Lens: lens, X: x, Y: y, Source: domain.CoordSourceUser, Pinned: true, Rationale: rationale, Revision: 1, CreatedAt: now, UpdatedAt: now}
			return tx.Create(&coord).Error
		}
		if expected < 0 {
			return ErrPrecondition
		}
		if existing.Revision != expected {
			return ErrRevision
		}
		result := tx.Model(&persistence.TaskCoord{}).Where("id = ? AND revision = ?", existing.ID, expected).Updates(map[string]any{
			"x": x, "y": y, "source": domain.CoordSourceUser, "pinned": true,
			"rationale": rationale, "revision": expected + 1, "updated_at": now,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrRevision
		}
		return tx.Where("id = ?", existing.ID).First(&coord).Error
	})
	if err != nil {
		return nil, err
	}
	return &coord, nil
}

type agentCoord struct {
	X         int
	Y         int
	Rationale string
}

// coordFromPatch 从 Agent patch 中提取坐标。两个字段都缺失时 ok=false；
// 任一字段缺失或越界则该值回落 50，另一值保留，ok=true。
// 坐标不是任务树的结构性内容，绝不因为坐标字段有问题让整份提案失败。
func coordFromPatch(patch map[string]any) (agentCoord, bool) {
	rawX, hasX := patch["uncertainty"]
	rawY, hasY := patch["contribution"]
	if !hasX && !hasY {
		return agentCoord{}, false
	}
	coord := agentCoord{X: domain.CoordThreshold, Y: domain.CoordThreshold, Rationale: truncateRationale(textValue(patch["coord_rationale"]))}
	if value, ok := coordValue(rawX); ok {
		coord.X = value
	}
	if value, ok := coordValue(rawY); ok {
		coord.Y = value
	}
	return coord, true
}

func coordValue(value any) (int, bool) {
	if value == nil {
		return 0, false
	}
	parsed := intValue(value)
	if parsed < domain.CoordMin || parsed > domain.CoordMax {
		return 0, false
	}
	return parsed, true
}

// createAgentCoord 为新建任务写入 Agent 坐标。新建任务不可能已有坐标。
func createAgentCoord(tx *gorm.DB, userID, taskID string, coord agentCoord, now time.Time) error {
	record := persistence.TaskCoord{
		ID: persistence.NewID("coord"), UserID: userID, TaskID: taskID,
		Lens: domain.LensResearchRisk, X: coord.X, Y: coord.Y,
		Source: domain.CoordSourceAgent, Pinned: false, Rationale: coord.Rationale,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	return tx.Create(&record).Error
}

// updateAgentCoord 更新已有任务的 Agent 坐标；pinned 的坐标静默跳过，Agent 不得覆盖用户数据。
func updateAgentCoord(tx *gorm.DB, userID, taskID string, coord agentCoord, now time.Time) error {
	existing, found, err := findCoord(tx, userID, taskID, domain.LensResearchRisk)
	if err != nil {
		return err
	}
	if !found {
		return createAgentCoord(tx, userID, taskID, coord, now)
	}
	if existing.Pinned {
		return nil
	}
	result := tx.Model(&persistence.TaskCoord{}).Where("id = ? AND revision = ?", existing.ID, existing.Revision).Updates(map[string]any{
		"x": coord.X, "y": coord.Y, "source": domain.CoordSourceAgent,
		"rationale": coord.Rationale, "revision": existing.Revision + 1, "updated_at": now,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrRevision
	}
	return nil
}

func findCoord(tx *gorm.DB, userID, taskID, lens string) (persistence.TaskCoord, bool, error) {
	var coord persistence.TaskCoord
	err := tx.Where("task_id = ? AND user_id = ? AND lens = ?", taskID, userID, lens).First(&coord).Error
	if err == nil {
		return coord, true, nil
	}
	if persistence.IsNotFound(err) {
		return coord, false, nil
	}
	return coord, false, err
}

func normalizeLens(lens string) string {
	if strings.TrimSpace(lens) == "" {
		return domain.LensResearchRisk
	}
	return strings.TrimSpace(lens)
}

func truncateRationale(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= coordRationaleMaxBytes {
		return value
	}
	cut := coordRationaleMaxBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

func round3(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return math.Round(value*1000) / 1000
}

func share(part, total int) float64 {
	if total <= 0 {
		return 0
	}
	return round3(float64(part) / float64(total))
}

// resolveZone 按 请求参数 → users.timezone → Asia/Shanghai 解析时区，失败一律回落，不报错。
func resolveZone(requested ...string) (*time.Location, string) {
	for _, candidate := range requested {
		name := strings.TrimSpace(candidate)
		if name == "" {
			continue
		}
		if location, err := time.LoadLocation(name); err == nil {
			return location, name
		}
	}
	location, err := time.LoadLocation(defaultTimezone)
	if err != nil {
		return time.FixedZone("CST", 8*3600), defaultTimezone
	}
	return location, defaultTimezone
}

func focusMinutesByTask(db *gorm.DB, userID string, start, end *time.Time) (map[string]int, error) {
	type row struct {
		TaskID  string `gorm:"column:task_id"`
		Seconds int    `gorm:"column:seconds"`
	}
	query := db.Table("work_sessions").
		Select("task_id, SUM(duration_seconds) AS seconds").
		Where("user_id = ?", userID).
		Where("status IN ?", []string{"completed", "stopped"}).
		Where("duration_seconds > 0")
	if start != nil && end != nil {
		query = query.Where("ended_at >= ? AND ended_at < ?", *start, *end)
	}
	var rows []row
	if err := query.Group("task_id").Scan(&rows).Error; err != nil {
		return nil, err
	}
	minutes := make(map[string]int, len(rows))
	for _, item := range rows {
		if item.TaskID == "" {
			continue
		}
		minutes[item.TaskID] = item.Seconds / 60
	}
	return minutes, nil
}

func lastProgressByTask(db *gorm.DB, userID string) (map[string]time.Time, error) {
	var rows []map[string]any
	err := db.Table("progress_events").
		Select("task_id, MAX(occurred_at) AS last_at").
		Where("user_id = ? AND task_id IS NOT NULL AND type IN ?", userID, []string{"result", "step"}).
		Group("task_id").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	result := make(map[string]time.Time, len(rows))
	for _, item := range rows {
		taskID, _ := item["task_id"].(string)
		if taskID == "" {
			continue
		}
		if at, ok := storedTime(item["last_at"]); ok {
			result[taskID] = at
		}
	}
	return result, nil
}

func storedTime(value any) (time.Time, bool) {
	switch typed := value.(type) {
	case time.Time:
		return typed, !typed.IsZero()
	case []byte:
		return storedTime(string(typed))
	case string:
		for _, layout := range []string{
			"2006-01-02 15:04:05.999999999-07:00",
			"2006-01-02T15:04:05.999999999-07:00",
			"2006-01-02 15:04:05.999999999Z07:00",
			time.RFC3339Nano,
			"2006-01-02 15:04:05",
			"2006-01-02T15:04:05",
			"2006-01-02",
		} {
			if parsed, err := time.Parse(layout, strings.TrimSpace(typed)); err == nil {
				return parsed, true
			}
		}
		return time.Time{}, false
	default:
		// GORM 把聚合列扫描成 *any，这里统一解引用后再解析。
		reflected := reflect.ValueOf(value)
		if reflected.Kind() == reflect.Pointer {
			if reflected.IsNil() {
				return time.Time{}, false
			}
			return storedTime(reflected.Elem().Interface())
		}
		return time.Time{}, false
	}
}

func daysSinceProgress(last, fallback, now time.Time, loc *time.Location) int {
	reference := last
	if reference.IsZero() {
		reference = fallback
	}
	if reference.IsZero() {
		return 0
	}
	return domain.LocalDaysBetween(reference, now, loc)
}

func isClosedTask(status string) bool {
	switch status {
	case "completed", "done", "cancelled", "superseded":
		return true
	default:
		return false
	}
}

type ReviewQuadrant struct {
	Key             string  `json:"key"`
	Label           string  `json:"label"`
	Minutes         int     `json:"minutes"`
	Share           float64 `json:"share"`
	PreviousMinutes int     `json:"previous_minutes"`
	DeltaMinutes    int     `json:"delta_minutes"`
}

type ReviewFocus struct {
	TotalMinutes     int              `json:"total_minutes"`
	UnplottedMinutes int              `json:"unplotted_minutes"`
	Quadrants        []ReviewQuadrant `json:"quadrants"`
}

type ReviewEvidence struct {
	Result             int     `json:"result"`
	Step               int     `json:"step"`
	Time               int     `json:"time"`
	MinimumAction      int     `json:"minimum_action"`
	Total              int     `json:"total"`
	MinimumActionShare float64 `json:"minimum_action_share"`
}

type ReviewSummary struct {
	Source string `json:"source"`
	Rule   string `json:"rule"`
	Text   string `json:"text"`
}

type WeeklyReview struct {
	Week      string               `json:"week"`
	Timezone  string               `json:"timezone"`
	StartDate string               `json:"start_date"`
	EndDate   string               `json:"end_date"`
	Focus     ReviewFocus          `json:"focus"`
	Stalled   []domain.StalledTask `json:"stalled"`
	Evidence  ReviewEvidence       `json:"evidence"`
	Summary   ReviewSummary        `json:"summary"`
	LLMNote   string               `json:"llm_note"`
}

// WeeklyReview 实时聚合一周的确定性指标。只读，不落库，不同步调用 LLM。
func (a *App) WeeklyReview(ctx context.Context, userID, week, timezone string) (*WeeklyReview, error) {
	db := a.Store.DB.WithContext(ctx)
	var user persistence.User
	if err := db.Where("id = ?", userID).First(&user).Error; err != nil {
		return nil, notFound(err)
	}
	loc, zone := resolveZone(timezone, user.Timezone)
	now := persistence.Now()
	if strings.TrimSpace(week) == "" {
		week = domain.CurrentISOWeek(now, loc)
	}
	start, end, startDate, endDate, err := domain.ISOWeekRange(week, loc)
	if err != nil {
		return nil, ErrValidation
	}
	previousStart, previousEnd, _, _, err := domain.ISOWeekRange(domain.CurrentISOWeek(start.AddDate(0, 0, -1), loc), loc)
	if err != nil {
		return nil, ErrValidation
	}
	current, err := focusMinutesByTask(db, userID, &start, &end)
	if err != nil {
		return nil, err
	}
	previous, err := focusMinutesByTask(db, userID, &previousStart, &previousEnd)
	if err != nil {
		return nil, err
	}
	progress, err := lastProgressByTask(db, userID)
	if err != nil {
		return nil, err
	}
	coords, err := coordsByTask(db, userID, domain.LensResearchRisk)
	if err != nil {
		return nil, err
	}

	quadrantMinutes := map[string]int{}
	previousMinutes := map[string]int{}
	total, unplotted := 0, 0
	for taskID, minutes := range current {
		total += minutes
		coord, plotted := coords[taskID]
		if !plotted {
			unplotted += minutes
			continue
		}
		quadrantMinutes[domain.Quadrant(coord.X, coord.Y)] += minutes
	}
	for taskID, minutes := range previous {
		coord, plotted := coords[taskID]
		if !plotted {
			continue
		}
		previousMinutes[domain.Quadrant(coord.X, coord.Y)] += minutes
	}

	tasks, err := tasksByID(db, userID, coordTaskIDs(coords))
	if err != nil {
		return nil, err
	}
	stalled := make([]domain.StalledTask, 0)
	for taskID, coord := range coords {
		if domain.Quadrant(coord.X, coord.Y) != domain.QuadrantCriticalRisk {
			continue
		}
		task, ok := tasks[taskID]
		if !ok || isClosedTask(task.Status) {
			continue
		}
		days := daysSinceProgress(progress[taskID], task.CreatedAt, now, loc)
		if days < domain.StalledProgressDays {
			continue
		}
		stalled = append(stalled, domain.StalledTask{TaskID: taskID, Title: task.Title, GoalID: task.GoalID, Quadrant: domain.QuadrantCriticalRisk, DaysSinceProgress: days})
	}
	sort.SliceStable(stalled, func(i, j int) bool {
		if stalled[i].DaysSinceProgress != stalled[j].DaysSinceProgress {
			return stalled[i].DaysSinceProgress > stalled[j].DaysSinceProgress
		}
		return stalled[i].TaskID < stalled[j].TaskID
	})
	if len(stalled) > maxStalledTasks {
		stalled = stalled[:maxStalledTasks]
	}

	evidence, err := weeklyEvidence(db, userID, startDate, endDate)
	if err != nil {
		return nil, err
	}

	quadrants := make([]ReviewQuadrant, 0, 4)
	delta := map[string]int{}
	for _, key := range domain.Quadrants() {
		minutes, before := quadrantMinutes[key], previousMinutes[key]
		delta[key] = minutes - before
		quadrants = append(quadrants, ReviewQuadrant{
			Key: key, Label: domain.QuadrantLabel(key), Minutes: minutes, Share: share(minutes, total),
			PreviousMinutes: before, DeltaMinutes: minutes - before,
		})
	}

	rule, text := domain.WeeklySummary(domain.WeeklySummaryInput{
		TotalMinutes:       total,
		QuadrantMinutes:    quadrantMinutes,
		QuadrantDelta:      delta,
		EvidenceTotal:      evidence.Total,
		MinimumActionCount: evidence.MinimumAction,
		MinimumActionShare: evidence.MinimumActionShare,
		Stalled:            stalled,
	})

	return &WeeklyReview{
		Week:      week,
		Timezone:  zone,
		StartDate: startDate,
		EndDate:   endDate,
		Focus:     ReviewFocus{TotalMinutes: total, UnplottedMinutes: unplotted, Quadrants: quadrants},
		Stalled:   stalled,
		Evidence:  evidence,
		Summary:   ReviewSummary{Source: "template", Rule: rule, Text: text},
		LLMNote:   "",
	}, nil
}

func weeklyEvidence(db *gorm.DB, userID, startDate, endDate string) (ReviewEvidence, error) {
	type row struct {
		CompletionType string `gorm:"column:completion_type"`
		Count          int    `gorm:"column:count"`
	}
	var rows []row
	// daily_plan_items 的计划外键列名是 plan_id（见 migration 000001）。
	err := db.Table("daily_plan_items AS dpi").
		Select("dpi.completion_type, COUNT(*) AS count").
		Joins("JOIN daily_plans dp ON dp.id = dpi.plan_id").
		Where("dpi.user_id = ?", userID).
		Where("dpi.kind = ?", "core").
		Where("dpi.status = ?", "satisfied").
		Where("dp.local_date >= ? AND dp.local_date <= ?", startDate, endDate).
		Group("dpi.completion_type").
		Scan(&rows).Error
	if err != nil {
		return ReviewEvidence{}, err
	}
	evidence := ReviewEvidence{}
	for _, item := range rows {
		switch item.CompletionType {
		case "result":
			evidence.Result += item.Count
		case "step":
			evidence.Step += item.Count
		case "time":
			evidence.Time += item.Count
		case "minimum_action":
			evidence.MinimumAction += item.Count
		}
		evidence.Total += item.Count
	}
	evidence.MinimumActionShare = share(evidence.MinimumAction, evidence.Total)
	return evidence, nil
}

func coordsByTask(db *gorm.DB, userID, lens string) (map[string]persistence.TaskCoord, error) {
	var coords []persistence.TaskCoord
	if err := db.Where("user_id = ? AND lens = ?", userID, lens).Find(&coords).Error; err != nil {
		return nil, err
	}
	result := make(map[string]persistence.TaskCoord, len(coords))
	for _, coord := range coords {
		result[coord.TaskID] = coord
	}
	return result, nil
}

func coordTaskIDs(coords map[string]persistence.TaskCoord) []string {
	ids := make([]string, 0, len(coords))
	for taskID := range coords {
		ids = append(ids, taskID)
	}
	sort.Strings(ids)
	return ids
}

func tasksByID(db *gorm.DB, userID string, ids []string) (map[string]persistence.Task, error) {
	result := map[string]persistence.Task{}
	if len(ids) == 0 {
		return result, nil
	}
	var tasks []persistence.Task
	if err := db.Where("user_id = ? AND id IN ?", userID, ids).Find(&tasks).Error; err != nil {
		return nil, err
	}
	for _, task := range tasks {
		result[task.ID] = task
	}
	return result, nil
}

type GoalMapNode struct {
	TaskID            string `json:"task_id"`
	Title             string `json:"title"`
	Type              string `json:"type"`
	Status            string `json:"status"`
	Revision          int    `json:"revision"`
	X                 int    `json:"x"`
	Y                 int    `json:"y"`
	Quadrant          string `json:"quadrant"`
	Source            string `json:"source"`
	Pinned            bool   `json:"pinned"`
	Rationale         string `json:"rationale"`
	CoordID           string `json:"coord_id"`
	CoordRevision     int    `json:"coord_revision"`
	FocusMinutes      int    `json:"focus_minutes"`
	DaysSinceProgress int    `json:"days_since_progress"`
}

type GoalMap struct {
	GoalID     string        `json:"goal_id"`
	Lens       string        `json:"lens"`
	Threshold  int           `json:"threshold"`
	TargetDate string        `json:"target_date"`
	Unplotted  int           `json:"unplotted"`
	Nodes      []GoalMapNode `json:"nodes"`
}

// GoalMap 返回一个 goal 的全部叶子任务及其坐标、累计有效专注分钟和距上次实质推进天数。
func (a *App) GoalMap(ctx context.Context, userID, goalID, lens string) (*GoalMap, error) {
	lens = normalizeLens(lens)
	if !domain.ValidLens(lens) {
		return nil, domain.ErrLens
	}
	db := a.Store.DB.WithContext(ctx)
	var goal persistence.Goal
	if err := db.Where("id = ? AND user_id = ?", goalID, userID).First(&goal).Error; err != nil {
		return nil, notFound(err)
	}
	var user persistence.User
	if err := db.Where("id = ?", userID).First(&user).Error; err != nil {
		return nil, notFound(err)
	}
	loc, _ := resolveZone(user.Timezone)
	now := persistence.Now()

	var tasks []persistence.Task
	if err := db.Where("goal_id = ? AND user_id = ?", goalID, userID).Order("position, id").Find(&tasks).Error; err != nil {
		return nil, err
	}
	parents := map[string]bool{}
	for _, task := range tasks {
		if task.ParentID != nil && *task.ParentID != "" {
			parents[*task.ParentID] = true
		}
	}
	coords, err := coordsByTask(db, userID, lens)
	if err != nil {
		return nil, err
	}
	focus, err := focusMinutesByTask(db, userID, nil, nil)
	if err != nil {
		return nil, err
	}
	progress, err := lastProgressByTask(db, userID)
	if err != nil {
		return nil, err
	}

	nodes := make([]GoalMapNode, 0, len(tasks))
	unplotted := 0
	for _, task := range tasks {
		if task.Type != "task" && task.Type != "action" {
			continue
		}
		if parents[task.ID] || task.Status == "cancelled" || task.Status == "superseded" {
			continue
		}
		node := GoalMapNode{
			TaskID: task.ID, Title: task.Title, Type: task.Type, Status: task.Status, Revision: task.Revision,
			X: domain.CoordThreshold, Y: domain.CoordThreshold, Source: domain.CoordSourceDefault,
			FocusMinutes:      focus[task.ID],
			DaysSinceProgress: daysSinceProgress(progress[task.ID], task.CreatedAt, now, loc),
		}
		if coord, plotted := coords[task.ID]; plotted {
			node.X, node.Y = coord.X, coord.Y
			node.Source, node.Pinned, node.Rationale = coord.Source, coord.Pinned, coord.Rationale
			node.CoordID, node.CoordRevision = coord.ID, coord.Revision
		} else {
			unplotted++
		}
		node.Quadrant = domain.Quadrant(node.X, node.Y)
		nodes = append(nodes, node)
	}
	targetDate := ""
	if goal.TargetDate != nil {
		targetDate = *goal.TargetDate
	}
	return &GoalMap{GoalID: goal.ID, Lens: lens, Threshold: domain.CoordThreshold, TargetDate: targetDate, Unplotted: unplotted, Nodes: nodes}, nil
}
