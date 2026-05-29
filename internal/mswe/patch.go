package mswe

import "strings"

// ScopeFixPatch trims a `git diff` down to the source changes the harness
// should apply, dropping:
//   - test files (*_test.go) — the harness applies the dataset's own hidden
//     test patch on top, so a model patch touching tests would conflict with,
//     or defeat, the hidden tests;
//   - build artifacts (e.g. Go test binaries, *.test) that `git add -A` may
//     sweep in after octo runs `go test`/`go build`;
//   - binary-file sections (a source fix is never a binary blob).
//
// The input is unified diff output where each file section starts with a
// `diff --git a/<path> b/<path>` header; surviving sections are rejoined as-is.
func ScopeFixPatch(diff string) string {
	sections := splitDiffSections(diff)
	var kept []string
	for _, s := range sections {
		if isExcludedFile(diffSectionPath(s)) || isBinarySection(s) {
			continue
		}
		kept = append(kept, s)
	}
	return strings.Join(kept, "")
}

// splitDiffSections breaks a unified diff into per-file sections, each starting
// at a `diff --git ` line and including everything up to the next one.
func splitDiffSections(diff string) []string {
	lines := strings.SplitAfter(diff, "\n")
	var sections []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			sections = append(sections, cur.String())
			cur.Reset()
		}
	}
	for _, ln := range lines {
		if strings.HasPrefix(ln, "diff --git ") {
			flush()
		}
		cur.WriteString(ln)
	}
	flush()
	return sections
}

// diffSectionPath extracts the b/ path from a section's `diff --git a/x b/y`
// header. Returns "" when the header is missing or malformed.
func diffSectionPath(section string) string {
	nl := strings.IndexByte(section, '\n')
	header := section
	if nl >= 0 {
		header = section[:nl]
	}
	if !strings.HasPrefix(header, "diff --git ") {
		return ""
	}
	fields := strings.Fields(strings.TrimPrefix(header, "diff --git "))
	if len(fields) != 2 {
		return ""
	}
	return strings.TrimPrefix(fields[1], "b/")
}

// isExcludedFile reports whether a diff section's file should be dropped from
// the fix patch: Go test files (the harness supplies the hidden tests) and Go
// test binaries (build artifacts swept in by `git add -A`). (The eval is
// Go-only; extend this set if the dataset language scope widens.)
func isExcludedFile(path string) bool {
	return strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, ".test")
}

// isBinarySection reports whether a diff section is a binary-file change (git
// emits "Binary files … differ" or a "GIT binary patch" block) rather than a
// textual source edit. Compiled artifacts have no place in a source fix patch.
func isBinarySection(section string) bool {
	return strings.Contains(section, "\nBinary files ") ||
		strings.Contains(section, "\nGIT binary patch\n")
}
