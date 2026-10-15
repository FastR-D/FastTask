package application

import (
	"context"
	"errors"
	"strings"

	"github.com/FastR-D/FastTask/internal/domain"
	"github.com/FastR-D/FastTask/internal/persistence"
)

// PlanItemWorkspace is navigation metadata only. It never carries a credential
// or claims that work has been executed in the target application.
type PlanItemWorkspace struct {
	System string `json:"system"`
	Label  string `json:"label"`
	URL    string `json:"url,omitempty"`
}

// PlanItemView keeps the legacy item fields and adds a write precondition and
// optional module entry. LogicalID remains stable across explicit plan revisions;
// ID and ItemETag identify the currently writable revision.
type PlanItemView struct {
	persistence.DailyPlanItem
	ItemETag            string             `json:"item_etag"`
	TaskETag            string             `json:"task_etag,omitempty"`
	TaskSuccessCriteria string             `json:"task_success_criteria,omitempty"`
	CanCompleteTask     bool               `json:"can_complete_task"`
	Workspace           *PlanItemWorkspace `json:"workspace,omitempty"`
}

var workspaceLabels = map[string]string{
	"fastwrite": "FastWrite", "fastread": "FastRead", "fastnews": "FastNews",
	"fastppt": "FastPPT", "fastlab": "FastLab",
}

func planItemWorkspaceSystem(item persistence.DailyPlanItem) string {
	fields := []string{item.Title, item.Commitment, item.MinimumAction}
	// An explicit module selection outranks inferred action words, including an
	// explicit selection in the output or first action rather than the title.
	for _, field := range fields {
		text := strings.ToLower(field)
		for _, system := range IntegrationSystems {
			if strings.Contains(text, system) {
				return system
			}
		}
	}
	// Prefer the task's action over context in its expected output. Unknown
	// research steps have no guessed link and can still be worked on normally.
	groups := []struct {
		system string
		words  []string
	}{
		{"fastppt", []string{"ppt", "幻灯片", "汇报演示", "答辩展示", "slide", "slides", "presentation"}},
		{"fastnews", []string{"新闻", "资讯", "领域动态", "研究动态", "最新动态", "news"}},
		{"fastread", []string{"阅读", "读论文", "读文献", "文献", "检索", "查阅", "literature", "read", "reading"}},
		{"fastwrite", []string{"写作", "撰写", "起草", "修订", "写论文", "写初稿", "写摘要", "写方法", "写结果", "draft", "write", "writing"}},
		{"fastlab", []string{"实验", "仿真", "模拟", "数据分析", "运行代码", "基线", "benchmark", "experiment"}},
	}
	for _, field := range fields {
		text := strings.ToLower(field)
		for _, group := range groups {
			for _, word := range group.words {
				if workspaceActionWord(text, word) {
					// Literature and research news can be the subject of a writing
					// task. A clear writing action outranks those topic words alone.
					if (group.system == "fastread" || group.system == "fastnews") && workspaceWritingAction(text) {
						return "fastwrite"
					}
					return group.system
				}
			}
		}
	}
	return ""
}

func workspaceWritingAction(text string) bool {
	// Do not introduce a new preference for a cross-tool sequence of actions.
	for _, word := range []string{"阅读", "读论文", "读文献", "检索", "查阅", "追踪", "跟踪", "浏览", "制作", "汇报", "答辩", "运行", "仿真", "模拟", "数据分析", "read", "reading", "track", "browse", "run", "simulate"} {
		if workspaceActionWord(text, word) {
			return false
		}
	}
	for _, word := range []string{"写作", "撰写", "起草", "修订", "写论文", "写初稿", "写摘要", "写方法", "写结果", "draft", "write", "writing"} {
		if workspaceActionWord(text, word) {
			return true
		}
	}
	return false
}

func workspaceActionWord(text, word string) bool {
	if word == "" || word[0] >= 128 {
		return strings.Contains(text, word)
	}
	wordByte := func(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_' }
	for start := 0; start < len(text); {
		index := strings.Index(text[start:], word)
		if index < 0 {
			return false
		}
		index += start
		end := index + len(word)
		if (index == 0 || !wordByte(text[index-1])) && (end == len(text) || !wordByte(text[end])) {
			return true
		}
		start = index + 1
	}
	return false
}

// PlanItemViews reuses the current user's configured module webpages. It reads
// addresses only: no target-system request, decryption or login takes place.
func (a *App) PlanItemViews(ctx context.Context, userID string, items []persistence.DailyPlanItem) ([]PlanItemView, error) {
	connections, err := a.IntegrationConnections(ctx, userID, false)
	if err != nil {
		return nil, err
	}
	addresses := map[string]string{}
	for _, connection := range connections {
		if connection.WebURL != "" && validateIntegrationURL(connection.WebURL) == nil {
			addresses[connection.System] = connection.WebURL
		}
	}
	views := make([]PlanItemView, 0, len(items))
	var user persistence.User
	if err := a.Store.DB.WithContext(ctx).Where("id = ? AND status = 'active'", userID).First(&user).Error; err != nil {
		return nil, notFound(err)
	}
	loc, _ := resolveZone(user.Timezone)
	today := persistence.Now().In(loc).Format("2006-01-02")
	plans := map[string]persistence.DailyPlan{}
	for _, item := range items {
		item.LogicalID = logicalPlanItemID(item)
		view := PlanItemView{DailyPlanItem: item, ItemETag: StrongETag("dpi", item.ID, item.Revision)}
		if item.TaskID != nil {
			plan, ok := plans[item.PlanID]
			if !ok {
				if err := a.Store.DB.WithContext(ctx).Where("id = ? AND user_id = ?", item.PlanID, userID).First(&plan).Error; err != nil {
					return nil, notFound(err)
				}
				plans[item.PlanID] = plan
			}
			task, err := RequireLiveTaskTx(a.Store.DB.WithContext(ctx), userID, *item.TaskID)
			if err != nil && !errors.Is(err, ErrNotFound) && !persistence.IsNotFound(err) {
				return nil, err
			}
			if err == nil {
				view.TaskETag = StrongETag("task", task.ID, task.Revision)
				view.TaskSuccessCriteria = task.SuccessCriteria
				if plan.Status == "active" && plan.LocalDate <= today && plan.CurrentRevision == item.PlanRevision && (item.Status == "planned" || item.Status == "in_progress") && domain.ValidateCompletionType("result", item.AllowedTypes) {
					view.CanCompleteTask, err = completeTaskFromDailyEligibleTx(a.Store.DB.WithContext(ctx), userID, *task)
					if err != nil {
						return nil, err
					}
				}
			}
		}
		if system := planItemWorkspaceSystem(item); system != "" {
			view.Workspace = &PlanItemWorkspace{System: system, Label: workspaceLabels[system], URL: addresses[system]}
		}
		views = append(views, view)
	}
	return views, nil
}
