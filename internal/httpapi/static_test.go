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
