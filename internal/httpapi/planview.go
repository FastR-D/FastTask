package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/persistence"
	"github.com/danielgtaylor/huma/v2"
)

// Every current-day consumer runs the same schedule refresh. EnsureDailyPlan
// alone can return a frozen one-item plan without noticing newly added tasks.
func (s RouteDeps) currentPlan(ctx context.Context, userID string) (*persistence.DailyPlan, []persistence.DailyPlanItem, error) {
	days, err := s.app.RefreshSchedule(ctx, userID, persistence.Now())
	if err != nil {
		return nil, nil, err
	}
	if len(days) == 0 {
		return nil, nil, application.ErrNotFound
	}
	return &days[0].Plan, days[0].Items, nil
}

func (s RouteDeps) panelUser(ctx context.Context, authorization, scope string) (string, error) {
	p, err := s.auth.Authenticate(authorization)
	if err != nil {
		p, err = s.auth.AuthenticateService(authorization, scope)
	}
	if err != nil {
		return "", huma.Error401Unauthorized("authentication required with the requested panel read scope")
	}
	var user persistence.User
	if err := s.app.Store.DB.WithContext(ctx).Where("id = ? AND status = 'active'", p.UserID).First(&user).Error; err != nil {
		return "", huma.Error401Unauthorized("invalid panel user")
	}
	return p.UserID, nil
}

// HTTP GET validators accept weak tags and comma-separated alternatives.
func matchesReadETag(header, etag string) bool {
	for _, value := range strings.Split(header, ",") {
		value = strings.TrimSpace(value)
		if value == "*" || strings.TrimPrefix(value, "W/") == etag {
			return true
		}
	}
	return false
}

func viewETag(kind, id string, body any) string {
	payload, _ := json.Marshal(body)
	return "\"" + kind + "_" + id + "_" + persistence.Hash(string(payload))[:16] + "\""
}

type panelTasksBody struct {
	DailyPlanID  string                     `json:"daily_plan_id"`
	LocalDate    string                     `json:"local_date"`
	Timezone     string                     `json:"timezone"`
	PlanStatus   string                     `json:"plan_status"`
	PlanRevision int                        `json:"plan_revision"`
	PlanETag     string                     `json:"plan_etag"`
	TodayItems   []application.PlanItemView `json:"today_items"`
	UpdatedAt    time.Time                  `json:"updated_at"`
	ReadOnly     bool                       `json:"read_only"`
}

type panelTasksResponse struct {
	Status       int    `status:"default"`
	ETag         string `header:"ETag"`
	CacheControl string `header:"Cache-Control"`
	Body         *panelTasksBody
}

func (s panelRoutes) registerTaskList(api huma.API) {
	type input struct {
		Authorization string `header:"Authorization"`
		IfNoneMatch   string `header:"If-None-Match"`
	}
	register(api, "panel-tasks", http.MethodGet, "/panel/tasks", "Read the complete current task list with panel:tasks:read or user authentication", publicSecurity(), func(ctx context.Context, input *input) (*panelTasksResponse, error) {
		userID, err := s.panelUser(ctx, input.Authorization, "panel:tasks:read")
		if err != nil {
			return nil, err
		}
		plan, items, err := s.currentPlan(ctx, userID)
		if err != nil {
			return nil, mapError(err)
		}
		views, err := s.app.PlanItemViews(ctx, userID, items)
		if err != nil {
			return nil, mapError(err)
		}
		body := panelTasksBody{DailyPlanID: plan.ID, LocalDate: plan.LocalDate, Timezone: plan.Timezone, PlanStatus: plan.Status, PlanRevision: plan.CurrentRevision, PlanETag: application.StrongETag("plan", plan.ID, plan.Revision), TodayItems: views, UpdatedAt: plan.UpdatedAt, ReadOnly: true}
		etag := viewETag("panel-tasks", userID, body)
		if matchesReadETag(input.IfNoneMatch, etag) {
			return &panelTasksResponse{Status: http.StatusNotModified, ETag: etag, CacheControl: "private, no-cache"}, nil
		}
		return &panelTasksResponse{Status: http.StatusOK, ETag: etag, CacheControl: "private, no-cache", Body: &body}, nil
	})
}
