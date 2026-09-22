package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/domain"
	"github.com/FastR-D/FastTask/internal/persistence"
	"github.com/danielgtaylor/huma/v2"
)

type lensAxis struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Min       int    `json:"min"`
	Max       int    `json:"max"`
	LowLabel  string `json:"low_label"`
	HighLabel string `json:"high_label"`
}

type lensQuadrant struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Advice string `json:"advice"`
}

type lensDefinition struct {
	ID        string         `json:"id"`
	Title     string         `json:"title"`
	Threshold int            `json:"threshold"`
	X         lensAxis       `json:"x"`
	Y         lensAxis       `json:"y"`
	Quadrants []lensQuadrant `json:"quadrants"`
}

type lensListBody struct {
	Items []lensDefinition `json:"items"`
}

func presetLenses() []lensDefinition {
	return []lensDefinition{{
		ID:        domain.LensResearchRisk,
		Title:     "科研风险透镜",
		Threshold: domain.CoordThreshold,
		X:         lensAxis{Key: "uncertainty", Label: "不确定性", Min: domain.CoordMin, Max: domain.CoordMax, LowLabel: "知道怎么做", HighLabel: "方法未知"},
		Y:         lensAxis{Key: "contribution", Label: "贡献度", Min: domain.CoordMin, Max: domain.CoordMax, LowLabel: "间接", HighLabel: "直接决定验收"},
		Quadrants: []lensQuadrant{
			{Key: domain.QuadrantCriticalRisk, Label: domain.QuadrantLabel(domain.QuadrantCriticalRisk), Advice: "尽早验证，拖延成本最高"},
			{Key: domain.QuadrantMainProgress, Label: domain.QuadrantLabel(domain.QuadrantMainProgress), Advice: "稳定产出"},
			{Key: domain.QuadrantTimeHole, Label: domain.QuadrantLabel(domain.QuadrantTimeHole), Advice: "降级、拆小或砍掉"},
			{Key: domain.QuadrantDrain, Label: domain.QuadrantLabel(domain.QuadrantDrain), Advice: "必要，但不应占主要时间"},
		},
	}}
}

func (s lensRoutes) RegisterRoutes(api huma.API) {
	type emptyInput struct{}
	register(api, "list-lenses", http.MethodGet, "/lenses", "List preset decision lenses", userSecurity(), func(ctx context.Context, input *emptyInput) (*itemResponse[lensListBody], error) {
		return &itemResponse[lensListBody]{Body: lensListBody{Items: presetLenses()}}, nil
	})

	type putCoordInput struct {
		TaskID  string `path:"task_id"`
		IfMatch string `header:"If-Match"`
		Body    struct {
			Lens      string `json:"lens,omitempty"`
			X         int    `json:"x" minimum:"0" maximum:"100"`
			Y         int    `json:"y" minimum:"0" maximum:"100"`
			Rationale string `json:"rationale,omitempty" maxLength:"120"`
		}
	}
	register(api, "put-task-coord", http.MethodPut, "/tasks/{task_id}/coords", "Set task coordinates", userSecurity(), func(ctx context.Context, input *putCoordInput) (*resourceResponse[persistence.TaskCoord], error) {
		expected := -1
		if strings.TrimSpace(input.IfMatch) != "" {
			revision, err := revisionFromETag(input.IfMatch)
			if err != nil {
				return nil, mapError(err)
			}
			expected = revision
		}
		coord, err := s.app.SetTaskCoord(ctx, principal(ctx).UserID, input.TaskID, input.Body.Lens, input.Body.X, input.Body.Y, input.Body.Rationale, expected)
		if err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.TaskCoord]{ETag: application.StrongETag("coord", coord.ID, coord.Revision), Body: *coord}, nil
	})

	type reviewInput struct {
		Week     string `query:"week"`
		Timezone string `query:"timezone"`
	}
	register(api, "get-weekly-review", http.MethodGet, "/reviews/weekly", "Get weekly review", userSecurity(), func(ctx context.Context, input *reviewInput) (*itemResponse[application.WeeklyReview], error) {
		review, err := s.app.WeeklyReview(ctx, principal(ctx).UserID, input.Week, input.Timezone)
		if err != nil {
			return nil, mapError(err)
		}
		return &itemResponse[application.WeeklyReview]{Body: *review}, nil
	})

	type mapInput struct {
		GoalID string `path:"goal_id"`
		Lens   string `query:"lens"`
	}
	register(api, "get-goal-map", http.MethodGet, "/goals/{goal_id}/map", "Get goal decision map", userSecurity(), func(ctx context.Context, input *mapInput) (*itemResponse[application.GoalMap], error) {
		result, err := s.app.GoalMap(ctx, principal(ctx).UserID, input.GoalID, input.Lens)
		if err != nil {
			return nil, mapError(err)
		}
		return &itemResponse[application.GoalMap]{Body: *result}, nil
	})
}
