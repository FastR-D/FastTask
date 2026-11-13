package application

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/domain"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

// ReviewActivityItem is a projection of a saved record, never a new completion.
type ReviewActivityItem struct {
	ID              string    `json:"id"`
	Source          string    `json:"source"`
	Kind            string    `json:"kind"`
	Title           string    `json:"title"`
	Summary         string    `json:"summary"`
	OccurredAt      time.Time `json:"occurred_at"`
	TimeBasis       string    `json:"time_basis"`
	TaskID          string    `json:"task_id,omitempty"`
	TaskTitle       string    `json:"task_title,omitempty"`
	GoalID          string    `json:"goal_id,omitempty"`
	GoalTitle       string    `json:"goal_title,omitempty"`
	Status          string    `json:"status"`
	URL             string    `json:"url,omitempty"`
	Version         string    `json:"version,omitempty"`
	DurationMinutes int       `json:"duration_minutes,omitempty"`
	Attribution     string    `json:"attribution"`
	RelatedEventIDs []string  `json:"related_event_ids,omitempty"`
}

type ReviewUpcoming struct {
	Date          string `json:"date" gorm:"column:local_date"`
	TaskID        string `json:"task_id"`
	Title         string `json:"title"`
	GoalID        string `json:"goal_id"`
	TargetMinutes int    `json:"target_minutes"`
	Status        string `json:"status"`
}

type ReviewActivity struct {
	Week         string               `json:"week"`
	Timezone     string               `json:"timezone"`
	StartDate    string               `json:"start_date"`
	EndDate      string               `json:"end_date"`
	Items        []ReviewActivityItem `json:"items"`
	Total        int                  `json:"total"`
	HasMore      bool                 `json:"has_more"`
	NextOffset   int                  `json:"next_offset"`
	Complete     bool                 `json:"complete"`
	FocusMinutes int                  `json:"focus_minutes"`
	Counts       map[string]int       `json:"counts"`
	Upcoming     []ReviewUpcoming     `json:"upcoming"`
	Warnings     []string             `json:"warnings"`
}

const reviewScanLimit = 10000

type reviewEventEvidence struct {
	DailyCompletionEventID string   `json:"daily_completion_event_id"`
	ReversesEventID        string   `json:"reverses_event_id"`
	LogicalID              string   `json:"logical_id"`
	WorkSessionIDs         []string `json:"work_session_ids"`
}

// ReviewActivity reads a consistent local snapshot. Unlike plan reads it does
// not generate, advance, or reschedule any work, even when the next week is empty.
func (a *LensService) ReviewActivity(ctx context.Context, userID, week, timezone string, limit, offset int) (*ReviewActivity, error) {
	if limit < 1 || limit > 200 || offset < 0 || offset > 1000000 {
		return nil, ErrValidation
	}
	result := &ReviewActivity{Items: []ReviewActivityItem{}, Upcoming: []ReviewUpcoming{}, Warnings: []string{}, Counts: map[string]int{"result": 0, "step": 0, "minimum_action": 0, "time": 0}, Complete: true}
	err := reviewReadSnapshot(a.Store.DB.WithContext(ctx), func(tx *gorm.DB) error {
		var user persistence.User
		if err := tx.Where("id = ?", userID).First(&user).Error; err != nil {
			return notFound(err)
		}
		loc, zone := resolveZone(timezone, user.Timezone)
		week = strings.TrimSpace(week)
		if week == "" {
			week = domain.CurrentISOWeek(persistence.Now(), loc)
		}
		result.Week, result.Timezone = week, zone
		rangeWeek := week
		if week == "all" {
			rangeWeek = domain.CurrentISOWeek(persistence.Now(), loc)
		}
		start, end, startDate, endDate, err := domain.ISOWeekRange(rangeWeek, loc)
		if err != nil {
			return ErrValidation
		}
		if week != "all" {
			result.StartDate, result.EndDate = startDate, endDate
		}
		inRange := func(query *gorm.DB, column string) *gorm.DB {
			if week != "all" {
				return query.Where(column+" >= ? AND "+column+" < ?", start, end)
			}
			return query
		}
		var events []persistence.ProgressEvent
		if err := inRange(tx.Where("user_id = ? AND type IN ?", userID, []string{"result", "step", "minimum_action", "time"}), "occurred_at").Order("occurred_at DESC, id DESC").Limit(reviewScanLimit + 1).Find(&events).Error; err != nil {
			return err
		}
		if len(events) > reviewScanLimit {
			events = events[:reviewScanLimit]
			result.Complete = false
		}
		var sessions []persistence.WorkSession
		if err := inRange(tx.Where("user_id = ? AND status IN ? AND duration_seconds > 0 AND ended_at IS NOT NULL", userID, []string{"completed", "stopped"}), "ended_at").Order("ended_at DESC, id DESC").Limit(reviewScanLimit + 1).Find(&sessions).Error; err != nil {
			return err
		}
		if len(sessions) > reviewScanLimit {
			sessions = sessions[:reviewScanLimit]
			result.Complete = false
		}
		taskIDs, goalIDs, eventIDs := []string{}, []string{}, []string{}
		for _, event := range events {
			eventIDs = append(eventIDs, event.ID)
			if event.TaskID != nil {
				taskIDs = append(taskIDs, *event.TaskID)
			}
			if event.GoalID != nil {
				goalIDs = append(goalIDs, *event.GoalID)
			}
		}
		for _, session := range sessions {
			taskIDs = append(taskIDs, session.TaskID)
		}
		tasks, err := reviewOwnedRows[persistence.Task](tx, userID, "id", taskIDs)
		if err != nil {
			return err
		}
		taskMap := map[string]persistence.Task{}
		for _, task := range tasks {
			taskMap[task.ID] = task
			goalIDs = append(goalIDs, task.GoalID)
		}
		goals, err := reviewOwnedRows[persistence.Goal](tx, userID, "id", goalIDs)
		if err != nil {
			return err
		}
		goalMap := map[string]persistence.Goal{}
		for _, goal := range goals {
			goalMap[goal.ID] = goal
		}
		reversals, err := reviewOwnedRows[persistence.ProgressEvent](tx, userID, "type = 'daily_item_reopened' AND json_valid(evidence_json) AND json_extract(evidence_json, '$.reverses_event_id')", eventIDs)
		if err != nil {
			return err
		}
		reversed := map[string]bool{}
		for _, event := range reversals {
			var e reviewEventEvidence
			_ = json.Unmarshal([]byte(event.EvidenceJSON), &e)
			reversed[e.ReversesEventID] = true
		}
		reopenings, err := reviewOwnedRows[persistence.ProgressEvent](tx, userID, "type = 'task_reopened' AND task_id", taskIDs)
		if err != nil {
			return err
		}
		reopened := map[string]time.Time{}
		for _, event := range reopenings {
			if event.TaskID != nil && event.OccurredAt.After(reopened[*event.TaskID]) {
				reopened[*event.TaskID] = event.OccurredAt
			}
		}
		// A joint completion writes two related events. Keep the final-outcome
		// card; reversing the daily step does not undo that independent outcome.
		joint := map[string]string{}
		for _, event := range events {
			if event.PlanItemID == nil {
				var e reviewEventEvidence
				_ = json.Unmarshal([]byte(event.EvidenceJSON), &e)
				if e.DailyCompletionEventID != "" {
					joint[e.DailyCompletionEventID] = event.ID
				}
			}
		}
		decorate := func(item *ReviewActivityItem, taskID, goalID string) {
			if task, ok := taskMap[taskID]; ok {
				item.TaskID, item.TaskTitle = task.ID, task.Title
				goalID = task.GoalID
			}
			if goal, ok := goalMap[goalID]; ok {
				item.GoalID, item.GoalTitle = goal.ID, goal.Title
			}
			if item.Title == "" {
				item.Title = item.TaskTitle
			}
			if item.Title == "" {
				item.Title = "已记录的推进"
			}
		}
		sessionMap := map[string]persistence.WorkSession{}
		for _, session := range sessions {
			sessionMap[session.ID] = session
		}
		usedSessions := map[string]bool{}
		for _, event := range events {
			if joint[event.ID] != "" {
				continue
			}
			item := ReviewActivityItem{ID: event.ID, Source: "fasttask", Kind: event.Type, Summary: event.Summary, OccurredAt: event.OccurredAt, TimeBasis: "occurred_at", Status: "recorded", Attribution: "用户记录"}
			taskID, goalID := "", ""
			if event.TaskID != nil {
				taskID = *event.TaskID
			}
			if event.GoalID != nil {
				goalID = *event.GoalID
			}
			decorate(&item, taskID, goalID)
			if event.PlanItemID == nil && (event.Type == "result" || event.Type == "step") {
				item.Status = "completed"
				if reopened[taskID].After(event.OccurredAt) {
					item.Status = "reopened"
				}
			}
			if reversed[event.ID] {
				item.Status = "retracted"
			}
			var e reviewEventEvidence
			_ = json.Unmarshal([]byte(event.EvidenceJSON), &e)
			if e.DailyCompletionEventID != "" {
				item.RelatedEventIDs = []string{e.DailyCompletionEventID}
			}
			if event.Type == "time" && item.Status != "retracted" {
				seconds := 0
				for _, id := range e.WorkSessionIDs {
					if session, ok := sessionMap[id]; ok && session.TaskID == taskID && !usedSessions[id] {
						seconds += session.DurationSeconds
						usedSessions[id] = true
					}
				}
				item.DurationMinutes = seconds / 60
			}
			result.Items = append(result.Items, item)
		}
		for _, session := range sessions {
			if usedSessions[session.ID] {
				continue
			}
			item := ReviewActivityItem{ID: session.ID, Source: "fasttask", Kind: "time", Summary: session.Note, OccurredAt: *session.EndedAt, TimeBasis: "occurred_at", Status: "recorded", Attribution: "专注记录", DurationMinutes: session.DurationSeconds / 60}
			decorate(&item, session.TaskID, "")
			result.Items = append(result.Items, item)
		}
		// Sum seconds before rounding; separate from evidence-card counts.
		var seconds int64
		if err := inRange(tx.Model(&persistence.WorkSession{}).Where("user_id = ? AND status IN ? AND duration_seconds > 0 AND ended_at IS NOT NULL", userID, []string{"completed", "stopped"}), "ended_at").Select("COALESCE(SUM(duration_seconds), 0)").Scan(&seconds).Error; err != nil {
			return err
		}
		result.FocusMinutes = int(seconds / 60)
		for _, item := range result.Items {
			if item.Status != "retracted" && item.Status != "reopened" {
				result.Counts[item.Kind]++
			}
		}
		sort.Slice(result.Items, func(i, j int) bool {
			if result.Items[i].OccurredAt.Equal(result.Items[j].OccurredAt) {
				return result.Items[i].ID > result.Items[j].ID
			}
			return result.Items[i].OccurredAt.After(result.Items[j].OccurredAt)
		})
		result.Total = len(result.Items)
		from := offset
		if from > result.Total {
			from = result.Total
		}
		to := from + limit
		if to > result.Total {
			to = result.Total
		}
		result.Items = result.Items[from:to]
		result.HasMore = to < result.Total
		result.NextOffset = to
		if !result.Complete {
			result.Warnings = append(result.Warnings, "任务记录较多，当前汇总包含最近 10000 条任务进展及最近 10000 条专注记录；请按周筛选后生成完整周报。")
		}
		// Only already persisted current plan revisions are read. Missing next
		// week plans stay empty; querying a report must never create a schedule.
		if err := tx.Table("daily_plan_items dpi").Select("dp.local_date, dpi.task_id, dpi.title, t.goal_id, dpi.target_minutes, dpi.status").
			Joins("JOIN daily_plans dp ON dp.id = dpi.plan_id AND dp.user_id = dpi.user_id AND dp.current_revision = dpi.plan_revision").
			Joins("JOIN tasks t ON t.id = dpi.task_id AND t.user_id = dpi.user_id").
			Joins("JOIN goals g ON g.id = t.goal_id AND g.user_id = t.user_id").
			Where("dpi.user_id = ? AND dp.local_date >= ? AND dp.local_date < ? AND dp.status NOT IN ('cancelled', 'withdrawn') AND dpi.status IN ('planned','in_progress') AND t.deleted_at IS NULL AND g.deleted_at IS NULL AND t.status IN ('ready','in_progress') AND g.status = 'active'", userID, end.In(loc).Format("2006-01-02"), end.In(loc).AddDate(0, 0, 7).Format("2006-01-02")).
			Order("dp.local_date, dpi.position, dpi.id").Limit(101).Scan(&result.Upcoming).Error; err != nil {
			return err
		}
		if len(result.Upcoming) > 100 {
			result.Upcoming = result.Upcoming[:100]
			result.Complete = false
			result.Warnings = append(result.Warnings, "下周已有安排较多，当前展示前 100 项。")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Store.Transaction deliberately takes SQLite's writer lock. This view needs
// only a WAL read snapshot, so keep one connection and explicitly defer BEGIN.
func reviewReadSnapshot(db *gorm.DB, read func(*gorm.DB) error) error {
	return db.Connection(func(conn *gorm.DB) (err error) {
		if err = conn.Exec("BEGIN DEFERRED").Error; err != nil {
			return err
		}
		defer func() {
			if rollback := conn.Exec("ROLLBACK").Error; err == nil {
				err = rollback
			}
		}()
		return read(conn.Session(&gorm.Session{NewDB: true}))
	})
}

// Predicate is an internal SQL expression; user input is always bound. Batches
// keep large history views below SQLite's bind-variable limit.
func reviewOwnedRows[T any](db *gorm.DB, userID, predicate string, ids []string) ([]T, error) {
	unique := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	result := []T{}
	for start := 0; start < len(unique); start += 500 {
		end := start + 500
		if end > len(unique) {
			end = len(unique)
		}
		var page []T
		if err := db.Where("user_id = ? AND "+predicate+" IN ?", userID, unique[start:end]).Find(&page).Error; err != nil {
			return nil, err
		}
		result = append(result, page...)
	}
	return result, nil
}
