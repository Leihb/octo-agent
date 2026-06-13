<script lang="ts">
  import { onMount } from 'svelte'
  import Segment from '../components/ui/Segment.svelte'
  import Switch from '../components/ui/Switch.svelte'
  import { showToast, activeSessionId } from '../lib/stores'
  import { setLocale, locale } from '../lib/i18n'
  import * as api from '../lib/api'

  // --- local state ---
  let language      = $state('en')
  let fontSize      = $state('Medium')
  let theme         = $state('Light')
  let model         = $state('')
  let modelOptions  = $state<string[]>([])
  let reasoning     = $state('Medium')
  let permMode      = $state('Ask')
  let workdir       = $state('')
  let desktopNotif  = $state(true)
  let failureNotif  = $state(true)
  let versionStr    = $state('')
  let saving        = $state(false)
  let loading       = $state(true)

  // Original values for dirty-checking
  let origModel   = ''
  let origWorkdir = ''

  const langOptions = [
    { value: 'en',  label: 'English' },
    { value: 'zh',  label: '简体中文' },
    { value: 'zh-TW', label: '繁體中文' },
  ]

  // The whole UI is sized in px, so a :root font-size has no effect. Scale the
  // app with `zoom` instead — that visibly resizes text (and everything else),
  // which is what "base text size" means in practice here.
  const fontZoomMap: Record<string, string> = { Small: '0.9', Medium: '1', Large: '1.1' }

  onMount(async () => {
    const savedFont = localStorage.getItem('octo.fontSize')
    if (savedFont) fontSize = savedFont
    await Promise.all([loadConfig(), loadVersion()])
  })

  async function loadConfig() {
    loading = true
    try {
      const cfg = await api.getConfig() as any
      // models list
      const ms: any[] = cfg.models ?? []
      modelOptions = ms.map((m: any) => m.model ?? m.id)
      const defaultIdx = cfg.default_model_idx ?? 0
      const def = ms[defaultIdx]
      if (def) {
        model = def.model ?? def.id ?? ''
        reasoning = capitalize(def.reasoning_effort ?? 'medium')
        permMode  = capitalize(def.permission_mode ?? 'ask')
      }
      origModel = model
      // Font size is a client-only preference (persisted in localStorage); the
      // server only hardcodes a placeholder, so don't let it clobber the saved
      // choice. Language still seeds from config when present.
      if (cfg.language)   language = cfg.language
    } catch (e: any) {
      showToast(`Failed to load config: ${e.message}`, 'error')
    } finally {
      loading = false
    }
  }

  async function loadVersion() {
    try {
      const v = await api.getVersion() as any
      versionStr = v.current ?? v.version ?? ''
    } catch { /* non-critical */ }
  }

  function capitalize(s: string): string {
    if (!s) return s
    return s[0].toUpperCase() + s.slice(1).toLowerCase()
  }

  $effect(() => {
    // Apply font size via zoom and remember it across reloads.
    ;(document.documentElement.style as any).zoom = fontZoomMap[fontSize] ?? '1'
    localStorage.setItem('octo.fontSize', fontSize)
  })

  $effect(() => {
    // Apply theme
    const themeMap: Record<string, string> = { Light: 'light', Dark: 'dark', System: 'system' }
    document.documentElement.setAttribute('data-theme', themeMap[theme] ?? 'light')
  })

  $effect(() => {
    // Apply language
    setLocale(language === 'zh' || language === 'zh-TW' ? 'zh' : 'en')
  })

  async function handleSave() {
    saving = true
    const sid = $activeSessionId
    try {
      if (sid) {
        const promises: Promise<void>[] = []
        if (model !== origModel) {
          promises.push(api.updateSessionModel(sid, model))
        }
        if (workdir !== origWorkdir) {
          promises.push(api.updateSessionWorkingDir(sid, workdir))
        }
        const effortMap: Record<string, string> = { Low: 'low', Medium: 'medium', High: 'high' }
        promises.push(api.updateSessionReasoningEffort(sid, effortMap[reasoning] ?? 'medium'))
        await Promise.all(promises)
        origModel   = model
        origWorkdir = workdir
      }
      showToast('Settings saved', 'success')
    } catch (e: any) {
      showToast(`Save failed: ${e.message}`, 'error')
    } finally {
      saving = false
    }
  }
</script>

<div class="page">
  <div class="inner">
    <div class="page-header">
      <h2>Settings</h2>
      <p>Workspace preferences and agent defaults</p>
    </div>

    {#if loading}
      <div class="loading-state">Loading settings…</div>
    {:else}
      <!-- General -->
      <div class="section-card">
        <div class="section-title">General</div>
        <div class="setting-row">
          <div class="setting-info">
            <span class="setting-label">Language</span>
            <span class="setting-desc">Display language for the interface</span>
          </div>
          <select class="sel" bind:value={language}>
            {#each langOptions as o}
              <option value={o.value}>{o.label}</option>
            {/each}
          </select>
        </div>
        <div class="setting-row">
          <div class="setting-info">
            <span class="setting-label">Font Size</span>
            <span class="setting-desc">Base text size across chat and tables</span>
          </div>
          <Segment options={['Small', 'Medium', 'Large']} bind:value={fontSize} />
        </div>
        <div class="setting-row last">
          <div class="setting-info">
            <span class="setting-label">Theme</span>
            <span class="setting-desc">Appearance of the workbench</span>
          </div>
          <Segment options={['Light', 'Dark', 'System']} bind:value={theme} />
        </div>
      </div>

      <!-- Agent defaults -->
      <div class="section-card">
        <div class="section-title">Agent Defaults</div>
        <div class="setting-row">
          <div class="setting-info">
            <span class="setting-label">Default Model</span>
            <span class="setting-desc">Used for new sessions unless overridden</span>
          </div>
          {#if modelOptions.length > 0}
            <select class="sel" bind:value={model}>
              {#each modelOptions as o}<option value={o}>{o}</option>{/each}
            </select>
          {:else}
            <input class="input" bind:value={model} placeholder="e.g. claude-sonnet-4-5" />
          {/if}
        </div>
        <div class="setting-row">
          <div class="setting-info">
            <span class="setting-label">Reasoning Effort</span>
            <span class="setting-desc">Higher effort thinks longer before answering</span>
          </div>
          <Segment options={['Low', 'Medium', 'High']} bind:value={reasoning} />
        </div>
        <div class="setting-row">
          <div class="setting-info">
            <span class="setting-label">Permission Mode</span>
            <span class="setting-desc">Ask confirms file changes and shell commands before running</span>
          </div>
          <Segment options={['Ask', 'Auto']} bind:value={permMode} />
        </div>
        <div class="setting-row last">
          <div class="setting-info">
            <span class="setting-label">Working Directory</span>
            <span class="setting-desc">Default project root for new sessions</span>
          </div>
          <input class="input" bind:value={workdir} placeholder="~/code/my-project" />
        </div>
      </div>

      <!-- Notifications -->
      <div class="section-card">
        <div class="section-title">Notifications</div>
        <div class="setting-row">
          <div class="setting-info">
            <span class="setting-label">Desktop Notifications</span>
            <span class="setting-desc">Notify when the agent finishes or needs input</span>
          </div>
          <Switch bind:checked={desktopNotif} />
        </div>
        <div class="setting-row last">
          <div class="setting-info">
            <span class="setting-label">Notify on Task Failure</span>
            <span class="setting-desc">Send an alert to your default channel when a scheduled task fails</span>
          </div>
          <Switch bind:checked={failureNotif} />
        </div>
      </div>

      <!-- Save -->
      <div class="save-row">
        <button class="btn-primary" onclick={handleSave} disabled={saving}>
          {saving ? 'Saving…' : 'Save Changes'}
        </button>
        {#if versionStr}
          <span class="version-badge">Version {versionStr}</span>
        {/if}
      </div>
    {/if}
  </div>
</div>

<style>
.page { flex: 1; overflow-y: auto; min-height: 0; }
.inner { max-width: 800px; margin: 0 auto; padding: 24px; display: flex; flex-direction: column; gap: 24px; }
.page-header { display: flex; flex-direction: column; gap: 4px; }
h2 { margin: 0; font-size: 24px; font-weight: 600; color: #1F1F1F; }
p { margin: 0; font-size: 14px; color: rgba(0,0,0,0.65); }
.loading-state { padding: 40px; text-align: center; color: rgba(0,0,0,0.45); font-size: 14px; }
.section-card { background: #fff; border-radius: 16px; box-shadow: 0 8px 24px rgba(15,23,42,0.03); overflow: hidden; }
.section-title { padding: 16px 24px; border-bottom: 1px solid #F0F0F0; font-size: 16px; font-weight: 600; color: #1F1F1F; }
.setting-row {
  display: flex; align-items: center; justify-content: space-between;
  gap: 24px; padding: 14px 24px; border-bottom: 1px solid #F0F0F0;
}
.setting-row.last { border-bottom: none; }
.setting-info { display: flex; flex-direction: column; gap: 2px; min-width: 0; }
.setting-label { font-size: 14px; color: rgba(0,0,0,0.88); }
.setting-desc { font-size: 12px; color: rgba(0,0,0,0.45); }
.sel {
  width: 220px; flex: 0 0 auto; height: 32px; padding: 0 10px;
  border: 1px solid #D9D9D9; border-radius: 6px; font-size: 13px;
  color: rgba(0,0,0,0.88); font-family: inherit; background: #fff; cursor: pointer; outline: none;
}
.sel:focus { border-color: #1677FF; box-shadow: 0 0 0 2px rgba(5,145,255,0.1); }
.input {
  width: 220px; flex: 0 0 auto; height: 32px; padding: 0 10px;
  border: 1px solid #D9D9D9; border-radius: 6px; font-size: 13px;
  color: rgba(0,0,0,0.88); font-family: inherit; outline: none;
}
.input:focus { border-color: #1677FF; box-shadow: 0 0 0 2px rgba(5,145,255,0.1); }
.save-row { display: flex; align-items: center; gap: 16px; }
.btn-primary { height: 32px; padding: 0 16px; border: none; background: #1677FF; border-radius: 6px; font-size: 14px; color: #fff; cursor: pointer; font-family: inherit; }
.btn-primary:hover:not(:disabled) { background: #4096FF; }
.btn-primary:disabled { opacity: 0.5; cursor: not-allowed; }
.version-badge { font-size: 12px; color: rgba(0,0,0,0.35); }
</style>
