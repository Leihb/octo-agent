package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
)

func jpegDataURL(payload []byte) string {
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(payload)
}

func TestParseUserFiles_ImageDataURL(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	payload := []byte{0xFF, 0xD8, 0xFF, 1, 2, 3}
	att := parseUserFiles([]wsUserFile{
		{Name: "shot.png", MimeType: "image/jpeg", DataURL: jpegDataURL(payload)},
	})

	if len(att.blocks) != 1 || len(att.images) != 1 || len(att.notes) != 0 {
		t.Fatalf("blocks/images/notes = %d/%d/%d, want 1/1/0",
			len(att.blocks), len(att.images), len(att.notes))
	}
	b := att.blocks[0]
	if b.Type != "image" || b.Image == nil {
		t.Fatalf("block = %+v", b)
	}
	if string(b.Image.Data) != string(payload) {
		t.Errorf("decoded bytes mismatch")
	}
	if b.Image.MIMEType != "image/jpeg" {
		t.Errorf("MIMEType = %q", b.Image.MIMEType)
	}
	// Persisted copy exists, carries a MIME-true extension, and the display
	// URL references it.
	if b.ImagePath == "" {
		t.Fatal("ImagePath not set")
	}
	if filepath.Ext(b.ImagePath) != ".jpg" {
		t.Errorf("stored ext = %q, want .jpg (MIME-derived)", filepath.Ext(b.ImagePath))
	}
	onDisk, err := os.ReadFile(b.ImagePath)
	if err != nil || string(onDisk) != string(payload) {
		t.Errorf("persisted copy: err=%v, match=%v", err, string(onDisk) == string(payload))
	}
	wantURL := "/api/uploads/" + filepath.Base(b.ImagePath)
	if att.images[0] != wantURL {
		t.Errorf("display URL = %q, want %q", att.images[0], wantURL)
	}
}

func TestParseUserFiles_DocPath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	dir, err := ensureUploadsDir()
	if err != nil {
		t.Fatalf("uploads dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "123_report.pdf"), []byte("pdf"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	att := parseUserFiles([]wsUserFile{
		{Name: "report.pdf", MimeType: "application/pdf", Path: "/api/uploads/123_report.pdf"},
	})

	if len(att.blocks) != 0 || len(att.images) != 1 || len(att.notes) != 1 {
		t.Fatalf("blocks/images/notes = %d/%d/%d, want 0/1/1",
			len(att.blocks), len(att.images), len(att.notes))
	}
	if att.images[0] != "pdf:report.pdf" {
		t.Errorf("badge sentinel = %q", att.images[0])
	}
	wantPath := filepath.Join(dir, "123_report.pdf")
	if !strings.Contains(att.notes[0], wantPath) {
		t.Errorf("note %q does not reference %q", att.notes[0], wantPath)
	}
}

func TestParseUserFiles_SkipsBadEntries(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	att := parseUserFiles([]wsUserFile{
		{Name: "bad.jpg", DataURL: "not-a-data-url"},
		{Name: "evil.pdf", Path: "/api/uploads/does-not-exist.pdf"},
		{Name: "text.txt", DataURL: "data:text/plain;base64,aGk="}, // non-image data URL
	})
	if len(att.blocks) != 0 || len(att.images) != 0 || len(att.notes) != 0 {
		t.Errorf("bad entries not skipped: %+v", att)
	}
}

func TestHandleGetUpload(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	dir, err := ensureUploadsDir()
	if err != nil {
		t.Fatalf("uploads dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1_pic.jpg"), []byte("img-bytes"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	srv := mustServer(t, Config{Addr: "127.0.0.1:0"})
	ts := httptest.NewServer(srv.http.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/uploads/1_pic.jpg")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	// Traversal must be rejected.
	for _, bad := range []string{"..%2F..%2Fsecret", "a%2Fb.jpg"} {
		resp, err := http.Get(ts.URL + "/api/uploads/" + bad)
		if err != nil {
			t.Fatalf("get %s: %v", bad, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("traversal %q served with 200", bad)
		}
	}
}

// TestHandleWSUserMessage_ImageOnly is the regression guard for the web-UI
// "sent an image, nothing happened" bug: handleWSUserMessage ignored
// msg.Files entirely and returned early on empty text, silently dropping
// image-only messages.
func TestHandleWSUserMessage_ImageOnly(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	srv := mustServer(t, Config{Addr: "127.0.0.1:0", Tools: false})
	srv.initWS()
	srv.turnRunning = make(map[string]bool)
	srv.steerQueues = make(map[string][]string)
	srv.sessionAgents = make(map[string]*agent.Agent)

	sess := agent.NewSession("stub-model", "")
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	conn := &wsConn{
		hub:        srv.wsHub,
		send:       make(chan []byte, 256),
		subscribed: map[string]struct{}{},
	}
	srv.wsHub.register <- conn
	srv.wsHub.subscribe(conn, sess.ID)

	payload := []byte{0xFF, 0xD8, 0xFF, 9, 9}
	srv.handleWSUserMessage(conn, &wsMsgUserMessage{
		SessionID: sess.ID,
		Content:   json.RawMessage(`""`),
		Files: []wsUserFile{
			{Name: "shot.jpg", MimeType: "image/jpeg", DataURL: jpegDataURL(payload)},
		},
	})

	// Drain broadcasts until the turn completes; the user-message event must
	// carry the thumbnail URL.
	var gotImages bool
	deadline := time.After(5 * time.Second)
drain:
	for {
		select {
		case b := <-conn.send:
			var ev map[string]any
			if err := json.Unmarshal(b, &ev); err != nil {
				continue
			}
			switch ev["type"] {
			case "history_user_message":
				if imgs, ok := ev["images"].([]any); ok && len(imgs) == 1 {
					url, _ := imgs[0].(string)
					if strings.HasPrefix(url, "/api/uploads/") {
						gotImages = true
					}
				}
			case "complete":
				break drain
			}
		case <-deadline:
			t.Fatal("turn did not complete — image-only message still dropped?")
		}
	}
	if !gotImages {
		t.Error("history_user_message carried no /api/uploads/ image ref")
	}

	// The persisted session must carry the image block with its file path.
	loaded, err := agent.LoadSession(sess.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	var foundBlock bool
	for _, m := range loaded.Messages {
		for _, blk := range m.Blocks {
			if blk.Type == "image" && blk.ImagePath != "" && blk.Image != nil {
				foundBlock = true
			}
		}
	}
	if !foundBlock {
		t.Error("no rehydratable image block persisted in session")
	}
}
