---
name: code-review
description: Review local code changes using an isolated sub-agent for unbiased analysis. Checks correctness, conventions, performance, tests, security, and tech design compliance. Use when the user wants a code review or says "review the diff" / "review my changes" / "代码评审" / "review 一下改动".
---

Review local code changes by dispatching an **isolated sub-agent** — a fresh agent with no knowledge of the current session's implementation decisions. This eliminates the bias of reviewing your own work.

## Process

### 1. Gather context for the sub-agent

Collect everything the reviewer needs:

```bash
# What changed
git log --oneline main..HEAD
git diff main...HEAD
git diff
git diff --cached
```

Also identify:
- Tech design doc path (if one exists in conversation context or commit messages)
- Which services/modules are touched
- Test files added or modified

### 2. Dispatch the sub-agent

Call the `sub_agent` tool with `subagent_type: "code-review"` — a read-only reviewer that starts with zero context. Pass it the diff range, file list, and tech design reference in the prompt — but NOT your implementation reasoning or conversation history.

The sub-agent prompt should follow the template in [code-reviewer.md](code-reviewer.md).

Fill in:
- `{DESCRIPTION}` — brief summary of what these changes do
- `{TECH_DESIGN_PATH}` — path to tech design doc (or "none")
- `{BASE_SHA}` / `{HEAD_SHA}` — git range
- `{DIFF_STAT}` — output of `git diff --stat`

If the `sub_agent` tool is not available in this session, perform the review yourself following the same template — and say explicitly in the report that the review was not isolated.

### 3. Act on the review

When the sub-agent returns its findings:

**Receiving feedback — rules:**
- Verify before implementing. Don't blindly agree.
- If any item is unclear, clarify ALL unclear items before fixing any.
- Implementation order: Critical → Important → Minor, test each fix individually.
- Push back with technical reasoning if the reviewer is wrong (missing context, doesn't apply to this codebase, YAGNI).
- No performative agreement ("Great point!", "You're absolutely right!"). Just fix and show the result.

**Priority:**
- **Critical**: fix immediately, no exceptions
- **Important**: fix before proceeding
- **Minor**: note for later or fix if quick

### 4. Report to user

Relay the sub-agent's review to the user with your own assessment of each finding — agree, disagree with reasoning, or need discussion.
