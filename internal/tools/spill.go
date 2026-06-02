package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// TerminalSpillBytes is the size past which terminal output is written to a
// temp file instead of being returned to the LLM inline. ~16 KB (~4k tokens)
// is comfortably "too long" for a single tool result; below it the output is
// handed back unchanged.
const TerminalSpillBytes = 16 * 1024

// spillHeadLines and spillTailLines bound the inline preview when output is
// spilled. Build/test failures put their error at the tail, so we keep both
// ends — the common case is answered without the agent reading the file.
const (
	spillHeadLines = 50
	spillTailLines = 50
)

// spillMaxAge is how long a spill file lives before a later spill sweeps it.
// Files from a clean shutdown are removed immediately (CleanSpillFiles); this
// only catches the leftovers of a crashed session.
const spillMaxAge = 24 * time.Hour

// MaybeSpillOutput returns body unchanged when it is small enough to give the
// LLM directly. When body exceeds TerminalSpillBytes — and has more lines than
// the preview would show — it writes the full body to a temp file and returns
// a head+tail preview plus the file path and a one-line read hint, so the
// agent decides how to read the rest (read_file with offset/limit, or grep)
// instead of having the whole blob flood its context.
//
// id names the source (e.g. a background process id) and is woven into the
// temp filename. On any write failure it degrades to returning body unchanged:
// losing context is worse than a missing file.
func MaybeSpillOutput(id, body string) string {
	if len(body) <= TerminalSpillBytes {
		return body
	}
	lines := strings.Split(body, "\n")
	if len(lines) <= spillHeadLines+spillTailLines {
		// A few very long lines — a line-based preview would hide nothing,
		// so a temp file buys us no context savings. Return as-is (bounded
		// by the 1 MiB per-line scanner cap upstream).
		return body
	}

	path, err := writeSpillFile(id, body)
	if err != nil {
		return body
	}

	head := strings.Join(lines[:spillHeadLines], "\n")
	tail := strings.Join(lines[len(lines)-spillTailLines:], "\n")
	marker := fmt.Sprintf(
		"[output too long: %d lines / %s written to\n %s\n showing first %d + last %d lines. read_file (offset/limit) or grep that path for the rest.]",
		len(lines), formatBytes(int64(len(body))), path, spillHeadLines, spillTailLines,
	)
	return head + "\n\n" + marker + "\n\n" + tail
}

// writeSpillFile persists body under ~/.octo/tmp and returns the absolute path.
// The filename carries the source id and this process's pid so concurrent
// sessions never collide and CleanSpillFiles can find its own files.
func writeSpillFile(id, body string) (string, error) {
	dir, err := spillDir()
	if err != nil {
		return "", err
	}
	sweepOldSpillFiles(dir)
	name := fmt.Sprintf("term-%s-%d.log", sanitizeSpillID(id), os.Getpid())
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// spillDir returns (creating if needed) ~/.octo/tmp.
func spillDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".octo", "tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// sanitizeSpillID keeps the filename safe: an id is normally "bg_7", but guard
// against anything with path separators or spaces sneaking in.
func sanitizeSpillID(id string) string {
	if id == "" {
		id = "out"
	}
	repl := func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			return r
		}
		return '_'
	}
	return strings.Map(repl, id)
}

// sweepOldSpillFiles best-effort removes spill files older than spillMaxAge —
// the leftovers of sessions that crashed before CleanSpillFiles ran. Errors
// are ignored; this is housekeeping, not correctness.
func sweepOldSpillFiles(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-spillMaxAge)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "term-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// CleanSpillFiles removes this process's spill files. Wire it into session
// shutdown next to KillAllBackground so a normal exit leaves no leftovers.
func CleanSpillFiles() {
	dir, err := spillDir()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	suffix := fmt.Sprintf("-%d.log", os.Getpid())
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "term-") && strings.HasSuffix(e.Name(), suffix) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
