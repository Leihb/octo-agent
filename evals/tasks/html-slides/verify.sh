#!/bin/sh
# Structural gate (deterministic) → LLM quality score. cwd is the working copy.
here=$(CDPATH= cd "$(dirname "$0")" && pwd)

[ -f index.html ] || { echo "FAIL: index.html was not created"; exit 1; }
grep -qiE '<!doctype html|<html' index.html || { echo "FAIL: index.html is not HTML"; exit 1; }
[ "$(wc -c < index.html)" -gt 800 ] || { echo "FAIL: index.html too small to be a real deck (stub?)"; exit 1; }
# Self-contained: no external stylesheet/script references.
if grep -qiE '<link[^>]+stylesheet|<script[^>]+src=' index.html; then
	echo "FAIL: index.html pulls in external CSS/JS (must be self-contained)"; exit 1
fi

sh "$here/../../lib/llm-judge.sh" "$here/rubric.md" 7 index.html
