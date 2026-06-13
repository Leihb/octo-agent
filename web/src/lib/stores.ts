import { writable, derived, get } from 'svelte/store'
import type { Session, Skill, ScheduledTask, McpServer, Channel, Memory, Artifact, ArtifactView } from './types'

// Navigation
export const view = writable('chat')
export const sidebar = writable('full')
export const cmdkOpen = writable(false)
export const mcpModalOpen = writable(false)
export const toast = writable<{ msg: string; type: string } | null>(null)

// Runtime / WS state
export const running = writable(false)
export const wsDown = writable(false)

// Artifacts panel
export const artifactsOpen = writable(false)
export const artifacts = writable<Artifact[]>([])
export const artifactSel = writable(0)
export const artifactView = writable<ArtifactView>('preview')

// Sessions
export const sessions = writable<Session[]>([])
export const activeSessionId = writable<string | null>(null)
// Sidebar session UI state
export const activeSession = writable<string | null>(null)
export const selMode = writable(false)
export const sel = writable<Record<string, boolean>>({})
export const menuFor = writable<string | null>(null)
export const editId = writable<string | null>(null)
export const editDraft = writable('')

// Per-session chat state (keyed by sessionId)
export const chatMessages = writable<Record<string, any[]>>({})
export const chatStreaming = writable<Record<string, boolean>>({})
export const chatProgress = writable<Record<string, any>>({})
export const chatBgTasks = writable<Record<string, any[]>>({})
export const chatTodos = writable<Record<string, any[]>>({})
export const chatContextUsage = writable<Record<string, any>>({})
export const chatWorkingDir = writable<Record<string, string>>({})
export const chatPermMode = writable<Record<string, string>>({})
export const chatReasoningEffort = writable<Record<string, string>>({})
export const chatSuggestion = writable<Record<string, string>>({})

// Permission/question modals
export const confirmModal = writable<any | null>(null)
export const questionModal = writable<any | null>(null)
export const feedbackModal = writable<any | null>(null)

// Admin data (loaded by views)
export const skills = writable<Skill[]>([])
export const tasks = writable<ScheduledTask[]>([])
export const mcpServers = writable<McpServer[]>([])
export const toolSearchMode = writable('auto')
export const channels = writable<Channel[]>([])
export const memories = writable<Memory[]>([])
export const trashItems = writable<any[]>([])
export const profileSoul = writable<string | null>(null)
export const profileUser = writable<any | null>(null)
export const memTab = writable<'soul' | 'user' | 'memories'>('soul')
export const recallFiles = writable<any[]>([])

// Monotonic id generator — Date.now() collides when called many times in the
// same millisecond (e.g. rendering a page of history synchronously), which
// produces duplicate {#each} keys and crashes the list with each_key_duplicate.
let _idSeq = 0
export function uid(prefix = 'm'): string {
  _idSeq += 1
  return `${prefix}-${_idSeq}`
}

// Helper functions
export function showToast(msg: string, type = 'success') {
  toast.set({ msg, type })
  setTimeout(() => toast.set(null), 3200)
}

export function setActiveSession(id: string) {
  activeSessionId.set(id)
  chatMessages.update(m => ({ ...m, [id]: m[id] || [] }))
}

export function addChatMsg(sessionId: string, msg: any) {
  chatMessages.update(m => ({ ...m, [sessionId]: [...(m[sessionId] || []), msg] }))
}

export function updateLastMsg(sessionId: string, updater: (msg: any) => any) {
  chatMessages.update(m => {
    const msgs = [...(m[sessionId] || [])]
    if (msgs.length) msgs[msgs.length - 1] = updater(msgs[msgs.length - 1])
    return { ...m, [sessionId]: msgs }
  })
}

export function appendToLastAssistant(sessionId: string, content: string) {
  chatMessages.update(m => {
    const msgs = [...(m[sessionId] || [])]
    const last = msgs.findLastIndex((x: any) => x.type === 'assistant')
    if (last >= 0) msgs[last] = { ...msgs[last], content: msgs[last].content + content, streaming: true }
    return { ...m, [sessionId]: msgs }
  })
}

export function clearMsgs(sessionId: string) {
  chatMessages.update(m => ({ ...m, [sessionId]: [] }))
}

export function addToolCallToGroup(sessionId: string, toolCall: any) {
  chatMessages.update(m => {
    const msgs = [...(m[sessionId] || [])]
    const lastGroup = msgs.findLastIndex((x: any) => x.type === 'tool_group')
    if (lastGroup >= 0 && msgs[lastGroup].streaming) {
      msgs[lastGroup] = { ...msgs[lastGroup], tools: [...msgs[lastGroup].tools, toolCall] }
    } else {
      msgs.push({ id: uid('grp'), type: 'tool_group', content: '', streaming: true, tools: [toolCall], todos: [], createdAt: Date.now() })
    }
    return { ...m, [sessionId]: msgs }
  })
}

// Find the tool to update: prefer an exact toolId match (correct for parallel
// tool calls whose results arrive out of order), else the first not-yet-done
// tool, else the last one. Parallel tool_use returns N calls then N results, so
// "last tool" alone mis-assigns every result to the final call.
function pickToolIndex(tools: any[], toolId: string | undefined): number {
  if (toolId) {
    const byId = tools.findIndex((t: any) => t.toolId && t.toolId === toolId)
    if (byId >= 0) return byId
  }
  const pending = tools.findIndex((t: any) => !t.done && !t.error)
  if (pending >= 0) return pending
  return tools.length - 1
}

export function updateToolResult(sessionId: string, toolId: string | undefined, result: any, uiPayload: any) {
  chatMessages.update(m => {
    const msgs = [...(m[sessionId] || [])]
    const lastGroup = msgs.findLastIndex((x: any) => x.type === 'tool_group')
    if (lastGroup >= 0) {
      const tools = [...msgs[lastGroup].tools]
      const idx = pickToolIndex(tools, toolId)
      if (idx >= 0) {
        const started = tools[idx].startedAt
        const elapsed = started ? (Date.now() - started) / 1000 : tools[idx].elapsed
        tools[idx] = { ...tools[idx], result, ui_payload: uiPayload, done: true, elapsed }
      }
      msgs[lastGroup] = { ...msgs[lastGroup], tools }
    }
    return { ...m, [sessionId]: msgs }
  })
}

export function setToolError(sessionId: string, toolId: string | undefined, error: string) {
  chatMessages.update(m => {
    const msgs = [...(m[sessionId] || [])]
    const lastGroup = msgs.findLastIndex((x: any) => x.type === 'tool_group')
    if (lastGroup >= 0) {
      const tools = [...msgs[lastGroup].tools]
      const idx = pickToolIndex(tools, toolId)
      if (idx >= 0) {
        const started = tools[idx].startedAt
        const elapsed = started ? (Date.now() - started) / 1000 : tools[idx].elapsed
        tools[idx] = { ...tools[idx], error, done: true, elapsed }
      }
      msgs[lastGroup] = { ...msgs[lastGroup], tools }
    }
    return { ...m, [sessionId]: msgs }
  })
}

// Mark every still-running tool in the session as done — called on `complete`
// so a finished turn never leaves tools spinning (parallel results that never
// matched, or a tool whose result event was dropped).
export function finishAllTools(sessionId: string) {
  chatMessages.update(m => {
    const msgs = (m[sessionId] || []).map((msg: any) =>
      msg.type === 'tool_group'
        ? {
            ...msg,
            streaming: false,
            tools: msg.tools.map((t: any) => (t.done || t.error ? t : { ...t, done: true })),
          }
        : msg
    )
    return { ...m, [sessionId]: msgs }
  })
}
