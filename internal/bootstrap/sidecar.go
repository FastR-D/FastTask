package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/FastR-D/FastTask/internal/application"
	"github.com/FastR-D/FastTask/internal/config"
	"go.uber.org/fx"
)

// The Node sidecar's lifecycle (doc/harness.md §8.4).
//
// The sidecar is a fallback host for browsers without JSPI, so it is optional and OFF by default: a
// deployment whose users all have JSPI ships one Go binary and nothing else (§8.1). When it is enabled it
// is supervised properly, because "half-working" is the worst state for a component whose only job is to
// be a fallback: readiness must fail startup, a crash must restart with backoff, and repeated failure must
// mark the sidecar unavailable without touching the WASM path (§8.4, §14.10).

// sidecarDefaults are the values §8.4 names, kept here so a config that leaves them empty still behaves.
const (
	sidecarDefaultEntry      = "sidecar/dist/host.mjs"
	sidecarRestartBackoffMax = 30 * time.Second
	sidecarFailureThreshold  = 5
	sidecarStopGrace         = 5 * time.Second
	// sidecarProbeInterval is how often an externally managed host is checked. It is a fallback path, so
	// the interval trades a little staleness for not polling a process this one does not own.
	sidecarProbeInterval = 5 * time.Second
)

// SidecarModule provides the supervisor and its lifecycle. It is registered before HTTPModule so that it
// starts before the server accepts traffic and stops after the server has drained (§8.4).
var SidecarModule = fx.Module("sidecar",
	fx.Provide(NewSidecar),
	fx.Invoke(registerSidecarLifecycle),
)

// SidecarSupervisor owns the sidecar process and the client that talks to it.
type SidecarSupervisor struct {
	cfg    config.Config
	entry  string
	socket string
	secret string

	client *application.SidecarClient

	mu          sync.Mutex
	cmd         *exec.Cmd
	stop        context.CancelFunc
	done        chan struct{}
	failures    int
	unavailable bool
	started     bool

	// probeInterval and failureThreshold are fields rather than constants so a test can watch availability
	// flip without waiting out production timings. Their defaults are the §8.4 behaviour. Both are written
	// once at construction and read afterwards, so they need no lock of their own.
	probeInterval    time.Duration
	failureThreshold int
}

// NewSidecar builds the supervisor. When the sidecar is disabled it returns a supervisor that reports
// itself unavailable, so the rest of the graph does not have to branch on a nil dependency.
func NewSidecar(cfg config.Config) (*SidecarSupervisor, error) {
	supervisor := &SidecarSupervisor{
		cfg: cfg, unavailable: !cfg.SidecarEnabled,
		probeInterval: sidecarProbeInterval, failureThreshold: sidecarFailureThreshold,
	}
	if !cfg.SidecarEnabled {
		return supervisor, nil
	}
	entry := strings.TrimSpace(envOr("FASTTASK_SIDECAR_ENTRY", sidecarDefaultEntry))
	socket := strings.TrimSpace(cfg.SidecarSocket)
	if socket == "" {
		socket = filepath.Join(filepath.Dir(cfg.DatabasePath), "sidecar.sock")
	}
	// A deployment that starts the sidecar itself (doc/tech.md §21.4) supplies the secret both sides read;
	// otherwise one is generated for this process lifetime and handed to the child (§8.2).
	secret := strings.TrimSpace(cfg.SidecarSecret)
	if secret == "" {
		generated, err := newSidecarSecret()
		if err != nil {
			return nil, err
		}
		secret = generated
	}
	client, err := application.NewSidecarClient("unix://"+socket, secret, 10*time.Minute)
	if err != nil {
		return nil, err
	}
	supervisor.entry = entry
	supervisor.socket = socket
	supervisor.secret = secret
	supervisor.client = client
	supervisor.unavailable = true // until the first successful health check
	return supervisor, nil
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// newSidecarSecret generates the shared secret for one process lifetime (§8.2). It is never logged and
// never persisted: a restart rotates it, which is what makes a leaked one useless.
func newSidecarSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// Enabled reports whether the sidecar is configured at all. A nil supervisor — the shape a caller uses when
// the deployment has none — is simply disabled.
func (s *SidecarSupervisor) Enabled() bool { return s != nil && s.cfg.SidecarEnabled }

// Available reports whether the sidecar can currently drive a run. The WASM path never consults it (§14.10).
func (s *SidecarSupervisor) Available() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.SidecarEnabled && !s.unavailable
}

// Driver is the application-facing port. It reports unavailable when the supervisor has given up, so
// SubmitCommands refuses a sidecar run instead of queueing a job nobody will execute (§1.2).
func (s *SidecarSupervisor) Driver() application.SidecarDriver {
	if s == nil || !s.cfg.SidecarEnabled {
		return nil
	}
	return sidecarDriver{supervisor: s}
}

// sidecarDriver adapts the client to the application port, gating on the supervisor's availability.
type sidecarDriver struct{ supervisor *SidecarSupervisor }

func (d sidecarDriver) Healthy(ctx context.Context) bool {
	return d.supervisor.Available() && d.supervisor.client.Healthy(ctx)
}

func (d sidecarDriver) Drive(ctx context.Context, run application.SidecarRun) (application.SidecarResult, error) {
	if !d.Healthy(ctx) {
		return application.SidecarResult{}, errors.New("the sidecar is not available")
	}
	return d.supervisor.client.Drive(ctx, run)
}

func (d sidecarDriver) Cancel(ctx context.Context, runID string) error {
	if !d.supervisor.Available() {
		return nil
	}
	return d.supervisor.client.Cancel(ctx, runID)
}

// Start spawns the sidecar and waits for it to report healthy. A sidecar that does not become ready inside
// the configured timeout fails startup rather than leaving the server running with a fallback that cannot
// fall back (§8.4: "就绪超时应失败启动，不带病运行").
func (s *SidecarSupervisor) Start(ctx context.Context) error {
	if !s.cfg.SidecarEnabled {
		return nil
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	s.started = true
	runCtx, cancel := context.WithCancel(context.Background())
	s.stop = cancel
	s.done = make(chan struct{})
	s.mu.Unlock()

	if s.cfg.SidecarSpawn {
		if err := s.spawn(runCtx); err != nil {
			return err
		}
	}
	timeout := s.cfg.SidecarStartTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if s.client.Healthy(ctx) {
			s.mu.Lock()
			s.unavailable = false
			s.failures = 0
			s.mu.Unlock()
			if s.cfg.SidecarSpawn {
				go s.supervise(runCtx)
			} else {
				// Somebody else owns the process, so there is nothing to restart: readiness is still this
				// module's job (§8.4), and a host that disappears must stop being offered to runs.
				go s.probe(runCtx)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	return fmt.Errorf("sidecar did not become healthy within %s", timeout)
}

// webRoot is the absolute path of the web workspace whose node_modules the sidecar resolves libfx from.
func (s *SidecarSupervisor) webRoot() string {
	root := filepath.Dir(s.cfg.WebDist)
	if filepath.IsAbs(root) {
		return root
	}
	if absolute, err := filepath.Abs(root); err == nil {
		return absolute
	}
	return root
}

// threshold is how many consecutive failures make the sidecar unavailable.
func (s *SidecarSupervisor) threshold() int {
	if s.failureThreshold > 0 {
		return s.failureThreshold
	}
	return sidecarFailureThreshold
}

// recordFailure counts one crash and flips the supervisor to unavailable at the threshold (§8.4). It
// returns the failure count so the supervisor loop can log it.
func (s *SidecarSupervisor) recordFailure() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures++
	if s.failures >= s.threshold() {
		s.unavailable = true
	}
	return s.failures
}

// spawn starts one sidecar process. The socket is removed first: a stale file from a killed process would
// make bind() fail and look like a crash loop.
func (s *SidecarSupervisor) spawn(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.socket), 0o750); err != nil {
		return err
	}
	_ = os.Remove(s.socket)

	command := exec.CommandContext(ctx, s.cfg.SidecarNodePath, s.entry)
	command.Env = append(os.Environ(),
		"FASTTASK_SIDECAR_ENDPOINT=unix://"+s.socket,
		"FASTTASK_SIDECAR_SECRET="+s.secret,
		"FASTTASK_SERVER_ORIGIN="+strings.TrimRight(s.cfg.PublicURL, "/"),
		"FASTTASK_LIBFX_VERSION="+application.LibfxVersion,
		// The sidecar resolves libfx out of the web workspace's node_modules instead of keeping a second
		// copy of the 6 MB native addons (§8.2). The path is absolute: Node's module resolution is
		// relative to the process, not to this one's working directory.
		"FASTTASK_WEB_ROOT="+s.webRoot(),
	)
	// The sidecar is a fallback for a browser; it must not inherit a controlling terminal or keep the
	// server's stdout interleaved with its own.
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	s.mu.Lock()
	s.cmd = command
	s.mu.Unlock()
	if err := command.Start(); err != nil {
		return fmt.Errorf("start sidecar (%s %s): %w", s.cfg.SidecarNodePath, s.entry, err)
	}
	return nil
}

// supervise restarts the process when it exits unexpectedly, with a backoff, and gives up after a threshold
// so a broken fallback stops consuming resources — while leaving the WASM path completely alone (§8.4).
func (s *SidecarSupervisor) supervise(ctx context.Context) {
	defer func() {
		s.mu.Lock()
		done := s.done
		s.mu.Unlock()
		if done != nil {
			close(done)
		}
	}()
	backoff := time.Second
	for {
		s.mu.Lock()
		command := s.cmd
		s.mu.Unlock()
		if command == nil || command.Process == nil {
			return
		}
		err := command.Wait()
		if ctx.Err() != nil {
			return // a planned stop, not a crash
		}
		failures := s.recordFailure()
		if failures >= s.threshold() {
			fmt.Fprintf(os.Stderr, "sidecar exited %d times in a row (%v); marking sidecar mode unavailable — WASM mode is unaffected\n", failures, err)
			return
		}
		fmt.Fprintf(os.Stderr, "sidecar exited (%v); restarting in %s\n", err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, sidecarRestartBackoffMax)
		if spawnErr := s.spawn(ctx); spawnErr != nil {
			fmt.Fprintf(os.Stderr, "sidecar restart failed: %v\n", spawnErr)
		} else if s.client.Healthy(ctx) {
			s.mu.Lock()
			s.failures = 0
			s.unavailable = false
			s.mu.Unlock()
			backoff = time.Second
		}
	}
}

// probe watches an externally managed sidecar. It owns no process, so all it can do is keep the
// availability flag honest: unavailable after the same threshold of consecutive failures the supervisor
// uses, and available again as soon as the host answers, which is what a systemd restart looks like from
// here (doc/tech.md §21.4).
func (s *SidecarSupervisor) probe(ctx context.Context) {
	interval := s.probeInterval
	if interval <= 0 {
		interval = sidecarProbeInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		healthy := s.client.Healthy(probeCtx)
		cancel()
		s.mu.Lock()
		if healthy {
			s.failures = 0
			s.unavailable = false
		} else {
			s.failures++
			if s.failures >= s.threshold() {
				if !s.unavailable {
					fmt.Fprintf(os.Stderr, "sidecar stopped answering %d probes in a row; marking sidecar mode unavailable — WASM mode is unaffected\n", s.failures)
				}
				s.unavailable = true
			}
		}
		s.mu.Unlock()
	}
}

// Stop ends the process politely and then firmly (§8.4: SIGTERM, a grace period, SIGKILL). An externally
// managed sidecar is not this process's to signal: its own unit stops it, so only the probe ends here.
func (s *SidecarSupervisor) Stop(ctx context.Context) error {
	s.mu.Lock()
	command := s.cmd
	stop := s.stop
	done := s.done
	s.cmd = nil
	s.started = false
	s.unavailable = true
	s.mu.Unlock()
	if command == nil || command.Process == nil {
		if stop != nil {
			stop()
		}
		return nil
	}
	// The process group is signalled, so a sidecar that spawned children does not outlive it.
	_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
	if stop != nil {
		stop()
	}
	select {
	case <-done:
	case <-time.After(sidecarStopGrace):
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-doneOr(ctx, done, 2*time.Second)
	case <-ctx.Done():
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	if s.cfg.SidecarSpawn {
		// The socket file belongs to the process this one started; an external unit manages its own.
		_ = os.Remove(s.socket)
	}
	return nil
}

func doneOr(ctx context.Context, done chan struct{}, timeout time.Duration) <-chan struct{} {
	out := make(chan struct{})
	go func() {
		defer close(out)
		select {
		case <-done:
		case <-ctx.Done():
		case <-time.After(timeout):
		}
	}()
	return out
}

// Doctor reports what §8.4 asks the doctor command to check: the Node version, whether the native addon
// loaded, and whether the socket answers. A missing addon is a warning rather than an error — libfx falls
// back to Node's WASM backend, which on some Node versions needs --experimental-wasm-jspi (§8.2).
func (s *SidecarSupervisor) Doctor(ctx context.Context) ([]string, error) {
	if !s.cfg.SidecarEnabled {
		return []string{"sidecar=disabled (WASM mode only)"}, nil
	}
	lines := []string{}
	version, err := exec.CommandContext(ctx, s.cfg.SidecarNodePath, "--version").Output()
	if err != nil {
		return lines, fmt.Errorf("node not runnable at %q: %w", s.cfg.SidecarNodePath, err)
	}
	nodeVersion := strings.TrimSpace(string(version))
	lines = append(lines, "node="+nodeVersion)
	// libfx's native addon needs Node 20+ (§8.2). An unreadable version is not treated as a failure:
	// the health check below is the real gate.
	if major := nodeMajor(nodeVersion); major > 0 && major < 20 {
		lines = append(lines, "node-warning=Node.js 20 or newer is required")
	}
	if _, err := os.Stat(s.entry); err != nil {
		return lines, fmt.Errorf("sidecar entry %q is missing: build it with `npm run build:sidecar`", s.entry)
	}
	lines = append(lines, "sidecar-entry="+s.entry)
	if !s.client.Healthy(ctx) {
		return lines, errors.New("sidecar is not answering /healthz")
	}
	lines = append(lines, "sidecar-socket=unix://"+s.socket)
	if glibc, err := glibcVersion(ctx); err == nil {
		lines = append(lines, "glibc="+glibc)
		if !atLeast(glibc, "2.34") {
			lines = append(lines, "glibc-warning=the native addon needs glibc 2.34+; libfx will fall back to the Node WASM backend")
		}
	}
	return lines, nil
}

// nodeMajor reads the major version out of "v24.14.1".
func nodeMajor(version string) int {
	trimmed := strings.TrimPrefix(strings.TrimSpace(version), "v")
	major, _, _ := strings.Cut(trimmed, ".")
	var parsed int
	if _, err := fmt.Sscanf(major, "%d", &parsed); err != nil {
		return 0
	}
	return parsed
}

func glibcVersion(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "ldd", "--version").Output()
	if err != nil {
		return "", err
	}
	first := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	fields := strings.Fields(first)
	if len(fields) == 0 {
		return "", errors.New("unreadable ldd output")
	}
	return fields[len(fields)-1], nil
}

// atLeast compares dotted version strings well enough for a floor check.
func atLeast(have, want string) bool {
	haveParts := strings.Split(strings.TrimPrefix(have, "v"), ".")
	wantParts := strings.Split(want, ".")
	for i := 0; i < len(wantParts); i++ {
		var h, w int
		fmt.Sscanf(pad(haveParts, i), "%d", &h)
		fmt.Sscanf(wantParts[i], "%d", &w)
		if h != w {
			return h > w
		}
	}
	return true
}

func pad(parts []string, index int) string {
	if index < len(parts) {
		return parts[index]
	}
	return "0"
}

func registerSidecarLifecycle(lc fx.Lifecycle, supervisor *SidecarSupervisor, obs LifecycleObserver) {
	if !supervisor.Enabled() {
		return
	}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if err := supervisor.Start(ctx); err != nil {
				return err
			}
			obs.Started("sidecar")
			return nil
		},
		OnStop: func(ctx context.Context) error {
			obs.Stopped("sidecar")
			return supervisor.Stop(ctx)
		},
	})
}
