//go:build !darwin

package main

import "fmt"

// captureClipboardImage is unsupported off macOS for now. Linux (wl-paste /
// xclip) and Windows (PowerShell Get-Clipboard) can be added behind the same
// signature when needed.
func captureClipboardImage() (data []byte, mime string, err error) {
	return nil, "", fmt.Errorf("clipboard image paste is only supported on macOS right now")
}
