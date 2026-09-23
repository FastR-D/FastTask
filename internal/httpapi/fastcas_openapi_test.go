package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestFastCASOpenAPICoversGinRoutes(t *testing.T) {
	api := newTestAPI(t)
	response := api.do(t, http.MethodGet, "/api/v1/openapi.json", nil, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("OpenAPI status %d", response.Code)
	}
	var document struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, route := range api.server.Engine.Routes() {
		if !strings.HasPrefix(route.Path, "/api/v1/auth/fastcas/") {
			continue
		}
		path := strings.TrimPrefix(route.Path, "/api/v1")
		path = strings.ReplaceAll(path, ":id", "{id}")
		operation := document.Paths[path][strings.ToLower(route.Method)]
		if operation.OperationID == "" {
			t.Errorf("missing OpenAPI operation for %s %s", route.Method, path)
		}
		seen++
	}
	if seen != 10 {
		t.Fatalf("expected 10 FastCAS routes, found %d", seen)
	}
}
