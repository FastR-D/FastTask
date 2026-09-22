package httpapi

import (
	"bytes"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

var updateOpenAPIGolden = flag.Bool("update-openapi-golden", false, "rewrite the OpenAPI golden baseline")

// TestOpenAPIMatchesGolden asserts the assembled OpenAPI document is byte-identical
// to the committed baseline. This is the wiring.md §7 step 7 acceptance criterion
// ("改造前后 GET /api/v1/openapi.json 的输出必须完全一致"): converting the central
// register() dispatcher into the "routes" value group must not alter the external
// contract. huma marshals paths and component schemas as sorted maps, so the
// document is independent of registrar ordering — the value group's
// nondeterministic order cannot perturb the output.
//
// Regenerate the baseline with -update-openapi-golden only after confirming an
// intentional contract change.
func TestOpenAPIMatchesGolden(t *testing.T) {
	api := newTestAPI(t)
	resp := api.do(t, http.MethodGet, "/api/v1/openapi.json", nil, nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("openapi status=%d body=%s", resp.Code, resp.Body.String())
	}
	got := resp.Body.Bytes()

	goldenPath := filepath.Join("testdata", "openapi.golden.json")
	if *updateOpenAPIGolden {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Skipf("rewrote %s (%d bytes)", goldenPath, len(got))
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden baseline (regenerate with -update-openapi-golden): %v", err)
	}
	if !bytes.Equal(want, got) {
		for i := 0; i < len(want) && i < len(got); i++ {
			if want[i] != got[i] {
				t.Fatalf("OpenAPI differs from golden at byte %d (golden %d bytes vs got %d bytes); the routes refactor changed the external contract", i, len(want), len(got))
			}
		}
		t.Fatalf("OpenAPI length differs from golden (golden %d bytes vs got %d bytes)", len(want), len(got))
	}
}
