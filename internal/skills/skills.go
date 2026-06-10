// Package skills discovers Claude Code-compatible skills from disk and exposes
// them to the agent in two layers: a one-line manifest injected into the
// session-start system prompt (see RenderManifest), and full SKILL.md bodies
// loaded on demand through the `skill` tool. This progressive-disclosure split
// keeps the cached prompt prefix small while still letting the model pull in a
// skill's full instructions when a task matches.
//
// A skill is a directory containing a SKILL.md file with YAML frontmatter:
//
//	~/.octo/skills/<name>/SKILL.md      (user-level, cross-project)
//	<cwd>/.octo/skills/<name>/SKILL.md  (project-level, takes precedence)
//
// The directory name is the authoritative trigger name (matching Claude Code);
// the frontmatter `name` is display-only. The format is identical to
// ~/.claude/skills/*/SKILL.md, so users can symlink that directory in directly.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Leihb/octo-agent/internal/trash"
	"gopkg.in/yaml.v3"
)

// SkillFile is the per-skill instruction file Discover looks for in each skill
// directory.
const SkillFile = "SKILL.md"

// Skill is one discovered skill. Body is the SKILL.md content after the
// frontmatter — the instructions handed to the model on demand.
type Skill struct {
	Name        string // directory name; the /<name> trigger
	Description string // frontmatter description; the L1 manifest + trigger cue
	Body        string // SKILL.md body after frontmatter
	Dir         string // absolute path of the skill directory
	Source      string // "project" | "user" (display only, for --list-skills)
}

// Registry holds the skills discovered for a session, indexed by name. It is
// safe for concurrent use (sub-agents share one registry and call the skill
// tool from separate goroutines). cwd is remembered so Reload can re-scan the
// same roots a session was discovered from.
type Registry struct {
	mu       sync.RWMutex
	skills   map[string]Skill
	disabled map[string]bool // names toggled off by the user
	cwd      string
}

// userSkillsRoot returns ~/.octo/skills, or "" when the home dir can't be
// resolved. It's a var so tests can point discovery at a temp directory.
var userSkillsRoot = func() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".octo", "skills")
}

// Discover scans the user-level then project-level skill roots and returns a
// Registry. Project skills override user skills of the same name (the project
// root is scanned last, overwriting the map entry). A missing root is not an
// error — most environments won't have both.
func Discover(cwd string) *Registry {
	r := &Registry{skills: make(map[string]Skill), cwd: cwd}
	// Lowest precedence first; scanRoot overwrites by name, so user then
	// project win. Default skills (shipped with the binary, materialized to
	// ~/.octo/skills-default) are the floor — a user overrides one by dropping
	// a same-named skill in ~/.octo/skills.
	if root := defaultSkillsRoot(); root != "" {
		r.scanRoot(root, "default")
	}
	if root := userSkillsRoot(); root != "" {
		r.scanRoot(root, "user")
	}
	if cwd != "" {
		r.scanRoot(filepath.Join(cwd, ".octo", "skills"), "project")
	}
	return r
}

// scanRoot reads one skills root: each immediate subdirectory holding a
// readable, well-formed SKILL.md becomes a Skill. Anything malformed (missing
// SKILL.md, no frontmatter, missing name/description) is skipped silently —
// discovery never fails the session over one bad skill.
func (r *Registry) scanRoot(root, source string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return // missing/unreadable root: nothing to add
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		dir := filepath.Join(root, name)
		b, err := os.ReadFile(filepath.Join(dir, SkillFile))
		if err != nil {
			continue
		}
		desc, body, ok := parse(b)
		if !ok || desc == "" {
			continue
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			abs = dir
		}
		r.skills[name] = Skill{
			Name:        name,
			Description: desc,
			Body:        strings.TrimSpace(body),
			Dir:         abs,
			Source:      source,
		}
	}
}

// frontmatter is the subset of SKILL.md frontmatter we consume. yaml.v3 ignores
// every other key (allowed-tools, license, nested metadata blocks, …), which is
// exactly what Claude Code compatibility needs.
type frontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// parse splits a SKILL.md into its frontmatter description and body. It expects
// the file to open with a `---` line and contain a closing `---` line; the YAML
// between them is unmarshalled for the description. ok is false when the file
// has no frontmatter fence or the YAML doesn't parse.
func parse(b []byte) (desc, body string, ok bool) {
	front, body, ok := splitFrontmatter(string(b))
	if !ok {
		return "", "", false
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(front), &fm); err != nil {
		return "", "", false
	}
	return strings.TrimSpace(fm.Description), body, true
}

// splitFrontmatter returns the text between the opening and closing `---`
// fences and everything after the closing fence. ok is false unless the first
// non-empty content is a `---` line with a matching closing fence.
func splitFrontmatter(content string) (front, body string, ok bool) {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", false
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[1:i], "\n"), strings.Join(lines[i+1:], "\n"), true
		}
	}
	return "", "", false
}

// Get returns the skill with the given name. Disabled skills are treated as
// non-existent so the model can't load them.
func (r *Registry) Get(name string) (Skill, bool) {
	if r == nil {
		return Skill{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.skills[name]
	if !ok || r.disabled[name] {
		return Skill{}, false
	}
	return s, true
}

// Reload re-scans the skill roots in place, picking up skills added, removed, or
// edited since the registry was first discovered. The system-prompt manifest is
// intentionally NOT refreshed (recomputing it mid-session would change the
// cached prompt prefix); this only refreshes what the `skill` tool can load, so
// a skill dropped into ~/.octo/skills mid-session becomes loadable without a
// restart. Safe to call concurrently with Get/List/Len. The disabled set is
// preserved across reloads.
func (r *Registry) Reload() {
	if r == nil {
		return
	}
	fresh := Discover(r.cwd) // build off the lock, then swap atomically
	r.mu.Lock()
	r.skills = fresh.skills
	r.mu.Unlock()
}

// Len reports how many enabled skills were discovered (disabled skills are
// excluded so the system-prompt manifest and tool advertisement stay accurate).
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for name := range r.skills {
		if !r.disabled[name] {
			n++
		}
	}
	return n
}

// List returns enabled skills only, in a stable order: project skills first,
// then user skills, each group sorted by name.
func (r *Registry) List() []Skill {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	out := make([]Skill, 0, len(r.skills))
	for _, s := range r.skills {
		if !r.disabled[s.Name] {
			out = append(out, s)
		}
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source == "project" // project before user
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// All returns every discovered skill, including disabled ones, in the same
// order as List. Use this when the caller needs the full catalog (e.g. a UI
// that shows every skill with an on/off toggle).
func (r *Registry) All() []Skill {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	out := make([]Skill, 0, len(r.skills))
	for _, s := range r.skills {
		out = append(out, s)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source == "project" // project before user
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// SetDisabled replaces the set of disabled skill names. The registry filters
// them out of List, Len, Get and RenderManifest automatically.
func (r *Registry) SetDisabled(names []string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.disabled = make(map[string]bool, len(names))
	for _, n := range names {
		r.disabled[n] = true
	}
}

// IsEnabled reports whether a skill by name exists and is not disabled.
func (r *Registry) IsEnabled(name string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, exists := r.skills[name]
	if !exists {
		return false
	}
	return !r.disabled[name]
}

// Delete removes a skill from the registry and its on-disk directory.
// System (source=default) skills cannot be deleted.
func (r *Registry) Delete(name string) error {
	if r == nil {
		return fmt.Errorf("skill %q not found", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	s, exists := r.skills[name]
	if !exists {
		return fmt.Errorf("skill %q not found", name)
	}
	if s.Source == "default" {
		return fmt.Errorf("cannot delete system skill %q", name)
	}

	// Remove from registry first
	delete(r.skills, name)
	delete(r.disabled, name)

	// Move to trash before permanently deleting.
	if s.Dir != "" {
		projDir := filepath.Dir(s.Dir)
		if err := trash.Move(s.Dir, projDir); err != nil {
			return fmt.Errorf("trash skill directory %s: %w", s.Dir, err)
		}
	}
	return nil
}

// RenderManifest builds the L1 manifest injected into the system prompt: each
// skill's name and description, plus a note on how to load the full body.
// Returns "" for a nil/empty registry so the caller can skip the prompt layer.
func RenderManifest(r *Registry) string {
	if r.Len() == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Available skills\n\n")
	b.WriteString("When a task matches a skill's description, call the `skill` tool with its " +
		"name to load the full instructions before acting. Don't guess the instructions " +
		"from the one-line description. The user can also trigger one directly by typing /<name>.\n\n")
	for _, s := range r.List() {
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
	}
	return strings.TrimRight(b.String(), "\n")
}

// ccCompatNote bridges skills authored for Claude Code — the ecosystem most
// SKILL.md files are written for — onto octo's toolbelt at load time. A static
// note beats rewriting the skill on install: rewriting can't tell a tool
// reference from a string inside a bundled script, and for source-available
// skills a rewrite is a derivative work their license forbids. Injected for
// user/project skills only; octo-native defaults don't need it.
const ccCompatNote = "This skill may have been written for Claude Code. In this environment, " +
	"map tool references accordingly: Bash → terminal; Read/Write/Edit → read_file / " +
	"write_file / edit_file; Grep/Glob → grep / glob; Task/Agent → sub_agent; " +
	"WebFetch/WebSearch → web_fetch / web_search. There is no persistent working " +
	"directory across terminal calls — use absolute paths, or chain `cd … && …` " +
	"inside one command. If the skill depends on a Claude Code feature with no " +
	"equivalent here (hooks, plan mode, output styles), tell the user instead of " +
	"improvising."

// RenderSkill produces the text handed to the model when a skill is loaded —
// via the `skill` tool (model-initiated) or a /<name> trigger (user-initiated).
// It prefixes a one-line location header so the model can resolve files the
// SKILL.md references (scripts, templates, reference docs bundled in the skill
// directory) with its file tools, then the body, then any trailing user args.
// Without the header a relative reference like "see references/api.md" would be
// resolved against the project cwd and miss. Non-default skills additionally
// get the Claude Code → octo tool-name bridging note.
func RenderSkill(s Skill, args string) string {
	var b strings.Builder
	if s.Dir != "" {
		fmt.Fprintf(&b, "[skill %q — bundled files live in: %s\n", s.Name, s.Dir)
		b.WriteString("Resolve any paths this skill references (scripts, templates, reference docs) " +
			"against that directory and read them with your file tools.")
		if s.Source != "default" {
			b.WriteString("\n" + ccCompatNote)
		}
		b.WriteString("]\n\n")
	}
	b.WriteString(s.Body)
	if args != "" {
		b.WriteString("\n\nUser input: " + args)
	}
	return b.String()
}
