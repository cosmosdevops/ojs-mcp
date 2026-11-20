package main

import (
	"runtime/debug"
	"strings"
)

// buildVersion is set when building a release that has no version control
// information to read, such as the container image:
//
//	go build -ldflags "-X main.buildVersion=1.2.3"
var buildVersion string

// version is what -version prints and what the server reports to clients.
// Nothing in the code states it: it comes from the git tag being built.
var version = resolveVersion(buildVersion, moduleVersion())

// moduleVersion is the version Go recorded for this build. "go install
// …@v1.2.3" records the tag, and a build inside a git checkout records one
// worked out from the nearest tag.
func moduleVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return info.Main.Version
}

// resolveVersion picks the version to report: the one given at build time
// if there is one, else the one Go recorded, else "dev".
func resolveVersion(fromBuild, fromModule string) string {
	for _, v := range []string{fromBuild, fromModule} {
		v = strings.TrimPrefix(strings.TrimSpace(v), "v")
		if v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}
