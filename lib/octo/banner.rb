# frozen_string_literal: true

require "pastel"
require_relative "version"
require_relative "block_font"

module Octo
  # Banner provides logo for CLI startup.
  class Banner
    DEFAULT_CLI_LOGO = Octo::BlockFont.render("octo")

    TAGLINE = "[>] Your personal Assistant & Technical Co-founder"

    def initialize
      @pastel = Pastel.new
    end

    def logo
      @pastel.cyan(DEFAULT_CLI_LOGO)
    end

    def tagline
      @pastel.dim(TAGLINE)
    end

    def highlight(text)
      @pastel.bright_white(text)
    end

    def full_banner
      "#{logo}\n#{tagline}"
    end
  end
end
