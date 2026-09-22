package bootstrap

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/config"
	"github.com/FastR-D/FastTask/internal/persistence"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"
	"go.uber.org/goleak"
)

// testConfig returns a configuration that points at a throwaway database and an
// ephemeral loopback port, so fx apps can start and stop inside tests without
// touching real state.
func testConfig(t *testing.T) config.Config {
	t.Helper()
	port := freePort(t)
	return config.Config{
		Listen:                "127.0.0.1",
		Port:                  port,
		PublicURL:             fmt.Sprintf("http://127.0.0.1:%d", port),
		DatabasePath:          filepath.Join(t.TempDir(), "bootstrap-test.db"),
		JWTSecret:             "bootstrap-test-secret-value-0123456789",
		AccessTTL:             time.Hour,
		RefreshTTL:            24 * time.Hour,
		AdminIdentifier:       "admin",
		AdminPassword:         "bootstrap-test-admin-password",
		AdminName:             "Bootstrap Test Admin",
		WorkerInterval:        50 * time.Millisecond,
		WebDist:               filepath.Join(t.TempDir(), "no-web"),
		Environment:           "development",
		AudioDir:              filepath.Join(t.TempDir(), "audio"),
		PanelJWTSecret:        "bootstrap-test-panel-secret-0123456789",
		ProviderEncryptionKey: "bootstrap-test-provider-key-0123456789",
		TrustedProxies:        []string{"127.0.0.1/32"},
		IntegrationTimeout:    time.Second,
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestValidateRoleGraphs asserts every role composition builds a complete
// dependency graph. fx.ValidateApp runs in DryRun mode, so no constructor
// executes and no port or database is touched; assembly errors surface here in
// CI rather than at production startup (wiring.md §2 rule 5, §8).
func TestValidateRoleGraphs(t *testing.T) {
	cfg := testConfig(t)
	cases := []struct {
		name    string
		options []fx.Option
	}{
		{"serve-full", append(serveOptions(cfg, ServeOptions{WithWorker: true, WithScheduler: true}), fx.NopLogger)},
		{"serve-http-only", append(serveOptions(cfg, ServeOptions{}), fx.NopLogger)},
		{"serve-role", []fx.Option{ServeRole, fx.Supply(cfg), fx.NopLogger}},
		{"worker-role", []fx.Option{WorkerRole, fx.Supply(cfg), fx.NopLogger}},
		{"scheduler-role", []fx.Option{SchedulerRole, fx.Supply(cfg), fx.NopLogger}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := fx.ValidateApp(tc.options...); err != nil {
				t.Fatalf("graph validation failed: %v", err)
			}
		})
	}
}

// TestServeRoleStartsServesAndStops is the fxtest integration test required by
// wiring.md §8: start the full serve role, hit the health endpoint, shut down
// gracefully, and assert no goroutines leak.
func TestServeRoleStartsServesAndStops(t *testing.T) {
	defer goleak.VerifyNone(t,
		// SQLite and the net/http server wind down asynchronously; give them a
		// moment and ignore well-known runtime background goroutines.
		goleak.IgnoreTopFunction("database/sql.(*DB).connectionOpener"),
		goleak.IgnoreAnyFunction("os/signal.signal_recv"),
	)

	cfg := testConfig(t)
	app := fxtest.New(t,
		append(serveOptions(cfg, ServeOptions{WithWorker: true, WithScheduler: true}), fx.NopLogger)...,
	)
	app.RequireStart()
	defer app.RequireStop()

	client := &http.Client{Timeout: 5 * time.Second}
	url := fmt.Sprintf("http://%s/health/ready", cfg.Address())
	var ready bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			ready = resp.StatusCode == http.StatusOK
			resp.Body.Close()
			if ready {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("health/ready never returned 200 at %s", url)
	}
}

// TestServeRoleServesRegistrarRoutes proves the fx "routes" value group
// (wiring.md §7 step 7) actually registers domain routes in the assembled
// server — not merely that the dependency graph resolves. The OpenAPI document
// served by the fx-built serve role must contain paths contributed by several
// distinct group registrars. Path keys are config-independent (only the servers
// URL derives from cfg.PublicURL), so this complements, without duplicating, the
// byte-identical golden assertion in internal/httpapi (which exercises the
// builtin registrar path). Together they show both assembly routes — builtin and
// fx value group — produce the same external contract.
func TestServeRoleServesRegistrarRoutes(t *testing.T) {
	cfg := testConfig(t)
	app := fxtest.New(t, append(serveOptions(cfg, ServeOptions{}), fx.NopLogger)...)
	app.RequireStart()
	defer app.RequireStop()

	client := &http.Client{Timeout: 5 * time.Second}
	url := fmt.Sprintf("http://%s/api/v1/openapi.json", cfg.Address())
	var body []byte
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			body, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	// One path from each of three different registrars collected via group:"routes".
	for _, path := range []string{`"/auth/login"`, `"/daily-plans"`, `"/integrations/status"`} {
		if !bytes.Contains(body, []byte(path)) {
			t.Fatalf("fx-assembled OpenAPI missing registrar path %s; the routes value group did not register it", path)
		}
	}
}

// spyObserver records the order of component start/stop transitions.
type spyObserver struct {
	mu      sync.Mutex
	stopped []string
}

func (s *spyObserver) Started(string) {}
func (s *spyObserver) Stopped(component string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = append(s.stopped, component)
}

func (s *spyObserver) order() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.stopped...)
}

// TestShutdownStopsHTTPBeforeWorker asserts the ordering contract from
// wiring.md §6: on shutdown HTTP must stop before the worker, so no new request
// enters a runtime that is already draining. fx runs OnStop hooks in reverse
// registration order; HTTPModule is appended last in serveOptions, so it stops
// first.
func TestShutdownStopsHTTPBeforeWorker(t *testing.T) {
	defer goleak.VerifyNone(t,
		goleak.IgnoreTopFunction("database/sql.(*DB).connectionOpener"),
		goleak.IgnoreAnyFunction("os/signal.signal_recv"),
	)

	cfg := testConfig(t)
	spy := &spyObserver{}
	app := fxtest.New(t,
		append(
			serveOptions(cfg, ServeOptions{WithWorker: true, WithScheduler: true}),
			fx.Decorate(func() LifecycleObserver { return spy }),
			fx.NopLogger,
		)...,
	)
	app.RequireStart()
	app.RequireStop()

	order := spy.order()
	httpIdx, workerIdx := -1, -1
	for i, component := range order {
		switch component {
		case "http":
			httpIdx = i
		case "worker":
			workerIdx = i
		}
	}
	if httpIdx == -1 || workerIdx == -1 {
		t.Fatalf("expected both http and worker stop events, got %v", order)
	}
	if httpIdx > workerIdx {
		t.Fatalf("HTTP stopped after worker (order=%v); HTTP must stop first", order)
	}
}

// TestServeReturnsOnContextCancel verifies the Serve entry point honours context
// cancellation and returns cleanly, which is how the serve command shuts down on
// SIGINT/SIGTERM.
func TestServeReturnsOnContextCancel(t *testing.T) {
	defer goleak.VerifyNone(t,
		goleak.IgnoreTopFunction("database/sql.(*DB).connectionOpener"),
		goleak.IgnoreAnyFunction("os/signal.signal_recv"),
	)

	cfg := testConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, cfg, ServeOptions{WithWorker: true, WithScheduler: true})
	}()

	// Wait for the HTTP listener to accept connections, then cancel.
	deadline := time.Now().Add(5 * time.Second)
	addr := cfg.Address()
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Serve did not return after context cancellation")
	}
}

// seedRunningRun opens the database at path, migrates it, and leaves one agent
// run in the running state — simulating a predecessor process that crashed mid
// run. It returns the owning user and the stale run id.
func seedRunningRun(t *testing.T, path string) (userID, runID string) {
	t.Helper()
	store, err := persistence.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := persistence.Now()
	user := persistence.User{ID: persistence.NewID("user"), Identifier: "reap", PasswordHash: "h", DisplayName: "Reap", Timezone: "UTC", Locale: "en", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	repo := persistence.NewAgentRepository(store)
	thread, err := repo.CreateThread(ctx, user.ID, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	run := &persistence.AgentRun{UserID: user.ID, ThreadID: thread.ID, Status: persistence.RunRunning}
	if err := repo.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	return user.ID, run.ID
}

// TestWorkerStartupReapsInterruptedRuns proves the phase F wiring: the worker's
// OnStart marks runs left in flight by a crashed predecessor as interrupted
// BEFORE it claims any job (agent-impl.md §4.2, §10 "进程重启后运行标记为
// interrupted"). v1 does not resume across a process restart, so a stale running
// run must be terminal-failed on boot rather than silently re-executed.
func TestWorkerStartupReapsInterruptedRuns(t *testing.T) {
	defer goleak.VerifyNone(t,
		goleak.IgnoreTopFunction("database/sql.(*DB).connectionOpener"),
		goleak.IgnoreAnyFunction("os/signal.signal_recv"),
	)

	cfg := testConfig(t)
	userID, runID := seedRunningRun(t, cfg.DatabasePath)

	var store *persistence.Store
	app := fxtest.New(t, append(serveOptions(cfg, ServeOptions{WithWorker: true}), fx.Populate(&store), fx.NopLogger)...)
	app.RequireStart()
	defer app.RequireStop()

	// OnStart runs synchronously, so the reap has already happened by the time
	// RequireStart returns; poll briefly to be robust against scheduling.
	repo := persistence.NewAgentRepository(store)
	deadline := time.Now().Add(3 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		run, err := repo.GetRun(context.Background(), userID, runID)
		if err != nil {
			t.Fatal(err)
		}
		status = run.Status
		if status == persistence.RunInterrupted {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status != persistence.RunInterrupted {
		t.Fatalf("stale run status=%q, want interrupted (worker OnStart must reap before claiming jobs)", status)
	}
}

// TestWorkerRoleProcessesJobStandalone proves the `fasttask worker` role boots on
// its own (Core + WorkerModule, no HTTP listener) and actually processes a queued
// job (wiring.md §8/§9: "各自能独立启动并处理作业"). A voice_transcription job is
// used because it needs no model, transcriber or seed subject: with no transcriber
// configured the handler returns a demo transcript and it has no materializer, so
// it completes deterministically.
func TestWorkerRoleProcessesJobStandalone(t *testing.T) {
	defer goleak.VerifyNone(t,
		goleak.IgnoreTopFunction("database/sql.(*DB).connectionOpener"),
		goleak.IgnoreAnyFunction("os/signal.signal_recv"),
	)

	cfg := testConfig(t)
	ctx := context.Background()

	// Seed a user and a queued job through the real CreateJob path.
	seedStore, err := persistence.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := seedStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := persistence.Now()
	user := persistence.User{ID: persistence.NewID("user"), Identifier: "worker-role", PasswordHash: "h", DisplayName: "W", Timezone: "UTC", Locale: "en", Role: "member", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := seedStore.DB.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	job, err := application.New(seedStore).CreateJob(ctx, user.ID, "voice_transcription", "voice", "", 0, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	jobID := job.ID
	if err := seedStore.Close(); err != nil {
		t.Fatal(err)
	}

	// Run the worker role standalone; cancel once the job is processed.
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunWorker(runCtx, cfg) }()

	checkStore, err := persistence.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer checkStore.Close()
	deadline := time.Now().Add(5 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		var j persistence.AgentJob
		// Tolerate transient SQLITE_BUSY while the worker holds the write lock.
		if err := checkStore.DB.First(&j, "id = ?", jobID).Error; err == nil {
			status = j.Status
			if status == "succeeded" {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunWorker returned error: %v", err)
	}
	if status != "succeeded" {
		t.Fatalf("standalone worker left job status=%q, want succeeded (§8: worker role processes jobs independently)", status)
	}
}

// TestSchedulerRoleStartsAndStopsStandalone proves the scheduler role graph boots
// and shuts down cleanly on its own (wiring.md §8/§9: "fasttask scheduler ...
// 可独立运行"). fxtest runs every OnStart (store migrate, gocron Start) then every
// OnStop (gocron Shutdown) deterministically — no signal/timing race — and goleak
// asserts the cron goroutine is released. RunScheduler wraps this same
// SchedulerRole graph in the shared runRole lifecycle that the Serve and
// RunWorker tests exercise directly.
func TestSchedulerRoleStartsAndStopsStandalone(t *testing.T) {
	defer goleak.VerifyNone(t,
		goleak.IgnoreTopFunction("database/sql.(*DB).connectionOpener"),
		goleak.IgnoreAnyFunction("os/signal.signal_recv"),
	)

	cfg := testConfig(t)
	app := fxtest.New(t, SchedulerRole, fx.Supply(cfg), fx.NopLogger)
	app.RequireStart()
	app.RequireStop()
}
