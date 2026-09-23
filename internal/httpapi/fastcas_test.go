package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	platformauth "github.com/FastR-D/FastTask/internal/platform/auth"
)

func TestFastCASAgainstProvider(t *testing.T) {
	issuer := os.Getenv("FASTCAS_CONTRACT_ISSUER")
	if issuer == "" {
		t.Skip("run from FastCAS with FASTCAS_PROJECT_CONTRACT=1")
	}
	cfg := defaultTestConfig(t)
	cfg.FastCASIssuer = issuer
	cfg.FastCASClientID = "task"
	cfg.FastCASClientSecret = "integration-client-secret-32-characters-long"
	cfg.FastCASRedirectURI = cfg.PublicURL + "/api/v1/auth/fastcas/callback"
	cfg.FastCASLoopbackHTTP = true
	api := newTestApiWithConfig(t, cfg)
	backchannel, err := url.Parse(os.Getenv("FASTCAS_CONTRACT_BACKCHANNEL"))
	if err != nil || backchannel.Host == "" {
		t.Fatal("backchannel URL unavailable", err)
	}
	listener, err := net.Listen("tcp", backchannel.Host)
	if err != nil {
		t.Fatal(err)
	}
	receiver := httptest.NewUnstartedServer(api.server.Engine)
	receiver.Listener = listener
	receiver.Start()
	defer receiver.Close()
	ctx := context.Background()
	_, access, refresh, err := api.server.auth.Login(ctx, "admin", "password-for-tests")
	if err != nil {
		t.Fatal(err)
	}
	api.access = access
	cookies := map[string]string{}
	call := func(method, path string, body any) *httptest.ResponseRecorder {
		cookieParts := []string{}
		for k, v := range cookies {
			cookieParts = append(cookieParts, k+"="+v)
		}
		r := api.do(t, method, path, body, map[string]string{"Origin": cfg.PublicURL, "Cookie": strings.Join(cookieParts, "; ")})
		for _, cookie := range r.Result().Cookies() {
			cookies[cookie.Name] = cookie.Value
		}
		return r
	}
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request := func(target string, form url.Values) (*http.Response, string) {
		t.Helper()
		method := "GET"
		var body io.Reader
		if form != nil {
			method = "POST"
			body = strings.NewReader(form.Encode())
		}
		req, _ := http.NewRequest(method, target, body)
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", issuer)
		}
		r, e := browser.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		raw, _ := io.ReadAll(r.Body)
		r.Body.Close()
		return r, string(raw)
	}
	complete := func(target string) string {
		t.Helper()
		r, _ := request(target, nil)
		if r.StatusCode != 302 {
			t.Fatalf("authorize %d", r.StatusCode)
		}
		login, _ := url.Parse(r.Header.Get("Location"))
		base, _ := url.Parse(issuer)
		login = base.ResolveReference(login)
		id := login.Query().Get("auth_request_id")
		_, body := request(login.String(), nil)
		csrf := func() string {
			parts := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(body)
			if len(parts) != 2 {
				t.Fatal("csrf absent")
			}
			return parts[1]
		}
		if strings.Contains(body, `name="password"`) {
			r, _ = request(issuer+"/login", url.Values{"csrf": {csrf()}, "auth_request_id": {id}, "email": {"alice@example.test"}, "password": {"correct horse battery staple"}, "action": {"login"}})
			u, _ := url.Parse(r.Header.Get("Location"))
			_, body = request(base.ResolveReference(u).String(), nil)
		}
		r, _ = request(issuer+"/login", url.Values{"csrf": {csrf()}, "auth_request_id": {id}, "action": {"approve"}})
		u, _ := url.Parse(r.Header.Get("Location"))
		r, _ = request(base.ResolveReference(u).String(), nil)
		if r.StatusCode != 302 {
			t.Fatalf("CAS callback %d", r.StatusCode)
		}
		return r.Header.Get("Location")
	}
	start := call("POST", "/api/v1/auth/fastcas/link", map[string]string{"password": "password-for-tests"})
	if start.Code != 200 {
		t.Fatal(start.Body.String())
	}
	var startBody struct {
		URL string `json:"url"`
	}
	decode(t, start, &startBody)
	callback, _ := url.Parse(complete(startBody.URL))
	// A normal SPA refresh rotates the session ID but keeps local account proof.
	_, api.access, _, err = api.server.auth.Refresh(ctx, refresh)
	if err != nil {
		t.Fatal(err)
	}
	if r := call("GET", callback.RequestURI(), nil); r.Code != 302 {
		t.Fatal(r.Body.String())
	}
	finish := call("POST", "/api/v1/auth/fastcas/complete", map[string]string{})
	if finish.Code != 200 {
		t.Fatal(finish.Body.String())
	}
	var linked platformauth.FastCASResult
	decode(t, finish, &linked)
	if !linked.Linked {
		t.Fatal("not linked")
	}
	localAccess := api.access
	login := call("GET", "/api/v1/auth/fastcas/login", nil)
	callback, _ = url.Parse(complete(login.Header().Get("Location")))
	call("GET", callback.RequestURI(), nil)
	finish = call("POST", "/api/v1/auth/fastcas/complete", map[string]string{})
	if finish.Code != 200 {
		t.Fatal(finish.Body.String())
	}
	var result platformauth.FastCASResult
	decode(t, finish, &result)
	if result.User.ID != api.user.ID || result.User.Role != api.user.Role {
		t.Fatal("local identity/role changed")
	}
	api.access = result.AccessToken
	if r := call("GET", "/api/v1/me", nil); r.Code != 200 {
		t.Fatal("CAS user session rejected")
	}
	if signal := os.Getenv("FASTCAS_CONTRACT_STATUS_SIGNAL"); signal != "" {
		if err = os.WriteFile(signal, []byte("ready"), 0600); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			if _, err = api.server.auth.Authenticate(result.AccessToken); err != nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err == nil {
			t.Fatal("FastCAS session survived identity status event")
		}
		if _, _, _, err = api.server.auth.Refresh(ctx, result.RefreshToken); err == nil {
			t.Fatal("FastCAS refresh survived identity status event")
		}
		if _, err = api.server.auth.Authenticate(localAccess); err != nil {
			t.Fatal("local session revoked by identity status event", err)
		}
		t.Log("FastTask identity status contract passed: signed event revoked CAS session, local session preserved")
		return
	}
	_, me := request(issuer+"/api/v1/me", nil)
	var current struct {
		CSRF string `json:"csrf"`
	}
	if err = json.Unmarshal([]byte(me), &current); err != nil || current.CSRF == "" {
		t.Fatal("provider csrf unavailable", err)
	}
	logoutRequest, _ := http.NewRequest("POST", issuer+"/api/v1/me/logout-all", strings.NewReader("{}"))
	logoutRequest.Header.Set("Origin", issuer)
	logoutRequest.Header.Set("X-CSRF-Token", current.CSRF)
	logoutRequest.Header.Set("Content-Type", "application/json")
	logoutResponse, err := browser.Do(logoutRequest)
	if err != nil {
		t.Fatal(err)
	}
	logoutResponse.Body.Close()
	if logoutResponse.StatusCode != 204 {
		t.Fatalf("provider logout: %d", logoutResponse.StatusCode)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err = api.server.auth.Authenticate(result.AccessToken); err != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err == nil {
		t.Fatal("FastCAS source session survived signed logout")
	}
	if _, _, _, err = api.server.auth.Refresh(ctx, result.RefreshToken); err == nil {
		t.Fatal("FastCAS refresh survived signed logout")
	}
	if _, err = api.server.auth.Authenticate(localAccess); err != nil {
		t.Fatal("local session revoked by global logout", err)
	}
	links, err := api.server.auth.FastCASLinks(ctx, platformauth.Principal{UserID: api.user.ID})
	if err != nil || len(links) != 1 {
		t.Fatal("link missing", err)
	}
	api.access = localAccess
	r := call("POST", "/api/v1/auth/fastcas/links/"+links[0].ID+"/revoke", map[string]string{"password": "password-for-tests"})
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	if _, err = api.server.auth.Authenticate(result.AccessToken); err == nil {
		t.Fatal("revoked CAS session accepted")
	}
	if _, err = api.server.auth.Authenticate(localAccess); err != nil {
		t.Fatal("local session was revoked", err)
	}
	t.Log("FastTask real-provider binding, refreshed local proof, CAS login, signed global logout and selective revocation passed")
}
