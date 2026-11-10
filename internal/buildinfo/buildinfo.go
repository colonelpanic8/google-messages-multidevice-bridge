// Package buildinfo reports which build of the bridge is running.
package buildinfo

import "runtime/debug"

// Set at link time by the Nix build, which knows the flake revision; a plain
// `go build` inside a checkout falls back to the VCS stamp Go records.
var (
	Version = "dev"
	Commit  = ""
)

type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
}

func Get() Info {
	info := Info{Version: Version, Commit: Commit}
	if info.Commit != "" {
		return info
	}
	build, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	dirty := false
	for _, s := range build.Settings {
		switch s.Key {
		case "vcs.revision":
			info.Commit = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if dirty && info.Commit != "" {
		info.Commit += "-dirty"
	}
	return info
}
