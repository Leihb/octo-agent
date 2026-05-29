// Command mswe-eval runs the octo side of the Multi-SWE-bench evaluation: it
// drives octo over a slice of Go instances to produce patches, then hands them
// to the official Python+Docker judge.
//
// This tool is NOT part of the octo binary and never runs in CI — it needs a
// real model key, network, and (for `judge`) Docker + the multi_swe_bench
// Python package. See dev-docs/mswe-eval.md.
//
// Subcommands:
//
//	inspect   Print the schema (keys + scalars) of the first N dataset records,
//	          to confirm field names before a real run.
//	generate  Clone each repo at its base commit, drive octo to fix the issue,
//	          and write predictions.jsonl (one {org,repo,number,fix_patch}/line).
//	judge     Write the harness config, invoke run_evaluation, parse the report.
//	run       generate then judge.
package main

import (
	_ "embed"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Leihb/octo-agent/internal/mswe"
)

// judgeDockerfile builds the Linux image the harness runs in under --docker
// mode (it can't import on a case-insensitive macOS/Windows filesystem).
//
//go:embed Dockerfile.judge
var judgeDockerfile string

const judgeImage = "octo-mswe-judge"

// ensureJudgeImage builds judgeImage from the embedded Dockerfile if it isn't
// already present locally.
func ensureJudgeImage() error {
	if err := exec.Command("docker", "image", "inspect", judgeImage).Run(); err == nil {
		return nil
	}
	fmt.Printf("building judge image %s (one-time)…\n", judgeImage)
	cmd := exec.Command("docker", "build", "-t", judgeImage, "-")
	cmd.Stdin = strings.NewReader(judgeDockerfile)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("build judge image: %w", err)
	}
	return nil
}

// resolve returns p as an absolute, symlink-resolved path (best effort), so
// host paths and in-container bind-mount paths line up byte-for-byte.
func resolve(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// uniqueDirs returns the distinct non-empty entries (host dirs to bind-mount at
// identical paths in the judge container, so paths the harness hands to the
// host Docker daemon resolve correctly).
func uniqueDirs(paths ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "inspect":
		err = runInspect(os.Args[2:])
	case "generate":
		err = runGenerate(os.Args[2:])
	case "judge":
		err = runJudge(os.Args[2:])
	case "run":
		if err = runGenerate(os.Args[2:]); err == nil {
			err = runJudge(os.Args[2:])
		}
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "mswe-eval: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mswe-eval <inspect|generate|judge|run> [flags]")
	fmt.Fprintln(os.Stderr, "see dev-docs/mswe-eval.md")
}

// ── inspect ────────────────────────────────────────────────────────────────

func runInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	dataset := fs.String("dataset", "", "path to the Multi-SWE-bench JSONL dataset")
	lang := fs.String("lang", "", "language filter (''=all; the dataset ships pre-split per language, so usually leave empty)")
	limit := fs.Int("limit", 1, "number of records to inspect")
	if err := fs.Parse(args); err != nil {
		return err
	}
	insts, err := loadDataset(*dataset, *lang, *limit)
	if err != nil {
		return err
	}
	for i, inst := range insts {
		fmt.Printf("── record %d ── %s [%s]\n", i, inst.ID(), inst.Language())
		fmt.Printf("  keys: %s\n", strings.Join(inst.Keys(), ", "))
		fmt.Printf("  base_commit: %q\n", inst.BaseCommit())
		fmt.Printf("  clone_url:   %s\n", inst.CloneURL())
		ps := inst.ProblemStatement()
		if len(ps) > 200 {
			ps = ps[:200] + "…"
		}
		fmt.Printf("  problem (preview): %s\n", strings.ReplaceAll(ps, "\n", " "))
	}
	if len(insts) == 0 {
		fmt.Println("(no records matched — check --dataset path and --lang)")
	}
	return nil
}

// ── generate ───────────────────────────────────────────────────────────────

func runGenerate(args []string) error {
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	dataset := fs.String("dataset", "", "path to the Multi-SWE-bench JSONL dataset")
	lang := fs.String("lang", "", "language filter (''=all; dataset ships pre-split per language)")
	limit := fs.Int("limit", 5, "max instances to run")
	octoBin := fs.String("octo", "./octo", "path to the octo binary")
	out := fs.String("out", "predictions.jsonl", "output predictions file (JSONL)")
	workdir := fs.String("workdir", filepath.Join(os.TempDir(), "mswe-eval"), "scratch dir for clones + octo HOME")
	model := fs.String("model", "", "model passed to octo (empty = octo default)")
	provider := fs.String("provider", "", "provider passed to octo (empty = octo default)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	insts, err := loadDataset(*dataset, *lang, *limit)
	if err != nil {
		return err
	}
	if len(insts) == 0 {
		return fmt.Errorf("no %s instances found in %s", *lang, *dataset)
	}
	octoAbs, err := filepath.Abs(*octoBin)
	if err != nil {
		return err
	}

	// Isolated HOME so octo's ~/.octo (sessions, memory, permissions) doesn't
	// touch the user's, and so a permissive permissions file lets the agent run
	// tools non-interactively in the throwaway clone.
	evalHome := filepath.Join(*workdir, "home")
	if err := writeEvalPermissions(evalHome); err != nil {
		return err
	}

	var preds []mswe.Prediction
	for i, inst := range insts {
		fmt.Printf("[%d/%d] %s — clone + run octo…\n", i+1, len(insts), inst.ID())
		patch, err := generateOne(inst, octoAbs, evalHome, *workdir, *model, *provider)
		if err != nil {
			fmt.Printf("        ! skipped: %v\n", err)
			continue
		}
		if strings.TrimSpace(patch) == "" {
			fmt.Printf("        ! empty patch (octo made no source changes) — recording anyway\n")
		}
		preds = append(preds, mswe.Prediction{
			Org: inst.Org(), Repo: inst.Repo(), Number: inst.Number(), FixPatch: patch,
		})
	}

	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := mswe.WritePredictions(f, preds); err != nil {
		return err
	}
	fmt.Printf("wrote %d prediction(s) → %s\n", len(preds), *out)
	return nil
}

// generateOne clones the instance's repo at its base commit, drives octo to
// resolve the issue, and returns the test-scoped diff.
func generateOne(inst mswe.Instance, octoBin, evalHome, workdir, model, provider string) (string, error) {
	if inst.BaseCommit() == "" {
		return "", fmt.Errorf("no base commit (run `inspect` to confirm the field name)")
	}
	repoDir := filepath.Join(workdir, "repos", fmt.Sprintf("%s__%s__%d", inst.Org(), inst.Repo(), inst.Number()))
	_ = os.RemoveAll(repoDir)
	if err := os.MkdirAll(filepath.Dir(repoDir), 0o755); err != nil {
		return "", err
	}

	if out, err := run("", nil, "git", "clone", "--quiet", inst.CloneURL(), repoDir); err != nil {
		return "", fmt.Errorf("clone: %v (%s)", err, truncate(out, 200))
	}
	if out, err := run(repoDir, nil, "git", "checkout", "--quiet", inst.BaseCommit()); err != nil {
		return "", fmt.Errorf("checkout %s: %v (%s)", inst.BaseCommit(), err, truncate(out, 200))
	}

	// Drive octo through REPL mode (prompt piped on stdin, EOF ends it). The
	// agentic tool-execution loop only runs in REPL mode — a single-turn
	// `octo chat "msg"` does one model round-trip WITHOUT executing tools, so it
	// would never edit files. Strict perms + the eval HOME's permissive
	// permissions.yml let tools run without prompts; --no-save keeps the
	// throwaway session out of history.
	env := append(os.Environ(), "HOME="+evalHome)
	octoArgs := []string{"chat", "--tools", "--permission-mode", "strict", "--no-save", "--plain"}
	if model != "" {
		octoArgs = append(octoArgs, "--model", model)
	}
	if provider != "" {
		octoArgs = append(octoArgs, "--provider", provider)
	}
	octoOut, oerr := runStdin(repoDir, env, octoPrompt(inst)+"\n", octoBin, octoArgs...)
	// Persist octo's transcript for debugging OUTSIDE the repo, so `git add -A`
	// below doesn't sweep it into the patch. A non-zero exit isn't fatal — octo
	// may exit non-zero yet still have made useful edits, which the diff captures.
	logDir := filepath.Join(workdir, "logs")
	_ = os.MkdirAll(logDir, 0o755)
	_ = os.WriteFile(filepath.Join(logDir, fmt.Sprintf("%s__%s__%d.log", inst.Org(), inst.Repo(), inst.Number())), []byte(octoOut), 0o644)
	if oerr != nil {
		fmt.Printf("        (octo exited %v; capturing whatever it changed)\n", oerr)
	}

	// Capture every change (incl. new/deleted files), then drop test files.
	if out, err := run(repoDir, nil, "git", "add", "-A"); err != nil {
		return "", fmt.Errorf("git add: %v (%s)", err, truncate(out, 200))
	}
	diff, err := run(repoDir, nil, "git", "diff", "--cached")
	if err != nil {
		return "", fmt.Errorf("git diff: %v", err)
	}
	return mswe.ScopeFixPatch(diff), nil
}

func octoPrompt(inst mswe.Instance) string {
	return "You are working in the " + inst.Org() + "/" + inst.Repo() + " repository. " +
		"Resolve the following issue by editing the SOURCE code only — do NOT add or modify any test files (*_test.go). " +
		"When done, make sure the package still builds (`go build ./...`).\n\n" +
		"--- ISSUE ---\n" + inst.ProblemStatement()
}

// writeEvalPermissions drops a permissive permissions.yml into the eval HOME so
// octo runs its tools without interactive prompts inside the throwaway clone.
func writeEvalPermissions(evalHome string) error {
	dir := filepath.Join(evalHome, ".octo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	const perms = `# Generated by mswe-eval — allow tools non-interactively in the eval sandbox.
terminal:
  - allow: { pattern: "" }
write_file:
  - allow: { path: ["**"] }
edit_file:
  - allow: { path: ["**"] }
read_file:
  - allow: { path: ["**"] }
`
	return os.WriteFile(filepath.Join(dir, "permissions.yml"), []byte(perms), 0o644)
}

// ── judge ──────────────────────────────────────────────────────────────────

func runJudge(args []string) error {
	fs := flag.NewFlagSet("judge", flag.ContinueOnError)
	dataset := fs.String("dataset", "", "path to the Multi-SWE-bench JSONL dataset")
	predictions := fs.String("predictions", "predictions.jsonl", "predictions JSONL from `generate`")
	workdir := fs.String("workdir", filepath.Join(os.TempDir(), "mswe-eval"), "scratch dir for the harness")
	python := fs.String("python", "python3", "python interpreter with multi_swe_bench installed (native mode)")
	dockerMode := fs.Bool("docker", runtime.GOOS != "linux", "run the harness in a Linux container via the host Docker daemon — required on macOS/Windows, where the harness can't import")
	// generate-only flags tolerated so `run` can share one flag set.
	_ = fs.String("octo", "", "")
	_ = fs.String("out", "", "")
	_ = fs.String("lang", "", "")
	_ = fs.Int("limit", 0, "")
	_ = fs.String("model", "", "")
	_ = fs.String("provider", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dataset == "" {
		return fmt.Errorf("judge: --dataset is required")
	}
	if err := os.MkdirAll(*workdir, 0o755); err != nil {
		return err
	}
	// Resolve symlinks on every path the config + Docker mounts reference, so a
	// container's same-path bind mount agrees with the host (macOS /tmp →
	// /private/tmp would otherwise make the harness fail to find its config).
	wd := resolve(*workdir)
	outputDir := filepath.Join(wd, "judge-out")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return err
	}
	// The harness validates that repo_dir already exists (it clones into it).
	if err := os.MkdirAll(filepath.Join(wd, "repos"), 0o755); err != nil {
		return err
	}
	datasetAbs := resolve(*dataset)
	predAbs := resolve(*predictions)
	cfg := mswe.NewHarnessConfig(wd, datasetAbs, predAbs, outputDir)
	cfgPath := filepath.Join(outputDir, "config.json")
	cf, err := os.Create(cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.Write(cf); err != nil {
		cf.Close()
		return err
	}
	cf.Close()

	var cmd *exec.Cmd
	if *dockerMode {
		if err := ensureJudgeImage(); err != nil {
			return err
		}
		// Mount the host Docker socket (so the harness drives the host daemon to
		// build/run per-instance containers) and every host dir the config
		// references AT THE SAME PATH, so the paths the harness passes to
		// `docker run -v` resolve identically on the host.
		dargs := []string{"run", "--rm", "-v", "/var/run/docker.sock:/var/run/docker.sock"}
		for _, d := range uniqueDirs(wd, filepath.Dir(datasetAbs), filepath.Dir(predAbs)) {
			dargs = append(dargs, "-v", d+":"+d)
		}
		dargs = append(dargs, "-w", wd, judgeImage,
			"python", "-m", "multi_swe_bench.harness.run_evaluation", "--config", cfgPath)
		fmt.Printf("running judge in container %s (host daemon via socket)\n", judgeImage)
		cmd = exec.Command("docker", dargs...)
	} else {
		fmt.Printf("running judge: %s -m multi_swe_bench.harness.run_evaluation --config %s\n", *python, cfgPath)
		cmd = exec.Command(*python, "-m", "multi_swe_bench.harness.run_evaluation", "--config", cfgPath)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run_evaluation: %w (Docker running? on macOS/Windows use --docker)", err)
	}

	reportPath, err := findReport(outputDir)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(reportPath)
	if err != nil {
		return err
	}
	sum, err := mswe.ParseReport(data)
	if err != nil {
		return err
	}
	fmt.Printf("\n=== Multi-SWE-bench (Go) ===\nresolved %d / %d  (unresolved %d)\nreport: %s\n",
		sum.Resolved, sum.Total, sum.Unresolved, reportPath)
	return nil
}

// findReport locates final_report.json beneath dir (the harness may nest it in
// a run-specific subdirectory).
func findReport(dir string) (string, error) {
	var found string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() && filepath.Base(p) == "final_report.json" {
			found = p
		}
		return nil
	})
	if found == "" {
		return "", fmt.Errorf("final_report.json not found under %s — check the harness output above", dir)
	}
	return found, nil
}

// ── helpers ──────────────────────────────────────────────────────────────

func loadDataset(path, lang string, limit int) ([]mswe.Instance, error) {
	if path == "" {
		return nil, fmt.Errorf("--dataset is required (see dev-docs/mswe-eval.md for how to obtain the Go JSONL)")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return mswe.LoadInstances(f, lang, limit)
}

// run executes name+args in dir (cwd if "") with env (inherited if nil) and
// returns combined output.
func run(dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runStdin is like run but feeds stdin to the process — used to drive octo's
// REPL (the prompt on stdin, EOF ending the session).
func runStdin(dir string, env []string, stdin, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
