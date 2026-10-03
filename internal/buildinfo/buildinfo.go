// Package buildinfo is the one place a running binary learns what it is. The release version, commit
// and build time are stamped at link time:
//
//	go build -ldflags "-X github.com/FastR-D/FastTask/internal/buildinfo.Version=1.2.0 \
//	  -X github.com/FastR-D/FastTask/internal/buildinfo.Commit=$(git rev-parse HEAD) \
//	  -X github.com/FastR-D/FastTask/internal/buildinfo.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
//
// An unstamped build falls back to what the Go toolchain recorded (the VCS revision of a `go build` in
// a checkout), and the Go version always comes from the runtime, so no file in the repository carries a
// second, drifting copy of the toolchain version.
package buildinfo

import (
	"runtime"
	"runtime/debug"
	"sync"
)

var (
	Version   = "0.1.0-dev"
	Commit    = ""
	BuildTime = ""
)

type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	Go        string `json:"go"`
	Modified  bool   `json:"modified,omitempty"`
}

var (
	once   sync.Once
	cached Info
)

func Get() Info {
	once.Do(func() {
		cached = Info{Version: Version, Commit: Commit, BuildTime: BuildTime, Go: runtime.Version()}
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, setting := range info.Settings {
				switch setting.Key {
				case "vcs.revision":
					if cached.Commit == "" {
						cached.Commit = setting.Value
					}
				case "vcs.time":
					if cached.BuildTime == "" {
						cached.BuildTime = setting.Value
					}
				case "vcs.modified":
					cached.Modified = setting.Value == "true"
				}
			}
		}
		if cached.Commit == "" {
			cached.Commit = "unknown"
		}
		if cached.BuildTime == "" {
			cached.BuildTime = "unknown"
		}
	})
	return cached
}
