package application

import (
	"context"
	"github.com/FastR-D/FastTask/internal/persistence"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestHarnessGrantRaceAndStalePrincipal(t *testing.T) {
	svc, p := providerHarness(t, func(w http.ResponseWriter, r *http.Request) { t.Error("stale host reached provider") }, LoopLimits{})
	var wg sync.WaitGroup
	grants := make(chan RunGrant, 20)
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			grant, err := svc.IssueBrowserRunGrant(context.Background(), p.UserID, p.RunID, "")
			if err != nil {
				errs <- err
			} else {
				grants <- grant
			}
		}()
	}
	wg.Wait()
	close(grants)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	accepted := 0
	for grant := range grants {
		if _, err := svc.AuthenticateHarness(context.Background(), "Bearer "+grant.HarnessToken); err == nil {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("live host grants: %d", accepted)
	}
	if _, err := svc.OpenModelProxy(context.Background(), p, []byte(`{}`)); err == nil {
		t.Fatal("previously authenticated stale principal acquired model")
	}
	var count int64
	svc.store.DB.Model(&persistence.AgentHarnessToken{}).Where("run_id = ? AND revoked_at IS NULL", p.RunID).Count(&count)
	if count != 1 {
		t.Fatalf("live tokens: %d", count)
	}
}
func TestHarnessRotationResponseLossAndGenerationFencing(t *testing.T) {
	svc, p := providerHarness(t, func(w http.ResponseWriter, r *http.Request) {}, LoopLimits{})
	ctx := context.Background()
	now := persistence.Now()
	svc.nowFn = func() time.Time { return now }
	if err := svc.store.DB.Model(&persistence.AgentHarnessToken{}).Where("id = ?", p.TokenID).Update("expires_at", now.Add(5*time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	first, err := svc.HarnessHeartbeat(ctx, p)
	if err != nil || first.HarnessToken == "" {
		t.Fatalf("rotation failed: %+v %v", first, err)
	}
	replay, err := svc.HarnessHeartbeat(ctx, p)
	if err != nil || replay.HarnessToken != first.HarnessToken {
		t.Fatalf("lost rotation response unrecoverable: %+v %v", replay, err)
	}
	successor, err := svc.AuthenticateHarness(ctx, "Bearer "+first.HarnessToken)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := svc.repo.GetHarnessToken(ctx, p.TokenID)
	if old.NextTokenCipher == first.HarnessToken || old.NextTokenCipher == "" {
		t.Fatal("successor not encrypted")
	}
	now = now.Add(31 * time.Second)
	if _, err := svc.activeRun(ctx, p); err == nil {
		t.Fatal("old rotation grace never expired")
	}
	grant, err := svc.IssueBrowserRunGrant(ctx, p.UserID, p.RunID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.activeRun(ctx, successor); err == nil {
		t.Fatal("new generation did not fence old successor")
	}
	if _, err := svc.AuthenticateHarness(ctx, "Bearer "+grant.HarnessToken); err != nil {
		t.Fatal(err)
	}
}
