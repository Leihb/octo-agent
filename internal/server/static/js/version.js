// ── Version — version badge in sidebar footer ────────────────────────────
//
// Fetches /api/version to show the current server version. Displays an
// "update available" dot when the server signals a newer version exists.
// ─────────────────────────────────────────────────────────────────────────

const Version = (() => {
  function init() {
    loadVersion();
  }

  async function loadVersion() {
    try {
      const res = await api.get("/api/version");
      if (!res.ok) return;
      const data = await res.json();

      const badge = document.getElementById("version-badge");
      const text = document.getElementById("version-text");
      if (badge && text && data.version) {
        badge.style.display = "";
        text.textContent = "v" + data.version;
      }
    } catch (e) {
      // Non-critical — version endpoint may not exist yet.
    }
  }

  return { init };
})();

Version.init();
