# frozen_string_literal: true

require "pastel"
require_relative "../../version"
require_relative "../block_font"
require_relative "../../utils/workspace_rules"

module Octo
  module UI2
    module Components
      # WelcomeBanner displays the startup screen with ASCII logo, tagline, tips, and agent info.
      class WelcomeBanner
        LOGO = Octo::BlockFont.render("octo")

        TAGLINE = "[>] Your personal Assistant & Technical Co-founder"

        TIPS = [
          "[*] Ask questions, edit files, or run commands",
          "[*] Be specific for the best results",
          "[*] Create .octorules to customize interactions",
          "[*] Type /help for more commands"
        ].freeze

        # Minimum terminal width required for full logo display
        MIN_WIDTH_FOR_LOGO = 50

        def initialize
          @pastel = Pastel.new
        end

        # Get current theme from ThemeManager
        def theme
          UI2::ThemeManager.current_theme
        end

        # Render only the logo (ASCII art or simple text based on terminal width)
        # @param width [Integer] Terminal width
        # @return [String] Formatted logo only
        def render_logo(width:)
          lines = []
          lines << ""
          lines << logo_content(width)
          lines << ""
          lines.join("\n")
        end

        # Render startup banner
        # @param width [Integer] Terminal width
        # @return [String] Formatted startup banner
        def render_startup(width:)
          lines = []
          lines << ""
          lines << logo_content(width)
          lines << ""
          lines << @pastel.bright_cyan(TAGLINE)
          lines << @pastel.dim("    Version #{Octo::VERSION}")
          lines << ""
          TIPS.each do |tip|
            lines << @pastel.dim(tip)
          end
          lines << ""
          lines.join("\n")
        end

        # Render agent welcome section
        # @param working_dir [String] Working directory
        # @param mode [String] Permission mode
        # @return [String] Formatted agent welcome section
        def render_agent_welcome(working_dir:, mode:)
          lines = []
          lines << ""
          lines << separator("=")
          lines << @pastel.bright_green("[+] AGENT MODE INITIALIZED")
          lines << separator("=")
          lines << ""
          lines << info_line("Working Directory", working_dir)
          lines << info_line("Permission Mode", mode)

          # Show loaded project rules file if present
          main = Utils::WorkspaceRules.find_main(working_dir)
          lines << info_line("Project Rules", "#{main[:name]} ✓") if main

          lines << ""
          lines << theme.format_text("[!] Type 'exit' or 'quit' to terminate session", :thinking)
          lines << separator("-")
          lines << ""

          # Show sub-project agents block if any sub-dirs have .octorules
          sub_projects = Utils::WorkspaceRules.find_sub_projects(working_dir)
          unless sub_projects.empty?
            lines << @pastel.bright_cyan("[>] SUB-PROJECT AGENT MODE")
            lines << @pastel.dim("    #{sub_projects.size} sub-project(s) detected with rules:")
            sub_projects.each do |sp|
              first_line = sp[:summary].lines.first&.strip&.delete_prefix("#")&.strip
              label = @pastel.cyan("    • #{sp[:sub_name]}/")
              desc = first_line && !first_line.empty? ? @pastel.dim(" — #{first_line}") : ""
              lines << "#{label}#{desc}"
            end
            lines << @pastel.dim("    AI will read each sub-project's full .octorules before working in it.")
            lines << separator("-")
            lines << ""
          end

          lines.join("\n")
        end

        # Render full welcome (startup + agent info)
        # @param working_dir [String] Working directory
        # @param mode [String] Permission mode
        # @param width [Integer] Terminal width
        # @return [String] Full welcome content
        def render_full(working_dir:, mode:, width:)
          render_startup(width: width) + render_agent_welcome(
            working_dir: working_dir,
            mode: mode
          )
        end

        private def logo_content(width)
          if width >= MIN_WIDTH_FOR_LOGO
            @pastel.bright_green(LOGO)
          else
            @pastel.bright_green("Octo")
          end
        end

        private def info_line(label, value)
          label_text = @pastel.cyan("[#{label}]")
          value_text = theme.format_text(value, :info)
          "    #{label_text} #{value_text}"
        end

        private def separator(char = "-")
          theme.format_text(char * 80, :thinking)
        end
      end
    end
  end
end
