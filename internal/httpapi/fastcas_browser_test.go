package httpapi

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFastCASBrowserAgainstProvider(t *testing.T) {
	issuer, origin := os.Getenv("FASTCAS_CONTRACT_ISSUER"), os.Getenv("FASTCAS_CONTRACT_TASK_ORIGIN")
	if issuer == "" || origin == "" { t.Skip("run from FastCAS browser contract") }
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" { t.Fatal("invalid project origin") }
	cfg := defaultTestConfig(t)
	cfg.PublicURL = origin
	cfg.FastCASIssuer = issuer
	cfg.FastCASClientID = "task"
	cfg.FastCASClientSecret = "integration-client-secret-32-characters-long"
	cfg.FastCASRedirectURI = origin + "/api/v1/auth/fastcas/callback"
	cfg.FastCASLoopbackHTTP = true
	webDist, err := filepath.Abs("../../web/dist")
	if err != nil { t.Fatal(err) }
	cfg.WebDist = webDist
	api := newTestApiWithConfig(t, cfg)
	listener, err := net.Listen("tcp", parsed.Host)
	if err != nil { t.Fatal(err) }
	server := httptest.NewUnstartedServer(api.server.Engine)
	server.Listener = listener
	server.Start()
	defer server.Close()
	response, err := http.Get(origin + "/")
	if err != nil { t.Fatal(err) }
	response.Body.Close()
	if response.StatusCode != 200 { t.Fatalf("FastTask frontend returned %d", response.StatusCode) }
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	script, err := filepath.Abs("../../../FastCAS/web/tests/fasttask_contract.mjs")
	if err != nil { t.Fatal(err) }
	cmd := exec.CommandContext(ctx, "node", script)
	cmd.Env = append(os.Environ(), "FASTCAS_CONTRACT_ISSUER="+issuer, "FASTCAS_CONTRACT_TASK_ORIGIN="+origin)
	output, err := cmd.CombinedOutput()
	if err != nil { t.Fatalf("browser contract: %v\n%s", err, output) }
	t.Log(strings.TrimSpace(string(output)))
}
