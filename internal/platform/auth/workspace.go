package auth

import (
	"context"
	"errors"
	"slices"
	"strings"

	fastcas "github.com/FastR-D/FastCAS/sdk/go"
	"github.com/FastR-D/FastTask/internal/persistence"
	"gorm.io/gorm"
)

// Workspace access is a separate read-only grant. Legacy task keys and user
// tokens cannot authorize this generic application-summary protocol.
func (s *Service) AuthorizeWorkspaceService(ctx context.Context, raw string) error {
	claims, err := s.VerifyFastCASServiceToken(ctx, raw, "workspace:read")
	if err != nil {
		return err
	}
	client, err := s.FastCASClient()
	if err != nil {
		return err
	}
	live, err := client.IntrospectToken(ctx, raw)
	if err != nil {
		return err
	}
	if !live.Active || live.Subject != claims.Subject || live.ClientID != claims.ClientID || !slices.Contains(strings.Fields(live.Scope), "workspace:read") || !slices.Contains(live.Audience, s.config.FastCASServiceAudience) {
		return ErrFastCASServiceDenied
	}
	return nil
}

func (s *Service) WorkspaceAccount(ctx context.Context, subject string) (persistence.User, string, error) {
	var user persistence.User
	var local persistence.FastCASLink
	err := s.store.DB.WithContext(ctx).Where("issuer = ? AND client_id = ? AND subject = ?", s.config.FastCASIssuer, s.config.FastCASClientID, subject).First(&local).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return user, "missing", nil
	}
	if err != nil {
		return user, "", err
	}
	if local.State != "active" {
		return user, "inactive", nil
	}
	client, err := s.FastCASClient()
	if err != nil {
		return user, "", err
	}
	link, err := client.ResolveLink(ctx, subject)
	if err != nil {
		var api *fastcas.APIError
		if errors.As(err, &api) && (api.Status == 404 || api.Status == 403) {
			return user, "inactive", nil
		}
		return user, "", err
	}
	if link.ID != local.ID || link.ClientID != local.ClientID || link.Subject != subject || link.LocalRef != local.UserID || link.State != "active" || link.Version != local.Version {
		return user, "inactive", nil
	}
	err = s.store.DB.WithContext(ctx).Where("id = ?", local.UserID).First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && user.Status != "active") {
		return user, "inactive", nil
	}
	if err != nil {
		return user, "", err
	}
	return user, "active", nil
}
