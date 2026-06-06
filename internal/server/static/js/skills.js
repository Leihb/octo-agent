// ── Skills — skill management panel ──────────────────────────────────────
//
// Lists user and system skills. Supports import (ZIP/GitHub URL). The
// "Create" button launches a session for AI-driven skill creation.
// ─────────────────────────────────────────────────────────────────────────

const Skills = (() => {
  let _skills = [];
  let _showSystem = false;

  function init() {
    loadSkills();

    // Wire import bar toggle.
    const importBtn = $("btn-import-skill");
    const importBar = $("skill-import-bar");
    const importCancel = $("btn-skill-import-cancel");
    const importConfirm = $("btn-skill-import-confirm");

    if (importBtn && importBar) {
      importBtn.addEventListener("click", () => {
        importBar.style.display = importBar.style.display === "none" ? "" : "none";
        const input = $("skill-import-input");
        if (input) input.focus();
      });
    }
    if (importCancel && importBar) {
      importCancel.addEventListener("click", () => {
        importBar.style.display = "none";
      });
    }
    if (importConfirm) {
      importConfirm.addEventListener("click", doImport);
    }

    // Create skill button — opens a chat session.
    const createBtn = $("btn-create-skill");
    if (createBtn) {
      createBtn.addEventListener("click", () => {
        if (Sessions.activeId) {
          Sessions.sendMessage("/skill-creator Create a new skill for me");
        }
      });
    }
  }

  async function loadSkills() {
    try {
      const res = await api.get("/api/skills");
      if (!res.ok) return;
      _skills = await res.json();
      render();
    } catch (e) {
      console.error("Failed to load skills:", e);
    }
  }

  function render() {
    const container = $("skills-list");
    if (!container) return;

    const filtered = _skills.filter(s => {
      if (_showSystem) return true;
      return s.source !== "system";
    });

    if (filtered.length === 0) {
      container.innerHTML = `<div style="color:var(--text-secondary);padding:20px;text-align:center">No skills found. Import or create one!</div>`;
      return;
    }

    let html = "";
    filtered.forEach(s => {
      const isSystem = s.source === "system";
      html += `
        <div class="skill-card" style="border:1px solid var(--border);border-radius:8px;padding:14px 16px;margin-bottom:8px;background:var(--surface)">
          <div style="display:flex;justify-content:space-between;align-items:flex-start">
            <div>
              <div style="font-weight:600;font-size:14px">${escapeHtml(s.name)}${isSystem ? ' <span style="font-size:11px;color:var(--text-secondary)">(system)</span>' : ''}</div>
              <div style="color:var(--text-secondary);font-size:12px;margin-top:2px">${escapeHtml(s.description || "No description")}</div>
            </div>
          </div>
        </div>`;
    });
    container.innerHTML = html;
  }

  async function doImport() {
    const input = $("skill-import-input");
    if (!input) return;
    const val = input.value.trim();
    if (!val) return;

    // TODO: POST /api/skills/import with URL
    alert(`Skill import from "${val}" — API endpoint not yet implemented`);
    input.value = "";
    const importBar = $("skill-import-bar");
    if (importBar) importBar.style.display = "none";
  }

  return { init, loadSkills, render };
})();

// Wire sidebar nav item.
(function() {
  document.addEventListener("DOMContentLoaded", () => {
    // The sidebar click handler is wired in sessions.js wireInputEvents.
    // When the skills panel is navigated to, load skills if needed.
    const origNav = Router.navigate;
    Router.navigate = function(view, params) {
      origNav.call(Router, view, params);
      if (view === "skills") {
        Skills.render();
        Skills.loadSkills();
      }
    };
  });
})();
