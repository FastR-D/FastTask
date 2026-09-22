package httpapi

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStaticServesPWAFiles proves blocker #1 / §7 of pwa.md is fixed: the root
// PWA artifacts (sw.js, manifest.webmanifest, registerSW.js, hashed workbox
// chunks, icons) are served as real files with the correct Content-Type and
// no-store caching, instead of falling through to index.html — which would hand
// the browser HTML and break Service Worker registration and the manifest parse.
// The SPA fallback still serves index.html for unknown client routes, hashed
// /assets keep immutable caching, /api misses stay JSON 404, and a ".." path
// cannot escape the dist directory.
func TestStaticServesPWAFiles(t *testing.T) {
	dist := t.TempDir()
	mustWriteStatic(t, filepath.Join(dist, "index.html"), "<!doctype html><title>FastTask</title>")
	mustWriteStatic(t, filepath.Join(dist, "sw.js"), "console.log('service worker')")
	mustWriteStatic(t, filepath.Join(dist, "manifest.webmanifest"), `{"name":"FastTask","start_url":"/"}`)
	mustWriteStatic(t, filepath.Join(dist, "registerSW.js"), "/* registerSW */")
	mustWriteStatic(t, filepath.Join(dist, "workbox-9a9b3c.js"), "/* workbox runtime */")
	mustMkdirStatic(t, filepath.Join(dist, "icons"))
	mustWriteStatic(t, filepath.Join(dist, "icons", "icon-192.png"), "fake-png-bytes")
	mustMkdirStatic(t, filepath.Join(dist, "assets"))
	mustWriteStatic(t, filepath.Join(dist, "assets", "app-1a2b3c.js"), "/* app bundle */")

	cfg := defaultTestConfig(t)
	cfg.WebDist = dist
	api := newTestApiWithConfig(t, cfg)

	cases := []struct {
		path      string
		wantType  string
		wantCache string
		wantBody  string
	}{
		{"/sw.js", "text/javascript; charset=utf-8", "no-store", "console.log('service worker')"},
		{"/manifest.webmanifest", "application/manifest+json", "no-store", `{"name":"FastTask","start_url":"/"}`},
		{"/registerSW.js", "text/javascript; charset=utf-8", "no-store", "/* registerSW */"},
		{"/workbox-9a9b3c.js", "text/javascript; charset=utf-8", "no-store", "/* workbox runtime */"},
		{"/icons/icon-192.png", "image/png", "no-store", "fake-png-bytes"},
		{"/assets/app-1a2b3c.js", "text/javascript; charset=utf-8", "public, max-age=31536000, immutable", "/* app bundle */"},
		// SPA fallback: an unknown client route renders index.html.
		{"/goals/abc123", "text/html; charset=utf-8", "no-store", "<!doctype html><title>FastTask</title>"},
	}
	for _, tc := range cases {
		resp := api.do(t, http.MethodGet, tc.path, nil, nil)
		if resp.Code != http.StatusOK {
			t.Errorf("%s status=%d want 200 body=%s", tc.path, resp.Code, resp.Body.String())
			continue
		}
		if got := resp.Header().Get("Content-Type"); got != tc.wantType {
			t.Errorf("%s Content-Type=%q want %q", tc.path, got, tc.wantType)
		}
		if got := resp.Header().Get("Cache-Control"); got != tc.wantCache {
			t.Errorf("%s Cache-Control=%q want %q", tc.path, got, tc.wantCache)
		}
		if !strings.Contains(resp.Body.String(), tc.wantBody) {
			t.Errorf("%s body=%q want substring %q", tc.path, resp.Body.String(), tc.wantBody)
		}
	}

	// /api misses stay JSON 404, never the SPA shell.
	apiMiss := api.do(t, http.MethodGet, "/api/v1/does-not-exist", nil, nil)
	if apiMiss.Code != http.StatusNotFound {
		t.Errorf("/api miss status=%d want 404", apiMiss.Code)
	}
	if ct := apiMiss.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("/api miss Content-Type=%q want application/json", ct)
	}

	// A ".." traversal must not escape dist: path.Clean anchors it to a rooted
	// path that does not exist, so the SPA fallback is returned, never the
	// system file.
	traversal := api.do(t, http.MethodGet, "/../../etc/passwd", nil, nil)
	if strings.Contains(traversal.Body.String(), "root:") {
		t.Fatalf("path traversal escaped dist: %s", traversal.Body.String())
	}
}

func mustWriteStatic(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustMkdirStatic(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestStaticServesWasmPrecompressed covers doc/harness.md §9.2: the self-hosted libfx core must
// arrive as application/wasm — WebAssembly.instantiateStreaming refuses anything else — and a client
// that accepts brotli or gzip must be handed the precompressed sibling the build emits, because 2 MB
// is the difference between the agent starting and the user giving up.
func TestStaticServesWasmPrecompressed(t *testing.T) {
	dist := t.TempDir()
	mustWriteStatic(t, filepath.Join(dist, "index.html"), "<!doctype html><title>FastTask</title>")
	mustMkdirStatic(t, filepath.Join(dist, "assets"))
	mustWriteStatic(t, filepath.Join(dist, "fx-core.wasm"), "\x00asm-raw-bytes")
	mustWriteStatic(t, filepath.Join(dist, "fx-core.wasm.br"), "brotli-bytes")
	mustWriteStatic(t, filepath.Join(dist, "fx-core.wasm.gz"), "gzip-bytes")

	cfg := defaultTestConfig(t)
	cfg.WebDist = dist
	api := newTestApiWithConfig(t, cfg)

	cases := []struct {
		accept       string
		wantEncoding string
		wantBody     string
	}{
		{"br, gzip", "br", "brotli-bytes"},
		{"gzip, br", "br", "brotli-bytes"}, // our choice among what the client accepts: br is smaller
		{"gzip", "gzip", "gzip-bytes"},
		{"br;q=0, gzip", "gzip", "gzip-bytes"},
		{"identity", "", "\x00asm-raw-bytes"},
		{"", "", "\x00asm-raw-bytes"},
	}
	for _, tc := range cases {
		headers := map[string]string{}
		if tc.accept != "" {
			headers["Accept-Encoding"] = tc.accept
		}
		resp := api.do(t, http.MethodGet, "/fx-core.wasm", nil, headers)
		if resp.Code != http.StatusOK {
			t.Fatalf("Accept-Encoding=%q status=%d body=%s", tc.accept, resp.Code, resp.Body.String())
		}
		// The type is the ORIGINAL one: an encoding is a transport detail, and a browser that asked
		// for brotli still has to be told it received a wasm module.
		if got := resp.Header().Get("Content-Type"); got != "application/wasm" {
			t.Errorf("Accept-Encoding=%q Content-Type=%q, want application/wasm", tc.accept, got)
		}
		if got := resp.Header().Get("Content-Encoding"); got != tc.wantEncoding {
			t.Errorf("Accept-Encoding=%q Content-Encoding=%q, want %q", tc.accept, got, tc.wantEncoding)
		}
		if tc.wantEncoding != "" && resp.Header().Get("Vary") != "Accept-Encoding" {
			// Without Vary a shared cache hands a brotli body to a client that cannot decode it.
			t.Errorf("Accept-Encoding=%q is missing Vary: Accept-Encoding", tc.accept)
		}
		if resp.Body.String() != tc.wantBody {
			t.Errorf("Accept-Encoding=%q body=%q, want %q", tc.accept, resp.Body.String(), tc.wantBody)
		}
		if got := resp.Header().Get("Cache-Control"); got != "public, max-age=3600" {
			t.Errorf("Cache-Control=%q, want a revalidated hour (the name is not content-hashed)", got)
		}
	}

	// A file with no precompressed sibling is served as itself, whatever the client accepts.
	mustWriteStatic(t, filepath.Join(dist, "plain.txt"), "plain")
	plain := api.do(t, http.MethodGet, "/plain.txt", nil, map[string]string{"Accept-Encoding": "br, gzip"})
	if plain.Code != http.StatusOK || plain.Body.String() != "plain" {
		t.Fatalf("plain file=%d %q", plain.Code, plain.Body.String())
	}
	if got := plain.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("plain file Content-Encoding=%q, want none", got)
	}
}

// TestSecurityPolicyAllowsWasm covers the CSP half of §9: with script-src 'self' alone a browser
// refuses to instantiate WebAssembly, and the agent degrades to the sidecar for a reason nobody can
// see in the server logs.
func TestSecurityPolicyAllowsWasm(t *testing.T) {
	api := newTestAPI(t)
	resp := api.do(t, http.MethodGet, "/health/live", nil, nil)
	policy := resp.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "'wasm-unsafe-eval'") {
		t.Fatalf("CSP does not permit wasm compilation: %q", policy)
	}
	if strings.Contains(policy, "script-src 'self' 'unsafe-eval'") {
		t.Fatalf("CSP widened to unsafe-eval, which permits arbitrary JS eval: %q", policy)
	}
}
