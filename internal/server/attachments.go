package server

import (
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
)

// User attachments sent over the WebSocket message payload. Images arrive as
// data URLs (the composer compresses them client-side); documents were already
// uploaded via POST /api/upload and arrive as an /api/uploads/ URL reference.

// userAttachments is the parsed, persisted form of a WS message's files array.
type userAttachments struct {
	// blocks are image content blocks for the model: decoded bytes plus the
	// path of the on-disk copy the session transcript will reference.
	blocks []agent.ContentBlock
	// images are frontend display refs for the user bubble: an /api/uploads/
	// URL for an image thumbnail, or a "pdf:<name>" badge sentinel for a
	// document (the convention the history renderer already understands).
	images []string
	// notes are text lines folded into the message so the model learns the
	// on-disk path of each document attachment (read_file can open them).
	notes []string
}

// ensureUploadsDir returns ~/.octo/uploads, creating it if needed.
func ensureUploadsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("uploads: home dir: %w", err)
	}
	dir := filepath.Join(home, ".octo", uploadsDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("uploads: mkdir: %w", err)
	}
	return dir, nil
}

// parseUserFiles converts a WS files payload into model blocks, display refs,
// and document path notes. Per-file failures are logged and skipped so one bad
// attachment doesn't drop the rest of the message.
func parseUserFiles(files []wsUserFile) userAttachments {
	var att userAttachments
	for _, f := range files {
		switch {
		case f.DataURL != "":
			block, url, err := saveImageAttachment(f.Name, f.DataURL)
			if err != nil {
				log.Printf("[ws] image attachment %q: %v", f.Name, err)
				continue
			}
			att.blocks = append(att.blocks, block)
			att.images = append(att.images, url)
		case f.Path != "":
			dir, err := ensureUploadsDir()
			if err != nil {
				log.Printf("[ws] file attachment %q: %v", f.Name, err)
				continue
			}
			// The composer sends back the /api/uploads/<name> URL that
			// POST /api/upload returned; map it to the file on disk.
			abs := filepath.Join(dir, filepath.Base(f.Path))
			if _, err := os.Stat(abs); err != nil {
				log.Printf("[ws] file attachment %q: %v", f.Name, err)
				continue
			}
			att.images = append(att.images, "pdf:"+f.Name)
			att.notes = append(att.notes, fmt.Sprintf("[Attached file: %s]", abs))
		}
	}
	return att
}

// saveImageAttachment decodes a base64 data URL, persists the bytes under
// ~/.octo/uploads (the transcript stores the path, never the bytes), and
// returns the model-facing image block plus the /api/uploads/ display URL.
func saveImageAttachment(name, dataURL string) (agent.ContentBlock, string, error) {
	mime, data, err := decodeDataURL(dataURL)
	if err != nil {
		return agent.ContentBlock{}, "", err
	}

	dir, err := ensureUploadsDir()
	if err != nil {
		return agent.ContentBlock{}, "", err
	}

	// Basename only, no traversal; the stored extension is derived from the
	// actual MIME type (the composer re-encodes to JPEG but keeps the original
	// filename) so rehydration and Content-Type sniffing stay truthful.
	base := strings.ReplaceAll(filepath.Base(name), "..", "_")
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = "image"
	}
	base = strings.TrimSuffix(base, filepath.Ext(base))
	dstName := fmt.Sprintf("%d_%s%s", time.Now().UnixNano(), base, extForImageMIME(mime))
	dstPath := filepath.Join(dir, dstName)
	if err := os.WriteFile(dstPath, data, 0o600); err != nil {
		return agent.ContentBlock{}, "", fmt.Errorf("write %s: %w", dstPath, err)
	}

	block := agent.NewImageBlock(mime, data)
	block.ImagePath = dstPath
	return block, "/api/uploads/" + dstName, nil
}

// decodeDataURL splits "data:<mime>;base64,<payload>" into its parts.
func decodeDataURL(dataURL string) (mime string, data []byte, err error) {
	rest, ok := strings.CutPrefix(dataURL, "data:")
	if !ok {
		return "", nil, fmt.Errorf("not a data URL")
	}
	meta, payload, ok := strings.Cut(rest, ",")
	if !ok {
		return "", nil, fmt.Errorf("malformed data URL")
	}
	mime, _, _ = strings.Cut(meta, ";")
	if !strings.HasPrefix(mime, "image/") {
		return "", nil, fmt.Errorf("unsupported attachment type %q", mime)
	}
	data, err = base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", nil, fmt.Errorf("decode base64: %w", err)
	}
	if len(data) == 0 {
		return "", nil, fmt.Errorf("empty image payload")
	}
	return mime, data, nil
}

// extForImageMIME picks the stored file extension for an image MIME type.
func extForImageMIME(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	default:
		return ".jpg"
	}
}

// ─── GET /api/uploads/{name} ────────────────────────────────────────────────

// handleGetUpload serves a previously uploaded attachment back to the browser;
// image thumbnails in chat history reference /api/uploads/<name>.
func (s *Server) handleGetUpload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" || name != filepath.Base(name) || strings.Contains(name, "..") {
		writeError(w, http.StatusBadRequest, "invalid file name")
		return
	}
	dir, err := ensureUploadsDir()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.ServeFile(w, r, filepath.Join(dir, name))
}
