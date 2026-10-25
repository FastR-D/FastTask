package application

import "strings"

// These are the published FastResearch module entries. API endpoints and account
// credentials stay independently configured; navigation never forwards a Key.
var portalWorkspacePages = map[string]string{
	"fastwrite": "https://fastrd.io/write/",
	"fastread":  "https://fastrd.io/read/",
	"fastnews":  "https://47.110.133.67/news/",
	"fastppt":   "https://fastrd.io/fastppt/",
}

var portalWorkspaceEntries = map[string]string{
	"fastwrite": "fast-write", "fastread": "fast-read",
	"fastnews": "fast-news", "fastppt": "fast-ppt",
}

func workspaceNavigationURL(system, configured string) string {
	if configured != "" && validateIntegrationURL(configured) != nil {
		return ""
	}
	target := configured
	if target == "" {
		target = portalWorkspacePages[system]
	}
	// The hub owns login and chooses the destination from its configured catalog.
	// Custom deployments retain their exact address; there is no arbitrary next URL.
	if target != "" && strings.TrimRight(target, "/") == strings.TrimRight(portalWorkspacePages[system], "/") {
		return "https://fastrd.io/workspace?launch=" + portalWorkspaceEntries[system]
	}
	return target
}
