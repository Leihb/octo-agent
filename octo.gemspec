# frozen_string_literal: true

# Friendly Ruby version check — must come before anything else so it triggers
# during `gem install` when the gemspec is evaluated.
if RUBY_VERSION < "2.6.0"
  abort <<~MSG

    ✗  Ruby #{RUBY_VERSION} is not supported.

    Octo requires Ruby >= 2.6.0, but your system is running Ruby #{RUBY_VERSION}.

    ──────────────────────────────────────────────────────────────────────
     Recommended: Use the one-line installer (handles Ruby automatically)
    ──────────────────────────────────────────────────────────────────────

      /bin/bash -c "$(curl -sSL https://raw.githubusercontent.com/octo-ai/octo/main/scripts/install.sh)"

    This script will automatically install the correct Ruby version via mise
    and then install Octo — no manual Ruby upgrade needed.

    For more details, visit:
      https://github.com/octo-ai/octo#installation

  MSG
end

require_relative "lib/octo/version"

Gem::Specification.new do |spec|
  spec.name = "octo"
  spec.version = Octo::VERSION
  spec.authors = ["windy"]
  spec.email = ["yafei@dao42.com"]

  spec.summary = "A functionality-first AI agent with three equal faces (CLI, Web, IM)"
  spec.description = "Octo is a Ruby AI agent with three equal first-class interfaces: terminal CLI, web UI, and IM bridges (Feishu/WeCom/WeChat/Discord/Telegram). It speaks Anthropic Messages, OpenAI (Chat Completions + Responses), and AWS Bedrock natively, and provides autonomous tool use plus a Claude-Code-compatible skill format."
  spec.homepage = "https://github.com/yafeilee/octo"
  spec.license = "MIT"
  spec.required_ruby_version = ">= 2.6.0"

  spec.metadata["homepage_uri"] = spec.homepage
  spec.metadata["source_code_uri"] = "https://github.com/yafeilee/octo"
  spec.metadata["changelog_uri"] = "https://github.com/yafeilee/octo/blob/main/CHANGELOG.md"

  # Specify which files should be added to the gem when it is released.
  # The `git ls-files -z` loads the files in the RubyGem that have been added into git.
  gemspec = File.basename(__FILE__)
  spec.files = IO.popen(%w[git ls-files -z], chdir: __dir__, err: IO::NULL) do |ls|
    ls.readlines("\x0", chomp: true).reject do |f|
      (f == gemspec) ||
        f.start_with?(*%w[bin/ test/ spec/ features/ .git .github appveyor Gemfile])
    end
  end
  spec.bindir = "bin"
  spec.executables = ["octo"]
  spec.require_paths = ["lib"]

  # Runtime dependencies
  # faraday >= 2.9 requires Ruby >= 3.0; cap at < 2.9 so Ruby 2.6 gets 2.8.x
  spec.add_dependency "faraday", ">= 2.0", "< 2.9"
  spec.add_dependency "faraday-multipart", "~> 1.0"
  spec.add_dependency "thor", "~> 1.3"
  spec.add_dependency "tty-prompt", "~> 0.23"
  spec.add_dependency "tty-spinner", "~> 0.9"
  spec.add_dependency "diffy", "~> 3.4"
  spec.add_dependency "pastel", "~> 0.8"
  spec.add_dependency "tty-screen", "~> 0.8"
  spec.add_dependency "tty-markdown", "~> 0.7"
  # base64 is part of Ruby stdlib up to Ruby 3.3; only needed as explicit dep on Ruby 3.4+
  spec.add_dependency "base64", ">= 0.1.0"
  # logger left stdlib in Ruby 4.0; faraday 2.8.x's response/logger.rb does a bare
  # `require "logger"` so without this the gem can't load on Ruby 4.0+.
  spec.add_dependency "logger", ">= 1.4"
  spec.add_dependency "websocket", "~> 1.2"
  spec.add_dependency "webrick", "~> 1.8"
  spec.add_dependency "artii", "~> 2.1"
  # rubyzip 3.x requires Ruby >= 3.0; pin to ~> 2.4.1 for cross-version compatibility
  spec.add_dependency "rubyzip", "~> 2.4.1"

  # rouge 4.x requires Ruby >= 2.7; bundler auto-resolves to 3.x on older rubies
  spec.add_dependency "rouge", ">= 3.14", "< 5.0"
  spec.add_dependency "chunky_png", "~> 1.4"

  # For more information and examples about making a new gem, check out our
  # guide at: https://bundler.io/guides/creating_gem.html
end
