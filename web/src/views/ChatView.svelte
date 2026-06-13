<script lang="ts">
  import { get } from 'svelte/store'
  import {
    activeSessionId,
    sessions,
    chatMessages,
    chatStreaming,
    chatProgress,
    chatBgTasks,
    chatTodos,
    chatContextUsage,
    chatWorkingDir,
    chatPermMode,
    chatReasoningEffort,
    chatSuggestion,
    confirmModal,
    questionModal,
    feedbackModal,
    artifactsOpen,
    artifacts,
    addChatMsg,
    clearMsgs,
    appendToLastAssistant,
    addToolCallToGroup,
    updateToolResult,
    setToolError,
    finishAllTools,
    uid,
  } from '../lib/stores'
  import { ws, wsState } from '../lib/ws'
  import * as api from '../lib/api'
  import { renderMarkdown, setupCopyButtons } from '../lib/markdown'
  import { t } from '../lib/i18n'
  import StatusTag from '../components/ui/StatusTag.svelte'
  import ToolGroup from '../components/chat/ToolGroup.svelte'
  import BackgroundProcesses from '../components/chat/BackgroundProcesses.svelte'
  import Composer from '../components/chat/Composer.svelte'
  import ArtifactsPanel from '../components/ArtifactsPanel.svelte'

  // ── reactive state ─────────────────────────────────────────────────────────
  let messagesEl = $state<HTMLElement | null>(null)

  // In Svelte 5 runes mode, $store is reactive inside $derived / $effect.
  // get(store) is a one-time read — do NOT use inside $derived/$effect.
  let id          = $derived($activeSessionId)
  let msgs        = $derived($chatMessages[$activeSessionId ?? ''] ?? [])
  let streaming   = $derived($chatStreaming[$activeSessionId ?? ''] ?? false)
  let progress    = $derived($chatProgress[$activeSessionId ?? ''] ?? null)
  let bgTasks     = $derived($chatBgTasks[$activeSessionId ?? ''] ?? [])
  let todos       = $derived($chatTodos[$activeSessionId ?? ''] ?? [])
  let suggestion  = $derived($chatSuggestion[$activeSessionId ?? ''] ?? '')
  let currentSession = $derived($sessions.find((s: any) => s.id === $activeSessionId) ?? null)
  let artifactCount  = $derived($artifacts.length)
  let wsDisconnected = $derived($wsState === 'disconnected')

  // ── history handler ────────────────────────────────────────────────────────
  function handleHistoryEvent(ev: Record<string, any>) {
    const sid = get(activeSessionId)   // get() is fine in imperative functions
    if (!sid) return
    if (ev.type === 'history_user_message') {
      addChatMsg(sid, {
        id: uid('u'),
        type: 'user',
        content: ev.content ?? '',
        createdAt: ev.created_at ?? Date.now(),
        streaming: false,
        pending: false,
        tools: [],
        todos: [],
      })
    } else if (ev.type === 'assistant_message') {
      // Skip empty assistant turns (thinking-only / tool-only rounds) so they
      // don't render as blank bubbles.
      if (!(ev.content ?? '').trim()) return
      addChatMsg(sid, {
        id: uid('a'),
        type: 'assistant',
        content: ev.content ?? '',
        createdAt: Date.now(),
        streaming: false,
        tools: [],
        todos: [],
      })
    } else if (ev.type === 'tool_call') {
      addToolCallToGroup(sid, {
        id: uid('t'),
        toolId: ev.tool_id ?? '',
        name: ev.name ?? '',
        args: ev.args ?? '',
        summary: ev.summary ?? '',
        done: false,
        error: null,
        result: null,
        stdout: [],
        diff: null,
      })
    } else if (ev.type === 'tool_result') {
      updateToolResult(sid, ev.tool_id, ev.result, ev.ui_payload)
    }
  }

  // ── main lifecycle effect ──────────────────────────────────────────────────
  // $activeSessionId makes this effect re-run whenever the session changes.
  $effect(() => {
    const sid = $activeSessionId
    if (!sid) return

    clearMsgs(sid)
    ws.subscribe(sid)

    // Load history
    api.getSessionMessages(sid, { limit: 30 }).then((resp: any) => {
      const events: any[] = resp?.events ?? []
      for (const ev of events) {
        handleHistoryEvent(ev)
      }
      // History is a completed transcript — mark all tools done so nothing
      // shows a perpetual "running" spinner (there's no complete event in the
      // replayed history to close them).
      finishAllTools(sid)
    }).catch(() => {/* silently ignore history load errors */})

    // ── WS event handlers ───────────────────────────────────────────────────
    const cleanups: Array<() => void> = []

    cleanups.push(ws.on('output', (ev) => {
      if ((ev as any).session_id && (ev as any).session_id !== sid) return
      appendToLastAssistant(sid, (ev as any).content ?? '')
    }))

    cleanups.push(ws.on('assistant_message', (ev) => {
      if ((ev as any).session_id && (ev as any).session_id !== sid) return
      const curMsgs = get(chatMessages)[sid] ?? []
      if (streaming && curMsgs.length > 0 && curMsgs[curMsgs.length - 1]?.type === 'assistant') {
        // finalize streaming message
        chatMessages.update(m => {
          const arr = [...(m[sid] || [])]
          const last = arr.length - 1
          if (last >= 0) arr[last] = { ...arr[last], content: (ev as any).content ?? arr[last].content, streaming: false }
          return { ...m, [sid]: arr }
        })
      } else {
        addChatMsg(sid, {
          id: uid('a'),
          type: 'assistant',
          content: (ev as any).content ?? '',
          createdAt: Date.now(),
          streaming: false,
          tools: [],
          todos: [],
        })
      }
    }))

    cleanups.push(ws.on('history_user_message', (ev) => {
      if ((ev as any).session_id && (ev as any).session_id !== sid) return
      const content = (ev as any).content ?? ''
      const createdAt = (ev as any).created_at ?? Date.now()
      chatMessages.update(m => {
        const msgs = [...(m[sid] || [])]
        // If the last user bubble is a pending optimistic echo of the same
        // text, replace it in place (de-dup). Otherwise append a fresh one.
        const lastPending = msgs.findLastIndex((x: any) => x.type === 'user' && x.pending)
        if (lastPending >= 0 && msgs[lastPending].content === content) {
          msgs[lastPending] = { ...msgs[lastPending], id: uid('u'), createdAt, pending: false }
        } else {
          msgs.push({ id: uid('u'), type: 'user', content, createdAt, streaming: false, pending: false, tools: [], todos: [] })
        }
        return { ...m, [sid]: msgs }
      })
    }))

    cleanups.push(ws.on('tool_call', (ev) => {
      if ((ev as any).session_id && (ev as any).session_id !== sid) return
      addToolCallToGroup(sid, {
        id: uid('t'),
        toolId: (ev as any).tool_id ?? '',
        name: (ev as any).name ?? '',
        args: (ev as any).args ?? '',
        summary: (ev as any).summary ?? '',
        done: false,
        error: null,
        result: null,
        stdout: [],
        diff: null,
      })
    }))

    cleanups.push(ws.on('tool_result', (ev) => {
      if ((ev as any).session_id && (ev as any).session_id !== sid) return
      updateToolResult(sid, (ev as any).tool_id, (ev as any).result, (ev as any).ui_payload)
    }))

    cleanups.push(ws.on('tool_error', (ev) => {
      if ((ev as any).session_id && (ev as any).session_id !== sid) return
      setToolError(sid, (ev as any).tool_id, (ev as any).error ?? 'error')
    }))

    cleanups.push(ws.on('tool_stdout', (ev) => {
      if ((ev as any).session_id && (ev as any).session_id !== sid) return
      chatMessages.update(m => {
        const msgs = [...(m[sid] || [])]
        const lastGroup = msgs.findLastIndex((x: any) => x.type === 'tool_group')
        if (lastGroup >= 0) {
          const tools = [...msgs[lastGroup].tools]
          const lastTool = tools.length - 1
          if (lastTool >= 0) {
            tools[lastTool] = { ...tools[lastTool], stdout: [...(tools[lastTool].stdout ?? []), ...((ev as any).lines ?? [])] }
          }
          msgs[lastGroup] = { ...msgs[lastGroup], tools }
        }
        return { ...m, [sid]: msgs }
      })
    }))

    cleanups.push(ws.on('progress', (ev) => {
      if ((ev as any).session_id && (ev as any).session_id !== sid) return
      chatProgress.update(p => ({
        ...p,
        [sid]: { message: (ev as any).message || 'Thinking', phase: (ev as any).phase },
      }))
    }))

    cleanups.push(ws.on('complete', (ev) => {
      if ((ev as any).session_id && (ev as any).session_id !== sid) return
      chatStreaming.update(s => ({ ...s, [sid]: false }))
      chatProgress.update(p => ({ ...p, [sid]: null }))
      // Close open tool groups AND mark any still-spinning tools done — a
      // finished turn must never leave a tool on "running" (e.g. parallel
      // results that never matched a tool, or a dropped result event).
      finishAllTools(sid)
    }))

    cleanups.push(ws.on('session_update', (ev) => {
      if ((ev as any).session_id && (ev as any).session_id !== sid) return
      chatContextUsage.update(u => ({ ...u, [sid]: (ev as any).context_usage }))
      chatPermMode.update(mm => ({ ...mm, [sid]: (ev as any).permission_mode }))
      chatReasoningEffort.update(r => ({ ...r, [sid]: (ev as any).reasoning_effort }))
      chatWorkingDir.update(w => ({ ...w, [sid]: (ev as any).working_dir }))
    }))

    cleanups.push(ws.on('todo_update', (ev) => {
      if ((ev as any).session_id && (ev as any).session_id !== sid) return
      chatTodos.update(t => ({ ...t, [sid]: (ev as any).todos ?? [] }))
    }))

    cleanups.push(ws.on('background_task_update', (ev) => {
      if ((ev as any).session_id && (ev as any).session_id !== sid) return
      chatBgTasks.update(b => ({ ...b, [sid]: (ev as any).tasks ?? [] }))
    }))

    cleanups.push(ws.on('request_confirmation', (ev) => {
      if ((ev as any).session_id && (ev as any).session_id !== sid) return
      confirmModal.set({
        confId: (ev as any).id,
        sessionId: (ev as any).session_id,
        message: (ev as any).message,
        kind: (ev as any).kind,
      })
    }))

    cleanups.push(ws.on('request_user_question', (ev) => {
      if ((ev as any).session_id && (ev as any).session_id !== sid) return
      questionModal.set({
        questionId: (ev as any).question_id,
        sessionId: (ev as any).session_id,
        question: (ev as any).question,
        options: (ev as any).options,
        multiSelect: (ev as any).multi_select,
        header: (ev as any).header,
      })
    }))

    cleanups.push(ws.on('request_feedback', (ev) => {
      if ((ev as any).session_id && (ev as any).session_id !== sid) return
      feedbackModal.set({
        sessionId: (ev as any).session_id,
        question: (ev as any).question,
        context: (ev as any).context,
        options: (ev as any).options,
      })
    }))

    cleanups.push(ws.on('next_message_suggestion', (ev) => {
      if ((ev as any).session_id && (ev as any).session_id !== sid) return
      chatSuggestion.update(s => ({ ...s, [sid]: (ev as any).text ?? '' }))
    }))

    return () => {
      ws.unsubscribe(sid)
      for (const cleanup of cleanups) cleanup()
    }
  })

  // ── auto-scroll ────────────────────────────────────────────────────────────
  // Streaming appends text to the SAME message, so msgs.length doesn't change
  // and a length-only effect never re-fires. A ResizeObserver on the inner
  // content keeps us pinned to the bottom while the user is already there, and
  // backs off the moment they scroll up to read.
  let innerEl = $state<HTMLElement | null>(null)
  let stick = true

  $effect(() => {
    const scroller = messagesEl
    const content = innerEl
    if (!scroller || !content) return

    const onScroll = () => {
      stick = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < 80
    }
    scroller.addEventListener('scroll', onScroll, { passive: true })

    const ro = new ResizeObserver(() => {
      if (stick) scroller.scrollTop = scroller.scrollHeight
    })
    ro.observe(content)

    // Initial pin to bottom after history loads.
    scroller.scrollTop = scroller.scrollHeight

    return () => {
      scroller.removeEventListener('scroll', onScroll)
      ro.disconnect()
    }
  })

  // When the active session changes, re-pin to the bottom.
  $effect(() => {
    void $activeSessionId
    stick = true
  })

  // ── markdown copy buttons setup ────────────────────────────────────────────
  function setupAssistantEl(el: HTMLElement) {
    setupCopyButtons(el)
  }

  // ── send message ───────────────────────────────────────────────────────────
  function send(text: string) {
    const sid = get(activeSessionId)
    if (!sid || !text.trim()) return
    // Optimistically show the user bubble, marked pending. The server echoes
    // it back as a history_user_message — that handler replaces this pending
    // bubble (matching by content) instead of appending a duplicate.
    addChatMsg(sid, {
      id: 'pending-' + Date.now(),
      type: 'user',
      content: text,
      createdAt: Date.now(),
      streaming: false,
      pending: true,
      tools: [],
      todos: [],
    })
    chatStreaming.update(s => ({ ...s, [sid]: true }))
    ws.sendMessage(sid, text)
  }

  // ── plan progress helpers ──────────────────────────────────────────────────
  function planDoneCount(todos: any[]): number {
    return todos.filter((t: any) => t.status === 'completed').length
  }
  function planFill(todos: any[]): string {
    if (!todos.length) return '0%'
    return `${Math.round((planDoneCount(todos) / todos.length) * 100)}%`
  }
</script>

<div class="chat-view">
  <!-- Chat header -->
  <div class="chat-header">
    <div class="title-row">
      <span class="session-title">
        {currentSession?.title ?? currentSession?.name ?? 'Chat'}
      </span>
      {#if streaming}
        <StatusTag status="info">{t('status.running')}</StatusTag>
      {:else}
        <StatusTag status="default">{t('status.idle')}</StatusTag>
      {/if}
    </div>
    <div class="header-actions">
      <button class="hdr-btn" class:active={$artifactsOpen} onclick={() => artifactsOpen.update(v => !v)}>
        <iconify-icon icon="ant-design:file-text-outlined" width="13"></iconify-icon>
        {t('chat.artifacts')}
        {#if artifactCount > 0}
          <span class="count-badge">{artifactCount}</span>
        {/if}
      </button>
      <button class="hdr-btn">
        <iconify-icon icon="ant-design:export-outlined" width="13"></iconify-icon>
        {t('chat.export')}
      </button>
    </div>
  </div>

  <!-- WS disconnect banner -->
  {#if wsDisconnected}
    <div class="ws-banner">
      <iconify-icon icon="ant-design:loading-outlined" width="15" style="color:#FA8C16;animation:octo-spin 0.8s linear infinite"></iconify-icon>
      <span class="ws-msg">Connection lost — reconnecting…</span>
      <span style="margin-left:auto"></span>
      <button class="ws-retry" onclick={() => ws.connect()}>Retry now</button>
    </div>
  {/if}

  <!-- Body row: messages + artifacts -->
  <div class="body-row">
    <div class="conversation">
      <!-- Messages scroll area -->
      <div class="messages" bind:this={messagesEl}>
        <div class="messages-inner" bind:this={innerEl}>

          {#each msgs as msg (msg.id)}
            {#if msg.type === 'user'}
              <!-- Right-aligned user bubble -->
              <div class="msg-user fadein">
                <div class="user-bubble-wrap">
                  <div class="user-bubble">{msg.content}</div>
                  <div class="msg-actions">
                    <button class="action-btn" title="Copy" onclick={() => navigator.clipboard.writeText(msg.content)}>
                      <iconify-icon icon="ant-design:copy-outlined" width="13"></iconify-icon>
                    </button>
                  </div>
                </div>
              </div>

            {:else if msg.type === 'assistant'}
              <!-- Assistant message with avatar -->
              <div class="msg-agent fadein">
                <div class="agent-avatar">O</div>
                <div class="agent-content">
                  <!-- Plan card (todos attached to this message) -->
                  {#if msg.todos && msg.todos.length > 0}
                    <details open class="plan-card">
                      <summary class="plan-summary">
                        <iconify-icon icon="ant-design:ordered-list-outlined" width="14" style="color:#1677FF"></iconify-icon>
                        <span class="plan-title">{t('agent.plan')}</span>
                        <span class="plan-meta">{planDoneCount(msg.todos)} / {msg.todos.length} done</span>
                        <span class="plan-progress"><span class="plan-fill" style="width:{planFill(msg.todos)}"></span></span>
                        <span style="margin-left:auto"></span>
                        <iconify-icon icon="lucide:chevron-down" width="14" style="color:rgba(0,0,0,0.35)"></iconify-icon>
                      </summary>
                      <div class="plan-steps">
                        {#each msg.todos as step}
                          <div class="step" class:active={step.status === 'in_progress'}>
                            {#if step.status === 'completed'}
                              <iconify-icon icon="ant-design:check-circle-outlined" width="14" style="color:#52C41A"></iconify-icon>
                              <span class="done">{step.content}</span>
                            {:else if step.status === 'in_progress'}
                              <iconify-icon icon="ant-design:loading-outlined" width="14" style="color:#1677FF;animation:octo-spin 0.8s linear infinite"></iconify-icon>
                              <span>{step.content}</span>
                            {:else}
                              <iconify-icon icon="lucide:circle" width="14" style="color:rgba(0,0,0,0.25)"></iconify-icon>
                              <span class="pending">{step.content}</span>
                            {/if}
                          </div>
                        {/each}
                      </div>
                    </details>
                  {/if}

                  <!-- Rendered markdown content -->
                  {#if msg.content}
                    <div
                      class="rich-answer"
                      use:setupAssistantEl
                    >
                      {@html renderMarkdown(msg.content)}
                    </div>
                  {/if}

                  <!-- Streaming caret -->
                  {#if msg.streaming}
                    <span class="caret"></span>
                  {/if}

                  <!-- Message actions -->
                  {#if !msg.streaming}
                    <div class="msg-actions reply-actions">
                      <button class="action-btn" title={t('chat.copy')} onclick={() => navigator.clipboard.writeText(msg.content)}>
                        <iconify-icon icon="ant-design:copy-outlined" width="14"></iconify-icon>
                      </button>
                      <button class="action-btn" title={t('chat.retry')} onclick={() => ws.retry($activeSessionId ?? '')}>
                        <iconify-icon icon="ant-design:reload-outlined" width="14"></iconify-icon>
                      </button>
                    </div>
                  {/if}
                </div>
              </div>

            {:else if msg.type === 'tool_group'}
              <!-- Tool group card -->
              <div class="msg-agent fadein">
                <div class="agent-avatar">O</div>
                <div class="agent-content">
                  <ToolGroup tools={msg.tools} streaming={msg.streaming} />
                </div>
              </div>

            {:else if msg.type === 'progress'}
              <!-- Inline progress message -->
              <div class="msg-agent fadein">
                <div class="agent-avatar">O</div>
                <div class="thinking-indicator">
                  <iconify-icon icon="ant-design:loading-outlined" width="15" style="color:#1677FF;animation:octo-spin 0.8s linear infinite"></iconify-icon>
                  <span>{msg.content || t('chat.thinking')}</span>
                </div>
              </div>
            {/if}
          {/each}

          <!-- Live thinking indicator while streaming -->
          {#if streaming && progress}
            <div class="msg-agent fadein">
              <div class="agent-avatar">O</div>
              <div class="thinking-indicator">
                <iconify-icon icon="ant-design:loading-outlined" width="15" style="color:#1677FF;animation:octo-spin 0.8s linear infinite"></iconify-icon>
                <span>{progress.message || t('chat.thinking')}</span>
                <span class="dots">
                  <span></span>
                  <span style="animation-delay:0.2s"></span>
                  <span style="animation-delay:0.4s"></span>
                </span>
                {#if progress.phase}
                  <span class="think-meta mono">{progress.phase}</span>
                {/if}
              </div>
            </div>
          {/if}

          <!-- Suggestion chip -->
          {#if suggestion && !streaming}
            <div class="suggestion-row">
              <button class="suggestion-chip" onclick={() => send(suggestion)}>
                <iconify-icon icon="ant-design:bulb-outlined" width="13"></iconify-icon>
                {suggestion}
              </button>
            </div>
          {/if}
        </div>
      </div>

      <!-- Background processes tray -->
      {#if bgTasks && bgTasks.length > 0}
        <BackgroundProcesses tasks={bgTasks} />
      {/if}

      <!-- Composer -->
      <Composer onSend={send} />
    </div>

    <!-- Artifacts panel -->
    {#if $artifactsOpen}
      <ArtifactsPanel />
    {/if}
  </div>
</div>

<style>
/* ── Layout ──────────────────────────────────────────────────────────────── */
.chat-view { flex: 1; display: flex; flex-direction: column; min-height: 0; }

/* ── Header ──────────────────────────────────────────────────────────────── */
.chat-header {
  flex: 0 0 auto; background: #fff; border-bottom: 1px solid #EEEFF1;
  padding: 12px 24px; display: flex; align-items: center; justify-content: space-between;
}
.title-row { display: flex; align-items: center; gap: 10px; }
.session-title { font-size: 16px; font-weight: 600; color: #1F1F1F; }
.header-actions { display: flex; align-items: center; gap: 8px; }
.hdr-btn {
  height: 28px; padding: 0 12px; border: 1px solid #D9D9D9; background: #fff;
  border-radius: 6px; display: flex; align-items: center; gap: 8px;
  font-size: 13px; color: rgba(0,0,0,0.65); cursor: pointer; font-family: inherit;
}
.hdr-btn:hover { border-color: #4096FF; color: #4096FF; }
.hdr-btn.active { border-color: #1677FF; color: #1677FF; background: rgba(22,119,255,0.06); }
.count-badge {
  font-size: 11px; font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  background: #E6F4FF; color: #1677FF; border-radius: 9999px;
  min-width: 16px; height: 16px; padding: 0 5px;
  display: flex; align-items: center; justify-content: center;
}

/* ── WS banner ───────────────────────────────────────────────────────────── */
.ws-banner {
  flex: 0 0 auto; display: flex; align-items: center; gap: 10px;
  padding: 10px 24px; background: #FFF7E6; border-bottom: 1px solid #FFD591;
}
.ws-msg { font-size: 13px; color: #874D00; }
.ws-retry {
  height: 28px; padding: 0 12px; border: 1px solid #FFD591; background: #fff;
  border-radius: 6px; font-size: 12px; color: #874D00; cursor: pointer; font-family: inherit;
}
.ws-retry:hover { border-color: #FA8C16; }

/* ── Body row ────────────────────────────────────────────────────────────── */
.body-row { flex: 1; display: flex; min-height: 0; }
.conversation { flex: 1; display: flex; flex-direction: column; min-width: 0; min-height: 0; }
.messages { flex: 1; overflow-y: auto; min-height: 0; }
.messages-inner {
  max-width: 800px; margin: 0 auto;
  padding: 24px 24px 16px; display: flex; flex-direction: column; gap: 20px;
}

/* ── User message ────────────────────────────────────────────────────────── */
.msg-user { display: flex; justify-content: flex-end; }
.user-bubble-wrap { display: flex; flex-direction: column; align-items: flex-end; gap: 4px; max-width: 80%; }
.user-bubble {
  background: #E6F4FF; border: 1px solid #BAE0FF;
  border-radius: 12px 12px 4px 12px; padding: 10px 14px;
  font-size: 14px; line-height: 1.6; color: rgba(0,0,0,0.88);
  white-space: pre-wrap; word-break: break-word;
}

/* ── Agent message ───────────────────────────────────────────────────────── */
.msg-agent { display: flex; gap: 12px; }
.agent-avatar {
  width: 28px; height: 28px; flex: 0 0 28px; border-radius: 8px;
  background: #1677FF; color: #fff;
  display: flex; align-items: center; justify-content: center;
  font-size: 13px; font-weight: 600;
}
.agent-content { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 12px; }

/* ── Plan card ───────────────────────────────────────────────────────────── */
.plan-card { border: 1px solid #BAE0FF; border-radius: 10px; background: #F0F7FF; overflow: hidden; }
.plan-summary {
  list-style: none; display: flex; align-items: center; gap: 10px;
  padding: 10px 12px; cursor: pointer; user-select: none;
}
.plan-title { font-size: 13px; font-weight: 600; color: #1F1F1F; }
.plan-meta { font-size: 12px; color: rgba(0,0,0,0.45); }
.plan-progress {
  flex: 1; min-width: 40px; max-width: 160px; height: 4px;
  background: #D6E8FF; border-radius: 9999px; overflow: hidden;
}
.plan-fill { display: block; height: 100%; background: #1677FF; }
.plan-steps {
  border-top: 1px solid #D6E8FF; background: #fff;
  padding: 10px 14px; display: flex; flex-direction: column; gap: 8px;
}
.step { display: flex; align-items: center; gap: 8px; font-size: 13px; }
.step .done { color: rgba(0,0,0,0.35); text-decoration: line-through; }
.step .pending { color: rgba(0,0,0,0.45); }
.step.active { margin: 0 -6px; padding: 4px 6px; background: rgba(22,119,255,0.06); border-radius: 6px; }

/* ── Rich answer (markdown) ──────────────────────────────────────────────── */
.rich-answer { font-size: 14px; line-height: 1.6; color: rgba(0,0,0,0.88); display: flex; flex-direction: column; gap: 12px; }
:global(.rich-answer p) { margin: 0; }
:global(.rich-answer code) {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 13px;
  background: #FAFAFA; border: 1px solid #F0F0F0; border-radius: 4px; padding: 1px 5px;
}
:global(.rich-answer .code-block) { border: 1px solid #F0F0F0; border-radius: 8px; overflow: hidden; background: #FBFBFB; }
:global(.rich-answer .code-header) {
  display: flex; align-items: center; gap: 8px; padding: 6px 8px 6px 12px;
  background: #FAFAFA; border-bottom: 1px solid #F0F0F0;
}
:global(.rich-answer .code-lang) { font-size: 11px; color: rgba(0,0,0,0.45); font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
:global(.rich-answer .copy-btn) {
  margin-left: auto; height: 24px; padding: 0 8px; border: none; background: transparent;
  border-radius: 5px; display: flex; align-items: center; gap: 5px;
  font-size: 11px; color: rgba(0,0,0,0.45); cursor: pointer;
}
:global(.rich-answer .copy-btn:hover) { background: rgba(0,0,0,0.05); color: #1677FF; }
:global(.rich-answer pre) {
  margin: 0; padding: 12px 14px; overflow-x: auto; font-size: 12.5px; line-height: 1.75;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace; color: rgba(0,0,0,0.85);
}
:global(.rich-answer .md-bq) {
  margin: 0; padding: 8px 14px; border-left: 3px solid #BAE0FF;
  background: #F0F7FF; border-radius: 0 6px 6px 0;
  font-size: 13px; line-height: 1.6; color: rgba(0,0,0,0.65);
}
:global(.rich-answer .think-block) { border-radius: 8px; }
:global(.rich-answer .think-summary) {
  list-style: none; display: inline-flex; align-items: center; gap: 6px;
  cursor: pointer; user-select: none; font-size: 13px; color: rgba(0,0,0,0.45);
}
:global(.rich-answer .think-summary:hover) { color: rgba(0,0,0,0.65); }
:global(.rich-answer .think-body) {
  margin-top: 8px; padding-left: 12px; border-left: 2px solid #EEEFF1;
  font-size: 13px; line-height: 1.7; color: rgba(0,0,0,0.45); font-style: italic;
}

/* ── Message actions ─────────────────────────────────────────────────────── */
.msg-actions { display: flex; align-items: center; gap: 2px; }
.reply-actions { margin-top: -4px; }
.action-btn {
  width: 26px; height: 26px; border: none; background: transparent;
  border-radius: 6px; display: flex; align-items: center; justify-content: center;
  cursor: pointer; color: rgba(0,0,0,0.4); opacity: 0; transition: opacity 0.12s;
}
.user-bubble-wrap:hover .action-btn,
.agent-content:hover .reply-actions .action-btn { opacity: 1; }
.action-btn:hover { background: rgba(0,0,0,0.06); color: #1677FF; }

/* ── Streaming caret ─────────────────────────────────────────────────────── */
.caret {
  display: inline-block; width: 7px; height: 15px;
  background: #1677FF; vertical-align: -2px; margin-left: 1px;
  animation: octo-blink 1s step-end infinite;
}

/* ── Thinking indicator ──────────────────────────────────────────────────── */
.thinking-indicator {
  display: flex; align-items: center; gap: 10px; min-height: 28px;
  font-size: 14px; color: rgba(0,0,0,0.65);
}
.dots { display: inline-flex; gap: 3px; align-items: center; }
.dots span {
  width: 4px; height: 4px; border-radius: 9999px;
  background: rgba(0,0,0,0.4); animation: octo-dot 1.2s infinite;
}
.think-meta { font-size: 12px; color: rgba(0,0,0,0.35); font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }

/* ── Suggestion ──────────────────────────────────────────────────────────── */
.suggestion-row { display: flex; justify-content: flex-end; }
.suggestion-chip {
  max-width: 80%; height: auto; padding: 7px 14px;
  border: 1px dashed #BAE0FF; background: #F0F7FF;
  border-radius: 10px; display: flex; align-items: center; gap: 8px;
  font-size: 13px; color: rgba(0,0,0,0.65); cursor: pointer; font-family: inherit;
  text-align: left; line-height: 1.5;
}
.suggestion-chip:hover { border-color: #1677FF; color: #1677FF; }

/* ── Fade-in ─────────────────────────────────────────────────────────────── */
.fadein { animation: octo-fadein 0.25s ease; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
</style>
