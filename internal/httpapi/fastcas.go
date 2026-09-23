package httpapi

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"

	fastcas "github.com/FastR-D/FastCAS/sdk/go"
	"github.com/FastR-D/FastCAS/sdk/go/ginadapter"
	platformauth "github.com/FastR-D/FastTask/internal/platform/auth"
	"github.com/danielgtaylor/huma/v2"
	"github.com/gin-gonic/gin"
)

// Browser callback transport is deliberately separate from user/device/service
// API authentication. It never accepts CAS tokens at the local HS256 boundary.
func (s *Server) registerFastCAS() {
	group := s.Engine.Group("/api/v1/auth/fastcas")
	group.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 65536)
		c.Next()
	})
	configured := func(c *gin.Context) bool {
		if _, err := s.auth.FastCASClient(); err != nil {
			c.AbortWithStatusJSON(503, gin.H{"detail": "FastCAS is not configured or configuration is invalid"})
			return false
		}
		return true
	}
	sameOrigin := func(c *gin.Context) bool {
		if c.GetHeader("Origin") != strings.TrimRight(s.cfg.PublicURL, "/") {
			c.AbortWithStatusJSON(403, gin.H{"detail": "same-origin request required"})
			return false
		}
		return true
	}
	local := func(c *gin.Context) (platformauth.Principal, bool) {
		p, err := s.auth.Authenticate(c.GetHeader("Authorization"))
		if err != nil {
			c.AbortWithStatusJSON(401, gin.H{"detail": "local sign-in required"})
			return p, false
		}
		return p, true
	}
	group.GET("/status", func(c *gin.Context) {
		if s.cfg.FastCASIssuer == "" {
			c.JSON(200, gin.H{"enabled": false, "links": []any{}})
			return
		}
		p, ok := local(c)
		if !ok {
			return
		}
		links, err := s.auth.FastCASLinks(c.Request.Context(), p)
		if err != nil {
			c.JSON(503, gin.H{"detail": "account status unavailable"})
			return
		}
		c.JSON(200, gin.H{"enabled": true, "issuer": s.cfg.FastCASIssuer, "links": links})
	})
	group.GET("/available", func(c *gin.Context) { c.JSON(200, gin.H{"enabled": s.cfg.FastCASIssuer != ""}) })
	group.GET("/login", func(c *gin.Context) {
		if !configured(c) {
			return
		}
		binding := fastcas.RandomBinding()
		target, err := s.auth.BeginFastCAS(c.Request.Context(), nil, "", binding)
		if err != nil {
			c.JSON(503, gin.H{"detail": "FastCAS login unavailable"})
			return
		}
		s.fastcasCookie(c, "binding", binding, 300)
		c.Redirect(302, target)
	})
	group.POST("/link", func(c *gin.Context) {
		if !configured(c) || !sameOrigin(c) {
			return
		}
		p, ok := local(c)
		if !ok {
			return
		}
		var body struct {
			Password string `json:"password"`
		}
		if c.ShouldBindJSON(&body) != nil {
			c.JSON(400, gin.H{"detail": "password required"})
			return
		}
		binding := fastcas.RandomBinding()
		target, err := s.auth.BeginFastCAS(c.Request.Context(), &p, body.Password, binding)
		if err != nil {
			c.JSON(400, gin.H{"detail": "unable to authenticate account; verify local password and FastCAS availability"})
			return
		}
		s.fastcasCookie(c, "binding", binding, 300)
		c.JSON(200, gin.H{"url": target})
	})
	group.GET("/callback", func(c *gin.Context) {
		if !configured(c) {
			return
		}
		if len(c.Request.URL.RawQuery) > 4096 {
			c.AbortWithStatus(400)
			return
		}
		callback, err := url.Parse(s.cfg.FastCASRedirectURI)
		if err != nil {
			c.AbortWithStatus(500)
			return
		}
		callback.RawQuery = c.Request.URL.RawQuery
		// Keep the authorization code in an HttpOnly cookie. The SPA completes via
		// same-origin POST and presents its current local bearer for a linking flow.
		s.fastcasCookie(c, "callback", base64.RawURLEncoding.EncodeToString([]byte(callback.String())), 300)
		c.Redirect(302, strings.TrimRight(s.cfg.PublicURL, "/")+"/?fastcas=callback")
	})
	group.POST("/complete", func(c *gin.Context) {
		if !configured(c) || !sameOrigin(c) {
			return
		}
		binding, _ := c.Cookie("fasttask.fastcas.binding")
		encoded, _ := c.Cookie("fasttask.fastcas.callback")
		callback, err := base64.RawURLEncoding.DecodeString(encoded)
		s.fastcasCookie(c, "binding", "", -1)
		s.fastcasCookie(c, "callback", "", -1)
		if err != nil || binding == "" || len(callback) == 0 {
			c.JSON(401, gin.H{"detail": "login transaction missing"})
			return
		}
		var p *platformauth.Principal
		if current, err := s.auth.Authenticate(c.GetHeader("Authorization")); err == nil {
			p = &current
		}
		result, err := s.auth.CompleteFastCAS(c.Request.Context(), string(callback), binding, p)
		if err != nil {
			c.JSON(401, gin.H{"detail": "FastCAS login or account authentication failed; sign in locally to verify the binding"})
			return
		}
		c.JSON(200, result)
	})
	for _, action := range []string{"revoke", "reconcile"} {
		group.POST("/links/:id/"+action, func(c *gin.Context) {
			if !configured(c) || !sameOrigin(c) {
				return
			}
			p, ok := local(c)
			if !ok {
				return
			}
			var body struct {
				Password string `json:"password"`
			}
			if action == "revoke" && c.ShouldBindJSON(&body) != nil {
				c.JSON(400, gin.H{"detail": "password required"})
				return
			}
			link, err := s.auth.UpdateFastCASLink(c.Request.Context(), p, c.Param("id"), body.Password, action == "revoke")
			if err != nil {
				c.JSON(400, gin.H{"detail": "authentication update failed"})
				return
			}
			c.JSON(200, link)
		})
	}
	group.POST("/events", ginadapter.Events(s.auth.FastCASClient, s.auth.ApplyFastCASNotification))
	group.POST("/backchannel-logout", ginadapter.Logout(s.auth.FastCASClient, s.auth.ApplyFastCASLogout))
	s.registerFastCASOpenAPI()
}

// These transport routes use Gin so the provider callback can keep the code in
// an HttpOnly cookie. Document them alongside Huma's /api/v1 routes.
func (s *Server) registerFastCASOpenAPI() {
	api := s.API.OpenAPI()
	if api.Paths == nil {
		api.Paths = map[string]*huma.PathItem{}
	}
	const prefix = "/auth/fastcas"
	str := &huma.Schema{Type: "string"}
	jsonResponse := func(description string, schema *huma.Schema) *huma.Response {
		return &huma.Response{Description: description, Content: map[string]*huma.MediaType{"application/json": {Schema: schema}}}
	}
	object := func(required []string, properties map[string]*huma.Schema) *huma.Schema {
		return &huma.Schema{Type: "object", Required: required, Properties: properties}
	}
	passwordBody := &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{"application/json": {
		Schema: object([]string{"password"}, map[string]*huma.Schema{"password": str}),
	}}}
	link := object([]string{"id", "state", "version"}, map[string]*huma.Schema{
		"id": str, "state": str, "version": {Type: "integer"}, "subject": str,
	})
	bearer := []map[string][]string{{"userBearer": {}}}
	optionalBearer := []map[string][]string{{}, {"userBearer": {}}}
	api.Paths[prefix+"/available"] = &huma.PathItem{Get: &huma.Operation{
		OperationID: "fastcas-available", Tags: []string{"FastCAS"}, Summary: "Check whether optional FastCAS login is configured",
		Responses: map[string]*huma.Response{"200": jsonResponse("availability", object([]string{"enabled"}, map[string]*huma.Schema{"enabled": {Type: "boolean"}}))},
	}}
	api.Paths[prefix+"/status"] = &huma.PathItem{Get: &huma.Operation{
		OperationID: "fastcas-status", Tags: []string{"FastCAS"}, Summary: "List the current local user's FastCAS links",
		Description: "When FastCAS is disabled, returns enabled=false without requiring local login; when enabled, requires a local bearer token.", Security: optionalBearer,
		Responses: map[string]*huma.Response{"200": jsonResponse("current links", object([]string{"enabled", "links"}, map[string]*huma.Schema{
			"enabled": {Type: "boolean"}, "issuer": str, "links": {Type: "array", Items: link},
		})), "401": {Description: "local sign-in required"}, "503": {Description: "account status unavailable"}},
	}}
	api.Paths[prefix+"/login"] = &huma.PathItem{Get: &huma.Operation{
		OperationID: "fastcas-login", Tags: []string{"FastCAS"}, Summary: "Start optional FastCAS login",
		Description: "Sets a short-lived HttpOnly browser-binding cookie and redirects to the configured issuer. Does not create a local account.",
		Responses:   map[string]*huma.Response{"302": {Description: "redirect to FastCAS", Headers: map[string]*huma.Param{"Location": {Schema: str}}}, "503": {Description: "FastCAS unavailable"}},
	}}
	api.Paths[prefix+"/link"] = &huma.PathItem{Post: &huma.Operation{
		OperationID: "fastcas-link", Tags: []string{"FastCAS"}, Summary: "Prove the existing local account and begin linking",
		Description: "Requires a local bearer token, matching Origin and the current local password. The returned URL is opened in the browser.", Security: bearer, RequestBody: passwordBody,
		Responses: map[string]*huma.Response{"200": jsonResponse("authorization URL", object([]string{"url"}, map[string]*huma.Schema{"url": str})), "400": {Description: "invalid password or request"}, "401": {Description: "local sign-in required"}, "403": {Description: "same-origin request required"}, "503": {Description: "FastCAS unavailable"}},
	}}
	api.Paths[prefix+"/callback"] = &huma.PathItem{Get: &huma.Operation{
		OperationID: "fastcas-callback", Tags: []string{"FastCAS"}, Summary: "Receive the provider callback without exposing its code to the SPA",
		Description: "Stores the callback in a short-lived HttpOnly cookie, then redirects to the application. The browser calls /complete using the same cookie.",
		Responses:   map[string]*huma.Response{"302": {Description: "redirect to application", Headers: map[string]*huma.Param{"Location": {Schema: str}}}, "400": {Description: "callback query too long"}, "503": {Description: "FastCAS unavailable"}},
	}}
	api.Paths[prefix+"/complete"] = &huma.PathItem{Post: &huma.Operation{
		OperationID: "fastcas-complete", Tags: []string{"FastCAS"}, Summary: "Consume the one-time callback and issue a local session",
		Description: "Requires matching Origin and browser-binding/callback cookies. For an existing-account link, include the current local bearer token; login uses no bearer token. Returns FastTask tokens, never FastCAS tokens.", Security: optionalBearer,
		Responses: map[string]*huma.Response{"200": jsonResponse("local FastTask session or link result", object([]string{"user", "access_token", "refresh_token", "linked"}, map[string]*huma.Schema{
			"user": {Type: "object"}, "access_token": str, "refresh_token": str, "linked": {Type: "boolean"},
		})), "401": {Description: "missing, consumed, or invalid callback"}, "403": {Description: "same-origin request required"}, "503": {Description: "FastCAS unavailable"}},
	}}
	for _, action := range []string{"revoke", "reconcile"} {
		operation := &huma.Operation{
			OperationID: "fastcas-" + action + "-link", Tags: []string{"FastCAS"}, Summary: action + " an existing FastCAS link", Security: bearer,
			Parameters: []*huma.Param{{Name: "id", In: "path", Required: true, Schema: str}},
			Responses:  map[string]*huma.Response{"200": jsonResponse("updated link", link), "400": {Description: "authentication update failed"}, "401": {Description: "local sign-in required"}, "403": {Description: "same-origin request required"}, "503": {Description: "FastCAS unavailable"}},
		}
		if action == "revoke" {
			operation.Description = "Requires matching Origin and the current local password; never deletes the local account."
			operation.RequestBody = passwordBody
		} else {
			operation.Description = "Retries a prepared relationship after an interrupted activation; requires matching Origin."
		}
		api.Paths[prefix+"/links/{id}/"+action] = &huma.PathItem{Post: operation}
	}
	api.Paths[prefix+"/events"] = &huma.PathItem{Post: &huma.Operation{
		OperationID: "fastcas-events", Tags: []string{"FastCAS"}, Summary: "Receive a signed FastCAS account event",
		Description: "Server-to-server signed JWT body (maximum 64 KiB); verifies issuer, audience and signature, then atomically updates only FastCAS-source sessions.",
		RequestBody: &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{"application/jwt": {Schema: str}}},
		Responses:   map[string]*huma.Response{"204": {Description: "event committed or previously applied"}, "401": {Description: "invalid notification"}, "413": {Description: "notification too large"}, "415": {Description: "unsupported media type"}, "503": {Description: "provider or local commit unavailable"}},
	}}
	api.Paths[prefix+"/backchannel-logout"] = &huma.PathItem{Post: &huma.Operation{
		OperationID: "fastcas-backchannel-logout", Tags: []string{"FastCAS"}, Summary: "Receive a signed OIDC back-channel logout",
		Description: "Server-to-server form body with one logout_token (maximum 64 KiB); only FastCAS-source sessions are revoked.",
		RequestBody: &huma.RequestBody{Required: true, Content: map[string]*huma.MediaType{"application/x-www-form-urlencoded": {Schema: object([]string{"logout_token"}, map[string]*huma.Schema{"logout_token": str})}}},
		Responses:   map[string]*huma.Response{"204": {Description: "logout committed or previously applied"}, "400": {Description: "invalid form"}, "401": {Description: "invalid logout token"}, "413": {Description: "notification too large"}, "415": {Description: "unsupported media type"}, "503": {Description: "provider or local commit unavailable"}},
	}}
}
func (s *Server) fastcasCookie(c *gin.Context, name, value string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{Name: "fasttask.fastcas." + name, Value: value, Path: "/api/v1/auth/fastcas", HttpOnly: true, Secure: strings.HasPrefix(s.cfg.PublicURL, "https://"), SameSite: http.SameSiteLaxMode, MaxAge: maxAge})
}
