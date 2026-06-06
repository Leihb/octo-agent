// ── Channels — IM channel management panel ──────────────────────────────
//
// Lists configured IM platforms (DingTalk, Feishu, WeChat). Supports
// adding, editing, deleting, and testing channel configurations.
// ─────────────────────────────────────────────────────────────────────────

const Channels = (() => {
  let _channels = [];      // configured channels from /api/channels
  let _available = [];      // registered platforms from /api/channels/available
  let _editing = null;      // platform being edited, or null

  function init() {
    loadAvailable();
    // Wire the add platform dropdown in the header.
    const addArea = document.getElementById("channels-add-area");
    if (addArea) {
      // This is rendered by renderHeader.
    }
  }

  async function loadAvailable() {
    try {
      const res = await api.get("/api/channels/available");
      if (res.ok) _available = await res.json();
    } catch (e) { /* ignore */ }
  }

  async function loadChannels() {
    try {
      const res = await api.get("/api/channels");
      if (res.ok) _channels = await res.json();
    } catch (e) { /* ignore */ }
    render();
  }

  function getChannel(platform) {
    return _channels.find(c => c.platform === platform);
  }

  function render() {
    renderHeader();
    renderList();
  }

  function renderHeader() {
    const container = document.getElementById("channels-body");
    if (!container) return;

    // Wire add button dropdown (idempotent — safe to call multiple times).
    const addBtn = document.getElementById("btn-add-channel");
    const dropdown = document.getElementById("add-channel-dropdown");
    if (addBtn && dropdown && !addBtn._wired) {
      addBtn._wired = true;
      addBtn.addEventListener("click", (e) => {
        e.stopPropagation();
        buildAddDropdown(dropdown);
        dropdown.hidden = !dropdown.hidden;
      });
      document.addEventListener("click", () => { dropdown.hidden = true; });
    }

    // Update title if needed.
    const title = container.querySelector(".channels-page-title");
    if (title) title.textContent = "Channels";
  }

  function buildAddDropdown(dropdown) {
    // Show platforms not yet configured.
    const configured = new Set(_channels.map(c => c.platform));
    const unconfigured = _available.filter(a => !configured.has(a.platform));

    if (unconfigured.length === 0) {
      dropdown.innerHTML = `<div class="dropdown-item" style="color:var(--text-secondary)">All platforms configured</div>`;
    } else {
      dropdown.innerHTML = unconfigured.map(a =>
        `<div class="dropdown-item" data-platform="${escapeHtml(a.platform)}">${escapeHtml(a.label)}</div>`
      ).join("");

      dropdown.querySelectorAll(".dropdown-item[data-platform]").forEach(item => {
        item.addEventListener("click", (e) => {
          e.stopPropagation();
          dropdown.hidden = true;
          showEditor(item.dataset.platform);
        });
      });
    }
  }

  function renderList() {
    const container = document.getElementById("channels-list");
    if (!container) return;

    if (_channels.length === 0) {
      container.innerHTML = `
        <div style="padding:40px 32px;color:var(--text-secondary);text-align:center">
          <p style="font-size:44px;margin-bottom:16px">🔌</p>
          <p style="font-size:16px;margin-bottom:8px">No channels configured</p>
          <p>Click "Add Platform" to connect DingTalk, Feishu, or WeChat.</p>
        </div>`;
      return;
    }

    let html = '<div style="padding:0 32px">';
    _channels.forEach(ch => {
      const avail = _available.find(a => a.platform === ch.platform);
      const label = avail ? avail.label : ch.platform;
      const statusBadge = ch.enabled
        ? '<span style="color:var(--success);font-size:12px;font-weight:600">● Enabled</span>'
        : '<span style="color:var(--text-secondary);font-size:12px">○ Disabled</span>';

      // Build fields summary.
      const fieldLines = Object.entries(ch.fields || {})
        .filter(([k]) => k !== "enabled")
        .map(([k, v]) => `<span style="font-family:monospace;font-size:11px;color:var(--text-secondary)">${escapeHtml(k)}: ${escapeHtml(v || "")}</span>`)
        .join(" &nbsp;|&nbsp; ");

      html += `
        <div style="border:1px solid var(--border);border-radius:8px;padding:16px;margin-bottom:8px;background:var(--surface)">
          <div style="display:flex;justify-content:space-between;align-items:flex-start">
            <div style="flex:1;min-width:0">
              <div style="display:flex;align-items:center;gap:8px;margin-bottom:4px">
                <span style="font-weight:600;font-size:14px">${escapeHtml(label)}</span>
                ${statusBadge}
              </div>
              <div style="margin-bottom:4px">${fieldLines || '<span style="color:var(--text-secondary);font-size:12px">No credentials configured</span>'}</div>
            </div>
            <div style="display:flex;gap:6px;margin-left:16px;flex-shrink:0">
              <button class="btn-channel-edit" data-platform="${escapeHtml(ch.platform)}"
                style="padding:4px 12px;border:1px solid var(--border);border-radius:4px;font-size:12px;cursor:pointer;background:var(--surface);color:var(--text)">Edit</button>
              <button class="btn-channel-delete" data-platform="${escapeHtml(ch.platform)}"
                style="padding:4px 10px;border:1px solid var(--error);border-radius:4px;font-size:12px;cursor:pointer;background:transparent;color:var(--error)">✕</button>
            </div>
          </div>
        </div>`;
    });
    html += '</div>';
    container.innerHTML = html;

    // Wire edit/delete.
    container.querySelectorAll(".btn-channel-edit").forEach(btn => {
      btn.addEventListener("click", () => showEditor(btn.dataset.platform));
    });
    container.querySelectorAll(".btn-channel-delete").forEach(btn => {
      btn.addEventListener("click", () => deleteChannel(btn.dataset.platform));
    });
  }

  // ── Editor modal ───────────────────────────────────────────────────────

  function showEditor(platform) {
    const ch = getChannel(platform);
    const avail = _available.find(a => a.platform === platform);
    const label = avail ? avail.label : platform;
    const fieldNames = avail ? avail.fields : [];
    const existingFields = ch ? ch.fields : {};
    const enabled = ch ? ch.enabled : true;

    _editing = platform;

    // Build modal content.
    let fieldsHtml = '';
    fieldNames.forEach(f => {
      const val = existingFields[f] || "";
      const isSecret = f.includes("secret") || f.includes("token");
      fieldsHtml += `
        <div class="modal-field">
          <label class="modal-label">${escapeHtml(f)}</label>
          <input type="${isSecret ? 'password' : 'text'}" class="modal-input channel-field"
            data-field="${escapeHtml(f)}" value="${escapeHtml(val)}" autocomplete="off">
        </div>`;
    });

    // "allowed_users" field (comma-separated) — add if not in fieldNames.
    if (!fieldNames.includes("allowed_users") && (platform === "dingtalk" || platform === "feishu")) {
      const val = existingFields["allowed_users"] || "";
      fieldsHtml += `
        <div class="modal-field">
          <label class="modal-label">allowed_users (optional, comma-separated)</label>
          <input type="text" class="modal-input channel-field"
            data-field="allowed_users" value="${escapeHtml(val)}" autocomplete="off">
        </div>`;
    }

    const modal = document.createElement("div");
    modal.className = "modal-overlay";
    modal.id = "channel-editor-overlay";
    modal.innerHTML = `
      <div class="modal-box" style="width:520px">
        <div class="modal-header">
          <h3 class="modal-title">${escapeHtml(label)}</h3>
        </div>
        <div class="modal-body">
          ${fieldsHtml}
          <div class="modal-field">
            <label class="modal-checkbox-label" style="display:flex;align-items:center;gap:8px;cursor:pointer">
              <input type="checkbox" id="channel-enabled" ${enabled ? 'checked' : ''}>
              <span>Enabled</span>
            </label>
          </div>
          <div id="channel-test-result" style="min-height:0;margin-top:8px;font-size:13px"></div>
        </div>
        <div class="modal-footer">
          <button id="btn-channel-test" class="btn-secondary">Test Connection</button>
          <button id="btn-channel-cancel" class="btn-secondary">Cancel</button>
          <button id="btn-channel-save" class="btn-primary">Save</button>
        </div>
      </div>`;
    document.body.appendChild(modal);

    // Wire buttons.
    document.getElementById("btn-channel-cancel").addEventListener("click", closeEditor);
    document.getElementById("btn-channel-save").addEventListener("click", saveChannel);
    document.getElementById("btn-channel-test").addEventListener("click", testChannel);
    modal.addEventListener("click", (e) => { if (e.target === modal) closeEditor(); });
  }

  function closeEditor() {
    const modal = document.getElementById("channel-editor-overlay");
    if (modal) modal.remove();
    _editing = null;
  }

  async function saveChannel() {
    const platform = _editing;
    if (!platform) return;

    const enabled = document.getElementById("channel-enabled")?.checked ?? true;
    const fields = {};
    document.querySelectorAll(".channel-field").forEach(input => {
      fields[input.dataset.field] = input.value.trim();
    });

    try {
      const res = await api.post(`/api/channels/${platform}`, { enabled, fields });
      if (res.ok) {
        closeEditor();
        await loadChannels();
      } else {
        const err = await res.json();
        alert("Save failed: " + (err.error || "unknown error"));
      }
    } catch (e) {
      alert("Save failed: " + e.message);
    }
  }

  async function testChannel() {
    const platform = _editing;
    if (!platform) return;

    const fields = {};
    document.querySelectorAll(".channel-field").forEach(input => {
      fields[input.dataset.field] = input.value.trim();
    });

    const resultEl = document.getElementById("channel-test-result");
    if (resultEl) {
      resultEl.textContent = "Testing…";
      resultEl.style.color = "var(--text-secondary)";
    }

    try {
      const res = await api.post(`/api/channels/${platform}/test`, { fields });
      const data = await res.json();
      if (resultEl) {
        resultEl.textContent = data.ok ? "✅ " + (data.message || "OK") : "❌ " + (data.error || "Failed");
        resultEl.style.color = data.ok ? "var(--success)" : "var(--error)";
      }
    } catch (e) {
      if (resultEl) {
        resultEl.textContent = "❌ " + e.message;
        resultEl.style.color = "var(--error)";
      }
    }
  }

  async function deleteChannel(platform) {
    if (!confirm(`Remove ${platform} configuration? This cannot be undone.`)) return;

    try {
      const res = await api.delete(`/api/channels/${platform}`);
      if (res.ok) {
        await loadChannels();
      }
    } catch (e) {
      console.error(e);
    }
  }

  return { init, loadChannels, render };
})();

// Router hook.
(function() {
  const origNav = Router.navigate;
  Router.navigate = function(view, params) {
    origNav.call(Router, view, params);
    if (view === "channels") {
      Channels.loadChannels();
    }
  };
})();
