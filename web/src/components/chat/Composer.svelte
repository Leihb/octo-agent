<script lang="ts">
  import { get } from 'svelte/store'
  import {
    running, activeSessionId, chatStreaming, sessions,
    chatContextUsage, chatWorkingDir, chatPermMode, chatReasoningEffort,
  } from '../../lib/stores'
  import { ws } from '../../lib/ws'
  import { t as tr } from '../../lib/i18n'
  import StatusTag from '../ui/StatusTag.svelte'

  let { onSend }: { onSend?: (text: string) => void } = $props()

  let text = $state('')

  // $store autosubscription is reactive inside $derived (get() is not).
  let sid = $derived($activeSessionId ?? '')
  let isStreaming = $derived($chatStreaming[sid] ?? false)
  let currentSession = $derived($sessions.find((s: any) => s.id === sid) ?? null)

  // Session meta chips — pull live values from per-session stores, fall back
  // to the session record, then to sensible defaults.
  let modelName = $derived(currentSession?.model || currentSession?.model_id || '—')
  let reasoning = $derived($chatReasoningEffort[sid] || currentSession?.reasoning_effort || 'medium')
  let workingDir = $derived($chatWorkingDir[sid] || currentSession?.working_dir || '')
  let permMode = $derived($chatPermMode[sid] || currentSession?.permission_mode || 'ask')
  let ctxUsage = $derived(Number($chatContextUsage[sid] ?? currentSession?.context_usage ?? 0))

  function cap(s: string): string {
    return s ? s[0].toUpperCase() + s.slice(1) : s
  }

  // Show only the last two path segments so a long working dir doesn't push
  // the chip row onto a second line. Full path is in the title tooltip.
  function shortDir(p: string): string {
    if (!p) return ''
    const parts = p.split('/').filter(Boolean)
    return parts.length <= 2 ? p : '…/' + parts.slice(-2).join('/')
  }

  function send() {
    if (!text.trim()) return
    const v = text.trim()
    text = ''
    if (onSend) {
      onSend(v)
    } else {
      running.set(true)
    }
  }

  function stop() {
    const s = get(activeSessionId)
    if (s) ws.interrupt(s)
    running.set(false)
  }

  function onKeydown(e: KeyboardEvent) {
    if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); send() }
  }
</script>

<div class="composer">
  <div class="chips">
    <button class="chip">
      <iconify-icon icon="ant-design:robot-outlined" width="12"></iconify-icon>
      <span>{modelName}</span>
      <iconify-icon icon="lucide:chevron-down" width="12"></iconify-icon>
    </button>
    <button class="chip">
      <span>Reasoning: {cap(reasoning)}</span>
      <iconify-icon icon="lucide:chevron-down" width="12"></iconify-icon>
    </button>
    {#if workingDir}
      <span class="chip static" title={workingDir}><span class="mono">{shortDir(workingDir)}</span></span>
    {/if}
    <span class="chip static context-chip">
      <span>{tr('chat.context')}</span>
      <span class="ctx-bar"><span class="ctx-fill" style="width:{Math.min(ctxUsage, 100)}%"></span></span>
      <span class="mono">{ctxUsage}%</span>
    </span>
    <span style="margin-left:auto;"></span>
    {#if permMode === 'auto'}
      <StatusTag status="success">{tr('chat.auto_mode')}</StatusTag>
    {:else}
      <StatusTag status="warning">{tr('chat.ask_mode')}</StatusTag>
    {/if}
  </div>

  <div class="input-wrap">
    <div class="input-card">
      <textarea
        rows={2}
        placeholder={tr('chat.placeholder')}
        bind:value={text}
        onkeydown={onKeydown}
      ></textarea>
      <div class="input-footer">
        <button class="tool-btn" title="Attach file">
          <iconify-icon icon="ant-design:paper-clip-outlined" width="15"></iconify-icon>
        </button>
        <button class="tool-btn skill-btn" title="Insert skill">/</button>
        <span style="margin-left:auto;"></span>
        {#if isStreaming || $running}
          <button class="stop-btn" onclick={stop}>
            <span class="stop-sq"></span>
            Stop
          </button>
        {:else}
          <button class="send-btn" onclick={send}>Send</button>
        {/if}
      </div>
    </div>
  </div>
</div>

<style>
.composer {
  flex: 0 0 auto;
  background: rgba(255,255,255,0.96);
  backdrop-filter: blur(8px);
  border-top: 1px solid #EEEFF1;
}
.chips {
  max-width: 800px; margin: 0 auto;
  padding: 12px 24px 0;
  display: flex; align-items: center; gap: 8px; flex-wrap: wrap;
}
.chip {
  height: 24px; padding: 0 10px; border: 1px solid #D9D9D9; background: #fff;
  border-radius: 9999px; display: flex; align-items: center; gap: 6px;
  font-size: 12px; color: rgba(0,0,0,0.65); cursor: pointer; font-family: inherit;
}
.chip:hover { border-color: #4096FF; color: #4096FF; }
.chip.static { cursor: default; background: #FAFAFA; border-color: #EEEFF1; }
.chip.static:hover { border-color: #EEEFF1; color: rgba(0,0,0,0.65); }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.context-chip { gap: 8px; }
.ctx-bar { width: 56px; height: 4px; background: #F0F0F0; border-radius: 9999px; overflow: hidden; display: inline-block; }
.ctx-fill { display: block; height: 100%; background: #1677FF; border-radius: 9999px; }
.input-wrap { max-width: 800px; margin: 10px auto 0; padding: 0 24px 16px; }
.input-card {
  background: #fff; border: 1px solid #D9D9D9; border-radius: 12px;
  padding: 10px 12px; display: flex; flex-direction: column; gap: 8px;
}
.input-card:focus-within {
  border-color: #1677FF;
  box-shadow: 0 0 0 2px rgba(5,145,255,0.1);
}
textarea {
  border: none; outline: none; resize: none; font-size: 14px; line-height: 1.6;
  font-family: inherit; color: rgba(0,0,0,0.88); background: transparent; width: 100%;
}
.input-footer { display: flex; align-items: center; gap: 4px; }
.tool-btn {
  width: 28px; height: 28px; border: none; background: transparent; border-radius: 6px;
  display: flex; align-items: center; justify-content: center;
  cursor: pointer; color: rgba(0,0,0,0.45);
}
.tool-btn:hover { background: rgba(0,0,0,0.04); color: rgba(0,0,0,0.65); }
.skill-btn { font-size: 14px; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.send-btn {
  height: 32px; padding: 0 16px; border: none; background: #1677FF;
  border-radius: 6px; font-size: 14px; color: #fff; cursor: pointer; font-family: inherit;
}
.send-btn:hover { background: #4096FF; }
.stop-btn {
  height: 32px; padding: 0 14px; border: 1px solid #FFCCC7; background: #FFF1F0;
  border-radius: 6px; display: flex; align-items: center; gap: 7px;
  font-size: 14px; color: #FF4D4F; cursor: pointer; font-family: inherit;
}
.stop-btn:hover { border-color: #FF4D4F; }
.stop-sq { width: 9px; height: 9px; border-radius: 2px; background: #FF4D4F; }
</style>
