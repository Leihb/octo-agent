# mise.sh — install mise, Ruby, and Node via mise
# Depends-On: colors.sh os.sh
# Requires-Vars: $SHELL_RC $USE_CN_MIRRORS $CURRENT_SHELL $MISE_INSTALL_URL $NODE_MIRROR_URL $RUBY_VERSION_SPEC
# Sets-Vars: $MISE_BIN
# Include via: @include lib/mise.sh

# --------------------------------------------------------------------------
# Internal helper — locate the mise binary
# --------------------------------------------------------------------------
_mise_bin() {
    if [ -x "$HOME/.local/bin/mise" ]; then
        echo "$HOME/.local/bin/mise"
    elif command_exists mise; then
        command -v mise
    else
        echo ""
    fi
}

# --------------------------------------------------------------------------
# ensure_mise — install mise if missing, activate it in the current session
# --------------------------------------------------------------------------
ensure_mise() {
    local mise
    mise=$(_mise_bin)

    if [ -n "$mise" ]; then
        print_success "mise already installed: $($mise --version 2>/dev/null || echo 'n/a')"
        export PATH="$HOME/.local/bin:$PATH"
        eval "$($mise activate bash 2>/dev/null)" 2>/dev/null || true
        MISE_BIN="$mise"
        return 0
    fi

    print_step "Installing mise..."
    if curl -fsSL "$MISE_INSTALL_URL" | sh; then
        export PATH="$HOME/.local/bin:$PATH"
        eval "$(~/.local/bin/mise activate bash 2>/dev/null)" 2>/dev/null || true

        # Persist mise activation to shell rc
        local init_line='eval "$(~/.local/bin/mise activate '"$CURRENT_SHELL"')"'
        if ! grep -q "mise activate" "$SHELL_RC" 2>/dev/null; then
            echo "$init_line" >> "$SHELL_RC"
            print_info "Added mise activation to $SHELL_RC"
        fi

        MISE_BIN="$HOME/.local/bin/mise"
        print_success "mise installed"
    else
        print_error "Failed to install mise"
        return 1
    fi
}

# --------------------------------------------------------------------------
# install_ruby_via_mise — install Ruby via mise (precompiled where available)
# --------------------------------------------------------------------------
install_ruby_via_mise() {
    local mise="${MISE_BIN:-$(_mise_bin)}"
    if [ -z "$mise" ]; then
        print_error "mise not found — call ensure_mise first"
        return 1
    fi

    print_info "Installing Ruby via mise ($RUBY_VERSION_SPEC)..."

    # Use mise's default precompiled-binary source (no custom CDN). CN users
    # get slower downloads — set a `ruby.precompiled_url` in `mise settings`
    # by hand if you have your own mirror.
    "$mise" settings ruby.compile=false 2>/dev/null || true
    "$mise" settings unset ruby.precompiled_url 2>/dev/null || true

    if "$mise" use -g "$RUBY_VERSION_SPEC"; then
        eval "$($mise activate bash 2>/dev/null)" 2>/dev/null || true
        local installed
        installed=$(ruby -e 'puts RUBY_VERSION' 2>/dev/null || echo "unknown")
        print_success "Ruby $installed installed"
    else
        print_error "Failed to install Ruby via mise"
        return 1
    fi
}

# --------------------------------------------------------------------------
# install_node_via_mise — install Node.js 22 via mise
# --------------------------------------------------------------------------
install_node_via_mise() {
    local mise="${MISE_BIN:-$(_mise_bin)}"
    if [ -z "$mise" ]; then
        print_error "mise not found — call ensure_mise first"
        return 1
    fi

    # Skip if compatible Node is already active
    if command_exists node; then
        local ver major
        ver=$(node --version 2>/dev/null | sed 's/v//')
        major="${ver%%.*}"
        if [ "${major:-0}" -ge 22 ] 2>/dev/null; then
            print_success "Node.js v${ver} already satisfies >= 22 — skipping"
            return 0
        fi
    fi

    # Apply CN Node mirror if needed
    if [ "$USE_CN_MIRRORS" = true ] && [ -n "$NODE_MIRROR_URL" ]; then
        "$mise" settings node.mirror_url="$NODE_MIRROR_URL" 2>/dev/null || true
        print_info "Node mirror → ${NODE_MIRROR_URL}"
    fi

    print_info "Installing Node.js 22 via mise..."
    if "$mise" use -g node@22; then
        eval "$($mise activate bash 2>/dev/null)" 2>/dev/null || true
        print_success "Node.js $(node --version 2>/dev/null) installed"
    else
        print_error "Failed to install Node.js via mise"
        return 1
    fi
}
