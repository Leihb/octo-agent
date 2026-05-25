# frozen_string_literal: true

require "tmpdir"
require "fileutils"
require "json"

RSpec.describe Octo::ShellHookRunner do
  describe ".matches?" do
    it "matches when no matcher is set" do
      expect(described_class.matches?({ "command" => "echo" }, [{ name: "terminal" }])).to be true
    end

    it "matches when matcher equals the first arg's :name" do
      expect(described_class.matches?({ "matcher" => "terminal" }, [{ name: "terminal" }])).to be true
    end

    it "rejects when matcher differs from the tool name" do
      expect(described_class.matches?({ "matcher" => "terminal" }, [{ name: "edit" }])).to be false
    end

    it "rejects when there is no tool name in args" do
      expect(described_class.matches?({ "matcher" => "terminal" }, [42])).to be false
    end
  end

  describe ".build_env" do
    let(:agent) { double("agent", session_id: "abcd1234", working_dir: "/tmp/proj") }

    it "always sets OCTO_EVENT" do
      env = described_class.build_env(event: :on_complete, args: [{ status: :success }], agent: agent)
      expect(env["OCTO_EVENT"]).to eq("on_complete")
    end

    it "exposes session_id and working_dir when agent is given" do
      env = described_class.build_env(event: :on_start, args: ["hi"], agent: agent)
      expect(env["OCTO_SESSION_ID"]).to eq("abcd1234")
      expect(env["OCTO_WORKING_DIR"]).to eq("/tmp/proj")
    end

    it "exposes OCTO_TOOL_NAME + OCTO_TOOL_INPUT on tool events" do
      env = described_class.build_env(
        event: :before_tool_use,
        args: [{ name: "terminal", arguments: { command: "ls" } }],
        agent: agent
      )
      expect(env["OCTO_TOOL_NAME"]).to eq("terminal")
      expect(JSON.parse(env["OCTO_TOOL_INPUT"])).to eq({ "command" => "ls" })
    end
  end

  describe ".run" do
    it "returns allow when the entry has no command" do
      result = described_class.run(event: :on_start, entry: {}, args: [], agent: nil)
      expect(result).to eq({ action: :allow })
    end

    it "fires async (allow) when block is not true" do
      Dir.mktmpdir do |tmp|
        marker = File.join(tmp, "marker")
        described_class.run(
          event: :on_complete,
          entry: { "command" => "echo done > #{marker}" },
          args: [{ status: :success }],
          agent: nil
        )
        # Async thread may or may not have flushed yet; check eventually.
        deadline = Time.now + 3
        sleep 0.05 while !File.exist?(marker) && Time.now < deadline
        expect(File).to exist(marker)
      end
    end

    it "returns allow on zero exit when block:true" do
      result = described_class.run(
        event: :before_tool_use,
        entry: { "command" => "true", "block" => true },
        args: [{ name: "terminal" }],
        agent: nil
      )
      expect(result).to eq({ action: :allow })
    end

    it "returns deny with stderr when block:true and command fails" do
      result = described_class.run(
        event: :before_tool_use,
        entry: { "command" => "echo 'forbidden' >&2 && exit 7", "block" => true },
        args: [{ name: "terminal" }],
        agent: nil
      )
      expect(result[:action]).to eq(:deny)
      expect(result[:reason]).to match(/forbidden/i)
    end

    it "honors matcher: ignores the entry when tool name differs" do
      result = described_class.run(
        event: :before_tool_use,
        entry: { "matcher" => "edit", "command" => "exit 1", "block" => true },
        args: [{ name: "terminal" }],
        agent: nil
      )
      expect(result).to eq({ action: :allow })
    end

    it "denies on timeout when block:true" do
      result = described_class.run(
        event: :before_tool_use,
        entry: { "command" => "sleep 5", "block" => true, "timeout" => 1 },
        args: [{ name: "terminal" }],
        agent: nil
      )
      expect(result[:action]).to eq(:deny)
      expect(result[:reason]).to match(/timed out/)
    end

    it "passes OCTO_TOOL_INPUT into the command env" do
      Dir.mktmpdir do |tmp|
        out = File.join(tmp, "captured")
        described_class.run(
          event: :before_tool_use,
          entry: { "command" => "printenv OCTO_TOOL_INPUT > #{out}", "block" => true },
          args: [{ name: "terminal", arguments: { command: "ls" } }],
          agent: nil
        )
        expect(JSON.parse(File.read(out).strip)).to eq({ "command" => "ls" })
      end
    end
  end
end
