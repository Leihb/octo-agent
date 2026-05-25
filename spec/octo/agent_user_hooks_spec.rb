# frozen_string_literal: true

RSpec.describe Octo::Agent, "user shell hooks wiring" do
  let(:client) do
    instance_double(Octo::Client).tap do |c|
      c.instance_variable_set(:@api_key, "test-api-key")
    end
  end

  def build_agent(hooks: {})
    config = Octo::AgentConfig.new(permission_mode: :auto_approve, hooks: hooks)
    config.add_model(
      model: "claude-sonnet-4-5",
      api_key: "test-api-key",
      base_url: "https://api.anthropic.com"
    )
    described_class.new(
      client, config,
      working_dir: Dir.pwd,
      ui: nil,
      profile: "coding",
      session_id: Octo::SessionManager.generate_id,
      source: :manual
    )
  end

  describe "AgentConfig" do
    it "defaults hooks to an empty hash" do
      expect(Octo::AgentConfig.new.hooks).to eq({})
    end

    it "persists hooks through to_yaml when non-empty" do
      cfg = Octo::AgentConfig.new(hooks: { "before_tool_use" => [{ "command" => "echo" }] })
      yaml = cfg.to_yaml
      expect(yaml).to include("hooks:")
      expect(yaml).to include("command: echo")
    end

    it "omits hooks from to_yaml when empty (no stray key)" do
      cfg = Octo::AgentConfig.new
      yaml = cfg.to_yaml
      expect(yaml).not_to include("hooks:")
    end
  end

  describe "constructor wiring" do
    it "registers no hooks when config.hooks is empty" do
      agent = build_agent
      expect(agent.instance_variable_get(:@hooks).has_hooks?(:before_tool_use)).to be false
    end

    it "registers a programmatic block per yaml entry" do
      hooks = {
        "before_tool_use" => [
          { "matcher" => "terminal", "command" => "true", "block" => true }
        ],
        "on_complete" => [
          { "command" => "true" }
        ]
      }
      agent = build_agent(hooks: hooks)
      expect(agent.instance_variable_get(:@hooks).has_hooks?(:before_tool_use)).to be true
      expect(agent.instance_variable_get(:@hooks).has_hooks?(:on_complete)).to be true
    end

    it "delegates to Octo::ShellHookRunner.run when the hook fires" do
      hooks = {
        "before_tool_use" => [{ "command" => "true", "block" => true }]
      }
      agent = build_agent(hooks: hooks)

      expect(Octo::ShellHookRunner).to receive(:run).with(
        hash_including(event: :before_tool_use, entry: hooks["before_tool_use"].first)
      ).and_return({ action: :allow })

      agent.instance_variable_get(:@hooks).trigger(:before_tool_use, { name: "terminal" })
    end

    it "is a no-op when the same agent is later marked as a subagent" do
      hooks = {
        "before_tool_use" => [{ "command" => "true", "block" => true }]
      }
      agent = build_agent(hooks: hooks)
      agent.instance_variable_set(:@is_subagent, true)

      expect(Octo::ShellHookRunner).not_to receive(:run)
      result = agent.instance_variable_get(:@hooks).trigger(:before_tool_use, { name: "terminal" })
      expect(result).to eq({ action: :allow })
    end

    it "ignores unknown event names in config" do
      hooks = { "not_a_real_event" => [{ "command" => "true" }] }
      expect { build_agent(hooks: hooks) }.not_to raise_error
    end
  end
end
