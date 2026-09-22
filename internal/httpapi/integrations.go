package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

type integrationStatus struct {
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`
	Configured bool      `json:"configured"`
	Reachable  bool      `json:"reachable"`
	StatusCode int       `json:"status_code,omitempty"`
	LatencyMS  int64     `json:"latency_ms,omitempty"`
	Message    string    `json:"message"`
	CheckedAt  time.Time `json:"checked_at"`
}

type integrationStatusBody struct {
	Services []integrationStatus `json:"services"`
}

func (s integrationStatusRoutes) RegisterRoutes(api huma.API) {
	type input struct{}
	register(api, "integration-status", http.MethodGet, "/integrations/status", "Check configured FastResearch services", userSecurity(), func(ctx context.Context, input *input) (*itemResponse[integrationStatusBody], error) {
		if principal(ctx).Role != "admin" {
			return nil, huma.Error403Forbidden("administrator access required")
		}
		services := []integrationStatus{
			{Name: "fastinsight", Kind: "cli", Configured: false, Reachable: false, Message: "CLI runner is not configured", CheckedAt: time.Now().UTC()},
			{Name: "fastnews", Kind: "cli", Configured: false, Reachable: false, Message: "CLI runner is not configured", CheckedAt: time.Now().UTC()},
			s.probeHTTP(ctx, "fastread", s.cfg.FastReadURL, "/api/sys_check"),
			s.probeHTTP(ctx, "fastwrite", s.cfg.FastWriteURL, "/api/health"),
		}
		return &itemResponse[integrationStatusBody]{Body: integrationStatusBody{Services: services}}, nil
	})
}

func (s RouteDeps) probeHTTP(ctx context.Context, name, baseURL, healthPath string) integrationStatus {
	checkedAt := time.Now().UTC()
	result := integrationStatus{Name: name, Kind: "http", Configured: baseURL != "", CheckedAt: checkedAt}
	if baseURL == "" {
		result.Message = "not configured"
		return result
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		result.Message = "invalid configured URL"
		return result
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		result.Message = "invalid configured URL"
		return result
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + healthPath
	parsed.RawPath = ""
	probeURL := parsed.String()
	requestCtx, cancel := context.WithTimeout(ctx, s.cfg.IntegrationTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, probeURL, nil)
	if err != nil {
		result.Message = "probe request could not be created"
		return result
	}
	client := &http.Client{
		Timeout: s.cfg.IntegrationTimeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	started := time.Now()
	response, err := client.Do(request)
	result.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		result.Message = "unreachable"
		return result
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 32<<10))
	result.StatusCode = response.StatusCode
	result.Reachable = response.StatusCode >= 200 && response.StatusCode < 300 && readErr == nil && validHealthResponse(name, body)
	if result.Reachable {
		result.Message = "healthy"
	} else {
		result.Message = "unhealthy response"
	}
	return result
}

func validHealthResponse(name string, body []byte) bool {
	switch name {
	case "fastread":
		var response struct {
			Code *int `json:"code"`
		}
		return json.Unmarshal(body, &response) == nil && response.Code != nil && *response.Code == 0
	case "fastwrite":
		var response struct {
			Status string `json:"status"`
		}
		return json.Unmarshal(body, &response) == nil && response.Status == "ok"
	default:
		return false
	}
}
