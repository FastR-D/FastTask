package application

import (
	"context"

	"github.com/FastR-D/FastTask/internal/persistence"
	platformauth "github.com/FastR-D/FastTask/internal/platform/auth"
)

// ProviderService owns model-provider administration and the API-key crypto that
// protects provider secrets (wiring.md §4, arch.md §7.1). It is split out of the
// App facade because the Worker resolves the active provider before every job
// (main.go:66-83 historically), so provider access is not an admin-only concern.
//
// The constructor is a plain Go function (wiring.md §2 rule 2): it can be built
// and unit-tested without fx. secretKey is the already-derived AES-GCM key, or nil
// when provider encryption is not configured (matching the historical New vs
// NewWithSecret behaviour, where a nil key makes encrypt/decrypt fail closed).
type ProviderService struct {
	Store     *persistence.Store
	secretKey []byte
}

// NewProviderService builds the provider service. secretKey may be nil (no
// encryption configured) or a key derived via deriveSecretKey.
func NewProviderService(store *persistence.Store, secretKey []byte) *ProviderService {
	return &ProviderService{Store: store, secretKey: secretKey}
}

// currentAdmin resolves and authorizes the admin principal from the request
// context. It is a package-level helper (not an App method) so services that are
// no longer part of the App facade — e.g. ProviderService.VerifyModelProvider —
// can enforce the same admin gate without depending on App.
func currentAdmin(ctx context.Context, store *persistence.Store) (persistence.User, error) {
	p, ok := platformauth.PrincipalFromContext(ctx)
	if !ok || p.UserID == "" || p.Role != "admin" {
		return persistence.User{}, ErrForbidden
	}
	var user persistence.User
	if err := store.DB.WithContext(ctx).First(&user, "id = ?", p.UserID).Error; err != nil {
		return persistence.User{}, notFound(err)
	}
	if user.Role != "admin" || user.Status != "active" {
		return persistence.User{}, ErrForbidden
	}
	return user, nil
}
