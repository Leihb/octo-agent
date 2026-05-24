# frozen_string_literal: true

module Octo
  class Agent
    # Scenario 2: Reflect on skill execution and suggest improvements.
    #
    # After a skill completes, forks a subagent to analyze:
    #   - Were instructions clear enough?
    #   - Any missing edge cases?
    #   - Any improvements needed?
    #
    # If the LLM identifies concrete improvements, it invokes skill-creator
    # to update the skill.
    module SkillReflector
      # Minimum iterations for a skill execution to warrant reflection.
      # This counts iterations within the skill execution only, not session-cumulative.
      MIN_SKILL_ITERATIONS = 5

      # Check if we should reflect on the skill that just executed
      # Called from SkillEvolution#run_skill_evolution_hooks
      def maybe_reflect_on_skill
        return unless @skill_execution_context

        # Only reflect on skills that the user explicitly invoked via slash command.
        # Skills triggered by the LLM itself (e.g. as part of a broader task) or
        # platform-management skills invoked incidentally should not be reflected on.
        return unless @skill_execution_context[:slash_command]

        # Skip default skills — they are system-owned and should not be
        # auto-improved by the evolution system.
        source = @skill_execution_context[:source]
        return if source == :default

        skill_name = @skill_execution_context[:skill_name]
        start_iteration = @skill_execution_context[:start_iteration]
        
        # Calculate iterations within the skill execution (not session-cumulative)
        iterations = @iterations - start_iteration

        # Only reflect if the skill actually ran for a meaningful number of iterations
        return if iterations < MIN_SKILL_ITERATIONS

        # Fork an isolated subagent to reflect + improve — does NOT touch main history
        @ui&.show_info("Reflecting on skill execution: #{skill_name}")
        subagent = fork_subagent
        result = subagent.run(build_skill_reflection_prompt(skill_name))

        # Merge subagent cost into parent's cumulative session spend so the
        # sessionbar reflects the real total. Without this, reflection cost
        # silently disappears from the user's visible total.
        if result
          subagent_cost = result[:total_cost_usd] || 0.0
          @total_cost += subagent_cost
          @ui&.update_sessionbar(cost: @total_cost, cost_source: @cost_source)
        end

        # Clear the context so we don't reflect again
        @skill_execution_context = nil
      end

      # Build the reflection prompt content
      # @param skill_name [String]
      # @return [String]
      private def build_skill_reflection_prompt(skill_name)
        <<~PROMPT
          ═══════════════════════════════════════════════════════════════
          SKILL REFLECTION MODE
          ═══════════════════════════════════════════════════════════════
          You just executed the skill "#{skill_name}".

          ## Quick Analysis

          Reflect on whether the skill could be improved:
          - Were the instructions clear enough?
          - Did you encounter any edge cases not covered?
          - Were there any steps that could be streamlined?
          - Is there missing context that would make it easier next time?
          - Did the skill produce the expected results?

          ## Decision

          If you identified **concrete, actionable improvements**:
            → Call invoke_skill("skill-creator", task: "Improve skill #{skill_name}: [describe specific improvements needed]")

          If the skill worked well as-is:
            → Respond briefly: "Skill #{skill_name} worked well, no improvements needed."

          ## Constraints

          - DO NOT spend more than 30 seconds on this reflection
          - Be specific and actionable in your improvement suggestions
          - Only suggest improvements that would make a meaningful difference
          - If you're unsure, err on the side of "no improvements needed"
        PROMPT
      end
    end
  end
end
