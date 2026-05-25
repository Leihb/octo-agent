# brew.sh — Homebrew install and CN mirror configuration
# Depends-On: colors.sh os.sh
# Requires-Vars: $SHELL_RC $USE_CN_MIRRORS
# Sets-Vars: (none)
# Include via: @include lib/brew.sh

# --------------------------------------------------------------------------
# Homebrew CN mirror URLs (Aliyun public mirrors)
# --------------------------------------------------------------------------
CN_HOMEBREW_BREW_GIT_REMOTE="https://mirrors.aliyun.com/homebrew/brew.git"
CN_HOMEBREW_CORE_GIT_REMOTE="https://mirrors.aliyun.com/homebrew/homebrew-core.git"
CN_HOMEBREW_BOTTLE_DOMAIN="https://mirrors.aliyun.com/homebrew/homebrew-bottles"
CN_HOMEBREW_API_DOMAIN="https://mirrors.aliyun.com/homebrew-bottles/api"

HOMEBREW_INSTALL_SCRIPT_URL="https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh"

# --------------------------------------------------------------------------
# configure_homebrew_cn_mirrors — export env vars and persist to SHELL_RC
# Only runs when USE_CN_MIRRORS=true
# --------------------------------------------------------------------------
configure_homebrew_cn_mirrors() {
    [ "$USE_CN_MIRRORS" = true ] || return 0

    print_info "Configuring Homebrew CN mirrors..."
    export HOMEBREW_INSTALL_FROM_API=1
    export HOMEBREW_API_DOMAIN="$CN_HOMEBREW_API_DOMAIN"
    export HOMEBREW_BREW_GIT_REMOTE="$CN_HOMEBREW_BREW_GIT_REMOTE"
    export HOMEBREW_CORE_GIT_REMOTE="$CN_HOMEBREW_CORE_GIT_REMOTE"
    export HOMEBREW_BOTTLE_DOMAIN="$CN_HOMEBREW_BOTTLE_DOMAIN"

    if ! grep -q "HOMEBREW_BOTTLE_DOMAIN" "$SHELL_RC" 2>/dev/null; then
        {
            echo ""
            echo "# Homebrew CN mirrors (added by octo installer)"
            echo "export HOMEBREW_INSTALL_FROM_API=1"
            echo "export HOMEBREW_API_DOMAIN=\"${CN_HOMEBREW_API_DOMAIN}\""
            echo "export HOMEBREW_BREW_GIT_REMOTE=\"${CN_HOMEBREW_BREW_GIT_REMOTE}\""
            echo "export HOMEBREW_CORE_GIT_REMOTE=\"${CN_HOMEBREW_CORE_GIT_REMOTE}\""
            echo "export HOMEBREW_BOTTLE_DOMAIN=\"${CN_HOMEBREW_BOTTLE_DOMAIN}\""
        } >> "$SHELL_RC"
        print_success "Homebrew CN mirrors written to $SHELL_RC"
    else
        print_success "Homebrew CN mirrors already configured in $SHELL_RC"
    fi
}

# --------------------------------------------------------------------------
# restore_homebrew_cn_mirrors — remove CN mirror lines from SHELL_RC
# --------------------------------------------------------------------------
restore_homebrew_cn_mirrors() {
    if [ -n "$SHELL_RC" ] && [ -f "$SHELL_RC" ] && grep -q "HOMEBREW_BOTTLE_DOMAIN" "$SHELL_RC" 2>/dev/null; then
        sed -i.bak '/# Homebrew CN mirrors (added by octo installer)/d' "$SHELL_RC"
        sed -i.bak '/HOMEBREW_INSTALL_FROM_API/d' "$SHELL_RC"
        sed -i.bak '/HOMEBREW_API_DOMAIN/d' "$SHELL_RC"
        sed -i.bak '/HOMEBREW_BREW_GIT_REMOTE/d' "$SHELL_RC"
        sed -i.bak '/HOMEBREW_CORE_GIT_REMOTE/d' "$SHELL_RC"
        sed -i.bak '/HOMEBREW_BOTTLE_DOMAIN/d' "$SHELL_RC"
        rm -f "${SHELL_RC}.bak"
        unset HOMEBREW_BREW_GIT_REMOTE HOMEBREW_CORE_GIT_REMOTE HOMEBREW_BOTTLE_DOMAIN
        print_success "Homebrew CN mirrors removed from $SHELL_RC"
    else
        print_info "Homebrew CN mirrors — nothing to restore"
    fi
}

# --------------------------------------------------------------------------
# ensure_homebrew — install Homebrew if missing, add to PATH
# --------------------------------------------------------------------------
ensure_homebrew() {
    if command_exists brew; then
        print_success "Homebrew already installed"
        return 0
    fi

    print_info "Installing Homebrew..."
    # Always use the official Homebrew install script; CN bottles still go
    # through Aliyun via the HOMEBREW_* env vars set in configure_homebrew_cn_mirrors.
    if /bin/bash -c "$(curl -fsSL "$HOMEBREW_INSTALL_SCRIPT_URL")"; then
        # Add Homebrew to PATH (Apple Silicon default path)
        echo 'export PATH="/opt/homebrew/bin:$PATH"' >> "$SHELL_RC"
        export PATH="/opt/homebrew/bin:$PATH"
        print_success "Homebrew installed"
    else
        print_error "Failed to install Homebrew"
        return 1
    fi
}
