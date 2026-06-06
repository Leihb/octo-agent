// theme.js — Theme switcher module
//
// Behavior:
//   • Default follows the OS preference (prefers-color-scheme).
//   • User can manually override via the 🌓 header button — persisted
//     to localStorage. Once overridden, the explicit choice wins.
//   • Choosing the theme that happens to match the current OS setting
//     clears the override, restoring "auto-follow-system" mode.
//   • Responds to live OS theme changes when no manual override is set.

const Theme = (() => {
  const STORAGE_KEY = "octo-theme";
  const ATTR_NAME   = "data-theme";

  // ── Helpers ──────────────────────────────────────────────────────────
  function _systemTheme() {
    return window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches
      ? "dark" : "light";
  }

  function _applyAttr(theme) {
    document.documentElement.setAttribute(ATTR_NAME, theme);
    _updateToggleIcon(theme);
  }

  function _updateToggleIcon(theme) {
    const headerToggle = document.getElementById("theme-toggle-header");
    if (headerToggle) {
      // Icon shows what you'd switch TO, not what you are on.
      if (theme === "light") {
        // In light mode → show moon (click to go dark)
        headerToggle.innerHTML = `<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="icon-sm">
          <path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z"/>
        </svg>`;
      } else {
        // In dark mode → show sun (click to go light)
        headerToggle.innerHTML = `<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="icon-sm">
          <circle cx="12" cy="12" r="4"/>
          <path d="M12 2v2"/>
          <path d="M12 20v2"/>
          <path d="m4.93 4.93 1.41 1.41"/>
          <path d="m17.66 17.66 1.41 1.41"/>
          <path d="M2 12h2"/>
          <path d="M20 12h2"/>
          <path d="m6.34 17.66-1.41 1.41"/>
          <path d="m19.07 4.93-1.41 1.41"/>
        </svg>`;
      }
    }

    // Legacy settings toggle (kept for compatibility)
    const toggle = document.getElementById("theme-toggle");
    if (toggle) {
      const icon  = theme === "light" ? "🌙" : "☀️";
      const label = theme === "light" ? "Dark" : "Light";
      toggle.innerHTML = `<span class="theme-icon">${icon}</span><span>${label}</span>`;
    }
  }

  // ── Public API ───────────────────────────────────────────────────────
  function init() {
    const saved = localStorage.getItem(STORAGE_KEY);
    const effective = saved || _systemTheme();
    _applyAttr(effective);

    // Live-follow OS changes when user has no manual override.
    if (window.matchMedia) {
      const mq = window.matchMedia("(prefers-color-scheme: dark)");
      const onChange = () => {
        if (!localStorage.getItem(STORAGE_KEY)) {
          _applyAttr(_systemTheme());
        }
      };
      if (mq.addEventListener) mq.addEventListener("change", onChange);
      else if (mq.addListener) mq.addListener(onChange);  // Safari < 14
    }
  }

  // Explicit apply (used by toggle). Persists the choice unless it equals
  // the OS preference — in which case we clear the override so subsequent
  // OS theme changes once again propagate.
  function apply(theme) {
    _applyAttr(theme);
    if (theme === _systemTheme()) {
      localStorage.removeItem(STORAGE_KEY);
    } else {
      localStorage.setItem(STORAGE_KEY, theme);
    }
  }

  function toggle() {
    const current = document.documentElement.getAttribute(ATTR_NAME) || _systemTheme();
    const next = current === "dark" ? "light" : "dark";
    apply(next);
  }

  function current() {
    return document.documentElement.getAttribute(ATTR_NAME) || _systemTheme();
  }

  return { init, toggle, apply, current };
})();

// Initialize theme on page load
Theme.init();
