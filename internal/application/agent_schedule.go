package application

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

type proposedSchedule struct {
	Assumptions string              `json:"assumptions,omitempty"`
	TargetDate  *string             `json:"target_date,omitempty"`
	DateOnly    bool                `json:"date_only,omitempty"`
	Changes     []proposedTaskDates `json:"changes"`
	Warnings    []string            `json:"warnings,omitempty"`
}

type proposedTaskDates struct {
	Title       string `json:"title"`
	BeforeStart string `json:"before_start,omitempty"`
	BeforeEnd   string `json:"before_end,omitempty"`
	Start       string `json:"start"`
	End         string `json:"end"`
}

// A dated task-tree proposal records the goal version used for its schedule.
// Earlier proposals still decode as a plain array of operations.
type taskTreeSchedulePayload struct {
	Patch        []map[string]any  `json:"patch"`
	GoalRevision int               `json:"goal_revision"`
	Schedule     *proposedSchedule `json:"schedule"`
}

func decodeTaskTreeProposal(encoded string) (taskTreeSchedulePayload, error) {
	var payload taskTreeSchedulePayload
	data := strings.TrimSpace(encoded)
	if strings.HasPrefix(data, "[") {
		if err := json.Unmarshal([]byte(data), &payload.Patch); err != nil {
			return payload, ErrValidation
		}
		return payload, nil
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil || len(payload.Patch) == 0 || payload.GoalRevision <= 0 || payload.Schedule == nil {
		return payload, ErrValidation
	}
	return payload, nil
}

func scheduleFromToolResult(result string) *proposedSchedule {
	var value struct {
		Schedule *proposedSchedule `json:"schedule"`
	}
	if json.Unmarshal([]byte(result), &value) != nil {
		return nil
	}
	return value.Schedule
}

func proposedDates(patch map[string]any, task *persistence.Task) (bool, error) {
	start, hasStart := patch["planned_start"]
	end, hasEnd := patch["planned_end"]
	if !hasStart && !hasEnd {
		return false, nil
	}
	startDate, startOK := start.(string)
	endDate, endOK := end.(string)
	if !startOK || !endOK {
		return false, patchError("planned_start and planned_end must both be dates in YYYY-MM-DD format")
	}
	task.PlannedStart, task.PlannedEnd = &startDate, &endDate
	if err := validatePlannedDates(task); err != nil {
		return false, patchError(err.Error())
	}
	return true, nil
}

func scheduleOnlyPatch(patches []map[string]any) bool {
	for _, patch := range patches {
		if patch["op"] != "update" || len(patch) != 4 || patch["planned_start"] == nil || patch["planned_end"] == nil {
			return false
		}
	}
	return len(patches) > 0
}

// Draft dates for new actionable tasks when the model supplied no dates. Work
// with an unknown prerequisite or an explicit reason stays undated. Capacity
// comes from the user's research budget, and the proposal discloses that estimate.
func defaultNewTaskDates(tx *gorm.DB, userID string, patches []map[string]any, existing []persistence.Task, assumptions string) ([]map[string]any, string, error) {
	parents := map[string]bool{}
	for _, patch := range patches {
		if ref := textValue(patch["parent_ref"]); ref != "" {
			parents[ref] = true
		}
	}
	needsDates := func(patch map[string]any) bool {
		op, kind := textValue(patch["op"]), textValue(patch["type"])
		_, start := patch["planned_start"]
		_, end := patch["planned_end"]
		return (op == "" || op == "create") && (kind == "task" || kind == "action") &&
			!parents[textValue(patch["client_ref"])] && !start && !end && textValue(patch["leave_unscheduled_reason"]) == ""
	}
	if !slices.ContainsFunc(patches, needsDates) {
		return patches, assumptions, nil
	}
	var user persistence.User
	if err := tx.Select("id", "timezone").Where("id = ?", userID).First(&user).Error; err != nil {
		return nil, "", err
	}
	settings, err := planningSettingsTx(tx, userID)
	if err != nil {
		return nil, "", err
	}
	zone, _ := resolveZone("", user.Timezone)
	calendar, _ := time.Parse(time.DateOnly, persistence.Now().In(zone).Format(time.DateOnly))
	budget := max(1, settings.DailyBudgetMinutes)
	remaining := budget
	known := make(map[string]persistence.Task, len(existing)+len(patches))
	for _, task := range existing {
		if task.DeletedAt == nil {
			known[task.ID] = task
		}
	}
	refs := map[string]string{}
	var staged []map[string]any
	generated := false
	for i, input := range patches {
		patch := maps.Clone(input)
		staged = append(staged, patch)
		op := textValue(patch["op"])
		id := textValue(patch["target_id"])
		if op == "" || op == "create" {
			id = fmt.Sprintf("proposal-task-%d", i)
			if ref := textValue(patch["client_ref"]); ref != "" {
				refs[ref] = id
			}
		}
		if op == "delete" || op == "supersede" {
			delete(known, id)
			continue
		}
		task := known[id]
		if parent, present := patch["parent_id"]; present {
			task.ParentID = nil
			if parentID := textValue(parent); parentID != "" {
				task.ParentID = &parentID
			}
		}
		if parentRef := refs[textValue(patch["parent_ref"])]; parentRef != "" {
			task.ParentID = &parentRef
		}
		dependencies, supplied, err := taskDependenciesFromPatch(patch, refs)
		if err != nil {
			return nil, "", err
		}
		if supplied {
			task.DependsOn = dependencies
		}
		if scheduled, err := proposedDates(patch, &task); err != nil {
			return nil, "", err
		} else if !scheduled && needsDates(patch) {
			dependencies := append([]string(nil), task.DependsOn...)
			earliest := calendar
			seenParents := map[string]bool{id: true}
			for ancestor := derefString(task.ParentID); ancestor != "" && !seenParents[ancestor]; ancestor = derefString(known[ancestor].ParentID) {
				seenParents[ancestor] = true
				parent := known[ancestor]
				dependencies = append(dependencies, parent.DependsOn...)
				if start, err := time.Parse(time.DateOnly, derefString(parent.PlannedStart)); err == nil && start.After(earliest) {
					earliest = start
				}
			}
			for _, prerequisite := range dependencies {
				predecessor := known[prerequisite]
				if predecessor.Status == "completed" {
					continue
				}
				end, err := time.Parse(time.DateOnly, derefString(predecessor.PlannedEnd))
				if err != nil {
					patch["leave_unscheduled_reason"] = "前置任务尚无可用的计划结束日期。"
					break
				}
				if end.After(earliest) {
					earliest = end
				}
			}
			if textValue(patch["leave_unscheduled_reason"]) == "" {
				if earliest.After(calendar) {
					calendar, remaining = earliest, budget
				}
				minutes := intValue(patch["estimate_minutes"])
				if minutes == 0 {
					minutes = 25 // Same default as task creation.
				}
				if remaining == 0 {
					calendar, remaining = calendar.AddDate(0, 0, 1), budget
				}
				start := calendar.Format(time.DateOnly)
				if minutes > remaining {
					days := (minutes - remaining + budget - 1) / budget
					calendar, remaining = calendar.AddDate(0, 0, days), remaining+days*budget
				}
				remaining -= minutes
				end := calendar.Format(time.DateOnly)
				patch["planned_start"], patch["planned_end"] = start, end
				task.PlannedStart, task.PlannedEnd = &start, &end
				generated = true
			}
		}
		known[id] = task
	}
	if generated {
		assumptions = strings.TrimSpace(assumptions + fmt.Sprintf(" 未提供起止日期的新任务按本地今天和每日 %d 分钟研究预算估算。", budget))
	}
	return staged, assumptions, nil
}

func schedulePreview(patches []map[string]any, tasks []persistence.Task, deadline *string, assumptions string, reschedule bool) (*proposedSchedule, error) {
	if utf8.RuneCountInString(assumptions) > 1200 {
		return nil, patchError("schedule_assumptions must be at most 1200 characters")
	}
	byID := make(map[string]persistence.Task, len(tasks)+len(patches))
	var order []string
	for _, task := range tasks {
		if task.DeletedAt == nil {
			byID[task.ID] = task
			order = append(order, task.ID)
		}
	}
	preview := &proposedSchedule{Assumptions: assumptions, TargetDate: deadline, DateOnly: scheduleOnlyPatch(patches)}
	refs := map[string]string{}
	changed := map[string]bool{}
	for i, patch := range patches {
		op := textValue(patch["op"])
		id := textValue(patch["target_id"])
		if op == "" || op == "create" {
			id = fmt.Sprintf("proposal-task-%d", i)
			order = append(order, id)
			if ref := textValue(patch["client_ref"]); ref != "" {
				refs[ref] = id
			}
		}
		if op == "delete" || op == "supersede" {
			delete(byID, id)
			continue
		}
		if op == "restore" {
			continue
		}
		task := byID[id]
		if title := textValue(patch["title"]); title != "" {
			task.Title = title
		}
		reason := textValue(patch["leave_unscheduled_reason"])
		if reason != "" {
			preview.Warnings = append(preview.Warnings, fmt.Sprintf("%s 暂未排期。%s", task.Title, reason))
		}
		if parent, present := patch["parent_id"]; present {
			task.ParentID = nil
			if parentID := textValue(parent); parentID != "" {
				task.ParentID = &parentID
			}
		}
		if parentRef := refs[textValue(patch["parent_ref"])]; parentRef != "" {
			task.ParentID = &parentRef
		}
		if dependencies, present, err := taskDependenciesFromPatch(patch, refs); err != nil {
			return nil, err
		} else if present {
			task.DependsOn = dependencies
		}
		beforeStart, beforeEnd := derefString(task.PlannedStart), derefString(task.PlannedEnd)
		scheduled, err := proposedDates(patch, &task)
		if err != nil {
			return nil, err
		}
		if scheduled {
			if reason != "" {
				return nil, patchError("provide dates or leave_unscheduled_reason for a task")
			}
			if op == "update" && !reschedule && (beforeStart != "" || task.Status == "in_progress" || task.Status == "completed" || task.Status == "cancelled" || task.Status == "superseded") &&
				(beforeStart != *task.PlannedStart || beforeEnd != *task.PlannedEnd) {
				return nil, patchError(fmt.Sprintf("task %q has existing dates or has already started or ended; preserve its dates unless the user explicitly requested rescheduling", task.Title))
			}
			if changed[id] {
				return nil, patchError("propose dates only once per task in a single patch")
			}
			changed[id] = true
			preview.Changes = append(preview.Changes, proposedTaskDates{Title: task.Title, BeforeStart: beforeStart, BeforeEnd: beforeEnd, Start: *task.PlannedStart, End: *task.PlannedEnd})
		}
		byID[id] = task
	}
	if len(preview.Changes) == 0 && len(preview.Warnings) == 0 {
		return nil, nil
	}
	preview.Warnings = append(preview.Warnings, scheduleWarnings(byID, order, changed, deadline)...)
	return preview, nil
}

func scheduleWarnings(tasks map[string]persistence.Task, order []string, changed map[string]bool, deadline *string) []string {
	children := map[string][]string{}
	for _, id := range order {
		if task, exists := tasks[id]; exists && task.ParentID != nil {
			children[*task.ParentID] = append(children[*task.ParentID], id)
		}
	}
	type dateRange struct{ start, end string }
	ranges := map[string]dateRange{}
	var span func(string) dateRange
	span = func(id string) dateRange {
		if rangeValue, seen := ranges[id]; seen {
			return rangeValue
		}
		task := tasks[id]
		result := dateRange{derefString(task.PlannedStart), derefString(task.PlannedEnd)}
		ranges[id] = result // Stop if a proposed parent cycle reaches this node again.
		if result.start == "" {
			for _, child := range children[id] {
				value := span(child)
				if value.start != "" && (result.start == "" || value.start < result.start) {
					result.start = value.start
				}
				result.end = max(result.end, value.end)
			}
			ranges[id] = result
		}
		return result
	}
	var warnings []string
	latest := ""
	for _, id := range order {
		task, exists := tasks[id]
		if !exists || span(id).start == "" {
			continue
		}
		window := span(id)
		if changed[id] {
			latest = max(latest, window.end)
		}
		seenParents, prerequisites := map[string]bool{}, map[string]bool{}
		for ancestor := id; ancestor != "" && !seenParents[ancestor]; ancestor = derefString(tasks[ancestor].ParentID) {
			seenParents[ancestor] = true
			parent := tasks[ancestor]
			if ancestor != id && (changed[id] || changed[ancestor]) && parent.PlannedStart != nil && parent.PlannedEnd != nil &&
				(window.start < *parent.PlannedStart || window.end > *parent.PlannedEnd) {
				warnings = append(warnings, fmt.Sprintf("%s 超出「%s」的计划范围。", task.Title, parent.Title))
			}
			for _, prerequisiteID := range parent.DependsOn {
				if prerequisites[prerequisiteID] || (!changed[id] && !changed[prerequisiteID]) {
					continue
				}
				prerequisites[prerequisiteID] = true
				prerequisite, known := tasks[prerequisiteID]
				if !known || prerequisite.Status == "completed" {
					continue
				}
				if end := span(prerequisiteID).end; end == "" {
					warnings = append(warnings, fmt.Sprintf("%s 的前置任务「%s」尚未排期。", task.Title, prerequisite.Title))
				} else if window.start < end {
					warnings = append(warnings, fmt.Sprintf("%s 开始于前置任务「%s」计划结束之前。", task.Title, prerequisite.Title))
				}
			}
		}
	}
	if deadline != nil && latest > *deadline {
		end, _ := time.Parse(time.DateOnly, latest)
		due, _ := time.Parse(time.DateOnly, *deadline)
		warnings = append([]string{fmt.Sprintf("本次排期最晚到 %s，超过目标截止日 %s %d 天。", latest, *deadline, (end.Unix()-due.Unix())/86400)}, warnings...)
	}
	return warnings
}
