package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/config"
	"github.com/FastR-D/FastTask/internal/domain"
	"github.com/FastR-D/FastTask/internal/persistence"
	platformauth "github.com/FastR-D/FastTask/internal/platform/auth"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humagin"
	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

type Server struct {
	Engine *gin.Engine
	API    huma.API
	app    *application.App
	auth   *platformauth.Service
	agent  *application.AgentService
	cfg    config.Config
}

func New(app *application.App, authService *platformauth.Service, cfg config.Config, agent *application.AgentService, routes ...RouteRegistrar) *Server {
	if agent == nil {
		agent = application.NewAgentService(app)
	}
	if cfg.AudioDir == "" {
		cfg.AudioDir = filepath.Join(filepath.Dir(cfg.DatabasePath), "audio")
	}
	if cfg.IntegrationTimeout == 0 {
		cfg.IntegrationTimeout = 2 * time.Second
	}
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()
	if err := engine.SetTrustedProxies(cfg.TrustedProxies); err != nil {
		panic("invalid trusted proxy configuration: " + err.Error())
	}
	engine.Use(requestID(), gin.CustomRecovery(func(c *gin.Context, recovered any) {
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"title": "Internal Server Error", "status": 500, "detail": "unexpected server error"})
	}))
	engine.Use(func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("Permissions-Policy", "camera=(), microphone=(self), geolocation=()")
		c.Header("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		if strings.HasPrefix(c.Request.URL.Path, "/assets/") {
			c.Header("Cache-Control", "public, max-age=31536000, immutable")
		} else if !strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.Header("Cache-Control", "no-store")
		}
		if cfg.Environment == "production" || strings.HasPrefix(cfg.PublicURL, "https://") {
			c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		c.Next()
	})
	engine.Use(cors.New(cors.Config{AllowOrigins: []string{cfg.PublicURL}, AllowMethods: []string{"GET", "POST", "PATCH", "PUT", "DELETE", "OPTIONS"}, AllowHeaders: []string{"Authorization", "Content-Type", "If-Match", "If-None-Match", "Idempotency-Key", "X-Device-Token", "X-Service-Token", "X-Request-ID"}, ExposeHeaders: []string{"ETag", "Location", "X-Request-ID"}, MaxAge: 12 * time.Hour}))
	engine.GET("/health/live", func(c *gin.Context) { c.JSON(200, gin.H{"status": "ok"}) })
	engine.GET("/health/ready", func(c *gin.Context) {
		if err := app.Store.Ready(c.Request.Context()); err != nil {
			c.JSON(503, gin.H{"status": "not_ready", "detail": "database schema is not current"})
			return
		}
		c.JSON(200, gin.H{"status": "ready"})
	})
	engine.GET("/health/version", func(c *gin.Context) { c.JSON(200, gin.H{"version": "0.1.0", "go": "1.24"}) })

	v1 := engine.Group("/api/v1")
	v1.Use(func(c *gin.Context) { c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 34<<20); c.Next() })
	v1.Use(idempotencyMiddleware(app.Store, authService))
	humaConfig := huma.DefaultConfig("FastTask API", "0.1.0")
	humaConfig.OpenAPIPath, humaConfig.DocsPath, humaConfig.SchemasPath = "/openapi", "/docs", "/schemas"
	humaConfig.Servers = []*huma.Server{{URL: strings.TrimRight(cfg.PublicURL, "/") + "/api/v1"}}
	humaConfig.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"userBearer":            {Type: "http", Scheme: "bearer", BearerFormat: "JWT"},
		"deviceToken":           {Type: "apiKey", In: "header", Name: "X-Device-Token"},
		"serviceImportsWrite":   {Type: "http", Scheme: "bearer", BearerFormat: "JWT"},
		"serviceImportsRead":    {Type: "http", Scheme: "bearer", BearerFormat: "JWT"},
		"serviceAgentJobsWrite": {Type: "http", Scheme: "bearer", BearerFormat: "JWT"},
	}
	humaConfig.Components.Schemas = huma.NewMapRegistry("#/components/schemas/", schemaNamer)
	humagin.MultipartMaxMemory = 8 << 20
	api := humagin.NewWithGroup(engine, v1, humaConfig)
	server := &Server{Engine: engine, API: api, app: app, auth: authService, agent: agent, cfg: cfg}
	api.UseMiddleware(server.authenticationMiddleware)
	// Domain routes come from the "routes" value group (wiring.md §5, §7 step 7).
	// Callers that build a Server directly (unit tests, §2 rule 4) pass none and
	// get the builtin registrars, preserving the historical registration set.
	if len(routes) == 0 {
		routes = BuiltinRouteRegistrars(NewRouteDeps(app, authService, &server.cfg))
	}
	for _, registrar := range routes {
		registrar.RegisterRoutes(api)
	}
	// The agent transport endpoints are bare-Gin (§9.1) and the SPA fallback is
	// not a huma route, so both stay direct Server calls rather than registrars.
	server.registerAgent()
	server.static()
	return server
}

func (s *Server) authenticationMiddleware(ctx huma.Context, next func(huma.Context)) {
	security := ctx.Operation().Security
	if len(security) == 0 {
		next(ctx)
		return
	}
	authorization := ctx.Header("Authorization")
	validService := false
	for _, requirement := range security {
		if _, ok := requirement["userBearer"]; ok {
			if authenticated, err := s.auth.Authenticate(authorization); err == nil {
				ctx = huma.WithContext(ctx, platformauth.WithPrincipal(ctx.Context(), authenticated))
				ctx = huma.WithValue(ctx, principalContextKey{}, authenticated)
				next(ctx)
				return
			}
		}
		for scheme, scope := range map[string]string{"serviceImportsWrite": "imports:write", "serviceImportsRead": "imports:read", "serviceAgentJobsWrite": "agent-jobs:write"} {
			if _, ok := requirement[scheme]; !ok {
				continue
			}
			authenticated, err := s.auth.AuthenticateServicePrincipal(authorization)
			if err == nil {
				validService = true
				if platformauth.HasScopes(authenticated, scope) {
					ctx = huma.WithValue(ctx, principalContextKey{}, authenticated)
					next(ctx)
					return
				}
			}
		}
	}
	if validService {
		huma.WriteErr(s.API, ctx, http.StatusForbidden, "insufficient service scope")
		return
	}
	huma.WriteErr(s.API, ctx, http.StatusUnauthorized, "authentication required")
}

type principalContextKey struct{}

func principal(ctx context.Context) platformauth.Principal {
	value, _ := ctx.Value(principalContextKey{}).(platformauth.Principal)
	return value
}
func userSecurity() []map[string][]string   { return []map[string][]string{{"userBearer": {}}} }
func publicSecurity() []map[string][]string { return []map[string][]string{} }
func serviceSecurity(scheme string) []map[string][]string {
	return []map[string][]string{{scheme: {}}}
}
func userOrServiceSecurity(scheme string) []map[string][]string {
	return []map[string][]string{{"userBearer": {}}, {scheme: {}}}
}

type itemResponse[T any] struct{ Body T }
type listResponse[T any] struct {
	Body struct {
		Items      []T     `json:"items"`
		NextCursor *string `json:"next_cursor"`
		HasMore    bool    `json:"has_more"`
	}
}
type resourceResponse[T any] struct {
	ETag string `header:"ETag"`
	Body T
}
type acceptedResponse struct {
	Location string `header:"Location"`
	Body     persistence.AgentJob
}

func register[I, O any](api huma.API, id, method, path, summary string, security []map[string][]string, handler func(context.Context, *I) (*O, error)) {
	huma.Register(api, huma.Operation{OperationID: id, Method: method, Path: path, Summary: summary, Security: security, DefaultStatus: operationStatus(id), Errors: []int{400, 401, 403, 404, 409, 412, 422, 428, 500}}, handler)
}

func operationStatus(id string) int {
	switch id {
	case "create-goal", "create-task", "create-daily-plan", "add-daily-plan-item", "create-work-session", "create-conversation", "create-device", "create-import":
		return http.StatusCreated
	case "create-admin-user", "create-model-provider":
		return http.StatusCreated
	case "generate-task-tree", "revise-task-tree", "generate-daily-plan", "generate-support-items", "create-conversation-message", "create-voice-transcription", "retry-agent-job":
		return http.StatusAccepted
	case "auth-logout":
		return http.StatusNoContent
	default:
		return http.StatusOK
	}
}

func (s authRoutes) RegisterRoutes(api huma.API) {
	type loginInput struct {
		Body struct {
			Identifier string `json:"identifier" minLength:"1"`
			Password   string `json:"password"`
		}
	}
	type tokenBody struct {
		TokenType        string           `json:"token_type"`
		AccessToken      string           `json:"access_token"`
		ExpiresIn        int              `json:"expires_in"`
		RefreshToken     string           `json:"refresh_token"`
		RefreshExpiresIn int              `json:"refresh_expires_in"`
		User             persistence.User `json:"user"`
	}
	register(api, "auth-login", http.MethodPost, "/auth/login", "Login", publicSecurity(), func(ctx context.Context, input *loginInput) (*itemResponse[tokenBody], error) {
		user, access, refresh, err := s.auth.Login(ctx, input.Body.Identifier, input.Body.Password)
		if err != nil {
			return nil, huma.Error401Unauthorized("invalid credentials")
		}
		return &itemResponse[tokenBody]{Body: tokenBody{"Bearer", access, 3600, refresh, 2592000, user}}, nil
	})
	type refreshInput struct {
		IdempotencyKey string `header:"Idempotency-Key" required:"true"`
		Body           struct {
			RefreshToken string `json:"refresh_token" minLength:"20"`
		}
	}
	register(api, "auth-refresh", http.MethodPost, "/auth/refresh", "Rotate refresh token", publicSecurity(), func(ctx context.Context, input *refreshInput) (*itemResponse[tokenBody], error) {
		user, access, refresh, err := s.auth.Refresh(ctx, input.Body.RefreshToken)
		if err != nil {
			return nil, huma.Error401Unauthorized("invalid refresh token")
		}
		return &itemResponse[tokenBody]{Body: tokenBody{"Bearer", access, 3600, refresh, 2592000, user}}, nil
	})
	type emptyInput struct{}
	register(api, "auth-logout", http.MethodPost, "/auth/logout", "Logout", userSecurity(), func(ctx context.Context, input *emptyInput) (*struct{}, error) {
		p := principal(ctx)
		if err := s.auth.Logout(ctx, p.SessionID); err != nil {
			return nil, mapError(err)
		}
		return &struct{}{}, nil
	})
	register(api, "auth-session", http.MethodGet, "/auth/session", "Current session", userSecurity(), func(ctx context.Context, input *emptyInput) (*itemResponse[platformauth.Principal], error) {
		return &itemResponse[platformauth.Principal]{Body: principal(ctx)}, nil
	})
}

func (s meRoutes) RegisterRoutes(api huma.API) {
	type empty struct{}
	register(api, "get-me", http.MethodGet, "/me", "Get current user", userSecurity(), func(ctx context.Context, input *empty) (*resourceResponse[persistence.User], error) {
		var user persistence.User
		p := principal(ctx)
		if err := s.app.Store.DB.WithContext(ctx).First(&user, "id = ?", p.UserID).Error; err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.User]{ETag: application.StrongETag("user", user.ID, user.Revision), Body: user}, nil
	})
	type patch struct {
		IfMatch string `header:"If-Match" required:"true"`
		Body    struct {
			DisplayName *string `json:"display_name,omitempty"`
			Timezone    *string `json:"timezone,omitempty"`
			Locale      *string `json:"locale,omitempty"`
		}
	}
	register(api, "patch-me", http.MethodPatch, "/me", "Update current user", userSecurity(), func(ctx context.Context, input *patch) (*resourceResponse[persistence.User], error) {
		p := principal(ctx)
		var user persistence.User
		if err := s.app.Store.DB.WithContext(ctx).First(&user, "id = ?", p.UserID).Error; err != nil {
			return nil, mapError(err)
		}
		if !matchETag(input.IfMatch, "user", user.ID, user.Revision) {
			return nil, mapError(application.ErrRevision)
		}
		updates := map[string]any{"revision": user.Revision + 1, "updated_at": persistence.Now()}
		if input.Body.DisplayName != nil {
			updates["display_name"] = *input.Body.DisplayName
		}
		if input.Body.Timezone != nil {
			if _, e := time.LoadLocation(*input.Body.Timezone); e != nil {
				return nil, mapError(application.ErrValidation)
			}
			updates["timezone"] = *input.Body.Timezone
		}
		if input.Body.Locale != nil {
			updates["locale"] = *input.Body.Locale
		}
		if err := s.app.Store.DB.WithContext(ctx).Model(&user).Updates(updates).Error; err != nil {
			return nil, mapError(err)
		}
		s.app.Store.DB.First(&user, "id = ?", user.ID)
		return &resourceResponse[persistence.User]{ETag: application.StrongETag("user", user.ID, user.Revision), Body: user}, nil
	})
}

func (s goalRoutes) RegisterRoutes(api huma.API) {
	type listInput struct {
		Status string `query:"status"`
	}
	register(api, "list-goals", http.MethodGet, "/goals", "List goals", userSecurity(), func(ctx context.Context, input *listInput) (*listResponse[persistence.Goal], error) {
		p := principal(ctx)
		var goals []persistence.Goal
		q := s.app.Store.DB.WithContext(ctx).Where("user_id = ?", p.UserID)
		if input.Status != "" {
			q = q.Where("status = ?", input.Status)
		}
		if err := q.Order("created_at DESC").Find(&goals).Error; err != nil {
			return nil, mapError(err)
		}
		out := &listResponse[persistence.Goal]{}
		out.Body.Items = goals
		return out, nil
	})
	type createInput struct {
		IdempotencyKey string `header:"Idempotency-Key"`
		Body           struct {
			Title           string  `json:"title" minLength:"1" maxLength:"200"`
			Description     string  `json:"description,omitempty" maxLength:"4000"`
			SuccessCriteria string  `json:"success_criteria" minLength:"1" maxLength:"2000"`
			TargetDate      *string `json:"target_date,omitempty"`
		}
	}
	register(api, "create-goal", http.MethodPost, "/goals", "Create goal", userSecurity(), func(ctx context.Context, input *createInput) (*resourceResponse[persistence.Goal], error) {
		p := principal(ctx)
		goal := persistence.Goal{Title: input.Body.Title, Description: input.Body.Description, SuccessCriteria: input.Body.SuccessCriteria, TargetDate: input.Body.TargetDate}
		if err := s.app.CreateGoal(ctx, p.UserID, &goal); err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.Goal]{ETag: application.StrongETag("goal", goal.ID, goal.Revision), Body: goal}, nil
	})
	type getInput struct {
		ID string `path:"goal_id"`
	}
	register(api, "get-goal", http.MethodGet, "/goals/{goal_id}", "Get goal", userSecurity(), func(ctx context.Context, input *getInput) (*resourceResponse[persistence.Goal], error) {
		p := principal(ctx)
		var goal persistence.Goal
		if err := s.app.Store.DB.WithContext(ctx).Where("id = ? AND user_id = ?", input.ID, p.UserID).First(&goal).Error; err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.Goal]{ETag: application.StrongETag("goal", goal.ID, goal.Revision), Body: goal}, nil
	})
	type patchInput struct {
		ID      string `path:"goal_id"`
		IfMatch string `header:"If-Match" required:"true"`
		Body    struct {
			Title           *string `json:"title,omitempty"`
			Description     *string `json:"description,omitempty"`
			SuccessCriteria *string `json:"success_criteria,omitempty"`
			Status          *string `json:"status,omitempty"`
			TargetDate      *string `json:"target_date,omitempty"`
		}
	}
	register(api, "patch-goal", http.MethodPatch, "/goals/{goal_id}", "Update goal", userSecurity(), func(ctx context.Context, input *patchInput) (*resourceResponse[persistence.Goal], error) {
		expected, err := revisionFromETag(input.IfMatch)
		if err != nil {
			return nil, mapError(err)
		}
		changes := map[string]any{}
		if input.Body.Title != nil {
			changes["title"] = *input.Body.Title
		}
		if input.Body.Description != nil {
			changes["description"] = *input.Body.Description
		}
		if input.Body.SuccessCriteria != nil {
			changes["success_criteria"] = *input.Body.SuccessCriteria
		}
		if input.Body.Status != nil {
			changes["status"] = *input.Body.Status
		}
		if input.Body.TargetDate != nil {
			changes["target_date"] = *input.Body.TargetDate
		}
		goal, e := s.app.UpdateGoal(ctx, principal(ctx).UserID, input.ID, expected, changes)
		if e != nil {
			return nil, mapError(e)
		}
		return &resourceResponse[persistence.Goal]{ETag: application.StrongETag("goal", goal.ID, goal.Revision), Body: *goal}, nil
	})
}

func (s taskRoutes) RegisterRoutes(api huma.API) {
	type listInput struct {
		GoalID   string `query:"goal_id"`
		ParentID string `query:"parent_id"`
		Status   string `query:"status"`
	}
	register(api, "list-tasks", http.MethodGet, "/tasks", "List tasks", userSecurity(), func(ctx context.Context, input *listInput) (*listResponse[persistence.Task], error) {
		p := principal(ctx)
		var tasks []persistence.Task
		q := s.app.Store.DB.WithContext(ctx).Where("user_id = ?", p.UserID)
		if input.GoalID != "" {
			q = q.Where("goal_id = ?", input.GoalID)
		}
		if input.ParentID != "" {
			q = q.Where("parent_id = ?", input.ParentID)
		}
		if input.Status != "" {
			q = q.Where("status = ?", input.Status)
		}
		if err := q.Order("priority DESC, position, id").Find(&tasks).Error; err != nil {
			return nil, mapError(err)
		}
		out := &listResponse[persistence.Task]{}
		out.Body.Items = tasks
		return out, nil
	})
	type createInput struct {
		IdempotencyKey string `header:"Idempotency-Key"`
		Body           struct {
			GoalID          string  `json:"goal_id"`
			ParentID        *string `json:"parent_id,omitempty"`
			Type            string  `json:"type" enum:"milestone,task,action"`
			Title           string  `json:"title" minLength:"1"`
			Description     string  `json:"description,omitempty"`
			SuccessCriteria string  `json:"success_criteria" minLength:"1"`
			EstimateMinutes int     `json:"estimate_minutes" minimum:"1" maximum:"1440"`
			MinimumAction   string  `json:"minimum_action" minLength:"1"`
			Priority        int     `json:"priority" minimum:"0" maximum:"100"`
			Position        int     `json:"position"`
		}
	}
	register(api, "create-task", http.MethodPost, "/tasks", "Create task", userSecurity(), func(ctx context.Context, input *createInput) (*resourceResponse[persistence.Task], error) {
		b := input.Body
		task := persistence.Task{GoalID: b.GoalID, ParentID: b.ParentID, Type: b.Type, Title: b.Title, Description: b.Description, SuccessCriteria: b.SuccessCriteria, EstimateMinutes: b.EstimateMinutes, MinimumAction: b.MinimumAction, Priority: b.Priority, Position: b.Position}
		if err := s.app.CreateTask(ctx, principal(ctx).UserID, &task); err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.Task]{ETag: application.StrongETag("task", task.ID, task.Revision), Body: task}, nil
	})
	type getInput struct {
		ID string `path:"task_id"`
	}
	register(api, "get-task", http.MethodGet, "/tasks/{task_id}", "Get task", userSecurity(), func(ctx context.Context, input *getInput) (*resourceResponse[persistence.Task], error) {
		var task persistence.Task
		if err := s.app.Store.DB.WithContext(ctx).Where("id = ? AND user_id = ?", input.ID, principal(ctx).UserID).First(&task).Error; err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.Task]{ETag: application.StrongETag("task", task.ID, task.Revision), Body: task}, nil
	})
	type patchInput struct {
		ID      string `path:"task_id"`
		IfMatch string `header:"If-Match" required:"true"`
		Body    struct {
			ParentID        *string `json:"parent_id,omitempty"`
			Title           *string `json:"title,omitempty"`
			Description     *string `json:"description,omitempty"`
			Status          *string `json:"status,omitempty"`
			Priority        *int    `json:"priority,omitempty"`
			EstimateMinutes *int    `json:"estimate_minutes,omitempty"`
			SuccessCriteria *string `json:"success_criteria,omitempty"`
			MinimumAction   *string `json:"minimum_action,omitempty"`
			BlockedReason   *string `json:"blocked_reason,omitempty"`
		}
	}
	register(api, "patch-task", http.MethodPatch, "/tasks/{task_id}", "Update task", userSecurity(), func(ctx context.Context, input *patchInput) (*resourceResponse[persistence.Task], error) {
		expected, e := revisionFromETag(input.IfMatch)
		if e != nil {
			return nil, mapError(e)
		}
		changes := map[string]any{}
		body, _ := json.Marshal(input.Body)
		var raw map[string]any
		_ = json.Unmarshal(body, &raw)
		for k, v := range raw {
			changes[k] = v
		}
		task, err := s.app.UpdateTask(ctx, principal(ctx).UserID, input.ID, expected, changes)
		if err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.Task]{ETag: application.StrongETag("task", task.ID, task.Revision), Body: *task}, nil
	})
	type completionInput struct {
		ID             string `path:"task_id"`
		IfMatch        string `header:"If-Match" required:"true"`
		IdempotencyKey string `header:"Idempotency-Key"`
		Body           struct {
			Type    string `json:"type" enum:"result,step"`
			Summary string `json:"summary" minLength:"1"`
		}
	}
	register(api, "complete-task", http.MethodPost, "/tasks/{task_id}/completions", "Complete task", userSecurity(), func(ctx context.Context, input *completionInput) (*resourceResponse[persistence.Task], error) {
		rev, e := revisionFromETag(input.IfMatch)
		if e != nil {
			return nil, mapError(e)
		}
		task, err := s.app.CompleteTask(ctx, principal(ctx).UserID, input.ID, rev, input.Body.Type, input.Body.Summary, false)
		if err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.Task]{ETag: application.StrongETag("task", task.ID, task.Revision), Body: *task}, nil
	})
	register(api, "reopen-task", http.MethodPost, "/tasks/{task_id}/reopenings", "Reopen task", userSecurity(), func(ctx context.Context, input *completionInput) (*resourceResponse[persistence.Task], error) {
		rev, e := revisionFromETag(input.IfMatch)
		if e != nil {
			return nil, mapError(e)
		}
		task, err := s.app.CompleteTask(ctx, principal(ctx).UserID, input.ID, rev, "task_reopened", input.Body.Summary, true)
		if err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.Task]{ETag: application.StrongETag("task", task.ID, task.Revision), Body: *task}, nil
	})
	register(api, "task-progress", http.MethodGet, "/tasks/{task_id}/progress-events", "Task progress", userSecurity(), func(ctx context.Context, input *getInput) (*listResponse[persistence.ProgressEvent], error) {
		var events []persistence.ProgressEvent
		if err := s.app.Store.DB.WithContext(ctx).Where("task_id = ? AND user_id = ?", input.ID, principal(ctx).UserID).Order("occurred_at DESC").Find(&events).Error; err != nil {
			return nil, mapError(err)
		}
		out := &listResponse[persistence.ProgressEvent]{}
		out.Body.Items = events
		return out, nil
	})
}

func (s taskTreeRoutes) RegisterRoutes(api huma.API) {
	type goalInput struct {
		GoalID string `path:"goal_id"`
	}
	type treeBody struct {
		GoalID    string                 `json:"goal_id"`
		Revision  int                    `json:"revision"`
		Nodes     []map[string]any       `json:"nodes"`
		Proposals []persistence.Proposal `json:"proposals"`
	}
	register(api, "get-task-tree", http.MethodGet, "/goals/{goal_id}/task-tree", "Get task tree", userSecurity(), func(ctx context.Context, input *goalInput) (*resourceResponse[treeBody], error) {
		p := principal(ctx)
		var goal persistence.Goal
		if err := s.app.Store.DB.WithContext(ctx).Where("id = ? AND user_id = ?", input.GoalID, p.UserID).First(&goal).Error; err != nil {
			return nil, mapError(err)
		}
		var tasks []persistence.Task
		s.app.Store.DB.WithContext(ctx).Where("goal_id = ? AND user_id = ?", goal.ID, p.UserID).Find(&tasks)
		var revision int
		s.app.Store.DB.Model(&persistence.TaskTreeRevision{}).Where("goal_id = ?", goal.ID).Select("COALESCE(MAX(revision),0)").Scan(&revision)
		var proposals []persistence.Proposal
		s.app.Store.DB.Where("goal_id = ? AND user_id = ? AND status = 'pending'", goal.ID, p.UserID).Find(&proposals)
		body := treeBody{goal.ID, revision, application.BuildTree(tasks), proposals}
		return &resourceResponse[treeBody]{ETag: application.StrongETag("tree", goal.ID, revision), Body: body}, nil
	})
	type jobInput struct {
		GoalID         string `path:"goal_id"`
		IdempotencyKey string `header:"Idempotency-Key"`
		IfMatch        string `header:"If-Match"`
		Body           struct {
			Instruction string         `json:"instruction"`
			Constraints map[string]any `json:"constraints,omitempty"`
		}
	}
	createJob := func(jobType string) func(context.Context, *jobInput) (*acceptedResponse, error) {
		return func(ctx context.Context, input *jobInput) (*acceptedResponse, error) {
			userID := principal(ctx).UserID
			var goal persistence.Goal
			if err := s.app.Store.DB.WithContext(ctx).Where("id = ? AND user_id = ?", input.GoalID, userID).First(&goal).Error; err != nil {
				return nil, mapError(err)
			}
			var base int
			if err := s.app.Store.DB.WithContext(ctx).Model(&persistence.TaskTreeRevision{}).Where("goal_id = ?", goal.ID).Select("COALESCE(MAX(revision),0)").Scan(&base).Error; err != nil {
				return nil, mapError(err)
			}
			if jobType == "task_tree_revision" && input.IfMatch == "" {
				return nil, mapError(application.ErrPrecondition)
			}
			if input.IfMatch != "" {
				expected, err := revisionFromETag(input.IfMatch)
				if err != nil {
					return nil, mapError(err)
				}
				if base != expected {
					return nil, mapError(application.ErrRevision)
				}
			}
			job, err := s.app.CreateJob(ctx, userID, jobType, "goal", input.GoalID, base, input.Body)
			if err != nil {
				return nil, mapError(err)
			}
			return &acceptedResponse{Location: "/api/v1/agent-jobs/" + job.ID, Body: *job}, nil
		}
	}
	register(api, "generate-task-tree", http.MethodPost, "/goals/{goal_id}/task-tree/generation-jobs", "Generate task tree", userSecurity(), createJob("task_tree_generation"))
	register(api, "revise-task-tree", http.MethodPost, "/goals/{goal_id}/task-tree/revision-jobs", "Revise task tree", userSecurity(), createJob("task_tree_revision"))
	register(api, "list-task-tree-revisions", http.MethodGet, "/goals/{goal_id}/task-tree/revisions", "List task tree revisions", userSecurity(), func(ctx context.Context, input *goalInput) (*listResponse[persistence.TaskTreeRevision], error) {
		var items []persistence.TaskTreeRevision
		if err := s.app.Store.DB.WithContext(ctx).Where("goal_id = ? AND user_id = ?", input.GoalID, principal(ctx).UserID).Order("revision DESC").Find(&items).Error; err != nil {
			return nil, mapError(err)
		}
		out := &listResponse[persistence.TaskTreeRevision]{}
		out.Body.Items = items
		return out, nil
	})
	type proposalInput struct {
		GoalID     string `path:"goal_id"`
		ProposalID string `path:"proposal_id"`
		IfMatch    string `header:"If-Match" required:"true"`
	}
	register(api, "apply-task-tree-proposal", http.MethodPost, "/goals/{goal_id}/task-tree/proposals/{proposal_id}/application", "Apply proposal", userSecurity(), func(ctx context.Context, input *proposalInput) (*itemResponse[map[string]any], error) {
		p := principal(ctx)
		expected, e := revisionFromETag(input.IfMatch)
		if e != nil {
			return nil, mapError(e)
		}
		var proposal persistence.Proposal
		if err := s.app.Store.DB.WithContext(ctx).Where("id = ? AND goal_id = ? AND user_id = ?", input.ProposalID, input.GoalID, p.UserID).First(&proposal).Error; err != nil {
			return nil, mapError(err)
		}
		var current int
		s.app.Store.DB.Model(&persistence.TaskTreeRevision{}).Where("goal_id = ?", input.GoalID).Select("COALESCE(MAX(revision),0)").Scan(&current)
		if current != expected || proposal.BaseRevision != 0 && proposal.BaseRevision != current {
			return nil, mapError(application.ErrRevision)
		}
		var patches []map[string]any
		if err := json.Unmarshal([]byte(proposal.PatchJSON), &patches); err != nil {
			return nil, mapError(application.ErrValidation)
		}
		created, err := s.app.ApplyProposal(ctx, p.UserID, &proposal, patches, expected)
		if err != nil {
			return nil, mapError(err)
		}
		return &itemResponse[map[string]any]{Body: map[string]any{"proposal_id": proposal.ID, "created_tasks": created}}, nil
	})
	register(api, "reject-task-tree-proposal", http.MethodPut, "/goals/{goal_id}/task-tree/proposals/{proposal_id}/rejection", "Reject proposal", userSecurity(), func(ctx context.Context, input *proposalInput) (*itemResponse[persistence.Proposal], error) {
		rev, e := revisionFromETag(input.IfMatch)
		if e != nil {
			return nil, mapError(e)
		}
		proposal, err := s.app.RejectProposal(ctx, principal(ctx).UserID, input.GoalID, input.ProposalID, rev)
		if err != nil {
			return nil, mapError(err)
		}
		return &itemResponse[persistence.Proposal]{Body: *proposal}, nil
	})
}

func (s planRoutes) RegisterRoutes(api huma.API) {
	type listInput struct{}
	register(api, "list-daily-plans", http.MethodGet, "/daily-plans", "List daily plans", userSecurity(), func(ctx context.Context, input *listInput) (*listResponse[persistence.DailyPlan], error) {
		var plans []persistence.DailyPlan
		if err := s.app.Store.DB.WithContext(ctx).Where("user_id = ?", principal(ctx).UserID).Order("local_date DESC").Find(&plans).Error; err != nil {
			return nil, mapError(err)
		}
		out := &listResponse[persistence.DailyPlan]{}
		out.Body.Items = plans
		return out, nil
	})
	type createInput struct {
		IdempotencyKey string `header:"Idempotency-Key"`
		Body           struct {
			LocalDate        string   `json:"local_date"`
			Timezone         string   `json:"timezone"`
			TaskIDs          []string `json:"task_ids,omitempty"`
			AvailableMinutes int      `json:"available_minutes" minimum:"0"`
		}
	}
	register(api, "create-daily-plan", http.MethodPost, "/daily-plans", "Create daily plan", userSecurity(), func(ctx context.Context, input *createInput) (*itemResponse[map[string]any], error) {
		if err := application.ParseDateInZone(input.Body.LocalDate, input.Body.Timezone); err != nil {
			return nil, mapError(err)
		}
		plan, items, err := s.app.CreateDailyPlan(ctx, principal(ctx).UserID, input.Body.LocalDate, input.Body.Timezone, input.Body.TaskIDs, input.Body.AvailableMinutes)
		if err != nil {
			return nil, mapError(err)
		}
		return &itemResponse[map[string]any]{Body: map[string]any{"plan": plan, "items": items}}, nil
	})
	type currentInput struct{}
	register(api, "current-daily-plan", http.MethodGet, "/daily-plans/current", "Get current daily plan", userSecurity(), func(ctx context.Context, input *currentInput) (*resourceResponse[map[string]any], error) {
		p := principal(ctx)
		var user persistence.User
		if err := s.app.Store.DB.First(&user, "id = ?", p.UserID).Error; err != nil {
			return nil, mapError(err)
		}
		loc, _ := time.LoadLocation(user.Timezone)
		date := time.Now().In(loc).Format("2006-01-02")
		var plan persistence.DailyPlan
		if err := s.app.Store.DB.Where("user_id = ? AND local_date = ? AND timezone = ?", p.UserID, date, user.Timezone).First(&plan).Error; err != nil {
			return nil, mapError(err)
		}
		var items []persistence.DailyPlanItem
		s.app.Store.DB.Where("plan_id = ? AND plan_revision = ? AND status != 'superseded'", plan.ID, plan.CurrentRevision).Order("kind, position").Find(&items)
		return &resourceResponse[map[string]any]{ETag: application.StrongETag("plan", plan.ID, plan.Revision), Body: map[string]any{"plan": plan, "items": items}}, nil
	})
	type getInput struct {
		PlanID string `path:"plan_id"`
	}
	register(api, "get-daily-plan", http.MethodGet, "/daily-plans/{plan_id}", "Get daily plan", userSecurity(), func(ctx context.Context, input *getInput) (*resourceResponse[map[string]any], error) {
		var plan persistence.DailyPlan
		if err := s.app.Store.DB.Where("id = ? AND user_id = ?", input.PlanID, principal(ctx).UserID).First(&plan).Error; err != nil {
			return nil, mapError(err)
		}
		var items []persistence.DailyPlanItem
		s.app.Store.DB.Where("plan_id = ? AND plan_revision = ?", plan.ID, plan.CurrentRevision).Order("kind, position").Find(&items)
		return &resourceResponse[map[string]any]{ETag: application.StrongETag("plan", plan.ID, plan.Revision), Body: map[string]any{"plan": plan, "items": items}}, nil
	})
	register(api, "list-daily-plan-revisions", http.MethodGet, "/daily-plans/{plan_id}/revisions", "List plan revisions", userSecurity(), func(ctx context.Context, input *getInput) (*listResponse[persistence.DailyPlanRevision], error) {
		var plan persistence.DailyPlan
		if err := s.app.Store.DB.Where("id = ? AND user_id = ?", input.PlanID, principal(ctx).UserID).First(&plan).Error; err != nil {
			return nil, mapError(err)
		}
		var items []persistence.DailyPlanRevision
		s.app.Store.DB.Where("plan_id = ?", plan.ID).Order("revision DESC").Find(&items)
		out := &listResponse[persistence.DailyPlanRevision]{}
		out.Body.Items = items
		return out, nil
	})
	type patchPlanInput struct {
		PlanID  string `path:"plan_id"`
		IfMatch string `header:"If-Match" required:"true"`
		Body    struct {
			Status string `json:"status" enum:"active,closed,cancelled"`
		}
	}
	register(api, "patch-daily-plan", http.MethodPatch, "/daily-plans/{plan_id}", "Update plan status", userSecurity(), func(ctx context.Context, input *patchPlanInput) (*resourceResponse[persistence.DailyPlan], error) {
		rev, e := revisionFromETag(input.IfMatch)
		if e != nil {
			return nil, mapError(e)
		}
		plan, err := s.app.CloseDailyPlan(ctx, principal(ctx).UserID, input.PlanID, rev, input.Body.Status)
		if err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.DailyPlan]{ETag: application.StrongETag("plan", plan.ID, plan.Revision), Body: *plan}, nil
	})
	type genInput struct {
		IdempotencyKey string `header:"Idempotency-Key"`
		Body           struct {
			LocalDate        string `json:"local_date"`
			Timezone         string `json:"timezone"`
			AvailableMinutes int    `json:"available_minutes"`
			Instruction      string `json:"instruction,omitempty"`
			ReplaceExisting  bool   `json:"replace_existing"`
			BaseRevision     int    `json:"base_revision,omitempty"`
		}
	}
	register(api, "generate-daily-plan", http.MethodPost, "/daily-plans/generation-jobs", "Generate daily plan", userSecurity(), func(ctx context.Context, input *genInput) (*acceptedResponse, error) {
		job, err := s.app.CreateJob(ctx, principal(ctx).UserID, "daily_plan_generation", "user", principal(ctx).UserID, 0, input.Body)
		if err != nil {
			return nil, mapError(err)
		}
		return &acceptedResponse{Location: "/api/v1/agent-jobs/" + job.ID, Body: *job}, nil
	})
	type addItemInput struct {
		PlanID         string `path:"plan_id"`
		IfMatch        string `header:"If-Match" required:"true"`
		IdempotencyKey string `header:"Idempotency-Key"`
		Body           struct {
			TaskID        *string `json:"task_id,omitempty"`
			Kind          string  `json:"kind" enum:"core,support,input"`
			Title         string  `json:"title"`
			Commitment    string  `json:"commitment"`
			MinimumAction string  `json:"minimum_action"`
			TargetMinutes int     `json:"target_minutes"`
			Position      int     `json:"position"`
		}
	}
	register(api, "add-daily-plan-item", http.MethodPost, "/daily-plans/{plan_id}/items", "Add daily plan item", userSecurity(), func(ctx context.Context, input *addItemInput) (*resourceResponse[persistence.DailyPlanItem], error) {
		rev, e := revisionFromETag(input.IfMatch)
		if e != nil {
			return nil, mapError(e)
		}
		b := input.Body
		item := persistence.DailyPlanItem{TaskID: b.TaskID, Kind: b.Kind, Title: b.Title, Commitment: b.Commitment, MinimumAction: b.MinimumAction, TargetMinutes: b.TargetMinutes, Position: b.Position}
		if err := s.app.AddPlanItem(ctx, principal(ctx).UserID, input.PlanID, rev, &item); err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.DailyPlanItem]{ETag: application.StrongETag("dpi", item.ID, item.Revision), Body: item}, nil
	})
	type itemInput struct {
		PlanID string `path:"plan_id"`
		ItemID string `path:"item_id"`
	}
	register(api, "get-daily-plan-item", http.MethodGet, "/daily-plans/{plan_id}/items/{item_id}", "Get plan item", userSecurity(), func(ctx context.Context, input *itemInput) (*resourceResponse[persistence.DailyPlanItem], error) {
		var item persistence.DailyPlanItem
		if err := s.app.Store.DB.Where("id = ? AND plan_id = ? AND user_id = ?", input.ItemID, input.PlanID, principal(ctx).UserID).First(&item).Error; err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.DailyPlanItem]{ETag: application.StrongETag("dpi", item.ID, item.Revision), Body: item}, nil
	})
	type patchItemInput struct {
		PlanID  string `path:"plan_id"`
		ItemID  string `path:"item_id"`
		IfMatch string `header:"If-Match" required:"true"`
		Body    struct {
			Title         *string `json:"title,omitempty"`
			Commitment    *string `json:"commitment,omitempty"`
			MinimumAction *string `json:"minimum_action,omitempty"`
			TargetMinutes *int    `json:"target_minutes,omitempty"`
			Position      *int    `json:"position,omitempty"`
			Status        *string `json:"status,omitempty" enum:"skipped"`
		}
	}
	register(api, "patch-daily-plan-item", http.MethodPatch, "/daily-plans/{plan_id}/items/{item_id}", "Update plan item", userSecurity(), func(ctx context.Context, input *patchItemInput) (*resourceResponse[persistence.DailyPlanItem], error) {
		rev, e := revisionFromETag(input.IfMatch)
		if e != nil {
			return nil, mapError(e)
		}
		rawBytes, _ := json.Marshal(input.Body)
		var updates map[string]any
		_ = json.Unmarshal(rawBytes, &updates)
		item, err := s.app.UpdatePlanItem(ctx, principal(ctx).UserID, input.PlanID, input.ItemID, rev, updates)
		if err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.DailyPlanItem]{ETag: application.StrongETag("dpi", item.ID, item.Revision), Body: *item}, nil
	})
	type completeInput struct {
		PlanID         string `path:"plan_id"`
		ItemID         string `path:"item_id"`
		IfMatch        string `header:"If-Match" required:"true"`
		IdempotencyKey string `header:"Idempotency-Key"`
		Body           struct {
			Type           string   `json:"type" enum:"result,step,time,minimum_action"`
			Summary        string   `json:"summary"`
			WorkSessionIDs []string `json:"work_session_ids,omitempty"`
		}
	}
	register(api, "complete-daily-plan-item", http.MethodPost, "/daily-plans/{plan_id}/items/{item_id}/completions", "Complete plan item", userSecurity(), func(ctx context.Context, input *completeInput) (*resourceResponse[persistence.DailyPlanItem], error) {
		rev, e := revisionFromETag(input.IfMatch)
		if e != nil {
			return nil, mapError(e)
		}
		item, err := s.app.CompletePlanItem(ctx, principal(ctx).UserID, input.ItemID, rev, input.Body.Type, input.Body.Summary, input.Body.WorkSessionIDs)
		if err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.DailyPlanItem]{ETag: application.StrongETag("dpi", item.ID, item.Revision), Body: *item}, nil
	})
	type supportInput struct {
		PlanID         string `path:"plan_id"`
		IfMatch        string `header:"If-Match" required:"true"`
		IdempotencyKey string `header:"Idempotency-Key"`
	}
	register(api, "generate-support-items", http.MethodPost, "/daily-plans/{plan_id}/support-generation-jobs", "Generate support items", userSecurity(), func(ctx context.Context, input *supportInput) (*acceptedResponse, error) {
		var plan persistence.DailyPlan
		if err := s.app.Store.DB.Where("id = ? AND user_id = ?", input.PlanID, principal(ctx).UserID).First(&plan).Error; err != nil {
			return nil, mapError(err)
		}
		rev, err := revisionFromETag(input.IfMatch)
		if err != nil {
			return nil, mapError(err)
		}
		if rev != plan.Revision {
			return nil, mapError(application.ErrRevision)
		}
		var count int64
		s.app.Store.DB.Model(&persistence.DailyPlanItem{}).Where("plan_id = ? AND kind = 'core' AND status != 'satisfied' AND status != 'superseded'", input.PlanID).Count(&count)
		if count > 0 {
			return nil, mapError(domain.ErrSupportBeforeCoreDone)
		}
		job, err := s.app.CreateJob(ctx, principal(ctx).UserID, "support_generation", "daily_plan", input.PlanID, 0, map[string]any{})
		if err != nil {
			return nil, mapError(err)
		}
		return &acceptedResponse{Location: "/api/v1/agent-jobs/" + job.ID, Body: *job}, nil
	})
}

func (s sessionRoutes) RegisterRoutes(api huma.API) {
	type listInput struct{}
	register(api, "list-work-sessions", http.MethodGet, "/work-sessions", "List sessions", userSecurity(), func(ctx context.Context, input *listInput) (*listResponse[persistence.WorkSession], error) {
		var items []persistence.WorkSession
		if err := s.app.Store.DB.Where("user_id = ?", principal(ctx).UserID).Order("created_at DESC").Find(&items).Error; err != nil {
			return nil, mapError(err)
		}
		out := &listResponse[persistence.WorkSession]{}
		out.Body.Items = items
		return out, nil
	})
	type createInput struct {
		IdempotencyKey string `header:"Idempotency-Key"`
		Body           struct {
			TaskID          string    `json:"task_id"`
			DailyPlanItemID *string   `json:"daily_plan_item_id,omitempty"`
			SessionType     string    `json:"session_type"`
			TargetMinutes   int       `json:"target_minutes"`
			StartedAt       time.Time `json:"started_at,omitempty"`
		}
	}
	register(api, "create-work-session", http.MethodPost, "/work-sessions", "Start session", userSecurity(), func(ctx context.Context, input *createInput) (*resourceResponse[persistence.WorkSession], error) {
		b := input.Body
		session := persistence.WorkSession{TaskID: b.TaskID, DailyPlanItemID: b.DailyPlanItemID, SessionType: b.SessionType, TargetMinutes: b.TargetMinutes, StartedAt: b.StartedAt}
		if err := s.app.StartSession(ctx, principal(ctx).UserID, &session); err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.WorkSession]{ETag: application.StrongETag("work", session.ID, session.Revision), Body: session}, nil
	})
	type getInput struct {
		ID string `path:"session_id"`
	}
	register(api, "get-work-session", http.MethodGet, "/work-sessions/{session_id}", "Get session", userSecurity(), func(ctx context.Context, input *getInput) (*resourceResponse[persistence.WorkSession], error) {
		var item persistence.WorkSession
		if err := s.app.Store.DB.Where("id = ? AND user_id = ?", input.ID, principal(ctx).UserID).First(&item).Error; err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.WorkSession]{ETag: application.StrongETag("work", item.ID, item.Revision), Body: item}, nil
	})
	type transitionInput struct {
		ID             string `path:"session_id"`
		IfMatch        string `header:"If-Match" required:"true"`
		IdempotencyKey string `header:"Idempotency-Key"`
		Body           struct {
			At      time.Time `json:"at,omitempty"`
			EndedAt time.Time `json:"ended_at,omitempty"`
			Outcome string    `json:"outcome,omitempty"`
			Note    string    `json:"note,omitempty"`
		}
	}
	for _, definition := range []struct{ id, path, action string }{{"pause-work-session", "/work-sessions/{session_id}/pauses", "pause"}, {"resume-work-session", "/work-sessions/{session_id}/resumptions", "resume"}, {"complete-work-session", "/work-sessions/{session_id}/completions", "complete"}, {"stop-work-session", "/work-sessions/{session_id}/stoppings", "stop"}, {"invalidate-work-session", "/work-sessions/{session_id}/invalidations", "invalidate"}} {
		def := definition
		register(api, def.id, http.MethodPost, def.path, def.action+" session", userSecurity(), func(ctx context.Context, input *transitionInput) (*resourceResponse[persistence.WorkSession], error) {
			rev, e := revisionFromETag(input.IfMatch)
			if e != nil {
				return nil, mapError(e)
			}
			at := input.Body.At
			if !input.Body.EndedAt.IsZero() {
				at = input.Body.EndedAt
			}
			session, err := s.app.TransitionSession(ctx, principal(ctx).UserID, input.ID, rev, def.action, input.Body.Outcome, input.Body.Note, at)
			if err != nil {
				return nil, mapError(err)
			}
			return &resourceResponse[persistence.WorkSession]{ETag: application.StrongETag("work", session.ID, session.Revision), Body: *session}, nil
		})
	}
}

func (s conversationRoutes) RegisterRoutes(api huma.API) {
	type listInput struct{}
	register(api, "list-conversations", http.MethodGet, "/conversations", "List conversations", userSecurity(), func(ctx context.Context, input *listInput) (*listResponse[persistence.Conversation], error) {
		var items []persistence.Conversation
		s.app.Store.DB.Where("user_id = ?", principal(ctx).UserID).Order("created_at DESC").Find(&items)
		out := &listResponse[persistence.Conversation]{}
		out.Body.Items = items
		return out, nil
	})
	type createInput struct {
		IdempotencyKey string `header:"Idempotency-Key"`
		Body           struct {
			GoalID *string `json:"goal_id,omitempty"`
			Title  string  `json:"title"`
		}
	}
	register(api, "create-conversation", http.MethodPost, "/conversations", "Create conversation", userSecurity(), func(ctx context.Context, input *createInput) (*resourceResponse[persistence.Conversation], error) {
		now := persistence.Now()
		if input.Body.GoalID != nil {
			var goal persistence.Goal
			if err := s.app.Store.DB.Where("id = ? AND user_id = ?", *input.Body.GoalID, principal(ctx).UserID).First(&goal).Error; err != nil {
				return nil, mapError(err)
			}
		}
		item := persistence.Conversation{ID: persistence.NewID("conv"), UserID: principal(ctx).UserID, GoalID: input.Body.GoalID, Title: input.Body.Title, Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
		if err := s.app.Store.DB.Create(&item).Error; err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.Conversation]{ETag: application.StrongETag("conv", item.ID, item.Revision), Body: item}, nil
	})
	type convInput struct {
		ID string `path:"conversation_id"`
	}
	register(api, "get-conversation", http.MethodGet, "/conversations/{conversation_id}", "Get conversation", userSecurity(), func(ctx context.Context, input *convInput) (*resourceResponse[persistence.Conversation], error) {
		var item persistence.Conversation
		if err := s.app.Store.DB.Where("id = ? AND user_id = ?", input.ID, principal(ctx).UserID).First(&item).Error; err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.Conversation]{ETag: application.StrongETag("conv", item.ID, item.Revision), Body: item}, nil
	})
	type patchInput struct {
		ID      string `path:"conversation_id"`
		IfMatch string `header:"If-Match" required:"true"`
		Body    struct {
			Title  *string `json:"title,omitempty"`
			Status *string `json:"status,omitempty"`
		}
	}
	register(api, "patch-conversation", http.MethodPatch, "/conversations/{conversation_id}", "Update conversation", userSecurity(), func(ctx context.Context, input *patchInput) (*resourceResponse[persistence.Conversation], error) {
		var item persistence.Conversation
		if err := s.app.Store.DB.Where("id = ? AND user_id = ?", input.ID, principal(ctx).UserID).First(&item).Error; err != nil {
			return nil, mapError(err)
		}
		rev, e := revisionFromETag(input.IfMatch)
		if e != nil || rev != item.Revision {
			return nil, mapError(application.ErrRevision)
		}
		raw, _ := json.Marshal(input.Body)
		var changes map[string]any
		_ = json.Unmarshal(raw, &changes)
		changes["revision"], changes["updated_at"] = item.Revision+1, persistence.Now()
		s.app.Store.DB.Model(&item).Updates(changes)
		s.app.Store.DB.First(&item, "id = ?", item.ID)
		return &resourceResponse[persistence.Conversation]{ETag: application.StrongETag("conv", item.ID, item.Revision), Body: item}, nil
	})
	register(api, "list-conversation-messages", http.MethodGet, "/conversations/{conversation_id}/messages", "List messages", userSecurity(), func(ctx context.Context, input *convInput) (*listResponse[persistence.ConversationMessage], error) {
		var conv persistence.Conversation
		if err := s.app.Store.DB.Where("id = ? AND user_id = ?", input.ID, principal(ctx).UserID).First(&conv).Error; err != nil {
			return nil, mapError(err)
		}
		var items []persistence.ConversationMessage
		s.app.Store.DB.Where("conversation_id = ?", conv.ID).Order("created_at").Find(&items)
		out := &listResponse[persistence.ConversationMessage]{}
		out.Body.Items = items
		return out, nil
	})
	type messageInput struct {
		ID             string `path:"conversation_id"`
		IdempotencyKey string `header:"Idempotency-Key"`
		Body           struct {
			Content            string `json:"content" minLength:"1"`
			TranscriptionJobID string `json:"transcription_job_id,omitempty"`
		}
	}
	register(api, "create-conversation-message", http.MethodPost, "/conversations/{conversation_id}/messages", "Send message", userSecurity(), func(ctx context.Context, input *messageInput) (*acceptedResponse, error) {
		p := principal(ctx)
		var conv persistence.Conversation
		if err := s.app.Store.DB.Where("id = ? AND user_id = ?", input.ID, p.UserID).First(&conv).Error; err != nil {
			return nil, mapError(err)
		}
		if input.Body.TranscriptionJobID != "" {
			var transcription persistence.AgentJob
			if err := s.app.Store.DB.Where("id = ? AND user_id = ? AND type = 'voice_transcription' AND status = 'succeeded'", input.Body.TranscriptionJobID, p.UserID).First(&transcription).Error; err != nil {
				return nil, mapError(err)
			}
		}
		job, err := s.app.CreateJob(ctx, p.UserID, "conversation", "conversation", conv.ID, conv.Revision, map[string]any{"conversation_id": conv.ID, "content": input.Body.Content})
		if err != nil {
			return nil, mapError(err)
		}
		message := persistence.ConversationMessage{ID: persistence.NewID("msg"), UserID: p.UserID, ConversationID: conv.ID, Role: "user", Content: input.Body.Content, JobID: job.ID, TranscriptionJobID: input.Body.TranscriptionJobID, CreatedAt: persistence.Now()}
		if err := s.app.Store.DB.Create(&message).Error; err != nil {
			return nil, mapError(err)
		}
		return &acceptedResponse{Location: "/api/v1/agent-jobs/" + job.ID, Body: *job}, nil
	})
	type voiceInput struct {
		IdempotencyKey string `header:"Idempotency-Key"`
		RawBody        multipart.Form
	}
	register(api, "create-voice-transcription", http.MethodPost, "/voice-transcription-jobs", "Transcribe voice", userSecurity(), func(ctx context.Context, input *voiceInput) (*acceptedResponse, error) {
		files := input.RawBody.File["audio"]
		if len(files) != 1 {
			return nil, huma.Error400BadRequest("one audio file is required")
		}
		file, err := files[0].Open()
		if err != nil {
			return nil, huma.Error400BadRequest("cannot read audio")
		}
		defer file.Close()
		limited, err := io.ReadAll(io.LimitReader(file, 16<<20))
		if err != nil || len(limited) == 0 {
			return nil, huma.Error400BadRequest("audio is empty or invalid")
		}
		if err := os.MkdirAll(s.cfg.AudioDir, 0o750); err != nil {
			return nil, mapError(err)
		}
		audioPath := filepath.Join(s.cfg.AudioDir, persistence.NewID("audio")+filepath.Ext(files[0].Filename))
		if err := os.WriteFile(audioPath, limited, 0o600); err != nil {
			return nil, mapError(err)
		}
		job, err := s.app.CreateJob(ctx, principal(ctx).UserID, "voice_transcription", "audio", "", 0, map[string]any{"filename": filepath.Base(files[0].Filename), "size": len(limited), "path": audioPath})
		if err != nil {
			_ = os.Remove(audioPath)
			return nil, mapError(err)
		}
		return &acceptedResponse{Location: "/api/v1/agent-jobs/" + job.ID, Body: *job}, nil
	})
}

func (s jobRoutes) RegisterRoutes(api huma.API) {
	type listInput struct{}
	register(api, "list-agent-jobs", http.MethodGet, "/agent-jobs", "List agent jobs", userSecurity(), func(ctx context.Context, input *listInput) (*listResponse[persistence.AgentJob], error) {
		var items []persistence.AgentJob
		s.app.Store.DB.Where("user_id = ?", principal(ctx).UserID).Order("created_at DESC").Find(&items)
		out := &listResponse[persistence.AgentJob]{}
		out.Body.Items = items
		return out, nil
	})
	type jobInput struct {
		ID string `path:"job_id"`
	}
	register(api, "get-agent-job", http.MethodGet, "/agent-jobs/{job_id}", "Get agent job", userSecurity(), func(ctx context.Context, input *jobInput) (*resourceResponse[persistence.AgentJob], error) {
		var job persistence.AgentJob
		if err := s.app.Store.DB.Where("id = ? AND user_id = ?", input.ID, principal(ctx).UserID).First(&job).Error; err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.AgentJob]{ETag: application.StrongETag("job", job.ID, job.Revision), Body: job}, nil
	})
	type mutateInput struct {
		ID             string `path:"job_id"`
		IfMatch        string `header:"If-Match" required:"true"`
		IdempotencyKey string `header:"Idempotency-Key"`
	}
	register(api, "cancel-agent-job", http.MethodPut, "/agent-jobs/{job_id}/cancellation", "Cancel agent job", userSecurity(), func(ctx context.Context, input *mutateInput) (*resourceResponse[persistence.AgentJob], error) {
		rev, e := revisionFromETag(input.IfMatch)
		if e != nil {
			return nil, mapError(e)
		}
		job, err := s.app.CancelJob(ctx, principal(ctx).UserID, input.ID, rev)
		if err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.AgentJob]{ETag: application.StrongETag("job", job.ID, job.Revision), Body: *job}, nil
	})
	register(api, "retry-agent-job", http.MethodPost, "/agent-jobs/{job_id}/retries", "Retry agent job", userSecurity(), func(ctx context.Context, input *mutateInput) (*acceptedResponse, error) {
		rev, e := revisionFromETag(input.IfMatch)
		if e != nil {
			return nil, mapError(e)
		}
		job, err := s.app.RetryJob(ctx, principal(ctx).UserID, input.ID, rev)
		if err != nil {
			return nil, mapError(err)
		}
		return &acceptedResponse{Location: "/api/v1/agent-jobs/" + job.ID, Body: *job}, nil
	})
	type callbackInput struct {
		ID             string `path:"job_id"`
		Authorization  string `header:"Authorization" required:"true"`
		IdempotencyKey string `header:"Idempotency-Key"`
		Body           struct {
			AttemptNo    int            `json:"attempt_no"`
			LeaseVersion int            `json:"lease_version"`
			RunToken     string         `json:"run_token"`
			Status       string         `json:"status" enum:"succeeded,failed,cancelled"`
			Result       map[string]any `json:"result,omitempty"`
			ErrorCode    string         `json:"error_code,omitempty"`
			ErrorMessage string         `json:"error_message,omitempty"`
		}
	}
	register(api, "agent-job-callback", http.MethodPost, "/agent-jobs/{job_id}/callbacks", "External worker callback", serviceSecurity("serviceAgentJobsWrite"), func(ctx context.Context, input *callbackInput) (*resourceResponse[persistence.AgentJob], error) {
		job, err := s.app.AgentCallback(ctx, principal(ctx).UserID, input.ID, input.Body.AttemptNo, input.Body.LeaseVersion, input.Body.RunToken, input.Body.Status, input.Body.Result, input.Body.ErrorCode, input.Body.ErrorMessage)
		if err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.AgentJob]{ETag: application.StrongETag("job", job.ID, job.Revision), Body: *job}, nil
	})
}

func (s deviceRoutes) RegisterRoutes(api huma.API) {
	type listInput struct{}
	register(api, "list-devices", http.MethodGet, "/devices", "List devices", userSecurity(), func(ctx context.Context, input *listInput) (*listResponse[persistence.Device], error) {
		var items []persistence.Device
		s.app.Store.DB.Where("user_id = ?", principal(ctx).UserID).Find(&items)
		out := &listResponse[persistence.Device]{}
		out.Body.Items = items
		return out, nil
	})
	type createInput struct {
		IdempotencyKey string `header:"Idempotency-Key"`
		Body           struct {
			Name         string         `json:"name"`
			Kind         string         `json:"kind"`
			Timezone     string         `json:"timezone"`
			Capabilities map[string]any `json:"capabilities,omitempty"`
		}
	}
	type deviceCreated struct {
		Device      persistence.Device `json:"device"`
		DeviceToken string             `json:"device_token"`
	}
	register(api, "create-device", http.MethodPost, "/devices", "Register device", userSecurity(), func(ctx context.Context, input *createInput) (*itemResponse[deviceCreated], error) {
		capabilities, _ := json.Marshal(input.Body.Capabilities)
		device := persistence.Device{Name: input.Body.Name, Kind: input.Body.Kind, Timezone: input.Body.Timezone, CapabilitiesJSON: string(capabilities)}
		token, err := s.app.RegisterDevice(ctx, principal(ctx).UserID, &device)
		if err != nil {
			return nil, mapError(err)
		}
		return &itemResponse[deviceCreated]{Body: deviceCreated{device, token}}, nil
	})
	type deviceInput struct {
		ID string `path:"device_id"`
	}
	register(api, "get-device", http.MethodGet, "/devices/{device_id}", "Get device", userSecurity(), func(ctx context.Context, input *deviceInput) (*resourceResponse[persistence.Device], error) {
		var item persistence.Device
		if err := s.app.Store.DB.Where("id = ? AND user_id = ?", input.ID, principal(ctx).UserID).First(&item).Error; err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.Device]{ETag: application.StrongETag("device", item.ID, item.Revision), Body: item}, nil
	})
	type updateInput struct {
		ID      string `path:"device_id"`
		IfMatch string `header:"If-Match" required:"true"`
		Body    struct {
			Name     *string `json:"name,omitempty"`
			Timezone *string `json:"timezone,omitempty"`
		}
	}
	register(api, "patch-device", http.MethodPatch, "/devices/{device_id}", "Update device", userSecurity(), func(ctx context.Context, input *updateInput) (*resourceResponse[persistence.Device], error) {
		var item persistence.Device
		if err := s.app.Store.DB.Where("id = ? AND user_id = ?", input.ID, principal(ctx).UserID).First(&item).Error; err != nil {
			return nil, mapError(err)
		}
		rev, e := revisionFromETag(input.IfMatch)
		if e != nil || rev != item.Revision {
			return nil, mapError(application.ErrRevision)
		}
		raw, _ := json.Marshal(input.Body)
		var updates map[string]any
		_ = json.Unmarshal(raw, &updates)
		updates["revision"], updates["updated_at"] = item.Revision+1, persistence.Now()
		s.app.Store.DB.Model(&item).Updates(updates)
		s.app.Store.DB.First(&item, "id = ?", item.ID)
		return &resourceResponse[persistence.Device]{ETag: application.StrongETag("device", item.ID, item.Revision), Body: item}, nil
	})
	type revokeInput struct {
		ID      string `path:"device_id"`
		IfMatch string `header:"If-Match" required:"true"`
	}
	register(api, "revoke-device", http.MethodPut, "/devices/{device_id}/revocation", "Revoke device", userSecurity(), func(ctx context.Context, input *revokeInput) (*resourceResponse[persistence.Device], error) {
		var item persistence.Device
		if err := s.app.Store.DB.Where("id = ? AND user_id = ?", input.ID, principal(ctx).UserID).First(&item).Error; err != nil {
			return nil, mapError(err)
		}
		rev, e := revisionFromETag(input.IfMatch)
		if e != nil || rev != item.Revision {
			return nil, mapError(application.ErrRevision)
		}
		s.app.Store.DB.Model(&item).Updates(map[string]any{"status": "revoked", "revision": item.Revision + 1, "updated_at": persistence.Now()})
		s.app.Store.DB.First(&item, "id = ?", item.ID)
		return &resourceResponse[persistence.Device]{ETag: application.StrongETag("device", item.ID, item.Revision), Body: item}, nil
	})
	type rotateInput struct {
		ID             string `path:"device_id"`
		IfMatch        string `header:"If-Match" required:"true"`
		IdempotencyKey string `header:"Idempotency-Key"`
	}
	register(api, "rotate-device-token", http.MethodPost, "/devices/{device_id}/token-rotations", "Rotate device token", userSecurity(), func(ctx context.Context, input *rotateInput) (*itemResponse[deviceCreated], error) {
		rev, e := revisionFromETag(input.IfMatch)
		if e != nil {
			return nil, mapError(e)
		}
		device, token, err := s.app.RotateDeviceToken(ctx, principal(ctx).UserID, input.ID, rev)
		if err != nil {
			return nil, mapError(err)
		}
		return &itemResponse[deviceCreated]{Body: deviceCreated{*device, token}}, nil
	})
	type pollInput struct {
		DeviceToken string `header:"X-Device-Token" required:"true"`
		IfNoneMatch string `header:"If-None-Match"`
	}
	type pollBody struct {
		DeviceID             string                      `json:"device_id"`
		LocalDate            string                      `json:"local_date"`
		Timezone             string                      `json:"timezone"`
		PlanStatus           string                      `json:"plan_status"`
		PlanRevision         int                         `json:"plan_revision"`
		UpdatedAt            time.Time                   `json:"updated_at"`
		CoreItems            []persistence.DailyPlanItem `json:"core_items"`
		NextPollAfterSeconds int                         `json:"next_poll_after_seconds"`
	}
	type pollResponse struct {
		Status       int    `status:"default"`
		ETag         string `header:"ETag"`
		CacheControl string `header:"Cache-Control"`
		Body         *pollBody
	}
	register(api, "poll-device", http.MethodGet, "/devices/self/poll", "Poll device view", publicSecurity(), func(ctx context.Context, input *pollInput) (*pollResponse, error) {
		device, err := s.app.DeviceByToken(ctx, input.DeviceToken)
		if err != nil {
			return nil, huma.Error401Unauthorized("invalid device token")
		}
		loc, _ := time.LoadLocation(device.Timezone)
		date := time.Now().In(loc).Format("2006-01-02")
		var plan persistence.DailyPlan
		body := pollBody{DeviceID: device.ID, LocalDate: date, Timezone: device.Timezone, CoreItems: []persistence.DailyPlanItem{}, NextPollAfterSeconds: 300, UpdatedAt: persistence.Now()}
		if err := s.app.Store.DB.Where("user_id = ? AND local_date = ? AND timezone = ?", device.UserID, date, device.Timezone).First(&plan).Error; err == nil {
			body.PlanStatus, body.PlanRevision, body.UpdatedAt = plan.Status, plan.CurrentRevision, plan.UpdatedAt
			s.app.Store.DB.Where("plan_id = ? AND plan_revision = ? AND kind = 'core' AND status != 'superseded'", plan.ID, plan.CurrentRevision).Order("position").Limit(3).Find(&body.CoreItems)
		}
		payload, _ := json.Marshal(body)
		etag := "\"device-view_" + device.ID + "_" + persistence.Hash(string(payload))[:16] + "\""
		if input.IfNoneMatch == etag {
			return &pollResponse{Status: http.StatusNotModified, ETag: etag, CacheControl: "private, no-cache", Body: nil}, nil
		}
		return &pollResponse{Status: 200, ETag: etag, CacheControl: "private, no-cache", Body: &body}, nil
	})
}

func (s panelRoutes) RegisterRoutes(api huma.API) {
	type input struct {
		Authorization string `header:"Authorization"`
	}
	type current struct{ ID, Title, MinimumAction string }
	type body struct {
		LocalDate     string    `json:"local_date"`
		Timezone      string    `json:"timezone"`
		DailyPlanID   string    `json:"daily_plan_id,omitempty"`
		PlanStatus    string    `json:"plan_status"`
		CoreTotal     int       `json:"core_total"`
		CoreSatisfied int       `json:"core_satisfied"`
		CurrentItem   *current  `json:"current_item,omitempty"`
		HasBlockers   bool      `json:"has_blockers"`
		EntryURL      string    `json:"entry_url"`
		UpdatedAt     time.Time `json:"updated_at"`
	}
	register(api, "panel-summary", http.MethodGet, "/panel/summary", "FastResearch panel summary", publicSecurity(), func(ctx context.Context, input *input) (*itemResponse[body], error) {
		var userID string
		if input.Authorization != "" {
			p, err := s.auth.Authenticate(input.Authorization)
			if err == nil {
				userID = p.UserID
			} else {
				service, serviceErr := s.auth.AuthenticatePanel(input.Authorization)
				if serviceErr != nil {
					return nil, huma.Error401Unauthorized("authentication required")
				}
				userID = service.UserID
			}
		} else {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		var user persistence.User
		if err := s.app.Store.DB.First(&user, "id = ?", userID).Error; err != nil {
			return nil, mapError(err)
		}
		loc, _ := time.LoadLocation(user.Timezone)
		date := time.Now().In(loc).Format("2006-01-02")
		result := body{LocalDate: date, Timezone: user.Timezone, EntryURL: "/today", UpdatedAt: persistence.Now()}
		var plan persistence.DailyPlan
		if err := s.app.Store.DB.Where("user_id = ? AND local_date = ? AND timezone = ?", user.ID, date, user.Timezone).First(&plan).Error; err == nil {
			result.DailyPlanID, result.PlanStatus, result.UpdatedAt = plan.ID, plan.Status, plan.UpdatedAt
			var items []persistence.DailyPlanItem
			s.app.Store.DB.Where("plan_id = ? AND plan_revision = ? AND kind = 'core' AND status != 'superseded'", plan.ID, plan.CurrentRevision).Order("position").Find(&items)
			result.CoreTotal = len(items)
			for _, item := range items {
				if item.Status == "satisfied" {
					result.CoreSatisfied++
				} else if result.CurrentItem == nil {
					result.CurrentItem = &current{item.ID, item.Title, item.MinimumAction}
				}
			}
		}
		var blockers int64
		s.app.Store.DB.Model(&persistence.Task{}).Where("user_id = ? AND status = 'blocked'", user.ID).Count(&blockers)
		result.HasBlockers = blockers > 0
		return &itemResponse[body]{Body: result}, nil
	})
}

type externalImportSourceBody struct {
	System      string `json:"system" enum:"fastinsight,fastnews,fastread,fastwrite"`
	ExternalID  string `json:"external_id" minLength:"1" maxLength:"500"`
	URL         string `json:"url,omitempty" maxLength:"4000"`
	ContentHash string `json:"content_hash,omitempty" maxLength:"500"`
}

type externalImportBody struct {
	ID              string                   `json:"id"`
	SchemaVersion   string                   `json:"schema_version"`
	TraceID         string                   `json:"trace_id,omitempty"`
	Source          externalImportSourceBody `json:"source"`
	Kind            string                   `json:"kind"`
	Title           string                   `json:"title"`
	Description     string                   `json:"description"`
	SuggestedGoalID *string                  `json:"suggested_goal_id,omitempty"`
	Artifacts       []map[string]any         `json:"artifacts"`
	Metadata        map[string]any           `json:"metadata"`
	Status          string                   `json:"status"`
	TaskID          *string                  `json:"task_id,omitempty"`
	DecisionNote    string                   `json:"decision_note,omitempty"`
	Revision        int                      `json:"revision"`
	DecidedAt       *time.Time               `json:"decided_at,omitempty"`
	CreatedAt       time.Time                `json:"created_at"`
	UpdatedAt       time.Time                `json:"updated_at"`
}

func externalImportResponse(item persistence.ExternalImport) externalImportBody {
	artifacts := []map[string]any{}
	metadata := map[string]any{}
	_ = json.Unmarshal([]byte(item.ArtifactsJSON), &artifacts)
	_ = json.Unmarshal([]byte(item.MetadataJSON), &metadata)
	return externalImportBody{ID: item.ID, SchemaVersion: item.SchemaVersion, TraceID: item.TraceID, Source: externalImportSourceBody{System: item.SourceSystem, ExternalID: item.SourceExternalID, URL: item.SourceURL, ContentHash: item.ContentHash}, Kind: item.Kind, Title: item.Title, Description: item.Description, SuggestedGoalID: item.SuggestedGoalID, Artifacts: artifacts, Metadata: metadata, Status: item.Status, TaskID: item.TaskID, DecisionNote: item.DecisionNote, Revision: item.Revision, DecidedAt: item.DecidedAt, CreatedAt: item.CreatedAt, UpdatedAt: item.UpdatedAt}
}

func (s importRoutes) RegisterRoutes(api huma.API) {
	type createInput struct {
		IdempotencyKey string `header:"Idempotency-Key" required:"true"`
		Body           struct {
			SchemaVersion   string                   `json:"schema_version,omitempty"`
			TraceID         string                   `json:"trace_id,omitempty" maxLength:"200"`
			Source          externalImportSourceBody `json:"source"`
			Kind            string                   `json:"kind" enum:"candidate_task,research_material,progress_evidence,review_issue,generated_report"`
			Title           string                   `json:"title" minLength:"1" maxLength:"300"`
			Description     string                   `json:"description,omitempty" maxLength:"8000"`
			SuggestedGoalID *string                  `json:"suggested_goal_id,omitempty"`
			Artifacts       []map[string]any         `json:"artifacts,omitempty" maxItems:"100"`
			Metadata        map[string]any           `json:"metadata,omitempty"`
		}
	}
	register(api, "create-import", http.MethodPost, "/imports", "Create external import", userOrServiceSecurity("serviceImportsWrite"), func(ctx context.Context, input *createInput) (*resourceResponse[externalImportBody], error) {
		if input.Body.Artifacts == nil {
			input.Body.Artifacts = []map[string]any{}
		}
		if input.Body.Metadata == nil {
			input.Body.Metadata = map[string]any{}
		}
		artifacts, err := json.Marshal(input.Body.Artifacts)
		if err != nil {
			return nil, mapError(application.ErrValidation)
		}
		metadata, err := json.Marshal(input.Body.Metadata)
		if err != nil {
			return nil, mapError(application.ErrValidation)
		}
		item := persistence.ExternalImport{SchemaVersion: input.Body.SchemaVersion, TraceID: input.Body.TraceID, SourceSystem: input.Body.Source.System, SourceExternalID: input.Body.Source.ExternalID, SourceURL: input.Body.Source.URL, ContentHash: input.Body.Source.ContentHash, Kind: input.Body.Kind, Title: input.Body.Title, Description: input.Body.Description, SuggestedGoalID: input.Body.SuggestedGoalID, ArtifactsJSON: string(artifacts), MetadataJSON: string(metadata)}
		if err := s.app.CreateExternalImport(ctx, principal(ctx).UserID, &item); err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[externalImportBody]{ETag: application.StrongETag("import", item.ID, item.Revision), Body: externalImportResponse(item)}, nil
	})

	type listInput struct {
		Status           string `query:"status" enum:"candidate,converted,rejected"`
		SourceSystem     string `query:"source_system" enum:"fastinsight,fastnews,fastread,fastwrite"`
		Kind             string `query:"kind" enum:"candidate_task,research_material,progress_evidence,review_issue,generated_report"`
		SourceExternalID string `query:"source_external_id"`
		Limit            int    `query:"limit" minimum:"1" maximum:"100" default:"20"`
		Cursor           string `query:"cursor"`
	}
	register(api, "list-imports", http.MethodGet, "/imports", "List external imports", userOrServiceSecurity("serviceImportsRead"), func(ctx context.Context, input *listInput) (*listResponse[externalImportBody], error) {
		var items []persistence.ExternalImport
		q := s.app.Store.DB.WithContext(ctx).Where("user_id = ?", principal(ctx).UserID)
		if input.Status != "" {
			q = q.Where("status = ?", input.Status)
		}
		if input.SourceSystem != "" {
			q = q.Where("source_system = ?", input.SourceSystem)
		}
		if input.Kind != "" {
			q = q.Where("kind = ?", input.Kind)
		}
		if input.SourceExternalID != "" {
			q = q.Where("source_external_id = ?", input.SourceExternalID)
		}
		if input.Cursor != "" {
			var cursorTime time.Time
			var cursorID string
			if err := decodeImportCursor(input.Cursor, &cursorTime, &cursorID); err != nil {
				return nil, mapError(application.ErrValidation)
			}
			q = q.Where("created_at < ? OR (created_at = ? AND id < ?)", cursorTime, cursorTime, cursorID)
		}
		if err := q.Order("created_at DESC, id DESC").Limit(input.Limit + 1).Find(&items).Error; err != nil {
			return nil, mapError(err)
		}
		out := &listResponse[externalImportBody]{}
		if len(items) > input.Limit {
			out.Body.HasMore = true
			items = items[:input.Limit]
			cursor := encodeImportCursor(items[len(items)-1].CreatedAt, items[len(items)-1].ID)
			out.Body.NextCursor = &cursor
		}
		out.Body.Items = make([]externalImportBody, 0, len(items))
		for _, item := range items {
			out.Body.Items = append(out.Body.Items, externalImportResponse(item))
		}
		return out, nil
	})

	type getInput struct {
		ID string `path:"import_id"`
	}
	register(api, "get-import", http.MethodGet, "/imports/{import_id}", "Get external import", userOrServiceSecurity("serviceImportsRead"), func(ctx context.Context, input *getInput) (*resourceResponse[externalImportBody], error) {
		var item persistence.ExternalImport
		if err := s.app.Store.DB.WithContext(ctx).Where("id = ? AND user_id = ?", input.ID, principal(ctx).UserID).First(&item).Error; err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[externalImportBody]{ETag: application.StrongETag("import", item.ID, item.Revision), Body: externalImportResponse(item)}, nil
	})

	type conversionInput struct {
		ID             string `path:"import_id"`
		IfMatch        string `header:"If-Match" required:"true"`
		IdempotencyKey string `header:"Idempotency-Key" required:"true"`
		Body           struct {
			Mode            string  `json:"mode" enum:"create,attach" required:"true" doc:"Use create to create a Task or attach to link an existing Task."`
			ExistingTaskID  *string `json:"existing_task_id,omitempty"`
			GoalID          string  `json:"goal_id,omitempty"`
			ParentID        *string `json:"parent_id,omitempty"`
			Type            string  `json:"type,omitempty" enum:"milestone,task,action"`
			Title           string  `json:"title,omitempty" maxLength:"300"`
			Description     string  `json:"description,omitempty" maxLength:"8000"`
			SuccessCriteria string  `json:"success_criteria,omitempty" maxLength:"2000"`
			MinimumAction   string  `json:"minimum_action,omitempty" maxLength:"2000"`
			Priority        int     `json:"priority,omitempty" minimum:"0" maximum:"100"`
			EstimateMinutes int     `json:"estimate_minutes,omitempty" minimum:"0" maximum:"1440"`
			Position        int     `json:"position,omitempty" minimum:"0"`
			DecisionNote    string  `json:"decision_note,omitempty" maxLength:"2000"`
		}
	}
	type conversionBody struct {
		Import externalImportBody `json:"import"`
		Task   persistence.Task   `json:"task"`
	}
	register(api, "convert-import", http.MethodPost, "/imports/{import_id}/conversion", "Convert external import to task", userSecurity(), func(ctx context.Context, input *conversionInput) (*resourceResponse[conversionBody], error) {
		revision, err := revisionFromResourceETag(input.IfMatch, "import", input.ID)
		if err != nil {
			return nil, mapError(err)
		}
		if input.Body.Mode == "attach" && input.Body.ExistingTaskID == nil || input.Body.Mode == "create" && input.Body.ExistingTaskID != nil {
			return nil, mapError(application.ErrValidation)
		}
		if input.Body.Mode == "attach" && (input.Body.GoalID != "" || input.Body.ParentID != nil || input.Body.Type != "" || input.Body.Title != "" || input.Body.Description != "" || input.Body.SuccessCriteria != "" || input.Body.MinimumAction != "" || input.Body.Priority != 0 || input.Body.EstimateMinutes != 0 || input.Body.Position != 0) {
			return nil, mapError(application.ErrValidation)
		}
		command := application.ConvertExternalImportCommand{ExistingTaskID: input.Body.ExistingTaskID, GoalID: input.Body.GoalID, ParentID: input.Body.ParentID, Type: input.Body.Type, Title: input.Body.Title, Description: input.Body.Description, SuccessCriteria: input.Body.SuccessCriteria, MinimumAction: input.Body.MinimumAction, Priority: input.Body.Priority, EstimateMinutes: input.Body.EstimateMinutes, Position: input.Body.Position, DecisionNote: input.Body.DecisionNote}
		item, task, err := s.app.ConvertExternalImport(ctx, principal(ctx).UserID, input.ID, revision, command)
		if err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[conversionBody]{ETag: application.StrongETag("import", item.ID, item.Revision), Body: conversionBody{Import: externalImportResponse(*item), Task: *task}}, nil
	})

	type rejectionInput struct {
		ID             string `path:"import_id"`
		IfMatch        string `header:"If-Match" required:"true"`
		IdempotencyKey string `header:"Idempotency-Key" required:"true"`
		Body           struct {
			Note string `json:"note,omitempty" maxLength:"2000"`
		}
	}
	register(api, "reject-import", http.MethodPut, "/imports/{import_id}/rejection", "Reject external import", userSecurity(), func(ctx context.Context, input *rejectionInput) (*resourceResponse[externalImportBody], error) {
		revision, err := revisionFromResourceETag(input.IfMatch, "import", input.ID)
		if err != nil {
			return nil, mapError(err)
		}
		item, err := s.app.RejectExternalImport(ctx, principal(ctx).UserID, input.ID, revision, input.Body.Note)
		if err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[externalImportBody]{ETag: application.StrongETag("import", item.ID, item.Revision), Body: externalImportResponse(*item)}, nil
	})
}

func (s *Server) static() {
	index := filepath.Join(s.cfg.WebDist, "index.html")
	if _, err := os.Stat(index); err != nil {
		return
	}
	s.Engine.Static("/assets", filepath.Join(s.cfg.WebDist, "assets"))
	s.Engine.NoRoute(func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.JSON(404, gin.H{"title": "Not Found", "status": 404})
			return
		}
		c.File(index)
	})
}

func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if id == "" {
			id = persistence.NewID("req")
		}
		c.Header("X-Request-ID", id)
		c.Next()
	}
}

type captureWriter struct {
	gin.ResponseWriter
	body bytes.Buffer
}

func (w *captureWriter) Write(data []byte) (int, error) {
	w.body.Write(data)
	return w.ResponseWriter.Write(data)
}

type replayEnvelope struct {
	Body        string            `json:"body"`
	ContentType string            `json:"content_type"`
	Headers     map[string]string `json:"headers"`
}

func idempotencyMiddleware(store *persistence.Store, authService *platformauth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
		if key == "" || c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead {
			c.Next()
			return
		}
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"title": "Bad Request", "status": 400, "detail": "cannot read request body"})
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		principalSeed := c.GetHeader("Authorization") + c.GetHeader("X-Device-Token")
		if bearer := c.GetHeader("Authorization"); bearer != "" {
			if principal, authErr := authService.Authenticate(bearer); authErr == nil {
				principalSeed = "user:" + principal.UserID
			} else if service, serviceErr := authService.AuthenticateServicePrincipal(bearer); serviceErr == nil {
				scopes := append([]string(nil), service.Scopes...)
				sort.Strings(scopes)
				principalSeed = "service:" + service.ClientID + ":" + service.UserID + ":" + strings.Join(scopes, ",")
			}
		}
		if c.Request.URL.Path == "/api/v1/auth/refresh" {
			var refresh struct {
				RefreshToken string `json:"refresh_token"`
			}
			_ = json.Unmarshal(body, &refresh)
			var session persistence.Session
			if err := store.DB.Where("refresh_hash = ?", persistence.Hash(refresh.RefreshToken)).First(&session).Error; err == nil {
				principalSeed = "refresh-family:" + session.FamilyID
			} else {
				principalSeed = "refresh-token:" + persistence.Hash(refresh.RefreshToken)
			}
		}
		principalHash := persistence.Hash(principalSeed)
		requestHash := persistence.Hash(c.Request.Method + "\n" + c.Request.URL.RequestURI() + "\n" + string(body))
		var record persistence.IdempotencyRecord
		err = store.DB.Where("principal = ? AND method = ? AND path = ? AND idem_key = ? AND expires_at > ?", principalHash, c.Request.Method, c.Request.URL.Path, key, persistence.Now()).First(&record).Error
		if err == nil {
			if record.RequestHash != requestHash {
				c.AbortWithStatusJSON(http.StatusConflict, gin.H{"title": "Idempotency key reused", "status": 409, "detail": "the idempotency key was used for a different request", "code": "IDEMPOTENCY_KEY_REUSED"})
				return
			}
			if record.StatusCode == 0 {
				c.AbortWithStatusJSON(http.StatusConflict, gin.H{"title": "Request in progress", "status": 409, "detail": "an identical idempotent request is still in progress", "code": "IDEMPOTENCY_REQUEST_IN_PROGRESS"})
				return
			}
			var envelope replayEnvelope
			if json.Unmarshal([]byte(record.ResponseJSON), &envelope) == nil {
				for name, value := range envelope.Headers {
					c.Header(name, value)
				}
				c.Header("Idempotency-Replayed", "true")
				c.Data(record.StatusCode, envelope.ContentType, []byte(envelope.Body))
				c.Abort()
				return
			}
		} else if !persistence.IsNotFound(err) {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"title": "Internal Server Error", "status": 500})
			return
		}
		now := persistence.Now()
		record = persistence.IdempotencyRecord{ID: persistence.NewID("idem"), Principal: principalHash, Method: c.Request.Method, Path: c.Request.URL.Path, IdemKey: key, RequestHash: requestHash, StatusCode: 0, ResponseJSON: "{}", ExpiresAt: now.Add(30 * 24 * time.Hour), CreatedAt: now}
		if err := store.DB.Create(&record).Error; err != nil {
			c.AbortWithStatusJSON(http.StatusConflict, gin.H{"title": "Request in progress", "status": 409, "code": "IDEMPOTENCY_REQUEST_IN_PROGRESS"})
			return
		}
		writer := &captureWriter{ResponseWriter: c.Writer}
		c.Writer = writer
		c.Next()
		envelope := replayEnvelope{Body: writer.body.String(), ContentType: c.Writer.Header().Get("Content-Type"), Headers: map[string]string{}}
		for _, name := range []string{"ETag", "Location", "Cache-Control"} {
			if value := c.Writer.Header().Get(name); value != "" {
				envelope.Headers[name] = value
			}
		}
		encoded, _ := json.Marshal(envelope)
		_ = store.DB.Model(&persistence.IdempotencyRecord{}).Where("id = ?", record.ID).Updates(map[string]any{"status_code": c.Writer.Status(), "response_json": string(encoded), "expires_at": persistence.Now().Add(24 * time.Hour)}).Error
	}
}

func mapError(err error) error {
	switch {
	case errors.Is(err, application.ErrNotFound) || persistence.IsNotFound(err):
		return huma.Error404NotFound("resource not found")
	case errors.Is(err, application.ErrRevision):
		return huma.NewError(http.StatusPreconditionFailed, "resource revision does not match")
	case errors.Is(err, application.ErrPrecondition):
		return huma.NewError(http.StatusPreconditionRequired, "If-Match is required")
	case errors.Is(err, application.ErrConflict), errors.Is(err, application.ErrActiveSession), errors.Is(err, application.ErrExternalImportExists), errors.Is(err, domain.ErrCoreLimit), errors.Is(err, domain.ErrSupportBeforeCoreDone):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, application.ErrValidation), errors.Is(err, domain.ErrCycle),
		errors.Is(err, domain.ErrCoord), errors.Is(err, domain.ErrLens), errors.Is(err, domain.ErrValidation):
		return huma.Error422UnprocessableEntity(err.Error())
	case errors.Is(err, application.ErrStaleAgentAttempt):
		return huma.Error409Conflict("stale agent attempt")
	case errors.Is(err, application.ErrForbidden):
		return huma.Error403Forbidden("administrator permission required")
	default:
		return huma.Error500InternalServerError("internal server error")
	}
}

func revisionFromETag(value string) (int, error) {
	if strings.TrimSpace(value) == "" {
		return 0, application.ErrPrecondition
	}
	parts := strings.Split(strings.Trim(value, "\""), "_rev_")
	if len(parts) != 2 {
		return 0, application.ErrRevision
	}
	revision, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, application.ErrRevision
	}
	return revision, nil
}
func revisionFromResourceETag(value, kind, id string) (int, error) {
	revision, err := revisionFromETag(value)
	if err != nil || value != application.StrongETag(kind, id, revision) {
		return 0, application.ErrRevision
	}
	return revision, nil
}
func encodeImportCursor(createdAt time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(createdAt.UTC().Format(time.RFC3339Nano) + "\n" + id))
}
func decodeImportCursor(value string, createdAt *time.Time, id *string) error {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return err
	}
	parts := strings.SplitN(string(decoded), "\n", 2)
	if len(parts) != 2 || parts[1] == "" {
		return errors.New("invalid cursor")
	}
	parsed, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return err
	}
	*createdAt, *id = parsed, parts[1]
	return nil
}
func matchETag(value, kind, id string, revision int) bool {
	return value == application.StrongETag(kind, id, revision)
}
func toInt(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		i, _ := v.Int64()
		return int(i)
	default:
		i, _ := strconv.Atoi(fmt.Sprint(v))
		return i
	}
}

func schemaNamer(t reflect.Type, hint string) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	base := huma.DefaultSchemaNamer(t, hint)
	if t.Name() != "" {
		return base
	}
	hash := sha256.Sum256([]byte(t.String()))
	return fmt.Sprintf("%s_%x", base, hash[:4])
}
