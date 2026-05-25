#!/bin/bash
# uninstall.sh — Octo uninstaller
# Generated from scripts/build/src/uninstall.sh.cc — DO NOT EDIT DIRECTLY

set -e

@include lib/colors.sh
@include lib/os.sh
@include lib/shell.sh
@include lib/gem.sh

check_installation() {
    command_exists octo && return 0
    gem list -i octo-agent >/dev/null 2>&1 && return 0
    return 1
}

uninstall_gem() {
    command_exists gem || return 1
    if gem list -i octo-agent >/dev/null 2>&1; then
        print_step "Uninstalling via RubyGems..."
        gem uninstall octo-agent -x
    else
        print_info "Gem 'octo-agent' not found (already removed)"
    fi
}

remove_config() {
    local config_dir="$HOME/.octo"
    [ -d "$config_dir" ] || return 0
    print_warning "Configuration directory found: $config_dir"
    read -r -p "Remove configuration files (including API keys)? [y/N] " reply
    if [ "$reply" = "y" ] || [ "$reply" = "Y" ]; then
        rm -rf "$config_dir"
        print_success "Configuration removed"
    else
        print_info "Configuration preserved at: $config_dir"
    fi
}

# --------------------------------------------------------------------------
# Main
# --------------------------------------------------------------------------
main() {
    detect_shell

    echo ""
    echo "╔═══════════════════════════════════════════════════════════╗"
    echo "║                                                           ║"
    echo "║   🗑️  Octo Uninstallation                                 ║"
    echo "║                                                           ║"
    echo "╚═══════════════════════════════════════════════════════════╝"
    echo ""

    if ! check_installation; then
        print_warning "Octo does not appear to be installed"
        echo ""; exit 0
    fi

    uninstall_gem || print_warning "gem command not found, skipping gem uninstall"
    print_success "Octo uninstalled successfully"
    restore_gemrc
    restore_gem_home
    remove_config

    echo ""
    print_success "Uninstallation complete!"
    print_info "Thank you for using Octo 👋"
    echo ""
}

main
