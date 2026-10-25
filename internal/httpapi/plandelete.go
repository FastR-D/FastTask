package httpapi

import (
	"context"
	"net/http"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/danielgtaylor/huma/v2"
)

func (s planRoutes) registerPlanItemDeletions(api huma.API) {
	type deleteInput struct {
		PlanID         string `path:"plan_id"`
		IfMatch        string `header:"If-Match"`
		IdempotencyKey string `header:"Idempotency-Key" required:"true" minLength:"1" maxLength:"200"`
		Body           struct {
			Items []struct {
				ID      string `json:"id" minLength:"1" maxLength:"200"`
				IfMatch string `json:"if_match" minLength:"1" maxLength:"300"`
			} `json:"items" minItems:"1" maxItems:"3"`
		}
	}
	register(api, "delete-daily-plan-items", http.MethodPost, "/daily-plans/{plan_id}/item-deletions", "Delete selected commitments from today's plan, preserving tasks and evidence", userSecurity(), func(ctx context.Context, input *deleteInput) (*resourceResponse[application.PlanItemDeletionResult], error) {
		revision, err := revisionFromResourceETag(input.IfMatch, "plan", input.PlanID)
		if err != nil {
			return nil, mapError(err)
		}
		selected := make([]application.PlanItemDeletion, 0, len(input.Body.Items))
		for _, item := range input.Body.Items {
			itemRevision, err := revisionFromResourceETag(item.IfMatch, "dpi", item.ID)
			if err != nil {
				return nil, mapError(err)
			}
			selected = append(selected, application.PlanItemDeletion{ID: item.ID, Revision: itemRevision})
		}
		result, err := s.app.DeletePlanItems(ctx, principal(ctx).UserID, input.PlanID, revision, selected)
		if err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[application.PlanItemDeletionResult]{ETag: application.StrongETag("plan", result.Plan.ID, result.Plan.Revision), Body: *result}, nil
	})
}
