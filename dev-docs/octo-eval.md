# octo-eval — lightweight eval harness

Measures whether a prompt, tool, or model change made octo better or worse at
real edits, in **seconds**. It clones nothing and builds no Docker images: every
task is a local fixture with a hidden verify step, so one task = one octo
agentic run + one `verify.sh`. It's the fast regression signal to reach for
during iteration.

## Task layout

Each directory under `evals/tasks/` is one task:

```
evals/tasks/<name>/
  task.yaml      # name + prompt (+ optional `timeout: 5m`)
  repo/          # fixture source octo edits — the only thing octo sees
  hidden/        # files injected AFTER octo runs; octo can't read or game them
  verify.sh      # run from the working copy after injection; exit 0 == resolved
```

The `hidden/` + `verify.sh` split is the anti-gaming mechanism: the judging
files don't exist in the working copy while octo edits, so it can only ever see
`repo/`.

Fixtures are hand-written Go modules. Each carries its own nested `go.mod`, so
the parent module's `go test ./...` and `go vet ./...` skip them — the
intentionally-broken fixtures never run in CI. `verify.sh` runs `go test ./...`
inside the working copy; the framework itself is language-agnostic (any
`verify.sh` that exits 0 on success works).

## Architecture

```
octo-eval run
  for each task:
    copy repo/ → <workdir>/<task>/work
    octo chat --tools --permission-mode strict --no-save --plain
         --prompt-file <task.prompt> --sandbox --max-turns N   (cwd = work)
    copy hidden/ → work          # inject judging files
    sh verify.sh                 # cwd = work; exit 0 == resolved
  → resolved / total
```

octo runs under an **isolated `HOME`** (`<workdir>/<task>/home`) with a
permissive `permissions.yml`, so eval sessions never touch your real `~/.octo`
and tools run without prompts. Without `--allow-net` the run is `--sandbox`
(hermetic — no gold-patch leak via `web_fetch`/`web_search`). A non-zero octo
exit (including a hit timeout) is not fatal; `verify.sh` is the source of truth.

The Go code splits into pure logic in `internal/eval` (task parsing, fixture
copy, verify exit-code mapping — unit-tested, no network) and the CLI in
`cmd/octo-eval`. `internal/eval/eval_test.go` exercises the full orchestration
with a fake octo (a shell script that edits the working copy), so the tests need
no model or network.

## Run it

```bash
make build                              # produces ./octo
go build -o octo-eval ./cmd/octo-eval

./octo-eval list                        # show the suite

ANTHROPIC_API_KEY=… ANTHROPIC_BASE_URL=… \
  ./octo-eval run --octo ./octo --model <model> --provider anthropic

./octo-eval run --filter fix-nil-panic  # one task
```

Flags: `--tasks-dir` (default `evals/tasks`), `--octo`, `--model`, `--provider`,
`--workdir`, `--filter`, `--max-turns` (default 50), `--timeout` (per-task octo
cap, default 5m; a task's own `timeout` overrides), `--verify-timeout`,
`--allow-net`.

## Adding a task

1. `mkdir -p evals/tasks/<name>/{repo,hidden}`
2. Write the broken fixture under `repo/` (with a `go.mod`) and the judging
   `*_test.go` under `hidden/`.
3. `task.yaml` with `name` + a `prompt` that names the file and the expected
   behaviour, and tells octo to edit source only.
4. `verify.sh` → `go test ./...`.
5. Confirm the fixture **fails unfixed**: copy `repo/` + `hidden/` into a temp
   dir and run `go test ./...` — it must fail (proving the bug is real and the
   test catches it).
```
