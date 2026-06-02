package version

import (
	"strings"
	"testing"
)

func TestString_WithoutCommit(t *testing.T) {
	t.Cleanup(saveAndRestore(&Version, &Commit))
	Version = "1.2.3"
	Commit = ""
	if got, want := String(), "1.2.3"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestString_WithCommit(t *testing.T) {
	t.Cleanup(saveAndRestore(&Version, &Commit))
	Version = "1.2.3"
	Commit = "abc1234"
	if got, want := String(), "1.2.3 (abc1234)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestUserAgent(t *testing.T) {
	t.Cleanup(saveAndRestore(&Version, &Commit))
	Version = "0.12.0"
	Commit = "abc1234"
	got := UserAgent()
	if !strings.HasPrefix(got, "octo-agent/0.12.0 (") {
		t.Errorf("UserAgent() = %q, expected prefix octo-agent/0.12.0 (", got)
	}
	if !strings.HasSuffix(got, ")") {
		t.Errorf("UserAgent() = %q, expected suffix )", got)
	}
	// Must contain OS and arch separated by semicolon.
	if !strings.Contains(got, "; ") {
		t.Errorf("UserAgent() = %q, expected '; ' between OS and arch", got)
	}
}

func saveAndRestore(version, commit *string) func() {
	origV, origC := *version, *commit
	return func() {
		*version = origV
		*commit = origC
	}
}
