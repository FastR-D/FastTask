package auth

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Password-guessing defence (doc/tech.md §12). FastTask runs as one process over one SQLite file, so the
// throttle lives in memory: it is reset by a restart, which is acceptable because a restart is neither
// attacker-triggerable nor frequent, and it needs no shared store because there is never a second writer.
//
// Three keys are charged for every attempt, each with its own threshold:
//
//   - account+client: the ordinary case, one client guessing one account. Blocks quickly.
//   - client:         one client spraying many accounts. A higher threshold, so a shared NAT that
//     mistypes a few passwords is not locked out.
//   - account:        many clients guessing one account. The highest threshold. This is the one key an
//     attacker can use to lock a real user out, so it only slows a distributed attack down and never
//     blocks for long; the account+client key is what stops a single source.
//
// An attempt is charged before the password is checked and refunded when it succeeds. Checking and
// charging under one lock is what makes the limit hold against parallel requests: with check-then-verify-
// then-record, fifty concurrent guesses all pass the check before the first failure is recorded.
type loginThrottle struct {
	mu        sync.Mutex
	entries   map[string]*throttleEntry
	now       func() time.Time
	lastSweep time.Time
}

type throttleEntry struct {
	failures     int
	blockedUntil time.Time
	lastSeen     time.Time
}

type throttleRule struct {
	prefix    string
	threshold int
	maxBlock  time.Duration
}

var (
	ruleAccountClient = throttleRule{prefix: "ac:", threshold: 5, maxBlock: 15 * time.Minute}
	ruleClient        = throttleRule{prefix: "c:", threshold: 30, maxBlock: 15 * time.Minute}
	ruleAccount       = throttleRule{prefix: "a:", threshold: 50, maxBlock: 2 * time.Minute}
)

const (
	// throttleBaseBlock is the first block once a key reaches its threshold; each further failure
	// doubles it up to the rule's cap.
	throttleBaseBlock = 30 * time.Second
	// throttleIdle forgets a key that has not failed for this long. It is longer than every cap, so an
	// entry is never forgotten while it is still blocking.
	throttleIdle = 30 * time.Minute
	// throttleMaxEntries bounds memory against an attacker cycling identifiers or addresses. Past it,
	// the oldest unblocked entries are dropped first.
	throttleMaxEntries = 50_000
)

func newLoginThrottle() *loginThrottle {
	return &loginThrottle{entries: map[string]*throttleEntry{}, now: time.Now}
}

type throttleTicket struct {
	keys []throttleKey
}

type throttleKey struct {
	key  string
	rule throttleRule
}

func throttleKeys(identifier, client string) []throttleKey {
	account := strings.ToLower(strings.TrimSpace(identifier))
	keys := []throttleKey{{ruleAccountClient.prefix + client + "\x00" + account, ruleAccountClient}}
	if client != "" {
		keys = append(keys, throttleKey{ruleClient.prefix + client, ruleClient})
	}
	return append(keys, throttleKey{ruleAccount.prefix + account, ruleAccount})
}

// begin reports whether an attempt may proceed and, if it may, charges it to every key. The returned
// ticket is handed back to succeed when the credentials turn out to be correct.
func (t *loginThrottle) begin(identifier, client string) (throttleTicket, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.sweepLocked(now)
	keys := throttleKeys(identifier, client)
	for _, k := range keys {
		if entry := t.entries[k.key]; entry != nil && entry.blockedUntil.After(now) {
			return throttleTicket{}, false
		}
	}
	for _, k := range keys {
		entry := t.entries[k.key]
		if entry == nil {
			entry = &throttleEntry{}
			t.entries[k.key] = entry
		}
		entry.failures++
		entry.lastSeen = now
		if over := entry.failures - k.rule.threshold; over >= 0 {
			block := throttleBaseBlock << min(over, 10)
			entry.blockedUntil = now.Add(min(block, k.rule.maxBlock))
		}
	}
	return throttleTicket{keys: keys}, true
}

// succeed refunds the attempt. The account+client key is cleared outright: the client has proved it
// knows the password. The shared keys only lose this one charge, so a correct login from one client
// does not wipe out what other clients have been doing.
func (t *loginThrottle) succeed(ticket throttleTicket) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, k := range ticket.keys {
		entry := t.entries[k.key]
		if entry == nil {
			continue
		}
		if k.rule == ruleAccountClient {
			delete(t.entries, k.key)
			continue
		}
		entry.failures--
		if entry.failures < k.rule.threshold {
			entry.blockedUntil = time.Time{}
		}
		if entry.failures <= 0 {
			delete(t.entries, k.key)
		}
	}
}

func (t *loginThrottle) sweepLocked(now time.Time) {
	if len(t.entries) < throttleMaxEntries && now.Sub(t.lastSweep) < time.Minute {
		return
	}
	t.lastSweep = now
	for key, entry := range t.entries {
		if now.Sub(entry.lastSeen) > throttleIdle && !entry.blockedUntil.After(now) {
			delete(t.entries, key)
		}
	}
	if len(t.entries) < throttleMaxEntries {
		return
	}
	// Still full of live entries: shed unblocked ones. Blocked entries are what the throttle exists
	// for, so they are the last to go.
	for key, entry := range t.entries {
		if len(t.entries) < throttleMaxEntries*9/10 {
			return
		}
		if !entry.blockedUntil.After(now) {
			delete(t.entries, key)
		}
	}
}

type clientIPKey struct{}

// WithClientIP records the caller's network address, as resolved by the HTTP layer from the trusted
// proxy configuration, so the login throttle can key on it.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey{}, ip)
}

func clientIPFromContext(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey{}).(string)
	return ip
}
