package eval

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Options drive a suite run.
type Options struct {
	OctoBin     string        // path to the octo binary
	WorkDir     string        // scratch root; per-task copies live under it
	Model       string        // empty = octo default
	Provider    string        // empty = octo default
	MaxTurns    int           // octo --max-turns
	AllowNet    bool          // allow octo network access (default false)
	Timeout     time.Duration // per-task default; a task's own timeout overrides
	VerifyAfter time.Duration // cap on the verify command itself
}

// Result is the outcome of one task.
type Result struct {
	Task     string
	Resolved bool          // verify exited 0
	Duration time.Duration // octo + verify wall-clock
	OctoLog  string        // path to octo's transcript
	Verify   string        // combined verify output (tail kept by caller)
	Err      error         // harness error (copy/octo-launch/verify-launch failed)
}

// RunTask copies the fixture, drives octo, injects the hidden files, and runs
// verify.sh. A non-zero verify exit means unresolved, not a harness error;
// Err is set only when the harness itself could not complete a step.
func RunTask(ctx context.Context, t Task, opt Options) Result {
	start := time.Now()
	res := Result{Task: t.Name}

	taskWork := filepath.Join(opt.WorkDir, t.Name)
	_ = os.RemoveAll(taskWork)
	work := filepath.Join(taskWork, "work")
	if err := copyDir(filepath.Join(t.Dir, "repo"), work); err != nil {
		res.Err = fmt.Errorf("copy fixture: %w", err)
		return res
	}

	timeout := opt.Timeout
	if t.Timeout > 0 {
		timeout = t.Timeout
	}
	octoCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		octoCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	evalHome := filepath.Join(taskWork, "home")
	octoOut, oerr := driveOcto(octoCtx, octoOptions{
		Bin:      opt.OctoBin,
		EvalHome: evalHome,
		Model:    opt.Model,
		Provider: opt.Provider,
		MaxTurns: opt.MaxTurns,
		AllowNet: opt.AllowNet,
	}, work, t.Prompt)

	logPath := filepath.Join(taskWork, "octo.log")
	_ = os.WriteFile(logPath, []byte(octoOut), 0o644)
	res.OctoLog = logPath
	// A non-zero octo exit (incl. a hit timeout) isn't fatal: verify decides.

	// Inject the hidden judging files now that octo is done, then verify.
	if hidden := filepath.Join(t.Dir, "hidden"); dirExists(hidden) {
		if err := copyDir(hidden, work); err != nil {
			res.Err = fmt.Errorf("inject hidden: %w", err)
			res.Duration = time.Since(start)
			return res
		}
	}

	verifyOut, code, verr := runVerify(ctx, filepath.Join(t.Dir, "verify.sh"), work, opt.VerifyAfter)
	res.Verify = verifyOut
	res.Duration = time.Since(start)
	if verr != nil {
		res.Err = fmt.Errorf("run verify: %w", verr)
		return res
	}
	res.Resolved = code == 0
	_ = oerr // surfaced via transcript; verify is the source of truth
	return res
}

// runVerify executes verify.sh with cwd=work and returns combined output plus
// the exit code. An exit code (even non-zero) is a normal result; err is set
// only when the process could not be started or was killed by the timeout.
func runVerify(ctx context.Context, script, work string, timeout time.Duration) (string, int, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	// Invoke via `sh` so verify.sh needn't carry the executable bit through git.
	cmd := exec.CommandContext(ctx, "sh", script)
	cmd.Dir = work
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return buf.String(), -1, fmt.Errorf("verify timed out")
	}
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return buf.String(), exit.ExitCode(), nil // non-zero exit == unresolved
		}
		return buf.String(), -1, err // couldn't start
	}
	return buf.String(), 0, nil
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// copyDir recursively copies src into dst, creating dst. Symlinks are copied as
// regular files (fixtures shouldn't contain them); file modes are preserved.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return copyFile(path, target, info.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
