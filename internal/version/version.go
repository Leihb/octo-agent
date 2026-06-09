// Package version holds the build-time version metadata for the octo binary.
//
// The default values here describe an unreleased local build. Production
// binaries set Version and Commit via -ldflags at build time:
//
//	go build -ldflags "-X github.com/Leihb/octo-agent/internal/version.Version=0.1.0 -X github.com/Leihb/octo-agent/internal/version.Commit=$(git rev-parse --short HEAD)" ./cmd/octo
package version

import (
	"runtime"
)

// Version is the SemVer string for this build. Overridden via -ldflags.
var Version = "0.17.0"

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

// UserAgent returns the standard User-Agent string for octo-agent HTTP requests.
// Format follows the convention used by Claude Code:
//
//	octo-agent/<version> (<os>; <arch>)
//
// Examples:
//
//	octo-agent/0.12.0 (darwin; arm64)
//	octo-agent/0.12.0 (linux; amd64)
func UserAgent() string {
	return "octo-agent/" + Version + " (" + runtime.GOOS + "; " + runtime.GOARCH + ")"
}
