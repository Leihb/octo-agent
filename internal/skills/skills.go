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

// Registry holds the skills discovered for a session, indexed by name.
type Registry struct {
	skills map[string]Skill
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
	r := &Registry{skills: make(map[string]Skill)}
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

// Get returns the skill with the given name.
func (r *Registry) Get(name string) (Skill, bool) {
	if r == nil {
		return Skill{}, false
	}
	s, ok := r.skills[name]
	return s, ok
}

// Len reports how many skills were discovered.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	return len(r.skills)
}

// List returns all discovered skills in a stable order: project skills first,
// then user skills, each group sorted by name.
func (r *Registry) List() []Skill {
	if r == nil {
		return nil
	}
	out := make([]Skill, 0, len(r.skills))
	for _, s := range r.skills {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source == "project" // project before user
		}
		return out[i].Name < out[j].Name
	})
	return out
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
