package tools

import (
	"os/exec"
	"strings"
)

// toolchainProbes is the curated set of developer tools an agent reaches for
// most often. Each entry maps a single reported name to the candidate
// executables that satisfy it — `python` is satisfied by python3 or python,
// `pip` by pip3 or pip — so the model sees one stable name regardless of which
// variant is on PATH. Kept tight on purpose: a longer list is noise the model
// pays for on every turn.
var toolchainProbes = []struct {
	name string
	cmds []string
}{
	{"git", []string{"git"}},
	{"gh", []string{"gh"}},
	{"node", []string{"node"}},
	{"npm", []string{"npm"}},
	{"python", []string{"python3", "python"}},
	{"pip", []string{"pip3", "pip"}},
	{"uv", []string{"uv"}},
	{"go", []string{"go"}},
	{"docker", []string{"docker"}},
	{"make", []string{"make"}},
}

// DetectToolchain reports which curated developer tools resolve on the current
// PATH. Presence-only via exec.LookPath — a filesystem lookup, not a subprocess
// — so it is cheap enough to call on every context build, including the
// server's per-turn recompose. Versions are deliberately not probed; the agent
// runs `<tool> --version` on demand when it actually needs one. On Windows
// LookPath honours PATHEXT, so npm.cmd / npx.cmd resolve as expected.
func DetectToolchain() (present, missing []string) {
	for _, p := range toolchainProbes {
		found := false
		for _, c := range p.cmds {
			if _, err := exec.LookPath(c); err == nil {
				found = true
				break
			}
		}
		if found {
			present = append(present, p.name)
		} else {
			missing = append(missing, p.name)
		}
	}
	return present, missing
}

// ToolchainNote renders the detected/missing toolchain as a session-context
// block, pairing with ShellEnvNote (which says how to install on this platform)
// so the model knows both what is missing and how to add it. Returns "" only in
// the degenerate case where nothing was probed.
func ToolchainNote() string {
	present, missing := DetectToolchain()
	var b strings.Builder
	if len(present) > 0 {
		b.WriteString("- Detected tools on PATH: " + strings.Join(present, ", ") + ".\n")
	}
	if len(missing) > 0 {
		b.WriteString("- Not found (install on demand if a task needs one): " + strings.Join(missing, ", ") + ".\n")
	}
	return b.String()
}
