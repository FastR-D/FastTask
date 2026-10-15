package integrationclient

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// These protocols were checked against FastNews 8fe40ab, FastPPT d96c2e1
// (skill branch), and FastLabs 72f8fa2. They deliberately expose no mutations.
func extraHealthProfile(profile string) (string, bool) {
	switch profile {
	case "fastnews-local":
		return "auth/info", true
	case "fastppt-studio":
		return "/healthz", true
	case "fastlabs":
		return "health", true
	default:
		return "", false
	}
}

func extraHealthValid(profile string, data []byte) bool {
	switch profile {
	case "fastnews-local":
		var health struct {
			Local        *bool   `json:"local"`
			Registration *string `json:"registration"`
			FastCAS      *bool   `json:"fastcas"`
		}
		return json.Unmarshal(data, &health) == nil && health.Local != nil && *health.Local && health.Registration != nil && *health.Registration == "invite" && health.FastCAS != nil
	case "fastppt-studio":
		var health struct {
			Status  string `json:"status"`
			Service string `json:"service"`
			Project string `json:"project"`
		}
		return json.Unmarshal(data, &health) == nil && health.Status == "ok" && health.Service == "project-studio-ts" && safeID(health.Project)
	case "fastlabs":
		var health struct {
			OK            *bool             `json:"ok"`
			SetupRequired *bool             `json:"setupRequired"`
			Python        *string           `json:"python"`
			Executors     []json.RawMessage `json:"executors"`
		}
		return json.Unmarshal(data, &health) == nil && health.OK != nil && *health.OK && health.SetupRequired != nil && health.Python != nil && *health.Python != "" && health.Executors != nil
	}
	return false
}

func extraBoundary(p *prepared) *Diagnostic {
	switch p.profile {
	case "fastppt-studio", "fastlabs":
		if !p.config.PersonalInstance || !loopback(p.base.Hostname()) || p.config.AuthKind != "" || p.config.Credential != "" {
			return diagnostic("BOUNDARY_UNSUPPORTED", "此协议仅支持本机个人实例", "该工具没有此适配器可验证的多用户读取授权；填写令牌不能将本机接口变成远程授权接口。", "请明确确认本机个人实例并使用回环地址和匿名连接；远程部署需先提供独立授权协议。", 0, false)
		}
	case "fastnews-local":
		if p.config.AuthKind != "cookie" || p.cookie != "fastnews_session" || p.config.Credential == "" {
			return diagnostic("AUTH_REQUIRED", "需要 FastNews 本地账号授权", "独立账号协议只接受 FastNews 签发的 fastnews_session，会拒绝 Panel 的 fr_session 或其他系统令牌。", "请登录对应 FastNews 实例并配置其本地会话；Panel 内容请选择 fastnews-panel。", 0, false)
		}
	default:
		return protocolMismatch(0)
	}
	return nil
}

func (c Client) listExtra(ctx context.Context, p *prepared, query string, limit, offset int) (Page, *Diagnostic) {
	empty := Page{Items: []Resource{}, Limit: limit, Offset: offset}
	if problem := extraBoundary(p); problem != nil {
		return empty, problem
	}
	switch p.profile {
	case "fastnews-local":
		return c.listNewsProfile(ctx, p, query, limit, offset)
	case "fastppt-studio":
		data, problem := c.get(ctx, p, "projects", nil, false)
		if problem != nil {
			return empty, problem
		}
		var result struct {
			Projects []pptProject `json:"projects"`
		}
		if json.Unmarshal(data, &result) != nil || result.Projects == nil {
			return empty, protocolMismatch(http.StatusOK)
		}
		resources := make([]Resource, 0, len(result.Projects))
		seen := map[string]bool{}
		for _, project := range result.Projects {
			if !project.valid() || seen[project.ID] {
				return empty, protocolMismatch(http.StatusOK)
			}
			seen[project.ID] = true
			summary, _ := truncateText(project.Note)
			// /api/project is instance-bound, not a detail URL for every list row.
			// A link to it here could open a different presentation.
			resources = append(resources, Resource{ID: project.ID, Title: *project.Name, Kind: "presentation", Summary: summary})
		}
		return localPage(resources, query, limit, offset), nil
	case "fastlabs":
		data, problem := c.get(ctx, p, "tasks", nil, false)
		if problem != nil {
			return empty, problem
		}
		var result struct {
			Tasks []labTask `json:"tasks"`
		}
		if json.Unmarshal(data, &result) != nil || result.Tasks == nil {
			return empty, protocolMismatch(http.StatusOK)
		}
		resources := make([]Resource, 0, len(result.Tasks))
		seen := map[string]bool{}
		for _, task := range result.Tasks {
			if !task.valid() || seen[task.ID] {
				return empty, protocolMismatch(http.StatusOK)
			}
			seen[task.ID] = true
			resources = append(resources, task.resource(p))
		}
		return localPage(resources, query, limit, offset), nil
	}
	return empty, protocolMismatch(0)
}

func (c Client) readExtra(ctx context.Context, p *prepared, id, filePath string) (Content, *Diagnostic) {
	empty := Content{Children: []Resource{}}
	if problem := extraBoundary(p); problem != nil {
		return empty, problem
	}
	if !safeID(id) || filePath != "" {
		return empty, diagnostic("INPUT_INVALID", "资源标识无效", "这些只读协议不接受本地文件路径或任意资源路径。", "请从材料列表选择资源。", 0, false)
	}
	switch p.profile {
	case "fastnews-local":
		return c.readNewsProfile(ctx, p, id)
	case "fastppt-studio":
		return c.readPPT(ctx, p, id)
	case "fastlabs":
		if !labID(id) {
			return empty, diagnostic("INPUT_INVALID", "实验任务标识无效", "FastLabs 任务接口只接受小写十六进制标识。", "请从实验任务列表选择资源。", 0, false)
		}
		data, problem := c.get(ctx, p, "tasks/"+id, nil, false)
		if problem != nil {
			return empty, problem
		}
		var task labTask
		if json.Unmarshal(data, &task) != nil || !task.valid() || task.ID != id || task.Documents == nil {
			return empty, protocolMismatch(http.StatusOK)
		}
		text := *task.Title + "\n\n目标：" + *task.Goal + "\n状态：" + *task.Status
		for _, name := range []string{"task.md", "progress.md", "acceptance.md"} {
			value, exists := task.Documents[name]
			if !exists {
				return empty, protocolMismatch(http.StatusOK)
			}
			if strings.TrimSpace(value) != "" {
				text += "\n\n## " + name + "\n\n" + value
			}
		}
		empty.Resource = task.resource(p)
		empty.Text, empty.Truncated = truncateText(text)
		empty.Version = task.UpdatedAt
		return empty, nil
	}
	return empty, protocolMismatch(0)
}

type pptProject struct {
	SchemaVersion *int    `json:"schemaVersion"`
	ID            string  `json:"projectId"`
	Name          *string `json:"name"`
	Note          string  `json:"note"`
}

func (project pptProject) valid() bool {
	return project.SchemaVersion != nil && *project.SchemaVersion == 1 && safeID(project.ID) && project.Name != nil && strings.TrimSpace(*project.Name) != ""
}

func (c Client) readPPT(ctx context.Context, p *prepared, id string) (Content, *Diagnostic) {
	content := Content{Children: []Resource{}}
	data, problem := c.get(ctx, p, "project", nil, false)
	if problem != nil {
		return content, problem
	}
	var project struct {
		ID           string  `json:"projectId"`
		Name         *string `json:"projectName"`
		Note         *string `json:"projectNote"`
		Stage        *string `json:"stage"`
		DeckStatus   *string `json:"deckStatus"`
		DeckRevision string  `json:"deckRevision"`
	}
	if json.Unmarshal(data, &project) != nil || !safeID(project.ID) || project.Name == nil || strings.TrimSpace(*project.Name) == "" || project.Stage == nil || project.DeckStatus == nil {
		return content, protocolMismatch(http.StatusOK)
	}
	if project.ID != id {
		return content, diagnostic("RESOURCE_NOT_ACTIVE", "该演示项目尚未在此实例打开", "FastPPT Studio 的内容接口绑定当前活动项目，不能把其他项目的内容冒充为所选项目。", "请先在 FastPPT 打开该项目，再刷新连接；FastTask 不会代为切换项目。", 409, false)
	}
	summary := ""
	if project.Note != nil {
		summary, _ = truncateText(*project.Note)
	}
	content.Resource = Resource{ID: id, Title: *project.Name, Kind: "presentation", URL: p.webEndpoint("/", nil), Summary: summary}
	content.Version = project.DeckRevision
	text := *project.Name + "\n\n" + summary + "\n阶段：" + *project.Stage + "\n演示状态：" + *project.DeckStatus
	data, problem = c.get(ctx, p, "workflow/plan", nil, false)
	if problem != nil {
		if problem.HTTPStatus != http.StatusNotFound {
			return Content{Children: []Resource{}}, problem
		}
		text += "\n\n尚未生成页面计划；当前只读取到了项目状态。"
	} else {
		var plan struct {
			SchemaVersion *int    `json:"schemaVersion"`
			Title         *string `json:"title"`
			Pages         []struct {
				ID    string  `json:"id"`
				Title *string `json:"title"`
				Role  *string `json:"role"`
			} `json:"pages"`
		}
		if json.Unmarshal(data, &plan) != nil || plan.SchemaVersion == nil || *plan.SchemaVersion != 1 || plan.Title == nil || strings.TrimSpace(*plan.Title) == "" || len(plan.Pages) == 0 {
			return Content{Children: []Resource{}}, protocolMismatch(http.StatusOK)
		}
		text += "\n\n页面计划：" + *plan.Title
		seen := map[string]bool{}
		for _, page := range plan.Pages {
			if !safeID(page.ID) || page.Title == nil || page.Role == nil || strings.TrimSpace(*page.Title) == "" || strings.TrimSpace(*page.Role) == "" || seen[page.ID] {
				return Content{Children: []Resource{}}, protocolMismatch(http.StatusOK)
			}
			seen[page.ID] = true
			text += "\n- " + *page.Title + "（" + *page.Role + "）"
		}
	}
	content.Text, content.Truncated = truncateText(strings.TrimSpace(text))
	return content, nil
}

type labTask struct {
	ID        string            `json:"id"`
	Title     *string           `json:"title"`
	Goal      *string           `json:"goal"`
	Status    *string           `json:"status"`
	UpdatedAt string            `json:"updated_at"`
	Documents map[string]string `json:"documents"`
}

func labID(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func (task labTask) valid() bool {
	return labID(task.ID) && task.Title != nil && strings.TrimSpace(*task.Title) != "" && task.Goal != nil && strings.TrimSpace(*task.Goal) != "" && task.Status != nil && strings.TrimSpace(*task.Status) != ""
}

func (task labTask) resource(p *prepared) Resource {
	summary, _ := truncateText(*task.Goal)
	// This version of FastLabs has no task deep-link contract. Keep the link
	// absent instead of opening raw JSON or suggesting a different task is active.
	return Resource{ID: task.ID, Title: *task.Title, Kind: "experiment", Summary: summary}
}
