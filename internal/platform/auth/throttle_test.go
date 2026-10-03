package auth

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/config"
	"github.com/FastR-D/FastTask/internal/persistence"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func newTestThrottle() (*loginThrottle, *fakeClock) {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	t := newLoginThrottle()
	t.now = clock.Now
	return t, clock
}

func TestThrottleBlocksOneClientWithoutLockingOutOthers(t *testing.T) {
	throttle, clock := newTestThrottle()
	for i := 0; i < ruleAccountClient.threshold; i++ {
		if _, ok := throttle.begin("admin", "198.51.100.7"); !ok {
			t.Fatalf("attempt %d refused before the threshold", i+1)
		}
	}
	if _, ok := throttle.begin("admin", "198.51.100.7"); ok {
		t.Fatal("attacker was not blocked after the threshold")
	}
	// The real user, from elsewhere, is unaffected: identifier-only lockout is the denial of service
	// the per-client key exists to prevent.
	ticket, ok := throttle.begin("admin", "203.0.113.9")
	if !ok {
		t.Fatal("a different client was locked out of the account")
	}
	throttle.succeed(ticket)
	clock.now = clock.now.Add(ruleAccountClient.maxBlock + time.Second)
	if _, ok := throttle.begin("admin", "198.51.100.7"); !ok {
		t.Fatal("block never expired")
	}
}

func TestThrottleBackoffDoublesWithinAWindow(t *testing.T) {
	throttle, clock := newTestThrottle()
	key := ruleAccountClient.prefix + "198.51.100.7\x00victim"
	want := throttleBaseBlock
	for round := 0; round < 4; round++ {
		for {
			if _, ok := throttle.begin("victim", "198.51.100.7"); !ok {
				break
			}
		}
		entry := throttle.entries[key]
		if block := entry.blockedUntil.Sub(clock.now); block != want {
			t.Fatalf("round %d: block %s, want %s", round, block, want)
		}
		clock.now = entry.blockedUntil.Add(time.Millisecond)
		want *= 2
	}
}

func TestThrottleBlockIsCapped(t *testing.T) {
	throttle, clock := newTestThrottle()
	key := ruleAccountClient.prefix + "198.51.100.7\x00victim"
	throttle.entries[key] = &throttleEntry{failures: 40, windowStart: clock.now, lastSeen: clock.now}
	throttle.begin("victim", "198.51.100.7")
	if block := throttle.entries[key].blockedUntil.Sub(clock.now); block != ruleAccountClient.maxBlock {
		t.Fatalf("block %s, want the cap %s", block, ruleAccountClient.maxBlock)
	}
}

// Failures spread over a working day must not add up to a lockout: the count restarts each window.
func TestThrottleCountRestartsEachWindow(t *testing.T) {
	throttle, clock := newTestThrottle()
	for day := 0; day < 20; day++ {
		for i := 0; i < ruleAccountClient.threshold-1; i++ {
			if _, ok := throttle.begin("typo-prone", "203.0.113.1"); !ok {
				t.Fatalf("window %d: blocked after %d failures", day, i)
			}
		}
		clock.now = clock.now.Add(throttleWindow + time.Second)
	}
}

func TestThrottleLimitsSprayingAcrossAccounts(t *testing.T) {
	throttle, _ := newTestThrottle()
	for i := 0; i < ruleClient.threshold; i++ {
		if _, ok := throttle.begin("user"+string(rune('a'+i%26))+string(rune('a'+i/26)), "198.51.100.7"); !ok {
			t.Fatalf("attempt %d refused before the per-client threshold", i+1)
		}
	}
	if _, ok := throttle.begin("someone-new", "198.51.100.7"); ok {
		t.Fatal("one client sprayed past the per-client threshold")
	}
}

func TestThrottleIsBoundedAndForgetsIdleEntries(t *testing.T) {
	throttle, clock := newTestThrottle()
	for i := 0; i < throttleMaxEntries; i++ {
		throttle.begin("id-"+time.Duration(i).String(), "")
	}
	if len(throttle.entries) > throttleMaxEntries+3 {
		t.Fatalf("throttle grew to %d entries", len(throttle.entries))
	}
	clock.now = clock.now.Add(throttleIdle + time.Minute)
	throttle.begin("fresh", "")
	if len(throttle.entries) > 3 {
		t.Fatalf("idle entries were kept: %d remain", len(throttle.entries))
	}
}

func TestLoginThrottleHoldsUnderConcurrentGuesses(t *testing.T) {
	store, err := persistence.Open(filepath.Join(t.TempDir(), "concurrent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Config{JWTSecret: "rate-test-secret-with-enough-characters", AccessTTL: time.Hour, RefreshTTL: time.Hour, AdminIdentifier: "admin", AdminPassword: "password-for-tests", AdminName: "Admin"})
	if err := service.EnsureAdmin(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := WithClientIP(context.Background(), "198.51.100.7")
	var verified atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, _, err := service.Login(ctx, "admin", "wrong-password-guess"); err == ErrInvalidCredentials {
				verified.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := verified.Load(); got != int32(ruleAccountClient.threshold) {
		t.Fatalf("%d concurrent guesses reached password verification, want exactly %d", got, ruleAccountClient.threshold)
	}
	if _, _, _, err := service.Login(ctx, "admin", "password-for-tests"); err != ErrLoginThrottled {
		t.Fatalf("blocked client logged in: %v", err)
	}
	// Another client still can.
	if _, _, _, err := service.Login(WithClientIP(context.Background(), "203.0.113.9"), "admin", "password-for-tests"); err != nil {
		t.Fatalf("legitimate client refused: %v", err)
	}
}

func TestLoginUnknownIdentifierDoesTheSameWork(t *testing.T) {
	store, err := persistence.Open(filepath.Join(t.TempDir(), "enum.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Config{JWTSecret: "rate-test-secret-with-enough-characters", AccessTTL: time.Hour, RefreshTTL: time.Hour, AdminIdentifier: "admin", AdminPassword: "password-for-tests", AdminName: "Admin"})
	if err := service.EnsureAdmin(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _, _, unknownErr := service.Login(context.Background(), "nobody-here", "wrong-password-guess")
	_, _, _, wrongErr := service.Login(context.Background(), "admin", "wrong-password-guess")
	if unknownErr != ErrInvalidCredentials || wrongErr != ErrInvalidCredentials {
		t.Fatalf("errors differ: unknown=%v wrong=%v", unknownErr, wrongErr)
	}
	// The unknown identifier must have gone through a real Argon2id verification.
	if !strings.HasPrefix(dummyPasswordHash(), "argon2id$") {
		t.Fatalf("dummy hash is not a real Argon2id hash: %q", dummyPasswordHash())
	}
	if VerifyPassword("anything", dummyPasswordHash()) {
		t.Fatal("dummy hash matched a password")
	}
}

func TestVerifyPasswordRejectsHostileParameters(t *testing.T) {
	for _, encoded := range []string{
		"argon2id$v=19$m=4294967295,t=1,p=4$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"argon2id$v=19$m=65536,t=100000,p=4$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"argon2i$v=19$m=65536,t=1,p=4$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	} {
		if VerifyPassword("password", encoded) {
			t.Fatalf("accepted %q", encoded)
		}
	}
}
