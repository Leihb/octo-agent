# frozen_string_literal: true

require "open3"
require "json"
require "timeout"

module Octo
  # Executes a single user-defined shell hook entry pulled from
  # ~/.octo/config.yml's `settings.hooks` section.
  #
  # An entry looks like:
  #   { "matcher" => "terminal",                       # optional
  #     "command" => "echo $OCTO_TOOL_INPUT >> log",   # required
  #     "block"   => true,                             # default false
  #     "timeout" => 5 }                               # default 30s
  #
  # The runner is stateless — `run` takes the event name, the entry, the
  # event arguments, and the parent agent (for session_id / working_dir),
  # then spawns a shell with ENV vars exposing event context.
  #
  # Return value mirrors what HookManager merges into its result hash:
  #   { action: :allow }                — keep going
  #   { action: :deny, reason: "..." }  — only for before_tool_use with block:true
  module ShellHookRunner
    DEFAULT_TIMEOUT_SECONDS = 30

    class << self
      # @param event [Symbol] one of HookManager::HOOK_EVENTS
      # @param entry [Hash]   the YAML hook entry (string-keyed)
      # @param args  [Array]  positional args originally passed to HookManager#trigger
      # @param agent [Octo::Agent, nil] for session_id / working_dir env vars
      # @return [Hash]
      def run(event:, entry:, args:, agent: nil)
        return { action: :allow } unless entry.is_a?(Hash)
        return { action: :allow } unless entry["command"].is_a?(String)
        return { action: :allow } unless matches?(entry, args)

        env = build_env(event: event, args: args, agent: agent)
        command = entry["command"]
        timeout = (entry["timeout"] || DEFAULT_TIMEOUT_SECONDS).to_i
        block = entry["block"] == true

        if block
          run_blocking(command: command, env: env, timeout: timeout, event: event)
        else
          run_fire_and_forget(command: command, env: env)
          { action: :allow }
        end
      end

      # A matcher string matches against the first arg's :name / "name" key
      # (i.e. tool name on tool events). Without a matcher the entry runs
      # for every invocation of the event.
      def matches?(entry, args)
        matcher = entry["matcher"]
        return true if matcher.nil? || matcher.to_s.empty?

        first = args.first
        tool_name =
          if first.is_a?(Hash)
            first[:name] || first["name"]
          end
        return false unless tool_name
        tool_name.to_s == matcher.to_s
      end

      def build_env(event:, args:, agent: nil)
        env = {
          "OCTO_EVENT" => event.to_s
        }
        env["OCTO_SESSION_ID"] = agent.session_id.to_s if agent.respond_to?(:session_id) && agent.session_id
        env["OCTO_WORKING_DIR"] = agent.working_dir.to_s if agent.respond_to?(:working_dir) && agent.working_dir

        first = args.first
        if first.is_a?(Hash) && (first[:name] || first["name"])
          env["OCTO_TOOL_NAME"] = (first[:name] || first["name"]).to_s
          input = first[:arguments] || first["arguments"] || first[:input] || first["input"] || first
          env["OCTO_TOOL_INPUT"] = input.is_a?(String) ? input : JSON.generate(input)
        end

        env
      end

      def run_blocking(command:, env:, timeout:, event:)
        stdout = +""
        stderr = +""
        status = nil
        timed_out = false

        # Explicit popen3 (rather than capture3 + Timeout.timeout) so we can
        # SIGTERM the child on timeout and stop the reader threads cleanly —
        # otherwise Ruby 4.x prints "stream closed" warnings when the readers
        # are interrupted mid-IO#read by the Timeout.
        Open3.popen3(env, command) do |_stdin, out_io, err_io, wait_thr|
          pid = wait_thr.pid
          out_thread = Thread.new { out_io.read }
          err_thread = Thread.new { err_io.read }
          out_thread.report_on_exception = false
          err_thread.report_on_exception = false

          begin
            Timeout.timeout(timeout) { wait_thr.value }
          rescue Timeout::Error
            timed_out = true
            (Process.kill("TERM", pid) rescue nil)
            (Process.wait(pid) rescue nil)
          end

          stdout = (out_thread.value rescue "").to_s
          stderr = (err_thread.value rescue "").to_s
          status = wait_thr.value
        end

        return { action: :deny, reason: "Shell hook timed out after #{timeout}s: #{command}" } if timed_out

        if status&.success?
          { action: :allow }
        else
          reason = (stderr || stdout).to_s.strip
          reason = "shell hook exit #{status&.exitstatus}" if reason.empty?
          Octo::Logger.warn("shell_hook.non_zero_exit",
            event: event, exit: status&.exitstatus, stderr: stderr&.strip)
          { action: :deny, reason: reason }
        end
      rescue StandardError => e
        Octo::Logger.error("shell_hook.spawn_error", event: event, error: e)
        { action: :allow }
      end

      def run_fire_and_forget(command:, env:)
        Thread.new do
          begin
            Open3.capture3(env, command)
          rescue StandardError => e
            Octo::Logger.error("shell_hook.async_error", error: e)
          end
        end
      end
    end
  end
end
