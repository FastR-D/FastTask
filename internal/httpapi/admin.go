package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/persistence"
)

func (s *Server) registerAdmin() {
	s.registerAdminUsers()
	s.registerAdminModelProviders()
	s.registerAdminAudit()
}

func (s *Server) registerAdminUsers() {
	type listInput struct {
		Query  string `query:"q"`
		Status string `query:"status"`
	}
	register(s.API, "list-admin-users", http.MethodGet, "/admin/users", "List users", userSecurity(), func(ctx context.Context, input *listInput) (*listResponse[persistence.User], error) {
		if _, err := s.app.CurrentAdmin(ctx); err != nil {
			return nil, mapError(err)
		}
		users, err := s.app.ListUsers(ctx, input.Query, input.Status)
		if err != nil {
			return nil, mapError(err)
		}
		out := &listResponse[persistence.User]{}
		out.Body.Items = users
		return out, nil
	})

	type createInput struct {
		Body struct {
			Identifier  string `json:"identifier" minLength:"3" maxLength:"120"`
			Password    string `json:"password" minLength:"12" maxLength:"200"`
			DisplayName string `json:"display_name" minLength:"1" maxLength:"120"`
			Timezone    string `json:"timezone,omitempty"`
			Locale      string `json:"locale,omitempty"`
			Role        string `json:"role" enum:"member,admin"`
		}
	}
	register(s.API, "create-admin-user", http.MethodPost, "/admin/users", "Create user", userSecurity(), func(ctx context.Context, input *createInput) (*itemResponse[persistence.User], error) {
		actor, err := s.app.CurrentAdmin(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		user, err := s.app.CreateUser(ctx, actor, application.CreateUserCommand{Identifier: input.Body.Identifier, Password: input.Body.Password, DisplayName: input.Body.DisplayName, Timezone: input.Body.Timezone, Locale: input.Body.Locale, Role: input.Body.Role})
		if err != nil {
			return nil, mapError(err)
		}
		return &itemResponse[persistence.User]{Body: *user}, nil
	})

	type itemInput struct {
		UserID string `path:"user_id"`
	}
	register(s.API, "get-admin-user", http.MethodGet, "/admin/users/{user_id}", "Get user", userSecurity(), func(ctx context.Context, input *itemInput) (*resourceResponse[persistence.User], error) {
		if _, err := s.app.CurrentAdmin(ctx); err != nil {
			return nil, mapError(err)
		}
		var user persistence.User
		if err := s.app.Store.DB.WithContext(ctx).First(&user, "id = ?", input.UserID).Error; err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.User]{ETag: application.StrongETag("user", user.ID, user.Revision), Body: user}, nil
	})

	type updateInput struct {
		UserID  string `path:"user_id"`
		IfMatch string `header:"If-Match" required:"true"`
		Body    struct {
			DisplayName *string `json:"display_name,omitempty"`
			Timezone    *string `json:"timezone,omitempty"`
			Locale      *string `json:"locale,omitempty"`
			Role        *string `json:"role,omitempty" enum:"member,admin"`
			Status      *string `json:"status,omitempty" enum:"active,disabled"`
		}
	}
	register(s.API, "update-admin-user", http.MethodPatch, "/admin/users/{user_id}", "Update user", userSecurity(), func(ctx context.Context, input *updateInput) (*resourceResponse[persistence.User], error) {
		actor, err := s.app.CurrentAdmin(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		expected, err := revisionFromETag(input.IfMatch)
		if err != nil {
			return nil, mapError(err)
		}
		user, err := s.app.UpdateUser(ctx, actor, input.UserID, expected, application.UpdateUserCommand{DisplayName: input.Body.DisplayName, Timezone: input.Body.Timezone, Locale: input.Body.Locale, Role: input.Body.Role, Status: input.Body.Status})
		if err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.User]{ETag: application.StrongETag("user", user.ID, user.Revision), Body: *user}, nil
	})

	type passwordInput struct {
		UserID string `path:"user_id"`
		Body   struct {
			Password string `json:"password" minLength:"12" maxLength:"200"`
		}
	}
	register(s.API, "reset-admin-user-password", http.MethodPost, "/admin/users/{user_id}/password", "Reset user password", userSecurity(), func(ctx context.Context, input *passwordInput) (*struct{}, error) {
		actor, err := s.app.CurrentAdmin(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		if err := s.app.ResetUserPassword(ctx, actor, input.UserID, input.Body.Password); err != nil {
			return nil, mapError(err)
		}
		return &struct{}{}, nil
	})

	type sessionInput struct {
		UserID     string `path:"user_id"`
		ActiveOnly string `query:"active_only"`
	}
	register(s.API, "list-admin-user-sessions", http.MethodGet, "/admin/users/{user_id}/sessions", "List user sessions", userSecurity(), func(ctx context.Context, input *sessionInput) (*listResponse[application.SessionView], error) {
		if _, err := s.app.CurrentAdmin(ctx); err != nil {
			return nil, mapError(err)
		}
		sessions, err := s.app.ListUserSessions(ctx, input.UserID, input.ActiveOnly == "true")
		if err != nil {
			return nil, mapError(err)
		}
		out := &listResponse[application.SessionView]{}
		out.Body.Items = sessions
		return out, nil
	})

	register(s.API, "revoke-admin-user-sessions", http.MethodPost, "/admin/users/{user_id}/session-revocation", "Revoke user sessions", userSecurity(), func(ctx context.Context, input *itemInput) (*struct{}, error) {
		actor, err := s.app.CurrentAdmin(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		if err := s.app.RevokeUserSessions(ctx, actor, input.UserID); err != nil {
			return nil, mapError(err)
		}
		return &struct{}{}, nil
	})
}

func (s *Server) registerAdminModelProviders() {
	register(s.API, "list-admin-model-providers", http.MethodGet, "/admin/model-providers", "List model providers", userSecurity(), func(ctx context.Context, input *struct{}) (*listResponse[persistence.ModelProvider], error) {
		if _, err := s.app.CurrentAdmin(ctx); err != nil {
			return nil, mapError(err)
		}
		providers, err := s.app.ListModelProviders(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		out := &listResponse[persistence.ModelProvider]{}
		out.Body.Items = providers
		return out, nil
	})

	type createInput struct {
		Body struct {
			Name               string `json:"name" minLength:"1" maxLength:"120"`
			BaseURL            string `json:"base_url" minLength:"1" maxLength:"500"`
			ModelName          string `json:"model_name" minLength:"1" maxLength:"160"`
			TranscriptionModel string `json:"transcription_model,omitempty" maxLength:"160"`
			APIKey             string `json:"api_key" minLength:"12" maxLength:"2000"`
			Status             string `json:"status,omitempty" enum:"active,disabled"`
			IsDefault          bool   `json:"is_default" required:"false"`
		}
	}
	register(s.API, "create-model-provider", http.MethodPost, "/admin/model-providers", "Create model provider", userSecurity(), func(ctx context.Context, input *createInput) (*itemResponse[persistence.ModelProvider], error) {
		actor, err := s.app.CurrentAdmin(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		provider, err := s.app.CreateModelProvider(ctx, actor, application.ModelProviderCommand{Name: input.Body.Name, BaseURL: input.Body.BaseURL, ModelName: input.Body.ModelName, TranscriptionModel: input.Body.TranscriptionModel, APIKey: input.Body.APIKey, Status: input.Body.Status, IsDefault: input.Body.IsDefault})
		if err != nil {
			return nil, mapError(err)
		}
		return &itemResponse[persistence.ModelProvider]{Body: *provider}, nil
	})

	type updateInput struct {
		ID      string `path:"provider_id"`
		IfMatch string `header:"If-Match" required:"true"`
		Body    struct {
			Name               *string `json:"name,omitempty" minLength:"1" maxLength:"120"`
			BaseURL            *string `json:"base_url,omitempty" minLength:"1" maxLength:"500"`
			ModelName          *string `json:"model_name,omitempty" minLength:"1" maxLength:"160"`
			TranscriptionModel *string `json:"transcription_model,omitempty" maxLength:"160"`
			APIKey             *string `json:"api_key,omitempty" minLength:"12" maxLength:"2000"`
			Status             *string `json:"status,omitempty" enum:"active,disabled"`
			IsDefault          *bool   `json:"is_default,omitempty"`
		}
	}
	register(s.API, "update-model-provider", http.MethodPatch, "/admin/model-providers/{provider_id}", "Update model provider", userSecurity(), func(ctx context.Context, input *updateInput) (*resourceResponse[persistence.ModelProvider], error) {
		actor, err := s.app.CurrentAdmin(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		expected, err := revisionFromETag(input.IfMatch)
		if err != nil {
			return nil, mapError(err)
		}
		provider, err := s.app.UpdateModelProvider(ctx, actor, input.ID, expected, application.UpdateModelProviderCommand{Name: input.Body.Name, BaseURL: input.Body.BaseURL, ModelName: input.Body.ModelName, TranscriptionModel: input.Body.TranscriptionModel, APIKey: input.Body.APIKey, Status: input.Body.Status, IsDefault: input.Body.IsDefault})
		if err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.ModelProvider]{ETag: application.StrongETag("provider", provider.ID, provider.Revision), Body: *provider}, nil
	})

	type activateInput struct {
		ID string `path:"provider_id"`
	}
	register(s.API, "activate-model-provider", http.MethodPost, "/admin/model-providers/{provider_id}/activation", "Activate model provider", userSecurity(), func(ctx context.Context, input *activateInput) (*resourceResponse[persistence.ModelProvider], error) {
		actor, err := s.app.CurrentAdmin(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		provider, err := s.app.ActivateModelProvider(ctx, actor, input.ID)
		if err != nil {
			return nil, mapError(err)
		}
		return &resourceResponse[persistence.ModelProvider]{ETag: application.StrongETag("provider", provider.ID, provider.Revision), Body: *provider}, nil
	})

	register(s.API, "delete-model-provider", http.MethodDelete, "/admin/model-providers/{provider_id}", "Delete model provider", userSecurity(), func(ctx context.Context, input *activateInput) (*struct{}, error) {
		actor, err := s.app.CurrentAdmin(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		if err := s.app.DeleteModelProvider(ctx, actor, input.ID); err != nil {
			return nil, mapError(err)
		}
		return &struct{}{}, nil
	})

	type verifyInput struct {
		ID string `path:"provider_id"`
	}
	type verifyBody struct {
		Verified bool   `json:"verified"`
		Provider string `json:"provider"`
		Detail   string `json:"detail,omitempty"`
	}
	register(s.API, "verify-model-provider", http.MethodPost, "/admin/model-providers/{provider_id}/verification", "Verify model provider credentials", userSecurity(), func(ctx context.Context, input *verifyInput) (*itemResponse[verifyBody], error) {
		actor, err := s.app.CurrentAdmin(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		name, err := s.app.VerifyModelProvider(ctx, actor, input.ID)
		if err != nil {
			if errors.Is(err, application.ErrNotFound) || persistence.IsNotFound(err) {
				return nil, mapError(err)
			}
			return &itemResponse[verifyBody]{Body: verifyBody{Verified: false, Provider: name, Detail: "provider rejected the credentials or endpoint"}}, nil
		}
		return &itemResponse[verifyBody]{Body: verifyBody{Verified: true, Provider: name}}, nil
	})
}

func (s *Server) registerAdminAudit() {
	type listInput struct {
		Action string `query:"action"`
		Limit  int    `query:"limit"`
	}
	register(s.API, "list-admin-audit-events", http.MethodGet, "/admin/audit-events", "List administrative audit events", userSecurity(), func(ctx context.Context, input *listInput) (*listResponse[application.AuditView], error) {
		if _, err := s.app.CurrentAdmin(ctx); err != nil {
			return nil, mapError(err)
		}
		events, err := s.app.ListAuditEvents(ctx, input.Action, input.Limit)
		if err != nil {
			return nil, mapError(err)
		}
		out := &listResponse[application.AuditView]{}
		out.Body.Items = events
		return out, nil
	})
}
