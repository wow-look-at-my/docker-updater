// version is an explicit build identifier, settable at link time: go build -ldflags "-X main.version=<sha>" It is empty by default.

package main

import "runtime/debug"

// version is an explicit build identifier, settable at link time: go build -ldflags "-X main.version=<sha>" It is empty by default.
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
				// A local build with uncommitted changes is not the commit it claims.
				return revision + "+dirty"
			}
			return revision
		}
	}
	return "dev"
}
