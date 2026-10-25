package application

// These are the published FastResearch module entries. API endpoints and account
// credentials stay independently configured; navigation never forwards a Key.
var portalWorkspacePages = map[string]string{
	"fastwrite": "https://fastrd.io/write/",
	"fastread":  "https://fastrd.io/read/",
	"fastnews":  "https://47.110.133.67/news/",
	"fastppt":   "https://fastrd.io/fastppt/",
}

func workspaceNavigationURL(system, configured string) string {
	if configured != "" && validateIntegrationURL(configured) != nil {
		return ""
	}
	target := configured
	if target == "" {
		target = portalWorkspacePages[system]
	}
	return target
}
