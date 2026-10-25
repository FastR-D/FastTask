package application

import "testing"

func TestWorkspaceNavigationUsesPublishedPortalAndKeepsCustomSites(t *testing.T) {
	for system, want := range portalWorkspacePages {
		if got := workspaceNavigationURL(system, ""); got != want {
			t.Fatalf("%s default = %q, want %q", system, got, want)
		}
		if got := workspaceNavigationURL(system, portalWorkspacePages[system]); got != want {
			t.Fatalf("%s published page overwritten: %q", system, got)
		}
	}
	for _, system := range []string{"fastlab", "unknown"} {
		if got := workspaceNavigationURL(system, ""); got != "" {
			t.Fatalf("unpublished module %s has a fabricated destination: %q", system, got)
		}
	}
	for _, value := range []string{"https://tools.example.org/editor", "http://127.0.0.1:8080/lab"} {
		if got := workspaceNavigationURL("fastwrite", value); got != value {
			t.Fatalf("custom deployment overwritten: %q", got)
		}
	}
	for _, value := range []string{"javascript:alert(1)", "https://key@fastrd.io/write/", "https://fastrd.io/write/?key=secret", "https://fastrd.io/write/#secret"} {
		if got := workspaceNavigationURL("fastwrite", value); got != "" {
			t.Fatalf("unsafe destination accepted: %q", got)
		}
	}
}
