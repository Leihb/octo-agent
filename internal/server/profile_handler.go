package server

import (
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Leihb/octo-agent/internal/prompt"
	"github.com/Leihb/octo-agent/internal/trash"
)

// ─── Profile API ──────────────────────────────────────────────────────────

func (s *Server) handleGetProfileSoul(w http.ResponseWriter, r *http.Request) {
	path := prompt.IdentityPath(octoDir(), "soul.md")
	content, err := os.ReadFile(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "soul.md not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"content": string(content),
		"path":    path,
	})
}

func (s *Server) handleGetProfileUser(w http.ResponseWriter, r *http.Request) {
	path := prompt.IdentityPath(octoDir(), "user.md")
	content, err := os.ReadFile(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "user.md not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"content": string(content),
		"path":    path,
	})
}

func (s *Server) handleGetMemories(w http.ResponseWriter, r *http.Request) {
	type memFile struct {
		Name      string `json:"name"`
		Path      string `json:"path"`
		Size      int64  `json:"size"`
		UpdatedAt string `json:"updated_at"`
		Source    string `json:"source"`
	}
	files := make([]memFile, 0)

	for _, src := range []struct {
		dir   string
		label string
	}{{
		s.memDir, "project",
	}, {
		s.homeMemDir, "inherited",
	}} {
		if src.dir == "" {
			continue
		}
		entries, err := os.ReadDir(src.dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".md" {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			files = append(files, memFile{
				Name:      e.Name(),
				Path:      filepath.Join(src.dir, e.Name()),
				Size:      info.Size(),
				UpdatedAt: info.ModTime().UTC().Format(time.RFC3339),
				Source:    src.label,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

func (s *Server) handleGetTrash(w http.ResponseWriter, r *http.Request) {
	entries, err := trash.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if entries == nil {
		entries = []trash.Entry{}
	}
	var totalSize int64
	for _, e := range entries {
		totalSize += e.Size
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"files":       entries,
		"total_count": len(entries),
		"total_size":  totalSize,
	})
}

type emptyTrashRequest struct {
	Mode string `json:"mode"` // "all", "old", "orphans"
}

func (s *Server) handleEmptyTrash(w http.ResponseWriter, r *http.Request) {
	var req emptyTrashRequest
	if err := readBodyJSON(r, &req); err != nil {
		req.Mode = "all"
	}
	count, freed, err := trash.Empty(req.Mode)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"removed":    count,
		"freed_size": freed,
	})
}

func (s *Server) handleRestoreTrash(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing file id")
		return
	}
	if err := trash.Restore(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "restored"})
}

func (s *Server) handleDeleteTrash(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing file id")
		return
	}
	entries, err := trash.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, e := range entries {
		if e.ID == id {
			var freed int64
			if info, err := os.Stat(e.TrashPath); err == nil {
				freed = info.Size()
			}
			os.Remove(e.TrashPath)
			os.Remove(e.TrashPath + ".meta.json")
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":         true,
				"freed_size": freed,
			})
			return
		}
	}
	writeError(w, http.StatusNotFound, "trash entry not found")
}

func octoDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".octo")
	}
	return filepath.Join(home, ".octo")
}
