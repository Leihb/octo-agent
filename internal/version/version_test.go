package version

import "testing"

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

func saveAndRestore(version, commit *string) func() {
	origV, origC := *version, *commit
	return func() {
		*version = origV
		*commit = origC
	}
}
