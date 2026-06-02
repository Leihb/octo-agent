#!/bin/sh
# Structural gate (deterministic) → LLM quality score. cwd is the working copy.
here=$(CDPATH= cd "$(dirname "$0")" && pwd)

[ -f comparison.md ] || { echo "FAIL: comparison.md was not created"; exit 1; }
[ "$(wc -c < comparison.md)" -gt 800 ] || { echo "FAIL: comparison.md too short (stub?)"; exit 1; }
grep -qi 'rest' comparison.md && grep -qi 'grpc' comparison.md || { echo "FAIL: must discuss both REST and gRPC"; exit 1; }
grep -qE '^#{1,6} ' comparison.md || { echo "FAIL: no Markdown headings"; exit 1; }
grep -qE '^\|.*\|' comparison.md || { echo "FAIL: no Markdown table"; exit 1; }

sh "$here/../../lib/llm-judge.sh" "$here/rubric.md" 7 comparison.md
