package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withUserRules points userRulesPath at a temp file containing body, and
// restores the original afterward.
func withUserRules(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "octorules.md")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := userRulesPath
	userRulesPath = func() string { return p }
	t.Cleanup(func() { userRulesPath = orig })
}

func TestCompose_UserLayerBetweenEnvAndProject(t *testing.T) {
	withUserRules(t, "USER_GLOBAL_RULE")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ProjectContextFile), []byte("PROJECT_RULE"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := Compose("SYSTEM_OVERRIDE", dir, "ENV_BLOCK", "")

	envIdx := strings.Index(out, "ENV_BLOCK")
	userIdx := strings.Index(out, "USER_GLOBAL_RULE")
	projIdx := strings.Index(out, "PROJECT_RULE")
	sysIdx := strings.Index(out, "SYSTEM_OVERRIDE")
	for name, idx := range map[string]int{"env": envIdx, "user": userIdx, "proj": projIdx, "sys": sysIdx} {
		if idx == -1 {
			t.Fatalf("%s layer missing:\n%s", name, out)
		}
	}
	// base < env < user < project < --system
	if !(envIdx < userIdx && userIdx < projIdx && projIdx < sysIdx) {
		t.Errorf("order wrong: env=%d user=%d proj=%d sys=%d", envIdx, userIdx, projIdx, sysIdx)
	}
	if !strings.Contains(out, "~/.octo/octorules.md") {
		t.Error("user layer should be labelled with its source path")
	}
}

func TestCompose_NoUserFile_NoUserLayer(t *testing.T) {
	// Point at a non-existent file.
	orig := userRulesPath
	userRulesPath = func() string { return filepath.Join(t.TempDir(), "absent.md") }
	t.Cleanup(func() { userRulesPath = orig })

	out := Compose("", t.TempDir(), "", "")
	if strings.Contains(out, "octorules.md") {
		t.Errorf("absent user file should add no layer:\n%s", out)
	}
	if strings.Contains(out, "---") {
		t.Errorf("base-only prompt should have no separators:\n%s", out)
	}
}

func TestInclude_InlinesRelativeFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "style.md"), []byte("STYLE_FROM_INCLUDE"), 0o644); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(dir, ProjectContextFile)
	if err := os.WriteFile(main, []byte("top line\n@style.md\nbottom line"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := readProjectContext(dir)
	for _, want := range []string{"top line", "STYLE_FROM_INCLUDE", "bottom line"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in expanded content:\n%s", want, got)
		}
	}
}

func TestInclude_NestedAndDepth(t *testing.T) {
	dir := t.TempDir()
	// a → b → c chain (depth 3, under the cap).
	must := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("c.md", "DEEP_C")
	must("b.md", "MID_B\n@c.md")
	must(ProjectContextFile, "TOP_A\n@b.md")

	got := readProjectContext(dir)
	for _, want := range []string{"TOP_A", "MID_B", "DEEP_C"} {
		if !strings.Contains(got, want) {
			t.Errorf("nested include lost %q:\n%s", want, got)
		}
	}
}

func TestInclude_MissingFileLeavesMarker(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ProjectContextFile), []byte("@nope.md"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readProjectContext(dir)
	if !strings.Contains(got, "missing include: nope.md") {
		t.Errorf("missing include should leave a marker, got:\n%s", got)
	}
}

func TestInclude_CycleIsBroken(t *testing.T) {
	dir := t.TempDir()
	// a includes b, b includes a → cycle.
	if err := os.WriteFile(filepath.Join(dir, "b.md"), []byte("B_BODY\n@"+ProjectContextFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ProjectContextFile), []byte("A_BODY\n@b.md"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readProjectContext(dir)
	if !strings.Contains(got, "A_BODY") || !strings.Contains(got, "B_BODY") {
		t.Errorf("both bodies should appear once:\n%s", got)
	}
	if !strings.Contains(got, "include cycle") {
		t.Errorf("cycle back to the root should be marked, got:\n%s", got)
	}
}

func TestInclude_DiamondAllowed(t *testing.T) {
	dir := t.TempDir()
	mk := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("shared.md", "SHARED")
	mk("left.md", "@shared.md")
	mk("right.md", "@shared.md")
	mk(ProjectContextFile, "@left.md\n@right.md")

	got := readProjectContext(dir)
	// SHARED reached via two sibling branches (diamond) is allowed, not a cycle.
	if strings.Contains(got, "include cycle") {
		t.Errorf("diamond include must not be flagged as a cycle:\n%s", got)
	}
	if n := strings.Count(got, "SHARED"); n != 2 {
		t.Errorf("SHARED should appear twice (once per branch), got %d:\n%s", n, got)
	}
}
