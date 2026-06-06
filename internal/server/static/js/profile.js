// ── Profile — Assistant Memory panel ─────────────────────────────────────
//
// Three tabs: Soul (SOUL.md), User (USER.md), Memories (~/.octo/memories/).
// Content is loaded from REST API. Mutations are done via /onboard sessions.
// ─────────────────────────────────────────────────────────────────────────

const Profile = (() => {
  function init() {
    // Wire tab switching.
    document.querySelectorAll(".profile-tab").forEach(tab => {
      tab.addEventListener("click", () => {
        const tabName = tab.dataset.tab;
        // Deactivate all.
        document.querySelectorAll(".profile-tab").forEach(t => t.classList.remove("active"));
        document.querySelectorAll(".profile-tab-panel").forEach(p => p.style.display = "none");
        // Activate selected.
        tab.classList.add("active");
        const panel = document.getElementById("profile-pane-" + tabName);
        if (panel) panel.style.display = "";
        // Load content.
        if (tabName === "soul") loadSoul();
        else if (tabName === "user") loadUser();
        else if (tabName === "memories") loadMemories();
      });
    });

    loadSoul();
  }

  async function loadSoul() {
    const container = $("profile-soul-body");
    if (!container) return;
    try {
      const res = await api.get("/api/profile/soul");
      if (!res.ok) {
        container.textContent = "SOUL.md not found. Run onboarding to create one.";
        return;
      }
      const data = await res.json();
      container.innerHTML = renderMarkdown(data.content || "");
    } catch (e) {
      container.textContent = "Failed to load SOUL.md";
    }
  }

  async function loadUser() {
    const container = $("profile-user-body");
    if (!container) return;
    try {
      const res = await api.get("/api/profile/user");
      if (!res.ok) {
        container.textContent = "USER.md not found. Run onboarding to create one.";
        return;
      }
      const data = await res.json();
      container.innerHTML = renderMarkdown(data.content || "");
    } catch (e) {
      container.textContent = "Failed to load USER.md";
    }
  }

  async function loadMemories() {
    const container = $("memories-list");
    if (!container) return;
    try {
      const res = await api.get("/api/memories");
      if (!res.ok) {
        container.textContent = "No memories yet.";
        return;
      }
      const data = await res.json();
      if (!data.files || data.files.length === 0) {
        container.textContent = "No memories yet. They'll appear as you work with the assistant.";
        return;
      }
      let html = "";
      data.files.forEach(f => {
        html += `<div style="border:1px solid var(--border);border-radius:6px;padding:10px 14px;margin-bottom:6px;background:var(--surface)">
          <div style="font-weight:600;font-size:13px">${escapeHtml(f.name)}</div>
        </div>`;
      });
      container.innerHTML = html;
    } catch (e) {
      container.textContent = "Failed to load memories.";
    }
  }

  return { init, loadSoul, loadUser, loadMemories };
})();

// Router hook.
(function() {
  const origNav = Router.navigate;
  Router.navigate = function(view, params) {
    origNav.call(Router, view, params);
    if (view === "profile") {
      Profile.loadSoul();
    }
  };
})();
