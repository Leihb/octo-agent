---
name: worktree-isolate
description: Do risky or experimental work in an isolated git worktree, then merge or discard. Use when the user wants changes tried in isolation ("试在 worktree 里改", "隔离环境跑一下", "worktree 隔离", "在临时分支上做"), or before a large/uncertain edit that shouldn't touch the main checkout. Trailing text is the task to perform in the worktree.
---

# Work in an isolated git worktree

Create a throwaway git worktree on a new branch, do all the work there, verify, then either merge it back or remove it. octo has **no session-level working directory** — so the one rule that makes this work is: **address the worktree by absolute path in every tool call.**

## ⚠️ The absolute-path rule (octo-specific — read first)

octo's `terminal` runs each command in a **fresh shell** in the **process's launch directory**; a `cd` does **not** carry to the next `terminal` call, and `read_file` / `edit_file` / `write_file` resolve relative paths against that same fixed directory. There is no "the session is now inside the worktree" mode.

Therefore, after creating the worktree:

- **Shell:** use `git -C "$WT" …` and absolute paths, or chain everything inside ONE command: `cd "$WT" && go build ./... && go test ./...`. Never split `cd "$WT"` from the work into separate `terminal` calls.
- **Files:** always pass the worktree's **absolute path** to read_file / edit_file / write_file (e.g. `$WT/internal/foo.go`), never a bare relative path.

Resolve `$WT` to a real absolute path once and reuse it verbatim.

## Steps

1. **Locate the repo and base.**
   - `terminal: git rev-parse --show-toplevel` → this is `$REPO` (absolute).
   - Note the current branch (`git -C "$REPO" rev-parse --abbrev-ref HEAD`); the new branch will fork from current HEAD unless the user says otherwise.

2. **Pick a name and path.** Slugify the task into `<slug>`. Use:
   - worktree dir: `$WT = $REPO/.worktrees/<slug>`
   - branch: `wt/<slug>`
   - Make sure `.worktrees/` is ignored: if `git -C "$REPO" check-ignore .worktrees` prints nothing, append `/.worktrees/` to `$REPO/.gitignore` first.

3. **Create the worktree.**
   ```
   git -C "$REPO" worktree add "$WT" -b "wt/<slug>"
   ```
   If the branch already exists, drop `-b` and pass the branch name as a third arg, or pick a fresh slug.

4. **Do the task in `$WT`** — following the absolute-path rule above. Build and run the project's tests inside `$WT` to verify (e.g. `cd "$WT" && <project test command>`), not in the main checkout.

5. **Finish — ask the user which, unless they already said:**
   - **Keep / merge:** leave the branch for a PR, or `git -C "$REPO" merge wt/<slug>` if they want it folded in. Report the branch name and the diff summary.
   - **Discard:** `git -C "$REPO" worktree remove --force "$WT"` and `git -C "$REPO" branch -D wt/<slug>`.

## Notes

- Don't `os.Chdir` / don't expect a persistent cwd — that's exactly what this skill routes around.
- A worktree shares the repo's object store; creating/removing it is cheap and never touches the main working tree.
- If anything fails mid-way, surface it and leave the worktree in place (so the user can inspect) rather than silently removing it.
