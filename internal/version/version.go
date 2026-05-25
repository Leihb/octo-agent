// Package version holds the build-time version metadata for the octo binary.
//
// The default values here describe an unreleased local build. Production
// binaries set Version and Commit via -ldflags at build time:
//
//	go build -ldflags "-X github.com/Leihb/octo/internal/version.Version=0.12.0 -X github.com/Leihb/octo/internal/version.Commit=$(git rev-parse --short HEAD)" ./cmd/octo
package version

// Version is the SemVer string for this build. Overridden via -ldflags.
//
// 0.12.0-dev is the placeholder for the in-progress Go rewrite of Octo —
// the Ruby line ended at v0.11.2-final-ruby.
var Version = "0.12.0-dev"

// Commit is the short git SHA for this build. Overridden via -ldflags.
// Empty in local `go build` invocations.
var Commit = ""

// String renders a human-friendly version line, e.g. "0.12.0-dev (abc1234)".
// The commit suffix is omitted when Commit is empty.
func String() string {
	if Commit == "" {
		return Version
	}
	return Version + " (" + Commit + ")"
}
