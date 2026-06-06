// Package trash provides a simple file-level trash (recycle bin) for octo.
//
// When the agent deletes or overwrites a file, the old copy is moved to
// ~/.octo/trash/<project_hash>/<filename>, with a sidecar .meta.json
// recording the original path and deletion timestamp.
package trash

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Entry describes a single trashed file.
type Entry struct {
	ID        string `json:"id"`
	Original  string `json:"original"`
	TrashPath string `json:"trash_path"`
	DeletedAt string `json:"deleted_at"`
	Project   string `json:"project"`
	Size      int64  `json:"size"`
}

type meta struct {
	Original  string `json:"original"`
	DeletedAt string `json:"deleted_at"`
	Project   string `json:"project"`
}

// Dir returns the trash root directory.
func Dir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".octo", "trash")
}

// Move moves a file to the trash, preserving its original path for later
// restoration. It creates the project subdirectory under the trash root,
// copies the file there, and writes a .meta.json sidecar. On success the
// original file is removed.
func Move(originalPath, projectDir string) error {
	if _, err := os.Stat(originalPath); os.IsNotExist(err) {
		return fmt.Errorf("file not found: %s", originalPath)
	}

	trashRoot := Dir()
	projHash := hashProject(projectDir)
	targetDir := filepath.Join(trashRoot, projHash)
	if err := os.MkdirAll(targetDir, 0700); err != nil {
		return err
	}

	// Use a timestamped name to avoid collisions.
	ts := time.Now().Format("20060102-150405")
	base := filepath.Base(originalPath)
	trashName := fmt.Sprintf("%s_%s", ts, base)
	trashPath := filepath.Join(targetDir, trashName)

	// Copy.
	if err := copyFile(originalPath, trashPath); err != nil {
		return err
	}

	// Write meta.
	meta := meta{
		Original:  originalPath,
		DeletedAt: time.Now().UTC().Format(time.RFC3339),
		Project:   projectDir,
	}
	metaData, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(trashPath+".meta.json", metaData, 0600); err != nil {
		os.Remove(trashPath)
		return err
	}

	// Remove original.
	return os.Remove(originalPath)
}

// Restore moves a trashed file back to its original location.
func Restore(id string) error {
	entries, err := List()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.ID == id {
			// Ensure parent directory exists.
			parent := filepath.Dir(e.Original)
			if err := os.MkdirAll(parent, 0755); err != nil {
				return err
			}
			if err := os.Rename(e.TrashPath, e.Original); err != nil {
				return err
			}
			// Clean up meta file.
			os.Remove(e.TrashPath + ".meta.json")
			return nil
		}
	}
	return fmt.Errorf("trash entry not found: %s", id)
}

// List returns all trashed files sorted by deletion time (newest first).
func List() ([]Entry, error) {
	root := Dir()
	var entries []Entry

	dirs, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		projDir := filepath.Join(root, d.Name())
		files, err := os.ReadDir(projDir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || strings.HasSuffix(f.Name(), ".meta.json") {
				continue
			}
			trashPath := filepath.Join(projDir, f.Name())
			metaPath := trashPath + ".meta.json"
			m, err := readMeta(metaPath)
			if err != nil {
				continue
			}
			info, _ := f.Info()
			size := int64(0)
			if info != nil {
				size = info.Size()
			}
			entries = append(entries, Entry{
				ID:        filepath.Base(trashPath),
				Original:  m.Original,
				TrashPath: trashPath,
				DeletedAt: m.DeletedAt,
				Project:   m.Project,
				Size:      size,
			})
		}
	}

	// Sort newest first.
	sortEntries(entries)
	return entries, nil
}

// Empty removes trashed files. Modes: "all", "old" (>7 days), "orphans"
// (project directory no longer exists).
func Empty(mode string) (int, error) {
	entries, err := List()
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	count := 0
	for _, e := range entries {
		remove := false
		switch mode {
		case "all":
			remove = true
		case "old":
			t, err := time.Parse(time.RFC3339, e.DeletedAt)
			if err == nil && t.Before(cutoff) {
				remove = true
			}
		case "orphans":
			if _, err := os.Stat(e.Project); os.IsNotExist(err) {
				remove = true
			}
		}
		if remove {
			os.Remove(e.TrashPath)
			os.Remove(e.TrashPath + ".meta.json")
			count++
		}
	}
	return count, nil
}

// ─── Helpers ──────────────────────────────────────────────────────────────

func hashProject(dir string) string {
	h := sha256.Sum256([]byte(dir))
	return fmt.Sprintf("%x", h[:8])
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

func readMeta(path string) (meta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return meta{}, err
	}
	var m meta
	if err := json.Unmarshal(data, &m); err != nil {
		return meta{}, err
	}
	return m, nil
}

func sortEntries(entries []Entry) {
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			if entries[i].DeletedAt < entries[j].DeletedAt {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}
}
