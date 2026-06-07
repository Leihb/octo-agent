// Package permission gates tool calls through a rule-driven decision
// engine. Each tool invocation is evaluated against an ordered list of
// rules; the first matching rule's decision (allow / deny / ask) wins.
// If no rule matches, the implicit default is ask.
//
// The engine is the centerpiece of M6.5 — without it, any caller that
// reaches the agent (CLI, future M8 HTTP server, future M9 IM bridge)
// can drive arbitrary tools, including `terminal: rm -rf`. With it,
// CLI users get an interactive prompt for risky operations and remote
// callers are denied unless explicitly whitelisted in ~/.octo/permissions.yml.
//
// Embedding rules:
//
//	The package ships an embedded defaults.yml that's loaded when no user
//	config is present. The defaults aim for "safe and usable" — common
//	tools like ls / git status auto-allow, dangerous operations like
//	rm -rf / sudo trigger ask, and credential-adjacent paths (~/.ssh/*,
//	.env*, /etc/*) always deny.
package permission

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Decision is the outcome of a permission check.
type Decision string

const (
	Allow Decision = "allow"
	Deny  Decision = "deny"
	Ask   Decision = "ask"
)

// Mode selects how the engine resolves Ask decisions.
//
// Interactive (CLI default) returns Ask as-is so the caller can prompt the
// user. AutoApprove converts Ask → Allow, so no interactive prompt is shown.
// Allow / Deny pass through unchanged in both modes.
type Mode string

const (
	ModeInteractive Mode = "interactive"
	ModeAutoApprove Mode = "auto"
)

// Rule is one entry in the rule list for a tool. Exactly one of Pattern /
// Hostname / Path is populated, depending on the tool's natural axis.
type Rule struct {
	Decision Decision

	// Pattern is a case-sensitive substring match against the relevant
	// input field. For `terminal` that's input["command"]; for the
	// pattern-free tools (glob / grep / web_search) Pattern is empty and
	// the rule matches unconditionally.
	Pattern string

	// Hostname is a list of glob patterns matched against URL hosts.
	// Used by web_fetch. `*` matches a single DNS label, so `*.dev`
	// matches `foo.dev` but not `foo.bar.dev`.
	Hostname []string

	// Path is a list of glob patterns matched against file paths.
	// Used by write_file / edit_file / read_file. `**` matches any
	// number of directory components; `$CWD` expands to the engine's
	// configured working directory at construction time.
	Path []string
}

// RuleSet maps tool name → ordered list of rules.
type RuleSet map[string][]Rule

// Engine evaluates rules against tool calls and remembers session-level
// decisions for "always allow this" style answers.
type Engine struct {
	rules RuleSet
	mode  Mode
	cwd   string

	mu       sync.Mutex
	remember map[string]Decision // input-signature → decision
}

//go:embed defaults.yml
var defaultsYAML []byte

// New builds an Engine. configPath may be empty (use embedded defaults)
// or point to a YAML file overriding them. cwd is the working directory
// used to expand `$CWD` in path rules — pass os.Getwd() unless overriding
// for tests. allowWriteRoots are extra directories (e.g. the per-repo memory
// directory, which lives outside CWD) whose contents the agent may write/edit
// without a prompt; each gets a prepended allow rule on write_file / edit_file.
func New(configPath string, cwd string, mode Mode, allowWriteRoots ...string) (*Engine, error) {
	rules, err := loadRules(defaultsYAML)
	if err != nil {
		return nil, fmt.Errorf("permission: parse default rules: %w", err)
	}

	if configPath != "" {
		raw, err := os.ReadFile(configPath)
		switch {
		case err == nil:
			user, err := loadRules(raw)
			if err != nil {
				return nil, fmt.Errorf("permission: parse %s: %w", configPath, err)
			}
			// User rules override defaults per-tool. (Full-replace per
			// tool, not append — keeps "I want to allow rm -rf in this
			// environment" simple to express.)
			for tool, rs := range user {
				rules[tool] = rs
			}
		case os.IsNotExist(err):
			// Fall through with defaults only.
		default:
			return nil, fmt.Errorf("permission: read %s: %w", configPath, err)
		}
	}

	if cwd == "" {
		// Reasonable fallback; rules using $CWD against a missing cwd
		// will just not match anything.
		cwd, _ = os.Getwd()
	}
	if mode == "" {
		mode = ModeInteractive
	}

	// Whitelist extra writable roots (e.g. the memory directory) by prepending
	// an allow rule so it wins over the implicit ask-fallthrough for out-of-CWD
	// paths. Prepending also means it precedes the secret-path denies, which is
	// fine: these roots are octo-managed dirs, not places secrets live.
	for _, root := range allowWriteRoots {
		if root == "" {
			continue
		}
		allow := Rule{Decision: Allow, Path: []string{root, root + "/**"}}
		rules["write_file"] = append([]Rule{allow}, rules["write_file"]...)
		rules["edit_file"] = append([]Rule{allow}, rules["edit_file"]...)
	}

	return &Engine{
		rules:    rules,
		mode:     mode,
		cwd:      cwd,
		remember: map[string]Decision{},
	}, nil
}

// Check evaluates the rules for one tool invocation. Returns Allow,
// Deny, or Ask.
//
// Resolution order:
//  1. Session-remembered decision (set by Remember) short-circuits.
//  2. Rules for the tool are scanned in order; first match wins.
//  3. No match → implicit Ask.
//  4. Mode adjustment: ModeAutoApprove turns Ask into Allow.
func (e *Engine) Check(toolName string, input map[string]any) Decision {
	sig := signature(toolName, input)

	e.mu.Lock()
	cached, ok := e.remember[sig]
	e.mu.Unlock()
	if ok {
		return e.applyMode(cached)
	}

	d := Ask // implicit default
	for _, r := range e.rules[toolName] {
		if e.matches(toolName, r, input) {
			d = r.Decision
			break
		}
	}
	return e.applyMode(d)
}

// Remember stores a session-level decision so a subsequent Check for the
// same (toolName, input-signature) pair short-circuits. Use this when the
// user answers "always allow this turn / this session".
func (e *Engine) Remember(toolName string, input map[string]any, decision Decision) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.remember[signature(toolName, input)] = decision
}

// Mode returns the engine's current mode (for callers that need to branch
// on it, e.g. to decide whether to prompt the user).
func (e *Engine) GetMode() Mode { return e.mode }

// SetMode changes the engine's mode at runtime (e.g. TUI shortcut).
func (e *Engine) SetMode(mode Mode) { e.mode = mode }

// DenialReason returns a human-readable explanation for why a tool call
// was denied. Useful for surfacing to the LLM so it knows the failure
// was a policy denial, not a tool malfunction.
func (e *Engine) DenialReason(toolName string, input map[string]any) string {
	for _, r := range e.rules[toolName] {
		if e.matches(toolName, r, input) {
			return formatRuleReason(toolName, r)
		}
	}
	return fmt.Sprintf("permission_denied: %s — user declined the interactive prompt.", toolName)
}

// applyMode adjusts Ask decisions based on the engine mode:
//   - ModeAutoApprove: Ask → Allow
//   - ModeInteractive: Ask passes through unchanged
func (e *Engine) applyMode(d Decision) Decision {
	if e.mode == ModeAutoApprove && d == Ask {
		return Allow
	}
	return d
}

// matches reports whether rule r applies to the tool invocation. The
// matching axis (pattern / hostname / path) is chosen by which field of r
// is populated.
func (e *Engine) matches(toolName string, r Rule, input map[string]any) bool {
	switch {
	case len(r.Hostname) > 0:
		// web_fetch input["url"]
		raw, _ := input["url"].(string)
		host := parseHost(raw)
		if host == "" {
			return false
		}
		for _, pat := range r.Hostname {
			if globMatch(pat, host) {
				return true
			}
		}
		return false

	case len(r.Path) > 0:
		// write_file / edit_file / read_file input["path"]
		raw, _ := input["path"].(string)
		if raw == "" {
			return false
		}
		abs := absPath(raw, e.cwd)
		for _, pat := range r.Path {
			expanded := expandCWD(pat, e.cwd)
			if pathMatch(expanded, abs) {
				return true
			}
		}
		return false

	default:
		// Empty pattern matches unconditionally (used for "default
		// allow / default deny" entries on pattern-free tools).
		if r.Pattern == "" {
			return true
		}
		// terminal input["command"], otherwise stringified input.
		cmd, _ := input["command"].(string)
		if cmd == "" {
			// Fall back to substring search across all string fields,
			// so a rule like { pattern: ".env" } on a hypothetical tool
			// that exposes the file via input["file"] still works.
			cmd = stringifyInput(input)
		}
		return patternMatches(cmd, r.Pattern)
	}
}

// patternMatches reports whether cmd triggers a terminal pattern rule. The
// documented semantics are a case-sensitive substring match, with one
// refinement: a pattern ending in a filesystem-root marker (`/` or `~`) only
// matches when that marker sits at an argument boundary — i.e. the command
// targets root/home itself, not a path beneath it.
//
// Without this, `deny: rm -rf /` (intended to block a root wipe) is a literal
// substring of every absolute-path delete like `rm -rf /Users/me/project`,
// hard-denying legitimate operations that should instead fall through to the
// `ask: rm -rf` rule. The boundary check keeps `rm -rf /`, `rm -rf /*`, and
// `rm -rf ~` blocked while letting a delete of a specific subpath ask.
func patternMatches(cmd, pattern string) bool {
	if pattern == "" {
		return true
	}
	if last := pattern[len(pattern)-1]; last != '/' && last != '~' {
		return strings.Contains(cmd, pattern)
	}
	// Boundary-anchored: the character after a match must terminate the
	// argument (end of command, whitespace, or a `*` glob), otherwise the
	// match is a longer path under root/home and the rule does not apply.
	from := 0
	for {
		i := strings.Index(cmd[from:], pattern)
		if i < 0 {
			return false
		}
		end := from + i + len(pattern)
		if end >= len(cmd) || isArgBoundary(cmd[end]) {
			return true
		}
		from += i + 1
	}
}

// isArgBoundary reports whether c ends a shell argument for the purposes of
// patternMatches' root/home anchoring.
func isArgBoundary(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '*'
}

// signature returns a stable hash of (tool, input) for the remember
// cache. Two calls with the same logical input hit the same cache slot
// regardless of map iteration order, since json.Marshal sorts keys.
func signature(toolName string, input map[string]any) string {
	b, _ := json.Marshal(input)
	h := sha256.Sum256(append([]byte(toolName+"|"), b...))
	return hex.EncodeToString(h[:16])
}

// parseHost extracts the lowercased host from a URL. Returns "" for
// unparseable inputs.
func parseHost(raw string) string {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	host := u.Hostname()
	return strings.ToLower(host)
}

// globMatch tests pattern against name. `*` matches any sequence of
// characters (including dots) and `?` matches a single character. Same
// semantics filesystem globs use, so users don't have to learn a new
// dialect for permissions.yml.
//
// Note that `*` in a hostname rule like `*.github.com` therefore matches
// both `raw.github.com` and `a.b.github.com`. That's acceptable for our
// allow-list use case: the suffix anchor (`.github.com`) is still
// enforced, so `foo.github.com.attacker.example` does NOT match.
func globMatch(pattern, name string) bool {
	if pattern == name {
		return true
	}
	return globMatchStateMachine(pattern, name)
}

func globMatchStateMachine(pattern, name string) bool {
	pi, ni := 0, 0
	starPi, starNi := -1, 0
	for ni < len(name) {
		switch {
		case pi < len(pattern) && (pattern[pi] == name[ni] || pattern[pi] == '?'):
			pi++
			ni++
		case pi < len(pattern) && pattern[pi] == '*':
			starPi = pi
			starNi = ni
			pi++
		case starPi != -1:
			pi = starPi + 1
			starNi++
			ni = starNi
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

// pathMatch reports whether path is selected by glob. Supports `**` for
// recursive directory matching (so `$CWD/**` matches any file under the
// working dir). Single `*` matches within a single path component.
func pathMatch(glob, path string) bool {
	// Fast path: exact equality.
	if glob == path {
		return true
	}
	// Translate `**` to a sentinel, then split on `/`, then re-substitute.
	parts := strings.Split(glob, "/")
	pathParts := strings.Split(path, "/")
	return matchParts(parts, pathParts)
}

func matchParts(pattern, path []string) bool {
	switch {
	case len(pattern) == 0:
		return len(path) == 0
	case pattern[0] == "**":
		// `**` consumes zero or more path components.
		for i := 0; i <= len(path); i++ {
			if matchParts(pattern[1:], path[i:]) {
				return true
			}
		}
		return false
	case len(path) == 0:
		// Pattern still has parts (and the first isn't **) — no match.
		return false
	default:
		if !globMatchStateMachine(pattern[0], path[0]) {
			return false
		}
		return matchParts(pattern[1:], path[1:])
	}
}

// expandCWD replaces a leading `$CWD` with the engine's working directory.
func expandCWD(pat, cwd string) string {
	if strings.HasPrefix(pat, "$CWD") {
		return cwd + pat[len("$CWD"):]
	}
	return pat
}

// absPath resolves rel against cwd if it isn't already absolute. Output
// is always slash-separated regardless of host OS, so rule patterns
// (which are always written with `/`) match consistently on Windows.
//
// Symlink expansion is intentionally NOT performed — we want rules to
// match the path the user/LLM typed, not its canonical form, so a rule
// like `deny: /etc/**` blocks an attempt to write `/etc/passwd` via a
// symlink-traversal trick (the deny still applies on the literal path,
// even though the underlying file is elsewhere).
func absPath(rel, cwd string) string {
	rel = filepath.ToSlash(rel)
	cwd = filepath.ToSlash(cwd)
	// Windows drive-letter absolute (`C:/...`).
	if len(rel) >= 3 && rel[1] == ':' && rel[2] == '/' {
		return path.Clean(rel)
	}
	if path.IsAbs(rel) {
		return path.Clean(rel)
	}
	return path.Clean(cwd + "/" + rel)
}

// stringifyInput renders input map values as a single space-joined
// string. Used as a fallback for tools without a single canonical input
// field.
func stringifyInput(input map[string]any) string {
	var parts []string
	for _, v := range input {
		parts = append(parts, fmt.Sprintf("%v", v))
	}
	return strings.Join(parts, " ")
}

// formatRuleReason produces a short, structured-enough message that an
// LLM can read it and explain to its user why the call was rejected.
func formatRuleReason(toolName string, r Rule) string {
	switch r.Decision {
	case Deny:
		switch {
		case r.Pattern != "":
			return fmt.Sprintf("permission_denied: %s matched deny rule (pattern: %q). "+
				"This operation is blocked by policy.", toolName, r.Pattern)
		case len(r.Hostname) > 0:
			return fmt.Sprintf("permission_denied: %s matched deny rule (hostname: %v). "+
				"This host is blocked by policy.", toolName, r.Hostname)
		case len(r.Path) > 0:
			return fmt.Sprintf("permission_denied: %s matched deny rule (path: %v). "+
				"This path is blocked by policy.", toolName, r.Path)
		}
	case Ask:
		return fmt.Sprintf("permission_denied: %s matched ask rule but caller is non-interactive. "+
			"Add an explicit allow rule to ~/.octo/permissions.yml.", toolName)
	}
	return fmt.Sprintf("permission_denied: %s rejected.", toolName)
}

// ─── YAML loading ─────────────────────────────────────────────────────────

type rawClause struct {
	Pattern  string   `yaml:"pattern"`
	Hostname []string `yaml:"hostname"`
	Path     []string `yaml:"path"`
}

type rawRule struct {
	Allow *rawClause `yaml:"allow"`
	Deny  *rawClause `yaml:"deny"`
	Ask   *rawClause `yaml:"ask"`
}

// loadRules parses YAML bytes into a RuleSet. The YAML schema is the one
// used in defaults.yml: top-level keys are tool names, values are lists of
// { allow|deny|ask: { pattern|hostname|path: ... } } entries.
func loadRules(data []byte) (RuleSet, error) {
	var raw map[string][]rawRule
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("yaml: %w", err)
	}

	out := RuleSet{}
	for tool, rules := range raw {
		for i, rr := range rules {
			rule, ok := convertRawRule(rr)
			if !ok {
				return nil, fmt.Errorf("tool %q rule %d: must have exactly one of allow/deny/ask",
					tool, i)
			}
			out[tool] = append(out[tool], rule)
		}
	}
	return out, nil
}

func convertRawRule(rr rawRule) (Rule, bool) {
	set := 0
	out := Rule{}
	if rr.Allow != nil {
		set++
		out.Decision = Allow
		out.Pattern, out.Hostname, out.Path = rr.Allow.Pattern, rr.Allow.Hostname, rr.Allow.Path
	}
	if rr.Deny != nil {
		set++
		out.Decision = Deny
		out.Pattern, out.Hostname, out.Path = rr.Deny.Pattern, rr.Deny.Hostname, rr.Deny.Path
	}
	if rr.Ask != nil {
		set++
		out.Decision = Ask
		out.Pattern, out.Hostname, out.Path = rr.Ask.Pattern, rr.Ask.Hostname, rr.Ask.Path
	}
	return out, set == 1
}
