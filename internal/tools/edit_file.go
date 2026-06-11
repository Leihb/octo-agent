package tools

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
)

// EditFileTool replaces an exact substring inside an existing file. The
// match must be unique unless replace_all is true. Refuses to create the
// file if it doesn't exist — use write_file for that.
type EditFileTool struct{}

// curly quote constants for normalization
const (
	leftSingleCurlyQuote  = '\u2018' // '
	rightSingleCurlyQuote = '\u2019' // '
	leftDoubleCurlyQuote  = '\u201c' // "
	rightDoubleCurlyQuote = '\u201d' // "
)

func (EditFileTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "edit_file",
		Description: "Replace an exact substring in an existing file. " +
			"You MUST read the file with read_file before calling this tool. " +
			"old_string must appear exactly once (or set replace_all=true to swap every occurrence). " +
			"The file must already exist — use write_file to create. " +
			"Include enough surrounding context in old_string (typically 2-4 lines) " +
			"to make it unique. If old_string matches multiple times and replace_all is false, " +
			"the edit will fail — add more context to disambiguate.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path (absolute preferred).",
				},
				"old_string": map[string]any{
					"type":        "string",
					"description": "Exact text to find. Must appear in the file. Include enough surrounding context for it to be unique unless replace_all is set.",
				},
				"new_string": map[string]any{
					"type":        "string",
					"description": "Replacement text. Empty string is allowed (deletes old_string).",
				},
				"replace_all": map[string]any{
					"type":        "boolean",
					"description": "When true, replace every occurrence instead of requiring a unique match. Defaults to false.",
				},
			},
			"required": []string{"path", "old_string", "new_string"},
		},
	}
}

// normalizeQuotes converts curly quotes to straight quotes, handling
// the common case where LLMs output straight quotes but files contain
// curly quotes (or vice versa).
func normalizeQuotes(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case leftSingleCurlyQuote, rightSingleCurlyQuote:
			b.WriteByte('\'')
		case leftDoubleCurlyQuote, rightDoubleCurlyQuote:
			b.WriteByte('"')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// stripTrailingWhitespace removes trailing spaces/tabs from every line while
// preserving line endings. SplitAfter keeps the trailing "\n" on each fragment;
// we peel that newline off before TrimRight and re-attach it, otherwise the
// "\n" (not in the cutset) shields the spaces in front of it and only the final
// line would ever be trimmed. Skipped for markdown files where a trailing
// double-space is a hard line break.
func stripTrailingWhitespace(s string) string {
	lines := strings.SplitAfter(s, "\n")
	for i, line := range lines {
		if nl := strings.HasSuffix(line, "\n"); nl {
			lines[i] = strings.TrimRight(line[:len(line)-1], " \t\r") + "\n"
		} else {
			lines[i] = strings.TrimRight(line, " \t\r")
		}
	}
	return strings.Join(lines, "")
}

// indentWidths are the tab widths tried by the leading-whitespace fallback,
// ordered by likelihood: read_file and the terminal tool render tabs 4 wide,
// 8 is the classic terminal default, 2 is a common compact style models emit.
var indentWidths = []int{4, 8, 2}

// findActualString attempts to locate the search string in the file content,
// first with an exact match, then with quote-normalized match, and finally
// with leading-whitespace normalization (tab → space expansion at several
// widths) so that space-indented old_string from the LLM still matches
// tab-indented files. Returns the actual string found in the file (preserving
// original formatting) and the tab width that matched — 4, the default indent
// unit, unless a wider/narrower expansion was what matched. Empty string means
// not found.
func findActualString(fileContent, searchString string) (string, int) {
	// First try exact match
	if strings.Contains(fileContent, searchString) {
		return searchString, defaultIndentWidth
	}

	// Try with normalized quotes
	normalizedSearch := normalizeQuotes(searchString)
	normalizedFile := normalizeQuotes(fileContent)

	idx := strings.Index(normalizedFile, normalizedSearch)
	if idx != -1 {
		// Map the index back to the original file content.
		// Since normalization only replaces runes with ASCII equivalents
		// of the same byte length (UTF-8 curly quotes are 3 bytes each,
		// straight quotes are 1 byte), we can't use byte offsets directly.
		// Instead, scan through the original file rune-by-rune.
		fileRunes := []rune(fileContent)
		normalizedRunes := []rune(normalizedFile)
		searchRunes := []rune(normalizedSearch)

		// Verify the match at the rune level in normalized space
		if idx+len(searchRunes) > len(normalizedRunes) {
			return "", 0
		}
		for i := 0; i < len(searchRunes); i++ {
			if normalizedRunes[idx+i] != searchRunes[i] {
				return "", 0
			}
		}

		// Map rune index back to original file substring
		// Count runes in original file up to the match position
		origStart := -1
		runeCount := 0
		for i := range fileRunes {
			if runeCount == idx {
				origStart = i
				break
			}
			runeCount++
		}
		if origStart == -1 {
			return "", 0
		}

		origEnd := -1
		runeCount = 0
		for i := range fileRunes {
			if runeCount == idx+len(searchRunes) {
				origEnd = i
				break
			}
			runeCount++
		}
		if origEnd == -1 {
			origEnd = len(fileRunes)
		}

		return string(fileRunes[origStart:origEnd]), defaultIndentWidth
	}

	// Try with normalized leading whitespace (tab → space expansion).
	// This is line-based because indentation differences only affect
	// leading whitespace, and the matching needs to map lines back
	// to the original file. Several widths are tried because the width the
	// model assumed when it turned tabs into spaces is unknowable.
	for _, w := range indentWidths {
		if actual := findWithIndentNorm(fileContent, searchString, w); actual != "" {
			return actual, w
		}
	}

	return "", 0
}

// defaultIndentWidth is the tab width assumed when no whitespace
// normalization was needed to find the match — it matches how read_file and
// the terminal tool render tabs.
const defaultIndentWidth = 4

// normalizeLeadingWhitespace expands tabs in leading whitespace of each line
// to width-space stops, making tab-indented and space-indented text comparable.
// Only leading whitespace is touched — tabs inside line content are left alone.
func normalizeLeadingWhitespace(s string, width int) string {
	var b strings.Builder
	b.Grow(len(s) + len(s)/10)
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed != line {
			leadingLen := len(line) - len(trimmed)
			leading := line[:leadingLen]
			b.WriteString(strings.ReplaceAll(leading, "\t", strings.Repeat(" ", width)))
		}
		b.WriteString(trimmed)
		if i < len(lines)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// findWithIndentNorm matches searchString against fileContent after
// normalizing leading whitespace in both at the given tab width. Returns the
// corresponding substring from the original (unnormalized) fileContent so the
// edit preserves the file's actual whitespace convention.
func findWithIndentNorm(fileContent, searchString string, width int) string {
	// The matching is line-based, so a trailing newline on the search string
	// would otherwise become an empty last line that must equal the file's
	// next line. Strip it for matching and restore it from the file after.
	search := searchString
	wantTrailingNL := strings.HasSuffix(search, "\n")
	if wantTrailingNL {
		search = strings.TrimSuffix(search, "\n")
	}

	normFile := normalizeLeadingWhitespace(fileContent, width)
	normSearch := normalizeLeadingWhitespace(search, width)

	if !strings.Contains(normFile, normSearch) {
		return ""
	}

	fileLines := strings.Split(fileContent, "\n")
	normFileLines := strings.Split(normFile, "\n")
	normSearchLines := strings.Split(normSearch, "\n")

	start := -1
	for i := 0; i <= len(normFileLines)-len(normSearchLines); i++ {
		match := true
		for j := 0; j < len(normSearchLines); j++ {
			if normFileLines[i+j] != normSearchLines[j] {
				match = false
				break
			}
		}
		if match {
			start = i
			break
		}
	}

	if start == -1 {
		return ""
	}

	end := start + len(normSearchLines)
	actual := strings.Join(fileLines[start:end], "\n")

	if wantTrailingNL {
		// The search demands a newline after the last matched line; only
		// honor it if the file actually has one there (i.e. the match did
		// not land on an unterminated final line).
		if end >= len(fileLines) {
			return ""
		}
		actual += "\n"
	}

	return actual
}

func (EditFileTool) Execute(_ context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	path, _ := input["path"].(string)
	if strings.TrimSpace(path) == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("edit_file: path is required")
	}
	oldStr, ok1 := input["old_string"].(string)
	newStr, ok2 := input["new_string"].(string)
	if !ok1 {
		return agent.ToolResult{Text: ""}, fmt.Errorf("edit_file: old_string is required")
	}
	if !ok2 {
		return agent.ToolResult{Text: ""}, fmt.Errorf("edit_file: new_string is required (use empty string to delete)")
	}
	if oldStr == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("edit_file: old_string must be non-empty")
	}
	if oldStr == newStr {
		return agent.ToolResult{Text: ""}, fmt.Errorf("edit_file: old_string and new_string are identical — nothing to do")
	}
	replaceAll, _ := input["replace_all"].(bool)

	abs, err := resolvePath(path)
	if err != nil {
		return agent.ToolResult{Text: ""}, err
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("edit_file: read %q: %w", path, err)
	}
	body := string(data)

	// CRLF handling: an LLM that read the file via read_file (which uses
	// bufio.Scanner — strips `\r` from `\r\n` lines) and then copies a
	// substring back into old_string would compare against `\n`-terminated
	// lines, but the on-disk file may have `\r\n`. Match in normalized
	// (LF) space; if the original was CRLF, restore on write so the file's
	// line-ending convention isn't silently flipped.
	hasCRLF := strings.Contains(body, "\r\n")
	bodyForMatch := body
	if hasCRLF {
		bodyForMatch = strings.ReplaceAll(body, "\r\n", "\n")
	}

	// Strip trailing whitespace from new_string (except for markdown files
	// where trailing double-space is a hard line break).
	isMarkdown := strings.HasSuffix(strings.ToLower(abs), ".md") ||
		strings.HasSuffix(strings.ToLower(abs), ".mdx")
	newStrClean := newStr
	if !isMarkdown {
		newStrClean = stripTrailingWhitespace(newStr)
	}

	// Refuse to inject a live-credential shape — same guard write_file applies,
	// so a secret can't slip in through the edit path instead. Only the new text
	// is scanned (not the whole file): a pre-existing match the model didn't
	// introduce shouldn't block an unrelated edit elsewhere in the file.
	if secret := scanForSecrets(newStrClean); secret != "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("edit_file: refusing to write new_string that contains a %s. "+
			"If this is genuinely intended (e.g. a test fixture), remove the live-credential "+
			"shape or create the file outside the agent.", secret)
	}

	// Use findActualString for quote- and indent-normalized matching.
	actualOldStr, indentWidth := findActualString(bodyForMatch, oldStr)
	if actualOldStr == "" {
		// Build a helpful error message showing what we tried
		msg := fmt.Sprintf("edit_file: old_string not found in %s", path)

		// If quote normalization was attempted but still failed, hint at it
		if normalizeQuotes(oldStr) != oldStr {
			msg += " (also tried with normalized quotes)"
		}
		return agent.ToolResult{Text: ""}, fmt.Errorf("%s", msg)
	}

	count := strings.Count(bodyForMatch, actualOldStr)
	if count == 0 {
		// Should not happen after findActualString succeeded, but be safe
		return agent.ToolResult{Text: ""}, fmt.Errorf("edit_file: old_string not found in %s", path)
	}
	if count > 1 && !replaceAll {
		return agent.ToolResult{Text: ""}, fmt.Errorf(
			"edit_file: old_string matches %d times — either include more context to make it unique, or set replace_all=true",
			count,
		)
	}

	// Preserve curly quote style from the matched text when the LLM
	// provided straight quotes but the file uses curly quotes.
	actualNewStr := preserveQuoteStyle(oldStr, actualOldStr, newStrClean)
	// Preserve file indent convention (tabs vs spaces) — when the file
	// uses tabs but the LLM supplied space-indented new_string.
	actualNewStr = preserveIndentStyle(actualOldStr, actualNewStr, indentWidth)

	var updated string
	if replaceAll {
		updated = strings.ReplaceAll(bodyForMatch, actualOldStr, actualNewStr)
	} else {
		updated = strings.Replace(bodyForMatch, actualOldStr, actualNewStr, 1)
	}
	if hasCRLF {
		updated = strings.ReplaceAll(updated, "\n", "\r\n")
	}

	if err := os.WriteFile(abs, []byte(updated), 0o644); err != nil {
		return agent.ToolResult{Text: ""}, fmt.Errorf("edit_file: write %q: %w", path, err)
	}

	occurrences := 1
	if replaceAll {
		occurrences = count
	}
	ui := map[string]any{
		"type":        "edit",
		"path":        abs,
		"occurrences": occurrences,
		"diff":        editUIDiff(actualOldStr, actualNewStr),
	}
	if replaceAll {
		return agent.ToolResult{Text: fmt.Sprintf("Replaced %d occurrence(s) in %s", count, abs), UI: ui}, nil
	}
	return agent.ToolResult{Text: fmt.Sprintf("Replaced 1 occurrence in %s", abs), UI: ui}, nil
}

// editUIDiff renders the replacement as a removed/added block for the web
// UI's diff view. The edit is an exact substring swap, so old/new lines ARE
// the change — no diff algorithm needed.
func editUIDiff(oldStr, newStr string) string {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(oldStr, "\n"), "\n") {
		b.WriteString("- " + l + "\n")
	}
	if newStr != "" {
		for _, l := range strings.Split(strings.TrimRight(newStr, "\n"), "\n") {
			b.WriteString("+ " + l + "\n")
		}
	}
	return uiHead(b.String(), 24, 1600)
}

// preserveQuoteStyle copies curly quote style from actualOldStr to newStr
// when the LLM provided straight quotes but the file uses curly quotes.
func preserveQuoteStyle(oldStr, actualOldStr, newStr string) string {
	// If no normalization happened, return as-is
	if oldStr == actualOldStr {
		return newStr
	}

	// Detect which curly quote types were in the file
	hasDoubleQuotes := strings.ContainsRune(actualOldStr, leftDoubleCurlyQuote) ||
		strings.ContainsRune(actualOldStr, rightDoubleCurlyQuote)
	hasSingleQuotes := strings.ContainsRune(actualOldStr, leftSingleCurlyQuote) ||
		strings.ContainsRune(actualOldStr, rightSingleCurlyQuote)

	if !hasDoubleQuotes && !hasSingleQuotes {
		return newStr
	}

	result := newStr
	if hasDoubleQuotes {
		result = applyCurlyDoubleQuotes(result)
	}
	if hasSingleQuotes {
		result = applyCurlySingleQuotes(result)
	}
	return result
}

func isOpeningContext(chars []rune, index int) bool {
	if index == 0 {
		return true
	}
	prev := chars[index-1]
	return prev == ' ' || prev == '\t' || prev == '\n' || prev == '\r' ||
		prev == '(' || prev == '[' || prev == '{' ||
		prev == '\u2014' || prev == '\u2013'
}

func applyCurlyDoubleQuotes(str string) string {
	chars := []rune(str)
	result := make([]rune, len(chars))
	for i, ch := range chars {
		if ch == '"' {
			if isOpeningContext(chars, i) {
				result[i] = leftDoubleCurlyQuote
			} else {
				result[i] = rightDoubleCurlyQuote
			}
		} else {
			result[i] = ch
		}
	}
	return string(result)
}

func applyCurlySingleQuotes(str string) string {
	chars := []rune(str)
	result := make([]rune, len(chars))
	for i, ch := range chars {
		if ch == '\'' {
			// Don't convert apostrophes in contractions (e.g., "don't", "it's")
			prevIsLetter := i > 0 && isLetter(chars[i-1])
			nextIsLetter := i < len(chars)-1 && isLetter(chars[i+1])
			if prevIsLetter && nextIsLetter {
				result[i] = rightSingleCurlyQuote
			} else if isOpeningContext(chars, i) {
				result[i] = leftSingleCurlyQuote
			} else {
				result[i] = rightSingleCurlyQuote
			}
		} else {
			result[i] = ch
		}
	}
	return string(result)
}

func isLetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// preserveIndentStyle converts leading whitespace in newStr to match the
// file's indentation convention (detected from actualOldStr). When the file
// uses tabs and the LLM supplied space-indented new_string, each run of
// `width` spaces becomes one tab — width being the tab width that located the
// match, so the conversion mirrors the expansion. Remainder spaces (alignment,
// or a partial level) are kept rather than truncated away, so a 2-space line
// is never silently stripped to column zero. Lines whose leading whitespace
// already contains a tab are left untouched: the model followed the file's
// convention, and rewriting them would flatten tab+space alignment.
func preserveIndentStyle(actualOldStr, newStr string, width int) string {
	// Only convert when the file uses tabs for indentation
	if !strings.Contains(actualOldStr, "\n\t") && !strings.HasPrefix(actualOldStr, "\t") {
		return newStr
	}

	newLines := strings.Split(newStr, "\n")
	for i, line := range newLines {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == line {
			continue
		}
		leading := line[:len(line)-len(trimmed)]
		if strings.Contains(leading, "\t") {
			continue
		}
		tabs := len(leading) / width
		rem := len(leading) % width
		newLines[i] = strings.Repeat("\t", tabs) + strings.Repeat(" ", rem) + trimmed
	}
	return strings.Join(newLines, "\n")
}
