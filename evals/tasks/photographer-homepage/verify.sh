#!/bin/sh
# Structural gate (deterministic) → LLM quality score. cwd is the working copy.
here=$(CDPATH= cd "$(dirname "$0")" && pwd)

[ -f index.html ] || { echo "FAIL: index.html was not created"; exit 1; }
grep -qiE '<!doctype html|<html' index.html || { echo "FAIL: index.html is not HTML"; exit 1; }
[ "$(wc -c < index.html)" -gt 1500 ] || { echo "FAIL: index.html too small for a full homepage (stub?)"; exit 1; }
# Self-contained: no external stylesheet/script, no remote images.
if grep -qiE '<link[^>]+stylesheet|<script[^>]+src=' index.html; then
	echo "FAIL: external CSS/JS reference (must be self-contained)"; exit 1
fi
if grep -qiE '(src|href)=["'\'']https?://' index.html; then
	echo "FAIL: remote URL referenced (no hotlinked images/CDNs allowed)"; exit 1
fi

sh "$here/../../lib/llm-judge.sh" "$here/rubric.md" 7 index.html
