// Package version exposes ldflags-injected build metadata (Version, Commit,
// Date) plus a runtime/debug fallback so `go run` and unbuilt binaries
// still emit something useful from `auditor version`.
package version

import "runtime/debug"

// These are overridden at build time via:
//
//	go build -ldflags "-X github.com/cloud-auditor/cloud-asset-auditor/internal/version.Version=v0.1.0 ..."
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"go_version"`
}

// Get returns build/version info, falling back to runtime/debug.BuildInfo
// (vcs.revision, vcs.time) when ldflags weren't injected — so `go run` and
// `go install` both produce something useful.
func Get() Info {
	info := Info{Version: Version, Commit: Commit, Date: Date}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	info.GoVersion = bi.GoVersion
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if info.Commit == "none" {
				info.Commit = s.Value
			}
		case "vcs.time":
			if info.Date == "unknown" {
				info.Date = s.Value
			}
		}
	}
	return info
}
