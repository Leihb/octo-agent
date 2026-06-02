#!/bin/sh
# llm-judge.sh — score an artifact against a rubric with an LLM, gate on a
# threshold. Used by open-ended generative tasks whose quality ("is this an
# artistic photographer homepage?") no deterministic check can capture.
#
# Usage:
#   llm-judge.sh <rubric_file> <threshold 0-10> <artifact_file> [more files...]
#
# Exit: 0 iff score >= threshold; 1 if below; 2 on any harness/API failure
# (missing key, bad response). It fails CLOSED — an unreachable judge never
# silently passes a task.
#
# Env: OPENAI_API_KEY, OPENAI_BASE_URL (default https://api.openai.com), and a
# judge model from OCTO_EVAL_JUDGE_MODEL (preferred — judging with a different
# model than the one under test avoids self-grading) or OPENAI_MODEL.
set -e

rubric_file="$1"
threshold="$2"
shift 2 || { echo "llm-judge: usage: llm-judge.sh <rubric> <threshold> <files...>" >&2; exit 2; }

model="${OCTO_EVAL_JUDGE_MODEL:-$OPENAI_MODEL}"
base="${OPENAI_BASE_URL:-https://api.openai.com}"
key="$OPENAI_API_KEY"
if [ -z "$key" ] || [ -z "$model" ]; then
	echo "llm-judge: need OPENAI_API_KEY and a judge model (OCTO_EVAL_JUDGE_MODEL or OPENAI_MODEL)" >&2
	exit 2
fi
if [ ! -f "$rubric_file" ]; then
	echo "llm-judge: rubric not found: $rubric_file" >&2
	exit 2
fi

rubric=$(cat "$rubric_file")
artifacts=""
for f in "$@"; do
	if [ ! -f "$f" ]; then
		echo "llm-judge: artifact not found: $f" >&2
		exit 2
	fi
	artifacts="$artifacts

===== $f =====
$(cat "$f")"
done

python3 - "$model" "$base" "$key" "$threshold" "$rubric" "$artifacts" <<'PY'
import sys, json, re, urllib.request

model, base, key, threshold, rubric, artifacts = sys.argv[1:7]

prompt = f"""You are a strict, fair design and quality judge. Score the artifact(s)
against the rubric on a 0-10 integer scale (10 = excellent on every rubric point,
0 = missing or broken). Judge only what the rubric asks for. Be honest: a bland,
templated, or incomplete result should score low.

Return ONLY compact JSON, nothing else: {{"score": <int 0-10>, "reason": "<one sentence>"}}

RUBRIC:
{rubric}

ARTIFACT(S):
{artifacts}
"""

body = json.dumps({
    "model": model,
    "messages": [{"role": "user", "content": prompt}],
    "temperature": 0,
}).encode()

req = urllib.request.Request(
    base.rstrip("/") + "/v1/chat/completions",
    data=body,
    headers={"Authorization": f"Bearer {key}", "Content-Type": "application/json"},
)
try:
    resp = urllib.request.urlopen(req, timeout=180)
    data = json.load(resp)
except Exception as e:
    print(f"llm-judge: API call failed: {e}", file=sys.stderr)
    sys.exit(2)

try:
    content = data["choices"][0]["message"]["content"]
except (KeyError, IndexError):
    print(f"llm-judge: unexpected response shape: {json.dumps(data)[:300]}", file=sys.stderr)
    sys.exit(2)

m = re.search(r"\{.*\}", content, re.S)
if not m:
    print(f"llm-judge: no JSON verdict in response: {content[:300]}", file=sys.stderr)
    sys.exit(2)
try:
    verdict = json.loads(m.group(0))
    score = int(verdict.get("score", -1))
except Exception as e:
    print(f"llm-judge: could not parse verdict ({e}): {content[:300]}", file=sys.stderr)
    sys.exit(2)

reason = str(verdict.get("reason", "")).strip()
thr = int(threshold)
status = "PASS" if score >= thr else "FAIL"
print(f"llm-judge: {status} score={score}/10 (threshold {thr}) — {reason}")
sys.exit(0 if score >= thr else 1)
PY
