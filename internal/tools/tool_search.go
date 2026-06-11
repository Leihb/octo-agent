package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/Leihb/octo-agent/internal/agent"
)

// Tool Search defers MCP tool schemas behind three bridge tools instead of
// uploading every schema on every turn. The model discovers tools with
// mcp_search, loads one schema with mcp_describe, and invokes it with
// mcp_call — which routes straight into executeMCP, so all the existing
// mcp__-prefix dispatch, permission, and hook machinery runs against the real
// tool name. See dev-docs/tool-search-mcp.md.

// Bridge tool names. The mcp_ prefix is deliberate: it tells the model these
// three tools are the MCP-only discover/describe/invoke path, so it doesn't
// mistake mcp_call for a generic dispatcher and route built-in tools (sub_agent,
// read_file, …) through it. mcp_call is the only path by which a deferred MCP
// tool is actually invoked when Tool Search is active.
const (
	toolSearchName   = "mcp_search"
	toolDescribeName = "mcp_describe"
	toolCallName     = "mcp_call"
)

// mcpCatalog returns the deferred MCP tool catalog (the same defs mcpToolDefs
// would upload in full). A package var so tests can inject a fixed catalog
// without standing up a live MCP registry — mirrors jinaReaderHostForTest.
var mcpCatalog = mcpToolDefs

// ── configuration ──────────────────────────────────────────────────────────

// ToolSearchMode selects when the bridge replaces full MCP schema upload.
type ToolSearchMode int

const (
	// ToolSearchAuto activates the bridge only when deferred MCP schemas would
	// occupy at least ThresholdPct% of the model's context window.
	ToolSearchAuto ToolSearchMode = iota
	// ToolSearchOn activates the bridge whenever any MCP tool is present.
	ToolSearchOn
	// ToolSearchOff never activates the bridge — MCP schemas upload in full.
	ToolSearchOff
)

// ToolSearchConfig is the tools-package view of the user's tool_search config.
// cmd/octo maps the ~/.octo/config.yml block onto this and installs it via
// SetToolSearchConfig, mirroring SetSandbox / SetMCPRegistry.
type ToolSearchConfig struct {
	Mode           ToolSearchMode
	ThresholdPct   int // auto-mode activation threshold, percent of context window
	SearchLimit    int // default number of hits tool_search returns
	MaxSearchLimit int // upper bound on the caller-supplied limit
}

// defaultToolSearchConfig matches the documented defaults (auto / 10% / 5 / 20).
func defaultToolSearchConfig() ToolSearchConfig {
	return ToolSearchConfig{Mode: ToolSearchAuto, ThresholdPct: 10, SearchLimit: 5, MaxSearchLimit: 20}
}

var (
	toolSearchCfgMu sync.RWMutex
	toolSearchCfg   = defaultToolSearchConfig()
)

// SetToolSearchConfig installs the active tool_search configuration. Fields
// left at their zero value fall back to the documented defaults so a partial
// config block behaves sensibly.
func SetToolSearchConfig(c ToolSearchConfig) {
	d := defaultToolSearchConfig()
	if c.ThresholdPct <= 0 {
		c.ThresholdPct = d.ThresholdPct
	}
	if c.SearchLimit <= 0 {
		c.SearchLimit = d.SearchLimit
	}
	if c.MaxSearchLimit <= 0 {
		c.MaxSearchLimit = d.MaxSearchLimit
	}
	toolSearchCfgMu.Lock()
	toolSearchCfg = c
	toolSearchCfgMu.Unlock()
}

func toolSearchConfig() ToolSearchConfig {
	toolSearchCfgMu.RLock()
	defer toolSearchCfgMu.RUnlock()
	return toolSearchCfg
}

// toolSearchActive decides whether to replace the MCP defs with the bridge for
// a given model. mcpDefs is the catalog that would otherwise be uploaded.
//
// An empty model (the DefaultTools back-compat entry) never activates the
// bridge, so callers that don't know the model keep the original behaviour.
func toolSearchActive(model string, mcpDefs []agent.ToolDefinition) bool {
	if len(mcpDefs) == 0 {
		return false
	}
	switch toolSearchConfig().Mode {
	case ToolSearchOff:
		return false
	case ToolSearchOn:
		return true
	default: // auto
		if model == "" {
			return false
		}
		window := agent.ContextWindow(model)
		budget := window * toolSearchConfig().ThresholdPct / 100
		return estimateSchemaTokens(mcpDefs) >= budget
	}
}

// estimateSchemaTokens approximates how many tokens the MCP tool definitions
// occupy. A coarse bytes/4 heuristic is plenty for a threshold decision.
func estimateSchemaTokens(defs []agent.ToolDefinition) int {
	bytes := 0
	for _, d := range defs {
		bytes += len(d.Name) + len(d.Description)
		if b, err := json.Marshal(d.Parameters); err == nil {
			bytes += len(b)
		}
	}
	return bytes / 4
}

// ── bridge tool definitions ────────────────────────────────────────────────

// toolSearchBridgeDefs returns the three bridge tools advertised in place of
// the full MCP catalog when Tool Search is active.
func toolSearchBridgeDefs() []agent.ToolDefinition {
	cfg := toolSearchConfig()
	return []agent.ToolDefinition{
		{
			Name: toolSearchName,
			Description: fmt.Sprintf("Search the catalog of available MCP tools by keyword and "+
				"return matching tool names with a one-line description (NOT their full schema). "+
				"MCP tools are not loaded up front — discover them here first. Workflow: "+
				"mcp_search to find a tool, mcp_describe to load its parameters, mcp_call to "+
				"invoke it. This catalog holds ONLY MCP (mcp__-prefixed) tools — built-in tools "+
				"like sub_agent or read_file are not here; call those directly by name. Returns "+
				"up to %d hits by default.", cfg.SearchLimit),
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Keywords describing the tool you need, e.g. 'create github issue'.",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": fmt.Sprintf("Max hits to return (default %d, capped at %d).", cfg.SearchLimit, cfg.MaxSearchLimit),
					},
				},
				"required": []string{"query"},
			},
		},
		{
			Name: toolDescribeName,
			Description: "Load the full JSON Schema (parameters) for one MCP tool discovered via " +
				"mcp_search. Call this before mcp_call so you know the tool's exact arguments.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "The exact tool name from mcp_search (e.g. 'mcp__github__create_issue').",
					},
				},
				"required": []string{"name"},
			},
		},
		{
			Name: toolCallName,
			Description: "Invoke an MCP tool (and ONLY an MCP tool — its name starts with mcp__) " +
				"discovered via mcp_search. Pass the tool name and its arguments (matching the schema " +
				"from mcp_describe). This is the only way to call a deferred MCP tool while Tool Search " +
				"is active. Do NOT route built-in tools (sub_agent, read_file, terminal, …) through " +
				"here — call those directly by their own name.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "The exact MCP tool name to invoke (e.g. 'mcp__github__create_issue').",
					},
					"arguments": map[string]any{
						"type":        "object",
						"description": "The arguments object for the tool, matching its schema from mcp_describe.",
					},
				},
				"required": []string{"name", "arguments"},
			},
		},
	}
}

// ── dispatch (called from DefaultRegistry.Execute) ─────────────────────────

// execToolSearch runs a BM25 search over the live MCP catalog and returns the
// matching tool names with their one-line descriptions — never their schemas.
func execToolSearch(input map[string]any) (agent.ToolResult, error) {
	query := strings.TrimSpace(stringArg(input, "query"))
	if query == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("mcp_search: query is required")
	}
	cfg := toolSearchConfig()
	limit := intArg(input, "limit", cfg.SearchLimit)
	if limit < 1 {
		limit = cfg.SearchLimit
	}
	if limit > cfg.MaxSearchLimit {
		limit = cfg.MaxSearchLimit
	}

	catalog := mcpCatalog()
	if len(catalog) == 0 {
		return agent.ToolResult{Text: "(no MCP tools available)"}, nil
	}
	hits := bm25Search(catalog, query, limit)
	if len(hits) == 0 {
		return agent.ToolResult{Text: fmt.Sprintf("(no tools match %q)", query)}, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d match(es) for %q — use mcp_describe to load a tool's parameters:\n", len(hits), query)
	for _, d := range hits {
		desc := firstLine(d.Description)
		if desc == "" {
			desc = "(no description)"
		}
		fmt.Fprintf(&b, "- %s — %s\n", d.Name, desc)
	}
	return agent.ToolResult{Text: strings.TrimRight(b.String(), "\n")}, nil
}

// execToolDescribe returns the full JSON Schema of one catalog tool.
func execToolDescribe(input map[string]any) (agent.ToolResult, error) {
	name := strings.TrimSpace(stringArg(input, "name"))
	if name == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("mcp_describe: name is required")
	}
	for _, d := range mcpCatalog() {
		if d.Name == name {
			schema, err := json.MarshalIndent(d.Parameters, "", "  ")
			if err != nil {
				return agent.ToolResult{Text: ""}, fmt.Errorf("mcp_describe: marshal schema: %w", err)
			}
			return agent.ToolResult{Text: fmt.Sprintf("%s\n\n%s\n\n%s", d.Name, firstLine(d.Description), string(schema))}, nil
		}
	}
	return agent.ToolResult{Text: ""}, fmt.Errorf("mcp_describe: no tool named %q (use mcp_search to find the exact name)", name)
}

// execToolCall unwraps {name, arguments} and forwards to executeMCP, so the
// real MCP dispatch (and the permission/hook machinery keyed on the real name)
// runs unchanged.
func execToolCall(ctx context.Context, input map[string]any) (agent.ToolResult, error) {
	name := strings.TrimSpace(stringArg(input, "name"))
	if name == "" {
		return agent.ToolResult{Text: ""}, fmt.Errorf("mcp_call: name is required")
	}
	args, _ := input["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	out, ok, err := executeMCP(ctx, name, args)
	if !ok {
		// executeMCP only declines names without the mcp__ prefix.
		return agent.ToolResult{Text: ""}, fmt.Errorf("mcp_call: %q is not an MCP tool (MCP names look like 'mcp__<server>__<tool>'). If it's a built-in tool such as sub_agent, call it directly by name instead of through mcp_call", name)
	}
	return agent.ToolResult{Text: out}, err
}

// ToolCallTarget unwraps an mcp_call bridge invocation into the real MCP tool
// name and its arguments, so permission checks and hooks key on the real tool
// rather than the "mcp_call" wrapper. ok is false when name isn't an mcp_call
// (or carries no inner tool name), in which case the caller uses name/input
// unchanged.
func ToolCallTarget(name string, input map[string]any) (realName string, realInput map[string]any, ok bool) {
	if name != toolCallName {
		return "", nil, false
	}
	realName = strings.TrimSpace(stringArg(input, "name"))
	if realName == "" {
		return "", nil, false
	}
	realInput, _ = input["arguments"].(map[string]any)
	if realInput == nil {
		realInput = map[string]any{}
	}
	return realName, realInput, true
}

// ── BM25 ────────────────────────────────────────────────────────────────────

// bm25Search ranks catalog entries against query using BM25 over each tool's
// name + description + parameter-property names, and returns the top n. When no
// term has any document frequency (zero IDF — e.g. a brand-new term), it falls
// back to a substring match on name + description so a reasonable query never
// comes back empty just because the corpus is tiny.
func bm25Search(catalog []agent.ToolDefinition, query string, n int) []agent.ToolDefinition {
	const k1, b = 1.5, 0.75

	docs := make([][]string, len(catalog))
	var totalLen int
	for i, d := range catalog {
		docs[i] = tokenizeForSearch(catalogText(d))
		totalLen += len(docs[i])
	}
	avgLen := 0.0
	if len(docs) > 0 {
		avgLen = float64(totalLen) / float64(len(docs))
	}

	// Document frequency per term.
	df := map[string]int{}
	for _, toks := range docs {
		for t := range uniqueTokens(toks) {
			df[t]++
		}
	}

	qTerms := tokenizeForSearch(query)
	N := float64(len(docs))

	type scored struct {
		idx   int
		score float64
	}
	var ranked []scored
	for i, toks := range docs {
		tf := termFreq(toks)
		var score float64
		for _, qt := range qTerms {
			f := float64(tf[qt])
			if f == 0 {
				continue
			}
			idf := math.Log(1 + (N-float64(df[qt])+0.5)/(float64(df[qt])+0.5))
			denom := f + k1*(1-b+b*float64(len(toks))/maxF(avgLen, 1))
			score += idf * (f * (k1 + 1)) / denom
		}
		if score > 0 {
			ranked = append(ranked, scored{i, score})
		}
	}

	if len(ranked) == 0 {
		return substringFallback(catalog, query, n)
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	if len(ranked) > n {
		ranked = ranked[:n]
	}
	out := make([]agent.ToolDefinition, 0, len(ranked))
	for _, r := range ranked {
		out = append(out, catalog[r.idx])
	}
	return out
}

// substringFallback returns catalog entries whose name or description contains
// the query (case-insensitive), capped at n. Used when BM25 finds nothing.
func substringFallback(catalog []agent.ToolDefinition, query string, n int) []agent.ToolDefinition {
	q := strings.ToLower(strings.TrimSpace(query))
	var out []agent.ToolDefinition
	for _, d := range catalog {
		if strings.Contains(strings.ToLower(d.Name+" "+d.Description), q) {
			out = append(out, d)
			if len(out) >= n {
				break
			}
		}
	}
	return out
}

// catalogText is the searchable text for one tool: its name, description, and
// the names of its top-level parameter properties.
func catalogText(d agent.ToolDefinition) string {
	var b strings.Builder
	b.WriteString(d.Name)
	b.WriteByte(' ')
	b.WriteString(d.Description)
	if props, ok := d.Parameters["properties"].(map[string]any); ok {
		for k := range props {
			b.WriteByte(' ')
			b.WriteString(k)
		}
	}
	return b.String()
}

// tokenizeForSearch lower-cases and splits on non-alphanumeric runes, and also
// splits snake_case / mcp__ separators (already covered by the underscore split)
// and camelCase boundaries so "createIssue" matches "create" and "issue".
func tokenizeForSearch(s string) []string {
	var raw []string
	cur := strings.Builder{}
	flush := func() {
		if cur.Len() > 0 {
			raw = append(raw, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(unicode.ToLower(r))
		} else {
			flush()
		}
	}
	flush()

	// Split camelCase tokens into their parts (kept alongside the whole token).
	var out []string
	for _, tok := range raw {
		out = append(out, tok)
		for _, part := range splitCamel(tok) {
			if part != tok {
				out = append(out, part)
			}
		}
	}
	return out
}

// splitCamel breaks a token on lower→upper boundaries; here the input is
// already lower-cased, so this only ever returns the token itself. Kept as a
// hook so future tokenization (raw, pre-lowercase) can split camelCase without
// touching callers.
func splitCamel(tok string) []string { return []string{tok} }

func termFreq(toks []string) map[string]int {
	m := make(map[string]int, len(toks))
	for _, t := range toks {
		m[t]++
	}
	return m
}

func uniqueTokens(toks []string) map[string]struct{} {
	m := make(map[string]struct{}, len(toks))
	for _, t := range toks {
		m[t] = struct{}{}
	}
	return m
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
