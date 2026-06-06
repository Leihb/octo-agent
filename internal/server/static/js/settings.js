// ── Settings — application settings panel ───────────────────────────────
//
// Manages model configuration, language, and personalization.
// ─────────────────────────────────────────────────────────────────────────

const Settings = (() => {
  let _models = [];

  async function init() {
    await loadModels();
    wireEvents();
  }

  async function loadModels() {
    // Models are read from server config. For now, show a placeholder.
    _models = [
      { provider: "anthropic", model: "claude-sonnet-4-5-20250929" },
      { provider: "anthropic", model: "claude-haiku-4-5-20251001" },
      { provider: "openai", model: "gpt-4o" },
    ];
    renderModels();
  }

  function renderModels() {
    const container = $("model-cards");
    if (!container) return;

    if (_models.length === 0) {
      container.innerHTML = `<div style="color:var(--text-secondary);padding:10px">No models configured</div>`;
      return;
    }

    let html = "";
    _models.forEach((m, idx) => {
      html += `
        <div style="border:1px solid var(--border);border-radius:8px;padding:12px 16px;margin-bottom:8px;background:var(--surface);display:flex;justify-content:space-between;align-items:center">
          <div>
            <div style="font-weight:600;font-size:13px">${escapeHtml(m.model)}</div>
            <div style="color:var(--text-secondary);font-size:12px">${escapeHtml(m.provider)}</div>
          </div>
          <button class="btn-model-remove" data-idx="${idx}" style="padding:4px 10px;border:1px solid var(--error);border-radius:4px;font-size:12px;cursor:pointer;background:transparent;color:var(--error)">Remove</button>
        </div>`;
    });
    container.innerHTML = html;

    container.querySelectorAll(".btn-model-remove").forEach(btn => {
      btn.addEventListener("click", () => {
        const idx = parseInt(btn.dataset.idx);
        _models.splice(idx, 1);
        renderModels();
        syncModelSelect();
      });
    });

    syncModelSelect();
  }

  // Sync the new-session modal's model select with the current model list.
  function syncModelSelect() {
    const select = $("new-session-model");
    if (!select) return;
    const currentVal = select.value;
    select.innerHTML = "";
    _models.forEach(m => {
      const opt = document.createElement("option");
      opt.value = m.model;
      opt.textContent = `${escapeHtml(m.model)} (${escapeHtml(m.provider)})`;
      select.appendChild(opt);
    });
    // Restore previous selection if still valid.
    if (currentVal && _models.find(m => m.model === currentVal)) {
      select.value = currentVal;
    }
  }

  function wireEvents() {
    const addBtn = $("btn-add-model");
    if (addBtn) {
      addBtn.addEventListener("click", () => {
        const provider = prompt("Provider (anthropic, openai):") || "anthropic";
        const model = prompt("Model name:") || "claude-sonnet-4-5-20250929";
        _models.push({ provider, model });
        renderModels();
      });
    }

    const onboardBtn = $("btn-rerun-onboard");
    if (onboardBtn) {
      onboardBtn.addEventListener("click", () => {
        if (Sessions.activeId) {
          Sessions.sendMessage("/onboard");
        }
      });
    }

    // Language buttons.
    document.querySelectorAll(".settings-lang-btn").forEach(btn => {
      btn.addEventListener("click", () => {
        const lang = btn.dataset.lang;
        if (lang) {
          localStorage.setItem("octo-lang", lang);
          alert(`Language set to ${lang}. Reload to apply.`);
        }
      });
    });
  }

  return { init, loadModels };
})();
