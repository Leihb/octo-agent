// settings.js — Settings panel logic
// Handles reading, editing, saving, testing AI model configurations.

const Settings = (() => {
  // Local copy of models loaded from server
  let _models = [];
  // Provider presets loaded from server
  let _providers = [];

  // ── Public API ──────────────────────────────────────────────────────────────

  function open() {
    _load();
    _loadBrowserStatus();
  }

  // ── Data Loading ────────────────────────────────────────────────────────────

  async function _load() {
    const container = document.getElementById("model-cards");
    container.innerHTML = `<div class="settings-loading">${I18n.t("settings.models.loading")}</div>`;
    try {
      // Load config and providers in parallel
      const [configRes, providerRes] = await Promise.all([
        fetch("/api/config"),
        fetch("/api/providers")
      ]);
      const configData   = await configRes.json();
      const providerData = await providerRes.json();
      _models    = configData.models   || [];
      _providers = providerData.providers || [];
      _renderCards();
    } catch (e) {
      container.innerHTML = `<div class="settings-error">${I18n.t("settings.models.error", { msg: e.message })}</div>`;
    }
  }

  // ── Rendering ───────────────────────────────────────────────────────────────

  function _renderCards() {
    const container = document.getElementById("model-cards");
    container.innerHTML = "";

    if (_models.length === 0) {
      container.innerHTML = `<div class="settings-empty">${I18n.t("settings.models.empty")}</div>`;
      return;
    }

    _models.forEach((m, i) => _renderCard(container, m, i));
  }

  function _renderCard(container, model, index) {
    const isDefault = model.type === "default";
    const isLite    = model.type === "lite";

    const card = document.createElement("div");
    card.className = "model-card";
    card.dataset.index = index;

    // Build provider options
    const providerOptions = _providers.map(p =>
      `<option value="${p.id}">${p.name}</option>`
    ).join("");

    card.innerHTML = `
      <div class="model-card-header">
        <div class="model-card-badges">
          ${isDefault ? `<span class="badge badge-default">${I18n.t("settings.models.badge.default")}</span>` : ""}
          ${isLite    ? `<span class="badge badge-lite">${I18n.t("settings.models.badge.lite")}</span>` : ""}
          ${!isDefault && !isLite ? `<span class="badge badge-secondary">${I18n.t("settings.models.badge.model", { n: index + 1 })}</span>` : ""}
        </div>
        <div class="model-card-actions">
          ${_models.length > 1
            ? `<button class="btn-model-remove" data-index="${index}" title="Remove this model">×</button>`
            : ""}
        </div>
      </div>

      <div class="model-fields">
        <label class="model-field quick-setup-field" ${(model.model || model.base_url) ? 'style="display:none"' : ''}>
          <span class="field-label">${I18n.t("settings.models.field.quicksetup")}</span>
          <div class="custom-select-wrapper" data-index="${index}">
            <div class="custom-select-trigger" tabindex="0">
              <span class="custom-select-value placeholder">${I18n.t("settings.models.placeholder.provider")}</span>
              <svg class="custom-select-arrow" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 9l-7 7-7-7"></path>
              </svg>
            </div>
            <div class="custom-select-dropdown">
              <div class="custom-select-option" data-value="">${I18n.t("settings.models.placeholder.provider")}</div>
              ${_providers.map(p => `<div class="custom-select-option" data-value="${p.id}" data-label="${_esc(p.name)}">${_esc(p.name)}</div>`).join("")}
              <div class="custom-select-option" data-value="custom">${I18n.t("settings.models.custom")}</div>
            </div>
          </div>
          <div class="provider-promo-hint" data-index="${index}"></div>
        </label>
        <label class="model-field">
          <span class="field-label">${I18n.t("settings.models.field.model")}</span>
          <div class="model-name-combobox" data-index="${index}">
            <input type="text" class="field-input model-name-input" data-key="model" data-index="${index}"
              placeholder="${I18n.t("settings.models.placeholder.model")}" value="${_esc(model.model)}"
              autocomplete="off">
            <button class="model-name-dropdown-btn" type="button" title="Select from presets">
              <svg width="12" height="12" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 9l-7 7-7-7"></path>
              </svg>
            </button>
            <div class="model-name-dropdown" style="display:none"></div>
          </div>
        </label>
        <label class="model-field">
          <span class="field-label">${I18n.t("settings.models.field.baseurl")}</span>
          <div class="base-url-combobox" data-index="${index}">
            <input type="text" class="field-input base-url-input" data-key="base_url" data-index="${index}"
              placeholder="${I18n.t("settings.models.placeholder.baseurl")}" value="${_esc(model.base_url)}"
              autocomplete="off">
            <button class="base-url-dropdown-btn" type="button" title="Select preset endpoint">
              <svg width="12" height="12" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 9l-7 7-7-7"></path>
              </svg>
            </button>
            <div class="base-url-dropdown" style="display:none"></div>
          </div>
        </label>
        <label class="model-field">
          <span class="field-label">
            ${I18n.t("settings.models.field.apikey")}
            <a class="get-apikey-link" data-index="${index}" href="#" target="_blank" rel="noopener" style="display:none;margin-left:0.5rem;font-size:0.75rem;color:var(--accent,#6366f1);text-decoration:none;opacity:0.85;">${I18n.t("settings.models.field.getApiKey")}</a>
          </span>
          <div class="field-input-row">
            <input type="password" class="field-input api-key-input" data-key="api_key" data-index="${index}"
              placeholder="${I18n.t("settings.models.placeholder.apikey")}" value="${_esc(model.api_key_masked)}">
            <button class="btn-toggle-key" data-index="${index}" title="Show/hide key">
              <svg width="16" height="16" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 12a3 3 0 11-6 0 3 3 0 016 0z"></path>
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M2.458 12C3.732 7.943 7.523 5 12 5c4.478 0 8.268 2.943 9.542 7-1.274 4.057-5.064 7-9.542 7-4.477 0-8.268-2.943-9.542-7z"></path>
              </svg>
            </button>
          </div>
        </label>
      </div>

      <div class="model-card-footer">
        ${!model.api_key_masked ? `<span class="model-card-docs-link" style="font-size:0.75rem;">
          <span style="color:var(--muted,#6b7280);">${I18n.t("settings.models.field.docsGuide.question")}</span>
          <a href="https://www.octo.com/docs/ai-key-guide" target="_blank" rel="noopener" style="margin-left:0.25rem;color:var(--accent,#6366f1);text-decoration:none;">${I18n.t("settings.models.field.docsGuide.cta")}</a>
        </span>` : ""}
        <span class="model-test-result" data-index="${index}"></span>
        <div class="model-card-actions-row">
          ${!isDefault ? `<button class="btn-set-default" data-index="${index}" title="${I18n.t("settings.models.btn.setDefault")}">${I18n.t("settings.models.btn.setDefault")}</button>` : ""}
          <button class="btn-save-model btn-primary" data-index="${index}">${I18n.t("settings.models.btn.save")}</button>
        </div>
      </div>
    `;

    container.appendChild(card);
    _bindCardEvents(card, index);
  }

  function _bindCardEvents(card, index) {
    // Custom dropdown interactions
    const customSelectWrapper = card.querySelector(".custom-select-wrapper");
    const trigger = customSelectWrapper.querySelector(".custom-select-trigger");
    const dropdown = customSelectWrapper.querySelector(".custom-select-dropdown");
    const valueSpan = trigger.querySelector(".custom-select-value");
    const options = dropdown.querySelectorAll(".custom-select-option");

    // Toggle dropdown
    trigger.addEventListener("click", (e) => {
      e.stopPropagation();
      const isOpen = dropdown.classList.contains("open");
      // Close all other dropdowns
      document.querySelectorAll(".custom-select-dropdown.open").forEach(d => {
        d.classList.remove("open");
        d.previousElementSibling.classList.remove("open");
      });
      if (!isOpen) {
        dropdown.classList.add("open");
        trigger.classList.add("open");
      }
    });

    // Select option
    options.forEach(option => {
      option.addEventListener("click", (e) => {
        e.stopPropagation();
        const value = option.dataset.value;
        const text = option.dataset.label || option.textContent;
        
        // Update UI
        valueSpan.textContent = text;
        if (value) {
          valueSpan.classList.remove("placeholder");
        } else {
          valueSpan.classList.add("placeholder");
        }
        
        // Update selected state
        options.forEach(opt => opt.classList.remove("selected"));
        option.classList.add("selected");
        
        // Close dropdown
        dropdown.classList.remove("open");
        trigger.classList.remove("open");
        
        // Auto-fill model & base_url if a provider preset was selected
        const getApiKeyLink = card.querySelector(`.get-apikey-link[data-index="${index}"]`);
        const promoHint = card.querySelector(`.provider-promo-hint[data-index="${index}"]`);
        if (value && value !== "custom") {
          const preset = _providers.find(p => p.id === value);
          if (preset) {
            const modelInput   = card.querySelector(`[data-key="model"]`);
            const baseUrlInput = card.querySelector(`[data-key="base_url"]`);
            if (modelInput)   modelInput.value   = preset.default_model || "";
            if (baseUrlInput) baseUrlInput.value = preset.base_url       || "";
            // Show "how to get" link if provider has a website_url
            if (getApiKeyLink && preset.website_url) {
              getApiKeyLink.href = preset.website_url;
              getApiKeyLink.style.display = "";
            } else if (getApiKeyLink) {
              getApiKeyLink.style.display = "none";
            }
          }
          if (promoHint) promoHint.classList.remove("visible");
        } else {
          if (getApiKeyLink) getApiKeyLink.style.display = "none";
        }
      });
    });

    // Close dropdown when clicking outside
    document.addEventListener("click", () => {
      dropdown.classList.remove("open");
      trigger.classList.remove("open");
    });

    // Toggle API key visibility
    const toggleKeyBtn = card.querySelector(".btn-toggle-key");
    const apiKeyInput = card.querySelector(".api-key-input");
    const eyeIcon = toggleKeyBtn.querySelector("svg");
    
    toggleKeyBtn.addEventListener("click", () => {
      const isPassword = apiKeyInput.type === "password";
      apiKeyInput.type = isPassword ? "text" : "password";
      
      // Update icon
      if (isPassword) {
        // Show eye-off icon
        eyeIcon.innerHTML = `
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M13.875 18.825A10.05 10.05 0 0112 19c-4.478 0-8.268-2.943-9.543-7a9.97 9.97 0 011.563-3.029m5.858.908a3 3 0 114.243 4.243M9.878 9.878l4.242 4.242M9.88 9.88l-3.29-3.29m7.532 7.532l3.29 3.29M3 3l3.59 3.59m0 0A9.953 9.953 0 0112 5c4.478 0 8.268 2.943 9.543 7a10.025 10.025 0 01-4.132 5.411m0 0L21 21"></path>
        `;
      } else {
        // Show eye icon
        eyeIcon.innerHTML = `
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 12a3 3 0 11-6 0 3 3 0 016 0z"></path>
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M2.458 12C3.732 7.943 7.523 5 12 5c4.478 0 8.268 2.943 9.542 7-1.274 4.057-5.064 7-9.542 7-4.477 0-8.268-2.943-9.542-7z"></path>
        `;
      }
    });

    // Save: auto-test first, then save if passed
    card.querySelector(".btn-save-model").addEventListener("click", () => _saveModel(index));

    // Remove model
    const removeBtn = card.querySelector(".btn-model-remove");
    if (removeBtn) {
      removeBtn.addEventListener("click", () => _removeModel(index));
    }

    // Set as default model
    const setDefaultBtn = card.querySelector(".btn-set-default");
    if (setDefaultBtn) {
      setDefaultBtn.addEventListener("click", () => _setAsDefault(index));
    }

    // Model name combobox: dropdown button + model list
    const modelCombobox = card.querySelector(".model-name-combobox");
    const modelInput = modelCombobox.querySelector(".model-name-input");
    const modelDropdownBtn = modelCombobox.querySelector(".model-name-dropdown-btn");
    const modelDropdown = modelCombobox.querySelector(".model-name-dropdown");

    // Build model list from current base_url's provider
    const _updateModelDropdown = () => {
      const baseUrlInput = card.querySelector(`[data-key="base_url"]`);
      const baseUrl = baseUrlInput ? baseUrlInput.value.trim().replace(/\/+$/, "") : "";

      // Find provider by matching base_url against BOTH the canonical
      // preset.base_url AND every endpoint_variants[].base_url — otherwise
      // picking e.g. GLM's Coding-Plan variant would wipe the model list
      // because only the canonical URL would match.
      const provider = _providers.find(p => {
        const candidates = [p.base_url].concat(
          Array.isArray(p.endpoint_variants) ? p.endpoint_variants.map(v => v.base_url) : []
        ).filter(Boolean);
        return candidates.some(c => {
          const norm = String(c).replace(/\/+$/, "");
          return baseUrl === norm || baseUrl.startsWith(norm + "/");
        });
      });
      const models = provider?.models || [];

      if (models.length === 0) {
        modelDropdown.innerHTML = '<div class="model-dropdown-empty">No preset models available</div>';
        return;
      }

      // Render model options
      modelDropdown.innerHTML = models.map(m => 
        `<div class="model-dropdown-option" data-value="${_esc(m)}">${_esc(m)}</div>`
      ).join("");

      // Bind click events
      modelDropdown.querySelectorAll(".model-dropdown-option").forEach(opt => {
        opt.addEventListener("click", (e) => {
          e.stopPropagation();
          const value = opt.dataset.value;
          if (modelInput) modelInput.value = value;
          modelDropdown.style.display = "none";
        });
      });
    };

    // Toggle dropdown
    modelDropdownBtn.addEventListener("click", (e) => {
      e.stopPropagation();
      const isOpen = modelDropdown.style.display === "block";
      
      // Close all other model dropdowns
      document.querySelectorAll(".model-name-dropdown").forEach(d => {
        d.style.display = "none";
      });

      if (!isOpen) {
        _updateModelDropdown();
        modelDropdown.style.display = "block";
      }
    });

    // Close dropdown when clicking outside
    document.addEventListener("click", () => {
      modelDropdown.style.display = "none";
    });

    // Re-populate model list when base_url changes
    const baseUrlInput = card.querySelector(`[data-key="base_url"]`);
    if (baseUrlInput) {
      baseUrlInput.addEventListener("blur", () => {
        _updateModelDropdown();
      });
    }

    // Base URL combobox: dropdown button + endpoint_variants list.
    //
    // Rationale: some providers (GLM on Zhipu/Z.ai, MiniMax on .com/.io) run
    // multiple regional / billing-plan endpoints under a single identity.
    // Listing every variant lets the user pick the right one instead of
    // hand-editing the URL, while still allowing free-form input for
    // unknown / self-hosted proxies. Mirrors the model-name combobox.
    //
    // Data source: the endpoint_variants[] field on each provider preset,
    // resolved by matching the currently-entered base_url against every
    // preset's {base_url + endpoint_variants[].base_url}. When no variants
    // are declared for the matched provider (single-endpoint providers like
    // Anthropic, Octo), the dropdown shows an "empty" hint.
    const baseUrlCombobox   = card.querySelector(".base-url-combobox");
    const baseUrlDropdownBtn = baseUrlCombobox.querySelector(".base-url-dropdown-btn");
    const baseUrlDropdown   = baseUrlCombobox.querySelector(".base-url-dropdown");

    // Resolve the "active" provider preset from the current form values:
    // 1. If the Quick Setup select points at a known provider, use that
    //    (even before the base_url input is typed into).
    // 2. Otherwise fall back to matching the current base_url against all
    //    preset base_url + endpoint_variants. Unknown URLs → null.
    const _currentProvider = () => {
      const selected = card.querySelector(".custom-select-option.selected");
      const selectedId = selected?.dataset.value;
      if (selectedId && selectedId !== "custom") {
        const byId = _providers.find(p => p.id === selectedId);
        if (byId) return byId;
      }
      const url = (baseUrlInput?.value || "").trim().replace(/\/+$/, "");
      if (!url) return null;
      return _providers.find(p => {
        const candidates = [p.base_url].concat(
          Array.isArray(p.endpoint_variants) ? p.endpoint_variants.map(v => v.base_url) : []
        ).filter(Boolean);
        return candidates.some(c => {
          const norm = String(c).replace(/\/+$/, "");
          return url === norm || url.startsWith(norm + "/");
        });
      }) || null;
    };

    const _renderBaseUrlDropdown = () => {
      const provider = _currentProvider();
      const variants = provider && Array.isArray(provider.endpoint_variants)
        ? provider.endpoint_variants
        : [];

      if (variants.length === 0) {
        baseUrlDropdown.innerHTML =
          `<div class="model-dropdown-empty">${I18n.t("settings.models.baseurl.noVariants")}</div>`;
        return;
      }

      baseUrlDropdown.innerHTML = variants.map(v => {
        // Prefer i18n key (localised per UI language); fall back to literal
        // `label` (shipped English copy) and finally to base_url for safety.
        // Pattern: _translateVariant(v) -> "大陆 · 按量付费" in zh, "Mainland · Pay-as-you-go" in en.
        const translated = v.label_key ? I18n.t(v.label_key) : null;
        // I18n.t typically returns the key itself when missing — treat that as a miss.
        const labelText = (translated && translated !== v.label_key) ? translated : (v.label || v.base_url);
        const label = _esc(labelText);
        const url   = _esc(v.base_url);
        return `
          <div class="model-dropdown-option base-url-dropdown-option" data-value="${url}">
            <div class="base-url-dropdown-label">${label}</div>
            <div class="base-url-dropdown-url">${url}</div>
          </div>`;
      }).join("");

      baseUrlDropdown.querySelectorAll(".base-url-dropdown-option").forEach(opt => {
        opt.addEventListener("click", (e) => {
          e.stopPropagation();
          if (baseUrlInput) {
            baseUrlInput.value = opt.dataset.value;
            // Trigger model-list refresh since base_url just changed.
            _updateModelDropdown();
          }
          baseUrlDropdown.style.display = "none";
        });
      });
    };

    baseUrlDropdownBtn.addEventListener("click", (e) => {
      e.stopPropagation();
      const isOpen = baseUrlDropdown.style.display === "block";
      // Close sibling dropdowns (model-name + other base-url) to avoid overlap.
      document.querySelectorAll(".model-name-dropdown, .base-url-dropdown").forEach(d => {
        d.style.display = "none";
      });
      if (!isOpen) {
        _renderBaseUrlDropdown();
        baseUrlDropdown.style.display = "block";
      }
    });

    // Close dropdown when clicking outside
    document.addEventListener("click", () => {
      baseUrlDropdown.style.display = "none";
    });
  }

  // ── Read form values from a card ────────────────────────────────────────────

  function _readCard(index) {
    const card = document.querySelector(`.model-card[data-index="${index}"]`);
    if (!card) return null;
    return {
      index,
      model:            card.querySelector(`[data-key="model"]`).value.trim(),
      base_url:         card.querySelector(`[data-key="base_url"]`).value.trim(),
      api_key:          card.querySelector(`[data-key="api_key"]`).value.trim(),
      anthropic_format: false,
      type:             _models[index]?.type ?? null
    };
  }

  // ── Save ─────────────────────────────────────────────────────────────────────

  async function _saveModel(index) {
    const saveBtn = document.querySelector(`.btn-save-model[data-index="${index}"]`);
    const updated = _readCard(index);
    if (!updated) return;

    saveBtn.disabled = true;

    // Step 1: auto-test first
    saveBtn.textContent = I18n.t("settings.models.btn.testing");
    _showTestResult(index, null, "");

    try {
      const testRes = await fetch("/api/config/test", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ ...updated, index })
      });
      const testData = await testRes.json();
      _showTestResult(index, testData.ok, testData.message);

      if (!testData.ok) {
        // Test failed — stop, let user fix
        saveBtn.textContent = I18n.t("settings.models.btn.save");
        saveBtn.disabled = false;
        return;
      }
    } catch (e) {
      _showTestResult(index, false, e.message);
      saveBtn.textContent = I18n.t("settings.models.btn.save");
      saveBtn.disabled = false;
      return;
    }

    // Step 2: test passed — now save via single-item endpoint.
    //
    // Contract (see http_server.rb):
    //   - Row has an id already → PATCH /api/config/models/:id
    //   - No id yet (locally-added row) → POST /api/config/models to
    //     create, then capture the server-assigned id.
    // We NEVER send "the whole list" — each save touches exactly one row,
    // so no bug in this function can ever affect another model's api_key.
    saveBtn.textContent = I18n.t("settings.models.btn.saving");

    const existing = _models[index] || {};
    const hasId    = !!existing.id;

    // For PATCH: only send api_key if the user actually typed something
    // non-masked. The masked display value ("sk-ab12****...5678") must
    // never be sent as api_key — the server treats it as "no change"
    // defensively, but the cleanest path is simply to omit it.
    const payload = {
      model:            updated.model,
      base_url:         updated.base_url,
      anthropic_format: updated.anthropic_format,
      type:             updated.type
    };
    if (updated.api_key && !updated.api_key.includes("****")) {
      payload.api_key = updated.api_key;
    }

    try {
      let res, data;
      if (hasId) {
        res  = await fetch(`/api/config/models/${encodeURIComponent(existing.id)}`, {
          method: "PATCH",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(payload)
        });
        data = await res.json();
      } else {
        // Creation requires a non-empty api_key — surface a friendly
        // error rather than a server 422.
        if (!payload.api_key) {
          saveBtn.textContent = I18n.t("settings.models.btn.save");
          saveBtn.disabled    = false;
          _showTestResult(index, false, I18n.t("settings.models.placeholder.apikey"));
          return;
        }
        res  = await fetch(`/api/config/models`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(payload)
        });
        data = await res.json();
        if (data.ok && data.id) {
          // Record the assigned id so subsequent saves become PATCH.
          _models[index].id = data.id;
        }
      }

      if (data.ok) {
        saveBtn.textContent = I18n.t("settings.models.btn.saved");
        setTimeout(() => { saveBtn.textContent = I18n.t("settings.models.btn.save"); saveBtn.disabled = false; }, 1500);
        // Reload to get fresh masked keys
        setTimeout(_load, 1600);
      } else {
        saveBtn.textContent = I18n.t("settings.models.btn.save");
        saveBtn.disabled = false;
        _showTestResult(index, false, data.error || I18n.t("settings.models.saveFailed"));
      }
    } catch (e) {
      saveBtn.textContent = I18n.t("settings.models.btn.save");
      saveBtn.disabled = false;
      _showTestResult(index, false, e.message);
    }
  }

  function _showTestResult(index, ok, message) {
    const el = document.querySelector(`.model-test-result[data-index="${index}"]`);
    if (!el) return;
    if (ok === null) { el.textContent = ""; el.className = "model-test-result"; return; }
    el.textContent = ok ? `✓ ${message || I18n.t("settings.models.connected")}` : `✗ ${I18n.t("settings.models.testFail")}: ${message || I18n.t("settings.models.failed")}`;
    el.className   = `model-test-result ${ok ? "result-ok" : "result-fail"}`;
  }

  // ── Set as Default Model ───────────────────────────────────────────────────

  async function _setAsDefault(index) {
    const btn = document.querySelector(`.btn-set-default[data-index="${index}"]`);
    if (!btn) return;

    const target = _models[index];
    if (!target || !target.id) {
      // Can only promote saved models. Locally-added unsaved cards have
      // no id yet — user must save them first.
      alert(I18n.t("settings.models.setDefaultFailed"));
      return;
    }

    btn.disabled    = true;
    btn.textContent = I18n.t("settings.models.btn.setting");

    try {
      const res = await fetch(`/api/config/models/${encodeURIComponent(target.id)}/default`, {
        method: "POST"
      });
      const data = await res.json();

      if (data.ok) {
        btn.textContent = I18n.t("settings.models.btn.done");
        // Reload to refresh the UI
        setTimeout(_load, 800);
      } else {
        btn.textContent = I18n.t("settings.models.btn.setDefault");
        btn.disabled    = false;
        alert(data.error || I18n.t("settings.models.setDefaultFailed"));
      }
    } catch (e) {
      btn.textContent = I18n.t("settings.models.btn.setDefault");
      btn.disabled    = false;
      alert(I18n.t("settings.models.errorPrefix") + e.message);
    }
  }

  // ── Add / Remove model ───────────────────────────────────────────────────────

  function _addModel() {
    // Locally append an empty card. No server call here — the new entry
    // is persisted via POST /api/config/models only when the user clicks
    // "Save" (inside _saveModel). This means typing into a half-filled
    // new card can never mutate the real config on the backend.
    //
    // UX convention: a newly added model becomes the "default" once
    // saved — users overwhelmingly add a new model because they want to
    // use it. We mark it default locally for the preview, and flip the
    // other cards' type to null so the UI only shows one default badge.
    // The backend enforces the single-default invariant on save anyway.
    _models.forEach(m => { if (m.type === "default") m.type = null; });
    _models.push({
      id:               null,   // will be assigned by server on first save
      index:            _models.length,
      model:            "",
      base_url:         "",
      api_key_masked:   "",
      anthropic_format: false,
      type:             "default"
    });
    _renderCards();

    // Scroll the new (last) card into view.
    //
    // Why not just scrollIntoView: the settings panel uses #settings-body
    // as a flex scroll container. When the page hasn't overflowed yet,
    // scrollIntoView is a no-op; and across browsers the "smooth" path
    // has been flaky when the scrolling element is a flex child.
    //
    // Two rAFs ensure the freshly rendered card has been laid out before
    // we compute its position.
    requestAnimationFrame(() => {
      requestAnimationFrame(() => {
        const container = document.getElementById("settings-body");
        if (!container) return;
        const cards = container.querySelectorAll(".model-card");
        const last  = cards[cards.length - 1];
        if (!last) return;

        // Align the new card's top with the container's top, minus a
        // small breathing-room offset so it doesn't touch the edge.
        // offsetTop is relative to the nearest positioned ancestor, so
        // we compute position via getBoundingClientRect delta — robust
        // regardless of the card's offsetParent chain.
        const OFFSET = 16;
        const delta  = last.getBoundingClientRect().top
                     - container.getBoundingClientRect().top;
        container.scrollTo({
          top:      container.scrollTop + delta - OFFSET,
          behavior: "smooth"
        });

        // Put focus on the new card so the user can start configuring.
        // Priority order:
        //   1. The provider `.custom-select-trigger` — nudges the user to
        //      pick a provider first (step 1 of the 3-field form). It's a
        //      div with tabindex="0", so it gets the accent border via
        //      `:focus` without expanding the dropdown.
        //   2. Fall back to the first form input (used when the quick-setup
        //      field is hidden, e.g. on a model card that already has values).
        const providerTrigger = last.querySelector(".custom-select-wrapper .custom-select-trigger");
        const isVisible = el => el && el.offsetParent !== null;
        if (isVisible(providerTrigger)) {
          providerTrigger.focus({ preventScroll: true });
        } else {
          const firstInput = last.querySelector("input, select, textarea");
          if (firstInput) firstInput.focus({ preventScroll: true });
        }
      });
    });
  }

  async function _removeModel(index) {
    if (_models.length <= 1) return;
    const modelName = _models[index]?.model || String(index + 1);
    const confirmed = await Modal.confirm(I18n.t("settings.models.confirmRemove", { model: modelName }));
    if (!confirmed) return;

    const target = _models[index];

    // Unsaved local card → just drop it from the local list, no server call.
    if (!target || !target.id) {
      _models.splice(index, 1);
      _renderCards();
      return;
    }

    try {
      const res = await fetch(`/api/config/models/${encodeURIComponent(target.id)}`, {
        method: "DELETE"
      });
      // Whatever the server says, reload to reflect the true state.
      // (On error, _load will re-show the model.)
      if (!res.ok) {
        const data = await res.json().catch(() => ({}));
        alert(data.error || I18n.t("settings.models.setDefaultFailed"));
      }
    } catch (_) { /* ignore */ }

    // Reload fresh state
    _load();
  }

  // ── Helpers ──────────────────────────────────────────────────────────────────

  function _esc(str) {
    return (str || "").replace(/&/g, "&amp;").replace(/"/g, "&quot;").replace(/</g, "&lt;");
  }

  // ── Rerun onboard ────────────────────────────────────────────────────────────

  async function _rerunOnboard() {
    const btn = document.getElementById("btn-rerun-onboard");
    btn.disabled    = true;
    btn.textContent = I18n.t("settings.personalize.btn.starting");

    try {
      // Close settings panel and navigate to chat, then start the onboard session.
      // Onboard.startSoulSession() creates a new session, selects it, and sends /onboard.
      Router.navigate("chat");
      await Onboard.startSoulSession();
    } catch (e) {
      btn.disabled    = false;
      btn.textContent = I18n.t("settings.personalize.btn.rerun");
    }
  }

  // ── Browser Setup ─────────────────────────────────────────────────────────────

  async function _loadBrowserStatus() {
    try {
      const res  = await fetch("/api/browser/status");
      const data = await res.json();
      const desc        = document.getElementById("browser-status-desc");
      const btn         = document.getElementById("btn-browser-setup");
      const toggleWrap  = document.getElementById("browser-toggle-wrap");
      const toggleInput = document.getElementById("browser-toggle-input");

      // File doesn't exist → not set up yet
      if (data.enabled === undefined || data.enabled === null) {
        desc.textContent = I18n.t("settings.browser.desc");
        btn.textContent  = I18n.t("settings.browser.btn");
        toggleWrap.style.display = "none";
        return;
      }

      // Configured — show toggle + reconfigure button
      const version = data.chrome_version ? ` (Chrome v${data.chrome_version})` : "";
      desc.textContent     = I18n.t(data.enabled ? "settings.browser.configured" : "settings.browser.disabled") + version;
      btn.textContent      = I18n.t("settings.browser.btn.reconfigure");
      toggleWrap.style.display = "inline-block";
      toggleInput.checked  = data.enabled;

      // Only bind once
      if (!toggleInput.dataset.bound) {
        toggleInput.dataset.bound = "1";
        toggleInput.addEventListener("change", _toggleBrowser);
      }
    } catch (_) { /* non-critical */ }
  }

  async function _toggleBrowser() {
    const toggleInput = document.getElementById("browser-toggle-input");
    toggleInput.disabled = true;
    try {
      const res  = await fetch("/api/browser/toggle", { method: "POST" });
      const data = await res.json();
      if (!data.ok) throw new Error(data.error || "toggle failed");
      await _loadBrowserStatus();
    } catch (_) {
      // Revert on failure
      toggleInput.checked  = !toggleInput.checked;
    } finally {
      toggleInput.disabled = false;
    }
  }

  async function _setupBrowser() {
    const btn = document.getElementById("btn-browser-setup");
    btn.disabled    = true;
    btn.textContent = I18n.t("settings.browser.btn.starting");
    try {
      const res = await fetch("/api/sessions", {
        method:  "POST",
        headers: { "Content-Type": "application/json" },
        body:    JSON.stringify({ name: "🌐 Browser Setup", source: "setup" })
      });
      const data    = await res.json();
      const session = data.session;
      if (!session) throw new Error("No session returned");
      Sessions.add(session);
      Sessions.renderList();
      Sessions.setPendingMessage(session.id, "/browser-setup");
      Sessions.select(session.id);
    } catch (e) {
      btn.disabled    = false;
      btn.textContent = I18n.t("settings.browser.btn");
    }
  }

  // ── Init ──────────────────────────────────────────────────────────────────────

  function _initLangBtns() {
    // Highlight the active language button on open
    document.querySelectorAll("#language-section .settings-lang-btn").forEach(btn => {
      btn.classList.toggle("active", btn.dataset.lang === I18n.lang());
      btn.addEventListener("click", () => {
        I18n.setLang(btn.dataset.lang);
        document.querySelectorAll("#language-section .settings-lang-btn").forEach(b =>
          b.classList.toggle("active", b.dataset.lang === I18n.lang())
        );
      });
    });
  }

  function init() {
    document.getElementById("btn-add-model").addEventListener("click", _addModel);
    document.getElementById("btn-rerun-onboard").addEventListener("click", _rerunOnboard);
    document.getElementById("btn-browser-setup").addEventListener("click", _setupBrowser);

    _initLangBtns();
    _initFontBtns();
    _initCurrencyBtns();

    // Re-render model cards when language changes (dynamic HTML, not data-i18n)
    document.addEventListener("langchange", () => _renderCards());
  }

  // ── Currency ──────────────────────────────────────────────────────────
  const CURRENCY_STORAGE_KEY = "octo-currency";
  const CURRENCY_DEFAULT     = "USD";

  function _applyCurrency(currency) {
    try { localStorage.setItem(CURRENCY_STORAGE_KEY, currency); } catch (_) {}
    // Update active state on all currency buttons
    document.querySelectorAll("#currency-section .settings-lang-btn").forEach(btn => {
      btn.classList.toggle("active", btn.dataset.currency === currency);
    });
    // Dispatch event for billing panel to update
    document.dispatchEvent(new CustomEvent("currencychange", { detail: { currency } }));
  }

  function _initCurrencyBtns() {
    // Apply saved preference (or default) on page load
    let saved = null;
    try { saved = localStorage.getItem(CURRENCY_STORAGE_KEY); } catch (_) {}
    const current = saved || CURRENCY_DEFAULT;

    // Wire up button clicks
    document.querySelectorAll("#currency-section .settings-lang-btn").forEach(btn => {
      btn.classList.toggle("active", btn.dataset.currency === current);
      btn.addEventListener("click", () => {
        _applyCurrency(btn.dataset.currency);
      });
    });
  }

  // ── Font Size ──────────────────────────────────────────────────────────
  const FONT_STORAGE_KEY = "octo-font-size";
  const FONT_DEFAULT     = "medium";

  function _applyFontSize(size) {
    document.documentElement.setAttribute("data-font-size", size);
    try { localStorage.setItem(FONT_STORAGE_KEY, size); } catch (_) {}
    // Update active state on all font-size buttons (if settings panel is open)
    document.querySelectorAll("#font-size-section .settings-lang-btn").forEach(btn => {
      btn.classList.toggle("active", btn.dataset.font === size);
    });
  }

  function _initFontBtns() {
    // Apply saved preference (or default) on page load
    let saved = null;
    try { saved = localStorage.getItem(FONT_STORAGE_KEY); } catch (_) {}
    _applyFontSize(saved || FONT_DEFAULT);

    // Wire up button clicks
    document.querySelectorAll("#font-size-section .settings-lang-btn").forEach(btn => {
      btn.classList.toggle("active", btn.dataset.font === (saved || FONT_DEFAULT));
      btn.addEventListener("click", () => {
        _applyFontSize(btn.dataset.font);
      });
    });
  }

  return { open, init };
})();
