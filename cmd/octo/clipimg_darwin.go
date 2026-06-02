//go:build darwin

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// captureClipboardImage reads an image from the macOS clipboard and returns
// its raw bytes plus MIME type. It coerces whatever the clipboard holds (a
// screenshot, a copied image) to PNG via AppleScript, which is built in — no
// Homebrew dependency. ok is false (with a friendly reason in err) when the
// clipboard holds no image.
func captureClipboardImage() (data []byte, mime string, err error) {
	tmp, err := os.CreateTemp("", "octo-clip-*.png")
	if err != nil {
		return nil, "", fmt.Errorf("clipboard: temp file: %w", err)
	}
	path := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(path)

	// AppleScript: coerce the clipboard to PNG and write it to path. The
	// coercion fails (caught) when the clipboard has no image, in which case
	// we return "NO_IMAGE" so the caller can show a friendly notice rather
	// than a raw osascript error.
	script := fmt.Sprintf(`try
	set imgData to (the clipboard as «class PNGf»)
on error
	return "NO_IMAGE"
end try
set f to open for access POSIX file %q with write permission
set eof f to 0
write imgData to f
close access f
return "OK"`, path)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "osascript", "-e", script).Output()
	if err != nil {
		return nil, "", fmt.Errorf("clipboard: osascript: %w", err)
	}
	if strings.TrimSpace(string(out)) == "NO_IMAGE" {
		return nil, "", fmt.Errorf("no image in clipboard")
	}

	data, err = os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("clipboard: read: %w", err)
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("no image in clipboard")
	}
	return data, "image/png", nil
}
