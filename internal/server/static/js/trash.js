// ── Trash — file recall panel ───────────────────────────────────────────
//
// Lists files the agent moved to trash across all projects. Supports
// refresh, empty old (>7 days), empty all.
// ─────────────────────────────────────────────────────────────────────────

const Trash = (() => {
  let _files = [];

  function init() {
    const refreshBtn = $("btn-trash-refresh");
    const emptyOldBtn = $("btn-trash-empty-old");
    const emptyAllBtn = $("btn-trash-empty-all");

    if (refreshBtn) refreshBtn.addEventListener("click", loadFiles);
    if (emptyOldBtn) emptyOldBtn.addEventListener("click", () => {
      if (confirm("Permanently delete files older than 7 days?")) {
        emptyTrash("old");
      }
    });
    if (emptyAllBtn) emptyAllBtn.addEventListener("click", () => {
      if (confirm("Permanently delete ALL trashed files? This cannot be undone.")) {
        emptyTrash("all");
      }
    });
  }

  async function loadFiles() {
    try {
      const res = await api.get("/api/trash");
      if (!res.ok) {
        _files = [];
      } else {
        _files = await res.json();
      }
    } catch (e) {
      _files = [];
    }
    render();
  }

  async function emptyTrash(mode) {
    try {
      await api.post("/api/trash/empty", { mode });
      await loadFiles();
    } catch (e) {
      console.error(e);
    }
  }

  function render() {
    const container = $("trash-list");
    const summary = $("trash-summary");
    if (!container) return;

    if (summary) {
      summary.textContent = _files.length === 0
        ? "No trashed files"
        : `${_files.length} file(s) in trash`;
    }

    if (_files.length === 0) {
      container.innerHTML = `<div style="color:var(--text-secondary);padding:20px;text-align:center">No files in trash</div>`;
      return;
    }

    let html = "";
    _files.forEach(f => {
      const date = f.deleted_at ? new Date(f.deleted_at).toLocaleString() : "";
      const size = f.size ? ` (${(f.size / 1024).toFixed(1)} KB)` : "";
      html += `
        <div style="border:1px solid var(--border);border-radius:6px;padding:10px 14px;margin-bottom:6px;background:var(--surface);display:flex;justify-content:space-between;align-items:center">
          <div style="flex:1;min-width:0">
            <div style="font-family:monospace;font-size:13px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${escapeHtml(f.original || f.path || "")}${size}</div>
            <div style="color:var(--text-secondary);font-size:11px">${date}</div>
          </div>
          <button class="btn-trash-restore" data-id="${escapeHtml(f.id || '')}" style="margin-left:12px;padding:4px 10px;border:1px solid var(--accent);border-radius:4px;font-size:12px;cursor:pointer;background:transparent;color:var(--accent);white-space:nowrap">Restore</button>
        </div>`;
    });
    container.innerHTML = html;

    // Wire restore buttons.
    container.querySelectorAll(".btn-trash-restore").forEach(btn => {
      btn.addEventListener("click", async () => {
        const id = btn.dataset.id;
        if (!id) return;
        try {
          const res = await api.post(`/api/trash/${encodeURIComponent(id)}/restore`);
          if (res.ok) {
            await loadFiles();
          } else {
            const err = await res.json();
            alert("Restore failed: " + (err.error || "unknown error"));
          }
        } catch (e) {
          console.error(e);
        }
      });
    });
  }

  return { init, loadFiles, render };
})();

// Router hook.
(function() {
  const origNav = Router.navigate;
  Router.navigate = function(view, params) {
    origNav.call(Router, view, params);
    if (view === "trash") {
      Trash.loadFiles();
    }
  };
})();
