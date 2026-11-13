package integrationclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func outcomeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func TestReadingOutcomesUseSavedSourceTimesAndExcludeUndatedSummaryFromWeek(t *testing.T) {
	stamp := time.Date(2026, 11, 12, 4, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("unexpected mutation %s", r.Method)
			w.WriteHeader(405)
			return
		}
		if r.Header.Get("Cookie") != "fastread_session=reader-one" {
			t.Errorf("source authorization missing")
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/api/papers":
			outcomeJSON(w, map[string]any{"items": []any{map[string]any{"id": "paper-one", "title": "Real paper"}}, "total": 2, "limit": 5, "offset": 0})
		case "/api/papers/paper-one":
			outcomeJSON(w, map[string]any{"id": "paper-one", "title": "Real paper", "summary": "An old saved summary without a timestamp", "active_version": "v2"})
		case "/api/workspace-notes":
			if r.URL.Query().Get("scope_id") != "paper-one" {
				t.Errorf("wrong note scope")
			}
			outcomeJSON(w, map[string]any{"items": []any{map[string]any{"id": "note-one", "scope_id": "paper-one", "title": "Saved observation", "content": "Concrete saved note", "created": stamp.Add(-24 * time.Hour).Unix(), "updated": stamp.Unix()}}})
		case "/api/papers/paper-one/reports":
			outcomeJSON(w, map[string]any{"items": []any{map[string]any{"id": "report-one", "version_id": "v1", "created": stamp.Unix()}}})
		case "/api/papers/paper-one/annotations":
			if r.URL.Query().Get("version") != "v2" {
				t.Errorf("wrong annotation version")
			}
			outcomeJSON(w, map[string]any{"items": []any{map[string]any{"id": "annotation-one", "paper_id": "paper-one", "version_id": "v2", "page": 2, "quote": "original evidence", "note": "saved interpretation", "updated": stamp.Unix()}}})
		default:
			t.Errorf("unexpected source route %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	config := Config{System: "fastread", APIURL: server.URL, AuthKind: "cookie", Credential: "reader-one"}
	start, end := stamp.Add(-24*time.Hour), stamp.Add(24*time.Hour)
	page := (Client{}).Outcomes(context.Background(), config, &start, &end, 5, 0)
	if page.Status != "ready" || len(page.Items) != 3 || !page.HasMore || page.NextOffset != 1 {
		t.Fatalf("unexpected page %#v", page)
	}
	for _, item := range page.Items {
		if item.OccurredAt != stamp.Format(time.RFC3339) || item.SourceUpdatedAt != item.OccurredAt {
			t.Errorf("source timestamp was replaced: %#v", item)
		}
		if item.Kind == "report" && item.Status != "stale" {
			t.Error("old document report must be stale")
		}
		if item.Attribution == "" {
			t.Error("missing attribution boundary")
		}
	}
	all := (Client{}).Outcomes(context.Background(), config, nil, nil, 5, 0)
	if len(all.Items) != 4 {
		t.Fatalf("all-time should retain undated saved summary: %#v", all)
	}
	for _, item := range all.Items {
		if item.Kind == "material" && (item.OccurredAt != "" || item.Status != "time_unknown") {
			t.Errorf("invented summary date: %#v", item)
		}
	}
}

func TestOutcomeAuthenticationLossDiscardsPartialRecords(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/papers":
			outcomeJSON(w, map[string]any{"items": []any{map[string]any{"id": "p", "title": "P"}}, "total": 1, "limit": 5, "offset": 0})
		case "/api/papers/p":
			outcomeJSON(w, map[string]any{"id": "p", "title": "P", "summary": "previously readable", "active_version": "v"})
		default:
			w.WriteHeader(403)
		}
	}))
	defer server.Close()
	page := (Client{}).Outcomes(context.Background(), Config{System: "fastread", APIURL: server.URL, AuthKind: "cookie", Credential: "expired"}, nil, nil, 5, 0)
	if len(page.Items) != 0 || page.Diagnostic == nil || page.Diagnostic.HTTPStatus != 403 {
		t.Fatalf("permission loss leaked partial data: %#v", page)
	}
}

func TestReadingOutcomesRejectWrongPaperOwnership(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/papers":
			outcomeJSON(w, map[string]any{"items": []any{map[string]any{"id": "p", "title": "P"}}, "total": 1, "limit": 5, "offset": 0})
		case "/api/papers/p":
			outcomeJSON(w, map[string]any{"id": "p", "title": "P"})
		case "/api/workspace-notes":
			outcomeJSON(w, map[string]any{"items": []any{map[string]any{"id": "n", "scope_id": "another-paper", "content": "must not appear"}}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	page := (Client{}).Outcomes(context.Background(), Config{System: "fastread", APIURL: server.URL, PersonalInstance: true}, nil, nil, 5, 0)
	if len(page.Items) != 0 || page.Status != "partial" {
		t.Fatalf("cross-paper note escaped contract validation: %#v", page)
	}
}

func TestWritingOutcomesOnlyIncludeAcceptedChangesAndLabelAutomaticHistory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer writer-token" {
			t.Errorf("unexpected authorization/method")
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/api/auth/me":
			outcomeJSON(w, map[string]any{"id": "writer"})
		case "/api/projects":
			outcomeJSON(w, []any{map[string]any{"id": "project", "name": "Manuscript"}})
		case "/api/projects/project/history-page":
			outcomeJSON(w, map[string]any{"commits": []any{map[string]any{"oid": "abc", "message": "chapter draft", "createdAt": "2026-11-12T04:00:00Z", "source": "manual"}, map[string]any{"oid": "def", "message": "autosave", "createdAt": "2026-11-12T04:01:00Z", "source": "automatic"}}, "nextCursor": "more"})
		case "/api/projects/project/reviews":
			outcomeJSON(w, []any{map[string]any{"id": "review", "projectId": "project", "overallAssessment": "Coverage incomplete", "createdAt": "2026-11-12T05:00:00Z", "createdFromProjectVersion": 3, "coverage": map[string]int{"failed": 1}}})
		case "/api/projects/project/provenance":
			outcomeJSON(w, map[string]any{"project": map[string]string{"id": "project"}, "changeSets": []any{map[string]any{"id": "c1", "status": "accepted", "summary": "Accepted real change", "reviewFinishedAt": "2026-11-12T06:00:00Z", "appliedCheckpointOids": []string{"xyz"}}, map[string]any{"id": "c2", "status": "proposed", "summary": "not accepted"}, map[string]any{"id": "c3", "status": "rolled-back", "summary": "retracted"}}})
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	page := (Client{}).Outcomes(context.Background(), Config{System: "fastwrite", APIURL: server.URL, AuthKind: "bearer", Credential: "writer-token"}, nil, nil, 5, 0)
	if len(page.Items) != 4 || page.Status != "partial" {
		t.Fatalf("unexpected writing outcomes %#v", page)
	}
	kinds := map[string]string{}
	for _, item := range page.Items {
		kinds[item.Kind] = item.Status
		if strings.Contains(item.Summary, "not accepted") || strings.Contains(item.Summary, "retracted") {
			t.Error("unaccepted change became progress")
		}
		if item.URL != server.URL+"/projects/project" {
			t.Errorf("unexpected source link %s", item.URL)
		}
	}
	if kinds["activity"] != "automatic" || kinds["review"] != "partial" || kinds["step"] != "accepted" {
		t.Fatalf("lost provenance distinctions: %#v", kinds)
	}
}

func TestExternalOutcomesMissingConfigDoesNotInventData(t *testing.T) {
	page := (Client{}).Outcomes(context.Background(), Config{System: "fastread"}, nil, nil, 5, 0)
	if page.Status != "not_configured" || len(page.Items) != 0 || page.Diagnostic == nil {
		t.Fatalf("unexpected disconnected state %#v", page)
	}
}

func TestWritingOutcomesMergeAppliedCheckpointsIntoOneAcceptedStep(t *testing.T) {
	for _, status := range []string{"accepted", "partially-accepted", "rolled-back"} {
		t.Run(status, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/projects":
					outcomeJSON(w, []any{map[string]any{"id": "project", "name": "Manuscript"}})
				case "/api/projects/project/history-page":
					outcomeJSON(w, map[string]any{"commits": []any{
						map[string]any{"oid": "applied1", "message": "accepted paragraph", "createdAt": "2026-11-12T04:00:00Z", "source": "manual"},
						map[string]any{"oid": "applied2", "message": "accepted conclusion", "createdAt": "2026-11-12T04:01:00Z", "source": "automatic"},
						map[string]any{"oid": "independent", "message": "separate saved version", "createdAt": "2026-11-12T03:00:00Z", "source": "manual"},
					}, "nextCursor": nil})
				case "/api/projects/project/reviews":
					outcomeJSON(w, []any{})
				case "/api/projects/project/provenance":
					// No separate acceptance timestamp: use the latest linked
					// checkpoint time, never the current synchronization time.
					outcomeJSON(w, map[string]any{"project": map[string]string{"id": "project"}, "changeSets": []any{
						map[string]any{"id": "change", "status": status, "summary": "One reviewed writing change", "appliedCheckpointOids": []string{"applied1", "applied2"}},
					}})
				default:
					t.Errorf("unexpected route %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			page := (Client{}).Outcomes(context.Background(), Config{System: "fastwrite", APIURL: server.URL, PersonalInstance: true}, nil, nil, 5, 0)
			if status == "rolled-back" {
				if len(page.Items) != 3 {
					t.Fatalf("retain history but no accomplished step: %#v", page)
				}
				for _, item := range page.Items {
					if item.Kind == "step" || item.Version != "independent" && item.Status != "retracted" {
						t.Errorf("retracted change counted as progress: %#v", item)
					}
				}
				return
			}
			if len(page.Items) != 2 {
				t.Fatalf("accepted work duplicated as checkpoints: %#v", page)
			}
			steps, versions := 0, 0
			for _, item := range page.Items {
				if item.Kind == "step" {
					steps++
					if item.Version != "applied1,applied2" || item.Status != strings.ReplaceAll(status, "-", "_") || item.OccurredAt != "2026-11-12T04:01:00Z" || item.Summary != "One reviewed writing change" {
						t.Errorf("merged card lost source evidence: %#v", item)
					}
				} else {
					versions++
					if item.Version != "independent" {
						t.Errorf("wrong checkpoint retained: %#v", item)
					}
				}
			}
			if steps != 1 || versions != 1 {
				t.Fatalf("unexpected merged kinds: %#v", page.Items)
			}
		})
	}
}

func TestSourceTimeRejectsMissingOrInvalidValues(t *testing.T) {
	for _, raw := range []string{"null", `"yesterday"`, `""`, "-1", "1e100"} {
		if got := sourceTime(json.RawMessage(raw)); got != "" {
			t.Errorf("%s became %s", raw, got)
		}
	}
}
