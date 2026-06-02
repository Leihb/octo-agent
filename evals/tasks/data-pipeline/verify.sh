#!/bin/sh
# Deterministic: result.txt must hold the canonical total (1034.00). cwd is the
# working copy. The expected value is computed from repo/input.csv:
#   awk -F, 'NR>1 {s+=$3*$4} END {printf "%.2f\n", s}'  ->  1034.00
[ -f result.txt ] || { echo "FAIL: result.txt was not created"; exit 1; }
val=$(tr -d ' \t\r\n' < result.txt)
case "$val" in
	1034.00 | 1034.0 | 1034) exit 0 ;;
	*) echo "FAIL: result.txt = '$val', want 1034.00"; exit 1 ;;
esac
