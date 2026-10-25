package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/panelweather"
	"github.com/FastR-D/FastTask/internal/persistence"
	"github.com/danielgtaylor/huma/v2"
)

type panelProfileResponse struct {
	ETag         string `header:"ETag"`
	CacheControl string `header:"Cache-Control"`
	Body         persistence.PanelProfile
}

func (s panelRoutes) registerPanelProfile(api huma.API) {
	service := application.NewPanelService(s.app.Store)
	type getInput struct{}
	register(api, "get-panel-profile", http.MethodGet, "/panel/profile", "Read your personal ink-screen template", userSecurity(), func(ctx context.Context, _ *getInput) (*panelProfileResponse, error) {
		profile, err := service.Profile(ctx, principal(ctx).UserID)
		if err != nil {
			return nil, mapError(err)
		}
		return &panelProfileResponse{ETag: application.StrongETag("panel-profile", profile.UserID, profile.Revision), CacheControl: "private, no-store", Body: *profile}, nil
	})
	type putInput struct {
		IfMatch string `header:"If-Match"`
		Body    application.PanelProfileInput
	}
	register(api, "put-panel-profile", http.MethodPut, "/panel/profile", "Save your avatar and personal ink-screen template", userSecurity(), func(ctx context.Context, input *putInput) (*panelProfileResponse, error) {
		userID := principal(ctx).UserID
		revision, err := revisionFromResourceETag(input.IfMatch, "panel-profile", userID)
		if err != nil {
			return nil, mapError(err)
		}
		profile, err := service.PutProfile(ctx, userID, revision, input.Body)
		if err != nil {
			return nil, mapError(err)
		}
		return &panelProfileResponse{ETag: application.StrongETag("panel-profile", profile.UserID, profile.Revision), CacheControl: "private, no-store", Body: *profile}, nil
	})
}

func (s panelRoutes) registerPanelPairings(api huma.API) {
	service := application.NewPanelService(s.app.Store)
	type listInput struct{}
	type listBody struct {
		Items []persistence.PanelPairing `json:"items"`
	}
	register(api, "list-panel-pairings", http.MethodGet, "/panel/pairings", "List your Windows ink-screen synchronizations", userSecurity(), func(ctx context.Context, _ *listInput) (*itemResponse[listBody], error) {
		items, err := service.Pairings(ctx, principal(ctx).UserID)
		if err != nil {
			return nil, mapError(err)
		}
		return &itemResponse[listBody]{Body: listBody{Items: items}}, nil
	})
	type createInput struct {
		Body struct {
			DeviceName string `json:"device_name" minLength:"1" maxLength:"80"`
		}
	}
	type createBody struct {
		persistence.PanelPairing
		Token string `json:"token"`
	}
	type createResponse struct {
		CacheControl string `header:"Cache-Control"`
		Body         createBody
	}
	register(api, "create-panel-pairing", http.MethodPost, "/panel/pairings", "Authorize a Windows helper for read-only ink-screen synchronization", userSecurity(), func(ctx context.Context, input *createInput) (*createResponse, error) {
		pairing, token, err := service.CreatePairing(ctx, principal(ctx).UserID, input.Body.DeviceName)
		if err != nil {
			return nil, mapError(err)
		}
		return &createResponse{CacheControl: "private, no-store", Body: createBody{PanelPairing: *pairing, Token: token}}, nil
	})
	type revokeInput struct {
		ID string `path:"pairing_id"`
	}
	type revokeBody struct {
		Revoked bool `json:"revoked"`
	}
	register(api, "revoke-panel-pairing", http.MethodDelete, "/panel/pairings/{pairing_id}", "Revoke a Windows helper without changing the account session", userSecurity(), func(ctx context.Context, input *revokeInput) (*itemResponse[revokeBody], error) {
		if err := service.RevokePairing(ctx, principal(ctx).UserID, input.ID); err != nil {
			return nil, mapError(err)
		}
		return &itemResponse[revokeBody]{Body: revokeBody{Revoked: true}}, nil
	})
}

type panelSchedule struct {
	Timezone     string   `json:"timezone"`
	RefreshTimes []string `json:"refresh_times"`
}

type panelSnapshotBody struct {
	Profile     persistence.PanelProfile `json:"profile"`
	Weather     panelweather.Snapshot    `json:"weather"`
	Tasks       panelTasksBody           `json:"tasks"`
	Schedule    panelSchedule            `json:"schedule"`
	GeneratedAt time.Time                `json:"generated_at"`
}

func (s panelRoutes) registerPanelSnapshot(api huma.API) {
	service := application.NewPanelService(s.app.Store)
	type input struct {
		Authorization string `header:"Authorization"`
		PanelToken    string `header:"X-Panel-Token"`
	}
	type response struct {
		CacheControl string `header:"Cache-Control"`
		Body         panelSnapshotBody
	}
	// A pairing capability is accepted here only. It cannot reach profile writes,
	// task mutations, user details, attachments, or the generic device API.
	register(api, "get-panel-snapshot", http.MethodGet, "/panel/snapshot", "Read your template and today's task state using a dedicated panel capability", publicSecurity(), func(ctx context.Context, input *input) (*response, error) {
		userID := ""
		if p, err := s.auth.Authenticate(input.Authorization); err == nil {
			userID = p.UserID
		} else if id, err := service.AuthenticatePairing(ctx, input.PanelToken); err == nil {
			userID = id
		}
		if userID == "" {
			return nil, huma.Error401Unauthorized("a user session or valid read-only X-Panel-Token is required")
		}
		profile, err := service.Profile(ctx, userID)
		if err != nil {
			return nil, mapError(err)
		}
		plan, items, err := s.currentPlan(ctx, userID)
		if err != nil {
			return nil, mapError(err)
		}
		views, err := s.app.PlanItemViews(ctx, userID, items)
		if err != nil {
			return nil, mapError(err)
		}
		tasks := panelTasksBody{DailyPlanID: plan.ID, LocalDate: plan.LocalDate, Timezone: plan.Timezone, PlanStatus: plan.Status, PlanRevision: plan.CurrentRevision, PlanETag: application.StrongETag("plan", plan.ID, plan.Revision), TodayItems: views, UpdatedAt: plan.UpdatedAt, ReadOnly: true}
		body := panelSnapshotBody{Profile: *profile, Weather: panelweather.Default.Get(ctx), Tasks: tasks, Schedule: panelSchedule{Timezone: "Asia/Shanghai", RefreshTimes: []string{"14:00", "17:30", "21:00"}}, GeneratedAt: persistence.Now()}
		return &response{CacheControl: "private, no-store", Body: body}, nil
	})
}
