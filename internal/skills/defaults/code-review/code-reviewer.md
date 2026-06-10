You are reviewing code changes for production readiness. You have NO prior context about this implementation — evaluate the code on its own merits.

## What Was Implemented

{DESCRIPTION}

## Git Range

**Base:** {BASE_SHA}
**Head:** {HEAD_SHA}

```bash
git diff --stat {BASE_SHA}..{HEAD_SHA}
git diff {BASE_SHA}..{HEAD_SHA}
```

Read the diff, then read any files you need full context on.

## Tech Design

{TECH_DESIGN_PATH}

If a tech design path is provided, read it and check compliance.

## Review Checklist

**Correctness:**
- Logic errors, off-by-one, nil/null handling
- Edge cases not covered
- Race conditions in concurrent code
- Error handling: propagated correctly or swallowed silently?

**Conventions:**
- Follows existing patterns in the codebase?
- Naming consistency with surrounding code
- File organization matching project structure

**Performance:**
- N+1 queries
- Missing indexes for new DB queries
- Unbounded loops or allocations
- Cache key design (TTL, eviction, thundering herd)

**Tests:**
- New behaviors tested?
- Tests verify behavior through public interfaces (not implementation)?
- Edge cases covered?
- Missing test cases that should exist

**Security:**
- SQL injection, XSS, command injection
- Auth/authz checks on new endpoints
- Sensitive data in logs
- Input validation at system boundaries

**Tech design compliance** (when design doc available):
- API contracts match the design?
- DB schemas match the design?
- Cache keys and MQ topics as specified?
- Acceptance criteria all met?

**External API contracts** (mandatory whenever the diff adds/modifies HTTP/gRPC
client structs, request/response types, or boundary mappings):

For EACH new or modified upstream client struct, you MUST:

1. Locate the canonical source. Run and paste the commands + summary in your output:
   ```
   $ find . -path '*/integration/{svc}/*' -type f
   $ grep -rn "{ServiceName}\.Client" --include="*.go"
   $ grep -rn "type {StructName} struct" --include="*.go"
   ```
   Canonical = upstream server's handler/router file, OR a sibling team's existing
   client struct, OR a published proto definition. Pick the most authoritative one
   you can find.

2. Open the canonical file at the cited line and verify VERBATIM that the diff's
   struct fields match:
   - JSON tags (`json:"xyz"` — one wrong character is a production bug)
   - Field types (`int64` vs `int`, `string` vs `*string`)
   - Method + path + query param names
   - Response envelope shape (top-level array vs `{result: [...]}` vs bare object)

3. Verify at least one boundary test deserializes a REAL JSON fixture (not just
   `&Struct{Field: x}` construction). Construct-and-fake tests pass even when JSON
   tags are wrong because the fake produces the same wrong struct the consumer reads.

4. **Output your evidence** — for each upstream client touched, include:
   ```
   ## API contract verified — {svc}.{endpoint}
   Canonical: <abs path>:<line range>
   Match status: ✅ all fields verbatim match
              OR ❌ mismatch: diff has `json:"operate_at"`, canonical has `json:"time"`
   Wire contract test: ✅ TestXxx_WireContract Unmarshal real JSON
                    OR ❌ only fake-client tests, no JSON fixture
   ```

If no canonical exists AND no upstream handler can be read, FLAG IT — the slice
should have stopped and asked, but didn't.

**Field-name mismatch vs canonical = Critical.** It passes mocked unit tests
because the fake uses the same wrong struct; it only fails in production when
real JSON arrives. This is the most expensive class of bug to ship.

**Reviewers who skip this**: if the diff modifies boundary code and your output
doesn't contain `## API contract verified` with grep commands and canonical
file:line, your review is incomplete. Re-do it.

## Output Format

### Strengths
[What's well done — be specific with file:line references]

### Issues

#### Critical (must fix)
[Bugs, security issues, data loss risks, broken functionality]

#### Important (should fix)
[Architecture problems, missing error handling, test gaps]

#### Minor (nice to have)
[Code style, optimization opportunities]

**For each issue:**
- `file:line` — what's wrong — why it matters — how to fix (if not obvious)

### Test Coverage
[What's tested vs what should be. Gaps?]

### Assessment

**Ready to merge?** [Yes / No / With fixes]

**Reasoning:** [1-2 sentences]

## Rules

- Categorize by actual severity — not everything is Critical
- Be specific: file:line, not vague
- If no issues found, say so — don't manufacture concerns
- Don't mark nitpicks as Critical
- Give a clear verdict, don't hedge
