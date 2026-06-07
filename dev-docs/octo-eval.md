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
  task.yaml      # name + prompt (+ optional `timeout: 5m`)        [required]
  repo/          # starting point octo edits — the only thing octo sees [required]
  verify.sh      # run from the working copy; exit 0 == resolved    [required]
  hidden/        # files injected AFTER octo runs; octo never sees them [optional]
  rubric.md      # scoring criteria for an LLM judge                [optional]
```

`verify.sh` is the universal contract: it runs with cwd = the working copy and
its exit code (0 = resolved) is the only signal the harness reads. Everything
else is a convention layered on top.

When present, the `hidden/` + `verify.sh` split is the anti-gaming mechanism:
the judging files don't exist in the working copy while octo edits, so it can
only ever see `repo/`. For a generative task with no hidden tests, octo just
starts from an empty `repo/` (a `.keep` placeholder).

## Two kinds of task

**Deterministic** — there's an objectively correct outcome. `verify.sh` checks
it and nothing else.
- *Coding* fixtures (`fix-off-by-one`, `add-json-field`, `fix-nil-panic`) are
  hand-written Go modules with a hidden `*_test.go`; `verify.sh` runs
  `go test ./...`. Each carries its own nested `go.mod` (see *CI exclusion*
  below).
- *Data* tasks (`data-pipeline`) give octo an input and a fixed expected answer;
  `verify.sh` checks the produced file against the canonical value.

**Open-ended / generative** — the result is subjective (an *artistic*
photographer homepage, a *clear* comparison doc), so there's no single correct
output. These use a two-stage gate in `verify.sh`:

1. A **structural gate** — cheap, deterministic assertions for the objective
   minimum (file exists, is HTML, is self-contained, isn't a stub). Kept
   intentionally loose so it never kills a reasonable result.
2. An **LLM judge** — `evals/lib/llm-judge.sh` scores the artifact against the
   task's `rubric.md` on a 0-10 scale and exits non-zero below a threshold. The
   specific requirements ("≥6 slides", "a gallery", "a comparison table") live
   in the rubric, not the structural gate, so quality — not just presence — is
   what's measured.

`html-slides`, `photographer-homepage`, and `tech-doc` are generative tasks.

### The LLM judge

`evals/lib/llm-judge.sh <rubric> <threshold> <artifact...>` builds a judging
prompt from the rubric + artifact, calls a chat-completions endpoint at
`temperature 0`, parses a `{"score", "reason"}` verdict, and exits 0 iff
`score >= threshold`. It **fails closed**: a missing key, unreachable API, or
unparseable response exits 2, so a broken judge never silently passes a task.

Env: `OPENAI_API_KEY`, `OPENAI_BASE_URL`, and a judge model from
`OCTO_EVAL_JUDGE_MODEL` (preferred) or `OPENAI_MODEL`. Prefer a *different*
judge model than the one under test to avoid self-grading. Judges are not
perfectly repeatable even at `temperature 0`, so treat generative scores as a
coarse signal and calibrate thresholds against real runs.

### CI exclusion

`evals/` is its own Go module (`evals/go.mod`), so the parent module's
`go test ./...` / `go vet ./...` skip the whole tree — the intentionally-broken
coding fixtures never run in CI. The harness copies a fixture's `repo/` into a
scratch dir at eval time; that copy gets its own `go.mod` and compiles
standalone.

## Architecture

```
octo-eval run
  for each task:
    copy repo/ → <workdir>/<task>/work
    octo chat --tools --permission-mode auto --no-save --plain
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

# Anthropic-protocol provider:
ANTHROPIC_API_KEY=… ANTHROPIC_BASE_URL=… \
  ./octo-eval run --octo ./octo --model <model> --provider anthropic

# OpenAI-protocol provider (also feeds the LLM judge via the same env):
OPENAI_API_KEY=… OPENAI_BASE_URL=https://api.deepseek.com \
OCTO_EVAL_JUDGE_MODEL=<judge-model> \
  ./octo-eval run --octo ./octo --model <model> --provider openai --allow-net

./octo-eval run --filter fix-nil-panic  # one task
```

Generative tasks need the judge env (`OPENAI_API_KEY` / `OPENAI_BASE_URL` /
`OCTO_EVAL_JUDGE_MODEL`) set even when octo itself runs on an Anthropic
provider, since `verify.sh` calls the judge over the OpenAI-protocol endpoint.

Flags: `--tasks-dir` (default `evals/tasks`), `--octo`, `--model`, `--provider`,
`--workdir`, `--filter`, `--max-turns` (default 50), `--max-tokens` (per-response
output cap, default 8192 — the provider default of 4096 truncates a large
single-file artifact mid-write), `--timeout` (per-task octo cap, default 5m; a
task's own `timeout` overrides), `--verify-timeout`, `--allow-net`.

## Adding a task

**Deterministic (coding/data):**

1. `mkdir -p evals/tasks/<name>/{repo,hidden}`
2. Write the broken fixture under `repo/` (Go fixtures need a `go.mod`) and the
   judging `*_test.go` under `hidden/`.
3. `task.yaml` with `name` + a `prompt` that names the file and the expected
   behaviour, and tells octo to edit source only.
4. `verify.sh` → `go test ./...` (or check the produced output against a fixed
   expected value).
5. Confirm it **fails unfixed**: copy `repo/` + `hidden/` into a temp dir and
   run `verify.sh` — it must fail (proving the check is real). Confirm a correct
   fix flips it to pass.

**Generative (open-ended):**

1. `mkdir -p evals/tasks/<name>/repo` and add `repo/.keep`.
2. `task.yaml` with a `prompt` describing the artifact and its requirements, and
   a `timeout` (generative runs take longer).
3. `rubric.md` with the scoring criteria — put the *specific* requirements here.
4. `verify.sh`: a loose structural gate (file exists, right format, not a stub),
   then `sh "$here/../../lib/llm-judge.sh" "$here/rubric.md" <threshold> <artifact>`.
5. Calibrate `<threshold>` against a couple of real runs: too high and even good
   output fails, too low and there's no discrimination.
