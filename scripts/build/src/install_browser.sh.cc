#!/bin/bash
# install_browser.sh — install Node.js + chrome-devtools-mcp for browser automation
# Generated from scripts/build/src/install_browser.sh.cc — DO NOT EDIT DIRECTLY

set -e

@include lib/colors.sh
@include lib/os.sh
@include lib/shell.sh
@include lib/network.sh
@include lib/mise.sh

# Chrome DMG — always download from Google's official CDN.
DEFAULT_CHROME_DMG_URL="https://dl.google.com/chrome/mac/universal/stable/GGRO/googlechrome.dmg"

# --------------------------------------------------------------------------
# Ensure Chrome is installed (macOS only)
# Downloads DMG to Desktop, opens it, then exits with instructions.
# Re-run the script after Chrome is installed.
# --------------------------------------------------------------------------
ensure_chrome_macos() {
    print_step "Checking Google Chrome..."

    if [ -d "/Applications/Google Chrome.app" ] || [ -d "$HOME/Applications/Google Chrome.app" ]; then
        print_success "Google Chrome already installed"
        return 0
    fi

    print_warning "Google Chrome not found — downloading from dl.google.com..."

    local dmg_path="$HOME/Desktop/googlechrome.dmg"
    print_info "Downloading Chrome (~238 MB) to Desktop..."
    curl -L --progress-bar "$DEFAULT_CHROME_DMG_URL" -o "$dmg_path" || {
        print_error "Download failed"
        print_info "Please download manually: ${DEFAULT_CHROME_DMG_URL}"
        exit 1
    }

    print_success "Downloaded: ${dmg_path}"
    print_info "Opening DMG installer..."
    open "$dmg_path"
    echo ""
    print_info "Drag 'Google Chrome' to Applications, then re-run this script."
    exit 0
}

# --------------------------------------------------------------------------
# install_chrome_devtools_mcp — npm global install
# --------------------------------------------------------------------------
install_chrome_devtools_mcp() {
    print_step "Installing chrome-devtools-mcp..."

    if [ "$USE_CN_MIRRORS" = true ]; then
        npm config set registry "$NPM_REGISTRY_URL" 2>/dev/null || true
        print_info "npm registry → ${NPM_REGISTRY_URL}"
    fi

    if npm install -g chrome-devtools-mcp@latest 2>/dev/null; then
        print_success "chrome-devtools-mcp installed"
    else
        print_error "chrome-devtools-mcp installation failed"
        print_info "Try manually: npm install -g chrome-devtools-mcp@latest"
        return 1
    fi
}

# --------------------------------------------------------------------------
# Main
# --------------------------------------------------------------------------
main() {
    echo ""
    echo "Browser Automation Setup"
    echo "========================"

    detect_os
    detect_shell
    detect_network_region

    is_macos && ensure_chrome_macos

    print_step "Checking Node.js..."
    ensure_mise || exit 1
    install_node_via_mise || exit 1

    install_chrome_devtools_mcp || exit 1

    echo ""
    print_success "Done. Browser automation is ready."
    echo ""
}

main "$@"
