package auth

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fastcas "github.com/FastR-D/FastCAS/sdk/go"
	"github.com/FastR-D/FastTask/internal/config"
	"github.com/FastR-D/FastTask/internal/persistence"
)

func casFixture(t *testing.T) (*persistence.Store, *Service) {
	t.Helper()
	store, err := persistence.Open(filepath.Join(t.TempDir(), "cas.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err = store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{JWTSecret: "fasttask-test-secret-at-least-32-characters", PanelJWTSecret: "separate-panel-test-secret-at-least-32", AccessTTL: time.Hour, RefreshTTL: time.Hour, AdminIdentifier: "admin", AdminPassword: "local-password-for-test", AdminName: "Admin", FastCASIssuer: "https://cas.example.test", FastCASClientID: "task", FastCASClientSecret: "independent-client-secret-at-least-32", FastCASRedirectURI: "https://task.example.test/callback"}
	service := New(store, cfg)
	if err = service.EnsureAdmin(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store, service
}
func TestFastCASPersistentTransactionConsumedOnce(t *testing.T) {
	store, _ := casFixture(t)
	ctx := context.Background()
	txs := FastCASTransactions{store}
	if err := txs.Put(ctx, fastcas.Transaction{State: "single-use", Purpose: "login", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := txs.Take(ctx, "single-use")
			if err != nil {
				t.Error(err)
			}
			if tx != nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("consumed %d times", successes.Load())
	}
}
func TestFastCASRevocationPreservesLocalSessionAndPermissions(t *testing.T) {
	store, s := casFixture(t)
	ctx := context.Background()
	user, localToken, _, err := s.Login(ctx, "admin", "local-password-for-test")
	if err != nil {
		t.Fatal(err)
	}
	link := fastcas.Link{ID: "link-one", ClientID: "task", LocalRef: user.ID, Subject: "cas-subject", State: "active", Version: 2, VerifiedAt: time.Now()}
	row := localLink(s.config.FastCASIssuer, link)
	if err = store.DB.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	identity := fastcas.Identity{Issuer: s.config.FastCASIssuer, Subject: link.Subject, SessionID: "upstream-session"}
	cas, err := s.loginFastCAS(ctx, identity, link)
	if err != nil {
		t.Fatal(err)
	}
	_, rotated, refresh, err := s.Refresh(ctx, cas.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticateServicePrincipal(rotated); err == nil {
		t.Fatal("user session accepted as service identity")
	}
	if _, err = s.Authenticate(rotated); err != nil {
		t.Fatal(err)
	}
	link.State = "revoked"
	link.Version = 3
	for range 2 {
		if err = s.applyFastCASLink(ctx, link, "event-one"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.Authenticate(rotated); err == nil {
		t.Fatal("CAS session survived revocation")
	}
	if _, _, _, err = s.Refresh(ctx, refresh); err == nil {
		t.Fatal("CAS refresh survived revocation")
	}
	if p, err := s.Authenticate(localToken); err != nil || p.UserID != user.ID || p.Role != user.Role {
		t.Fatal("local session/role changed", err)
	}
	var count int64
	store.DB.Model(&persistence.FastCASEvent{}).Count(&count)
	if count != 1 {
		t.Fatal("event not deduplicated")
	}
	link.State = "active"
	link.Version = 2
	if err = s.applyFastCASLink(ctx, link, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.loginFastCAS(ctx, identity, link); err == nil {
		t.Fatal("older version revived revoked link")
	}
}

func TestFastCASIdentityDisablePreservesLocalSession(t *testing.T) {
	store, s := casFixture(t)
	ctx := context.Background()
	user, localToken, _, err := s.Login(ctx, "admin", "local-password-for-test")
	if err != nil {
		t.Fatal(err)
	}
	link := fastcas.Link{ID: "link-one", ClientID: "task", LocalRef: user.ID, Subject: "cas-subject", State: "active", Version: 2, VerifiedAt: time.Now()}
	row := localLink(s.config.FastCASIssuer, link)
	if err = store.DB.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	cas, err := s.loginFastCAS(ctx, fastcas.Identity{Issuer: s.config.FastCASIssuer, Subject: link.Subject}, link)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = s.applyFastCASIdentityDisable(ctx, "status-event", link.Subject); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.Authenticate(cas.AccessToken); err == nil {
		t.Fatal("FastCAS session survived identity disable")
	}
	if _, err = s.Authenticate(localToken); err != nil {
		t.Fatalf("local session revoked: %v", err)
	}
	var count int64
	store.DB.Model(&persistence.FastCASEvent{}).Count(&count)
	if count != 1 {
		t.Fatalf("identity event applied %d times", count)
	}
}
