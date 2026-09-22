#!/usr/bin/env bash
# PHPRay Install Script
# Detects PHP version(s), builds extension, deploys collector, configures systemd
#
# Usage:
#   sudo ./scripts/install.sh              — full install (extension + collector)
#   sudo ./scripts/install.sh --ext-only   — extension only (no collector/service)
#   sudo ./scripts/install.sh --collector-only — collector only (pre-built binary)
#   sudo ./scripts/install.sh --uninstall  — remove everything
#   sudo ./scripts/install.sh --status     — check installation status
#
# Environment variables:
#   PHPRAY_PHP_VERSIONS="8.1 8.3"  — install for specific PHP versions only
#   PHPRAY_SKIP_SERVICE=1          — don't install/start systemd service
#   PHPRAY_COLLECTOR_PORT=9191     — API listen port (default: 9191)
#   PHPRAY_DB_PATH=/var/lib/phpray/traces.db — SQLite path
#   PHPRAY_RETENTION_DAYS=30       — trace retention days

set -euo pipefail

# ─── Colors & output ───────────────────────────────────────────────
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

info()  { echo -e "${BLUE}ℹ${NC} $*"; }
ok()    { echo -e "${GREEN}✔${NC} $*"; }
warn()  { echo -e "${YELLOW}⚠${NC} $*"; }
err()   { echo -e "${RED}✖${NC} $*" >&2; }
step()  { echo -e "\n${BOLD}${CYAN}━━━ $* ━━━${NC}"; }
die()   { err "$*"; exit 1; }

# ─── Global paths ──────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
EXT_SRC="$PROJECT_DIR/src/extension"
COLLECTOR_SRC="$PROJECT_DIR/src/collector"
COLLECTOR_BIN="$PROJECT_DIR/bin/phpray-collector"

# Configurable
INSTALL_PREFIX="${PHPRAY_INSTALL_PREFIX:-/usr/local}"
CONFIG_DIR="/etc/phpray"
DATA_DIR="${PHPRAY_DB_PATH:-/var/lib/phpray}"
COLLECTOR_PORT="${PHPRAY_COLLECTOR_PORT:-9191}"
RETENTION_DAYS="${PHPRAY_RETENTION_DAYS:-30}"

# State
INSTALLED_VERSIONS=()
SKIPPED_VERSIONS=()
ERRORS=()

# ─── Helpers ───────────────────────────────────────────────────────

check_root() {
    if [[ $EUID -ne 0 ]]; then
        die "This script must be run as root (use sudo)"
    fi
}

check_deps() {
    local missing=()
    for cmd in make gcc autoconf; do
        command -v "$cmd" &>/dev/null || missing+=("$cmd")
    done
    if [[ ${#missing[@]} -gt 0 ]]; then
        err "Missing build dependencies: ${missing[*]}"
        info "Install with: apt-get install -y build-essential autoconf"
        die "Cannot continue without build tools"
    fi
}

# Find all installed PHP versions with phpize available
detect_php_versions() {
    local versions=()

    # Check if user specified versions
    if [[ -n "${PHPRAY_PHP_VERSIONS:-}" ]]; then
        for v in $PHPRAY_PHP_VERSIONS; do
            versions+=("$v")
        done
        echo "${versions[@]}"
        return
    fi

    # Look for phpize binaries — covers php8.1, php8.2, php8.3, php8.4, etc.
    for phpize_bin in /usr/bin/phpize8.* /usr/bin/phpize-8.* /usr/bin/phpize; do
        [[ -x "$phpize_bin" ]] || continue
        local ver
        if [[ "$phpize_bin" =~ phpize([0-9]+\.[0-9]+) ]]; then
            ver="${BASH_REMATCH[1]}"
        elif [[ "$phpize_bin" =~ phpize-([0-9]+\.[0-9]+) ]]; then
            ver="${BASH_REMATCH[1]}"
        elif [[ "$phpize_bin" == "/usr/bin/phpize" ]]; then
            # Default phpize — get version from php
            ver="$(php -r 'echo PHP_MAJOR_VERSION.".".PHP_MINOR_VERSION;' 2>/dev/null)" || continue
        else
            continue
        fi
        # Deduplicate
        local found=0
        for existing in "${versions[@]:-}"; do
            [[ "$existing" == "$ver" ]] && found=1
        done
        [[ $found -eq 0 ]] && versions+=("$ver")
    done

    # Also check DirectAdmin-style paths
    for phpdir in /usr/local/php*/bin; do
        [[ -x "$phpdir/phpize" ]] || continue
        local ver
        ver="$("$phpdir/php" -r 'echo PHP_MAJOR_VERSION.".".PHP_MINOR_VERSION;' 2>/dev/null)" || continue
        local found=0
        for existing in "${versions[@]:-}"; do
            [[ "$existing" == "$ver" ]] && found=1
        done
        [[ $found -eq 0 ]] && versions+=("$ver")
    done

    if [[ ${#versions[@]} -eq 0 ]]; then
        die "No PHP installations found with phpize. Install php-dev package."
    fi

    echo "${versions[@]}"
}

# Get phpize path for a PHP version
get_phpize() {
    local ver="$1"
    for path in \
        "/usr/bin/phpize${ver}" \
        "/usr/bin/phpize-${ver}" \
        "/usr/local/php${ver//./}/bin/phpize" \
        "/usr/bin/phpize"; do
        [[ -x "$path" ]] && echo "$path" && return
    done
    return 1
}

# Get php-config path for a PHP version
get_php_config() {
    local ver="$1"
    for path in \
        "/usr/bin/php-config${ver}" \
        "/usr/bin/php-config-${ver}" \
        "/usr/local/php${ver//./}/bin/php-config" \
        "/usr/bin/php-config"; do
        [[ -x "$path" ]] && echo "$path" && return
    done
    return 1
}

# Get PHP INI scan directory
get_ini_dir() {
    local ver="$1"
    local php_bin

    for path in \
        "/usr/bin/php${ver}" \
        "/usr/bin/php-${ver}" \
        "/usr/bin/php${ver//./}" \
        "/usr/local/php${ver//./}/bin/php" \
        "/usr/bin/php"; do
        [[ -x "$path" ]] && php_bin="$path" && break
    done
    [[ -z "${php_bin:-}" ]] && return 1

    # Try FPM scan dir first, then CLI
    local scan_dir
    scan_dir="$("$php_bin" --ini 2>/dev/null | grep 'Scan for' | awk -F: '{print $2}' | tr -d ' ')" || true

    if [[ -n "$scan_dir" && -d "$scan_dir" ]]; then
        echo "$scan_dir"
        return
    fi

    # Common paths
    for dir in \
        "/etc/php/${ver}/mods-available" \
        "/etc/php/${ver}/fpm/conf.d" \
        "/etc/php/${ver}/cli/conf.d" \
        "/etc/php.d" \
        "/usr/local/php${ver//./}/lib/php.conf.d"; do
        [[ -d "$dir" ]] && echo "$dir" && return
    done

    return 1
}

# Get FPM service name
get_fpm_service() {
    local ver="$1"
    for svc in \
        "php${ver}-fpm" \
        "php-fpm-${ver}" \
        "php${ver//./}-fpm" \
        "php-fpm"; do
        systemctl list-unit-files "${svc}.service" &>/dev/null && echo "$svc" && return
    done
    return 1
}

# ─── Build extension for one PHP version ───────────────────────────
build_extension() {
    local ver="$1"
    local phpize_bin php_config_bin ext_dir

    info "Building phpray.so for PHP $ver..."

    phpize_bin="$(get_phpize "$ver")" || {
        warn "phpize not found for PHP $ver — skipping"
        SKIPPED_VERSIONS+=("$ver (no phpize)")
        return 1
    }

    php_config_bin="$(get_php_config "$ver")" || {
        warn "php-config not found for PHP $ver — skipping"
        SKIPPED_VERSIONS+=("$ver (no php-config)")
        return 1
    }

    ext_dir="$("$php_config_bin" --extension-dir)" || {
        warn "Cannot determine extension dir for PHP $ver — skipping"
        SKIPPED_VERSIONS+=("$ver (no ext dir)")
        return 1
    }

    # Build in a temp directory to avoid polluting source
    local build_dir
    build_dir="$(mktemp -d "/tmp/phpray-build-${ver}-XXXXXX")"
    trap "rm -rf '$build_dir'" RETURN

    cp "$EXT_SRC"/* "$build_dir/"
    cd "$build_dir"

    # Clean build
    "$phpize_bin" --clean &>/dev/null 2>&1 || true
    "$phpize_bin" >/dev/null 2>&1 || {
        err "phpize failed for PHP $ver"
        ERRORS+=("PHP $ver: phpize failed")
        return 1
    }

    ./configure --enable-phpray --with-php-config="$php_config_bin" >/dev/null 2>&1 || {
        err "configure failed for PHP $ver"
        ERRORS+=("PHP $ver: configure failed")
        return 1
    }

    make -j"$(nproc)" >/dev/null 2>&1 || {
        err "make failed for PHP $ver"
        ERRORS+=("PHP $ver: make failed")
        return 1
    }

    # Install the .so
    cp modules/phpray.so "$ext_dir/phpray.so"
    chmod 644 "$ext_dir/phpray.so"
    ok "Built and installed phpray.so → $ext_dir/phpray.so"

    cd "$PROJECT_DIR"
    return 0
}

# ─── Configure PHP INI for one version ─────────────────────────────
configure_ini() {
    local ver="$1"
    local ini_dir

    ini_dir="$(get_ini_dir "$ver")" || {
        warn "Cannot find INI directory for PHP $ver — configure manually"
        return 1
    }

    local ini_file="$ini_dir/99-phpray.ini"

    # Check if Debian/Ubuntu mods-available system
    if [[ "$ini_dir" == *"mods-available"* ]]; then
        ini_file="$ini_dir/phpray.ini"
        cat > "$ini_file" <<'PHPINI'
; PHPRay — PHP request tracing
; priority=99
extension=phpray.so
phpray.enabled = 1
phpray.mode = smart
phpray.smart_threshold_normal = 200
phpray.smart_threshold_full = 1000
phpray.smart_threshold_alert = 3000
phpray.always_trace_errors = 1
phpray.output_path = /tmp/phpray.jsonl
phpray.ignore_uris = /health,/ping,/favicon.ico,/robots.txt
phpray.trace_cli = 0
phpray.emit_header = 1
phpray.capture_errors = 1
phpray.output_mode = both
phpray.shm_path = /dev/shm/phpray
phpray.shm_size = 33554432
PHPINI
        # Enable for FPM and CLI via phpenmod if available
        if command -v phpenmod &>/dev/null; then
            phpenmod -v "$ver" phpray 2>/dev/null || true
        fi
    else
        cat > "$ini_file" <<'PHPINI'
; PHPRay — PHP request tracing
extension=phpray.so
phpray.enabled = 1
phpray.mode = smart
phpray.smart_threshold_normal = 200
phpray.smart_threshold_full = 1000
phpray.smart_threshold_alert = 3000
phpray.always_trace_errors = 1
phpray.output_path = /tmp/phpray.jsonl
phpray.ignore_uris = /health,/ping,/favicon.ico,/robots.txt
phpray.trace_cli = 0
phpray.emit_header = 1
phpray.capture_errors = 1
phpray.output_mode = both
phpray.shm_path = /dev/shm/phpray
phpray.shm_size = 33554432
PHPINI
    fi

    ok "PHP $ver INI configured → $ini_file"
}

# ─── Build Go collector ───────────────────────────────────────────
build_collector() {
    step "Building phpray-collector"

    if ! command -v go &>/dev/null; then
        warn "Go not found — checking for pre-built binary"
        if [[ -f "$COLLECTOR_BIN" ]]; then
            info "Using pre-built binary: $COLLECTOR_BIN"
            return 0
        fi
        die "Go is not installed and no pre-built binary found. Install Go 1.21+ or place binary in bin/"
    fi

    local go_version
    go_version="$(go version | awk '{print $3}' | sed 's/go//')"
    info "Go version: $go_version"

    cd "$COLLECTOR_SRC"
    CGO_ENABLED=1 go build -ldflags="-s -w" -o "$COLLECTOR_BIN" . 2>&1 || {
        die "Go build failed"
    }
    cd "$PROJECT_DIR"

    ok "Collector built → $COLLECTOR_BIN ($(du -h "$COLLECTOR_BIN" | awk '{print $1}'))"
}

# ─── Install collector binary + configs ────────────────────────────
install_collector() {
    step "Installing collector"

    # Binary
    install -m 755 "$COLLECTOR_BIN" "$INSTALL_PREFIX/bin/phpray-collector"
    ok "Binary → $INSTALL_PREFIX/bin/phpray-collector"

    # Config directory
    mkdir -p "$CONFIG_DIR"
    if [[ ! -f "$CONFIG_DIR/collector.toml" ]]; then
        sed \
            -e "s|/var/lib/phpray/traces.db|${DATA_DIR}/traces.db|" \
            -e "s|days = 30|days = ${RETENTION_DAYS}|" \
            "$PROJECT_DIR/configs/collector.toml" > "$CONFIG_DIR/collector.toml"
        ok "Config → $CONFIG_DIR/collector.toml"
    else
        info "Config exists, not overwriting: $CONFIG_DIR/collector.toml"
    fi

    # Data directory
    mkdir -p "$DATA_DIR"
    chmod 750 "$DATA_DIR"
    ok "Data dir → $DATA_DIR"

    # Shared memory (ensure writable)
    touch /dev/shm/phpray 2>/dev/null || true
    chmod 666 /dev/shm/phpray 2>/dev/null || true
}

# ─── Install systemd service ──────────────────────────────────────
install_service() {
    [[ "${PHPRAY_SKIP_SERVICE:-0}" == "1" ]] && {
        info "Skipping systemd service (PHPRAY_SKIP_SERVICE=1)"
        return 0
    }

    step "Installing systemd service"

    cp "$PROJECT_DIR/configs/phpray-collector.service" /etc/systemd/system/phpray-collector.service
    systemctl daemon-reload
    ok "Systemd unit installed"

    # Enable but don't start yet — user may want to configure first
    systemctl enable phpray-collector 2>/dev/null || true
    ok "Service enabled (will start on boot)"

    # Start if not running
    if systemctl is-active --quiet phpray-collector; then
        info "Service already running — restarting to pick up new binary"
        systemctl restart phpray-collector
    else
        info "Starting phpray-collector service..."
        systemctl start phpray-collector || {
            warn "Service failed to start — check: journalctl -u phpray-collector"
        }
    fi

    ok "Service status: $(systemctl is-active phpray-collector 2>/dev/null || echo 'unknown')"
}

# ─── Uninstall ─────────────────────────────────────────────────────
do_uninstall() {
    check_root
    step "Uninstalling PHPRay"

    # Stop service
    if systemctl is-active --quiet phpray-collector 2>/dev/null; then
        info "Stopping phpray-collector service..."
        systemctl stop phpray-collector
    fi
    systemctl disable phpray-collector 2>/dev/null || true
    rm -f /etc/systemd/system/phpray-collector.service
    systemctl daemon-reload 2>/dev/null || true
    ok "Service removed"

    # Remove binary
    rm -f "$INSTALL_PREFIX/bin/phpray-collector"
    ok "Binary removed"

    # Remove extension from all PHP versions
    local versions
    versions=($(detect_php_versions 2>/dev/null)) || true
    for ver in "${versions[@]:-}"; do
        [[ -z "$ver" ]] && continue
        local php_config_bin ext_dir
        php_config_bin="$(get_php_config "$ver" 2>/dev/null)" || continue
        ext_dir="$("$php_config_bin" --extension-dir 2>/dev/null)" || continue
        if [[ -f "$ext_dir/phpray.so" ]]; then
            rm -f "$ext_dir/phpray.so"
            ok "Removed phpray.so for PHP $ver"
        fi
        # Remove INI
        local ini_dir
        ini_dir="$(get_ini_dir "$ver" 2>/dev/null)" || continue
        rm -f "$ini_dir/99-phpray.ini" "$ini_dir/phpray.ini"
        if command -v phpdismod &>/dev/null; then
            phpdismod -v "$ver" phpray 2>/dev/null || true
        fi
        ok "Removed INI for PHP $ver"
    done

    # Clean shared memory
    rm -f /dev/shm/phpray

    info "Config ($CONFIG_DIR) and data ($DATA_DIR) preserved."
    info "To remove completely: rm -rf $CONFIG_DIR $DATA_DIR"

    ok "PHPRay uninstalled"
}

# ─── Status ────────────────────────────────────────────────────────
do_status() {
    step "PHPRay Installation Status"

    # Extension
    local versions
    versions=($(detect_php_versions 2>/dev/null)) || versions=()
    if [[ ${#versions[@]} -eq 0 ]]; then
        warn "No PHP installations detected"
    else
        for ver in "${versions[@]}"; do
            local php_config_bin ext_dir
            php_config_bin="$(get_php_config "$ver" 2>/dev/null)" || continue
            ext_dir="$("$php_config_bin" --extension-dir 2>/dev/null)" || continue
            if [[ -f "$ext_dir/phpray.so" ]]; then
                local so_date
                so_date="$(stat -c '%y' "$ext_dir/phpray.so" 2>/dev/null | cut -d. -f1)"
                ok "PHP $ver: phpray.so installed ($so_date)"
            else
                warn "PHP $ver: phpray.so NOT installed"
            fi
        done
    fi

    # Collector
    echo ""
    if [[ -x "$INSTALL_PREFIX/bin/phpray-collector" ]]; then
        local col_ver
        col_ver="$("$INSTALL_PREFIX/bin/phpray-collector" version 2>&1)" || col_ver="unknown"
        ok "Collector: $col_ver"
    else
        warn "Collector: not installed"
    fi

    # Service
    echo ""
    if systemctl is-enabled --quiet phpray-collector 2>/dev/null; then
        local status
        status="$(systemctl is-active phpray-collector 2>/dev/null)"
        if [[ "$status" == "active" ]]; then
            ok "Service: running"
        else
            warn "Service: $status"
        fi
    else
        warn "Service: not configured"
    fi

    # Data
    echo ""
    if [[ -f "$DATA_DIR/traces.db" ]]; then
        local db_size
        db_size="$(du -h "$DATA_DIR/traces.db" | awk '{print $1}')"
        ok "Database: $DATA_DIR/traces.db ($db_size)"
    else
        info "Database: not created yet"
    fi

    # Ring buffer
    if [[ -f /dev/shm/phpray ]]; then
        local shm_size
        shm_size="$(du -h /dev/shm/phpray | awk '{print $1}')"
        ok "Ring buffer: /dev/shm/phpray ($shm_size)"
    else
        info "Ring buffer: not active"
    fi

    # JSONL
    if [[ -f /tmp/phpray.jsonl ]]; then
        local jsonl_size lines
        jsonl_size="$(du -h /tmp/phpray.jsonl | awk '{print $1}')"
        lines="$(wc -l < /tmp/phpray.jsonl)"
        ok "JSONL: /tmp/phpray.jsonl ($jsonl_size, $lines traces)"
    else
        info "JSONL: no trace file"
    fi
}

# ─── Full install ──────────────────────────────────────────────────
do_full_install() {
    check_root

    echo -e "${BOLD}${CYAN}"
    echo "  ╔═══════════════════════════════════════╗"
    echo "  ║  🔦 PHPRay Installer                  ║"
    echo "  ║  Real-time PHP profiling              ║"
    echo "  ╚═══════════════════════════════════════╝"
    echo -e "${NC}"

    # Verify source exists
    [[ -f "$EXT_SRC/phpray.c" ]] || die "Extension source not found at $EXT_SRC"
    [[ -f "$COLLECTOR_SRC/main.go" ]] || die "Collector source not found at $COLLECTOR_SRC"

    # Phase 1: Build extension for all PHP versions
    step "Detecting PHP installations"
    check_deps

    local versions
    versions=($(detect_php_versions))
    info "Found PHP versions: ${versions[*]}"

    for ver in "${versions[@]}"; do
        step "PHP $ver — Extension"
        if build_extension "$ver"; then
            configure_ini "$ver"
            INSTALLED_VERSIONS+=("$ver")
        fi
    done

    # Phase 2: Build collector
    build_collector

    # Phase 3: Install collector + config + service
    install_collector
    install_service

    # Phase 4: Reload PHP-FPM if applicable
    step "Reloading PHP-FPM"
    local fpm_reloaded=0
    for ver in "${INSTALLED_VERSIONS[@]}"; do
        local svc
        svc="$(get_fpm_service "$ver" 2>/dev/null)" || continue
        if systemctl is-active --quiet "$svc" 2>/dev/null; then
            systemctl reload "$svc" 2>/dev/null && {
                ok "Reloaded $svc"
                fpm_reloaded=1
            } || warn "Failed to reload $svc"
        fi
    done
    [[ $fpm_reloaded -eq 0 ]] && info "No FPM services to reload"

    # Summary
    echo ""
    step "Installation Summary"
    echo ""

    if [[ ${#INSTALLED_VERSIONS[@]} -gt 0 ]]; then
        ok "Extension installed for PHP: ${INSTALLED_VERSIONS[*]}"
    fi
    if [[ ${#SKIPPED_VERSIONS[@]} -gt 0 ]]; then
        warn "Skipped: ${SKIPPED_VERSIONS[*]}"
    fi
    if [[ ${#ERRORS[@]} -gt 0 ]]; then
        for e in "${ERRORS[@]}"; do
            err "$e"
        done
    fi

    echo ""
    ok "Collector: $INSTALL_PREFIX/bin/phpray-collector"
    ok "Config:    $CONFIG_DIR/collector.toml"
    ok "Database:  $DATA_DIR/traces.db"
    ok "Service:   phpray-collector.service"

    echo ""
    info "Verify: php -m | grep phpray"
    info "Dashboard: http://localhost:${COLLECTOR_PORT}"
    info "Logs: journalctl -u phpray-collector -f"

    if [[ ${#ERRORS[@]} -gt 0 ]]; then
        echo ""
        warn "${#ERRORS[@]} error(s) during installation"
        exit 1
    fi

    echo ""
    ok "🔦 PHPRay installed successfully!"
}

# ─── Main ──────────────────────────────────────────────────────────
case "${1:-}" in
    --uninstall|-u)
        do_uninstall
        ;;
    --status|-s)
        do_status
        ;;
    --ext-only)
        check_root
        check_deps
        _versions=($(detect_php_versions))
        for ver in "${_versions[@]}"; do
            step "PHP $ver — Extension"
            if build_extension "$ver"; then
                configure_ini "$ver"
                INSTALLED_VERSIONS+=("$ver")
            fi
        done
        ok "Extension installed for: ${INSTALLED_VERSIONS[*]}"
        ;;
    --collector-only)
        check_root
        build_collector
        install_collector
        install_service
        ok "Collector installed"
        ;;
    --help|-h)
        echo "PHPRay Install Script"
        echo ""
        echo "Usage:"
        echo "  sudo $0              Full install (extension + collector)"
        echo "  sudo $0 --ext-only   Extension only"
        echo "  sudo $0 --collector-only  Collector + service only"
        echo "  sudo $0 --uninstall  Remove PHPRay"
        echo "  sudo $0 --status     Check installation status"
        echo ""
        echo "Environment:"
        echo "  PHPRAY_PHP_VERSIONS=\"8.1 8.3\"  Install for specific PHP versions"
        echo "  PHPRAY_SKIP_SERVICE=1          Don't install systemd service"
        echo "  PHPRAY_COLLECTOR_PORT=9191     API listen port"
        echo "  PHPRAY_RETENTION_DAYS=30       Trace retention (days)"
        ;;
    "")
        do_full_install
        ;;
    *)
        die "Unknown option: $1 (use --help)"
        ;;
esac
