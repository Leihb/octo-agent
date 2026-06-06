// ── Sessions — session state, rendering, message handling ────────────────
//
// Responsibilities:
//   - Maintain the canonical sessions list
//   - session_list (WS) populates the list on connect
//   - Render the session sidebar list
//   - Manage per-session message DOM
//   - Select / deselect sessions (delegates panel switching to Router)
//   - Load message history via GET /api/sessions/:id/messages
//   - Handle WS events for real-time message display
//
// Depends on: WS (ws.js), WSDispatcher (ws-dispatcher.js), Router (app.js)
// ─────────────────────────────────────────────────────────────────────────

const Sessions = (() => {
  const _sessions       = [];  // [{ id, name, status, created_at, ... }]
  let   _activeId       = null;
  let   _hasMore        = false;
  let   _loadingMore    = false;
  // Search state
  const _filter         = { q: "", date: "", type: "" };
  let   _searchOpen     = false;
  let   _cronView       = false;
  let   _cronCount      = 0;

  // ── Sidebar rendering ──────────────────────────────────────────────────

  function renderList() {
    const container = $("session-list");
    if (!container) return;

    if (_sessions.length === 0) {
      container.innerHTML = `<div style="padding:20px;color:var(--text-secondary);font-size:13px;text-align:center">No sessions yet</div>`;
      return;
    }

    let html = "";
    _sessions.forEach(s => {
      const activeCls = s.id === _activeId ? " active" : "";
      const name = escapeHtml(s.name || s.id.slice(0, 8));
      const date = new Date(s.created_at).toLocaleString();
      html += `
        <div class="session-item${activeCls}" data-sid="${escapeHtml(s.id)}">
          <div class="sess-body">
            <div class="sess-id">${name}</div>
            <div style="font-size:11px;color:var(--text-secondary)">${date}</div>
          </div>
        </div>`;
    });
    container.innerHTML = html;

    // Wire click handlers.
    container.querySelectorAll(".session-item").forEach(el => {
      el.addEventListener("click", () => {
        const sid = el.dataset.sid;
        if (sid) select(sid);
      });
    });
  }

  // ── Session selection ──────────────────────────────────────────────────

  async function select(id) {
    if (id === _activeId) return;

    // Handle deselect (e.g., switching to welcome).
    if (!id) {
      if (_activeId) {
        WS.send({ type: "unsubscribe", session_id: _activeId });
      }
      _activeId = null;
      WS.setSubscribedSession(null);
      const msgs = $("messages");
      if (msgs) msgs.innerHTML = "";
      const bar = $("session-info-bar");
      if (bar) bar.style.display = "none";
      renderList();
      return;
    }

    // Unsubscribe previous, subscribe new.
    if (_activeId) {
      WS.send({ type: "unsubscribe", session_id: _activeId });
    }
    _activeId = id;
    WS.setSubscribedSession(id);
    WS.send({ type: "subscribe", session_id: id });

    Router.navigate("session", { id });

    // Load message history from REST API.
    try {
      const res = await api.get(`/api/sessions/${id}`);
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const sess = await res.json();

      renderMessages(sess.messages || []);
    } catch (err) {
      console.error("Failed to load session:", err);
    }

    renderList();
    updateInfoBar();
  }

  function renderMessages(messages) {
    const container = $("messages");
    if (!container) return;
    container.innerHTML = "";

    if (!messages || messages.length === 0) return;

    messages.forEach(m => {
      if (m.role === "user") {
        addUserBubble(m.content);
      } else if (m.role === "assistant" && m.content) {
        addBotBubble(m.content);
      }
    });

    scrollToBottom(true);
  }

  // ── Message rendering (from WS events) ─────────────────────────────────

  function addUserBubble(content) {
    const container = $("messages");
    if (!container) return;
    const atBottom = isNearBottom();

    const div = document.createElement("div");
    div.className = "message user";
    div.innerHTML = `<div class="meta">You</div><div class="content">${escapeHtml(content)}</div>`;
    container.appendChild(div);
    scrollToBottomIfNeeded(atBottom);
    return div;
  }

  function addBotBubble(content) {
    const container = $("messages");
    if (!container) return;
    const atBottom = isNearBottom();

    const div = document.createElement("div");
    div.className = "message bot";
    div.innerHTML = `<div class="meta">Octo</div><div class="content">${renderMarkdown(content || "")}</div>`;
    container.appendChild(div);
    scrollToBottomIfNeeded(atBottom);
    return div;
  }

  function addToolCard(emoji, title, body, errorStyle) {
    const container = $("messages");
    if (!container) return;
    const atBottom = isNearBottom();

    const card = document.createElement("div");
    card.className = "tool-call";
    if (errorStyle) card.style.borderColor = "rgba(248,81,73,.3)";

    let nameColor = "var(--accent)";
    if (errorStyle) nameColor = "var(--error)";

    card.innerHTML = `
      <div class="tool-name" style="color:${nameColor}">${escapeHtml(emoji)} ${escapeHtml(title)}</div>
      ${body ? `<div class="tool-args">${escapeHtml(body)}</div>` : ""}
    `;
    container.appendChild(card);
    scrollToBottomIfNeeded(atBottom);
    return card;
  }

  function addErrorBubble(message) {
    const container = $("messages");
    if (!container) return;
    const atBottom = isNearBottom();

    const div = document.createElement("div");
    div.className = "message bot";
    div.style.borderColor = "var(--error)";
    div.innerHTML = `<div class="meta" style="color:var(--error)">Error</div><div class="content">${escapeHtml(message)}</div>`;
    container.appendChild(div);
    scrollToBottomIfNeeded(atBottom);
    return div;
  }

  function showTyping() {
    const container = $("messages");
    if (!container) return;
    const atBottom = isNearBottom();

    const div = document.createElement("div");
    div.className = "typing";
    div.id = "typing";
    div.innerHTML = "Thinking<span></span><span></span><span></span>";
    container.appendChild(div);
    scrollToBottomIfNeeded(atBottom);
  }

  function hideTyping() {
    const el = document.getElementById("typing");
    if (el) el.remove();
  }

  // ── Info bar ───────────────────────────────────────────────────────────

  function updateInfoBar() {
    const bar = $("session-info-bar");
    if (!bar) return;
    if (_activeId) {
      bar.style.display = "";
      const sibId = $("sib-id");
      if (sibId) sibId.textContent = _activeId.slice(0, 8);
      const sibStatus = $("sib-status");
      if (sibStatus) sibStatus.textContent = "●";
    } else {
      bar.style.display = "none";
    }
  }

  // ── Send message ───────────────────────────────────────────────────────

  function sendMessage(text) {
    if (!text || !text.trim()) return;

    const content = text.trim();
    $("user-input").value = "";

    if (!_activeId) {
      // No active session — the first message creates one.
      // For now, use the REST API to create a session.
      createSessionAndSend(content);
      return;
    }

    // Show user bubble, send via WS.
    addUserBubble(content);
    showTyping();
    setSendEnabled(false);

    WS.send({
      type: "user_message",
      session_id: _activeId,
      content: content,
    });
  }

  async function createSessionAndSend(content) {
    try {
      const res = await api.post("/api/chat", { message: content });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const data = await res.json();
      _activeId = data.session_id;
      WS.setSubscribedSession(data.session_id);
      WS.send({ type: "subscribe", session_id: data.session_id });

      addUserBubble(content);
      Router.navigate("session", { id: data.session_id });
      updateInfoBar();
      renderList();
    } catch (err) {
      console.error("Failed to create session:", err);
    }
  }

  function setSendEnabled(enabled) {
    const btn = $("btn-send");
    if (btn) btn.disabled = !enabled;
  }

  // ── Scroll helpers ─────────────────────────────────────────────────────

  function isNearBottom() {
    const el = $("messages");
    if (!el) return true;
    return el.scrollHeight - el.scrollTop - el.clientHeight < 80;
  }

  function scrollToBottomIfNeeded(wasNearBottom) {
    if (wasNearBottom) scrollToBottom(true);
  }

  function scrollToBottom(force) {
    const el = $("messages");
    if (!el) return;
    if (force || isNearBottom()) el.scrollTop = el.scrollHeight;
  }

  // ── WS event handlers ──────────────────────────────────────────────────

  function init() {
    // Register WS event handlers via dispatcher.
    WSDispatcher.on("_ws_connected", () => {
      // Reload session list on reconnect.
      WS.send({ type: "list_sessions" });
    });

    WSDispatcher.on("session_list", (ev) => {
      _sessions.length = 0;
      if (ev.sessions) {
        _sessions.push(...ev.sessions);
      }
      renderList();
    });

    WSDispatcher.on("history_user_message", (ev) => {
      if (!ev.content) return;
      addUserBubble(ev.content);
    });

    WSDispatcher.on("assistant_message", (ev) => {
      if (!ev.content) return;
      hideTyping();
      addBotBubble(ev.content);
    });

    WSDispatcher.on("tool_call", (ev) => {
      addToolCard("🔧", ev.name || "tool",
        ev.summary || (ev.args ? JSON.stringify(ev.args) : ""));
    });

    WSDispatcher.on("tool_result", (ev) => {
      addToolCard("✅", "result",
        typeof ev.result === "string" ? ev.result : JSON.stringify(ev.result));
    });

    WSDispatcher.on("tool_error", (ev) => {
      addToolCard("❌", "error", ev.error || "Unknown error", true);
      setSendEnabled(true);
    });

    WSDispatcher.on("error", (ev) => {
      // Generic error message from server.
      addErrorBubble(ev.message || "An error occurred");
      setSendEnabled(true);
      hideTyping();
    });

    WSDispatcher.on("tool_stdout", (ev) => {
      if (ev.lines && ev.lines.length > 0) {
        addToolCard("📤", "stdout", ev.lines.join("\n"));
      }
    });

    WSDispatcher.on("text_delta", (ev) => {
      // Streamed text deltas — appended to the last bot bubble.
      const msgs = $("messages");
      if (!msgs) return;
      hideTyping();
      const atBottom = isNearBottom();
      let last = msgs.querySelector(".message.bot:last-of-type .content");
      if (!last) {
        // No bot bubble yet — create one.
        const div = addBotBubble("");
        last = div.querySelector(".content");
      }
      if (last && ev.text) {
        last.textContent += ev.text;
      }
      scrollToBottomIfNeeded(atBottom);
    });

    WSDispatcher.on("turn_done", (ev) => {
      setSendEnabled(true);
      updateInfoBar();
      // Re-render Markdown on the last bot bubble now that streaming is done.
      if (ev.reply && ev.reply.content) {
        const msgs = $("messages");
        if (msgs) {
          const last = msgs.querySelector(".message.bot:last-of-type .content");
          if (last) {
            last.innerHTML = renderMarkdown(ev.reply.content);
          }
        }
      }
    });

    WSDispatcher.on("complete", (ev) => {
      setSendEnabled(true);
    });

    WSDispatcher.on("session_update", (ev) => {
      if (ev.status) {
        const sibStatus = $("sib-status");
        if (sibStatus) {
          sibStatus.textContent = ev.status === "working" ? "◉" : "●";
          sibStatus.style.color = ev.status === "working"
            ? "var(--accent)" : "var(--success)";
        }
      }
      // Update session in list.
      const item = _sessions.find(s => s.id === _activeId);
      if (item && ev.tasks !== undefined) {
        item.total_turns = ev.tasks || item.total_turns;
      }
    });

    WSDispatcher.on("session_deleted", (ev) => {
      const idx = _sessions.findIndex(s => s.id === ev.session_id);
      if (idx >= 0) {
        _sessions.splice(idx, 1);
        if (_activeId === ev.session_id) {
          _activeId = null;
          Router.navigate("welcome");
        }
        renderList();
      }
    });

    WSDispatcher.on("_ws_disconnected", () => {
      const hint = $("ws-disconnect-hint");
      if (hint) {
        hint.style.display = "";
        hint.textContent = "⚠ Disconnected. Reconnecting…";
        hint.style.color = "var(--warning)";
      }
    });

    WSDispatcher.on("_ws_connected", () => {
      const hint = $("ws-disconnect-hint");
      if (hint) hint.style.display = "none";
    });

    // Load initial session list from REST as fallback.
    api.get("/api/sessions")
      .then(r => r.json())
      .then(data => {
        if (Array.isArray(data) && _sessions.length === 0) {
          _sessions.push(...data.map(s => ({
            id: s.id,
            name: s.title || s.id.slice(0, 8),
            status: "idle",
            created_at: new Date(s.created_at).getTime(),
            source: "manual",
            model: s.model,
            total_turns: s.turn_count,
          })));
          renderList();
        }
      })
      .catch(() => {});

    renderList();
  }

  // ── Public API ─────────────────────────────────────────────────────────

  return {
    init,
    select,
    sendMessage,
    renderList,
    get activeId() { return _activeId; },
  };
})();

// ── Global event wiring (called after DOM is ready) ─────────────────────
(function wireInputEvents() {
  document.addEventListener("DOMContentLoaded", () => {
    const input = $("user-input");
    const sendBtn = $("btn-send");
    const newBtn = $("btn-new-session-inline");
    const welcomeNewBtn = $("btn-welcome-new");
    const interruptBtn = $("btn-interrupt");

    // Auto-resize textarea.
    if (input) {
      input.addEventListener("input", () => {
        input.style.height = "auto";
        input.style.height = Math.min(input.scrollHeight, 120) + "px";
      });

      input.addEventListener("keydown", (e) => {
        if (e.key === "Enter" && !e.shiftKey && !e.isComposing && e.keyCode !== 229) {
          e.preventDefault();
          Sessions.sendMessage(input.value);
        }
      });
    }

    if (sendBtn) sendBtn.addEventListener("click", () => {
      Sessions.sendMessage($("user-input")?.value || "");
    });

    if (newBtn) newBtn.addEventListener("click", () => {
      Router.navigate("welcome");
      Sessions.select(null);
    });

    if (welcomeNewBtn) welcomeNewBtn.addEventListener("click", () => {
      const input = $("user-input");
      if (input) input.focus();
    });

    if (interruptBtn) interruptBtn.addEventListener("click", () => {
      if (Sessions.activeId) {
        WS.send({ type: "interrupt", session_id: Sessions.activeId });
      }
    });

    // Sidebar navigation.
    const tasksItem = $("tasks-sidebar-item");
    const skillsItem = $("skills-sidebar-item");
    const channelsItem = $("channels-sidebar-item");
    const profileItem = $("profile-sidebar-item");
    const trashItem = $("trash-sidebar-item");
    const settingsBtn = $("btn-settings");
    const themeBtn = $("theme-toggle-header");
    const sidebarToggle = $("btn-toggle-sidebar");
    const brandEl = $("header-brand");

    if (tasksItem) tasksItem.addEventListener("click", () => Router.navigate("tasks"));
    if (skillsItem) skillsItem.addEventListener("click", () => Router.navigate("skills"));
    if (channelsItem) channelsItem.addEventListener("click", () => Router.navigate("channels"));
    if (profileItem) profileItem.addEventListener("click", () => Router.navigate("profile"));
    if (trashItem) trashItem.addEventListener("click", () => Router.navigate("trash"));
    if (settingsBtn) settingsBtn.addEventListener("click", () => Router.navigate("settings"));

    // Theme toggle.
    if (themeBtn) themeBtn.addEventListener("click", () => {
      const current = document.documentElement.getAttribute("data-theme");
      const next = current === "light" ? "dark" : "light";
      document.documentElement.setAttribute("data-theme", next);
      localStorage.setItem("octo-theme", next);
    });

    // Sidebar toggle (mobile).
    if (sidebarToggle) sidebarToggle.addEventListener("click", () => {
      const sidebar = $("sidebar");
      if (sidebar) sidebar.classList.toggle("open");
    });

    // Close sidebar on overlay click.
    const overlay = $("sidebar-overlay");
    if (overlay) overlay.addEventListener("click", () => {
      const sidebar = $("sidebar");
      if (sidebar) sidebar.classList.remove("open");
    });

    // File attach button.
    const attachBtn = $("btn-attach");
    const fileInput = $("image-file-input");
    if (attachBtn && fileInput) {
      attachBtn.addEventListener("click", () => fileInput.click());
    }

    // Interrupt button visibility.
    const btnInterrupt = $("btn-interrupt");
    WSDispatcher.on(["text_delta", "tool_call"], () => {
      if (btnInterrupt) btnInterrupt.style.display = "";
    });
    WSDispatcher.on(["turn_done", "complete", "tool_error"], () => {
      if (btnInterrupt) btnInterrupt.style.display = "none";
    });

    // New session modal.
    const newSessionModalBtn = $("btn-new-session-more");
    const modalOverlay = $("new-session-modal");
    const modalCancel = $("new-session-cancel");
    const modalCreate = $("new-session-create");
    if (newSessionModalBtn && modalOverlay) {
      newSessionModalBtn.addEventListener("click", () => {
        modalOverlay.style.display = "";
      });
    }
    if (modalCancel && modalOverlay) {
      modalCancel.addEventListener("click", () => {
        modalOverlay.style.display = "none";
      });
    }
    if (modalCreate && modalOverlay) {
      modalCreate.addEventListener("click", async () => {
        const name = $("new-session-name")?.value || "";
        const model = $("new-session-model")?.value || "";
        const dir = $("new-session-directory")?.value || "";
        modalOverlay.style.display = "none";
        try {
          const res = await api.post("/api/chat", { message: "/help", model, name });
          if (res.ok) {
            const data = await res.json();
            Sessions.select(data.session_id);
          }
        } catch (e) {
          console.error(e);
        }
      });
    }

    // New session split button dropdown.
    const splitArrow = $("btn-new-session-arrow");
    const dropdown = $("new-session-dropdown");
    if (splitArrow && dropdown) {
      splitArrow.addEventListener("click", (e) => {
        e.stopPropagation();
        dropdown.hidden = !dropdown.hidden;
      });
      document.addEventListener("click", () => {
        dropdown.hidden = true;
      });
    }
  });
})();
