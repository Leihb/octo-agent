package tools

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Leihb/octo-agent/internal/agent"
)

// imageExtensions maps file extensions to their MIME types for images
// that multimodal models can consume.
var imageExtensions = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".bmp":  "image/bmp",
	".webp": "image/webp",
	".ico":  "image/x-icon",
	".tiff": "image/tiff",
	".heic": "image/heic",
}

// ReadFileMaxLines caps how many lines a single read_file call returns.
// Past this, the caller must paginate via offset/limit. Matches the cap
// Claude Code's Read tool uses (helps keep LLM context sane).
const ReadFileMaxLines = 2000

// ReadFileImageMaxBytes is the maximum image file size read_file will
// embed for multimodal models. Past this the user gets an error suggesting
// they resize or compress the image.
const ReadFileImageMaxBytes = 5 * 1024 * 1024 // 5 MB

// ReadFileTool reads a UTF-8 text file, optionally a window of it, and
// returns its content with each line prefixed by its 1-based line number
// (the `cat -n` format the LLM is already familiar with).
//
// For image files (.png, .jpg, .jpeg, .gif, .webp, .bmp, .tiff, .heic, .ico)
// the tool reads the raw bytes and returns them as an image content block
// for multimodal model consumption alongside a short text description.
type ReadFileTool struct{}

func (ReadFileTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "read_file",
		Description: "Read a UTF-8 text file and return its content with cat-n-style " +
			"line numbers. Up to 2000 lines per call — use offset/limit to paginate. " +
			"Absolute paths are preferred; relative paths resolve against the current " +
			"working directory. Refuses binary extensions (executables, archives, " +
			"PDFs, DBs) and blocking device files (/dev/random, /dev/tty etc). " +
			"Image files (.png, .jpg, .jpeg, .gif, .webp, .bmp, .tiff, .heic, .ico) " +
			"are returned as image content for multimodal model consumption.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path (absolute preferred). Image files are supported for multimodal models.",
				},
				"offset": map[string]any{
					"type":        "integer",
					"description": "1-based line number to start from. Defaults to 1 (beginning of file). Only applies to text files.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum lines to return. Defaults to 2000. Only applies to text files.",
				},
			},
			"required": []string{"path"},
		},
	}
}

func (ReadFileTool) Execute(_ context.Context, _ string, input map[string]any) (agent.ToolResult, error) {
	path, _ := input["path"].(string)
	if strings.TrimSpace(path) == "" {
		return agent.ToolResult{}, fmt.Errorf("read_file: path is required")
	}
	// Refuse UNC / network paths (\\server\share, //server/share) before
	// resolving. On Windows these trigger an SMB connection that can leak
	// NTLM credentials to an attacker-controlled host; nowhere does a
	// legitimate read need one.
	if isUNCPath(path) {
		return agent.ToolResult{}, fmt.Errorf("read_file: refusing network/UNC path %q — it could leak credentials to a remote host", path)
	}
	abs, err := resolvePath(path)
	if err != nil {
		return agent.ToolResult{}, err
	}

	// Refuse device files that would block forever or generate infinite
	// output. /dev/null is permitted because it just returns EOF immediately.
	if isBlockedDevicePath(abs) {
		return agent.ToolResult{}, fmt.Errorf("read_file: refusing to read %s — device file would block or stream indefinitely", path)
	}

	// Check for image files first — multimodal models can consume them.
	if mimeType := imageMIMEType(abs); mimeType != "" {
		return readImageFile(abs, path, mimeType)
	}

	// Refuse non-image binaries by extension. Cheaper than mime-sniffing
	// and stops the LLM from blowing its context on a multi-MB .exe.
	if reason := isLikelyBinaryPath(abs); reason != "" {
		return agent.ToolResult{}, fmt.Errorf("read_file: refusing to read %s — %s", path, reason)
	}

	offset := intArg(input, "offset", 1)
	if offset < 1 {
		offset = 1
	}
	limit := intArg(input, "limit", ReadFileMaxLines)
	if limit < 1 || limit > ReadFileMaxLines {
		limit = ReadFileMaxLines
	}

	f, err := os.Open(abs)
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("read_file: open %q: %w", path, err)
	}
	defer f.Close()

	// Larger initial buffer than bufio.Scanner's 64KiB default so long
	// lines (minified JS, base64 blobs) don't trip MaxScanTokenSize.
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	var (
		out      strings.Builder
		lineNum  int
		returned int
	)
	for scanner.Scan() {
		lineNum++
		if lineNum < offset {
			continue
		}
		if returned >= limit {
			break
		}
		// Width-6 line number column matches `cat -n` so a 100k-line file
		// still aligns. Tab after the column keeps Markdown/code blocks
		// rendering correctly in UIs.
		fmt.Fprintf(&out, "%6d\t%s\n", lineNum, scanner.Text())
		returned++
	}
	if err := scanner.Err(); err != nil {
		return agent.ToolResult{}, fmt.Errorf("read_file: scan %q: %w", path, err)
	}

	if returned == 0 && lineNum > 0 {
		return agent.ToolResult{}, fmt.Errorf("read_file: offset %d is past end of file (only %d lines)", offset, lineNum)
	}
	if returned == 0 {
		return agent.ToolResult{Text: "(empty file)"}, nil
	}
	return agent.ToolResult{Text: out.String()}, nil
}

// imageMIMEType returns the MIME type for known image extensions, or "".
func imageMIMEType(absPath string) string {
	ext := strings.ToLower(filepath.Ext(absPath))
	return imageExtensions[ext]
}

// readImageFile reads an image file and returns a ToolResult with both a
// text description and an image content block for multimodal models.
func readImageFile(absPath, displayPath, mimeType string) (agent.ToolResult, error) {
	info, err := os.Stat(absPath)
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("read_file: stat %q: %w", displayPath, err)
	}
	if info.Size() > ReadFileImageMaxBytes {
		return agent.ToolResult{}, fmt.Errorf(
			"read_file: image %s is %s — exceeds %s limit. Resize or compress before reading.",
			displayPath, formatBytes(info.Size()), formatBytes(ReadFileImageMaxBytes),
		)
	}

	data, err := os.ReadFile(absPath)
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("read_file: read %q: %w", displayPath, err)
	}

	desc := fmt.Sprintf("Image: %s (%s, %s)", displayPath, mimeType, formatBytes(info.Size()))
	return agent.ToolResult{
		Text:   desc,
		Blocks: []agent.ContentBlock{agent.NewImageBlock(mimeType, data)},
	}, nil
}

// formatBytes renders a byte count in human-readable form.
func formatBytes(n int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
	)
	switch {
	case n >= MB:
		return fmt.Sprintf("%.1f MB", float64(n)/MB)
	case n >= KB:
		return fmt.Sprintf("%.1f KB", float64(n)/KB)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// isUNCPath reports whether path is a UNC / network path. Both the Windows
// backslash form (\\server\share) and the forward-slash form (//server/share,
// which SMB clients also accept) are treated as UNC. A genuine local path
// never needs a double-separator prefix, so this is safe to refuse outright.
func isUNCPath(path string) bool {
	return strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, "//")
}

// resolvePath normalises path → absolute, expanding "~" and cleaning.
func resolvePath(path string) (string, error) {
	if strings.HasPrefix(path, "~/") || path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve ~: %w", err)
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	if !filepath.IsAbs(path) {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve cwd: %w", err)
		}
		path = filepath.Join(cwd, path)
	}
	return filepath.Clean(path), nil
}

// intArg pulls an int from input, accepting either int or float64
// (JSON numbers arrive as float64 from encoding/json).
func intArg(input map[string]any, key string, dflt int) int {
	switch v := input[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return dflt
}

// binaryExtensions is the set of file extensions read_file refuses outright.
// The list focuses on formats that are almost never useful as text in an
// LLM context (executables, archives, audio/video, compiled artefacts).
// Images (.png, .jpg, etc.) are handled separately via imageExtensions —
// they're returned as image content blocks for multimodal models.
// PDFs remain refused because they need separate text extraction tooling.
var binaryExtensions = map[string]string{
	// Executables / libraries
	".exe": "Windows executable", ".dll": "Windows library",
	".so": "Linux shared object", ".dylib": "macOS dynamic library",
	".o": "object file", ".a": "static archive", ".lib": "library",
	".class": "Java class file", ".jar": "Java archive",
	".pyc": "compiled Python", ".pyo": "compiled Python",
	".wasm": "WebAssembly",
	// Archives
	".zip": "ZIP archive", ".tar": "tar archive", ".gz": "gzip archive",
	".bz2": "bzip2 archive", ".xz": "xz archive", ".7z": "7-zip archive",
	".rar": "RAR archive",
	// Audio / video
	".mp3": "MP3 audio", ".mp4": "MP4 video", ".mov": "video",
	".avi": "video", ".mkv": "video", ".webm": "video",
	".ogg": "audio", ".wav": "audio", ".flac": "audio", ".m4a": "audio",
	// Fonts
	".ttf": "font", ".otf": "font", ".woff": "web font", ".woff2": "web font",
	// Documents (non-text)
	".pdf": "PDF document (use a PDF-aware tool)",
	".doc": "Word document", ".docx": "Word document",
	".xls": "Excel spreadsheet", ".xlsx": "Excel spreadsheet",
	".ppt": "PowerPoint", ".pptx": "PowerPoint",
	// Databases / binary data
	".db": "database", ".sqlite": "SQLite database", ".sqlite3": "SQLite database",
	".dat": "binary data",
}

// isLikelyBinaryPath returns a human-readable reason if the path's
// extension is on the binary-refusal list, otherwise "".
func isLikelyBinaryPath(absPath string) string {
	ext := strings.ToLower(filepath.Ext(absPath))
	if reason, ok := binaryExtensions[ext]; ok {
		return reason + " (read_file only handles text)"
	}
	return ""
}

// blockedDevicePaths are paths whose read would block the agent forever
// or generate effectively-infinite output. /dev/null is intentionally
// absent — reading it just returns EOF and is safe.
var blockedDevicePaths = map[string]struct{}{
	"/dev/random":  {},
	"/dev/urandom": {},
	"/dev/zero":    {},
	"/dev/full":    {},
	"/dev/tty":     {},
	"/dev/stdin":   {},
	"/dev/stdout":  {},
	"/dev/stderr":  {},
}

// isBlockedDevicePath reports whether the absolute path names a device
// file that would block or stream indefinitely. The list is exact-match
// only; /dev/pts/* and similar slot-allocated TTYs aren't covered here
// because the LLM has no business reading them in the first place and
// they'd surface via "would block" rather than "infinite output".
func isBlockedDevicePath(absPath string) bool {
	_, blocked := blockedDevicePaths[absPath]
	return blocked
}
