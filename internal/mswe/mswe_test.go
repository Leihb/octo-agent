package mswe

import (
	"bytes"
	"strings"
	"testing"
)

func TestLoadInstances_FilterAndLimit(t *testing.T) {
	data := strings.Join([]string{
		`{"org":"a","repo":"r1","number":1,"language":"Go"}`,
		`{"org":"b","repo":"r2","number":2,"language":"Rust"}`,
		`{"org":"c","repo":"r3","number":3,"language":"go"}`,
		``, // blank line ignored
		`{"org":"d","repo":"r4","number":4,"language":"Go"}`,
	}, "\n")

	got, err := LoadInstances(strings.NewReader(data), "go", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("limit not honored: got %d", len(got))
	}
	if got[0].ID() != "a/r1#1" || got[1].ID() != "c/r3#3" {
		t.Errorf("Go filter (case-insensitive) wrong: %s, %s", got[0].ID(), got[1].ID())
	}
}

func TestInstance_BaseCommit_FlatAndNested(t *testing.T) {
	flat, _ := LoadInstances(strings.NewReader(`{"base_commit":"abc123"}`), "", 0)
	if flat[0].BaseCommit() != "abc123" {
		t.Errorf("flat base_commit = %q", flat[0].BaseCommit())
	}
	nested, _ := LoadInstances(strings.NewReader(`{"base":{"sha":"def456"}}`), "", 0)
	if nested[0].BaseCommit() != "def456" {
		t.Errorf("nested base.sha = %q", nested[0].BaseCommit())
	}
}

func TestInstance_ProblemStatement_FlatAndResolvedIssues(t *testing.T) {
	flat, _ := LoadInstances(strings.NewReader(`{"problem_statement":"fix the bug"}`), "", 0)
	if flat[0].ProblemStatement() != "fix the bug" {
		t.Errorf("flat problem = %q", flat[0].ProblemStatement())
	}
	fallback, _ := LoadInstances(strings.NewReader(
		`{"resolved_issues":[{"title":"Crash on nil","body":"It panics when x is nil."}]}`), "", 0)
	ps := fallback[0].ProblemStatement()
	if !strings.Contains(ps, "Crash on nil") || !strings.Contains(ps, "panics when x is nil") {
		t.Errorf("resolved_issues fallback lost content: %q", ps)
	}
}

func TestInstance_CloneURL(t *testing.T) {
	got, _ := LoadInstances(strings.NewReader(`{"org":"golang","repo":"go","number":7}`), "", 0)
	if u := got[0].CloneURL(); u != "https://github.com/golang/go.git" {
		t.Errorf("CloneURL = %q", u)
	}
}

func TestScopeFixPatch_StripsTestFiles(t *testing.T) {
	diff := "diff --git a/foo.go b/foo.go\n" +
		"--- a/foo.go\n+++ b/foo.go\n@@ -1 +1 @@\n-old\n+new\n" +
		"diff --git a/foo_test.go b/foo_test.go\n" +
		"--- a/foo_test.go\n+++ b/foo_test.go\n@@ -1 +1 @@\n-oldtest\n+newtest\n"

	got := ScopeFixPatch(diff)
	if !strings.Contains(got, "a/foo.go") || !strings.Contains(got, "+new") {
		t.Errorf("source change dropped:\n%s", got)
	}
	if strings.Contains(got, "foo_test.go") || strings.Contains(got, "newtest") {
		t.Errorf("test-file change should be stripped:\n%s", got)
	}
}

func TestScopeFixPatch_KeepsAllWhenNoTests(t *testing.T) {
	diff := "diff --git a/a.go b/a.go\n@@ -1 +1 @@\n-x\n+y\n"
	if got := ScopeFixPatch(diff); got != diff {
		t.Errorf("non-test diff should pass through unchanged:\n%s", got)
	}
}

func TestScopeFixPatch_StripsBuildArtifacts(t *testing.T) {
	// `go test` can leave a compiled <pkg>.test binary that `git add -A` sweeps
	// in; it must not reach the fix patch.
	diff := "diff --git a/pkg/cmd/api/api.go b/pkg/cmd/api/api.go\n--- a/pkg/cmd/api/api.go\n+++ b/pkg/cmd/api/api.go\n@@ -1 +1 @@\n-old\n+new\n" +
		"diff --git a/api.test b/api.test\nnew file mode 100755\nindex 0000000..abc1234\nBinary files /dev/null and b/api.test differ\n"
	got := ScopeFixPatch(diff)
	if !strings.Contains(got, "a/pkg/cmd/api/api.go") || !strings.Contains(got, "+new") {
		t.Errorf("source change dropped:\n%s", got)
	}
	if strings.Contains(got, "api.test") {
		t.Errorf(".test build artifact should be stripped:\n%s", got)
	}
}

func TestScopeFixPatch_StripsBinarySection(t *testing.T) {
	// A binary-file diff (no textual hunks) is never part of a source fix.
	diff := "diff --git a/main.go b/main.go\n@@ -1 +1 @@\n-x\n+y\n" +
		"diff --git a/bin/tool b/bin/tool\nindex 1111111..2222222 100755\nBinary files a/bin/tool and b/bin/tool differ\n"
	got := ScopeFixPatch(diff)
	if !strings.Contains(got, "a/main.go") {
		t.Errorf("source dropped:\n%s", got)
	}
	if strings.Contains(got, "bin/tool") {
		t.Errorf("binary section should be stripped:\n%s", got)
	}
}

func TestWritePredictions_JSONL(t *testing.T) {
	var b bytes.Buffer
	err := WritePredictions(&b, []Prediction{
		{Org: "a", Repo: "r", Number: 1, FixPatch: "diff1"},
		{Org: "b", Repo: "s", Number: 2, FixPatch: "diff2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 JSONL lines, got %d", len(lines))
	}
	if !strings.Contains(lines[0], `"fix_patch":"diff1"`) || !strings.Contains(lines[0], `"number":1`) {
		t.Errorf("line 0 wrong: %s", lines[0])
	}
}

func TestParseReport_ListAndCountShapes(t *testing.T) {
	list := []byte(`{"resolved_instances":["a/r#1","b/s#2"],"unresolved_instances":["c/t#3"]}`)
	s, err := ParseReport(list)
	if err != nil {
		t.Fatal(err)
	}
	if s.Resolved != 2 || s.Unresolved != 1 || s.Total != 3 {
		t.Errorf("list shape: %+v", s)
	}

	counts := []byte(`{"resolved":5,"unresolved":3,"total":8}`)
	s, err = ParseReport(counts)
	if err != nil {
		t.Fatal(err)
	}
	if s.Resolved != 5 || s.Unresolved != 3 || s.Total != 8 {
		t.Errorf("count shape: %+v", s)
	}
}

func TestHarnessConfig_Write(t *testing.T) {
	var b bytes.Buffer
	c := NewHarnessConfig("/work", "/data/go.jsonl", "/out/predictions.jsonl", "/out")
	if err := c.Write(&b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	// The harness's marshmallow loader requires every non-defaulted CliArgs
	// field; assert the full set is emitted (a missing one fails the real run).
	for _, want := range []string{
		`"patch_files"`, `"/out/predictions.jsonl"`,
		`"dataset_files"`, `"/data/go.jsonl"`,
		`"repo_dir"`, `"need_clone"`, `"clear_env"`, `"stop_on_error"`,
		`"max_workers"`, `"max_workers_build_image"`, `"max_workers_run_instance": 1`,
		`"fix_patch_run_cmd"`, `"log_dir"`, `"log_level"`, `"log_to_console"`, `"human_mode"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("config missing %q:\n%s", want, out)
		}
	}
	// `mode` is NOT a CliArgs field — marshmallow rejects unknown keys.
	if strings.Contains(out, `"mode"`) {
		t.Errorf("config should not emit a 'mode' key (harness rejects unknown fields):\n%s", out)
	}
}
