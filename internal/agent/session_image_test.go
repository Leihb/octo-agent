package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// LoadSession must reload persisted image attachments from ImagePath (bytes
// are never serialised into the transcript), and degrade gracefully — the
// anthropic adapter hard-errors on an image block with no data, so a husk
// left in history would fail every later turn of the resumed session.
func TestLoadSession_RehydratesImageBlocks(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	imgPath := filepath.Join(tmp, "pic.png")
	payload := []byte{0x89, 'P', 'N', 'G', 1, 2, 3}
	if err := os.WriteFile(imgPath, payload, 0o600); err != nil {
		t.Fatalf("write image: %v", err)
	}

	sess := NewSession("m", "")
	block := ContentBlock{Type: "image", ImagePath: imgPath}
	sess.Messages = append(sess.Messages, Message{
		Role:   RoleUser,
		Blocks: []ContentBlock{NewTextBlock("look at this"), block},
	})
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := LoadSession(sess.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	blocks := loaded.Messages[0].Blocks
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(blocks))
	}
	img := blocks[1]
	if img.Type != "image" || img.Image == nil {
		t.Fatalf("image block not rehydrated: %+v", img)
	}
	if string(img.Image.Data) != string(payload) {
		t.Errorf("rehydrated bytes mismatch")
	}
	if img.Image.MIMEType != "image/png" {
		t.Errorf("MIMEType = %q, want image/png", img.Image.MIMEType)
	}
}

func TestLoadSession_MissingImageFileBecomesPlaceholder(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	sess := NewSession("m", "")
	sess.Messages = append(sess.Messages, Message{
		Role: RoleUser,
		Blocks: []ContentBlock{
			{Type: "image", ImagePath: filepath.Join(tmp, "gone.jpg")},
			{Type: "image"}, // legacy husk saved before ImagePath existed
		},
	})
	if err := sess.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := LoadSession(sess.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for i, b := range loaded.Messages[0].Blocks {
		if b.Type != "text" {
			t.Errorf("block %d type = %q, want text placeholder", i, b.Type)
		}
		if !strings.Contains(b.Text, "no longer available") {
			t.Errorf("block %d text = %q", i, b.Text)
		}
	}
}
