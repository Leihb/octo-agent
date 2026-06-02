package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// useIdentityFiles points soulPath/userProfilePath at temp files (written when
// content is non-empty, otherwise an absent path) and clears userRulesPath,
// isolating the test from any real ~/.octo files.
func useIdentityFiles(t *testing.T, soul, profile string) {
	t.Helper()
	dir := t.TempDir()
	origSoul, origProfile, origRules := soulPath, userProfilePath, userRulesPath
	t.Cleanup(func() { soulPath, userProfilePath, userRulesPath = origSoul, origProfile, origRules })

	userRulesPath = func() string { return filepath.Join(dir, "absent-octorules.md") }

	write := func(name, content string) func() string {
		if content == "" {
			return func() string { return filepath.Join(dir, "absent-"+name) }
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return func() string { return p }
	}
	soulPath = write("soul.md", soul)
	userProfilePath = write("user.md", profile)
}

func TestCompose_IdentityLayers(t *testing.T) {
	useIdentityFiles(t, "SOUL_PERSONA", "USER_PROFILE_X")
	out := Compose("", t.TempDir(), "ENV_Z", "SKILLS_M", "", true)

	baseIdx := strings.Index(out, "octo")
	soulIdx := strings.Index(out, "SOUL_PERSONA")
	envIdx := strings.Index(out, "ENV_Z")
	skillsIdx := strings.Index(out, "SKILLS_M")
	profileIdx := strings.Index(out, "USER_PROFILE_X")

	for name, idx := range map[string]int{"soul": soulIdx, "env": envIdx, "skills": skillsIdx, "profile": profileIdx} {
		if idx == -1 {
			t.Fatalf("%s layer missing:\n%s", name, out)
		}
	}
	// base < soul < env < skills < profile
	if !(baseIdx < soulIdx && soulIdx < envIdx && envIdx < skillsIdx && skillsIdx < profileIdx) {
		t.Errorf("order wrong: base=%d soul=%d env=%d skills=%d profile=%d", baseIdx, soulIdx, envIdx, skillsIdx, profileIdx)
	}
	if !strings.Contains(out, "~/.octo/soul.md") || !strings.Contains(out, "~/.octo/user.md") {
		t.Error("identity layers should be labelled with their source paths")
	}
}

func TestCompose_IdentityAbsentSkipped(t *testing.T) {
	useIdentityFiles(t, "", "") // neither file present
	out := Compose("", t.TempDir(), "", "", "", false)
	if strings.Contains(out, "soul.md") || strings.Contains(out, "user.md") {
		t.Errorf("absent identity files should add no layer:\n%s", out)
	}
	if strings.Contains(out, "---") {
		t.Errorf("base-only prompt should have no separator:\n%s", out)
	}
}

func TestCompose_ProfileBeforeRules(t *testing.T) {
	useIdentityFiles(t, "", "USER_PROFILE_X")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ProjectContextFile), []byte("PROJECT_RULE"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := Compose("", dir, "", "", "", false)
	profileIdx := strings.Index(out, "USER_PROFILE_X")
	projIdx := strings.Index(out, "PROJECT_RULE")
	if profileIdx == -1 || projIdx == -1 || profileIdx >= projIdx {
		t.Errorf("profile should precede project rules: profile=%d proj=%d\n%s", profileIdx, projIdx, out)
	}
}
