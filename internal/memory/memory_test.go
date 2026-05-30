package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersion_ChangesOnSave(t *testing.T) {
	s := NewStoreAt(t.TempDir())

	// No memory yet → empty version, no error.
	if v, err := s.Version(); err != nil || v != "" {
		t.Fatalf("empty store Version() = (%q, %v), want (\"\", nil)", v, err)
	}

	if err := s.Save(Entry{Name: "a", Description: "first fact", Type: TypeReference}); err != nil {
		t.Fatal(err)
	}
	v1, err := s.Version()
	if err != nil || v1 == "" {
		t.Fatalf("after first Save: Version() = (%q, %v), want non-empty", v1, err)
	}

	// Same state → same version (stable read).
	if v, _ := s.Version(); v != v1 {
		t.Errorf("Version() not stable: %q vs %q", v, v1)
	}

	// A new entry changes the index → version moves.
	if err := s.Save(Entry{Name: "b", Description: "second fact", Type: TypeReference}); err != nil {
		t.Fatal(err)
	}
	if v2, _ := s.Version(); v2 == v1 {
		t.Errorf("Version() unchanged after adding an entry: %q", v2)
	}
}

func TestSave_GetRoundTrip(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	in := Entry{
		Name:        "tests-before-commit",
		Description: "Run tests before committing: user asked for this",
		Type:        TypeFeedback,
		Body:        "**Why:** user said so.\n**How to apply:** run `make test` first.",
	}
	if err := s.Save(in); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Get("tests-before-commit")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.Description != in.Description {
		t.Errorf("Description round-trip: %q (colon-bearing scalar mangled?)", got.Description)
	}
	if got.Type != TypeFeedback {
		t.Errorf("Type = %q", got.Type)
	}
	if got.Created == "" || got.LastVerified == "" {
		t.Errorf("Created/LastVerified should be stamped: %+v", got)
	}
	if !strings.Contains(got.Body, "How to apply") {
		t.Errorf("Body round-trip lost content: %q", got.Body)
	}
}

func TestSave_UnknownTypeDefaultsReference(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	if err := s.Save(Entry{Name: "n", Description: "d", Type: "bogus"}); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Get("n")
	if got.Type != TypeReference {
		t.Errorf("unknown type should default to reference, got %q", got.Type)
	}
}

func TestSave_OverwritesSameName(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	_ = s.Save(Entry{Name: "n", Description: "first", Type: TypeUser})
	_ = s.Save(Entry{Name: "n", Description: "second", Type: TypeUser})
	entries, _ := s.List()
	if len(entries) != 1 {
		t.Fatalf("same name should overwrite, got %d entries", len(entries))
	}
	if entries[0].Description != "second" {
		t.Errorf("Description = %q, want second", entries[0].Description)
	}
}

func TestList_SortedAndExcludesIndex(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	_ = s.Save(Entry{Name: "zebra", Description: "z", Type: TypeProject})
	_ = s.Save(Entry{Name: "alpha", Description: "a", Type: TypeProject})

	entries, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name != "alpha" || entries[1].Name != "zebra" {
		t.Fatalf("List should be sorted and exclude MEMORY.md: %+v", entries)
	}
	// MEMORY.md must exist but never appear as an entry.
	if _, err := os.Stat(filepath.Join(s.dir, indexFile)); err != nil {
		t.Errorf("index file missing: %v", err)
	}
}

func TestList_MissingDirIsEmpty(t *testing.T) {
	s := NewStoreAt(filepath.Join(t.TempDir(), "nope"))
	entries, err := s.List()
	if err != nil || entries != nil {
		t.Errorf("missing dir → nil,nil; got %v,%v", entries, err)
	}
}

func TestRenderInjection(t *testing.T) {
	s := NewStoreAt(t.TempDir())

	// Empty store → empty injection.
	if out, _ := s.RenderInjection(""); out != "" {
		t.Errorf("empty store injection = %q, want empty", out)
	}

	_ = s.Save(Entry{Name: "n", Description: "prefers Go", Type: TypeUser})
	out, err := s.RenderInjection("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "# Memory (from past sessions)") || !strings.Contains(out, "prefers Go") {
		t.Errorf("injection missing header/entry:\n%s", out)
	}

	// A consolidated summary is included; still-active (un-consolidated) entries
	// are ALSO shown beneath it so freshly remembered facts aren't hidden until
	// the next consolidation folds them in.
	if err := os.WriteFile(filepath.Join(s.dir, summaryFile), []byte("CONSOLIDATED SUMMARY"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ = s.RenderInjection("")
	if !strings.Contains(out, "CONSOLIDATED SUMMARY") || !strings.Contains(out, "prefers Go") {
		t.Errorf("injection should include both summary and active entries:\n%s", out)
	}
}

func TestSave_AutoSlugFromDescription(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	if err := s.Save(Entry{Description: "Run tests before committing!", Type: TypeFeedback}); err != nil {
		t.Fatal(err)
	}
	entries, _ := s.List()
	if len(entries) != 1 || entries[0].Name != "run-tests-before-committing" {
		t.Errorf("Save should auto-slug from description, got %+v", entries)
	}
}

func TestSave_RequiresDescription(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	if err := s.Save(Entry{Type: TypeUser}); err == nil {
		t.Error("expected error for missing description (no source for auto-slug)")
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Run tests before committing!": "run-tests-before-committing",
		"  Hello, World  ":             "hello-world",
		"用户偏好 Go":                      "go",
		"!!! ":                         "note-5cafe826",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestState_RoundTrip(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	if st := s.LoadState(); st.LastConsolidated != "" {
		t.Errorf("missing state should be zero: %+v", st)
	}
	in := State{LastConsolidated: "2026-05-28"}
	if err := s.SaveState(in); err != nil {
		t.Fatal(err)
	}
	got := s.LoadState()
	if got != in {
		t.Errorf("State round-trip: %+v, want %+v", got, in)
	}
}

func TestRenderInjection_CwdScoping(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	_ = s.Save(Entry{Name: "g", Description: "global pref", Type: TypeUser})
	_ = s.Save(Entry{Name: "a", Description: "proj A fact", Type: TypeProject, Cwd: "/proj/A"})
	_ = s.Save(Entry{Name: "b", Description: "proj B fact", Type: TypeProject, Cwd: "/proj/B"})

	// Active-entry scoping: global (cwd "") + current project only.
	out, _ := s.RenderInjection("/proj/A")
	if !strings.Contains(out, "global pref") || !strings.Contains(out, "proj A fact") {
		t.Errorf("injection for /proj/A should include global + A entries:\n%s", out)
	}
	if strings.Contains(out, "proj B fact") {
		t.Errorf("injection for /proj/A should NOT include B's entry:\n%s", out)
	}

	// Summary buckets: the matching project's summary is included, others aren't.
	_ = s.WriteSummary("/proj/A", "A SUMMARY")
	_ = s.WriteSummary("/proj/B", "B SUMMARY")
	out, _ = s.RenderInjection("/proj/A")
	if !strings.Contains(out, "A SUMMARY") || strings.Contains(out, "B SUMMARY") {
		t.Errorf("injection for /proj/A should include A's summary, not B's:\n%s", out)
	}
}

// requireGit skips the test when `git` isn't on PATH. The git-baseline path
// (auto-commit, ListArchived) degrades to no-ops without git; tests for those
// paths can't meaningfully run without it.
func requireGit(t *testing.T) {
	t.Helper()
	if !gitAvailable() {
		t.Skip("git not on PATH — skipping git-baseline test")
	}
}

func TestArchiveAll_DropsEntriesAndRebuildsIndex(t *testing.T) {
	requireGit(t)
	s := NewStoreAt(t.TempDir()).EnableGit()
	_ = s.Save(Entry{Description: "first", Type: TypeUser})
	_ = s.Save(Entry{Description: "second", Type: TypeFeedback})

	if err := s.ArchiveAll(); err != nil {
		t.Fatal(err)
	}
	// Active list is now empty.
	if active, _ := s.List(); len(active) != 0 {
		t.Errorf("after ArchiveAll, List should be empty: %+v", active)
	}
	// Archived list (recovered via git log) contains both.
	archived, err := s.ListArchived()
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 2 {
		t.Fatalf("expected 2 archived entries, got %d: %+v", len(archived), archived)
	}
	// Index was rebuilt (and now empty).
	b, err := os.ReadFile(filepath.Join(s.dir, indexFile))
	if err != nil {
		t.Fatalf("index missing after archive: %v", err)
	}
	if strings.Contains(string(b), "first") || strings.Contains(string(b), "second") {
		t.Errorf("index should not list archived entries:\n%s", string(b))
	}
}

func TestArchiveAll_PreservesArchivedContent(t *testing.T) {
	requireGit(t)
	s := NewStoreAt(t.TempDir()).EnableGit()
	_ = s.Save(Entry{Description: "a fact", Type: TypeProject, Body: "the body"})
	_ = s.ArchiveAll()

	archived, _ := s.ListArchived()
	if len(archived) != 1 || archived[0].Body != "the body" || archived[0].Description != "a fact" {
		t.Errorf("archived entry lost content: %+v", archived)
	}
}

func TestArchiveAll_KeepsLatestVersionAcrossRounds(t *testing.T) {
	requireGit(t)
	s := NewStoreAt(t.TempDir()).EnableGit()
	// First round: save and archive.
	_ = s.Save(Entry{Name: "n", Description: "v1", Type: TypeUser})
	_ = s.ArchiveAll()
	// Second round: same name, new content — Save then Archive again.
	_ = s.Save(Entry{Name: "n", Description: "v2", Type: TypeUser})
	if err := s.ArchiveAll(); err != nil {
		t.Fatal(err)
	}
	archived, _ := s.ListArchived()
	if len(archived) != 1 || archived[0].Description != "v2" {
		t.Errorf("archive should keep the latest version, got %+v", archived)
	}
}

func TestSave_AutoCommitsWhenGitEnabled(t *testing.T) {
	requireGit(t)
	s := NewStoreAt(t.TempDir()).EnableGit()
	_ = s.Save(Entry{Description: "first fact", Type: TypeUser})

	if !isGitRepo(s.dir) {
		t.Fatalf("Save should lazily init a git repo when git is enabled")
	}
	sha, err := gitHead(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if sha == "" {
		t.Errorf("HEAD should be non-empty after a successful Save+commit")
	}
}

func TestSave_NoGitWhenDisabled(t *testing.T) {
	// Default NewStoreAt has git off — Save should not create .git/.
	s := NewStoreAt(t.TempDir())
	_ = s.Save(Entry{Description: "first fact", Type: TypeUser})
	if isGitRepo(s.dir) {
		t.Errorf("Save should NOT create .git/ when git is disabled")
	}
}

func TestListArchived_EmptyWithoutGit(t *testing.T) {
	// Without git enabled, ListArchived returns nil — the older archive/
	// subdir no longer exists and there's no history to walk.
	s := NewStoreAt(t.TempDir())
	_ = s.Save(Entry{Description: "x", Type: TypeUser})
	_ = s.ArchiveAll()

	got, err := s.ListArchived()
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("ListArchived without git should be nil, got %+v", got)
	}
}

func TestReadSummary(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	if got := s.ReadSummary(""); got != "" {
		t.Errorf("missing summary = %q, want empty", got)
	}
	_ = s.WriteSummary("", "hello world")
	if got := s.ReadSummary(""); got != "hello world" {
		t.Errorf("ReadSummary = %q, want %q", got, "hello world")
	}
}

func TestWriteSummary_StampsV1Marker(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	if err := s.WriteSummary("", "body content"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(s.dir, summaryFile))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.HasPrefix(got, summaryMarker) {
		t.Errorf("summary file should start with %q, got:\n%s", summaryMarker, got)
	}
	// The body must still be there after the marker line.
	if !strings.Contains(got, "body content") {
		t.Errorf("summary body lost:\n%s", got)
	}
}

func TestReadSummary_HidesMarkerFromCallers(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	_ = s.WriteSummary("", "alpha\nbeta")
	got := s.ReadSummary("")
	if strings.Contains(got, summaryMarker) {
		t.Errorf("ReadSummary should strip the marker, got: %q", got)
	}
	if got != "alpha\nbeta" {
		t.Errorf("ReadSummary = %q, want %q", got, "alpha\nbeta")
	}
}

func TestReadSummary_AcceptsLegacyMarkerless(t *testing.T) {
	// Summaries written by the PR-#96 era code had no marker. They must still
	// read cleanly so the upgrade is non-breaking.
	s := NewStoreAt(t.TempDir())
	_ = s.ensureDir()
	if err := os.WriteFile(filepath.Join(s.dir, summaryFile), []byte("legacy body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := s.ReadSummary(""); got != "legacy body" {
		t.Errorf("legacy markerless summary should pass through, got %q", got)
	}
}

func TestRenderInjection_HidesMarker(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	_ = s.WriteSummary("", "CONSOLIDATED")
	out, err := s.RenderInjection("")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, summaryMarker) {
		t.Errorf("injection should not leak the protocol marker:\n%s", out)
	}
	if !strings.Contains(out, "CONSOLIDATED") {
		t.Errorf("injection missing summary body:\n%s", out)
	}
}

func TestExportNotes(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	if notes, _ := s.ExportNotes(); notes != "" {
		t.Errorf("empty store ExportNotes = %q, want empty", notes)
	}
	_ = s.Save(Entry{Description: "prefers Go", Type: TypeUser, Body: "User said so."})
	notes, _ := s.ExportNotes()
	if !strings.Contains(notes, "[user] prefers Go") || !strings.Contains(notes, "User said so") {
		t.Errorf("ExportNotes missing pieces:\n%s", notes)
	}
}

func TestReadEntry_MalformedSkipped(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	_ = s.ensureDir()
	// No frontmatter fence.
	if err := os.WriteFile(filepath.Join(s.dir, "bad.md"), []byte("just text"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = s.Save(Entry{Name: "good", Description: "d", Type: TypeUser})

	entries, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "good" {
		t.Errorf("malformed entry should be skipped, got %+v", entries)
	}
}

func TestEntryCwdRoundTrip(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	if err := s.Save(Entry{Name: "n", Description: "d", Type: TypeProject, Cwd: "/proj/X"}); err != nil {
		t.Fatal(err)
	}
	e, ok, err := s.Get("n")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if e.Cwd != "/proj/X" {
		t.Errorf("cwd lost in round-trip: %q", e.Cwd)
	}
	// An entry saved without a cwd has the field omitted (no `cwd:` line).
	_ = s.Save(Entry{Name: "g", Description: "d", Type: TypeUser})
	b, _ := os.ReadFile(filepath.Join(s.dir, "g.md"))
	if strings.Contains(string(b), "cwd:") {
		t.Errorf("global entry should omit the cwd frontmatter line:\n%s", b)
	}
}

func TestActiveNotesByCwd(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	_ = s.Save(Entry{Name: "g", Description: "global", Type: TypeUser})
	_ = s.Save(Entry{Name: "a1", Description: "alpha one", Type: TypeProject, Cwd: "/proj/A"})
	_ = s.Save(Entry{Name: "a2", Description: "alpha two", Type: TypeProject, Cwd: "/proj/A"})

	buckets, err := s.ActiveNotesByCwd()
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 2 {
		t.Fatalf("expected 2 buckets (global + /proj/A), got %d: %+v", len(buckets), buckets)
	}
	if !strings.Contains(buckets[""], "global") {
		t.Errorf("global bucket missing its entry: %q", buckets[""])
	}
	if !strings.Contains(buckets["/proj/A"], "alpha one") || !strings.Contains(buckets["/proj/A"], "alpha two") {
		t.Errorf("/proj/A bucket should hold both A entries: %q", buckets["/proj/A"])
	}
}

func TestArchiveCwd_OnlyTargetBucket(t *testing.T) {
	requireGit(t)
	s := NewStoreAt(t.TempDir()).EnableGit()
	_ = s.Save(Entry{Name: "g", Description: "global", Type: TypeUser})
	_ = s.Save(Entry{Name: "a", Description: "alpha", Type: TypeProject, Cwd: "/proj/A"})

	if err := s.ArchiveCwd("/proj/A"); err != nil {
		t.Fatal(err)
	}
	active, _ := s.List()
	if len(active) != 1 || active[0].Name != "g" {
		t.Errorf("ArchiveCwd should remove only the /proj/A entry, left: %+v", active)
	}
}

func TestSummaries_GlobalAndProject(t *testing.T) {
	s := NewStoreAt(t.TempDir())
	_ = s.WriteSummary("", "GLOBAL BODY")
	_ = s.WriteSummary("/proj/A", "PROJECT A BODY")

	buckets, err := s.Summaries()
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 2 {
		t.Fatalf("expected 2 summary buckets, got %d: %+v", len(buckets), buckets)
	}
	// Global sorts first; its cwd is empty.
	if buckets[0].Cwd != "" || buckets[0].Body != "GLOBAL BODY" {
		t.Errorf("first bucket should be global: %+v", buckets[0])
	}
	// The project bucket recovers its cwd from the embedded marker.
	if buckets[1].Cwd != "/proj/A" || buckets[1].Body != "PROJECT A BODY" {
		t.Errorf("project bucket should recover cwd from marker: %+v", buckets[1])
	}
}
