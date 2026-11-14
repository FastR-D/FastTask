package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/FastR-D/FastTask/internal/persistence"
	platformauth "github.com/FastR-D/FastTask/internal/platform/auth"
	"github.com/gin-gonic/gin"
)

// This is the same service-to-service summary protocol used by the other
// FastRD applications. No Research account mapping or browser token is accepted.
func (s *Server) registerWorkspaceSummary() {
	s.Engine.POST("/api/integrations/fastresearch/workspace", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16*1024)
		parts := strings.Fields(c.GetHeader("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || c.GetHeader("X-API-Key") != "" {
			c.AbortWithStatusJSON(401, gin.H{"error": "workspace service authorization required"})
			return
		}
		if err := s.auth.AuthorizeWorkspaceService(c.Request.Context(), parts[1]); err != nil {
			status := 401
			if errors.Is(err, platformauth.ErrFastCASServiceDisabled) || errors.Is(err, platformauth.ErrFastCASDisabled) {
				status = 503
			}
			c.AbortWithStatusJSON(status, gin.H{"error": "workspace service unavailable or not authorized"})
			return
		}
		var body struct {
			Subject string `json:"subject"`
		}
		if err := c.ShouldBindJSON(&body); err != nil || body.Subject == "" || len(body.Subject) > 256 {
			c.AbortWithStatusJSON(400, gin.H{"error": "CAS subject required"})
			return
		}
		user, state, err := s.auth.WorkspaceAccount(c.Request.Context(), body.Subject)
		if err != nil {
			c.AbortWithStatusJSON(503, gin.H{"error": "workspace account verification unavailable"})
			return
		}
		response := gin.H{"schemaVersion": "fastresearch.workspace.v1", "appId": "fast-task", "accountState": state, "metrics": []any{}, "items": []any{}}
		if state != "active" {
			c.JSON(200, response)
			return
		}
		deps := RouteDeps{app: s.app, auth: s.auth, cfg: &s.cfg}
		today := deps.workspaceTasksFor(c.Request.Context(), user)
		if today.Error != "" {
			c.AbortWithStatusJSON(503, gin.H{"error": "workspace task summary unavailable"})
			return
		}
		items := []any{}
		for _, item := range today.Items {
			items = append(items, gin.H{"id": item.ID, "taskId": item.TaskID, "title": item.Title, "minimumAction": item.MinimumAction, "status": item.Status, "completionSummary": item.CompletionSummary, "targetMinutes": item.TargetMinutes})
		}
		progress := gin.H{"total": today.Progress.Total, "completed": today.Progress.Completed, "inProgress": today.Progress.InProgress, "planned": today.Progress.Planned, "skipped": today.Progress.Skipped}
		response["tasks"] = gin.H{"received": len(items) > 0, "date": today.LocalDate, "timezone": today.Timezone, "items": items, "progress": progress}
		response["metrics"] = []any{gin.H{"id": "total", "label": "今日任务", "value": today.Progress.Total}, gin.H{"id": "completed", "label": "已完成", "value": today.Progress.Completed}, gin.H{"id": "in-progress", "label": "进行中", "value": today.Progress.InProgress}}
		response["updatedAt"] = persistence.Now()
		c.JSON(http.StatusOK, response)
	})
}
