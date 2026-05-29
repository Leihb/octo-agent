package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/config"
	"github.com/Leihb/octo-agent/internal/permission"
	"github.com/Leihb/octo-agent/internal/prompt"
	"github.com/Leihb/octo-agent/internal/tools"
)

// initInstruction is the prompt that drives `.octorules` generation, shared by
// the `octo init` subcommand and the REPL `/init` command. It tells the agent
// to investigate the repo with its tools before writing, and to keep the
// output terse — the file is loaded into the system prompt every session.
const initInstruction = `Analyze this repository and create (or update) a ` + "`.octorules`" + ` file at its root to help an AI coding agent work here effectively.

Investigate with your tools BEFORE writing:
- List the top-level layout and the important directories.
- Read the build/dependency manifests (go.mod, package.json, Makefile, etc.), the README, and any CI config.
- Determine the language(s), how to build / test / run / lint, and the main entry points.
- Note conventions that aren't obvious from a single file: architecture, naming, testing approach, branch/PR workflow.

Then write ` + "`.octorules`" + ` with concise, factual content — short sections, real commands and real paths, no marketing or filler. If ` + "`.octorules`" + ` already exists, read it first and improve it rather than discarding useful content. Keep it tight: every line is sent to the model on every turn.`

// runInit handles `octo init [flags]`: a one-shot agentic run that generates
// or updates the project's .octorules. It mirrors chat's tool setup but is
// non-interactive and exits when the file is written.
func runInit(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	providerName := fs.String("provider", "", "Provider: anthropic | openai (default from `octo config`, else anthropic)")
	model := fs.String("model", "", "Model name (defaults to the provider's cheapest reasoning model)")
	plain := fs.Bool("plain", false, "Render tool events as one-line ↳ status lines instead of rich diff cards")
	permMode := fs.String("permission-mode", "strict", "Tool permission handling: interactive | strict")
	useSandbox := fs.Bool("sandbox", false, "Confine commands to the project dir + tmp with no network (OS-enforced; macOS/Linux). Fails closed if unavailable.")
	sandboxAllowNet := fs.Bool("sandbox-allow-net", false, "Under --sandbox, permit network access (default: denied)")
	var sandboxWrite, sandboxRead stringList
	fs.Var(&sandboxWrite, "sandbox-write", "Under --sandbox, an extra writable directory (repeatable)")
	fs.Var(&sandboxRead, "sandbox-read", "Under --sandbox, an extra read-only directory (repeatable)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *permMode != string(permission.ModeInteractive) && *permMode != string(permission.ModeStrict) {
		fmt.Fprintf(stderr, "octo init: invalid --permission-mode %q (want 'interactive' or 'strict')\n", *permMode)
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(stderr, "octo init: %v\n", err)
		fmt.Fprintln(stderr, "Run `octo config` to rewrite ~/.octo/config.json.")
		return 1
	}
	provName, resolvedModel, ok := resolveProviderModel(*providerName, *model, cfg)
	if !ok {
		fmt.Fprintf(stderr, "octo init: unknown provider %q (use 'anthropic' or 'openai')\n", provName)
		return 2
	}

	prov, err := buildProvider(provName, cfg, stderr)
	if err != nil {
		return 1
	}

	cwd, _ := os.Getwd()
	env := buildEnvContext(cwd)

	if *useSandbox {
		opts := sandboxOpts{allowNet: *sandboxAllowNet, writeRoots: sandboxWrite, readRoots: sandboxRead}
		if err := activateSandbox(cwd, opts, stderr); err != nil {
			return 1
		}
	}

	a := agent.New(providerSender{p: prov, cacheKey: newCacheKey()}, resolvedModel)
	a.System = prompt.Compose("", cwd, env, "", "") // init is a one-shot task; no skills/memory

	engine, err := permission.New(permissionConfigPath(), cwd, resolvePermissionMode(*permMode))
	if err != nil {
		fmt.Fprintf(stderr, "octo init: permission config: %v\n", err)
		return 1
	}
	initView := newPlainView(newScannerLineReader(stdin, stdout), stdout, stderr, verbosityNormal, *plain)
	a.Gate = &cliPermissionGate{engine: engine, ask: initView}

	fmt.Fprintln(stdout, "Analyzing the repository to generate .octorules…")
	handler := replToolEventHandler(stdout, *plain)
	_, err = a.RunStream(context.Background(), initInstruction, tools.DefaultTools(), tools.NewDefaultRegistry(), handler)
	tools.KillAllBackground()
	fmt.Fprintln(stdout)
	if err != nil {
		fmt.Fprintf(stderr, "octo init: %v\n", err)
		return 1
	}
	return 0
}
