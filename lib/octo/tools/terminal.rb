# frozen_string_literal: true

require "pty"
require "securerandom"
require "fileutils"
require_relative "base"
require_relative "security"
require_relative "../utils/trash_directory"
require_relative "terminal/session_manager"
require_relative "terminal/output_cleaner"
require_relative "terminal/persistent_session"
require_relative "../background_task_registry"

module Octo
  module Tools
    # Unified terminal tool — the SINGLE entry point for running shell
    # commands. Replaces the former `shell` + `safe_shell` tools.
    #
    # === AI-facing contract
    #
    # Five call shapes, all on one tool:
    #
    #   1) Run a command, wait for it:
    #        terminal(command: "ls -la")
    #        → { exit_code: 0, output: "..." }
    #
    #   2) Run a command that is expected to keep running (dev servers,
    #      watchers, REPLs meant to stay open):
    #        terminal(command: "rails s", background: true)
    #      – collects ~2s of startup output, then:
    #      – if it crashed in those 2s → { exit_code: N, output: "..." }
    #      – if still alive           → { session_id: 7, state: "background",
    #                                     output: "Puma starting..." }
    #
    #   3) A previous call returned a session_id because the command
    #      blocked on input (sudo password, REPL, etc.). Answer it:
    #        terminal(session_id: 3, input: "mypass\n")
    #
    #   4) Poll a running session for new output without sending anything:
    #        terminal(session_id: 7, input: "")
    #
    #   5) Kill a stuck / no-longer-wanted session:
    #        terminal(session_id: 7, kill: true)
    #
    # === Response handshake
    #
    #   - Response has `exit_code` → command finished.
    #   - Response has `session_id` → command is still running;
    #     look at `state`: "waiting" means blocked on input,
    #     "background" means intentionally long-running.
    #
    # === Safety
    #
    # Every new `command` is routed through Octo::Tools::Security before
    # being handed to the shell. This:
    #   - Blocks sudo / pkill octo / eval / curl|bash / etc.
    #   - Rewrites `curl ... | bash` into "download & review".
    #   - Protects Gemfile / .env / .ssh / etc. from writes.
    # `rm` is additionally intercepted at runtime by a shell function
    # installed in each PTY session (see SAFE_RM_BASH): it moves files
    # into the per-project trash at $OCTO_TRASH_DIR instead of
    # deleting them. See trash_manager for list/restore.
    # `input` is NOT subject to these rules (it is a reply to an already-
    # running program, not a fresh command).
    class Terminal < Base
      self.tool_name = "terminal"
      self.tool_description = <<~DESC.strip
        Run shell commands via PTY. Safety: rm→trash, sudo blocked, secrets protected.

        Two modes:
          {command}                  DEFAULT — sync. Quick commands return {exit_code, output} immediately. Slow build/test/install commands are auto-routed to async by the harness (you get a handle, see below). Hits idle on an interactive prompt → also returns a handle.
          {command, async:true}      Async — never blocks. Use for a long task you intend to leave running (build, deploy, side quest). Returns a handle.

        Five operations on an existing handle:
          {handle_id}                            Query status. Returns {state, command, elapsed_seconds, output_file, exit_code (if exited)}.
          {handle_id, input:"y\n"}               Send input to the PTY + poll for new output. Use to answer prompts, drive REPLs, etc.
          {handle_id, kill:true}                 Terminate the underlying process and free the handle.
          Read(output_file)                      Read the raw PTY log file directly — fastest way to peek at progress mid-flight. (output_file path is in every handle response.)
          (wait for notification)                A <task-notification> with the same handle_id is pushed to your context when the task exits — you don't need to poll.

        Response shape:
          - sync completes → {exit_code, output, state:"exited"}
          - sync hits idle (prompt waiting) → {handle_id, state:"waiting", output, output_file}
          - async accepted → {accepted:true, handle_id, state:"running", output_file, startup_output}
          - handle query/input → {handle_id, state, ...} plus exit_code if state=="exited"

        If output exceeds the limit, `output` is truncated and `full_output_file`
        points at a file on disk — use terminal(command: "grep ... <path>") to search it.
        input supports byte escapes: \x03 Ctrl-C, \x04 Ctrl-D, \t Tab, \x1b Esc.

        GUIDANCE:
          - Default to sync. The harness recognises build/test/install patterns and
            auto-switches them to async — you don't need to think about it.
          - Use async:true when you want to fire off a long task and continue with
            unrelated work (you'll get a notification on completion). Also use it
            for dev servers, watchers, REPLs you need to control later — same flag,
            same handle.
          - Never poll a handle in a tight loop — wait for the notification, or
            Read(output_file) once if you really need to peek.
      DESC
      self.tool_category = "system"

      # agent_session_id is injected by the Agent that owns this tool instance.
      # It is NOT exposed in tool_parameters — AI agents cannot set it.
      attr_reader :agent_session_id

      def initialize(agent_session_id: nil)
        super()
        @agent_session_id = agent_session_id
      end

      self.tool_parameters = {
        type: "object",
        properties: {
          command:      { type: "string",  description: "Shell command to start. Mutually exclusive with handle_id." },
          async:        { type: "boolean", description: "Run async — return a handle immediately instead of blocking. Use for long tasks you intend to leave running, dev servers, REPLs, or any side-quest you'll pivot away from. Default false (sync, harness still auto-async's slow build/test/install patterns)." },
          handle_id:    { type: "string",  description: "Reference an existing async task or interactive session. Combine with input: to send input, with kill: to terminate, or use alone to query status. The handle_id is returned in every async / waiting-for-input response." },
          input:        { type: "string",  description: "Bytes to write to the PTY of the handle_id (usually ends with \\n). Also re-polls for new output. \"\" = poll only." },
          kill:         { type: "boolean", description: "Terminate the process referenced by handle_id and release the handle." },
          cwd:          { type: "string",  description: "Working dir for new command." },
          env:          { type: "object",  description: "Extra env vars for new command.", additionalProperties: { type: "string" } },
          max_duration: { type: "integer", description: "Hard ceiling (seconds) for an async task before the watcher kills it. Defaults to 7200 (2h). Raise for very long jobs (large docker build, full integration suite)." }
        }
      }

      # Hard ceiling on the raw `output:` string we send back to the LLM.
      # 4000 chars ≈ 1000 tokens — matches the value the legacy safe_shell
      # tool used, which was empirically tuned to keep tool-call turns cheap.
      # When real output exceeds this we SPILL the full cleaned text to a
      # dedicated overflow file and only return the first portion — see
      # OVERFLOW_PREVIEW_CHARS / spill_overflow_file below.
      MAX_LLM_OUTPUT_CHARS = 4_000
      # When output overflows, the preview we keep in-context is slightly
      # shorter than the hard ceiling so the "full output at: /tmp/..."
      # notice + path still fits under MAX_LLM_OUTPUT_CHARS.
      OVERFLOW_PREVIEW_CHARS = 3_800
      # Per-line cap applied at write-time (inside the cleaning pipeline).
      # Prevents a single minified JSON / CSS / JS blob from eating the
      # entire 4 KB budget in one go. 500 chars is long enough to preserve
      # real error messages (including stack frames) but short enough to
      # survive dozens of lines inside 4 KB.
      MAX_LINE_CHARS = 500
      # Max seconds we keep a single tool call blocked inside the shell.
      # Raised from 15s → 60s so long-running installs/builds (bundle install,
      # gem install, npm install, docker build, rails new, ...) produce far
      # fewer LLM round-trips: each poll replays the full context, so every
      # avoided poll saves ~all the tokens of one turn.
      DEFAULT_TIMEOUT = 60
      # How long output must be quiet before we assume the foreground command
      # is waiting for user input and return control to the LLM.
      # Raised from 500ms → 3000ms → 10_000ms: real shell prompts (sudo,
      # REPL, [Y/n] confirmations) stay quiet forever, so 10s still feels
      # instant for them; long builds / test runs frequently have multi-
      # second gaps between phases (compilation ↔ linking, spec file
      # transitions), and anything below 10s split those into multiple
      # polls — each poll replays the whole LLM context, which is expensive.
      DEFAULT_IDLE_MS = 10_000
      # Background commands collect this many seconds of startup output so
      # the agent can see crashes / readiness before getting the session_id.
      BACKGROUND_COLLECT_SECONDS = 2
      # Default ceiling for a fire-and-forget background task (fire_and_forget).
      # Tasks running longer than this are treated as stuck and the watcher
      # returns a timeout result. Callers can override via metadata[:max_duration].
      # 2 hours covers large CI suites (full rspec, big docker build, slow
      # `npm install` on a cold cache) but still bounds resource usage.
      BACKGROUND_TASK_MAX_DURATION = 7_200
      IDLE_MAX_DURATION            = 120    # 2 min — abandoned pagers/REPLs
      # Sentinel: when passed as idle_ms, disables idle early-return.
      DISABLED_IDLE_MS = 10_000_000

      # Commands that we know take a long time and produce bursty output
      # (quiet gaps between test files, compile phases, download batches,
      # etc.). When the command line STARTS WITH or CONTAINS any of these
      # tokens, we auto-extend the timeout to SLOW_COMMAND_TIMEOUT and
      # disable idle-return entirely — otherwise the LLM ends up polling
      # the same long-running job 5-10x, replaying full context each time.
      # Taken verbatim from the legacy shell.rb list.
      SLOW_COMMAND_PATTERNS = [
        # Ruby
        "bundle install", "bundle update", "bundle exec rspec",
        "rspec", "rake test", "rails test",

        # Node ecosystem — covers npm / yarn / pnpm test/dev/build/install variants
        "npm install", "npm ci", "npm test", "npm run build", "npm run test", "npm run dev",
        "yarn install", "yarn build", "yarn test", "yarn dev",
        "pnpm install", "pnpm build", "pnpm test", "pnpm dev",

        # Python
        "pytest", "pip install", "pip3 install", "python -m pip install",
        "python -m pytest", "python setup.py",

        # Go / Rust
        "cargo build", "cargo test", "cargo install", "cargo bench",
        "go build", "go test", "go install", "go mod tidy",

        # JVM (Maven / Gradle)
        "mvn test", "mvn package", "mvn install",
        "gradle build", "gradle test", "gradle assemble", "gradle bootRun",

        # .NET / Elixir / PHP / Swift
        "dotnet build", "dotnet test", "dotnet restore",
        "mix test", "mix deps.get",
        "composer install", "composer update",
        "xcodebuild", "swift test",

        # C / C++ / Make-family
        "make", "make test", "make install", "make build", "make all",
        "cmake --build", "cmake -B",

        # Containers / Infra
        "docker build", "docker-compose build",
        "terraform plan", "terraform apply",
        "helm install", "helm upgrade",
        "kubectl apply", "ansible-playbook", "vagrant up"
      ].freeze
      # Timeout granted to commands matched by SLOW_COMMAND_PATTERNS.
      # 180s matches the legacy safe_shell "hard_timeout" for slow commands.
      SLOW_COMMAND_TIMEOUT = 180

      # Patterns that are obviously quick — using fire_and_forget on these
      # is almost certainly a mistake and wastes tokens. The harness rejects
      # such calls at runtime with a clear error so the LLM falls back to
      # foreground mode.
      QUICK_COMMAND_PATTERNS = [
        /\A\s*ls\b/,
        /\A\s*cd\s/,
        /\A\s*pwd\b/,
        /\A\s*cat\s/,
        /\A\s*echo\b/,
        /\A\s*head\b/,
        /\A\s*tail\b/,
        /\A\s*wc\b/,
        /\A\s*which\b/,
        /\A\s*whoami\b/,
        /\A\s*date\b/,
        /\A\s*uname\b/,
        /\A\s*env\b/,
        /\A\s*clear\b/,
        /\A\s*history\b/,
        /\A\s*ps\b/,
        /\A\s*mkdir\b/,
        /\A\s*touch\b/,
        /\A\s*rm\b/,
        /\A\s*mv\b/,
        /\A\s*cp\b/
      ].freeze

      # Absolute path to the safe-rm shell snippet shipped with the gem.
      # Sourced by every interactive PTY session to install a `rm` shell
      # function that moves files to $OCTO_TRASH_DIR instead of
      # deleting them.
      #
      # Why source-from-file instead of writing the function body into
      # the PTY directly?
      #   Writing a multi-line function definition into `zsh -l -i` is
      #   unreliable — ZLE (Zsh Line Editor) treats multi-line input as
      #   interactive editing and garbles the body. Loading from a file
      #   via a single `source` line avoids ZLE entirely.
      #
      # Why a shell function (instead of a Ruby-side text rewrite)?
      #   A function defers parsing to the shell itself, so heredocs,
      #   multi-line commands, globs, and variable expansion are all
      #   handled correctly. The previous Ruby rewriter mis-parsed any
      #   command containing a heredoc body with "rm" in it.
      #
      # Coverage:
      #   Intercepts  — direct `rm …` in the interactive shell (incl.
      #                 multi-line, heredoc, glob, env-var expansion).
      #   Bypassed by — `command rm`, `/bin/rm`, `xargs rm`, `find -exec rm`,
      #                 child scripts. Same coverage as the old rewriter.
      SAFE_RM_PATH = File.expand_path("terminal/safe_rm.sh", __dir__).freeze
      # ---------------------------------------------------------------------
      # Public entrypoint — dispatches on parameter shape
      # ---------------------------------------------------------------------
      def execute(command: nil, handle_id: nil, input: nil, async: false,
                  cwd: nil, env: nil, kill: nil, idle_ms: nil,
                  working_dir: nil, max_duration: max_duration, **_ignored)
        # Auto-tune: for well-known long-running commands (rspec, bundle
        # install, cargo build, etc.), we stretch the budget AND disable
        # idle-return. This collapses what would otherwise be 5-10
        # "is it still running?" LLM round-trips into a single synchronous
        # call. Async runs and handle operations are NOT auto-tuned —
        # async already returns quickly by design.
        timeout = nil
        if command && !async && !handle_id && slow_command?(command)
          timeout ||= SLOW_COMMAND_TIMEOUT
          idle_ms ||= DISABLED_IDLE_MS
        end

        timeout = (timeout || DEFAULT_TIMEOUT).to_i
        idle_ms = (idle_ms || DEFAULT_IDLE_MS).to_i
        cwd ||= working_dir

        # Operations on an existing handle (query / send input / kill).
        if handle_id
          handle_id = handle_id.to_s
          if kill
            return do_kill_handle(handle_id)
          elsif input.nil?
            return do_query_handle(handle_id)
          else
            return do_continue_handle(handle_id, input.to_s, timeout: timeout, idle_ms: idle_ms)
          end
        end

        # Start a new command.
        if command && !command.to_s.strip.empty?
          # Runtime guard: reject async for obviously quick commands so the
          # LLM doesn't waste tokens on an "I started it" turn for `ls`.
          if async && quick_command?(command.to_s)
            return {
              error: "async:true is for long-running tasks (builds, tests, installs, dev servers). " \
                     "This command looks quick — drop async:true and use plain sync mode.",
              hint: "Commands like ls, cat, pwd, echo should not use async:true.",
              command: command.to_s
            }
          end
          return do_start(command.to_s, cwd: cwd, env: env, timeout: timeout,
                          idle_ms: idle_ms, async: async ? true : false,
                          max_duration: max_duration ? max_duration.to_i : nil)
        end

        { error: "terminal: must provide either `command`, or `handle_id` (alone to query, with input: to write, with kill:true to terminate)." }
      rescue SecurityError => e
        { error: "[Security] #{e.message}", security_blocked: true }
      rescue StandardError => e
        { error: "terminal failed: #{e.class}: #{e.message}", backtrace: e.backtrace.first(5) }
      end

      # Alias used by ToolExecutor to decide whether :confirm_safes mode
      # should auto-execute without asking the user.
      def self.command_safe_for_auto_execution?(command)
        Octo::Tools::Security.command_safe_for_auto_execution?(command)
      end

      # ---------------------------------------------------------------------
      # Internal Ruby API — synchronous capture
      # ---------------------------------------------------------------------
      #
      # Run a shell command and BLOCK until it terminates, returning
      # [output, exit_code]. Drop-in replacement for Open3.capture2e that
      # goes through the same PTY + login-shell + Security pipeline used by
      # the AI-facing tool (so rbenv/mise shims and gem mirrors work).
      #
      # Why this exists separately from #execute:
      #
      #   `execute` may return early with a :session_id the moment output
      #   goes idle for DEFAULT_IDLE_MS (3s) — this is intentional for AI
      #   agents (they can inspect progress, inject input, decide to kill).
      #   Ruby callers like the HTTP server's upgrade flow only care about
      #   "did it finish, with what output, what exit code" — they need
      #   synchronous semantics. Previously each caller re-implemented the
      #   poll loop (and 0.9.36's run_shell forgot to, causing the upgrade
      #   failure bug).
      #
      # NOT exposed in tool_parameters — AI agents cannot invoke this.
      #
      # @param command [String]   the shell command to run
      # @param timeout [Integer]  per-poll timeout AND the basis for the
      #                           overall deadline (deadline = timeout + 60s)
      # @param cwd     [String]   optional working directory
      # @param env     [Hash]     optional env overrides
      # @return [Array(String, Integer|nil)] [output, exit_code].
      #         exit_code is nil only if the overall deadline was hit and
      #         the session had to be force-killed.
      def self.run_sync(command, timeout: 120, cwd: nil, env: nil)
        terminal = new
        result   = terminal.execute(
          command: command,
          timeout: timeout,
          cwd:     cwd,
          env:     env,
        )
        output   = result[:output].to_s

        # Hard deadline in wall-clock terms — a genuinely stuck command
        # must terminate. Each individual poll still carries `timeout`.
        deadline = Time.now + timeout.to_i + 60

        while result[:exit_code].nil? && result[:session_id] && Time.now < deadline
          result = terminal.execute(
            session_id: result[:session_id],
            input:      "",
            timeout:    timeout,
          )
          output += result[:output].to_s
        end

        # Deadline exceeded — best-effort cleanup so the session doesn't leak.
        if result[:exit_code].nil? && result[:session_id]
          begin
            terminal.execute(session_id: result[:session_id], kill: true)
          rescue StandardError
            # swallow — cleanup is best-effort
          end
        end

        [output, result[:exit_code]]
      end

      # ---------------------------------------------------------------------
      # 1) Start a new command
      # ---------------------------------------------------------------------
      private def do_start(command, cwd:, env:, timeout:, async:, max_duration: nil, idle_ms: DEFAULT_IDLE_MS)
        if cwd && !Dir.exist?(cwd.to_s)
          return { error: "cwd does not exist: #{cwd}" }
        end

        # Security pre-flight: reject / rewrite dangerous commands before
        # they ever reach the shell. Raises SecurityError on block.
        safe_command = Octo::Tools::Security.make_safe(
          command,
          project_root: cwd || Dir.pwd
        )

        # Transparent async: if the caller didn't ask for async but the
        # command is a known slow pattern (build/test/install), behave AS
        # IF async:true was specified — the LLM gets a handle back, the
        # user keeps their input free.
        async ||= slow_command?(command)

        if async
          # Async path — spawn dedicated session, register a Registry
          # task, start the watcher that pushes a notification on exit.
          return start_async_command(command, safe_command, cwd: cwd, env: env, max_duration: max_duration)
        end

        # Foreground sync — try the persistent shell first, fall back to
        # a one-shot dedicated session if the persistent slot is busy.
        session, _reused = acquire_persistent_session(cwd: cwd, env: env)
        persistent = !session.nil?
        session ||= spawn_dedicated_session(cwd: cwd, env: env)
        return session if session.is_a?(Hash) && session[:error]

        # Run precmd/chpwd hooks before the user command so directory-
        # aware version managers (mise, direnv, conda, pyenv-virtualenv…)
        # pick up the current cwd and push their tools onto PATH. See
        # write_user_command for the full rationale.
        write_user_command(session, safe_command, with_hooks: true)

        result = wait_and_package(
          session,
          timeout: timeout,
          idle_ms: idle_ms,
          persistent: persistent,
          original_command: command,
          rewritten_command: safe_command
        )

        # Sync command is still alive when wait_and_package returned —
        # either waiting on a prompt (idle) or just slow (timeout reached)
        # or backgrounded. Promote to a handle so the LLM can address it
        # with terminal(handle_id:, ...). No watcher — the LLM is expected
        # to come back synchronously. If the LLM walks away without
        # killing the handle, the process leaks; we accept that as rare.
        if result[:session_id] && %w[waiting background timeout].include?(result[:state].to_s)
          return promote_to_handle(session, result)
        end

        result
      end

      # Spawn a session, write the command, collect ~2s of startup output
      # to surface crashes early, then register a Registry task + watcher.
      # Used by every async path (explicit async:true OR slow_command?
      # auto-routing).
      private def start_async_command(command, safe_command, cwd:, env:, max_duration:)
        session = spawn_dedicated_session(cwd: cwd, env: env)
        return session if session.is_a?(Hash) && session[:error]

        write_user_command(session, safe_command, with_hooks: true)

        # Collect ~2s of startup output so crashes are visible right away.
        result = wait_and_package(
          session,
          timeout: BACKGROUND_COLLECT_SECONDS,
          idle_ms: DISABLED_IDLE_MS,
          background: true,
          persistent: false,
          original_command: command,
          rewritten_command: safe_command
        )

        # If it finished inside the startup window (fast command misjudged
        # as slow, or it crashed immediately), return the sync result.
        unless result[:session_id] && %w[background waiting].include?(result[:state].to_s)
          return result
        end

        # Still running — register the task + watcher, return a handle.
        handle_id = register_task_for_session(session, command: command, cwd: cwd, max_duration: max_duration, watch: true)

        {
          accepted:       true,
          handle_id:      handle_id,
          state:          "running",
          output_file:    session.log_file,
          startup_output: result[:output],
          message:        "Async task started. You'll receive a <task-notification> when it exits. " \
                          "To peek at live progress: Read(output_file). To kill: " \
                          "terminal(handle_id: \"#{handle_id}\", kill: true)."
        }
      end

      # Promote a sync-hits-idle-or-timeout session to a handle. Two paths:
      #
      #   :waiting  → watch:false — LLM must come back with input/kill.
      #               No callback registered, no push notification.
      #   :timeout  → watch:true  — command is still running, just slow.
      #               Watcher monitors until exit, callback pushes a
      #               <task-notification> to the agent when done.
      #
      # For :timeout, we add accepted:true so the agent's act() path
      # registers a completion callback (same as explicit async:true).
      private def promote_to_handle(session, result)
        state_str = result[:state].to_s
        watch = (state_str == "timeout" || state_str == "idle")

        max_duration = state_str == "idle" ? IDLE_MAX_DURATION : nil

        handle_id = register_task_for_session(
          session,
          command: result[:original_command] || result[:rewritten_command] || nil,
          cwd: nil,
          max_duration: max_duration,
          watch: watch
        )

        idle_msg = "Command is waiting for input (idle). To answer: " \
                   "terminal(handle_id: \"#{handle_id}\", input: \"y\n\"). " \
                   "To kill: terminal(handle_id: \"#{handle_id}\", kill: true)."
        timeout_msg = "Command exceeded sync timeout but is still running. " \
                      "You'll be notified when it finishes. " \
                      "To peek: Read(output_file). " \
                      "To kill: terminal(handle_id: \"#{handle_id}\", kill: true)."

        result_hash = {
          handle_id:   handle_id,
          state:       result[:state],
          output:      result[:output],
          output_file: session.log_file,
          message:     watch ? timeout_msg : idle_msg,
          bytes_read:  result[:bytes_read]
        }
        result_hash[:accepted] = true if watch
        result_hash
      end

      # Register a Registry task for a running session. Stores the
      # internal SessionManager id in metadata so handle ops can look up
      # the PTY later. Watcher is optional — async paths want it (for
      # push notifications); sync-promoted handles don't (LLM is driving
      # synchronously). Returns the handle_id.
      private def register_task_for_session(session, command:, cwd:, max_duration:, watch:)
        handle_id = BackgroundTaskRegistry.create_task(
          type: "terminal",
          metadata: {
            command:              command,
            cwd:                  cwd,
            max_duration:         max_duration || BACKGROUND_TASK_MAX_DURATION,
            agent_session_id:     @agent_session_id,
            internal_session_id:  session.id,
            watched:              watch
          },
          on_cancel: build_session_cancel_hook(session)
        )

        if watch
          start_background_watcher(session, handle_id, command: command,
                                   max_duration: max_duration || BACKGROUND_TASK_MAX_DURATION)
        end

        handle_id
      end

      # Cancel hook used by Registry to kill the underlying process +
      # close fds when a task is cancelled. Same logic for both watched
      # (async) and unwatched (sync-promoted) handles.
      private def build_session_cancel_hook(session)
        ->(_task) {
          begin
            SessionManager.kill(session.id, signal: "TERM")
            sleep 0.1
            Process.kill("KILL", session.pid)
          rescue StandardError
            # ignore — best-effort cleanup
          end
          begin
            session.writer.close
            session.reader.close
            session.log_io.close
          rescue StandardError
            # ignore
          end
          SessionManager.forget(session.id)
        }
      end

      # Look up the PTY session backing a handle_id (UUID). Returns nil
      # if the handle is unknown, already completed, or the session was
      # forgotten by SessionManager.
      private def session_for_handle(handle_id)
        task = BackgroundTaskRegistry.get(handle_id)
        return nil unless task
        return nil unless task[:status] == "running"

        internal_id = task[:metadata]&.[](:internal_session_id)
        return nil unless internal_id

        SessionManager.refresh(internal_id)
      end

      # ---------------------------------------------------------------------
      # 2) Continue a handle: send input + poll for new output
      # ---------------------------------------------------------------------
      private def do_continue_handle(handle_id, input, timeout:, idle_ms: DEFAULT_IDLE_MS)
        session = session_for_handle(handle_id)
        return { error: "Handle #{handle_id} not found (already finished or killed)." } unless session

        # Bump last-activity so the sweep thread doesn't cancel an
        # unwatched handle that the LLM is actively driving.
        BackgroundTaskRegistry.record_activity(handle_id)

        if %w[exited killed].include?(session.status)
          # Mark Registry task complete (if not already) and clean up.
          BackgroundTaskRegistry.complete(handle_id, { exit_code: session.exit_code,
                                                       output: "",
                                                       state: "exited" })
          cleanup_session(session)
          return { handle_id: handle_id, state: "exited", exit_code: session.exit_code,
                   error: "Handle #{handle_id} has already #{session.status}." }
        end

        session.mutex.synchronize { session.writer.write(normalize_input_for_pty(input.to_s)) } unless input.to_s.empty?

        result = wait_and_package(session, timeout: timeout, idle_ms: idle_ms)

        # If the command finished as part of this sync poll, mark the Registry
        # task complete so any registered callback knows. For "watched"
        # handles (async path) the watcher would catch this anyway and we'd
        # race; complete() is idempotent (registry checks status before
        # firing) so it's safe.
        if result[:exit_code]
          BackgroundTaskRegistry.complete(handle_id, {
            exit_code: result[:exit_code],
            output:    result[:output],
            state:     "exited"
          })
        end

        # Rename session_id → handle_id in the result if wait_and_package
        # set it (it uses the internal int id by default). Once the command
        # has exited (exit_code is set), drop handle_id — the handle is gone.
        result.delete(:session_id)
        result[:handle_id] = handle_id unless result[:exit_code]
        result
      end

      # `\n` is a Unix newline, not the "Enter key". Inside cooked-mode PTYs
      # the kernel's ICRNL setting converts `\r` → `\n` on input, so `\r`
      # behaves identically to `\n` for ordinary shell/`read`/`input()` use.
      # BUT raw-mode TUI apps (curses-style installers, menus) read raw bytes
      # and only recognize `\r` as Enter; `\n` gets inserted as a literal
      # character into search fields, text inputs, etc.
      #
      # `\r` is therefore the only byte that means "Enter" in BOTH modes, so
      # we transparently translate `\n` → `\r` before writing to the PTY.
      # AI callers never need to know the difference.
      private def normalize_input_for_pty(str)
        str.gsub("\n", "\r")
      end

      # ---------------------------------------------------------------------
      # 3) Kill a handle — cancel the Registry task (which fires the
      #    on_cancel hook to TERM/KILL the underlying process and close fds).
      # ---------------------------------------------------------------------
      private def do_kill_handle(handle_id)
        cancelled = BackgroundTaskRegistry.cancel(handle_id, reason: "Killed by user via terminal tool.")
        if cancelled
          { killed: true, handle_id: handle_id, message: "Handle #{handle_id} cancelled." }
        else
          { error: "Handle #{handle_id} not found or already completed." }
        end
      end

      # ---------------------------------------------------------------------
      # 4) Query a handle — current state without sending input or waiting.
      # ---------------------------------------------------------------------
      private def do_query_handle(handle_id)
        task = BackgroundTaskRegistry.get(handle_id)
        return { error: "Handle #{handle_id} not found." } unless task

        # Bump last-activity so the sweep thread knows this handle is
        # still being driven by the LLM and doesn't cancel it.
        BackgroundTaskRegistry.record_activity(handle_id)

        elapsed = task[:created_at] ? (Time.now - task[:created_at]).round : nil
        session = session_for_handle(handle_id)
        {
          handle_id:       handle_id,
          state:           task[:status],
          command:         task[:metadata]&.[](:command),
          started_at:      task[:created_at]&.iso8601,
          elapsed_seconds: elapsed,
          output_file:     session&.log_file,
          exit_code:       task.dig(:result, :exit_code),
          message:         status_message_for_handle(task, elapsed)
        }.compact
      end

      private def status_message_for_handle(task, elapsed)
        status = task[:status]
        cmd = task[:metadata]&.[](:command) || "unknown command"
        time_str = elapsed ? "(running for #{elapsed}s)" : ""

        case status
        when "running"
          "Handle is still running #{time_str}: #{cmd}. You will be notified when it completes. DO NOT query this handle again — just wait for the notification."
        when "completed"
          result = task[:result] || {}
          exit_code = result[:exit_code]
          if exit_code.nil?
            "Handle completed with unknown status: #{cmd}."
          elsif exit_code.zero?
            "Handle completed successfully: #{cmd}."
          else
            "Handle failed with exit code #{exit_code}: #{cmd}."
          end
        when "cancelled"
          "Handle was cancelled: #{cmd}."
        else
          "Handle status: #{status} #{time_str}: #{cmd}."
        end
      end

      # =====================================================================
      # Plumbing
      # =====================================================================

      # Wait for the current command to either (a) finish with a marker,
      # (b) go idle on a prompt, or (c) hit the timeout. Package accordingly.
      #
      # Behaviour matrix:
      #
      #   state    | background: false            | background: true
      #   ---------+------------------------------+-----------------------------
      #   :matched | exit_code (finished)         | exit_code (crashed fast)
      #   :eof     | exit_code (child gone)       | exit_code (crashed fast)
      #   :idle    | session_id, state=waiting    | — (idle disabled)
      #   :timeout | session_id, state=timeout    | session_id, state=background
      private def wait_and_package(session, timeout:, idle_ms: DEFAULT_IDLE_MS,
                                   background: false, persistent: false,
                                   original_command: nil, rewritten_command: nil)
        start_offset = session.read_offset

        _before, code, state = read_until_marker(session, timeout: timeout, idle_ms: idle_ms)

        new_offset = log_size(session)
        raw = read_log_slice(session.log_file, start_offset, new_offset)
        cleaned = OutputCleaner.clean(raw)
        cleaned = cleaned.sub(session.marker_regex, "").rstrip if session.marker_regex
        cleaned = strip_command_echo(cleaned, marker_token: session.marker_token)
        # Per-line cap first: one minified JSON blob shouldn't blow the
        # whole 4 KB budget. MUST run before overflow spill so the file
        # on disk also has the long lines shortened (otherwise grep-ing
        # the spill file returns thousand-char lines the LLM chokes on).
        cleaned = truncate_long_lines(cleaned)
        truncated = false
        overflow_file = nil
        total_chars = cleaned.bytesize
        if cleaned.bytesize > MAX_LLM_OUTPUT_CHARS
          # Spill the FULL cleaned output to a sidecar file before we chop,
          # so the LLM can cat/grep/tail it in a follow-up tool call.
          overflow_file = spill_overflow_file(cleaned, session_id: session.id)

          # byteslice may cut through the middle of a multi-byte char, which
          # leaves the result as invalid UTF-8. Re-scrub after truncation so
          # everything downstream (JSON.generate, format_result, UI) gets a
          # guaranteed-valid UTF-8 string.
          preview = cleaned.byteslice(0, OVERFLOW_PREVIEW_CHARS)
          preview.force_encoding(Encoding::UTF_8)
          preview = preview.scrub("?") unless preview.valid_encoding?

          notice = if overflow_file
            "\n\n...[Output truncated for LLM: showing first #{OVERFLOW_PREVIEW_CHARS} " \
              "of #{total_chars} chars. Full output saved to: #{overflow_file} — " \
              "use `grep`, `head`, or `tail` on this path to search the rest.]"
          else
            "\n\n...[output truncated at #{OVERFLOW_PREVIEW_CHARS} chars " \
              "(overflow file unavailable; total was #{total_chars} chars)]"
          end

          cleaned = preview + notice
          truncated = true
        end
        SessionManager.advance_offset(session.id, new_offset)

        # Note rewrites so the agent notices if Security changed the command.
        rewrite_note = rewrite_note(original_command, rewritten_command)

        case state
        when :matched, :eof
          exit_code = code || session.exit_code
          if persistent && state == :matched && session_healthy?(session)
            # Command finished cleanly — return the shell to the pool so
            # the next call reuses it (no cold-start cost).
            stored = PersistentSessionPool.instance.release(session)
            cleanup_session(session) unless stored
          else
            cleanup_session(session)
          end
          if xcode_tools_missing?(cleaned)
            cleaned = "Xcode Command Line Tools are not installed.\n" \
                      "Run: bash ~/.octo/scripts/install_system_deps.sh\n" \
                      "Then retry the original command."
            exit_code = 1
          end
          {
            output: cleaned,
            exit_code: exit_code,
            bytes_read: new_offset - start_offset,
            output_truncated: truncated,
            full_output_file: overflow_file,
            security_rewrite: rewrite_note
          }.compact
        when :idle, :timeout
          # Command is still running interactively. If this was the persistent
          # session, we must release it from pool ownership — the caller now
          # owns it for follow-up input/kill, and the pool will spawn a fresh
          # one on the next acquire.
          PersistentSessionPool.instance.discard if persistent
          {
            output: cleaned,
            # NB: session_id here is the INTERNAL SessionManager int id, not
            # exposed to the LLM. Caller paths (do_start / do_continue_handle /
            # start_async_command) translate it into a handle_id (UUID) via
            # the Registry before returning to the LLM.
            session_id: session.id,
            state: background ? "background" : (state == :idle ? "waiting" : "timeout"),
            bytes_read: new_offset - start_offset,
            output_truncated: truncated,
            full_output_file: overflow_file,
            security_rewrite: rewrite_note
          }.compact
        end
      end

      private def xcode_tools_missing?(output)
        return false if output.nil? || output.empty?
        output.include?("xcode-select") && output.include?("No developer tools were found")
      end

      private def session_healthy?(session)
        return false unless session
        return false if %w[exited killed].include?(session.status.to_s)
        begin
          Process.kill(0, session.pid)
          true
        rescue Errno::ESRCH
          false
        rescue StandardError
          true
        end
      end

      # The shell may echo the wrapper line we injected (`{ USER_CMD; }; ...;
      # printf "__OCTO_DONE_..."`) before running it. When stty -echo is
      # honoured (bash/fresh pty) this is a no-op; when it isn't (zsh ZLE
      # sometimes re-enables echo on reuse, or the user sent input to a
      # running session) we strip the wrapper echo wherever it appears.
      #
      # Observed variants of the echoed wrapper:
      #
      #   1) Multi-line, starting the buffer (PTY in cooked mode, expanded
      #      \n escapes inside printf's double-quoted format string):
      #        { USER_CMD
      #        }; __octo_ec=$?; printf "
      #        __OCTO_DONE_<token>_%s__
      #        " "$__octo_ec"
      #
      #   2) Single-line / partially-truncated (PTY width wrap or partial
      #      char drop ate the leading `{` or first chars of the command):
      #        ails runner foo.rb ... }; __octo_ec=$?; printf " __OCTO_DONE_<token>_%s__ " "$__octo_ec"
      #
      #   3) Embedded mid-stream when re-echoed (e.g. after session re-use
      #      or after a user input: call landed in a shell that re-enabled
      #      echo). Same shape as (1) or (2) but not anchored to the start.
      #
      # We handle all three by running two passes:
      #   * an anchored multi-line strip (keeps the legacy behaviour and is
      #     cheapest when stty -echo silently failed);
      #   * a token-aware global strip that removes any remaining echoed
      #     wrapper fragment anywhere in the buffer. The token makes this
      #     safe: the real completion marker was already removed via
      #     session.marker_regex above, so any surviving occurrence of
      #     __OCTO_DONE_<token>_ is by definition an echoed wrapper.
      private def strip_command_echo(text, marker_token: nil)
        return text if text.nil? || text.empty?

        # Pass 0: strip the hooks prefix echo if `stty -echo` failed and
        # the shell echoed our `{ for __octo_f ...; } >/dev/null 2>&1`
        # line. `__octo_f` / `__octo_pc` are our private variable
        # names (double-underscore) that real user code effectively never
        # emits, which makes this safe to strip anywhere in the buffer.
        text = text.gsub(
          /\{\s*(?:for\s+__octo_f[^}]*?unset\s+__octo_f[^}]*?|if\s+\[[^}]*?__octo_pc[^}]*?unset\s+__octo_pc[^}]*?)\}\s*>\s*\/dev\/null\s+2>&1;?\n?/m,
          ""
        )

        # Pass 1: anchored strip — the full wrapper echoed at the start,
        # possibly spanning multiple real newlines.
        text = text.sub(/\A\{.*?"\$__octo_ec"\s*\n?/m, "")

        # Pass 2: token-aware global strip — remove any leftover wrapper
        # echo fragment, wherever it sits. Requires the session token so
        # we never touch unrelated user output that happens to mention
        # `__octo_ec`.
        if marker_token && !marker_token.empty?
          token_re = Regexp.escape(marker_token)

          # 2a. Multi-line shape: walk back from __OCTO_DONE_<token> to
          # the opening `{` of the wrapper (start of line or start of
          # buffer) and forward to the closing `"$__octo_ec"`.
          text = text.gsub(
            /(?:^|(?<=\n))\{[^\n]*\n(?:[^\n]*\n)*?[^\n]*__OCTO_DONE_#{token_re}_[^\n]*\n[^\n]*"\$__octo_ec"[^\n]*\n?/,
            ""
          )

          # 2b. Single-line shape: everything collapsed onto one line.
          # Strip from the wrapper's `}; __octo_ec=$?` pivot (or the
          # opening `{` if still present on that line) through the end of
          # the printf invocation (`"$__octo_ec"`).
          text = text.gsub(
            /[^\n]*\}; *__octo_ec=\$\?; *printf[^\n]*__OCTO_DONE_#{token_re}_[^\n]*"\$__octo_ec"[^\n]*\n?/,
            ""
          )

          # 2c. Last-resort: a bare marker-format fragment on its own,
          # without the `}; printf ...` prefix (e.g. terminal wrapped the
          # echo such that only the tail survived). Drop lines that
          # contain the literal `__OCTO_DONE_<token>_%s__` format —
          # the real marker has `\d+` in place of `%s` so this only hits
          # echoed wrappers.
          text = text.gsub(/^.*__OCTO_DONE_#{token_re}_%s__.*\n?/, "")
        end

        # Pass 3: token-INDEPENDENT fingerprint strip — PTY width-wrap
        # can chop the `__OCTO_DONE_<token>_%s__` format string out of
        # printf entirely, leaving e.g. `}; __octo_ec=$?; printf " " "$__octo_ec"`.
        # None of the token-aware patterns above catch that. The pair
        # `}; __octo_ec=$?` (opening pivot) and `"$__octo_ec"` (printf
        # tail) are our wrapper's unique fingerprints — `__octo_ec` is a
        # private double-underscore var name that user code effectively
        # never emits — so we strip anything between them (non-greedy,
        # multiline-aware) to also handle width-wrap that inserted
        # real \n breaks inside the echo.
        text = text.gsub(
          /[^\n]*\}; *__octo_ec=\$\?.*?"\$__octo_ec"[^\n]*\n?/m,
          ""
        )

        # Pass 4: bare pivot with no printf tail at all (extreme
        # truncation cut off everything after `__octo_ec=$?`). Still a
        # reliable fingerprint thanks to the `__octo_ec` var name.
        text = text.gsub(
          /[^\n]*\}; *__octo_ec=\$\?;?[^\n]*\n?/,
          ""
        )

        text
      end

      # NOTE: background_hint helper removed — the unified handle-based API
      # composes per-context messages in do_start / promote_to_handle /
      # start_async_command directly, all using the handle_id (UUID) the
      # LLM should reference.

      private def rewrite_note(original, rewritten)
        return nil if original.nil? || rewritten.nil?
        return nil if original.strip == rewritten.strip
        {
          original: original,
          rewritten: rewritten,
          message: "Command was rewritten by the safety layer."
        }
      end

      private def cleanup_session(session)
        SessionManager.kill(session.id, signal: "TERM") rescue nil
        sleep 0.05
        Process.kill("KILL", session.pid) rescue nil
        session.writer.close rescue nil
        session.reader.close rescue nil
        session.log_io.close rescue nil
        SessionManager.forget(session.id)
      end

      # -----------------------------------------------------------------
      # Background task watcher (fire_and_forget mode)
      # -----------------------------------------------------------------

      # Spawn a watcher thread that waits for the background session to
      # finish, then packages the result and notifies the registry.
      # The session is cleaned up after completion (success or crash).
      private def start_background_watcher(session, handle_id, command: nil, max_duration: BACKGROUND_TASK_MAX_DURATION)
        Thread.new do
          Thread.current.name = "bg-terminal-#{handle_id[0, 8]}"
          begin
            start_offset = session.read_offset

            _before, code, state = read_until_marker(
              session,
              timeout: max_duration,
              idle_ms: DISABLED_IDLE_MS
            )

            result = package_background_result(session, start_offset, code, state)
            result[:handle_id]   = handle_id
            result[:command]     = command
            result[:output_file] = session.log_file
            BackgroundTaskRegistry.complete(handle_id, result)
          rescue => e
            BackgroundTaskRegistry.complete(handle_id, {
              error: "Background watcher failed: #{e.class}: #{e.message}",
              exit_code: nil,
              handle_id: handle_id,
              command: command,
              output_file: session.log_file
            })
          ensure
            cleanup_session(session)
          end
        end
      end

      # Package the final result of a background session for the registry.
      # Mirrors wait_and_package but without session pooling logic.
      private def package_background_result(session, start_offset, code, state)
        new_offset = log_size(session)
        raw = read_log_slice(session.log_file, start_offset, new_offset)
        cleaned = OutputCleaner.clean(raw)
        cleaned = cleaned.sub(session.marker_regex, "").rstrip if session.marker_regex
        cleaned = strip_command_echo(cleaned, marker_token: session.marker_token)
        cleaned = truncate_long_lines(cleaned)

        exit_code = nil
        if state == :matched || state == :eof
          exit_code = code || session.exit_code
        end

        # Spill if oversized
        overflow_file = nil
        if cleaned.bytesize > MAX_LLM_OUTPUT_CHARS
          overflow_file = spill_overflow_file(cleaned, session_id: session.id)
          preview = cleaned.byteslice(0, OVERFLOW_PREVIEW_CHARS)
          preview.force_encoding(Encoding::UTF_8)
          preview = preview.scrub("?") unless preview.valid_encoding?
          notice = if overflow_file
            "\n\n...[Output truncated for LLM: showing first #{OVERFLOW_PREVIEW_CHARS} " \
              "of #{cleaned.bytesize} chars. Full output saved to: #{overflow_file}]"
          else
            "\n\n...[output truncated at #{OVERFLOW_PREVIEW_CHARS} chars]"
          end
          cleaned = preview + notice
        end

        result = {
          output: cleaned,
          exit_code: exit_code,
          state: state.to_s,
          bytes_read: new_offset - start_offset
        }
        result[:full_output_file] = overflow_file if overflow_file
        result[:error] = "Process exited without exit code" if state == :eof && exit_code.nil?
        result
      end

      private def chdir_args(cwd)
        cwd && Dir.exist?(cwd) ? { chdir: cwd } : {}
      end

      # ---------------------------------------------------------------------
      # Spawn a PTY-backed shell session and install our marker.
      #
      # Two flavours:
      #   * persistent — uses the user's real shell with full rc loading
      #     (`zsh -l -i` / `bash -l -i`) so shell functions, aliases, PATH
      #     tweaks etc. are all available. Cold-starts in ~1s which is why
      #     we aggressively reuse these via PersistentSessionPool.
      #   * dedicated — minimal shell with no rc (`bash --noprofile --norc
      #     -i`). Used for background commands (rails s, etc.) that will
      #     occupy the PTY for a long time, and as a fallback when a
      #     persistent spawn fails. Starts in ~50ms.
      # ---------------------------------------------------------------------

      # Try to acquire a persistent session. Returns [session, reused] or
      # [nil, false] on any failure (caller falls back to dedicated).
      private def acquire_persistent_session(cwd:, env:)
        PersistentSessionPool.instance.acquire(runner: self, cwd: cwd, env: env)
      rescue SpawnFailed
        [nil, false]
      rescue StandardError
        [nil, false]
      end

      # Public-ish: called by PersistentSessionPool to build a new long-lived
      # shell. Uses the user's SHELL with login+interactive flags so that all
      # rc hooks (nvm, rbenv, brew shellenv, mise, conda, etc.) are loaded.
      def spawn_persistent_session
        shell, shell_name = user_shell
        args = persistent_shell_args(shell, shell_name)
        session = spawn_shell(args: args, shell_name: shell_name,
                              command: "<persistent>", cwd: nil, env: {})
        raise SpawnFailed, session[:error] if session.is_a?(Hash)
        session
      end

      # Dedicated one-shot shell — no rc, fast startup. Used for background
      # commands and as a fallback.
      private def spawn_dedicated_session(cwd:, env:)
        args = ["/bin/bash", "--noprofile", "--norc", "-i"]
        spawn_shell(args: args, shell_name: "bash",
                    command: "<dedicated>", cwd: cwd, env: env || {})
      end

      # Returns [shell_path, shell_name]. Falls back to /bin/bash if SHELL
      # isn't set or the binary isn't executable.
      private def user_shell
        shell = ENV["SHELL"].to_s
        shell = "/bin/bash" if shell.empty? || !File.executable?(shell)
        name = File.basename(shell)
        # Only zsh / bash have first-class marker support; everything else
        # falls through to bash behaviour.
        name = "bash" unless %w[zsh bash].include?(name)
        [shell, name]
      end

      private def persistent_shell_args(shell, shell_name)
        case shell_name
        when "zsh", "bash"
          [shell, "-l", "-i"]
        else
          ["/bin/bash", "--noprofile", "--norc", "-i"]
        end
      end

      # Core spawn: PTY + reader thread + marker install.
      private def spawn_shell(args:, shell_name:, command:, cwd:, env:)
        # Per-project trash dir — the rm shell-function (see SAFE_RM_BASH
        # and install_marker) reads this env var to know where to move
        # deleted files.
        trash_dir =
          begin
            Octo::TrashDirectory.new(cwd || Dir.pwd).trash_dir
          rescue StandardError
            nil
          end

        spawn_env = {
          "TERM" => "xterm-256color",
          "PS1"  => "",
          # AI agents never need interactive pagers — less/more would
          # block the PTY waiting for input, causing idle promotion and
          # wasted timeout waiting. Force everything to dump to stdout.
          "PAGER"     => "cat",
          "GIT_PAGER" => "cat",
          # Prevent our sub-shell from polluting the user's ~/.zsh_history
          # (or ~/.bash_history). We fork a full interactive login shell to
          # get rbenv/nvm/brew-shellenv/mise loaded, but every command we
          # feed it (including our `{ cmd; }; printf "__OCTO_DONE_..."`
          # wrappers) would otherwise land in the user's shared HISTFILE
          # on exit.
          #
          # Note: zsh/bash rc files may *override* HISTFILE, so this is
          # only the first line of defence — `install_marker` re-disables
          # history after rc has run. See that method for details.
          "HISTFILE" => "/dev/null",
          "HISTSIZE" => "0",
          "SAVEHIST" => "0"
        }
        spawn_env["OCTO_TRASH_DIR"] = trash_dir if trash_dir
        (env || {}).each { |k, v| spawn_env[k.to_s] = v.to_s }

        log_file = SessionManager.allocate_log_file
        log_io   = File.open(log_file, "wb")

        # Prevent the child process from inheriting the server's
        # listening socket (port 7070) which would block hot_restart.
        # PTY.spawn does not support close_others, so we temporarily
        # set close_on_exec on the inherited fd — the kernel closes
        # it in the child after exec while the parent keeps it open.
        inherited_fd = ENV["OCTO_INHERIT_FD"].to_i
        if inherited_fd > 0
          begin
            inherited_io = IO.for_fd(inherited_fd)
            inherited_io.autoclose = false
            was_cloexec = inherited_io.close_on_exec?
            inherited_io.close_on_exec = true
          rescue StandardError
            inherited_fd = 0
          end
        end

        reader, writer, pid = PTY.spawn(
          spawn_env, *args, chdir_args(cwd)
        )
        reader.sync = true
        writer.sync = true

        # Restore original close_on_exec flag on the parent's fd so the
        # server can continue accepting connections after hot_restart.
        if inherited_fd > 0
          begin
            inherited_io.close_on_exec = was_cloexec
          rescue StandardError
            # best-effort
          end
        end

        begin
          writer.winsize = [40, 120]
        rescue StandardError
          # unsupported on some platforms
        end

        marker_token = SecureRandom.hex(8)
        reader_thread = start_reader_thread(reader, log_io)

        session = SessionManager.register(
          pid: pid, command: command, cwd: cwd || Dir.pwd,
          log_file: log_file, log_io: log_io,
          reader: reader, writer: writer,
          reader_thread: reader_thread,
          mode: "shell", marker_token: marker_token,
          shell_name: shell_name
        )

        # Give the shell a moment to print its startup banner (zsh -l -i
        # loads a lot of stuff), then drain whatever noise it wrote so the
        # marker install doesn't collide with it.
        sleep 0.2
        drain_any(session, timeout: 2.5)
        install_marker(session)
        _before, _code, state = read_until_marker(session, timeout: 10, idle_ms: DISABLED_IDLE_MS)
        unless state == :matched
          cleanup_session(session)
          return { error: "Failed to initialize terminal session (marker state=#{state}, shell=#{shell_name})" }
        end
        session.read_offset = log_size(session)
        SessionManager.advance_offset(session.id, session.read_offset)

        SessionManager.mark_running(session.id)
        session
      end

      # Background thread: drain PTY → log file.
      private def start_reader_thread(reader, log_io)
        Thread.new do
          loop do
            break if reader.closed? || log_io.closed?
            begin
              ready = IO.select([reader], nil, nil, 0.5)
              next unless ready
              chunk = reader.read_nonblock(4096)
              log_io.write(chunk) rescue nil
              log_io.flush rescue nil
            rescue IO::WaitReadable
              next
            rescue EOFError, Errno::EIO, IOError
              break
            rescue StandardError
              break
            end
          end
        ensure
          log_io.close rescue nil
        end
      end

      # Install minimal shell setup (runs AFTER rc has loaded):
      #   - disable history (HISTFILE=/dev/null + unset HISTFILE)
      #   - disable input echo (stty -echo)
      #   - empty PS1/PS2 so prompt lines don't add noise
      #
      # NOTE: we deliberately do NOT use PROMPT_COMMAND (bash) / precmd (zsh)
      # to emit the completion marker. Those hooks fight zsh's ZLE, iTerm2
      # shell integration, etc. Instead, every user command is wrapped with
      # an inline printf marker — see `write_user_command`. Same bytes work
      # in bash, zsh, and anything POSIX-ish.
      private def install_marker(session)
        # Order matters:
        #   1. Disable history BEFORE anything else, so this setup line
        #      itself never lands in ~/.zsh_history / ~/.bash_history.
        #      We already set HISTFILE=/dev/null in spawn_env, but the
        #      user's rc (.zshrc/.bashrc) may override it — so we reset
        #      it here, AFTER rc has run. Unsetting HISTFILE is the
        #      belt-and-braces: zsh/bash won't write history on exit if
        #      HISTFILE is unset.
        #   2. stty -echo stops the PTY from echoing our wrapper lines
        #      back into captured output.
        #   3. Empty PS1/PS2 keeps prompt noise out of captured output.
        setup_line = %Q{HISTFILE=/dev/null; HISTSIZE=0; SAVEHIST=0; unset HISTFILE 2>/dev/null; set +o histexpand 2>/dev/null; stty -echo 2>/dev/null; PS1=""; PS2=""; export PAGER=cat; export GIT_PAGER=cat\n}
        session.mutex.synchronize { session.writer.write(setup_line) }

        # Install the safe-rm shell function. Single-line `source`
        # avoids feeding a multi-line function definition through ZLE
        # (which would garble it under zsh -l -i). The file itself
        # ships with the gem — see SAFE_RM_PATH.
        if File.exist?(SAFE_RM_PATH)
          source_line = %Q{source #{SAFE_RM_PATH} 2>/dev/null || true\n}
          session.mutex.synchronize { session.writer.write(source_line) }
        end

        # Emit the first marker by running a no-op through the same wrapper
        # we use for real commands. spawn_shell's read_until_marker will
        # match this and consider the shell ready.
        write_user_command(session, ":")
      end

      # Wrap a user command so we can reliably detect its completion + exit
      # code regardless of shell flavour (bash/zsh/sh).
      #
      # The command runs in a group (`{ ...; }`) so trailing pipelines still
      # complete before the marker fires. `$?` inside the group captures the
      # user command's exit code; we stash it in `__octo_ec` immediately so
      # intervening shell activity doesn't clobber it before printf runs.
      #
      # Leading `\n` in the printf format ensures the marker starts on its
      # own line even when the user command ended without a trailing newline.
      #
      # `with_hooks:` — when true and the session is a real rc-loaded zsh/
      # bash, we run the shell's `chpwd_functions` + `precmd_functions`
      # before the user command. This mimics what the shell would do at
      # every prompt in an interactive session, and is what makes mise /
      # direnv / conda-auto-activate / pyenv-virtualenv / autoenv etc.
      # actually push their tools onto PATH.
      #
      # Why this is necessary:
      #   Most of these tools register themselves via precmd/chpwd hooks
      #   when you `eval "$(tool activate zsh)"` in ~/.zshrc. In a real
      #   terminal, those hooks fire every time the shell draws a new
      #   prompt. Our persistent session never draws a prompt (we drive
      #   it by writing one line at a time and reading back our marker),
      #   so the hooks never run — which is why commands like `node -v`
      #   come back as "command not found" even though ~/.zshrc was
      #   loaded at spawn time.
      #
      # We don't run hooks for internal bookkeeping commands (source rc,
      # env reset, cd, marker install) — those use with_hooks: false.
      private def write_user_command(session, command, with_hooks: false)
        token  = session.marker_token
        # Hooks run in their own group with stdout+stderr redirected to
        # /dev/null so any chatty hook (direnv's "direnv: loading .envrc",
        # conda banners, etc.) never contaminates captured output. Their
        # exit codes are also swallowed so the *user* command's $? is what
        # lands in `__octo_ec`.
        hooks_line = with_hooks ? hooks_prefix_for(session) : ""
        line   = %Q|#{hooks_line}{ #{command}\n}; __octo_ec=$?; printf "\n__OCTO_DONE_#{token}_%s__\n" "$__octo_ec"\n|
        session.mutex.synchronize { session.writer.write(line) }
      end

      # Build the "run hooks" prefix line. Empty string for shells where
      # we don't know how to introspect hook registries.
      private def hooks_prefix_for(session)
        body = hook_invocation_for(session)
        return "" if body.strip.empty?
        # Single-line `{ …; } >/dev/null 2>&1;` so the hooks always run in
        # the same shell (no subshell — they must mutate PATH in *this*
        # shell), but their output goes nowhere. The trailing semicolon
        # separates from the user-command wrapper. The whole thing stays
        # on one logical line (newlines inside `body` are fine inside
        # `{ ... }`).
        "{ #{body.strip}\n} >/dev/null 2>&1;\n"
      end

      # Build the shell-specific snippet that runs every registered
      # chpwd / precmd function. Returns an empty string for shells we
      # don't know how to introspect (sh, dedicated --norc bash, etc.)
      # so those sessions behave exactly as before.
      #
      # Each hook is wrapped in `2>/dev/null || true` so a single broken
      # hook can't abort the user command or leak stderr noise into
      # captured output.
      private def hook_invocation_for(session)
        case session.shell_name.to_s
        when "zsh"
          # zsh: chpwd_functions / precmd_functions are arrays of function
          # names. `(P)name` expansion is avoided — plain `$array` with
          # word splitting works under the default zsh options since
          # `.zshrc` already ran (KSH_ARRAYS etc. is off by default for
          # interactive zsh started via -i).
          <<~ZSH
            for __octo_f in $chpwd_functions; do "$__octo_f" 2>/dev/null || true; done
            for __octo_f in $precmd_functions; do "$__octo_f" 2>/dev/null || true; done
            unset __octo_f 2>/dev/null
          ZSH
        when "bash"
          # bash: no chpwd equivalent. PROMPT_COMMAND may be a string
          # (classic) or an array (bash 5.1+). Handle both.
          <<~BASH
            if [ "${BASH_VERSINFO[0]:-0}" -ge 5 ] && [ "${BASH_VERSINFO[1]:-0}" -ge 1 ] && declare -p PROMPT_COMMAND 2>/dev/null | grep -q 'declare -a'; then
              for __octo_pc in "${PROMPT_COMMAND[@]}"; do eval "$__octo_pc" 2>/dev/null || true; done
            elif [ -n "${PROMPT_COMMAND:-}" ]; then
              eval "$PROMPT_COMMAND" 2>/dev/null || true
            fi
            unset __octo_pc 2>/dev/null
          BASH
        else
          ""
        end
      end

      # ---------------------------------------------------------------------
      # In-session helpers used by PersistentSessionPool to reset state
      # between commands without having to respawn the shell.
      # ---------------------------------------------------------------------

      # Issue an in-shell command and wait for its marker. Returns true on
      # success (marker hit), false otherwise. Swallows output.
      private def run_inline(session, line, timeout: 5)
        write_user_command(session, line)
        _before, _code, state = read_until_marker(session, timeout: timeout, idle_ms: DISABLED_IDLE_MS)
        new_offset = log_size(session)
        SessionManager.advance_offset(session.id, new_offset)
        state == :matched
      end

      # Called by the pool when rc files (e.g. ~/.zshrc) have changed since
      # this session was spawned. Sources them in shell-startup order so
      # later files can see env set by earlier ones.
      #
      # Notes:
      #   - Errors inside each `source` are NOT silenced (dropping stderr
      #     previously masked failures like a broken `mise activate` that
      #     would leave PATH without node/ruby/etc.). They land in the PTY
      #     log where a developer can inspect them if a command mysteriously
      #     fails to find a tool.
      #   - `|| true` keeps the compound line's exit code at 0 so our
      #     marker reader treats the re-source as "succeeded" regardless
      #     of per-file hiccups — we don't want a flaky rc to disable the
      #     whole persistent shell.
      def source_rc_in_session(session, rc_files)
        return if rc_files.empty?
        cmd = rc_files.map { |f|
          escaped = f.gsub('"', '\"')
          "source \"#{escaped}\" || true"
        }.join("; ")
        run_inline(session, cmd, timeout: 15)
      end

      # Called by the pool to reset env between calls. First unsets any keys
      # we exported last time, then exports the new ones.
      def reset_env_in_session(session, unset_keys:, set_env:)
        parts = []
        unset_keys.each { |k| parts << "unset #{shell_escape_var(k)}" }
        set_env.each { |k, v| parts << "export #{shell_escape_var(k)}=#{shell_escape_value(v)}" }
        return if parts.empty?
        run_inline(session, parts.join("; "))
      end

      # Called by the pool to move the live shell to `cwd`.
      def cd_in_session(session, cwd)
        run_inline(session, "cd #{shell_escape_value(cwd)}")
      end

      private def shell_escape_var(name)
        # Env var names are alphanumeric + underscore by POSIX; reject anything
        # else defensively so we never build a malformed line.
        name.to_s.gsub(/[^A-Za-z0-9_]/, "")
      end

      private def shell_escape_value(val)
        # Wrap in single quotes, escaping any embedded single quotes.
        "'" + val.to_s.gsub("'", "'\\''") + "'"
      end

      # ---------------------------------------------------------------------
      # PTY/log read helpers
      # ---------------------------------------------------------------------
      private def drain_any(session, timeout: 1.0)
        deadline = Time.now + timeout
        loop do
          remaining = deadline - Time.now
          break if remaining <= 0
          ready = IO.select([session.reader], nil, nil, [remaining, 0.1].min)
          break unless ready
          begin
            session.reader.read_nonblock(4096)
          rescue IO::WaitReadable
            next
          rescue EOFError, Errno::EIO
            break
          end
        end
      end

      # Poll the log file until a marker matches, idle-return fires, or timeout.
      # Returns [raw_before_marker, exit_code_or_nil, state].
      # state ∈ :matched, :idle, :timeout, :eof
      private def read_until_marker(session, timeout:, idle_ms: DEFAULT_IDLE_MS)
        return ["", nil, :eof] unless session.marker_regex

        deadline    = Time.now + timeout
        idle_sec    = idle_ms / 1000.0
        start_size  = session.read_offset
        last_size   = start_size
        last_change = Time.now

        loop do
          current_size = log_size(session)
          if current_size > last_size
            slice = read_log_slice(session.log_file, session.read_offset, current_size)
            if (m = slice.match(session.marker_regex))
              return [slice[0...m.begin(0)], m[1].to_i, :matched]
            end
            last_size = current_size
            last_change = Time.now
          end

          SessionManager.refresh(session.id)
          if session.status == "exited" || session.status == "killed"
            slice = read_log_slice(session.log_file, session.read_offset, log_size(session))
            if (m = slice.match(session.marker_regex))
              return [slice[0...m.begin(0)], m[1].to_i, :matched]
            end
            return [slice, nil, :eof]
          end

          if last_size > start_size && (Time.now - last_change) >= idle_sec
            return ["", nil, :idle]
          end

          return ["", nil, :timeout] if Time.now >= deadline
          sleep 0.05
        end
      end

      private def log_size(session)
        session.log_io.size rescue File.size(session.log_file) rescue 0
      end

      private def read_log_slice(path, from, to)
        return "" if to <= from
        File.open(path, "rb") do |f|
          f.seek(from)
          f.read(to - from).to_s
        end
      rescue Errno::ENOENT
        ""
      end

      # Detect commands that are known to take a long time and produce
      # bursty output with multi-second quiet gaps. Used by `execute` to
      # auto-widen the timeout / disable idle-return so the LLM doesn't
      # poll a rspec/bundle-install 10 times over.
      #
      # Matching is substring-based after stripping common prefixes
      # (`sudo `, `env VAR=val `, `cd path && ...`) so that wrapping the
      # real slow command in another shell construct still hits.
      private def slow_command?(command)
        return false if command.nil? || command.empty?
        s = command.to_s

        # Strip leading `cd ... && ` / `cd ...;` — users / the agent often
        # prepend a cd to the real command.
        s = s.sub(/\Acd\s+\S+\s*(?:&&|;)\s*/, "")
        # Strip leading env-var assignments: `FOO=bar BAZ=qux cmd`.
        s = s.sub(/\A(?:[A-Za-z_][A-Za-z0-9_]*=\S+\s+)+/, "")
        # Trim leading whitespace.
        s = s.lstrip

        SLOW_COMMAND_PATTERNS.any? { |pat| s.include?(pat) }
      end

      # Check if a command is obviously quick and should never use
      # fire_and_forget. Used as a runtime guard to prevent token waste.
      private def quick_command?(command)
        return false if command.nil? || command.empty?
        s = command.to_s

        # Strip leading `cd ... && ` / `cd ...;` — the real command follows.
        s = s.sub(/\A\s*cd\s+\S+\s*(?:&&|;)\s*/, "")
        # Strip leading env-var assignments.
        s = s.sub(/\A(?:[A-Za-z_][A-Za-z0-9_]*=\S+\s+)+/, "")
        s = s.lstrip

        QUICK_COMMAND_PATTERNS.any? { |pat| s.match?(pat) }
      end

      # Apply per-line truncation to a cleaned (post-OutputCleaner) string.
      # If any single line exceeds MAX_LINE_CHARS, we chop it at that length
      # and append `…[line truncated: <original> chars]` so the LLM knows
      # content was elided. Critical for minified JS/CSS/JSON dumps that
      # would otherwise swallow the entire 4 KB budget with one line.
      private def truncate_long_lines(text, max: MAX_LINE_CHARS)
        return text if text.nil? || text.empty?
        lines = text.split("\n", -1)
        any_truncated = false
        truncated_lines = lines.map do |line|
          if line.bytesize > max
            any_truncated = true
            sliced = line.byteslice(0, max).to_s
            sliced.force_encoding(Encoding::UTF_8)
            sliced = sliced.scrub("?") unless sliced.valid_encoding?
            "#{sliced} …[line truncated: #{line.bytesize} chars]"
          else
            line
          end
        end
        return text unless any_truncated
        truncated_lines.join("\n")
      end

      # Overflow directory: shared across sessions (and persists after
      # Octo exits) so the LLM can re-read the full output in later
      # turns. Lives under /tmp so it is naturally swept by the OS, and
      # we also best-effort prune files older than OVERFLOW_MAX_AGE_SEC
      # on each write so long-running servers don't accumulate garbage.
      OVERFLOW_DIR_NAME = "octo-terminal-overflow"
      OVERFLOW_MAX_AGE_SEC = 7 * 24 * 60 * 60 # 7 days

      private def overflow_dir
        @overflow_dir ||= begin
          dir = File.join(Dir.tmpdir, OVERFLOW_DIR_NAME)
          FileUtils.mkdir_p(dir)
          dir
        end
      end

      # Drop overflow files older than OVERFLOW_MAX_AGE_SEC. Best-effort —
      # any error (permission, race with another process) is swallowed,
      # we'd rather keep the current command's result than crash because
      # of stale cleanup.
      private def prune_old_overflow_files
        cutoff = Time.now - OVERFLOW_MAX_AGE_SEC
        Dir.glob(File.join(overflow_dir, "*.log")).each do |f|
          next unless File.file?(f)
          begin
            File.delete(f) if File.mtime(f) < cutoff
          rescue StandardError
            # ignore
          end
        end
      rescue StandardError
        # ignore
      end

      # Write the full cleaned output to a sidecar file so the LLM can
      # `grep` / `head` / `tail` it in a follow-up tool call. Returns the
      # absolute path, or nil if the write failed (in which case we'll
      # just truncate without disclosure).
      private def spill_overflow_file(cleaned, session_id:)
        prune_old_overflow_files
        ts = Time.now.strftime("%Y%m%d-%H%M%S")
        sid = session_id || "nosid"
        rand = SecureRandom.hex(3)
        path = File.join(overflow_dir, "#{ts}-s#{sid}-#{rand}.log")
        File.open(path, "wb") { |f| f.write(cleaned) }
        path
      rescue StandardError
        nil
      end



      # Max visible length of a command inside the tool-call summary line.
      # Keeps the "terminal(...)" summary on a single UI row even when the
      # underlying command spans multiple lines (heredocs, multi-line ruby
      # -e blocks, etc.). The full command is still executed — only the
      # display is shortened.
      DISPLAY_COMMAND_MAX_CHARS = 80

      def format_call(args)
        cmd    = args[:command]   || args["command"]
        handle = args[:handle_id] || args["handle_id"]
        inp    = args[:input]     || args["input"]
        kill   = args[:kill]      || args["kill"]
        async  = args[:async]     || args["async"]

        if handle && kill
          "terminal(cancel handle)"
        elsif handle && !inp.nil?
          if inp.to_s.empty?
            "terminal(check handle)"
          else
            preview = inp.to_s.strip
            preview = preview.length > 30 ? "#{preview[0, 30]}..." : preview
            "terminal(send #{preview.inspect})"
          end
        elsif handle
          "terminal(query handle)"
        elsif cmd
          display_cmd = compact_command_for_display(cmd)
          if async
            "terminal(#{display_cmd}, async)"
          else
            "terminal(#{display_cmd})"
          end
        else
          "terminal(?)"
        end
      end

      # Collapse newlines and runs of whitespace into single spaces, then
      # truncate with an ellipsis so the command fits on one line in the UI.
      private def compact_command_for_display(cmd)
        one_line = cmd.to_s.gsub(/\s+/, " ").strip
        if one_line.length > DISPLAY_COMMAND_MAX_CHARS
          "#{one_line[0, DISPLAY_COMMAND_MAX_CHARS - 3]}..."
        else
          one_line
        end
      end

      # Number of trailing lines of output to include in the human-readable
      # display string (the result text that shows up in CLI / WebUI bubbles
      # under each tool call). Keep small so multi-poll loops stay readable.
      DISPLAY_TAIL_LINES = 6

      def format_result(result)
        return "[Blocked] #{result[:error]}" if result.is_a?(Hash) && result[:security_blocked]
        return "error: #{result[:error]}"   if result.is_a?(Hash) && result[:error]
        return "stopped" if result.is_a?(Hash) && result[:killed]

        return "done" unless result.is_a?(Hash)

        # Async task accepted — harness will notify on completion.
        if result[:accepted]
          return "async task started"
        end

        prefix = result[:security_rewrite] ? "[Safe] " : ""
        tail   = display_tail(result[:output])

        status =
          if result[:handle_id]
            # still running / waiting for input
            state = result[:state] || "waiting"
            "… #{state}"
          elsif result.key?(:exit_code)
            ec = result[:exit_code]
            ec.to_i.zero? ? "✓ exit=0" : "✗ exit=#{ec}"
          else
            "done"
          end

        status = "#{prefix}#{status}" unless prefix.empty?

        # When output overflowed, surface the file path in the UI too
        # (not just in the LLM-facing `output`). Keeps the dev aware that
        # the full log is recoverable.
        if result[:full_output_file]
          status = "#{status}  [full: #{result[:full_output_file]}]"
        end

        tail.empty? ? status : "#{tail}\n#{status}"
      end

      # Extract the last DISPLAY_TAIL_LINES non-empty lines of output so the
      # user can see what actually happened in this poll, not just a "128B"
      # byte-count. Output is USUALLY already cleaned by OutputCleaner, but
      # if a caller hands us raw bytes (or a byteslice chopped a multi-byte
      # character in half), `split`/`strip` would raise
      #   Encoding::CompatibilityError: invalid byte sequence in UTF-8
      # and the whole tool call would error. Guard with scrub.
      private def display_tail(output)
        return "" if output.nil?
        text = output.to_s
        # Defensive: make sure we have a valid UTF-8 string. No-op on the
        # happy path (already UTF-8, valid); only rebuilds when broken.
        unless text.encoding == Encoding::UTF_8 && text.valid_encoding?
          text = text.dup.force_encoding(Encoding::UTF_8)
          text = text.scrub("?") unless text.valid_encoding?
        end
        return "" if text.strip.empty?
        lines = text.split(/\r?\n/).reject { |l| l.strip.empty? }
        return "" if lines.empty?
        lines.last(DISPLAY_TAIL_LINES).join("\n")
      end
    end
  end
end
