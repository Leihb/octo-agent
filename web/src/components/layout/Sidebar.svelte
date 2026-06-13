<script lang="ts">
  import { onMount } from 'svelte'
  import { view, sidebar, sessions, activeSession, activeSessionId, selMode, sel, menuFor, editId, editDraft, showToast } from '../../lib/stores'
  import * as api from '../../lib/api'

  let versionStr = $state('')

  onMount(async () => {
    try {
      const v = await api.getVersion() as any
      const raw = v.current ?? v.version ?? ''
      versionStr = raw ? (raw.startsWith('v') ? raw : `v${raw}`) : ''
    } catch { /* version chip stays hidden */ }
  })

  $effect(() => {
    function onResize() {
      const w = window.innerWidth
      const next = w < 640 ? 'hidden' : w < 860 ? 'rail' : 'full'
      sidebar.set(next)
    }
    window.addEventListener('resize', onResize)
    onResize()
    return () => window.removeEventListener('resize', onResize)
  })

  const railNav = [
    { icon: 'ant-design:message-outlined', title: 'Chat', v: 'chat' },
    { icon: 'ant-design:clock-circle-outlined', title: 'Scheduled Tasks', v: 'tasks' },
    { icon: 'ant-design:thunderbolt-outlined', title: 'Skills', v: 'skills' },
    { icon: 'ant-design:api-outlined', title: 'MCP Servers', v: 'mcp' },
    { icon: 'ant-design:mobile-outlined', title: 'Channels', v: 'channels' },
    { icon: 'ant-design:user-outlined', title: 'Assistant Memory', v: 'profile' },
    { icon: 'ant-design:folder-open-outlined', title: 'File Recall', v: 'files' },
  ]

  function navActive(v: string) { return $view === v }

  function sessionIcon(s: any): string {
    if (s.source === 'cron') return 'ant-design:clock-circle-outlined'
    if (s.source === 'channel') return 'ant-design:send-outlined'
    if (s.status === 'working') return 'ant-design:code-outlined'
    return 'ant-design:message-outlined'
  }

  function toggleSel(id: string) {
    sel.update(s => { const n = { ...s }; n[id] ? delete n[id] : (n[id] = true); return n })
  }

  async function delSelected() {
    const ids = Object.keys($sel)
    try {
      await api.deleteSessions(ids)
      sessions.update(ss => ss.filter(s => !$sel[s.id]))
    } catch (e: any) { showToast(e.message, 'error') }
    sel.set({}); selMode.set(false)
  }

  async function delSession(id: string) {
    try {
      await api.deleteSession(id)
      sessions.update(ss => ss.filter(s => s.id !== id))
      if ($activeSessionId === id) activeSessionId.set(null)
    } catch (e: any) { showToast(e.message, 'error') }
    menuFor.set(null)
  }

  async function commitRename() {
    if (!$editId) return
    const draft = $editDraft.trim()
    if (draft) {
      try {
        await api.updateSession($editId, { name: draft })
        sessions.update(ss => ss.map(s => s.id === $editId ? { ...s, name: draft, title: draft } : s))
      } catch (e: any) { showToast(e.message, 'error') }
    }
    editId.set(null)
  }

  async function newSession() {
    try {
      const sess = await api.createSession({ source: 'manual' }) as any
      sessions.update(ss => [sess.session ?? sess, ...ss])
      activeSessionId.set((sess.session ?? sess).id)
      activeSession.set((sess.session ?? sess).id)
      view.set('chat')
    } catch (e: any) { showToast(e.message, 'error') }
  }

  const selCount = $derived(Object.keys($sel).length)
</script>

<aside style="width:{$sidebar === 'full' ? '256px' : $sidebar === 'rail' ? '64px' : '0px'};flex:0 0 {$sidebar === 'full' ? '256px' : $sidebar === 'rail' ? '64px' : '0px'};background:#FBFBFB;border-right:1px solid #EEEFF1;overflow:hidden;transition:width 0.32s cubic-bezier(0.2,0,0,1),flex-basis 0.32s cubic-bezier(0.2,0,0,1);">

  {#if $sidebar === 'full'}
  <div class="full">
    <div class="new-btn-wrap">
      <button class="new-btn" onclick={newSession}>
        <iconify-icon icon="ant-design:plus-outlined" width="14"></iconify-icon>
        <span>New Session</span>
      </button>
    </div>

    <div class="scroll">
      <!-- Sessions -->
      <div class="nav-group">
        <div class="group-header">
          <span class="group-label">SESSIONS</span>
          <span class="sel-toggle" onclick={() => { selMode.update(v => !v); sel.set({}); menuFor.set(null); editId.set(null) }}>
            {$selMode ? 'Done' : 'Select'}
          </span>
        </div>

        {#if $selMode && Object.keys($sel).length > 0}
        <div class="batch-bar">
          <span class="batch-count">{Object.keys($sel).length} selected</span>
          <button class="batch-del" onclick={delSelected}>
            <iconify-icon icon="ant-design:delete-outlined" width="12"></iconify-icon>
            Delete
          </button>
        </div>
        {/if}

        {#each $sessions as s}
        {@const active = s.id === $activeSession && $view === 'chat'}
        {@const selected = !!$sel[s.id]}
        {@const editing = $editId === s.id}
        {@const menuOpen = $menuFor === s.id && !$selMode}
        {@const solid = active && !$selMode}
        {@const displayName = (s as any).name || (s as any).title || s.id}
        {@const icon = sessionIcon(s)}
        <div
          class="nav-row"
          class:solid={solid}
          class:selected={selected && !solid}
          onclick={() => { if ($selMode) toggleSel(s.id); else { view.set('chat'); activeSession.set(s.id); activeSessionId.set(s.id); menuFor.set(null) } }}
        >
          {#if $selMode}
          <span
            class="checkbox"
            style="border-color:{selected ? '#1677FF' : '#D9D9D9'};background:{selected ? '#1677FF' : '#fff'}"
            onclick={(e) => { e.stopPropagation(); toggleSel(s.id) }}
          >
            {#if selected}<iconify-icon icon="ant-design:check-outlined" width="11" style="color:#fff"></iconify-icon>{/if}
          </span>
          {/if}

          {#if (s as any).status === 'working'}
            <iconify-icon icon="ant-design:loading-outlined" width="14" style="color:{solid ? '#fff' : '#1677FF'};flex:0 0 auto;animation:octo-spin 0.8s linear infinite"></iconify-icon>
          {:else}
            <iconify-icon icon={icon} width="14" style="color:{solid ? '#fff' : 'rgba(0,0,0,0.45)'};flex:0 0 auto"></iconify-icon>
          {/if}

          {#if editing}
          <input
            class="rename-input"
            value={$editDraft}
            oninput={(e) => editDraft.set((e.target as HTMLInputElement).value)}
            onclick={(e) => e.stopPropagation()}
          />
          <span class="row-action" onclick={(e) => { e.stopPropagation(); commitRename() }} style="color:#52C41A">
            <iconify-icon icon="ant-design:check-outlined" width="13"></iconify-icon>
          </span>
          <span class="row-action" onclick={(e) => { e.stopPropagation(); editId.set(null) }} style="color:rgba(0,0,0,0.45)">
            <iconify-icon icon="ant-design:close-outlined" width="13"></iconify-icon>
          </span>
          {:else}
          <span class="session-title" style="color:{solid ? '#fff' : 'rgba(0,0,0,0.88)'};">{displayName}</span>
          {#if !menuOpen}
            <span class="session-time" style="color:{solid ? 'rgba(255,255,255,0.75)' : 'rgba(0,0,0,0.25)'};">
              {(s as any).source === 'cron' ? 'Cron' : ''}
            </span>
          {/if}
          {#if !menuOpen && !$selMode}
            <span class="row-action kebab" onclick={(e) => { e.stopPropagation(); menuFor.update(m => m === s.id ? null : s.id) }} style="color:{solid ? '#fff' : 'rgba(0,0,0,0.45)'}">
              <iconify-icon icon="ant-design:more-outlined" width="14"></iconify-icon>
            </span>
          {/if}
          {#if menuOpen}
            <span class="row-action" onclick={(e) => { e.stopPropagation(); editId.set(s.id); editDraft.set(displayName); menuFor.set(null) }} title="Rename">
              <iconify-icon icon="ant-design:edit-outlined" width="13"></iconify-icon>
            </span>
            <span class="row-action del" onclick={(e) => { e.stopPropagation(); delSession(s.id) }} title="Delete">
              <iconify-icon icon="ant-design:delete-outlined" width="13"></iconify-icon>
            </span>
          {/if}
          {/if}
        </div>
        {/each}
      </div>

      <!-- Config -->
      <div class="nav-group">
        <div class="group-header"><span class="group-label">CONFIG</span></div>
        {#each [
          { icon: 'ant-design:clock-circle-outlined', label: 'Scheduled Tasks', v: 'tasks' },
          { icon: 'ant-design:thunderbolt-outlined', label: 'Skills', v: 'skills' },
          { icon: 'ant-design:api-outlined', label: 'MCP Servers', v: 'mcp', meta: '4' },
          { icon: 'ant-design:mobile-outlined', label: 'Channels', v: 'channels' },
        ] as item}
        <div class="nav-row" class:solid={navActive(item.v)} onclick={() => view.set(item.v as any)}>
          <iconify-icon icon={item.icon} width="14" style="color:{navActive(item.v) ? '#fff' : 'rgba(0,0,0,0.45)'}"></iconify-icon>
          <span style="font-size:13px;color:{navActive(item.v) ? '#fff' : 'rgba(0,0,0,0.65)'};">{item.label}</span>
          {#if item.meta}<span style="margin-left:auto;font-size:11px;color:{navActive(item.v) ? 'rgba(255,255,255,0.75)' : 'rgba(0,0,0,0.45)'};">{item.meta}</span>{/if}
        </div>
        {/each}
      </div>

      <!-- My Data -->
      <div class="nav-group">
        <div class="group-header"><span class="group-label">MY DATA</span></div>
        {#each [
          { icon: 'ant-design:user-outlined', label: 'Assistant Memory', v: 'profile' },
          { icon: 'ant-design:folder-open-outlined', label: 'File Recall', v: 'files' },
        ] as item}
        <div class="nav-row" class:solid={navActive(item.v)} onclick={() => view.set(item.v as any)}>
          <iconify-icon icon={item.icon} width="14" style="color:{navActive(item.v) ? '#fff' : 'rgba(0,0,0,0.45)'}"></iconify-icon>
          <span style="font-size:13px;color:{navActive(item.v) ? '#fff' : 'rgba(0,0,0,0.65)'};">{item.label}</span>
        </div>
        {/each}
      </div>
    </div>

    <div class="footer">
      <div class="footer-settings" style="color:{navActive('settings') ? '#1677FF' : 'rgba(0,0,0,0.65)'}" onclick={() => view.set('settings')}>
        <iconify-icon icon="ant-design:setting-outlined" width="14"></iconify-icon>
        <span>Settings</span>
      </div>
      {#if versionStr}<span class="version">{versionStr}</span>{/if}
    </div>
  </div>
  {/if}

  {#if $sidebar === 'rail'}
  <div class="rail">
    <div style="padding:16px 0 8px 0;">
      <button class="rail-btn primary" title="New Session" onclick={() => view.set('chat')}>
        <iconify-icon icon="ant-design:plus-outlined" width="16"></iconify-icon>
      </button>
    </div>
    <div class="rail-scroll">
      {#each railNav as item}
      <button
        class="rail-btn"
        class:active={navActive(item.v)}
        title={item.title}
        onclick={() => view.set(item.v as any)}
      >
        <iconify-icon icon={item.icon} width="16"></iconify-icon>
      </button>
      {/each}
    </div>
    <div class="rail-footer">
      <button class="rail-btn" class:active={navActive('settings')} title="Settings" onclick={() => view.set('settings')}>
        <iconify-icon icon="ant-design:setting-outlined" width="16"></iconify-icon>
      </button>
    </div>
  </div>
  {/if}
</aside>

<style>
.full { width: 256px; height: 100%; display: flex; flex-direction: column; min-height: 0; }
.new-btn-wrap { padding: 16px 12px 8px; }
.new-btn {
  width: 100%; height: 32px; border: none; border-radius: 6px;
  background: #1677FF; color: #fff; font-size: 14px;
  display: flex; align-items: center; justify-content: center; gap: 8px;
  cursor: pointer; font-family: inherit;
}
.new-btn:hover { background: #4096FF; }
.scroll { flex: 1; overflow-y: auto; padding: 8px 12px; display: flex; flex-direction: column; gap: 20px; }
.nav-group { display: flex; flex-direction: column; gap: 2px; }
.group-header { display: flex; align-items: center; justify-content: space-between; padding: 0 8px 6px; }
.group-label { font-size: 11px; font-weight: 600; letter-spacing: 0.5px; color: rgba(0,0,0,0.25); }
.sel-toggle { font-size: 11px; font-weight: 600; color: #1677FF; cursor: pointer; }
.batch-bar {
  display: flex; align-items: center; gap: 8px;
  margin: 0 4px 6px; padding: 6px 8px 6px 12px;
  background: #FFF1F0; border: 1px solid #FFCCC7; border-radius: 8px;
}
.batch-count { font-size: 12px; color: #CF1322; flex: 1; }
.batch-del {
  height: 26px; padding: 0 10px; border: none; background: #FF4D4F;
  border-radius: 6px; display: flex; align-items: center; gap: 6px;
  font-size: 12px; color: #fff; cursor: pointer; font-family: inherit;
}
.batch-del:hover { background: #FF7875; }
.nav-row {
  display: flex; align-items: center; gap: 10px;
  min-height: 36px; padding: 0 6px 0 10px;
  border-radius: 9999px; cursor: pointer;
}
.nav-row.solid { background: #1677FF; }
.nav-row.selected { background: rgba(22,119,255,0.06); }
/* Hover never overrides the active (solid) row — that washed the blue pill out
   to grey with near-invisible white text. */
.nav-row:hover:not(.solid) { background: rgba(0,0,0,0.04); }
.checkbox {
  width: 16px; height: 16px; flex: 0 0 16px;
  border-radius: 4px; border: 1.5px solid;
  display: flex; align-items: center; justify-content: center;
}
.rename-input {
  flex: 1; min-width: 0; font-size: 13px; font-family: inherit;
  border: 1px solid #1677FF; border-radius: 4px;
  padding: 2px 6px; outline: none; color: rgba(0,0,0,0.88);
}
.session-title {
  flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis;
  white-space: nowrap; font-size: 13px;
}
.session-time { font-size: 11px; flex: 0 0 auto; padding-right: 4px; }
.row-action {
  width: 22px; height: 22px; flex: 0 0 22px; border-radius: 5px;
  display: flex; align-items: center; justify-content: center;
  cursor: pointer; color: rgba(0,0,0,0.45);
  opacity: 0; transition: opacity 0.12s;
}
.nav-row:hover .row-action { opacity: 1; }
.kebab { opacity: 0; }
.nav-row:hover .kebab { opacity: 1; }
.row-action:hover { background: rgba(0,0,0,0.06); }
.del:hover { color: #FF4D4F !important; }
.footer {
  flex: 0 0 auto; border-top: 1px solid #EEEFF1;
  padding: 10px 12px; display: flex; align-items: center; justify-content: space-between;
}
.footer-settings {
  display: flex; align-items: center; gap: 8px;
  cursor: pointer; padding: 4px 8px; border-radius: 9999px;
}
.footer-settings:hover { background: rgba(0,0,0,0.04); }
.footer-settings span { font-size: 13px; }
.version {
  font-size: 11px; font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  color: rgba(0,0,0,0.45); background: #fff; border: 1px solid #EEEFF1;
  border-radius: 9999px; padding: 2px 8px;
}
/* Rail */
.rail {
  width: 64px; height: 100%; display: flex; flex-direction: column;
  align-items: center; min-height: 0;
}
.rail-scroll { flex: 1; overflow-y: auto; padding: 4px 0; display: flex; flex-direction: column; gap: 4px; align-items: center; }
.rail-footer { flex: 0 0 auto; border-top: 1px solid #EEEFF1; padding: 8px 0; width: 100%; display: flex; justify-content: center; }
.rail-btn {
  width: 40px; height: 40px; border: none; border-radius: 9999px;
  background: transparent; color: rgba(0,0,0,0.45);
  display: flex; align-items: center; justify-content: center; cursor: pointer;
}
.rail-btn:hover { background: rgba(0,0,0,0.04); }
.rail-btn.active { background: #1677FF; color: #fff; }
.rail-btn.primary { background: #1677FF; color: #fff; }
.rail-btn.primary:hover { background: #4096FF; }
</style>
