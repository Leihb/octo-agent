// ── MCP — MCP server management panel ──────────────────────────────────────
//
// Responsibilities:
//   - Render the MCP panel: tool_search switch + server list
//   - Add / edit / delete / enable-disable servers via /api/mcp/*
//   - Bulk import from a pasted Claude Code-style mcp.json
//   - Per-server reconnect + full reload
//
// Project-level entries (source=project) are read-only here — they belong to
// the repo's .octo/mcp.json. Every mutation response carries the refreshed
// server list, so the panel re-renders from the response instead of refetching.
//
// Panel switching is delegated to Router — MCP only manages data + rendering.
//
// Depends on: Router (app.js), Modal (app.js), I18n (i18n.js),
//             global $ / escapeHtml helpers
// ─────────────────────────────────────────────────────────────────────────

const MCP = (() => {
  // ── Private state ──────────────────────────────────────────────────────
  let _servers  = [];     // [{ name, transport, source, disabled, status, ... }]
  let _domWired = false;  // one-time DOM listeners bound?
  let _editing  = null;   // server name being edited, or null when adding

  // ── Rendering ──────────────────────────────────────────────────────────

  const STATUS_CLASSES = {
    connected:    "mcp-status-connected",
    error:        "mcp-status-error",
    disabled:     "mcp-status-disabled",
    invalid:      "mcp-status-invalid",
    disconnected: "mcp-status-disconnected",
  };

  /** One-line summary of how the server is launched / reached. */
  function _endpointSummary(s) {
    if (s.transport === "stdio") {
      return [s.command, ...(s.args || [])].join(" ");
    }
    return s.url || "";
  }

  function _renderServerCard(s) {
    const card = document.createElement("div");
    card.className = "mcp-card" + (s.invalid ? " mcp-card-invalid" : "");

    const isProject  = s.source === "project";
    const statusKey  = `mcp.status.${s.status}`;
    const statusHtml = `<span class="mcp-status-badge ${STATUS_CLASSES[s.status] || ""}">${I18n.t(statusKey)}</span>`;

    const toolsHtml = s.status === "connected"
      ? `<span class="mcp-tools-count">${I18n.t("mcp.tools.count", { n: s.tools })}</span>`
      : "";

    const sourceBadge = isProject
      ? `<span class="mcp-badge mcp-badge-project" data-tooltip="${escapeHtml(I18n.t("mcp.projectReadonly"))}">${I18n.t("mcp.badge.project")}</span>`
      : "";

    const errHtml = s.error
      ? `<div class="mcp-card-error">${escapeHtml(s.error)}</div>`
      : s.invalid
        ? `<div class="mcp-card-error">${escapeHtml(s.invalid)}</div>`
        : "";

    // Toggle: user entries only; project entries show a disabled toggle.
    const toggleDisabled = isProject || !!s.invalid;
    const toggleTip = isProject ? I18n.t("mcp.projectReadonly")
                    : s.invalid  ? I18n.t("mcp.invalid.toggleTip")
                    : s.disabled ? I18n.t("mcp.toggle.enable")
                    : I18n.t("mcp.toggle.disable");

    // OAuth servers that aren't connected get an explicit Authorize button —
    // the serve process can't run a device flow on its own.
    const authorizeHtml = (s.auth === "oauth" && !s.disabled && !s.invalid && s.status !== "connected")
      ? `<button class="btn-mcp-action btn-mcp-authorize" data-name="${escapeHtml(s.name)}" title="${escapeHtml(I18n.t("mcp.btn.authorize"))}">
          <svg xmlns="http://www.w3.org/2000/svg" width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 2l-2 2m-7.61 7.61a5.5 5.5 0 1 1-7.778 7.778 5.5 5.5 0 0 1 7.777-7.777zm0 0L15.5 7.5m0 0 3 3L22 7l-3-3m-3.5 3.5L19 4"/></svg>
        </button>`
      : "";

    const reconnectHtml = (!s.disabled && !s.invalid)
      ? `<button class="btn-mcp-action btn-mcp-reconnect" data-name="${escapeHtml(s.name)}" title="${escapeHtml(I18n.t("mcp.btn.reconnect"))}">
          <svg xmlns="http://www.w3.org/2000/svg" width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 2v6h-6"/><path d="M3 12a9 9 0 0 1 15-6.7L21 8"/><path d="M3 22v-6h6"/><path d="M21 12a9 9 0 0 1-15 6.7L3 16"/></svg>
        </button>`
      : "";

    const editDeleteHtml = isProject ? "" : `
      <button class="btn-mcp-action btn-mcp-edit" data-name="${escapeHtml(s.name)}" title="${escapeHtml(I18n.t("mcp.btn.edit"))}">
        <svg xmlns="http://www.w3.org/2000/svg" width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M17 3a2.85 2.83 0 1 1 4 4L7.5 20.5 2 22l1.5-5.5Z"/></svg>
      </button>
      <button class="btn-mcp-action btn-mcp-delete" data-name="${escapeHtml(s.name)}" title="${escapeHtml(I18n.t("mcp.btn.delete"))}">
        <svg xmlns="http://www.w3.org/2000/svg" width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 6h18"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6"/><path d="M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/></svg>
      </button>`;

    card.innerHTML = `
      <div class="mcp-card-main">
        <div class="mcp-card-info">
          <div class="mcp-card-title">
            <span class="mcp-name">${escapeHtml(s.name)}</span>
            <span class="mcp-badge mcp-badge-transport">${escapeHtml(s.transport || "?")}</span>
            ${sourceBadge}
            ${statusHtml}
            ${toolsHtml}
          </div>
          <div class="mcp-card-desc">${escapeHtml(_endpointSummary(s))}</div>
        </div>
        <div class="mcp-card-actions">
          ${authorizeHtml}
          ${reconnectHtml}
          ${editDeleteHtml}
          <label class="skill-toggle ${toggleDisabled ? "skill-toggle-disabled" : ""}" data-tooltip="${escapeHtml(toggleTip)}">
            <input type="checkbox" class="skill-toggle-input" ${s.disabled ? "" : "checked"} ${toggleDisabled ? "disabled" : ""}>
            <span class="skill-toggle-track"></span>
          </label>
        </div>
      </div>
      ${errHtml}`;

    if (!toggleDisabled) {
      card.querySelector(".skill-toggle-input").addEventListener("change", () => _toggle(s.name));
    }
    const authorizeBtn = card.querySelector(".btn-mcp-authorize");
    if (authorizeBtn) authorizeBtn.addEventListener("click", () => _authorize(s.name));
    const reconnectBtn = card.querySelector(".btn-mcp-reconnect");
    if (reconnectBtn) reconnectBtn.addEventListener("click", () => _reconnect(s.name));
    const editBtn = card.querySelector(".btn-mcp-edit");
    if (editBtn) editBtn.addEventListener("click", () => _openForm(s));
    const deleteBtn = card.querySelector(".btn-mcp-delete");
    if (deleteBtn) deleteBtn.addEventListener("click", () => _remove(s.name));

    return card;
  }

  function _renderList() {
    const container = $("mcp-list");
    if (!container) return;
    container.innerHTML = "";
    if (_servers.length === 0) {
      container.innerHTML = `<div class="mcp-empty">${I18n.t("mcp.empty")}</div>`;
      return;
    }
    _servers.forEach(s => container.appendChild(_renderServerCard(s)));
  }

  function _renderToolSearch(settings) {
    document.querySelectorAll(".mcp-ts-option").forEach(btn => {
      btn.classList.toggle("active", btn.dataset.value === settings.enabled);
    });
  }

  // ── API calls ──────────────────────────────────────────────────────────

  /** Run a mutate request; on success re-render from the returned list. */
  async function _mutate(method, path, body) {
    try {
      const res  = await fetch(path, {
        method,
        headers: body ? { "Content-Type": "application/json" } : undefined,
        body: body ? JSON.stringify(body) : undefined,
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        alert(I18n.t("mcp.error.request") + (data.error || res.status));
        return false;
      }
      _servers = data.servers || [];
      _renderList();
      return true;
    } catch (e) {
      alert(I18n.t("mcp.error.request") + e.message);
      return false;
    }
  }

  async function _load() {
    const container = $("mcp-list");
    if (container) container.innerHTML = `<div class="mcp-empty">${I18n.t("mcp.loading")}</div>`;
    try {
      const [serversRes, tsRes] = await Promise.all([
        fetch("/api/mcp/servers"),
        fetch("/api/config/toolsearch"),
      ]);
      const serversData = await serversRes.json();
      _servers = serversData.servers || [];
      _renderList();
      if (tsRes.ok) _renderToolSearch(await tsRes.json());
    } catch (e) {
      if (container) container.innerHTML = `<div class="mcp-empty">${escapeHtml(I18n.t("mcp.error.request") + e.message)}</div>`;
    }
  }

  function _toggle(name) {
    return _mutate("PATCH", `/api/mcp/servers/${encodeURIComponent(name)}/toggle`);
  }

  async function _reconnect(name) {
    const btn = document.querySelector(`.btn-mcp-reconnect[data-name="${CSS.escape(name)}"]`);
    if (btn) btn.classList.add("mcp-spinning");
    await _mutate("POST", `/api/mcp/servers/${encodeURIComponent(name)}/reconnect`);
  }

  async function _remove(name) {
    const confirmed = await Modal.confirm(I18n.t("mcp.confirmDelete", { name }));
    if (!confirmed) return;
    await _mutate("DELETE", `/api/mcp/servers/${encodeURIComponent(name)}`);
  }

  async function _reload() {
    const btn = $("btn-mcp-reload");
    if (btn) btn.disabled = true;
    await _mutate("POST", "/api/mcp/reload");
    if (btn) btn.disabled = false;
  }

  async function _setToolSearch(value) {
    try {
      const res = await fetch("/api/config/toolsearch", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ enabled: value }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) {
        alert(I18n.t("mcp.error.request") + (data.error || res.status));
        return;
      }
      _renderToolSearch(data);
    } catch (e) {
      alert(I18n.t("mcp.error.request") + e.message);
    }
  }

  // ── Add / edit form ────────────────────────────────────────────────────

  function _syncTransportFields() {
    const transport = $("mcp-form-transport").value;
    $("mcp-form-stdio-fields").style.display = transport === "stdio" ? "" : "none";
    $("mcp-form-http-fields").style.display  = transport === "http" ? "" : "none";
  }

  /** Open the form. Pass a server object to edit, nothing to add. */
  function _openForm(server) {
    _editing = server ? server.name : null;
    $("mcp-import-bar").style.display = "none";
    $("mcp-form").style.display = "";
    $("mcp-form-title").textContent = I18n.t(server ? "mcp.form.editTitle" : "mcp.form.addTitle");

    const nameInput = $("mcp-form-name");
    nameInput.value    = server ? server.name : "";
    nameInput.disabled = !!server; // the name keys the config entry — no rename

    $("mcp-form-transport").value = server ? (server.transport || "stdio") : "stdio";
    $("mcp-form-command").value   = server?.command || "";
    $("mcp-form-args").value      = (server?.args || []).join("\n");
    $("mcp-form-env").value       = Object.entries(server?.env || {}).map(([k, v]) => `${k}=${v}`).join("\n");
    $("mcp-form-url").value       = server?.url || "";
    $("mcp-form-headers").value   = Object.entries(server?.headers || {}).map(([k, v]) => `${k}: ${v}`).join("\n");
    $("mcp-form-oauth").checked   = server?.auth === "oauth";

    _syncTransportFields();
    nameInput.disabled ? $("mcp-form-command").focus() : nameInput.focus();
  }

  function _closeForm() {
    _editing = null;
    $("mcp-form").style.display = "none";
  }

  /** Parse "KEY=VALUE" lines into an object; null on a malformed line. */
  function _parseKVLines(text, sep) {
    const out = {};
    for (const raw of text.split("\n")) {
      const line = raw.trim();
      if (!line) continue;
      const idx = line.indexOf(sep);
      if (idx <= 0) return null;
      out[line.slice(0, idx).trim()] = line.slice(idx + sep.length).trim();
    }
    return Object.keys(out).length ? out : undefined;
  }

  function _entryFromForm() {
    const transport = $("mcp-form-transport").value;
    if (transport === "stdio") {
      const command = $("mcp-form-command").value.trim();
      if (!command) { alert(I18n.t("mcp.form.commandRequired")); return null; }
      const args = $("mcp-form-args").value.split("\n").map(s => s.trim()).filter(Boolean);
      const env  = _parseKVLines($("mcp-form-env").value, "=");
      if (env === null) { alert(I18n.t("mcp.form.envInvalid")); return null; }
      return { command, ...(args.length ? { args } : {}), ...(env ? { env } : {}) };
    }
    const url = $("mcp-form-url").value.trim();
    if (!url) { alert(I18n.t("mcp.form.urlRequired")); return null; }
    const headers = _parseKVLines($("mcp-form-headers").value, ":");
    if (headers === null) { alert(I18n.t("mcp.form.headersInvalid")); return null; }
    const entry = { url, ...(headers ? { headers } : {}) };
    if ($("mcp-form-oauth").checked) entry.auth = "oauth";
    return entry;
  }

  async function _submitForm() {
    const entry = _entryFromForm();
    if (!entry) return;
    let ok;
    if (_editing) {
      ok = await _mutate("PATCH", `/api/mcp/servers/${encodeURIComponent(_editing)}`, { server: entry });
    } else {
      const name = $("mcp-form-name").value.trim();
      if (!name) { alert(I18n.t("mcp.form.nameRequired")); return; }
      ok = await _mutate("POST", "/api/mcp/servers", { name, server: entry });
    }
    if (ok) _closeForm();
  }

  // ── OAuth device flow ──────────────────────────────────────────────────

  let _oauthTimer = null;

  function _closeOAuthModal() {
    if (_oauthTimer) { clearInterval(_oauthTimer); _oauthTimer = null; }
    $("mcp-oauth-modal").style.display = "none";
  }

  function _renderOAuthState(data) {
    const body = $("mcp-oauth-body");
    const state = data.state || "starting";
    if (state === "starting") {
      body.innerHTML = `<div class="mcp-oauth-waiting">${I18n.t("mcp.oauth.starting")}</div>`;
      return false;
    }
    if (state === "authorizing") {
      const link = data.verification_uri_complete || data.verification_uri || "";
      body.innerHTML = `
        <div class="mcp-oauth-instruction">${I18n.t("mcp.oauth.instruction")}</div>
        <div class="mcp-oauth-code">${escapeHtml(data.user_code || "")}</div>
        ${link ? `<a class="mcp-oauth-link" href="${escapeHtml(link)}" target="_blank" rel="noopener">${I18n.t("mcp.oauth.openLink")}</a>` : ""}
        <div class="mcp-oauth-waiting">${I18n.t("mcp.oauth.waiting")}</div>`;
      return false;
    }
    if (state === "connected") {
      body.innerHTML = `<div class="mcp-oauth-success">${I18n.t("mcp.oauth.success")}</div>`;
      return true;
    }
    body.innerHTML = `
      <div class="mcp-oauth-failed">${I18n.t("mcp.oauth.failed")}</div>
      <div class="mcp-card-error">${escapeHtml(data.error || "")}</div>`;
    return true;
  }

  async function _authorize(name) {
    let res, data;
    try {
      res  = await fetch(`/api/mcp/servers/${encodeURIComponent(name)}/oauth/start`, { method: "POST" });
      data = await res.json().catch(() => ({}));
    } catch (e) {
      alert(I18n.t("mcp.error.request") + e.message);
      return;
    }
    if (!res.ok) {
      alert(I18n.t("mcp.error.request") + (data.error || res.status));
      return;
    }

    $("mcp-oauth-title").textContent = I18n.t("mcp.oauth.title", { name });
    $("mcp-oauth-modal").style.display = "flex";
    _renderOAuthState(data);

    _oauthTimer = setInterval(async () => {
      try {
        const r = await fetch(`/api/mcp/servers/${encodeURIComponent(name)}/oauth/status`);
        if (!r.ok) return; // transient — keep polling until the modal closes
        const d = await r.json();
        const settled = _renderOAuthState(d);
        if (settled) {
          clearInterval(_oauthTimer);
          _oauthTimer = null;
          await _load(); // refresh statuses (connected / error)
          if (d.state === "connected") setTimeout(_closeOAuthModal, 1200);
        }
      } catch { /* transient — keep polling */ }
    }, 1500);
  }

  // ── Import (paste mcp.json) ────────────────────────────────────────────

  async function _submitImport() {
    const text = $("mcp-import-input").value.trim();
    if (!text) return;
    let parsed;
    try {
      parsed = JSON.parse(text);
    } catch {
      alert(I18n.t("mcp.import.invalidJson"));
      return;
    }
    // Accept a full {"mcpServers": {...}} file or a bare name→entry map.
    const servers = parsed.mcpServers || parsed;
    if (typeof servers !== "object" || Array.isArray(servers) || Object.keys(servers).length === 0) {
      alert(I18n.t("mcp.import.invalidJson"));
      return;
    }
    if (await _mutate("POST", "/api/mcp/servers", { mcpServers: servers })) {
      $("mcp-import-input").value = "";
      $("mcp-import-bar").style.display = "none";
    }
  }

  // ── One-time DOM wiring ────────────────────────────────────────────────

  function _wireDom() {
    if (_domWired) return;
    _domWired = true;

    $("btn-mcp-add").addEventListener("click", () => _openForm(null));
    $("btn-mcp-import").addEventListener("click", () => {
      _closeForm();
      const bar = $("mcp-import-bar");
      bar.style.display = bar.style.display === "none" ? "" : "none";
      if (bar.style.display !== "none") $("mcp-import-input").focus();
    });
    $("btn-mcp-reload").addEventListener("click", _reload);

    $("mcp-form-transport").addEventListener("change", _syncTransportFields);
    $("btn-mcp-form-save").addEventListener("click", _submitForm);
    $("btn-mcp-form-cancel").addEventListener("click", _closeForm);

    $("btn-mcp-import-confirm").addEventListener("click", _submitImport);
    $("btn-mcp-import-cancel").addEventListener("click", () => {
      $("mcp-import-bar").style.display = "none";
    });

    $("btn-mcp-oauth-close").addEventListener("click", _closeOAuthModal);

    document.querySelectorAll(".mcp-ts-option").forEach(btn => {
      btn.addEventListener("click", () => _setToolSearch(btn.dataset.value));
    });
  }

  // ── Public API ─────────────────────────────────────────────────────────

  return {
    async onPanelShow() {
      _wireDom();
      _closeForm();
      await _load();
    },
  };
})();
