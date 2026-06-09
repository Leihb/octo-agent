package tools

import (
	"context"
	"sync"
)

// backgroundManagerCtxKey scopes a BackgroundManager to a turn's context, the
// same way WithSubAgentManager / WithTaskStore scope theirs. The web server and
// IM bridge stamp a per-session manager so each conversation's background
// processes are isolated — their own bg_N id namespace, invisible to other
// sessions' terminal_output / kill_shell — and are reaped when the session
// ends, instead of sharing the process-global defaultBg.
type backgroundManagerCtxKey struct{}

// WithBackgroundManager returns ctx carrying mgr as the manager the terminal
// tools dispatch to for this turn.
func WithBackgroundManager(ctx context.Context, mgr *BackgroundManager) context.Context {
	return context.WithValue(ctx, backgroundManagerCtxKey{}, mgr)
}

func backgroundManagerFromContext(ctx context.Context) *BackgroundManager {
	m, _ := ctx.Value(backgroundManagerCtxKey{}).(*BackgroundManager)
	return m
}

// resolveBackgroundManager picks the manager a terminal tool should use: the
// ctx-scoped per-session one (web/IM) first, then a tool-local override, then
// the process-global default (CLI/TUI, which never stamp a ctx manager).
func resolveBackgroundManager(ctx context.Context, local *BackgroundManager) *BackgroundManager {
	if m := backgroundManagerFromContext(ctx); m != nil {
		return m
	}
	if local != nil {
		return local
	}
	return defaultBg
}

// Per-session background managers, keyed by an opaque session id chosen by the
// caller (web session id / IM session key). Created on demand, reaped either
// when the session is deleted (CloseSessionBackgroundManager) or on daemon
// shutdown (KillAllBackground reaps all of them). Kept separate from defaultBg
// so the CLI/TUI — which never stamp a ctx manager — are unaffected.
var (
	sessionMgrsMu sync.Mutex
	sessionMgrs   = map[string]*BackgroundManager{}
)

// SessionBackgroundManager returns the per-session manager for id, creating and
// registering it on first use.
func SessionBackgroundManager(id string) *BackgroundManager {
	sessionMgrsMu.Lock()
	defer sessionMgrsMu.Unlock()
	m := sessionMgrs[id]
	if m == nil {
		m = NewBackgroundManager()
		sessionMgrs[id] = m
	}
	return m
}

// CloseSessionBackgroundManager kills every process tracked for a session and
// drops its manager. Call when a session is deleted so its background daemons
// don't leak until daemon shutdown. No-op for an unknown id.
func CloseSessionBackgroundManager(id string) {
	sessionMgrsMu.Lock()
	m := sessionMgrs[id]
	delete(sessionMgrs, id)
	sessionMgrsMu.Unlock()
	if m != nil {
		m.KillAll()
	}
}

// allBackgroundManagers returns defaultBg plus every live per-session manager,
// so process-wide operations (shutdown reap) cover every tracked process.
func allBackgroundManagers() []*BackgroundManager {
	sessionMgrsMu.Lock()
	defer sessionMgrsMu.Unlock()
	out := make([]*BackgroundManager, 0, len(sessionMgrs)+1)
	out = append(out, defaultBg)
	for _, m := range sessionMgrs {
		out = append(out, m)
	}
	return out
}
