package main

import "runtime/debug"

// version is an explicit build identifier, settable at link time:
//
//	go build -ldflags "-X main.version=<sha>"
//
// It is empty by default; buildVersion falls back to the VCS revision the Go
// toolchain stamps into module builds (CI builds from a clean checkout carry
// exactly the commit SHA that also lands in the image's
// org.opencontainers.image.version label), and finally to "dev" when neither
// is available.
var version = ""

// buildVersion resolves the identifier of the build this binary came from.
func buildVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		var revision string
		var modified bool
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				revision = s.Value
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
		if revision != "" {
			if modified {
				// A local build with uncommitted changes is not the commit it
				// claims; mark it so a dashboard SHA is never mistaken for a
				// published build.
				return revision + "+dirty"
			}
			return revision
		}
	}
	return "dev"
}
