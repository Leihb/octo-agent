package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompose_BaseAlwaysPresent(t *testing.T) {
	out := Compose("", t.TempDir(), "", "", "", false) // empty user/env/skills, no .octorules
	if !strings.Contains(out, "octo") {
		t.Errorf("composed prompt should contain the base identity:\n%s", out)
	}
	if strings.TrimSpace(out) == "" {
		t.Error("base layer must never be empty")
	}
}

func TestCompose_LayersInOrder(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ProjectContextFile), []byte("PROJECT_RULE_X"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := Compose("USER_RULE_Y", dir, "ENV_BLOCK_Z", "SKILLS_MANIFEST_W", "", true)

	baseIdx := strings.Index(out, "octo")
	envIdx := strings.Index(out, "ENV_BLOCK_Z")
	skillsIdx := strings.Index(out, "SKILLS_MANIFEST_W")
	projIdx := strings.Index(out, "PROJECT_RULE_X")
	userIdx := strings.Index(out, "USER_RULE_Y")

	if baseIdx == -1 || envIdx == -1 || skillsIdx == -1 || projIdx == -1 || userIdx == -1 {
		t.Fatalf("all five layers should be present:\n%s", out)
	}
	// Order: base < env < skills < project < user.
	if !(baseIdx < envIdx && envIdx < skillsIdx && skillsIdx < projIdx && projIdx < userIdx) {
		t.Errorf("layer order wrong: base=%d env=%d skills=%d project=%d user=%d", baseIdx, envIdx, skillsIdx, projIdx, userIdx)
	}
	if !strings.Contains(out, ProjectContextFile) {
		t.Errorf("project layer should be labelled with the source file")
	}
}

func TestCompose_SkipsAbsentLayers(t *testing.T) {
	// Isolate from real ~/.octo/{soul,user,octorules}.md so the test runs the
	// same on a developer machine and on a fresh CI runner.
	useIdentityFiles(t, "", "")
	// No env, no skills, no .octorules, no user prompt → just the base, no separators.
	out := Compose("", t.TempDir(), "", "", "", false)
	if strings.Contains(out, "---") {
		t.Errorf("single-layer prompt should have no separator:\n%s", out)
	}
}

func TestReadProjectContext_MissingFileIsEmpty(t *testing.T) {
	if got := readProjectContext(t.TempDir()); got != "" {
		t.Errorf("missing .octorules should yield empty, got %q", got)
	}
	if got := readProjectContext(""); got != "" {
		t.Errorf("empty cwd should yield empty, got %q", got)
	}
}
