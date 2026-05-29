package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/tools"
)

// agentSpawner implements tools.Spawner by building a child agent on each
// launch_agent call. The child shares the parent's Sender (one provider
// connection) and System (same harness identity), but runs in isolation:
// fresh History, no visibility into the parent's conversation, its own
// loop budget. The final child reply text is returned to the parent as the
// launch_agent tool_result; the child's token usage is rolled into the
// parent's session totals so /cost reports one consolidated number.
//
// toolsFn is a deferred lookup of the LLM-facing tool catalog (DefaultTools).
// Resolving it on each Spawn — rather than capturing a slice at construction
// — lets cmd/octo set up the spawner before computing the tool list, since
// SetSpawner has to run first for launch_agent to appear in DefaultTools().
type agentSpawner struct {
	parent   *agent.Agent
	executor agent.ToolExecutor
	toolsFn  func() []agent.ToolDefinition
	// reg keeps spawned children alive after Spawn returns so a later
	// send_message (tools.Spawner.Continue) can re-run them with their history
	// intact. In-memory and session-scoped: it lives as long as this spawner
	// (one per REPL session), and a fresh process starts empty.
	reg *childRegistry
}

func newAgentSpawner(parent *agent.Agent, executor agent.ToolExecutor, toolsFn func() []agent.ToolDefinition) *agentSpawner {
	return &agentSpawner{
		parent:   parent,
		executor: executor,
		toolsFn:  toolsFn,
		reg:      newChildRegistry(),
	}
}

// childMaxTurns caps the sub-agent's tool loop. Smaller than the parent's
// default — sub-agents are meant for focused sub-tasks, not free-form work,
// and a runaway child can otherwise burn through budget unnoticed. Each
// Continue (send_message) re-arms this budget for the next round.
const childMaxTurns = 12

// Spawn implements tools.Spawner. It builds an isolated child, registers it so
// a later send_message can continue it, runs the first prompt, and returns the
// child's id alongside its reply.
func (s *agentSpawner) Spawn(ctx context.Context, req tools.SpawnRequest) (tools.SpawnResult, error) {
	childTools := filterChildTools(s.toolsFn(), req.Tools)

	model := req.Model
	if model == "" {
		model = s.parent.Model
	}

	child := agent.New(s.parent.Sender, model)
	child.System = s.parent.System // share harness identity (base + soul + env + skills + memory + …)
	child.MaxTokens = s.parent.MaxTokens
	child.Gate = s.parent.Gate
	child.MaxTurns = childMaxTurns
	// Inherit the parent's hard cost ceiling — the child's spend is rolled
	// back into the parent below, but the per-loop check inside the child
	// uses its own counter, so a budget the user already set on the parent
	// should still constrain the child.
	child.MaxCostUSD = s.parent.MaxCostUSD

	lc := &liveChild{agent: child, tools: childTools, executor: s.executor}
	id := s.reg.put(lc)

	reply, in, out, err := s.runChild(ctx, lc, req.Prompt)
	if err != nil {
		return tools.SpawnResult{}, err
	}
	return tools.SpawnResult{
		AgentID:      id,
		Reply:        reply,
		InputTokens:  in,
		OutputTokens: out,
	}, nil
}

// Continue implements tools.Spawner. It re-runs a still-alive child with a new
// message. An unknown / evicted id returns an error whose text steers the model
// to launch a fresh sub-agent.
func (s *agentSpawner) Continue(ctx context.Context, agentID, message string) (tools.SpawnResult, error) {
	lc, ok := s.reg.get(agentID)
	if !ok {
		return tools.SpawnResult{}, fmt.Errorf(
			"agent %s is no longer alive (idle-expired or evicted); launch a fresh sub-agent instead", agentID)
	}
	reply, in, out, err := s.runChild(ctx, lc, message)
	if err != nil {
		return tools.SpawnResult{}, err
	}
	return tools.SpawnResult{
		AgentID:      agentID,
		Reply:        reply,
		InputTokens:  in,
		OutputTokens: out,
	}, nil
}

// runChild is the shared body of Spawn and Continue. It serializes calls to a
// single child (a child's history can't take two interleaved turns), re-stamps
// the sub-agent context marker so the child can't recurse, and accrues only the
// token delta for this round into the parent (SessionTokens is cumulative, so
// re-accruing the total would double-count earlier rounds). The returned (in,
// out) are this round's delta.
func (s *agentSpawner) runChild(ctx context.Context, lc *liveChild, prompt string) (reply string, in, out int, err error) {
	lc.mu.Lock()
	defer lc.mu.Unlock()

	childCtx := tools.WithSubAgentMarker(ctx)
	r, err := lc.agent.Run(childCtx, prompt, lc.tools, lc.executor)
	if err != nil {
		return "", 0, 0, err
	}

	totIn, totOut := lc.agent.SessionTokens()
	in, out = totIn-lc.accruedIn, totOut-lc.accruedOut
	lc.accruedIn, lc.accruedOut = totIn, totOut
	s.parent.AccrueChildUsage(in, out)

	return r.Content, in, out, nil
}

// filterChildTools drops launch_agent and send_message (a sub-agent can
// neither spawn nor wake another sub-agent — those stay top-level-only) and,
// when allowed is non-empty, intersects with that allowlist so the parent can
// hand the child a restricted toolbelt (e.g. read-only research).
func filterChildTools(parent []agent.ToolDefinition, allowed []string) []agent.ToolDefinition {
	var allowSet map[string]bool
	if len(allowed) > 0 {
		allowSet = make(map[string]bool, len(allowed))
		for _, a := range allowed {
			allowSet[a] = true
		}
	}
	out := make([]agent.ToolDefinition, 0, len(parent))
	for _, td := range parent {
		if td.Name == "launch_agent" || td.Name == "send_message" {
			continue
		}
		if allowSet != nil && !allowSet[td.Name] {
			continue
		}
		out = append(out, td)
	}
	return out
}

// Live-child registry — keeps spawned sub-agents addressable for send_message.

const (
	// maxLiveChildren caps how many sub-agents stay resumable at once. Beyond
	// this the least-recently-used is evicted (its history is dropped; a
	// send_message to it then fails and the model relaunches).
	maxLiveChildren = 8
	// childIdleTTL evicts a sub-agent that hasn't been touched in this long,
	// so abandoned children don't pin their histories in memory for the whole
	// session.
	childIdleTTL = 30 * time.Minute
)

// liveChild is one resumable sub-agent: its Agent (history accumulates across
// Run calls), the toolbelt + executor it was spawned with (a Continue reuses
// them), and the bookkeeping for serialization, eviction, and delta-accounting.
type liveChild struct {
	agent    *agent.Agent
	tools    []agent.ToolDefinition
	executor agent.ToolExecutor

	mu sync.Mutex // serializes runChild on this child

	// accruedIn/accruedOut track how much of the child's cumulative
	// SessionTokens has already been folded into the parent, so each round
	// accrues only its delta.
	accruedIn  int
	accruedOut int

	lastUsed time.Time // for TTL eviction
	seq      uint64    // monotonic touch order, for LRU eviction (clock-independent)
}

// childRegistry holds the live children for one spawner (one REPL session).
// Purely in-memory: nothing is persisted, and the map is dropped when the
// spawner goes away with the session.
type childRegistry struct {
	mu     sync.Mutex
	m      map[string]*liveChild
	seqCtr uint64
	now    func() time.Time // injectable for tests
}

func newChildRegistry() *childRegistry {
	return &childRegistry{m: make(map[string]*liveChild), now: time.Now}
}

// put registers a child under a fresh id and returns it. Eviction runs after
// insertion so the cap holds even counting the new entry; the just-added child
// is the most-recently-used, so LRU never evicts it.
func (r *childRegistry) put(lc *liveChild) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.freshIDLocked()
	r.touchLocked(lc)
	r.m[id] = lc
	r.evictLocked()
	return id
}

// get returns the child for id (refreshing its LRU/TTL standing) or (nil,
// false) if it's unknown or already evicted.
func (r *childRegistry) get(id string) (*liveChild, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictLocked()
	lc, ok := r.m[id]
	if ok {
		r.touchLocked(lc)
	}
	return lc, ok
}

// touchLocked stamps a child as most-recently-used. Caller holds r.mu.
func (r *childRegistry) touchLocked(lc *liveChild) {
	r.seqCtr++
	lc.seq = r.seqCtr
	lc.lastUsed = r.now()
}

// evictLocked drops TTL-expired children, then trims to maxLiveChildren by
// least-recently-used. Caller holds r.mu.
func (r *childRegistry) evictLocked() {
	now := r.now()
	for id, lc := range r.m {
		if now.Sub(lc.lastUsed) > childIdleTTL {
			delete(r.m, id)
		}
	}
	for len(r.m) > maxLiveChildren {
		var oldestID string
		var oldestSeq uint64
		first := true
		for id, lc := range r.m {
			if first || lc.seq < oldestSeq {
				oldestID, oldestSeq, first = id, lc.seq, false
			}
		}
		delete(r.m, oldestID)
	}
}

// freshIDLocked returns an 8-hex-char id not currently in use. Same shape as
// agent.Session / taskgraph short ids. Caller holds r.mu.
func (r *childRegistry) freshIDLocked() string {
	for {
		var b [4]byte
		_, _ = rand.Read(b[:]) // crypto/rand.Read effectively never fails
		id := hex.EncodeToString(b[:])
		if _, taken := r.m[id]; !taken {
			return id
		}
	}
}
