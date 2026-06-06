// ── Tasks — scheduled tasks management ──────────────────────────────────
//
// CRUD for scheduled cron tasks. Loaded from REST API.
// ─────────────────────────────────────────────────────────────────────────

const Tasks = (() => {
  let _tasks = [];

  async function init() {
    await load();
    render();

    // Wire create button.
    const createBtn = $("btn-create-task");
    if (createBtn) {
      createBtn.addEventListener("click", showCreateModal);
    }
  }

  async function load() {
    try {
      const res = await api.get("/api/tasks");
      if (!res.ok) return;
      _tasks = await res.json();
    } catch (e) {
      console.error("Failed to load tasks:", e);
    }
  }

  function render() {
    const container = $("task-list-table");
    if (!container) return;

    if (_tasks.length === 0) {
      container.innerHTML = `<div style="color:var(--text-secondary);padding:20px;text-align:center">No scheduled tasks yet</div>`;
      return;
    }

    let html = `<div style="padding:16px 32px">`;
    _tasks.forEach(t => {
      const enabled = t.enabled !== false ? "✓" : "✗";
      const lastRun = t.last_run ? new Date(t.last_run).toLocaleString() : "—";
      html += `
        <div style="border:1px solid var(--border);border-radius:8px;padding:14px 16px;margin-bottom:8px;background:var(--surface)">
          <div style="display:flex;justify-content:space-between;align-items:flex-start">
            <div>
              <div style="font-weight:600;font-size:14px">${escapeHtml(t.name || t.id)}</div>
              <div style="color:var(--text-secondary);font-size:12px;margin-top:2px">
                Cron: <code>${escapeHtml(t.cron || "")}</code> &nbsp;|&nbsp;
                Last run: ${lastRun} &nbsp;|&nbsp;
                Status: ${enabled}
              </div>
              ${t.prompt ? `<div style="color:var(--text-secondary);font-size:12px;margin-top:4px">Prompt: ${escapeHtml(t.prompt.slice(0, 100))}${t.prompt.length > 100 ? '…' : ''}</div>` : ""}
            </div>
            <div style="display:flex;gap:6px">
              <button class="btn-task-run" data-id="${escapeHtml(t.id)}" style="padding:4px 10px;border:1px solid var(--border);border-radius:4px;font-size:12px;cursor:pointer;background:var(--surface);color:var(--text)">Run</button>
              <button class="btn-task-delete" data-id="${escapeHtml(t.id)}" style="padding:4px 10px;border:1px solid var(--error);border-radius:4px;font-size:12px;cursor:pointer;background:transparent;color:var(--error)">✕</button>
            </div>
          </div>
        </div>`;
    });
    html += `</div>`;
    container.innerHTML = html;

    // Wire run/delete buttons.
    container.querySelectorAll(".btn-task-run").forEach(btn => {
      btn.addEventListener("click", async () => {
        const id = btn.dataset.id;
        try {
          await api.post(`/api/tasks/${id}/run`);
        } catch (e) {
          console.error(e);
        }
      });
    });
    container.querySelectorAll(".btn-task-delete").forEach(btn => {
      btn.addEventListener("click", async () => {
        const id = btn.dataset.id;
        if (!confirm("Delete this task?")) return;
        try {
          const res = await api.delete(`/api/tasks/${id}`);
          if (res.ok) { await load(); render(); }
        } catch (e) {
          console.error(e);
        }
      });
    });
  }

  function showCreateModal() {
    const name = prompt("Task name:");
    if (!name) return;
    const cron = prompt("Cron expression (e.g. 0 9 * * * for daily at 9am):");
    if (!cron) return;
    const promptText = prompt("Prompt to send:");
    if (!promptText) return;

    createTask(name, cron, promptText);
  }

  async function createTask(name, cronExpr, prompt) {
    try {
      const res = await api.post("/api/tasks", { name, cron: cronExpr, prompt });
      if (res.ok) { await load(); render(); }
    } catch (e) {
      console.error(e);
    }
  }

  return { init, load, render };
})();

// Wire sidebar nav.
(function() {
  const origNav = Router.navigate;
  Router.navigate = function(view, params) {
    origNav.call(Router, view, params);
    if (view === "tasks") {
      Tasks.load().then(() => Tasks.render());
    }
  };
})();
