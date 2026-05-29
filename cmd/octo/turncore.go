package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Leihb/octo-agent/internal/agent"
	"github.com/Leihb/octo-agent/internal/hooks"
)

// ViewSink is the render-decoupling seam between the turn orchestration core
// (runTurn) and whatever is presenting it — today the plain-text REPL
// (plainView), later a bubbletea TUI. runTurn drives a ViewSink and never
// touches a terminal directly, so the same orchestration (memory nudge,
// pre/post hooks, the streaming RunStream call) backs both the TTY and the
// headless paths. The embedded userPrompter (Ask) lets the permission gate
// and ask_user_question request a structured answer through the same seam —
// stdin line in plainView, modal in the TUI.
type ViewSink interface {
	userPrompter

	// TurnStarted fires once before the model is called, so the view can
	// arm per-turn state (spinner, fresh event-rendering closure).
	TurnStarted()

	// Emit receives every streaming AgentEvent from the run, in order. It is
	// invoked synchronously from the agent loop — a blocking Emit blocks the
	// loop (same contract as agent.EventHandler).
	Emit(ev agent.AgentEvent)

	// TurnEnded fires once after the run returns, carrying the final reply and
	// any error (including context.Canceled for an interrupt), so the view can
	// tear down per-turn state and render the outcome (cache line, ^C, error).
	TurnEnded(reply agent.Reply, err error)

	// Notice surfaces an out-of-band message not tied to the model stream
	// (e.g. a pre/post-turn hook failure). Never blocks the turn.
	Notice(msg string)
}

// runTurn executes one user turn: it applies the memory nudge and the pre-turn
// hook to build the model-facing input, streams the turn through the agent
// loop (RunStream subsumes the no-tools case via its internal TurnStream
// fallback), then fires the post-turn hook. All presentation flows through
// sink; the caller owns input reading, slash-command dispatch, the turn's
// cancellable context, and the save/loop decision based on the returned error.
func runTurn(ctx context.Context, a *agent.Agent, cfg replConfig, sink ViewSink, line string) (agent.Reply, error) {
	// Memory-hygiene nudge: appended when both cross-session memory and tools
	// are active, reminding the model to scan for durable signals at the
	// decision point. Gated on tools because the nudge asks the model to call
	// a tool.
	turnInput := line
	if cfg.memStore != nil && len(cfg.tools) > 0 {
		turnInput = appendMemoryNudge(turnInput)
	}

	// Pre-turn hook: feed the raw user input to an external retrieval layer;
	// whatever it returns is folded into the user message. Hook errors are
	// surfaced but never block the turn.
	if cfg.hooks.Configured() {
		extra, herr := cfg.hooks.Pre(ctx, line)
		if herr != nil {
			sink.Notice(fmt.Sprintf("↳ pre-turn hook: %v", herr))
		}
		turnInput = hooks.InjectContext(turnInput, extra)
	}

	sink.TurnStarted()

	// RunStream owns the streaming + agentic tool loop. With no tools it falls
	// back internally to TurnStream, adapting text deltas into EventTextDelta
	// and firing a terminal EventTurnDone — so a single call path covers both
	// the tool-enabled and plain-chat turns and the sink sees a uniform event
	// stream regardless.
	reply, err := a.RunStream(ctx, turnInput, cfg.tools, cfg.executor, sink.Emit)

	sink.TurnEnded(reply, err)

	// Post-turn hook: fire-and-forget the finished turn at the retain side.
	// Runs only on success so interrupts/errors don't pollute the index. Uses
	// a fresh context so an interrupt of the turn doesn't cancel retention.
	if err == nil && cfg.hooks.Configured() {
		if herr := cfg.hooks.Post(context.Background(), line, reply.Content); herr != nil {
			sink.Notice(fmt.Sprintf("↳ post-turn hook: %v", herr))
		}
	}
	return reply, err
}

// plainView is the plain-text ViewSink: the rendering that the synchronous,
// line-based REPL used inline before turn orchestration was extracted. It
// backs both the non-tty path and the --no-tui fallback. Output is byte-for-
// byte the same as the pre-extraction REPL.
type plainView struct {
	out       io.Writer
	errOut    io.Writer
	reader    lineReader // for Ask prompts; nil → Ask auto-cancels
	verbosity verbosity
	plain     bool

	// Per-turn state, (re)armed in TurnStarted.
	spin  *spinner
	inner func(agent.AgentEvent)
}

func newPlainView(reader lineReader, out, errOut io.Writer, v verbosity, plain bool) *plainView {
	return &plainView{
		out:       out,
		errOut:    errOut,
		reader:    reader,
		verbosity: v,
		plain:     plain,
	}
}

func (v *plainView) TurnStarted() {
	// Fresh event-rendering closure per turn — it carries per-turn state
	// (started-at times, prevWasText, input dots).
	v.inner = replToolEventHandler(v.out, v.plain)
	// Spinner during the "thinking, nothing on screen yet" pause. Suppressed
	// in quiet mode (spin stays nil; spinner.Stop is nil-safe). 250ms grace so
	// a fast reply doesn't blink it.
	if v.verbosity.quiet() {
		v.spin = nil
		return
	}
	v.spin = newSpinner(v.out, "thinking…")
	v.spin.Start(250 * time.Millisecond)
}

func (v *plainView) Emit(ev agent.AgentEvent) {
	v.spin.Stop() // idempotent + nil-safe; first event of any kind clears it
	v.inner(ev)
}

func (v *plainView) TurnEnded(reply agent.Reply, err error) {
	v.spin.Stop() // belt-and-braces in case the turn produced zero events
	switch {
	case errors.Is(err, context.Canceled):
		// Ctrl-C: the agent already finalized history into a well-formed state.
		fmt.Fprintln(v.out, "\n^C interrupted")
	case err != nil:
		fmt.Fprintf(v.errOut, "\nerror: %v\n", err)
	default:
		fmt.Fprintln(v.out) // newline after the streamed reply
		// Surface cache activity per turn so the win is visible. Suppressed in
		// quiet mode; always-on in verbose (so "0 read, 0 write" is a useful
		// debugging signal there).
		if !v.verbosity.quiet() {
			show := reply.CacheReadTokens > 0 || reply.CacheWriteTokens > 0
			if v.verbosity.verbose() {
				show = true
			}
			if show {
				fmt.Fprintf(v.out, "  ⓘ cache: %d read, %d write (in %d / out %d)\n",
					reply.CacheReadTokens, reply.CacheWriteTokens, reply.InputTokens, reply.OutputTokens)
			}
		}
	}
}

func (v *plainView) Notice(msg string) {
	fmt.Fprintln(v.errOut, msg)
}

// Ask renders a structured prompt to stdout and reads the answer from the
// shared line reader — the synchronous behaviour the gate and asker used
// before the seam existed. With no reader (e.g. piped input exhausted) every
// prompt auto-cancels / denies.
func (v *plainView) Ask(_ context.Context, p UserPrompt) (UserResponse, error) {
	switch p.Kind {
	case KindPermission:
		return v.askPermission(p), nil
	case KindQuestion:
		return v.askQuestion(p), nil
	default:
		return UserResponse{Cancelled: true}, nil
	}
}

// askPermission prompts: y/yes → allow once; a/always → allow for the session;
// anything else (incl. empty / N / no reader) → deny.
func (v *plainView) askPermission(p UserPrompt) UserResponse {
	fmt.Fprintf(v.out, "\n⚠ permission: %s wants to run\n", p.ToolName)
	fmt.Fprintf(v.out, "    %s\n", summariseInput(p.ToolInput))

	answer := ""
	if v.reader != nil {
		if raw, ok := v.reader.ReadLine("  allow? [y]es / [a]lways this session / [N]o: "); ok {
			answer = strings.ToLower(strings.TrimSpace(raw))
		}
	}
	switch answer {
	case "y", "yes":
		return UserResponse{Allow: true}
	case "a", "always":
		return UserResponse{Allow: true, Always: true}
	default:
		return UserResponse{Allow: false}
	}
}

// askQuestion renders the ask_user_question card and parses the selection.
func (v *plainView) askQuestion(p UserPrompt) UserResponse {
	if v.reader == nil {
		return UserResponse{Cancelled: true}
	}

	prompt := v.printQuestion(p)
	raw, ok := v.reader.ReadLine(prompt)
	if !ok {
		// EOF / empty → cancel; surfacing "(cancelled)" lets the model retry
		// or pick a default itself.
		fmt.Fprintln(v.out)
		return UserResponse{Cancelled: true}
	}
	choice := strings.TrimSpace(raw)
	if choice == "" {
		return UserResponse{Cancelled: true}
	}

	otherIdx := len(p.Options) + 1
	indices, parseErr := parseSelection(choice, otherIdx, p.MultiSelect)
	if parseErr != nil {
		fmt.Fprintf(v.out, "  (couldn't parse %q, treating as cancellation)\n", choice)
		return UserResponse{Cancelled: true}
	}

	var (
		picks     []string
		wantOther bool
	)
	for _, idx := range indices {
		if idx == otherIdx {
			wantOther = true
			continue
		}
		if idx < 1 || idx > len(p.Options) {
			continue
		}
		picks = append(picks, p.Options[idx-1])
	}

	if wantOther {
		text, ok := v.reader.ReadLine("  Other (free text): ")
		if !ok || strings.TrimSpace(text) == "" {
			return UserResponse{Cancelled: true}
		}
		return UserResponse{Custom: strings.TrimSpace(text)}
	}
	if len(picks) == 0 {
		return UserResponse{Cancelled: true}
	}
	return UserResponse{Choices: picks}
}

// printQuestion writes the multi-line question card and returns the final
// inline "Select [...]: " prompt — passed to ReadLine so readline renders it
// in place.
func (v *plainView) printQuestion(p UserPrompt) string {
	header := p.Header
	if header == "" {
		header = "question"
	}
	fmt.Fprintf(v.out, "\n[ask_user_question · %s]\n", header)
	fmt.Fprintf(v.out, "  %s\n", p.Question)
	otherIdx := len(p.Options) + 1
	for i, opt := range p.Options {
		fmt.Fprintf(v.out, "    %d) %s\n", i+1, opt)
	}
	fmt.Fprintf(v.out, "    %d) Other (free text)\n", otherIdx)

	hint := fmt.Sprintf("[1-%d]", otherIdx)
	if p.MultiSelect {
		hint = "[comma-separated, e.g. 1,3]"
	}
	return fmt.Sprintf("  Select %s: ", hint)
}
