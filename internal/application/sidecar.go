package application

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// The sidecar client (doc/harness.md §8.3).
//
// The sidecar is a fallback host for browsers without JSPI, and from the application's point of view it is
// one method: drive this run. Everything else about it — the process, the socket, the restarts — belongs to
// the composition root (§8.4).
//
// Two rules shape this client. It talks to loopback only, because a host that can drive a run holds a
// capability token and must never be reachable from outside the machine (§8.2). And it authenticates with a
// secret generated at startup, so a stray local process cannot hand the server a transcript.

// SidecarClient drives runs in the Node sidecar over HTTP.
type SidecarClient struct {
	// endpoint is either a unix socket path (unix://…) or a loopback TCP address (http://127.0.0.1:port).
	endpoint string
	secret   string
	client   *http.Client
	healthy  func(ctx context.Context) bool
}

// NewSidecarClient builds a client for a sidecar listening on endpoint. A unix socket is preferred (§8.2);
// a TCP address is accepted only when it is loopback, because anything else would put a run capability on a
// reachable interface.
func NewSidecarClient(endpoint, secret string, timeout time.Duration) (*SidecarClient, error) {
	normalized := strings.TrimSpace(endpoint)
	if normalized == "" {
		return nil, fmt.Errorf("sidecar endpoint is required")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if strings.HasPrefix(normalized, "unix://") {
		socket := strings.TrimPrefix(normalized, "unix://")
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socket)
		}
		// The URL host is a placeholder; the dialer ignores it and connects to the socket.
		normalized = "http://sidecar"
	} else if !isLoopbackHTTP(normalized) {
		return nil, fmt.Errorf("sidecar endpoint %q must be a unix socket or a loopback address", endpoint)
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &SidecarClient{
		endpoint: strings.TrimRight(normalized, "/"),
		secret:   secret,
		client:   &http.Client{Timeout: timeout, Transport: transport},
	}, nil
}

// isLoopbackHTTP reports whether an HTTP endpoint is bound to this machine only.
func isLoopbackHTTP(endpoint string) bool {
	if !strings.HasPrefix(endpoint, "http://") {
		return false
	}
	host := strings.TrimPrefix(endpoint, "http://")
	host, _, _ = strings.Cut(host, "/")
	host, _, err := net.SplitHostPort(host)
	if err != nil {
		// No port is not a usable endpoint anyway; let the caller's health check fail.
		host = strings.TrimPrefix(endpoint, "http://")
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

// SidecarHealth is what GET /healthz reports (§8.3).
type SidecarHealth struct {
	OK           bool   `json:"ok"`
	NodeVersion  string `json:"node_version"`
	NativeAddon  bool   `json:"native_addon"`
	LibfxVersion string `json:"libfx_version"`
	RequiresJSPI bool   `json:"requires_jspi_flag,omitempty"`
	Detail       string `json:"detail,omitempty"`
}

// Healthy reports whether the sidecar can accept a run right now. It is cheap and short-timeout on
// purpose: SubmitCommands calls it before queueing a sidecar run, and §1.2 wants a refusal rather than a
// job nobody will ever execute.
func (c *SidecarClient) Healthy(ctx context.Context) bool {
	if c == nil {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, c.endpoint+"/healthz", nil)
	if err != nil {
		return false
	}
	c.authorize(request)
	response, err := c.client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	var health SidecarHealth
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&health); err != nil {
		return false
	}
	return health.OK
}

// Drive hands one run to the sidecar and waits for its report (§8.3). The response carries no transcript:
// the model proxy already wrote it, which is what makes the two hosts equivalent (§14.3).
func (c *SidecarClient) Drive(ctx context.Context, run SidecarRun) (SidecarResult, error) {
	if c == nil {
		return SidecarResult{}, fmt.Errorf("no sidecar is configured")
	}
	// The field names are the sidecar's SidecarRunRequest; the two sides are checked against each other by
	// TestSidecarRunPayloadMatchesHost, because a rename here fails only at runtime, in a process that the
	// Go test suite cannot see.
	payload, err := json.Marshal(map[string]any{
		"run_id":        run.RunID,
		"harness_token": run.HarnessToken,
		"prompt":        run.Prompt,
		"checkpoint":    run.Checkpoint,
		"libfx_version": run.LibfxVersion,
		"model":         run.Model,
		"instructions":  run.Instructions,
		"thread_id":     run.ThreadID,
	})
	if err != nil {
		return SidecarResult{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/run", bytes.NewReader(payload))
	if err != nil {
		return SidecarResult{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	c.authorize(request)
	response, err := c.client.Do(request)
	if err != nil {
		return SidecarResult{}, fmt.Errorf("sidecar call: %w", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return SidecarResult{}, fmt.Errorf("sidecar returned %d: %s", response.StatusCode, truncateForLog(string(body)))
	}
	var result struct {
		StopReason   string         `json:"stop_reason"`
		Usage        map[string]any `json:"usage"`
		Checkpoint   []byte         `json:"checkpoint"`
		LibfxVersion string         `json:"libfx_version"`
		ErrorMessage string         `json:"error_message"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return SidecarResult{}, fmt.Errorf("sidecar response: %w", err)
	}
	return SidecarResult{
		StopReason: result.StopReason, Usage: result.Usage, Checkpoint: result.Checkpoint,
		LibfxVersion: result.LibfxVersion, ErrorMessage: result.ErrorMessage,
	}, nil
}

// Cancel asks the sidecar to stop a run (§5.2: the third channel, for the host that is not a browser).
func (c *SidecarClient) Cancel(ctx context.Context, runID string) error {
	if c == nil {
		return nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/run/"+runID+"/cancel", nil)
	if err != nil {
		return err
	}
	c.authorize(request)
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	return nil
}

// authorize sets the shared secret. It is a bearer token on a loopback connection, generated at startup
// and never logged (§8.2).
func (c *SidecarClient) authorize(request *http.Request) {
	if c.secret != "" {
		request.Header.Set("Authorization", "Bearer "+c.secret)
	}
}

func truncateForLog(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 300 {
		return value
	}
	return value[:300] + "…"
}
