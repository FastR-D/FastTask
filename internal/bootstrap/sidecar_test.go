package bootstrap

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/config"
)

// The sidecar client and supervisor (doc/harness.md §8).
//
// What these tests pin down is the part that is easy to get wrong and expensive in production: the client
// only ever talks to this machine, it authenticates with the startup secret, a supervisor that cannot make
// the sidecar healthy says so instead of queueing runs into a hole, and none of it touches the WASM path.

// fakeSidecar serves the §8.3 interface on a unix socket and records what it was asked.
type fakeSidecar struct {
	listener net.Listener
	server   *http.Server
	healthy  bool
	requests []recordedCall
}

type recordedCall struct {
	path          string
	authorization string
	body          map[string]any
}

func newFakeSidecar(t *testing.T, socket string, healthy bool) *fakeSidecar {
	t.Helper()
	fake := &fakeSidecar{healthy: healthy}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	fake.listener = listener
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fake.requests = append(fake.requests, recordedCall{path: "/healthz", authorization: r.Header.Get("Authorization")})
		if !fake.healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "node_version": "v24.0.0", "native_addon": true, "libfx_version": application.LibfxVersion,
		})
	})
	mux.HandleFunc("/run", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fake.requests = append(fake.requests, recordedCall{path: "/run", authorization: r.Header.Get("Authorization"), body: body})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"stop_reason": "stop", "usage": map[string]any{"outputTokens": 12},
			"checkpoint": []byte("sidecar-checkpoint"), "libfx_version": application.LibfxVersion,
		})
	})
	mux.HandleFunc("/run/", func(w http.ResponseWriter, r *http.Request) {
		fake.requests = append(fake.requests, recordedCall{path: r.URL.Path, authorization: r.Header.Get("Authorization")})
		w.WriteHeader(http.StatusOK)
	})
	fake.server = &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	go func() { _ = fake.server.Serve(listener) }()
	t.Cleanup(func() {
		_ = fake.server.Close()
		_ = listener.Close()
	})
	return fake
}

func TestSidecarClientTalksToTheSocketWithTheSecret(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "sidecar.sock")
	fake := newFakeSidecar(t, socket, true)
	client, err := application.NewSidecarClient("unix://"+socket, "shared-secret", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if !client.Healthy(ctx) {
		t.Fatal("a healthy sidecar reported unhealthy")
	}
	result, err := client.Drive(ctx, application.SidecarRun{
		RunID: "run_1", HarnessToken: "fth_capability", Prompt: "帮我推进论文", LibfxVersion: application.LibfxVersion,
	})
	if err != nil {
		t.Fatalf("Drive: %v", err)
	}
	if result.StopReason != "stop" || string(result.Checkpoint) != "sidecar-checkpoint" {
		t.Fatalf("result=%#v", result)
	}
	if err := client.Cancel(ctx, "run_1"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	// Every call carried the shared secret, and /run carried the capability the host drives with (§8.2).
	for _, call := range fake.requests {
		if call.authorization != "Bearer shared-secret" {
			t.Fatalf("%s authorization=%q, want the shared secret", call.path, call.authorization)
		}
	}
	var sawRun bool
	for _, call := range fake.requests {
		if call.path != "/run" {
			continue
		}
		sawRun = true
		if call.body["run_id"] != "run_1" || call.body["harness_token"] != "fth_capability" || call.body["prompt"] != "帮我推进论文" {
			t.Fatalf("run body=%#v", call.body)
		}
	}
	if !sawRun {
		t.Fatal("the sidecar never received the run")
	}
	// The response carries no transcript: the model proxy already wrote it (§8.3, §14.3).
	if len(result.Usage) == 0 {
		t.Fatal("usage was not reported back for diagnostics")
	}
}

func TestSidecarClientRefusesNonLoopbackEndpoints(t *testing.T) {
	for _, endpoint := range []string{"http://10.0.0.5:9000", "https://sidecar.example.com", "http://0.0.0.0:9000", ""} {
		if _, err := application.NewSidecarClient(endpoint, "secret", time.Second); err == nil {
			t.Fatalf("endpoint %q was accepted; a sidecar must be loopback or a unix socket (§8.2)", endpoint)
		}
	}
	// Loopback is fine, on either spelling.
	for _, endpoint := range []string{"http://127.0.0.1:9000", "http://[::1]:9000", "http://localhost:9000"} {
		if _, err := application.NewSidecarClient(endpoint, "secret", time.Second); err != nil {
			t.Fatalf("loopback endpoint %q was refused: %v", endpoint, err)
		}
	}
}

func TestSidecarClientReportsUnhealthy(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "sidecar.sock")
	newFakeSidecar(t, socket, false)
	client, err := application.NewSidecarClient("unix://"+socket, "secret", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if client.Healthy(context.Background()) {
		t.Fatal("a sidecar answering 503 reported healthy")
	}
	if _, err := client.Drive(context.Background(), application.SidecarRun{RunID: "run_1"}); err == nil {
		// /run still answers in this fake; the point is that Healthy() is what gates queueing (§1.2).
		t.Log("drive succeeded against an unhealthy sidecar; Healthy() is the gate that matters")
	}
	// A socket nobody listens on is not a panic, it is "unhealthy".
	missing, err := application.NewSidecarClient("unix://"+filepath.Join(t.TempDir(), "absent.sock"), "secret", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if missing.Healthy(context.Background()) {
		t.Fatal("an absent socket reported healthy")
	}
}

func TestSidecarSupervisorGivesUpWithoutTouchingWasm(t *testing.T) {
	cfg := config.Config{
		SidecarEnabled: true, SidecarNodePath: "/bin/false", SidecarSocket: filepath.Join(t.TempDir(), "sidecar.sock"),
		SidecarStartTimeout: time.Second, DatabasePath: filepath.Join(t.TempDir(), "db.sqlite"),
	}
	supervisor, err := NewSidecar(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if supervisor.Driver() == nil {
		t.Fatal("an enabled sidecar produced no driver")
	}
	// Before a successful start the supervisor reports unavailable, so SubmitCommands refuses sidecar mode
	// rather than queueing a job nobody will run (§1.2).
	if supervisor.Available() {
		t.Fatal("a sidecar that never became healthy reported available")
	}
	if supervisor.Driver().Healthy(context.Background()) {
		t.Fatal("the driver reported a healthy sidecar that is not running")
	}

	// Repeated crashes cross the threshold and mark the mode unavailable (§8.4).
	for i := 0; i < sidecarFailureThreshold; i++ {
		supervisor.recordFailure()
	}
	if supervisor.Available() {
		t.Fatal("the supervisor still reports available after repeated failures")
	}

	// A disabled sidecar produces no driver at all, and its doctor line says so.
	disabled, err := NewSidecar(config.Config{DatabasePath: cfg.DatabasePath})
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Enabled() || disabled.Available() || disabled.Driver() != nil {
		t.Fatal("a disabled sidecar reported itself as usable")
	}
	lines, err := disabled.Doctor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0] != "sidecar=disabled (WASM mode only)" {
		t.Fatalf("doctor lines=%v", lines)
	}
	var nilSupervisor *SidecarSupervisor
	if nilSupervisor.Enabled() || nilSupervisor.Available() || nilSupervisor.Driver() != nil {
		t.Fatal("a nil supervisor must behave like a disabled one")
	}
}

func TestSidecarDoctorChecksNodeAndEntry(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{
		SidecarEnabled: true, SidecarNodePath: "/bin/echo", SidecarSocket: filepath.Join(dir, "sidecar.sock"),
		SidecarStartTimeout: time.Second, DatabasePath: filepath.Join(dir, "db.sqlite"),
	}
	supervisor, err := NewSidecar(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// The entry does not exist yet: the doctor must say so, and say how to build it.
	lines, err := supervisor.Doctor(context.Background())
	if err == nil {
		t.Fatalf("doctor accepted a missing entry: %v", lines)
	}
	joined := err.Error()
	for _, line := range lines {
		joined += "\n" + line
	}
	if !contains(joined, "build it with") {
		t.Fatalf("the doctor does not tell the operator how to fix a missing entry: %s", joined)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
