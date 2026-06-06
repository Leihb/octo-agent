// ── Auth — access-key authentication ──────────────────────────────────────
//
// On first visit, prompts for an access key. Stores it in localStorage so
// subsequent visits skip the prompt.
//
// Usage:
//   Auth.check()  — call once on boot; returns a Promise that resolves when
//                   either the key is known or the user dismisses the prompt.
//   Auth.getKey() — returns the stored access key, or null.
//   Auth.reset()  — clears stored key (used after 401 from server).
// ─────────────────────────────────────────────────────────────────────────

const Auth = (() => {
  const STORAGE_KEY = "octo-access-key";
  let _key = null;
  let _passed = false;

  /** Return the stored access key, or null if none. */
  function getKey() {
    if (_key !== null) return _key;
    _key = localStorage.getItem(STORAGE_KEY);
    return _key;
  }

  /** Remove the stored key. Called when the server rejects it. */
  function reset() {
    localStorage.removeItem(STORAGE_KEY);
    _key = null;
    _passed = false;
  }

  /** Prompt the user for an access key. Returns a Promise of the entered key,
   *  or rejects if the user cancels. */
  function promptForKey() {
    return new Promise((resolve, reject) => {
      const overlay = document.createElement("div");
      overlay.id = "auth-overlay";
      overlay.className = "auth-overlay";
      overlay.innerHTML = `
        <div class="auth-box">
          <h2 class="auth-title">🐙 Octo</h2>
          <p class="auth-subtitle" data-i18n="auth.subtitle">Enter your access key to continue</p>
          <input type="password" id="auth-input" class="auth-input"
                 placeholder="Access key" autocomplete="off" />
          <div id="auth-error" class="auth-error" style="display:none"></div>
          <div class="auth-actions">
            <button id="auth-submit-btn" class="btn-primary" data-i18n="auth.submit">Connect</button>
          </div>
        </div>
      `;
      document.body.appendChild(overlay);

      const input = document.getElementById("auth-input");
      const errorEl = document.getElementById("auth-error");
      const submitBtn = document.getElementById("auth-submit-btn");

      async function doSubmit() {
        const val = input.value.trim();
        if (!val) {
          errorEl.style.display = "block";
          errorEl.setAttribute("data-i18n", "auth.required");
          if (window.I18N) errorEl.textContent = I18N.t("auth.required");
          else errorEl.textContent = "Access key is required";
          return;
        }
        // Validate key against server before accepting.
        try {
          const resp = await fetch("/api/health?access_key=" + encodeURIComponent(val));
          if (!resp.ok) {
            errorEl.style.display = "block";
            errorEl.setAttribute("data-i18n", "auth.invalid");
            if (window.I18N) errorEl.textContent = I18N.t("auth.invalid");
            else errorEl.textContent = "Invalid access key";
            return;
          }
        } catch (e) {
          errorEl.style.display = "block";
          errorEl.textContent = "Cannot reach server";
          return;
        }
        overlay.remove();
        localStorage.setItem(STORAGE_KEY, val);
        _key = val;
        _passed = true;
        resolve(val);
      }

      submitBtn.addEventListener("click", doSubmit);
      input.addEventListener("keydown", (e) => {
        if (e.key === "Enter") doSubmit();
      });
      input.focus();
    });
  }

  /** Main entry: check if we have a key. If not, prompt. Returns a Promise
   *  that resolves to true (auth ok) or false (user cancelled / dismissed). */
  async function check() {
    if (getKey()) {
      _passed = true;
      return true;
    }
    try {
      await promptForKey();
      return true;
    } catch (e) {
      _passed = false;
      return false;
    }
  }

  return {
    check,
    getKey,
    reset,
    get passed() { return _passed; },
  };
})();

// ── api — authenticated fetch wrapper ──────────────────────────────────────
//
// Automatically appends the access_key query param to every request.
// Use this for all REST API calls from the Web UI.
// ─────────────────────────────────────────────────────────────────────────
const api = {
  fetch(url, options = {}) {
    const key = Auth.getKey();
    let fullUrl = url;
    if (key) {
      const sep = url.includes('?') ? '&' : '?';
      fullUrl = `${url}${sep}access_key=${encodeURIComponent(key)}`;
    }
    return window.fetch(fullUrl, options);
  },

  get(url) {
    return this.fetch(url, { method: 'GET' });
  },

  post(url, body) {
    return this.fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
  },

  delete(url) {
    return this.fetch(url, { method: 'DELETE' });
  },
};
