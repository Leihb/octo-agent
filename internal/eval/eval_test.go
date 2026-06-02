package eval

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// writeTask materializes a minimal task directory under root and returns it.
func writeTask(t *testing.T, root, name, taskYAML, verify string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	mustWrite(t, filepath.Join(dir, "task.yaml"), taskYAML)
	mustWrite(t, filepath.Join(dir, "verify.sh"), verify)
	mustWrite(t, filepath.Join(dir, "repo", ".keep"), "")
	for rel, content := range files {
		mustWrite(t, filepath.Join(dir, rel), content)
	}
	return dir
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadTasks(t *testing.T) {
	root := t.TempDir()
	writeTask(t, root, "bbb", "name: bbb\nprompt: fix bbb\ntimeout: 90s\n", "#!/bin/sh\ntrue\n", nil)
	writeTask(t, root, "aaa", "name: aaa\nprompt: fix aaa\n", "#!/bin/sh\ntrue\n", nil)
	// A non-task directory must be skipped, not error.
	mustWrite(t, filepath.Join(root, "notes", "README.md"), "hi")

	tasks, err := LoadTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(tasks))
	}
	if tasks[0].Name != "aaa" || tasks[1].Name != "bbb" {
		t.Errorf("tasks not sorted by name: %v", []string{tasks[0].Name, tasks[1].Name})
	}
	if tasks[1].Timeout != 90*time.Second {
		t.Errorf("timeout = %v, want 90s", tasks[1].Timeout)
	}
}

func TestLoadTasksErrors(t *testing.T) {
	cases := []struct {
		name     string
		yaml     string
		omitRepo bool
		omitVfy  bool
	}{
		{"no-prompt", "name: x\n", false, false},
		{"bad-timeout", "prompt: x\ntimeout: notaduration\n", false, false},
		{"no-repo", "prompt: x\n", true, false},
		{"no-verify", "prompt: x\n", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, c.name)
			mustWrite(t, filepath.Join(dir, "task.yaml"), c.yaml)
			if !c.omitRepo {
				mustWrite(t, filepath.Join(dir, "repo", ".keep"), "")
			}
			if !c.omitVfy {
				mustWrite(t, filepath.Join(dir, "verify.sh"), "#!/bin/sh\ntrue\n")
			}
			if _, err := LoadTasks(root); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestCopyDir(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "a.txt"), "A")
	mustWrite(t, filepath.Join(src, "sub", "b.txt"), "B")

	dst := filepath.Join(t.TempDir(), "out")
	if err := copyDir(src, dst); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{"a.txt": "A", "sub/b.txt": "B"} {
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
}

// TestRunTask exercises the full orchestration with a fake octo (a shell script
// that edits the working copy) so it needs no model or network. The verify
// script also confirms the hidden file was injected only after octo ran.
func TestRunTask(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("harness shells out via sh; covered on unix")
	}
	root := t.TempDir()
	// verify: pass iff octo created fixed.txt AND the hidden marker was injected.
	verify := "#!/bin/sh\ntest -f fixed.txt && grep -q judge marker.txt\n"
	task := writeTask(t, root, "demo", "name: demo\nprompt: create fixed.txt\n", verify,
		map[string]string{"hidden/marker.txt": "judge\n"})

	loaded, err := LoadTasks(root)
	if err != nil {
		t.Fatal(err)
	}
	_ = task

	fakeOcto := filepath.Join(root, "octo.sh")
	mustWrite(t, fakeOcto, "#!/bin/sh\necho done > fixed.txt\n")
	if err := os.Chmod(fakeOcto, 0o755); err != nil {
		t.Fatal(err)
	}

	opt := Options{OctoBin: fakeOcto, WorkDir: filepath.Join(root, "work"), Timeout: time.Minute, VerifyAfter: time.Minute}
	res := RunTask(context.Background(), loaded[0], opt)
	if res.Err != nil {
		t.Fatalf("harness error: %v", res.Err)
	}
	if !res.Resolved {
		t.Errorf("expected resolved; verify output:\n%s", res.Verify)
	}

	// A no-op octo must leave the task unresolved (not a harness error).
	mustWrite(t, fakeOcto, "#!/bin/sh\ntrue\n")
	if err := os.Chmod(fakeOcto, 0o755); err != nil {
		t.Fatal(err)
	}
	res = RunTask(context.Background(), loaded[0], opt)
	if res.Err != nil {
		t.Fatalf("harness error: %v", res.Err)
	}
	if res.Resolved {
		t.Error("expected unresolved when octo makes no change")
	}
}
