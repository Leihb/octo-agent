// ── Skills — skills state, rendering, enable/disable ──────────────────────
//
// Responsibilities:
//   - Single source of truth for skills data
//   - Render the "Skills" entry in the sidebar
//   - Show/render the skills panel with My Skills tab
//   - Toggle enable/disable via PATCH /api/skills/:name/toggle
//   - Create new skill by opening a session with /skill-creator
//
// Panel switching is delegated to Router — Skills only manages data + rendering.
//
// Depends on: WS (ws.js), Sessions (sessions.js), Router (app.js),
//             global $ / escapeHtml helpers
// ─────────────────────────────────────────────────────────────────────────

const Skills = (() => {
  // ── Private state ──────────────────────────────────────────────────────
  let _skills      = [];          // [{ name, description, source, enabled }]
  let _domWired       = false;    // whether one-time DOM listeners have been bound
  let _showSystemSkills = false;  // whether system (source=default) skills are shown

  // ── Private helpers ────────────────────────────────────────────────────

  /** Render a single skill card in My Skills tab. */
  function _renderSkillCard(skill) {
    const card = document.createElement("div");
    // invalid = unrecoverable (can't be used at all); warning = auto-corrected but fully usable
    card.className = "skill-card" + (skill.invalid ? " skill-card-invalid" : "");

    // "default" = built-in gem skills
    const isSystem   = skill.source === "default";
    const badgeClass = isSystem ? "skill-badge skill-badge-system" : "skill-badge skill-badge-custom";
    const badgeLabel = isSystem ? I18n.t("skills.badge.system") : I18n.t("skills.badge.custom");

    // Build warning icon for skills with auto-corrected issues (still fully usable)
    // Build error notice for truly invalid skills (can't be used)
    let warnIconHtml = "";
    let errorNoticeHtml = "";
    if (skill.invalid) {
      const reason = skill.invalid_reason || I18n.t("skills.invalid.reason");
      errorNoticeHtml = `<div class="skill-notice skill-notice-error">⚠ ${escapeHtml(reason)}</div>`;
    } else if (skill.warnings && skill.warnings.length > 0) {
      const reason    = skill.warnings.join("\n");
      const tooltip   = I18n.t("skills.warning.tooltip", { reason });
      warnIconHtml = `<span class="skill-warn-icon" data-tooltip="${escapeHtml(tooltip)}">⚠</span>`;
    }

    // toggle is only disabled for system skills or truly invalid ones; warning skills are fine
    const toggleDisabled = isSystem || skill.invalid;
    const toggleTitle    = isSystem     ? I18n.t("skills.systemDisabledTip")
                         : skill.invalid ? I18n.t("skills.invalid.toggleTip")
                         : skill.enabled  ? I18n.t("skills.toggle.disableDesc")
                         : I18n.t("skills.toggle.enableDesc");

    // Choose description based on current language
    const currentLang = I18n.lang();
    const description = (currentLang === "zh" && skill.description_zh)
                        ? skill.description_zh
                        : skill.description || "";

    // Show "Use" button for all skills except invalid ones
    const useButtonHtml = skill.invalid
      ? ""
      : `<button class="btn-skill-use" data-name="${escapeHtml(skill.name)}">${I18n.t("skills.btn.use")}</button>`;

    card.innerHTML = `
      <div class="skill-card-main">
        <div class="skill-card-info">
          <div class="skill-card-title">
            ${warnIconHtml}
            <span class="skill-name">${escapeHtml((currentLang === "zh" && skill.name_zh) ? skill.name_zh : skill.name)}</span>
            <span class="${badgeClass}">${badgeLabel}</span>
            ${skill.invalid ? `<span class="skill-badge skill-badge-invalid">${I18n.t("skills.badge.invalid")}</span>` : ""}
          </div>
          <div class="skill-card-desc">${escapeHtml(description)}</div>
        </div>
        <div class="skill-card-actions">
          <label class="skill-toggle ${toggleDisabled ? "skill-toggle-disabled" : ""}" data-tooltip="${escapeHtml(toggleTitle)}">
            <input type="checkbox" class="skill-toggle-input" ${skill.enabled ? "checked" : ""} ${toggleDisabled ? "disabled" : ""}>
            <span class="skill-toggle-track"></span>
          </label>
          ${useButtonHtml}
        </div>
      </div>
      ${errorNoticeHtml}`;

    // Bind toggle event
    if (!isSystem) {
      const checkbox = card.querySelector(".skill-toggle-input");
      checkbox.addEventListener("change", async () => {
        await Skills.toggle(skill.name, checkbox.checked);
      });
    }

    // Flip tooltip below when toggle is near top of scroll container
    const toggleLabel = card.querySelector(".skill-toggle");
    if (toggleLabel) {
      toggleLabel.addEventListener("mouseenter", () => {
        const scroller = toggleLabel.closest(".skills-tab-content");
        if (!scroller) return;
        const toggleTop = toggleLabel.getBoundingClientRect().top;
        const scrollerTop = scroller.getBoundingClientRect().top;
        if (toggleTop - scrollerTop < 80) {
          toggleLabel.classList.add("skill-toggle-flip");
        }
      });
    }

    // Bind "Use" button event
    const useBtn = card.querySelector(".btn-skill-use");
    if (useBtn) {
      useBtn.addEventListener("click", () => _useInstalledSkill(skill.name));
    }

    return card;
  }

  /** Render My Skills tab content. */
  function _renderMySkills() {
    const container = $("skills-list");
    console.log("[Skills] _renderMySkills, container=", container, "_skills.length=", _skills.length);
    if (!container) { console.error("[Skills] skills-list not found!"); return; }
    container.innerHTML = "";

    // Optionally hide system (source=default) skills
    const visible = _showSystemSkills
      ? _skills
      : _skills.filter(s => s.source !== "default");

    if (visible.length === 0) {
      const emptyText     = I18n.t("skills.empty");
      const createBtnText = I18n.t("skills.empty.createBtn");

      const emptyWrapper = document.createElement("div");
      emptyWrapper.className = "skills-empty";

      const emptyTextEl = document.createElement("div");
      emptyTextEl.className   = "skills-empty-text";
      emptyTextEl.textContent = emptyText;

      const createBtn = document.createElement("div");
      createBtn.className = "skills-empty-create-btn";
      createBtn.innerHTML = `
        <svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <path d="M12 2a10 10 0 1 0 10 10A10 10 0 0 0 12 2z"/><path d="M12 8v8"/><path d="M8 12h8"/>
        </svg>
        <span>${escapeHtml(createBtnText)}</span>
        <svg class="skills-empty-create-arrow" xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <path d="M5 12h14"/><path d="M12 5l7 7-7 7"/>
        </svg>`;
      createBtn.addEventListener("click", () => Skills.createInSession("/skill-creator"));

      emptyWrapper.appendChild(emptyTextEl);
      emptyWrapper.appendChild(createBtn);
      container.appendChild(emptyWrapper);
    } else {
      // System skills first, then custom
      const sorted = [
        ...visible.filter(s => s.source === "default"),
        ...visible.filter(s => s.source !== "default")
      ];
      sorted.forEach((skill, i) => {
        try {
          container.appendChild(_renderSkillCard(skill));
        } catch (e) {
          console.error("[Skills] _renderSkillCard failed for skill", i, skill.name, e);
        }
      });
    }
  }

  /** Open a new session and trigger a skill by sending "/{name}" as the first message. */
  async function _useInstalledSkill(name) {
    const maxN = Sessions.all.reduce((max, s) => {
      const m = s.name.match(/^Session (\d+)$/);
      return m ? Math.max(max, parseInt(m[1], 10)) : max;
    }, 0);
    const res = await fetch("/api/sessions", {
      method:  "POST",
      headers: { "Content-Type": "application/json" },
      body:    JSON.stringify({ name: "Session " + (maxN + 1), source: "manual" })
    });
    const data = await res.json();
    if (!res.ok) { alert(I18n.t("tasks.sessionError") + (data.error || "unknown")); return; }

    const session = data.session;
    if (!session) return;

    if (!WS.ready) {
      WS.connect();
      Skills.load();
    }

    Sessions.add(session);
    Sessions.renderList();
    Sessions.setPendingMessage(session.id, "/" + name);
    Sessions.select(session.id);
  }

  // ── Public API ─────────────────────────────────────────────────────────
  return {

    // ── Data ─────────────────────────────────────────────────────────────

    /** Return current skills list (read-only snapshot). */
    get all() { return _skills.slice(); },

    /** Fetch skills from server; re-render sidebar + panel if open. */
    async load() {
      try {
        const res  = await fetch("/api/skills");
        const data = await res.json();
        _skills = data.skills || [];
        Skills.renderSection();
        if (Router.current === "skills") {
          try {
            _renderMySkills();
          } catch (renderErr) {
            console.error("[Skills] _renderMySkills failed", renderErr);
          }
        }
      } catch (e) {
        console.error("[Skills] load failed", e);
      }
    },

    // ── Router interface ──────────────────────────────────────────────────

    /** Called by Router when the skills panel becomes active. */
    onPanelShow() {
      // ── One-time DOM wiring ──────────────────────────────────────────────
      // Bind tab clicks here (not in the IIFE) because $ and the DOM elements
      // are only guaranteed to exist after app.js has loaded and the panel
      // has been shown at least once. Guard with _domWired so we only do this
      // once no matter how many times the user navigates to the Skills panel.
      if (!_domWired) {
        // Wire the "show system skills" checkbox
        const chkSystem = $("chk-show-system-skills");
        if (chkSystem) {
          chkSystem.checked = _showSystemSkills;
          chkSystem.addEventListener("change", () => {
            _showSystemSkills = chkSystem.checked;
            _renderMySkills();
          });
        }

        // Re-render skill cards when the user switches language
        document.addEventListener("langchange", () => {
          _renderMySkills();
        });

        _domWired = true;
      }

      _renderMySkills();
      Skills.renderSection();
    },

    // ── Sidebar rendering ─────────────────────────────────────────────────

    renderSection() {
      // Sidebar item is static in HTML — just update the label text.
      const labelEl = $("skills-sidebar-label");
      if (!labelEl) return;
      labelEl.textContent = I18n.t("sidebar.skills");
    },

    // ── Actions ───────────────────────────────────────────────────────────

    /** Toggle enable/disable for a skill. */
    async toggle(name, enabled) {
      try {
        const res = await fetch(`/api/skills/${encodeURIComponent(name)}/toggle`, {
          method:  "PATCH",
          headers: { "Content-Type": "application/json" },
          body:    JSON.stringify({ enabled })
        });
        const data = await res.json();
        if (!res.ok) { alert(I18n.t("skills.toggleError") + (data.error || "unknown")); return; }
        await Skills.load();
      } catch (e) {
        console.error("[Skills] toggle failed", e);
      }
    },

    // ── Import bar ────────────────────────────────────────────────────────

    /** Toggle the inline import bar below the My Skills header.
     *  Wires confirm / cancel / Enter key handlers on first call.
     */
    toggleImportBar() {
      const bar    = $("skill-import-bar");
      const input  = $("skill-import-input");
      const confirmBtn = $("btn-skill-import-confirm");
      const cancelBtn  = $("btn-skill-import-cancel");
      if (!bar) return;

      const isOpen = bar.style.display !== "none";

      if (isOpen) {
        // Close the bar
        bar.style.display = "none";
        if (input) input.value = "";
        return;
      }

      // Open the bar
      bar.style.display = "";
      if (input) {
        input.focus();
        input.placeholder = I18n.t("skills.import.placeholder");
      }

      // Wire one-time listeners (guard with dataset flag)
      if (!bar.dataset.wired) {
        bar.dataset.wired = "1";

        // Confirm button
        confirmBtn.addEventListener("click", () => Skills._doImportFromBar());

        // Enter key in input
        input.addEventListener("keydown", (e) => {
          if (e.key === "Enter") { e.preventDefault(); Skills._doImportFromBar(); }
        });

        // Cancel button
        cancelBtn.addEventListener("click", () => {
          bar.style.display = "none";
          input.value = "";
        });

        // Browse button — open system file picker, upload zip, fill path into input
        const browseBtn  = $("btn-skill-import-browse");
        const fileInput  = $("skill-import-file");
        if (browseBtn && fileInput) {
          browseBtn.addEventListener("click", () => fileInput.click());
          fileInput.addEventListener("change", async () => {
            const file = fileInput.files[0];
            if (!file) return;

            // Show filename immediately so the user sees feedback
            input.value = file.name;
            input.placeholder = "";
            browseBtn.disabled = true;
            browseBtn.style.opacity = "0.5";

            try {
              const form = new FormData();
              form.append("file", file);
              const res  = await fetch("/api/upload", { method: "POST", body: form });
              const data = await res.json();
              if (res.ok && data.path) {
                // Fill the server-side temp path — /skill-add will read it directly
                input.value = data.path;
              } else {
                input.value = "";
                alert(data.error || "Upload failed");
              }
            } catch (e) {
              input.value = "";
              console.error("[Skills] upload error", e);
            } finally {
              browseBtn.disabled = false;
              browseBtn.style.opacity = "";
              // Reset file input so the same file can be picked again if needed
              fileInput.value = "";
            }
          });
        }
      }
    },

    /** Execute import: validate URL, open a session and send /skill-add <url>. */
    async _doImportFromBar() {
      const input = $("skill-import-input");
      const bar   = $("skill-import-bar");
      const url   = (input ? input.value : "").trim();

      if (!url) {
        input && input.focus();
        return;
      }

      // Validate: accept http(s) URLs or absolute local paths (from upload)
      const isUrl       = /^https?:\/\//i.test(url);
      const isLocalPath = url.startsWith("/") || url.startsWith("~");
      if (!isUrl && !isLocalPath) {
        input.classList.add("skill-import-input-error");
        setTimeout(() => input.classList.remove("skill-import-input-error"), 1200);
        input.focus();
        return;
      }

      // Close the bar immediately — the session takes over from here
      if (bar) bar.style.display = "none";
      if (input) input.value = "";

      // Create a new session and queue the /skill-add command
      try {
        const maxN = Sessions.all.reduce((max, s) => {
          const m = s.name.match(/^Session (\d+)$/);
          return m ? Math.max(max, parseInt(m[1], 10)) : max;
        }, 0);
        const res  = await fetch("/api/sessions", {
          method:  "POST",
          headers: { "Content-Type": "application/json" },
          body:    JSON.stringify({ name: "Session " + (maxN + 1), source: "manual" })
        });
        const data = await res.json();
        if (!res.ok) { alert(I18n.t("tasks.sessionError") + (data.error || "unknown")); return; }

        const session = data.session;
        if (!session) return;

        if (!WS.ready) { WS.connect(); Tasks.load(); }

        Sessions.add(session);
        Sessions.renderList();
        Sessions.setPendingMessage(session.id, `/skill-add ${url}`);
        Sessions.select(session.id);
      } catch (e) {
        console.error("[Skills] import failed", e);
        alert(I18n.lang() === "zh" ? "导入技能时网络错误。" : "Network error while importing skill.");
      }
    },

    /** Create a new custom skill by opening a session and sending /skill-creator. */
    async createInSession(message) {
      const maxN = Sessions.all.reduce((max, s) => {
        const m = s.name.match(/^Session (\d+)$/);
        return m ? Math.max(max, parseInt(m[1], 10)) : max;
      }, 0);
      const res = await fetch("/api/sessions", {
        method:  "POST",
        headers: { "Content-Type": "application/json" },
        body:    JSON.stringify({ name: "Session " + (maxN + 1), source: "manual" })
      });
      const data = await res.json();
      if (!res.ok) { alert(I18n.t("tasks.sessionError") + (data.error || "unknown")); return; }

      const session = data.session;
      if (!session) return;

      // If WS is not yet connected (e.g. called during onboarding), boot the UI
      // first so WS connects, then use setPendingMessage so the command is sent
      // once the socket is ready.
      if (!WS.ready) {
        WS.connect();
        Tasks.load();
      }

      Sessions.add(session);
      Sessions.renderList();
      Sessions.setPendingMessage(session.id, message || "/skill-creator");
      Sessions.select(session.id);
    },
  };
})();

// ─────────────────────────────────────────────────────────────────────────
// SkillAC — slash-command skill autocomplete dropdown + composer bindings
//
// Handles the "/xxx" slash-command autocomplete UI above the message input,
// plus all composer keyboard/composition/input DOM bindings that depend on it
// (Enter to send, / button, IME composition guard).
//
// Moved verbatim from app.js; structural changes only:
//   - `_lastCompositionEndTime` moved into the IIFE closure (was module-level)
//   - The bare DOM bindings (btn-slash, user-input keydown/input/compositionend,
//     btn-create-skill, btn-import-skill) are wrapped in a private
//     `_initDOMBindings()` function called at the end of `init()`.
//
// Depends on: Sessions (sendMessage), Skills (createInSession, toggleImportBar),
//             I18n, global $ helper.
// ─────────────────────────────────────────────────────────────────────────
const SkillAC = (() => {
  let _initialized    = false;
  let _visible        = false;
  let _activeIndex    = -1;
  let _items          = [];  // filtered [{ name, description, encrypted, source }]
  let _currentSession = null; // track active session id for live fetch

  // Load from localStorage, default to false (hide system skills)
  let _showSystemSkills = localStorage.getItem("skill-ac-show-system") === "true";

  // Cross-browser IME composition fix:
  // Safari fires compositionend BEFORE keydown (violating W3C spec), and the
  // gap between compositionend and keydown is ~5ms on Safari. We record the
  // timestamp of compositionend and treat any Enter keydown within 20ms as
  // still-composing. Chrome is unaffected because e.isComposing is still true.
  // Reference: https://bugs.webkit.org/show_bug.cgi?id=165004
  let _lastCompositionEndTime = -Infinity;

  // Command history navigation state
  let _historyIndex = -1;
  let _historyDraft = "";

  /** Called whenever the active session changes — just store the id, no prefetch. */
  function _loadForSession(sessionId) {
    _currentSession = sessionId || null;
  }

  /** Fetch live skill list from server for the current session. */
  async function _fetchSkills() {
    if (!_currentSession) return [];
    try {
      const res  = await fetch(`/api/sessions/${_currentSession}/skills`);
      const data = await res.json();
      return data.skills || [];
    } catch (e) {
      console.error("[SkillAC] fetchSkills failed", e);
      return [];
    }
  }

  /** Return the /xxx prefix if the entire input is a slash command, else null. */
  function _getSlashQuery(value) {
    // Full-width slash / dunhao are already replaced in the input event handler,
    // but guard here too in case value is passed programmatically.
    let trimmed = value.replace(/^[／、]/, "/");

    // Only activate when the whole input starts with / (no leading space)
    if (!trimmed.startsWith("/")) return null;
    // Only single-word slash token — no spaces allowed after /
    if (/^\/\S*$/.test(trimmed)) return trimmed.slice(1).toLowerCase();
    return null;
  }

  /**
   * Score how well a skill matches the query string.
   * Only matches against name and name_zh — description is intentionally excluded.
   * All matches are contiguous substring matches (no fuzzy/subsequence).
   * Returns 0 if no match (should be filtered out).
   *
   * Scoring tiers:
   *   100 — name or name_zh exact match
   *    80 — name or name_zh starts-with
   *    60 — name or name_zh contains
   *     0 — no match
   */
  function _scoreMatch(skill, query) {
    if (!query) return 50; // empty query → show all with neutral score

    const q    = query.toLowerCase();
    const name = (skill.name || "").toLowerCase();
    const zh   = (skill.name_zh || "").toLowerCase();

    // Exact match
    if (name === q || zh === q) return 100;

    // Prefix match
    if (name.startsWith(q) || zh.startsWith(q)) return 80;

    // Contains match (contiguous substring)
    if (name.includes(q) || zh.includes(q)) return 60;

    return 0;
  }

  /**
   * Wrap the matching substring in <mark> for highlighting.
   * Returns an array of DOM nodes (text + mark nodes).
   */
  function _highlight(text, query) {
    if (!query) return [document.createTextNode(text)];
    const idx = text.toLowerCase().indexOf(query.toLowerCase());
    if (idx === -1) return [document.createTextNode(text)];

    const nodes = [];
    if (idx > 0) nodes.push(document.createTextNode(text.slice(0, idx)));
    const mark = document.createElement("span");
    mark.className = "skill-ac-highlight";
    mark.textContent = text.slice(idx, idx + query.length);
    nodes.push(mark);
    if (idx + query.length < text.length) {
      nodes.push(document.createTextNode(text.slice(idx + query.length)));
    }
    return nodes;
  }

  async function _render(query) {
    const all = await _fetchSkills();

    // Score and filter
    let scored = all
      .map(s => ({ skill: s, score: _scoreMatch(s, query) }))
      .filter(({ score }) => score > 0);

    if (!_showSystemSkills) {
      scored = scored.filter(({ skill }) => skill.source_type !== "default");
    }

    // Sort by score descending, stable secondary sort by name
    scored.sort((a, b) => b.score - a.score || a.skill.name.localeCompare(b.skill.name));

    _items = scored.map(({ skill }) => skill);

    const list = $("skill-autocomplete-list");
    list.innerHTML = "";

    if (_items.length === 0) {
      // Show empty state instead of hiding the dropdown
      const emptyEl = document.createElement("div");
      emptyEl.className = "skill-ac-empty";
      emptyEl.textContent = I18n.t("skills.ac.empty");
      list.appendChild(emptyEl);
      $("skill-autocomplete").style.display = "";
      _visible = true;
      _createOverlay();
      return;
    }

    _items.forEach((skill, idx) => {
      const item = document.createElement("div");
      item.className = "skill-ac-item" + (idx === _activeIndex ? " active" : "");
      item.setAttribute("role", "option");
      item.setAttribute("data-idx", idx);

      const nameEl = document.createElement("span");
      nameEl.className = "skill-ac-name";

      const currentLangForName = I18n.lang();
      const showZhFirst = currentLangForName === "zh" && skill.name_zh;

      if (showZhFirst) {
        // Chinese UI: /中文名 first (with slash), then english id (no slash) after
        const zhEl = document.createElement("span");
        zhEl.className = "skill-ac-name-zh";
        zhEl.appendChild(document.createTextNode("/"));
        _highlight(skill.name_zh, query).forEach(function(n) { zhEl.appendChild(n); });
        nameEl.appendChild(zhEl);

        const nameTextEl = document.createElement("span");
        nameTextEl.className = "skill-ac-name-id";
        _highlight(skill.name, query).forEach(function(n) { nameTextEl.appendChild(n); });
        nameEl.appendChild(nameTextEl);
      } else {
        // English UI (or no zh name): show /id only, no zh name
        const nameTextEl = document.createElement("span");
        nameTextEl.appendChild(document.createTextNode("/"));
        _highlight(skill.name, query).forEach(function(n) { nameTextEl.appendChild(n); });
        nameEl.appendChild(nameTextEl);
      }

      // meta: encrypted badge + source type label (subtle)
      const metaEl = document.createElement("span");
      metaEl.className = "skill-ac-meta";
      if (skill.encrypted) {
        const encBadge = document.createElement("span");
        encBadge.className = "skill-ac-enc";
        encBadge.textContent = "🔒";
        metaEl.appendChild(encBadge);
      }
      const sourceLabel = {
        "default":        "built-in",
        "global_octo":  "user",
        "global_claude":  "user",
        "project_octo": "project",
        "project_claude": "project",
      }[skill.source_type];
      if (sourceLabel) {
        const srcEl = document.createElement("span");
        srcEl.className = "skill-ac-src";
        srcEl.textContent = sourceLabel;
        metaEl.appendChild(srcEl);
      }

      const descEl = document.createElement("span");
      descEl.className = "skill-ac-desc";
      const currentLang = I18n.lang();
      const descText = (currentLang === "zh" && skill.description_zh)
        ? skill.description_zh
        : (skill.description || "");
      descEl.textContent = descText;

      item.appendChild(nameEl);
      item.appendChild(metaEl);
      item.appendChild(descEl);

      item.addEventListener("click", () => {
        _select(idx);
      });

      list.appendChild(item);
    });

    $("skill-autocomplete").style.display = "";
    _visible = true;
    _createOverlay();
  }

  function _createOverlay() {
    let overlay = $("skill-ac-overlay");
    if (!overlay) {
      overlay = document.createElement("div");
      overlay.id = "skill-ac-overlay";
      overlay.style.cssText = "position:fixed;top:0;left:0;right:0;bottom:0;z-index:998;background:transparent;";
      overlay.addEventListener("click", _hide);
      document.body.appendChild(overlay);
    }
    overlay.style.display = "block";
  }

  function _removeOverlay() {
    const overlay = $("skill-ac-overlay");
    if (overlay) overlay.style.display = "none";
  }

  function _hide() {
    $("skill-autocomplete").style.display = "none";
    _visible = false;
    _activeIndex = -1;
    _removeOverlay();
  }

  function _select(idx) {
    const skill = _items[idx];
    if (!skill) return;
    const input = $("user-input");
    input.value = "/" + skill.name + " ";
    input.focus();
    _hide();
  }

  function _move(delta) {
    if (!_visible || _items.length === 0) return;
    _activeIndex = (_activeIndex + delta + _items.length) % _items.length;
    const list = $("skill-autocomplete-list");
    Array.from(list.children).forEach((child, i) => {
      child.classList.toggle("active", i === _activeIndex);
    });
  }

  // ── Public API ─────────────────────────────────────────────────────────
  return {
    init() {
      if (_initialized) return;
      _initialized = true;

      // Global click: hide dropdown when clicking outside
      document.addEventListener("click", (e) => {
        if (!_visible) return;
        const ac = $("skill-autocomplete");
        const input = $("user-input");
        if (ac && !ac.contains(e.target) && input && !input.contains(e.target)) {
          _hide();
        }
      });

      // Language change: re-render if visible
      document.addEventListener("langchange", () => {
        if (_visible) {
          const input = $("user-input");
          const query = _getSlashQuery(input ? input.value : "");
          _render(query);
        }
      });

      _initDOMBindings();
    },

    loadForSession(sessionId) {
      _loadForSession(sessionId);
    },

    hide() { _hide(); },

    // Expose for testing / debugging
    get visible() { return _visible; },
    get items()   { return _items.slice(); },
  };

  // ── DOM bindings (called once at init) ─────────────────────────────────
  function _initDOMBindings() {
    const input = $("user-input");
    if (!input) return;

    // Replace full-width slash / dunhao with ASCII slash on input
    input.addEventListener("input", () => {
      const val = input.value;
      const normalized = val.replace(/^[／、]/, "/");
      if (normalized !== val) {
        input.value = normalized;
      }

      const query = _getSlashQuery(input.value);
      if (query !== null) {
        _render(query);
      } else {
        _hide();
      }
    });

    // IME composition guard
    input.addEventListener("compositionend", () => {
      _lastCompositionEndTime = Date.now();
    });

    // Keyboard navigation
    input.addEventListener("keydown", (e) => {
      // Tab / Enter to select highlighted item
      if ((e.key === "Tab" || e.key === "Enter") && _visible) {
        if (_activeIndex >= 0) {
          e.preventDefault();
          _select(_activeIndex);
          return;
        }
        // If no item highlighted but dropdown visible, Enter still sends message
        // (default behavior) — do NOT preventDefault here.
      }

      // Arrow keys to navigate dropdown
      if (_visible) {
        if (e.key === "ArrowDown") { e.preventDefault(); _move(1); return; }
        if (e.key === "ArrowUp")   { e.preventDefault(); _move(-1); return; }
        if (e.key === "Escape")    { e.preventDefault(); _hide(); return; }
      }

      // Enter to send (unless shift or ctrl is held)
      if (e.key === "Enter" && !e.shiftKey && !e.ctrlKey && !e.metaKey) {
        // IME composition guard: Safari fires compositionend ~5ms before keydown.
        // Treat any Enter within 20ms of compositionend as still-composing.
        const isComposing = e.isComposing || (Date.now() - _lastCompositionEndTime < 20);
        if (isComposing) {
          e.preventDefault();
          return;
        }

        e.preventDefault();
        Sessions.sendMessage();
        return;
      }

      // Ctrl/Cmd + Enter = newline
      if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) {
        e.preventDefault();
        const start = input.selectionStart;
        const end   = input.selectionEnd;
        input.value = input.value.slice(0, start) + "\n" + input.value.slice(end);
        input.selectionStart = input.selectionEnd = start + 1;
        return;
      }

      // Up/Down arrow for command history (only when input is empty or single line)
      if ((e.key === "ArrowUp" || e.key === "ArrowDown") && !_visible) {
        const lines = input.value.split("\n");
        const isSingleLine = lines.length === 1;
        const caretAtStart = input.selectionStart === 0 && input.selectionEnd === 0;
        const caretAtEnd   = input.selectionStart === input.value.length && input.selectionEnd === input.value.length;

        if (isSingleLine && ((e.key === "ArrowUp" && caretAtStart) || (e.key === "ArrowDown" && caretAtEnd))) {
          e.preventDefault();
          Sessions.navigateHistory(e.key === "ArrowUp" ? -1 : 1);
          return;
        }
      }
    });

    // Slash button opens autocomplete with "/" prefilled
    const btnSlash = $("btn-slash");
    if (btnSlash) {
      btnSlash.addEventListener("click", () => {
        input.value = "/";
        input.focus();
        _render("");
      });
    }

    // Create skill button
    const btnCreateSkill = $("btn-create-skill");
    if (btnCreateSkill) {
      btnCreateSkill.addEventListener("click", () => {
        Skills.createInSession("/skill-creator");
      });
    }

    // Import skill button
    const btnImportSkill = $("btn-import-skill");
    if (btnImportSkill) {
      btnImportSkill.addEventListener("click", () => {
        Skills.toggleImportBar();
      });
    }
  }
})();
