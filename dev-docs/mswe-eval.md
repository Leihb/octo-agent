# Multi-SWE-bench eval harness

Measures octo's task-completion quality on a small slice of **Multi-SWE-bench**
(Go subset) — real GitHub issues in real repos, judged by hidden tests. This is
the Tier-1 "eval / regression" capability: it's how you tell whether a prompt,
tool, or model change made octo *better or worse* at actually fixing issues.

## Why it's not in CI

A real eval **must** use the real model (it measures whether the fix works) and
the official judge needs **Docker** + the `multi_swe_bench` Python package +
network (clone repos, pull images). None of that belongs in `go test` (the
project rule: no live network in CI). So this is a **manual / periodic** run.

The Go code that's part of CI is only the pure logic in `internal/mswe`
(dataset parsing, prediction writing, patch scoping, config, report parsing) —
fully unit-tested. The orchestration in `cmd/mswe-eval` is exercised by the
real run below.

## Architecture (two stages)

```
mswe-eval generate  (our Go tool, drives octo)
  for each Go instance:
    git clone <repo> ; git checkout <base_commit>
    octo chat --tools --permission-mode strict --no-save "<issue>"
    git add -A ; git diff --cached ; strip *_test.go hunks
  → predictions.jsonl   {org, repo, number, fix_patch}

mswe-eval judge  (invokes the official harness)
  → config.json {patch_files, dataset_files, output_dir, ...}
  → python -m multi_swe_bench.harness.run_evaluation --config config.json
       (builds a Docker env per instance, applies fix_patch + hidden tests, runs)
  → final_report.json → resolved / total
```

octo only ever produces the source patch; the official harness owns the
Docker-based judging. octo runs with an **isolated `HOME`** (a throwaway
`<workdir>/home`) so eval sessions/memory don't touch your real `~/.octo`, and a
permissive `permissions.yml` there lets it run tools without prompts in the
disposable clone.

## Prerequisites

- A model key in the environment (e.g. `ANTHROPIC_API_KEY` + `ANTHROPIC_BASE_URL`
  for a Kimi/Anthropic-compatible endpoint). `generate` passes the environment
  through to octo.
- `git`, network access (clone repos).
- For `judge`: **Docker running**. In the default `--docker` mode (auto off
  Linux) the harness lives in the auto-built `octo-mswe-judge` image, so no
  native Python/`multi_swe_bench` install is needed. Native mode (`--docker=false`,
  Linux only) instead needs `python3` + `pip install multi-swe-bench`.

## Get the Go dataset slice

The dataset lives on HuggingFace as raw JSONL: `ByteDance-Seed/Multi-SWE-bench`
(or the smaller `…_mini` / `…-flash`). Download it and keep only Go records, e.g.

```bash
huggingface-cli download ByteDance-Seed/Multi-SWE-bench --repo-type dataset --local-dir ~/mswe-data
```

The data ships **pre-split per language** as `go/<org>__<repo>_dataset.jsonl`
(no per-record `language` field), so combine the Go files into one:

```bash
cat ~/mswe-data/go/*.jsonl > ~/mswe-data/go-all.jsonl
```

`--dataset` (our tool) and `dataset_files` (the harness config) both point at
this Go JSONL. A record carries `org`, `repo`, `number`, nested `base.sha`,
`resolved_issues`, `test_patch`, and `f2p_tests`/`p2p_tests`/`s2p_tests`.

> **macOS/Windows:** put the working directory **under your home dir** — Docker
> Desktop shares `/Users` but not `/private/tmp`, and the judge bind-mounts the
> workdir into the container at the same path. Pass `--workdir ~/mswe-work`.

## Run it

```bash
# 0. Confirm the schema (base_commit + problem come out non-empty).
./mswe-eval inspect --dataset ~/mswe-data/go-all.jsonl --limit 1

# 1. Generate patches with octo (real model key in the env), then judge.
ANTHROPIC_API_KEY=… ANTHROPIC_BASE_URL=… \
  ./mswe-eval generate --dataset ~/mswe-data/go-all.jsonl --limit 5 \
    --octo ./octo --model <model> --provider anthropic \
    --out ~/mswe-work/predictions.jsonl --workdir ~/mswe-work
./mswe-eval judge --dataset ~/mswe-data/go-all.jsonl \
    --predictions ~/mswe-work/predictions.jsonl --workdir ~/mswe-work
```

`judge` runs the harness in a **Linux container** (`--docker`, auto-enabled off
Linux) because the harness can't import on a case-insensitive filesystem. It
auto-builds the `octo-mswe-judge` image (Python + harness) on first use and
drives the host Docker daemon via the mounted socket to build + test each
instance. To bound a first run to one instance, point `--dataset` at a
one-record JSONL.

## Validated end-to-end (2026-05-29)

Confirmed against `multi_swe_bench 1.1.2` + the real Go data: octo resolved
**cli/cli#10388** ("`gh api` HEAD request → unexpected end of JSON input") with
a 5-line fix to `pkg/cmd/api/api.go`, and the harness scored it **resolved 1/1**
against the hidden tests. The macOS-specific gotchas that the `--docker` path
handles: case-insensitive import failure (→ Linux container), no daemon in the
container (→ mount `/var/run/docker.sock`), `/tmp`→`/private/tmp` symlink and
`/private/tmp` not being Docker-shared (→ home workdir + symlink-resolved
same-path mounts), git "dubious ownership" on host-owned clones (→
`safe.directory '*'` in the image), and the harness requiring `repo_dir` to
pre-exist (→ created before invoking).

## Cost & scale

Each instance is one full octo agentic run (cheap on Kimi) plus a Docker image
build + test run — the latter dominates (the cli/cli base image took ~4 min to
build the first time). Images are cached, so re-runs of the same repo are much
faster. Start at `LIMIT=5`, grow once you've got a baseline.
