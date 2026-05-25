#!/bin/bash
# install.sh — Octo installer
# Generated from scripts/build/src/install.sh.cc — DO NOT EDIT DIRECTLY

set -e

@include lib/colors.sh
@include lib/os.sh
@include lib/shell.sh
@include lib/network.sh
@include lib/apt.sh

@include lib/gem.sh

# --------------------------------------------------------------------------
# Ensure Ruby >= 2.6 is available
# macOS: uses system Ruby or user-installed Ruby
# Linux: tries apt first; if missing or too old, prints manual install hint
# --------------------------------------------------------------------------
check_ruby() {
    command_exists ruby || return 1
    local ver; ver=$(ruby -e 'puts RUBY_VERSION' 2>/dev/null)
    version_ge "$ver" "2.6.0" && { print_success "Ruby $ver — OK"; return 0; }
    print_warning "Ruby $ver too old (need >= 2.6.0)"; return 1
}

ensure_ruby() {
    print_step "Checking Ruby..."
    check_ruby && return 0

    if is_linux_apt; then
        print_info "Installing Ruby via apt..."
        sudo apt-get install -y ruby ruby-dev 2>/dev/null && check_ruby && return 0
        print_warning "apt Ruby install failed or version too old"
    fi

    return 1
}

# --------------------------------------------------------------------------
# gem install octo-agent
# Source comes from configure_gem_source: official RubyGems globally,
# Aliyun mirror for CN users. No custom CDN.
# --------------------------------------------------------------------------
install_via_gem() {
    print_step "Installing Octo via gem..."
    configure_gem_source
    setup_gem_home

    # macOS system Ruby 2.6 has a buggy gem resolver that fails on rouge 4.x.
    # Pre-install a 2.6-compatible rouge to avoid resolver failure.
    local ruby_ver; ruby_ver=$(ruby -e 'puts RUBY_VERSION' 2>/dev/null)
    if [[ "$ruby_ver" == 2.6.* ]]; then
        print_warning "Ruby 2.6 detected — pinning rouge 3.30.0 first"
        gem install rouge -v 3.30.0 --no-document || { print_error "gem install rouge failed"; return 1; }
    fi

    if gem install octo-agent --no-document; then
        print_success "Octo installed successfully!"
        return 0
    fi

    print_error "gem install failed"; return 1
}

# --------------------------------------------------------------------------
# Post-install info
# --------------------------------------------------------------------------
show_post_install_info() {
    echo ""
    echo -e "  ${GREEN}Octo installed successfully!${NC}"
    echo ""
    echo "  Reload your shell:"
    echo -e "    ${YELLOW}source ${SHELL_RC}${NC}"
    echo ""
    echo -e "  ${GREEN}Web UI${NC} (recommended):"
    echo "    octo server"
    echo "    Open http://localhost:8888"
    echo ""
    echo -e "  ${GREEN}Terminal${NC}:"
    echo "    octo"
    echo ""
}

# --------------------------------------------------------------------------
# Main
# --------------------------------------------------------------------------
main() {
    echo ""
    echo "Octo Installation"
    echo ""

    detect_os
    detect_shell
    detect_network_region

    assert_supported_os "Please install Ruby >= 2.6.0 manually and run: gem install octo-agent"

    if [ "$OS" = "Linux" ]; then
        setup_apt_mirror
    fi

    ensure_ruby  || { print_error "Could not install a compatible Ruby"; exit 1; }
    install_via_gem && { show_post_install_info; exit 0; }
    print_error "Failed to install Octo"; exit 1
}

main "$@"
