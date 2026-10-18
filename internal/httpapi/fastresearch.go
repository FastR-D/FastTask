package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/persistence"
	"github.com/danielgtaylor/huma/v2"
	"github.com/gin-gonic/gin"
)

func (s *Server) registerFastResearch() {
	group := s.Engine.Group("/api/v1/auth/fastresearch")
	group.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 65536)
		c.Next()
	})
	configured := func(c *gin.Context) bool {
		if !s.cfg.FastResearchLoginEnabled {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"detail": "FastResearch login is not enabled"})
			return false
		}
		return true
	}
	group.GET("/available", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"enabled": s.cfg.FastResearchLoginEnabled, "url": s.cfg.FastResearchPublicURL})
	})
	group.GET("/login", func(c *gin.Context) {
		if !configured(c) {
			return
		}
		state := randomBrowserValue()
		if state == "" {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"detail": "unable to create login transaction"})
			return
		}
		setFastResearchCookie(c, "state", state, 300, s.cfg.PublicURL)
		callback := strings.TrimRight(s.cfg.PublicURL, "/") + "/api/v1/auth/fastresearch/callback?state=" + url.QueryEscape(state)
		target, _ := url.Parse(strings.TrimRight(s.cfg.FastResearchPublicURL, "/") + "/")
		query := target.Query()
		query.Set("launch", "fast-task")
		query.Set("next", callback)
		target.RawQuery = query.Encode()
		c.Redirect(http.StatusFound, target.String())
	})
	group.GET("/callback", func(c *gin.Context) {
		if !configured(c) {
			return
		}
		ticket := strings.TrimSpace(c.Query("sso"))
		if len(ticket) < 24 || len(ticket) > 512 {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"detail": "invalid login ticket"})
			return
		}
		state, _ := c.Cookie("fasttask.fastresearch.state")
		if state == "" || c.Query("state") != state {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"detail": "login transaction mismatch"})
			return
		}
		setFastResearchCookie(c, "ticket", base64.RawURLEncoding.EncodeToString([]byte(ticket)), 300, s.cfg.PublicURL)
		setFastResearchCookie(c, "state", "", -1, s.cfg.PublicURL)
		c.Redirect(http.StatusFound, strings.TrimRight(s.cfg.PublicURL, "/")+"/?fastresearch=callback")
	})
	group.POST("/complete", func(c *gin.Context) {
		if !configured(c) {
			return
		}
		if c.GetHeader("Origin") != strings.TrimRight(s.cfg.PublicURL, "/") {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"detail": "same-origin request required"})
			return
		}
		encoded, _ := c.Cookie("fasttask.fastresearch.ticket")
		ticket, err := base64.RawURLEncoding.DecodeString(encoded)
		setFastResearchCookie(c, "ticket", "", -1, s.cfg.PublicURL)
		if err != nil || len(ticket) == 0 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"detail": "login transaction missing"})
			return
		}
		result, err := s.auth.LoginFastResearch(c.Request.Context(), string(ticket))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"detail": "FastResearch login failed"})
			return
		}
		c.JSON(http.StatusOK, result)
	})
	s.registerFastResearchOpenAPI()
}

func randomBrowserValue() string {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(value)
}

func setFastResearchCookie(c *gin.Context, name, value string, maxAge int, publicURL string) {
	http.SetCookie(c.Writer, &http.Cookie{Name: "fasttask.fastresearch." + name, Value: value, Path: "/api/v1/auth/fastresearch", HttpOnly: true, Secure: strings.HasPrefix(publicURL, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: maxAge})
}

func (s *Server) registerFastResearchOpenAPI() {
	api := s.API.OpenAPI()
	if api.Paths == nil {
		api.Paths = map[string]*huma.PathItem{}
	}
	str := &huma.Schema{Type: "string"}
	api.Paths["/auth/fastresearch/available"] = &huma.PathItem{Get: &huma.Operation{OperationID: "fastresearch-available", Tags: []string{"FastResearch"}, Summary: "Check FastResearch login availability", Responses: map[string]*huma.Response{"200": {Description: "availability"}}}}
	api.Paths["/auth/fastresearch/login"] = &huma.PathItem{Get: &huma.Operation{OperationID: "fastresearch-login", Tags: []string{"FastResearch"}, Summary: "Start FastResearch Key login", Responses: map[string]*huma.Response{"302": {Description: "redirect", Headers: map[string]*huma.Param{"Location": {Schema: str}}}}}}
	api.Paths["/auth/fastresearch/callback"] = &huma.PathItem{Get: &huma.Operation{OperationID: "fastresearch-callback", Tags: []string{"FastResearch"}, Summary: "Receive FastResearch SSO ticket", Responses: map[string]*huma.Response{"302": {Description: "redirect"}}}}
	api.Paths["/auth/fastresearch/complete"] = &huma.PathItem{Post: &huma.Operation{OperationID: "fastresearch-complete", Tags: []string{"FastResearch"}, Summary: "Exchange FastResearch ticket for a FastTask session", Responses: map[string]*huma.Response{"200": {Description: "session"}, "401": {Description: "invalid ticket"}}}}
}

type fastResearchTodayItem struct {
	ID                string                         `json:"id"`
	TaskID            *string                        `json:"task_id,omitempty"`
	Kind              string                         `json:"kind"`
	Title             string                         `json:"title"`
	Commitment        string                         `json:"commitment"`
	MinimumAction     string                         `json:"minimum_action"`
	Status            string                         `json:"status"`
	CompletionType    string                         `json:"completion_type,omitempty"`
	CompletionSummary string                         `json:"completion_summary,omitempty"`
	TargetMinutes     int                            `json:"target_minutes"`
	Position          int                            `json:"position"`
	Workspace         *application.PlanItemWorkspace `json:"workspace,omitempty"`
}

type fastResearchTodayProgress struct {
	Total      int `json:"total"`
	Completed  int `json:"completed"`
	InProgress int `json:"in_progress"`
	Planned    int `json:"planned"`
	Skipped    int `json:"skipped"`
}

type fastResearchTodayMember struct {
	ReceiverRef  string                    `json:"receiver_ref"`
	Found        bool                      `json:"found"`
	UserID       string                    `json:"user_id,omitempty"`
	DisplayName  string                    `json:"display_name,omitempty"`
	LocalDate    string                    `json:"local_date,omitempty"`
	Timezone     string                    `json:"timezone,omitempty"`
	PlanID       string                    `json:"plan_id,omitempty"`
	PlanStatus   string                    `json:"plan_status,omitempty"`
	PlanRevision int                       `json:"plan_revision,omitempty"`
	UpdatedAt    *time.Time                `json:"updated_at,omitempty"`
	Items        []fastResearchTodayItem   `json:"items"`
	Progress     fastResearchTodayProgress `json:"progress"`
	Error        string                    `json:"error,omitempty"`
}

// registerFastResearchIntegration exposes the read-only daily plan surface the
// FastResearch workspace uses to show today's FastTask tasks. It is a
// server-to-server endpoint authenticated by the shared integration key, and the
// stable receiver_ref is the FastResearch account ID; no personal Key or user
// token crosses the boundary.
func (s *Server) registerFastResearchIntegration() {
	group := s.Engine.Group("/api/v1/integrations/fastresearch")
	group.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 256*1024)
		c.Next()
	})
	group.POST("/today", s.fastResearchToday)
}

func (s *Server) fastResearchToday(c *gin.Context) {
	if !s.requireFastResearchIntegrationKey(c) {
		return
	}
	var body struct {
		ReceiverRefs []string `json:"receiver_refs"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"detail": "invalid request body"})
		return
	}
	refs := make([]string, 0, len(body.ReceiverRefs))
	seen := make(map[string]bool, len(body.ReceiverRefs))
	for _, raw := range body.ReceiverRefs {
		ref := strings.TrimSpace(raw)
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		refs = append(refs, ref)
		if len(refs) == 200 {
			break
		}
	}
	if len(refs) == 0 {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"detail": "receiver_refs is required"})
		return
	}
	deps := RouteDeps{app: s.app, auth: s.auth, cfg: &s.cfg}
	members := make([]fastResearchTodayMember, 0, len(refs))
	for _, ref := range refs {
		members = append(members, deps.fastResearchTodayFor(c.Request.Context(), ref))
	}
	c.JSON(http.StatusOK, gin.H{"members": members})
}

func (s *Server) requireFastResearchIntegrationKey(c *gin.Context) bool {
	configured := strings.TrimSpace(s.cfg.FastResearchIngestKey)
	if configured == "" {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"detail": "FastResearch integration key is not configured"})
		return false
	}
	candidate := strings.TrimSpace(c.GetHeader("X-API-Key"))
	if candidate == "" {
		candidate = strings.TrimSpace(c.GetHeader("X-FastResearch-Provision-Key"))
	}
	if len(candidate) != len(configured) || subtle.ConstantTimeCompare([]byte(candidate), []byte(configured)) != 1 {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"detail": "invalid FastResearch integration key"})
		return false
	}
	return true
}

func (s RouteDeps) fastResearchTodayFor(ctx context.Context, receiverRef string) fastResearchTodayMember {
	member := fastResearchTodayMember{ReceiverRef: receiverRef, Items: []fastResearchTodayItem{}}
	var mapping persistence.FastResearchAccount
	if err := s.app.Store.DB.WithContext(ctx).Where("account_id = ?", receiverRef).First(&mapping).Error; err != nil {
		if !persistence.IsNotFound(err) {
			member.Error = "account lookup failed"
		}
		return member
	}
	var user persistence.User
	if err := s.app.Store.DB.WithContext(ctx).Where("id = ? AND status = 'active'", mapping.UserID).First(&user).Error; err != nil {
		if !persistence.IsNotFound(err) {
			member.Error = "user lookup failed"
		}
		return member
	}
	member.Found = true
	member.UserID = user.ID
	member.DisplayName = user.DisplayName
	plan, items, err := s.currentPlan(ctx, user.ID)
	if err != nil {
		if errors.Is(err, application.ErrNotFound) {
			member.PlanStatus = "none"
			return member
		}
		member.Error = "schedule refresh failed"
		return member
	}
	views, err := s.app.PlanItemViews(ctx, user.ID, items)
	if err != nil {
		member.Error = "plan view failed"
		return member
	}
	member.LocalDate = plan.LocalDate
	member.Timezone = plan.Timezone
	member.PlanID = plan.ID
	member.PlanStatus = plan.Status
	member.PlanRevision = plan.CurrentRevision
	member.UpdatedAt = &plan.UpdatedAt
	for _, view := range views {
		member.Items = append(member.Items, fastResearchTodayItem{
			ID:                view.ID,
			TaskID:            view.TaskID,
			Kind:              view.Kind,
			Title:             view.Title,
			Commitment:        view.Commitment,
			MinimumAction:     view.MinimumAction,
			Status:            view.Status,
			CompletionType:    view.CompletionType,
			CompletionSummary: view.CompletionSummary,
			TargetMinutes:     view.TargetMinutes,
			Position:          view.Position,
			Workspace:         view.Workspace,
		})
		member.Progress.Total++
		switch view.Status {
		case "satisfied":
			member.Progress.Completed++
		case "in_progress":
			member.Progress.InProgress++
		case "skipped":
			member.Progress.Skipped++
		default:
			member.Progress.Planned++
		}
	}
	return member
}
