#!/usr/bin/env bash
#
# PHPRay one-line installer.
#
#   curl -fsSL https://phpray.dev/install.sh | sudo bash -s -- [options]
#
# Installs the prebuilt PHPRay PHP extension (no compilation) for every
# supported PHP version it can find, optionally installs the Go collector
# daemon, and (with confirmation) restarts the affected PHP/web services.
#
# The script is idempotent: it is safe to re-run. It never leaves a half-written
# php.ini (writes go to a temp file and are renamed into place atomically), and
# it exits non-zero with a readable message on any real failure.
#
# Options:
#   --version vX.Y.Z   Install a specific release (default: latest from GitHub)
#   --no-collector     Skip installing the collector daemon
#   --no-restart       Do not restart PHP/web services after install
#   --yes              Assume "yes" for prompts (non-interactive / CI)
#   --cagefs           Force the CloudLinux/CageFS layout (auto-detected when cagefsctl exists)
#   --no-cagefs        Never use the CageFS layout
#   --dry-run          Print the actions without performing any of them
#   -h, --help         Show this help
#
# CloudLinux / CageFS mode (auto when cagefsctl is present):
#   Every CageFS user has a private, empty /dev/shm, so the default ring buffer
#   is invisible to the collector, and one world-writable ring would let
#   customers read each other's traces. The installer therefore creates the
#   shared 1777 directory /run/phpray (tmpfiles.d for reboots), mounts it into
#   the cages (/etc/cagefs/cagefs.mp + cagefsctl --force-update, asked for
#   unless --yes), configures one private 0600 ring per user
#   (phpray.shm_path=/run/phpray/ring-%u, phpray.enabled=0 = opt-in per
#   account via .user.ini) and points the collector at the glob
#   /run/phpray/ring-* (collector 0.15+).
#
set -euo pipefail

# ─── Defaults ───────────────────────────────────────────────────────────────
VERSION_ARG=""
NO_COLLECTOR=0
NO_RESTART=0
ASSUME_YES=0
LISTEN_ADDR="127.0.0.1:9191"
AUTH=0
AUTH_TOKEN=""
UNINSTALL=0
PURGE=0
DRY_RUN=0
COLLECTOR_INSTALLED=0
CAGEFS_MODE="auto"          # auto | 1 (--cagefs) | 0 (--no-cagefs)
CAGEFS=0                    # resolved by detect_cagefs
CAGEFS_MP="/etc/cagefs/cagefs.mp"
CAGEFS_DIR="/run/phpray"    # shared with the cages; holds ring-<uid> (0600) and the control table
CAGEFS_RING_SIZE=2097152    # per-user ring: 2 MB each (touched pages only)
CAGEFS_REMOUNT_PENDING=0    # cagefs.mp changed but cagefsctl not run (no --yes / declined)

# Release source. Default: phpray.dev mirror (RELEASE_BASE/<tag>/<file>, LATEST = plain-text tag).
# Override with PHPRAY_RELEASE_BASE / PHPRAY_LATEST_URL, e.g. the GitHub Releases of phpray/phpray:
#   PHPRAY_LATEST_URL=https://api.github.com/repos/phpray/phpray/releases/latest
#   PHPRAY_RELEASE_BASE=https://github.com/phpray/phpray/releases/download
LATEST_API="${PHPRAY_LATEST_URL:-https://phpray.dev/dl/LATEST}"
RELEASE_BASE="${PHPRAY_RELEASE_BASE:-https://phpray.dev/dl}"
WORKDIR=""
TMPFILES=()
RES_PHP=()
RES_VER=()
RES_STATUS=()

# ─── Coloured logging ───────────────────────────────────────────────────────
# Colours are only emitted when stdout is a TTY and NO_COLOR is not set, so
# the output stays clean when piped or in CI.
if [ -t 1 ] && [ "${NO_COLOR:-}" != "1" ]; then
    C_RED=$'\033[0;31m'
    C_GREEN=$'\033[0;32m'
    C_YELLOW=$'\033[0;33m'
    C_BLUE=$'\033[0;34m'
    C_CYAN=$'\033[0;36m'
    C_BOLD=$'\033[1m'
    C_NC=$'\033[0m'
else
    C_RED=""
    C_GREEN=""
    C_YELLOW=""
    C_BLUE=""
    C_CYAN=""
    C_BOLD=""
    C_NC=""
fi

log_info()  { printf '%s[INFO]%s %s\n' "$C_BLUE" "$C_NC" "$*"; }
log_ok()    { printf '%s[ OK ]%s %s\n' "$C_GREEN" "$C_NC" "$*"; }
log_warn()  { printf '%s[WARN]%s %s\n' "$C_YELLOW" "$C_NC" "$*" >&2; }
log_err()   { printf '%s[FAIL]%s %s\n' "$C_RED" "$C_NC" "$*" >&2; }
log_step()  { printf '\n%s%s== %s ==%s\n' "$C_BOLD" "$C_CYAN" "$*" "$C_NC"; }

die() {
    log_err "$*"
    exit 1
}

usage() {
    cat <<'USAGE'
PHPRay installer

Usage (one-liner):
  curl -fsSL https://phpray.dev/install.sh | sudo bash -s -- [options]

Options:
  --version vX.Y.Z   Install a specific release (default: latest from GitHub)
  --no-collector     Skip installing the collector daemon
  --no-restart       Do not restart PHP/web services after install
  --yes              Assume "yes" for prompts (non-interactive / CI)
  --listen ADDR      Dashboard/API listen address (default 127.0.0.1:9191; e.g. 0.0.0.0:9191 for a public IP)
  --uninstall        Remove the extension (ini + .so) from every PHP found and the collector service/binary
                     (keeps /etc/phpray and /var/lib/phpray; add --purge to delete them too)
  --auth             Protect the dashboard with a login token (forced when --listen is not loopback)
  --cagefs           Force the CloudLinux/CageFS layout: shared /run/phpray mounted into the cages,
                     one private ring per user (phpray.shm_path=/run/phpray/ring-%u), opt-in per account
                     (auto-detected when cagefsctl is installed)
  --no-cagefs        Never use the CageFS layout
  --dry-run          Print the actions without performing any of them
  -h, --help         Show this help
USAGE
}

# ─── HTTP helpers (curl preferred, wget fallback) ───────────────────────────
http_get() {
    local url="$1"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$url"
    elif command -v wget >/dev/null 2>&1; then
        wget -qO- "$url"
    else
        die "Need curl or wget to download $url"
    fi
}

download_file() {
    local url="$1" dest="$2"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --output "$dest" "$url"
    elif command -v wget >/dev/null 2>&1; then
        wget -qO "$dest" "$url"
    else
        die "Need curl or wget to download $url"
    fi
}

# ─── Temp-file / cleanup management ─────────────────────────────────────────
# Every partial write we create is registered in TMPFILES so that an EXIT trap
# removes it; combined with atomic rename (mv) this guarantees we never leave a
# half-written ini or binary behind.
track_tmp() {
    TMPFILES+=("$1")
}

cleanup() {
    local f
    for f in ${TMPFILES[@]+"${TMPFILES[@]}"}; do
        if [ -n "$f" ]; then
            rm -f "$f"
        fi
    done
    if [ -n "${WORKDIR:-}" ] && [ -d "$WORKDIR" ]; then
        rm -rf "$WORKDIR"
    fi
}
trap cleanup EXIT INT TERM

# Write a file atomically: content to a temp file in the same directory, then
# rename over the destination. Never produces a partially written target.
write_file_atomic() {
    local dest="$1"
    local dir tmp
    dir="$(dirname "$dest")"
    if [ ! -d "$dir" ]; then
        mkdir -p "$dir"
    fi
    tmp="$dir/.$(basename "$dest").tmp.$$"
    track_tmp "$tmp"
    cat > "$tmp"
    chmod 644 "$tmp"
    mv -f "$tmp" "$dest"
    # Untrack now that the temp no longer exists.
    local i newtmps=()
    for i in "${TMPFILES[@]}"; do
        if [ "$i" != "$tmp" ]; then
            newtmps+=("$i")
        fi
    done
    TMPFILES=(${newtmps[@]+"${newtmps[@]}"})
}

# ─── Version resolution ─────────────────────────────────────────────────────
# Produces VERSION_TAG (with a leading "v", e.g. v1.2.3) and VERSION (bare).
resolve_version() {
    local tag
    if [ -n "$VERSION_ARG" ]; then
        tag="$VERSION_ARG"
    elif [ "$DRY_RUN" = "1" ]; then
        tag="(latest)"
        log_info "[dry-run] would resolve the latest release via $LATEST_API"
    else
        local body
        body="$(http_get "$LATEST_API" 2>/dev/null || true)"
        if [ -z "$body" ]; then
            die "Could not fetch the latest release from $LATEST_API (is it reachable?)"
        fi
        # GitHub JSON ({"tag_name": "v0.14.0", ...}) or a plain-text LATEST file ("v0.14.0")
        tag="$(printf '%s' "$body" | grep -o '"tag_name"[[:space:]]*:[[:space:]]*"[^"]*"' | head -n 1 \
                | sed -E 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]*)".*/\1/' || true)"
        if [ -z "$tag" ]; then
            tag="$(printf '%s' "$body" | tr -d '[:space:]' | grep -E '^v?[0-9]+\.[0-9]+\.[0-9]+([-.][A-Za-z0-9]+)*$' || true)"
        fi
        if [ -z "$tag" ]; then
            die "Could not parse a release tag from $LATEST_API"
        fi
    fi

    case "$tag" in
        v*) VERSION_TAG="$tag" ;;
        *)  VERSION_TAG="v$tag" ;;
    esac
    VERSION="${VERSION_TAG#v}"
    log_info "Release: $VERSION_TAG"
}

# ─── Checksum verification (sha256sum or shasum) ────────────────────────────
# Expects WORKDIR/SHA256SUMS to have been downloaded beforehand.
verify_sha() {
    local file="$1" name expect actual
    name="$(basename "$file")"
    if [ ! -f "$WORKDIR/SHA256SUMS" ]; then
        return 1
    fi
    expect="$(awk -v n="$name" '$2 == n {print $1; exit}' "$WORKDIR/SHA256SUMS")"
    if [ -z "$expect" ]; then
        log_warn "No SHA256SUMS entry for $name; skipping verification"
        return 0
    fi
    if command -v sha256sum >/dev/null 2>&1; then
        actual="$(sha256sum "$file" | awk '{print $1}')"
    elif command -v shasum >/dev/null 2>&1; then
        actual="$(shasum -a 256 "$file" | awk '{print $1}')"
    else
        die "Neither sha256sum nor shasum is available; cannot verify $name"
    fi
    if [ "$expect" != "$actual" ]; then
        log_err "Checksum mismatch for $name (expected $expect, got $actual)"
        return 1
    fi
    log_ok "Verified $name"
    return 0
}

# ─── System detection ───────────────────────────────────────────────────────
detect_libc() {
    if [ -f /etc/alpine-release ]; then
        echo "musl"
        return 0
    fi
    if command -v ldd >/dev/null 2>&1; then
        if ldd --version 2>&1 | grep -qi musl; then
            echo "musl"
            return 0
        fi
    fi
    echo "glibc"
}

detect_arch() {
    local m
    m="$(uname -m)"
    case "$m" in
        x86_64|amd64)  echo "amd64" ;;
        aarch64|arm64) echo "arm64" ;;
        *)             echo "$m" ;;
    esac
}

# Compute the effective install target for a PHP binary: "version|libc|arch".
# Two different PHP paths (e.g. /usr/bin/php and /usr/bin/php8.3) that resolve
# to the same version/libc/arch share one target and should only be installed
# once. Prints an empty string if the binary cannot be versioned.
php_target_key() {
    local php="$1" ver
    ver="$("$php" -n -r 'echo PHP_MAJOR_VERSION.".".PHP_MINOR_VERSION;' 2>/dev/null || true)"
    if [ -z "$ver" ]; then
        return 0
    fi
    printf '%s|%s|%s\n' "$ver" "$(detect_libc)" "$(detect_arch)"
}

# Enumerate every PHP binary we know how to find, one per line, deduplicated.
find_php_bins() {
    local -a cands=()
    local dir f

    # php and php8.* found anywhere on PATH.
    local pathdirs
    pathdirs="$(printf '%s' "$PATH" | tr ':' '\n')"
    while IFS= read -r dir; do
        if [ -z "$dir" ]; then
            continue
        fi
        if [ ! -d "$dir" ]; then
            continue
        fi
        for f in "$dir/php" "$dir"/php8.*; do
            if [ -x "$f" ]; then
                cands+=("$f")
            fi
        done
    done <<< "$pathdirs"

    # Fixed locations for the common multi-PHP / control-panel layouts.
    for f in \
        /usr/bin/php /usr/bin/php8.* \
        /opt/alt/php*/usr/bin/php \
        /usr/local/lsws/lsphp*/bin/php \
        /opt/plesk/php/*/bin/php \
        /opt/cpanel/ea-php*/root/usr/bin/php; do
        if [ -x "$f" ]; then
            cands+=("$f")
        fi
    done

    # De-duplicate by inode so that `php`, `php8.3`, `/bin/php` etc. that all
    # point at the same on-disk binary collapse to a single entry. Falls back
    # to the path string when a file cannot be stat'ed.
    local -a out=()
    local -a seen_ids=()
    local c cid seen_id found
    for c in ${cands[@]+"${cands[@]}"}; do
        if stat -c '%i' "$c" >/dev/null 2>&1; then
            cid="$(stat -c '%i' "$c")"
        else
            cid="$c"
        fi
        found=0
        for seen_id in ${seen_ids[@]+"${seen_ids[@]}"}; do
            if [ "$seen_id" = "$cid" ]; then
                found=1
                break
            fi
        done
        if [ "$found" -eq 0 ]; then
            out+=("$c")
            seen_ids+=("$cid")
        fi
    done
    if [ "${#out[@]}" -gt 0 ]; then
        printf '%s\n' "${out[@]}"
    fi
    return 0
}

# ─── Per-PHP processing ─────────────────────────────────────────────────────
# Detects version / thread-safety / extension dir / ini dir for one PHP binary
# and (when not dry-run) downloads, verifies and installs the matching .so plus
# the ini file. Records the outcome in the RES_* arrays for the summary table.
process_php() {
    local php="$1"
    local ver ts extdir scan libc arch sofile so_url tmp
    local status

    RES_PHP+=("$php")
    RES_VER+=("")
    RES_STATUS+=("pending")

    # Version (major.minor).
    ver="$("$php" -n -r 'echo PHP_MAJOR_VERSION.".".PHP_MINOR_VERSION;' 2>/dev/null || true)"
    if [ -z "$ver" ]; then
        RES_STATUS+=("")
        RES_STATUS[-1]="error: could not read version"
        log_warn "$php: could not read PHP version"
        return 0
    fi
    RES_VER[-1]="$ver"

    # CloudLinux alt-php (PHP Selector): /opt/alt/php<NN>/usr/bin/php
    local altroot=""
    case "$php" in
        /opt/alt/php*/usr/bin/php) altroot="${php%/usr/bin/php}" ;;
    esac

    # Only PHP 8.1-8.5 are supported by the prebuilt matrix.
    case "$ver" in
        8.1|8.2|8.3|8.4|8.5) ;;
        *)
            if [ -n "$altroot" ]; then
                RES_STATUS[-1]="skipped: no prebuilt phpray.so for PHP $ver (alt-php; 8.1-8.5 only)"
                log_info "$php: PHP $ver has no prebuilt phpray.so (8.1-8.5 only) - skipping this alt-php version"
            else
                RES_STATUS[-1]="skipped: unsupported PHP $ver (need 8.1-8.5)"
                log_warn "$php: PHP $ver not supported (need 8.1-8.5) - skipping"
            fi
            return 0
            ;;
    esac

    # Thread Safety must be disabled (ZTS builds are not provided).
    ts="$("$php" -n -r 'echo (PHP_ZTS ? "enabled" : "disabled");' 2>/dev/null || true)"
    if [ "$ts" != "disabled" ]; then
        RES_STATUS[-1]="skipped: ZTS build (NTS required)"
        log_warn "$php: ZTS build detected - skipping (only NTS is provided)"
        return 0
    fi

    # extension_dir.
    extdir="$("$php" -n -r 'echo ini_get("extension_dir");' 2>/dev/null || true)"
    if [ -z "$extdir" ]; then
        RES_STATUS[-1]="error: could not determine extension_dir"
        log_err "$php: could not determine extension_dir"
        return 0
    fi

    # "Scan for additional .ini files" directory (from php --ini).
    scan="$("$php" --ini 2>/dev/null | sed -n 's/^Scan for additional .ini files in:[[:space:]]*//p' | head -n 1 || true)"

    libc="$(detect_libc)"
    arch="$(detect_arch)"

    log_info "$php -> PHP $ver, libc=$libc, arch=$arch"
    log_info "  extension_dir: $extdir"
    if [ -n "$scan" ]; then
        log_info "  ini scan dir:  $scan"
    else
        log_warn "  ini scan dir:  not detected"
    fi

    sofile="phpray-${ver}-${libc}-${arch}.so"
    so_url="${RELEASE_BASE}/${VERSION_TAG}/${sofile}"

    # Dry-run: report and stop here (no network, no writes).
    if [ "$DRY_RUN" = "1" ]; then
        RES_STATUS[-1]="dry-run: would install $sofile -> $extdir/phpray.so"
        log_info "[dry-run] would download $so_url"
        log_info "[dry-run] would verify checksum and install to $extdir/phpray.so"
        if [ -n "$altroot" ] && [ -d "$altroot/etc/php.d.all" ]; then
            log_info "[dry-run] would write $altroot/etc/php.d.all/phpray.ini (PHP Selector) and link it from ${scan:-<scan dir>}/phpray.ini"
        elif [ -n "$scan" ] && [ -d "$scan" ]; then
            log_info "[dry-run] would write $scan/zz-phpray.ini (or notice if it already exists)"
        fi
        return 0
    fi

    # Download.
    tmp="$WORKDIR/$sofile"
    log_info "Downloading $sofile ..."
    if ! download_file "$so_url" "$tmp"; then
        RES_STATUS[-1]="error: download failed ($so_url)"
        log_err "  download failed: $so_url"
        return 0
    fi

    # Verify.
    if ! verify_sha "$tmp"; then
        RES_STATUS[-1]="error: checksum failed ($sofile)"
        return 0
    fi

    # Install the .so atomically.
    if ! install_file_atomic "$tmp" "$extdir/phpray.so" 644; then
        RES_STATUS[-1]="error: could not install into $extdir"
        log_err "  could not install into $extdir"
        return 0
    fi
    log_ok "  installed $extdir/phpray.so"

    # Write the ini (do not overwrite an existing one).
    if [ -n "$altroot" ] && [ -d "$altroot/etc/php.d.all" ]; then
        # alt-php: extension snippets live in etc/php.d.all (that is what the PHP
        # Selector lists); the scan dir (link/conf) enables them through symlinks.
        if ! write_php_ini_altphp "$altroot" "$scan"; then
            RES_STATUS[-1]="error: could not write $altroot/etc/php.d.all/phpray.ini"
            log_err "  could not write $altroot/etc/php.d.all/phpray.ini"
            return 0
        fi
        RES_STATUS[-1]="installed (alt-php)"
    elif [ -n "$scan" ] && [ -d "$scan" ]; then
        if [ -f "$scan/zz-phpray.ini" ]; then
            log_warn "  ini already exists, not overwriting: $scan/zz-phpray.ini"
            RES_STATUS[-1]="installed (ini already present)"
        else
            if ! write_php_ini "$scan"; then
                RES_STATUS[-1]="error: could not write $scan/zz-phpray.ini"
                log_err "  could not write $scan/zz-phpray.ini"
                return 0
            fi
            log_ok "  wrote $scan/zz-phpray.ini"
            RES_STATUS[-1]="installed"
        fi
        # Debian/Ubuntu keep one conf.d per SAPI (/etc/php/8.4/{cli,fpm,apache2,cgi}/conf.d);
        # php --ini only reports the CLI one, so mirror the ini into the sibling SAPIs.
        write_php_ini_sibling_sapis "$scan"
    else
        log_warn "  no ini scan dir detected - the extension is installed but you must add 'extension=phpray.so' manually"
        RES_STATUS[-1]="installed (no ini dir)"
    fi
    rm -f "$tmp"
    return 0
}

# Copy a file into place atomically (temp in the destination dir, then rename).
install_file_atomic() {
    local src="$1" dest="$2" mode="${3:-644}"
    local dir tmp
    dir="$(dirname "$dest")"
    if [ ! -d "$dir" ]; then
        return 1
    fi
    tmp="$dir/.$(basename "$dest").tmp.$$"
    track_tmp "$tmp"
    if ! cp "$src" "$tmp"; then
        rm -f "$tmp"
        return 1
    fi
    chmod "$mode" "$tmp"
    if ! mv -f "$tmp" "$dest"; then
        rm -f "$tmp"
        return 1
    fi
    return 0
}

# For a Debian-style scan dir (/etc/php/<ver>/<sapi>/conf.d) write the same ini
# into every other SAPI conf.d that exists next to it, unless already present.
write_php_ini_sibling_sapis() {
    local scan="$1" base sapi dir
    case "$scan" in
        /etc/php/*/*/conf.d) ;;
        *) return 0 ;;
    esac
    base="${scan%/*/conf.d}"     # /etc/php/8.4
    for sapi in fpm apache2 cgi litespeed embed phpdbg cli; do
        dir="$base/$sapi/conf.d"
        [ "$dir" = "$scan" ] && continue
        [ -d "$dir" ] || continue
        if [ -f "$dir/zz-phpray.ini" ]; then
            continue
        fi
        if [ "$DRY_RUN" = "1" ]; then
            log_info "[dry-run] would write $dir/zz-phpray.ini"
        elif write_php_ini "$dir"; then
            log_ok "  wrote $dir/zz-phpray.ini"
        else
            log_warn "  could not write $dir/zz-phpray.ini"
        fi
    done
    return 0
}

write_php_ini() {
    local dir="$1" name="${2:-zz-phpray.ini}"
    if [ "$CAGEFS" = "1" ]; then
        # One private 0600 ring per uid in the directory shared with the cages;
        # enabled=0 here = every account opts in with phpray.enabled=1 in .user.ini.
        sed "s|__DIR__|${CAGEFS_DIR}|g; s|__SIZE__|${CAGEFS_RING_SIZE}|" <<'INI' | write_file_atomic "$dir/$name"
; PHPRay (CloudLinux / CageFS layout, written by the installer)
;
; This file is the PHP Selector's catalogue entry. It is NOT linked into the
; server-wide ini scan directory: the extension must be enabled per account
; (selectorctl --enable-extensions=phpray --user=<account>), so a site that did
; not ask for PHPRay never loads it. See docs/install/shared-hosting.md.
extension=phpray.so
; the process may install hooks (this file is only read inside an opted-in cage)
phpray.master_switch=1
; tracing still starts off: the account turns it on with phpray.enabled=1 in .user.ini
phpray.enabled=0
; ring buffer per user (%u = uid, file mode 0600) in the directory mounted into every cage
phpray.output_mode=shm
phpray.shm_path=__DIR__/ring-%u
phpray.shm_size=__SIZE__
; on-demand profiling table written by the collector (shared, read-only for PHP)
phpray.control_path=__DIR__/control
; phpray.profile_mode = sample
; phpray.profile_sample_rate = 3
INI
        return $?
    fi
    write_file_atomic "$dir/$name" <<'INI'
; PHPRay
extension=phpray.so
; phpray.enabled = 1
; phpray.profile_mode = sample
; phpray.profile_sample_rate = 3
INI
}

# CloudLinux alt-php: the snippet goes to /opt/alt/phpNN/etc/php.d.all/phpray.ini
# (the PHP Selector's catalogue of extensions).
#
# On a shared server it is deliberately NOT linked into the version's ini scan
# dir: such a link loads the extension into every account's PHP workers of that
# version, including sites that never asked for it. That is what took a shop
# offline on h1 on 2026-09-20 — phpray.enabled=0 does not prevent MINIT from
# installing hooks, so "disabled" was never inert. Opt in per account instead.
# On a single-tenant server (no CageFS) the link is still the right thing.
write_php_ini_altphp() {
    local altroot="$1" scan="$2"
    local ini="$altroot/etc/php.d.all/phpray.ini"
    if [ -f "$ini" ]; then
        log_warn "  ini already exists, not overwriting: $ini"
    else
        if ! write_php_ini "$altroot/etc/php.d.all" "phpray.ini"; then
            return 1
        fi
        log_ok "  wrote $ini"
    fi
    local sel_ver
    sel_ver="$(basename "$altroot" | sed 's/^php\([0-9]\)/\1./')"
    if [ "$CAGEFS" = "1" ]; then
        # Shared hosting: catalogue only, no server-wide link.
        if [ -L "$scan/phpray.ini" ]; then
            rm -f "$scan/phpray.ini" && log_info "  removed the server-wide link $scan/phpray.ini (opt-in is per account)"
        fi
        [ -f "$scan/zz-phpray.ini" ] && rm -f "$scan/zz-phpray.ini"
        log_info "  catalogued for PHP $sel_ver; enable it for one account with:"
        log_info "      selectorctl --enable-extensions=phpray --user=<account> --version=$sel_ver"
        return 0
    fi
    if [ -n "$scan" ] && [ -d "$scan" ]; then
        if [ -e "$scan/phpray.ini" ] && [ ! -L "$scan/phpray.ini" ]; then
            log_warn "  $scan/phpray.ini exists and is not a symlink, leaving it"
        elif ln -sfn "$ini" "$scan/phpray.ini"; then
            log_ok "  enabled: $scan/phpray.ini -> $ini"
        else
            log_warn "  could not link $scan/phpray.ini -> $ini; enable it with: selectorctl --enable-extensions=phpray --version=$(basename "$altroot" | sed 's/^php\([0-9]\)/\1./')"
        fi
        # an older installer run may have left the generic file: one copy is enough
        if [ -f "$scan/zz-phpray.ini" ]; then
            rm -f "$scan/zz-phpray.ini" && log_info "  removed duplicate $scan/zz-phpray.ini"
        fi
    else
        log_warn "  no ini scan dir detected for $altroot; enable the extension with: selectorctl --enable-extensions=phpray --version=$sel_ver"
    fi
    return 0
}

# ─── CloudLinux / CageFS ────────────────────────────────────────────────────
# Each CageFS user gets a private, empty /dev/shm, so a ring buffer there is
# invisible to the collector on the host. The layout below uses one directory
# mounted into every cage, with one 0600 ring per uid inside it.

detect_cagefs() {
    case "$CAGEFS_MODE" in
        0) CAGEFS=0; return 0 ;;
        1) CAGEFS=1 ;;
        *) if command -v cagefsctl >/dev/null 2>&1; then CAGEFS=1; else CAGEFS=0; fi ;;
    esac
    if [ "$CAGEFS" = "1" ]; then
        log_info "CloudLinux/CageFS layout: shared ${CAGEFS_DIR}, one private ring per account (opt-in with phpray.enabled=1)"
        if [ "$CAGEFS_MODE" = "1" ] && ! command -v cagefsctl >/dev/null 2>&1; then
            log_warn "--cagefs given but cagefsctl was not found; the directory will be created, the cage mount is up to you"
        fi
    fi
    return 0
}

# Shared directory for the per-user rings and the control table.
setup_cagefs_dir() {
    [ "$CAGEFS" = "1" ] || return 0
    log_step "Shared ring directory (${CAGEFS_DIR})"
    if [ "$DRY_RUN" = "1" ]; then
        log_info "[dry-run] would create ${CAGEFS_DIR} (1777) and /etc/tmpfiles.d/phpray.conf"
        log_info "[dry-run] would add ${CAGEFS_DIR} to ${CAGEFS_MP} and run: cagefsctl --force-update"
        return 0
    fi
    install -d -m 1777 "$CAGEFS_DIR" || die "could not create $CAGEFS_DIR"
    chmod 1777 "$CAGEFS_DIR"
    log_ok "  ${CAGEFS_DIR} ready (1777, sticky)"
    # survives a reboot (/run is a tmpfs)
    printf 'd %s 1777 root root -\n' "$CAGEFS_DIR" | write_file_atomic /etc/tmpfiles.d/phpray.conf
    log_ok "  /etc/tmpfiles.d/phpray.conf (recreated on boot)"
    mount_cagefs_dir
}

# Add the directory to cagefs.mp (shared mount for every cage) and apply it.
mount_cagefs_dir() {
    command -v cagefsctl >/dev/null 2>&1 || return 0
    [ -f "$CAGEFS_MP" ] || { log_warn "  $CAGEFS_MP not found - skipping the cage mount"; return 0; }
    if grep -qxF "$CAGEFS_DIR" "$CAGEFS_MP"; then
        log_info "  $CAGEFS_MP already lists $CAGEFS_DIR"
    else
        cp -a "$CAGEFS_MP" "${CAGEFS_MP}.bak-$(date +%Y%m%d-%H%M%S)"
        printf '%s\n' "$CAGEFS_DIR" >> "$CAGEFS_MP"
        log_ok "  added $CAGEFS_DIR to $CAGEFS_MP (backup kept next to it)"
    fi
    # cagefsctl --force-update rebuilds every cage on the server (all accounts at
    # once, typically seconds) - ask unless --yes.
    if [ "$ASSUME_YES" != "1" ]; then
        if [ -t 0 ]; then
            printf 'Run "cagefsctl --force-update" now? It rebuilds the cages of ALL accounts on this server. [y/N] '
            read -r ans || ans=""
        else
            ans="n"
        fi
        case "$ans" in
            y|Y|yes|YES) ;;
            *)
                CAGEFS_REMOUNT_PENDING=1
                log_warn "  NOT applied. PHP in the cages will not see $CAGEFS_DIR until you run:  cagefsctl --force-update"
                return 0
                ;;
        esac
    fi
    log_info "  running cagefsctl --force-update (rebuilds all cages) ..."
    if cagefsctl --force-update >/dev/null 2>&1; then
        log_ok "  cages updated"
        # The cages bind-mount the directory: if it is ever recreated (a new
        # inode), PHP inside the cages keeps the old one until a remount.
        cagefsctl --remount-all >/dev/null 2>&1 || true
    else
        CAGEFS_REMOUNT_PENDING=1
        log_warn "  cagefsctl --force-update reported an error; run it by hand and check the output"
    fi
    return 0
}

# ─── Collector installation ─────────────────────────────────────────────────
# Embedded copies used only for the manual (no dpkg/rpm) path, so the
# one-liner stays self-contained. Kept in sync with configs/collector.toml.example
# and packaging/phpray-collector.service in the repo.
write_collector_config() {
    local shm="/dev/shm/phpray" ctl="/dev/shm/phpray-control"
    if [ "$CAGEFS" = "1" ]; then
        shm="${CAGEFS_DIR}/ring-*"
        ctl="${CAGEFS_DIR}/control"
    fi
    sed "s|__SHM__|${shm}|; s|__CTL__|${ctl}|" <<'TOML' | write_file_atomic "$1"
# PHPRay Collector Configuration
# Managed by the PHPRay installer. Edit before starting the collector.

[input]
jsonl_path = "/tmp/phpray.jsonl"
# ring buffer(s) written by the extension (phpray.shm_path); a glob such as
# "/run/phpray/ring-*" reads one private ring per user (CloudLinux/CageFS layout)
shm_path = "__SHM__"
# on-demand profiling table (phpray.control_path in php.ini)
control_path = "__CTL__"

[storage]
db_path = "/var/lib/phpray/traces.db"

[collector]
poll_interval_ms = 100
flush_interval_s = 5
batch_size = 50
agg_interval_s = 60
mode = "auto"

[retention]
days = 30
max_mb = 4096

[logging]
level = "info"
quiet = false

[auth]
secret = ""

# PHPRay Cloud (fleet console): paste the server key from your account at
# https://app.phpray.dev, set enabled = true and restart the collector:
#   systemctl restart phpray-collector
# [cloud]
# enabled = true
# endpoint = "https://app.phpray.dev"
# server_key = "prk_..."
TOML
}

write_collector_unit() {
    sed "s|__LISTEN_ADDR__|${LISTEN_ADDR}|" <<'UNIT' | write_file_atomic "$1"
[Unit]
Description=PHPRay Collector (traces, dashboard on __LISTEN_ADDR__)
# managed-by: phpray (unit revision 3) - the installer replaces units carrying this marker
Documentation=https://phpray.dev
After=network.target
Wants=network.target

[Service]
Type=simple
User=root
Group=root
ExecStart=/usr/bin/phpray-collector serve -c /etc/phpray/collector.toml -addr __LISTEN_ADDR__
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=5
StartLimitBurst=5
StartLimitIntervalSec=60
RuntimeDirectory=phpray
StateDirectory=phpray
LogsDirectory=phpray
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
# PHP workers write /tmp/phpray.jsonl and the ring buffer under /dev/shm: the collector must see
# the real /tmp (no PrivateTmp) and paths that may not exist yet ("-" = ignore if missing;
# without it systemd fails with status=226/NAMESPACE, seen on AlmaLinux 8).
PrivateTmp=no
ReadWritePaths=/var/lib/phpray /dev/shm -/tmp/phpray.jsonl
ReadOnlyPaths=/etc/phpray
LimitNOFILE=65536
MemoryMax=256M
StandardOutput=journal
StandardError=journal
SyslogIdentifier=phpray-collector

[Install]
WantedBy=multi-user.target
UNIT
}

# Manual collector install: used when neither dpkg nor rpm is available.
# Dashboard login: a JWT secret in [auth] of collector.toml and one admin token
# (printed once, also kept in /etc/phpray/dashboard-token, mode 0600).
setup_dashboard_auth() {
    local cfg=/etc/phpray/collector.toml secret
    secret="$(sed -n 's/^secret[[:space:]]*=[[:space:]]*"\(.*\)".*/\1/p' "$cfg" | head -n 1)"
    if [ -z "$secret" ]; then
        if command -v openssl >/dev/null 2>&1; then
            secret="$(openssl rand -hex 32)"
        else
            secret="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
        fi
        if grep -q '^secret[[:space:]]*=' "$cfg"; then
            sed -i "s|^secret[[:space:]]*=.*|secret = \"$secret\"|" "$cfg"
        else
            printf '\n[auth]\nsecret = "%s"\n' "$secret" >> "$cfg"
        fi
        chmod 0640 "$cfg"
        log_ok "Dashboard auth enabled (secret in $cfg)"
    else
        log_info "Dashboard auth already configured in $cfg"
    fi
    if [ -x /usr/bin/phpray-collector ]; then
        AUTH_TOKEN="$(/usr/bin/phpray-collector token -secret "$secret" -sub admin -role admin -exp 8760h 2>/dev/null | tail -n 1 || true)"
    fi
    if [ -n "$AUTH_TOKEN" ]; then
        (umask 077; printf '%s\n' "$AUTH_TOKEN" > /etc/phpray/dashboard-token)
        log_ok "Admin login token written to /etc/phpray/dashboard-token (valid 365 days)"
    else
        log_warn "Could not create a login token now - later: phpray-collector token -secret <secret from $cfg> -sub admin -role admin -exp 8760h"
    fi
    return 0
}

manual_collector() {
    local arch="$1"
    local bin_url="$RELEASE_BASE/$VERSION_TAG/phpray-collector-linux-$arch"
    local tmp="$WORKDIR/phpray-collector-linux-$arch"

    log_info "Downloading collector binary: $bin_url"
    if ! download_file "$bin_url" "$tmp"; then
        die "Failed to download collector binary: $bin_url"
    fi
    if ! verify_sha "$tmp"; then
        die "Collector binary checksum verification failed"
    fi

    if ! install_file_atomic "$tmp" /usr/bin/phpray-collector 755; then
        die "Could not install the collector binary to /usr/bin"
    fi
    log_ok "Binary installed -> /usr/bin/phpray-collector"

    if [ ! -f /etc/phpray/collector.toml ]; then
        if ! write_collector_config /etc/phpray/collector.toml; then
            die "Could not write /etc/phpray/collector.toml"
        fi
        log_ok "Config installed -> /etc/phpray/collector.toml"
    else
        log_warn "Config already exists, not overwriting: /etc/phpray/collector.toml"
    fi


    mkdir -p /var/lib/phpray /var/log/phpray
    chmod 0750 /var/lib/phpray
    chown root:root /var/lib/phpray 2>/dev/null || true

    COLLECTOR_INSTALLED=1
    return 0
}

# After the package or the manual install: make sure the unit is ours and points at
# LISTEN_ADDR, enable dashboard auth when requested, and reload systemd.
finalize_collector() {
    case "$LISTEN_ADDR" in
        127.0.0.1:*|localhost:*|"[::1]":*) ;;
        *) AUTH=1 ;;   # anything reachable from outside must require a token
    esac
    local unit=/lib/systemd/system/phpray-collector.service
    if [ ! -f "$unit" ]; then
        if ! write_collector_unit "$unit"; then
            die "Could not write $unit"
        fi
        log_ok "Unit installed -> $unit"
    elif grep -q "unit revision 3" "$unit" 2>/dev/null && grep -q -- "-addr ${LISTEN_ADDR}\b" "$unit" 2>/dev/null; then
        log_info "Unit up to date: $unit"
    elif grep -q "phpray-collector" "$unit" 2>/dev/null; then
        # an older phpray unit (installer or package): refresh it, keep a backup
        cp -a "$unit" "$unit.bak" 2>/dev/null || true
        if ! write_collector_unit "$unit"; then
            die "Could not update $unit"
        fi
        log_ok "Unit updated -> $unit (previous copy in $unit.bak)"
    else
        log_warn "Unit already exists and is not ours, not overwriting: $unit"
    fi
    if [ "$AUTH" = "1" ]; then
        setup_dashboard_auth
    fi
    # `phpray status|top|tail…` as documented: a symlink to the collector binary
    if [ -x /usr/bin/phpray-collector ] && [ ! -e /usr/bin/phpray ]; then
        ln -s phpray-collector /usr/bin/phpray 2>/dev/null && log_ok "Command alias: phpray -> phpray-collector"
    fi
    return 0
}

# --uninstall: undo what the installer did. Config and data stay unless --purge.
uninstall_all() {
    log_step "Uninstalling PHPRay"
    local b ver scan extdir
    while IFS= read -r b; do
        [ -n "$b" ] || continue
        ver="$("$b" -n -r 'echo PHP_MAJOR_VERSION.".".PHP_MINOR_VERSION;' 2>/dev/null || true)"
        scan="$("$b" --ini 2>/dev/null | sed -n 's/^Scan for additional .ini files in:[[:space:]]*//p' | head -n 1 || true)"
        extdir="$("$b" -n -r 'echo ini_get("extension_dir");' 2>/dev/null || true)"
        if [ -n "$scan" ] && [ -f "$scan/zz-phpray.ini" ]; then
            rm -f "$scan/zz-phpray.ini" && log_ok "removed $scan/zz-phpray.ini"
            case "$scan" in /etc/php/*/*/conf.d) rm -f "${scan%/*/conf.d}"/*/conf.d/zz-phpray.ini 2>/dev/null ;; esac
        fi
        if [ -n "$extdir" ] && [ -f "$extdir/phpray.so" ]; then
            rm -f "$extdir/phpray.so" && log_ok "removed $extdir/phpray.so (PHP $ver)"
        fi
    done <<< "$(find_php_bins)"
    if command -v systemctl >/dev/null 2>&1; then
        systemctl disable --now phpray-collector 2>/dev/null && log_ok "phpray-collector stopped and disabled"
        if grep -q "managed-by: phpray" /lib/systemd/system/phpray-collector.service 2>/dev/null; then
            rm -f /lib/systemd/system/phpray-collector.service && systemctl daemon-reload 2>/dev/null || true
        fi
    fi
    if command -v dpkg >/dev/null 2>&1 && dpkg -s phpray-collector >/dev/null 2>&1; then
        dpkg -r phpray-collector >/dev/null 2>&1 && log_ok "package phpray-collector removed"
    elif command -v rpm >/dev/null 2>&1 && rpm -q phpray-collector >/dev/null 2>&1; then
        rpm -e phpray-collector >/dev/null 2>&1 && log_ok "package phpray-collector removed"
    fi
    rm -f /usr/bin/phpray-collector /usr/bin/phpray
    if [ "$PURGE" = "1" ]; then
        rm -rf /etc/phpray /var/lib/phpray /var/log/phpray /dev/shm/phpray /dev/shm/phpray-control /tmp/phpray.jsonl
        log_ok "config, data and buffers removed (--purge)"
    else
        log_info "kept /etc/phpray and /var/lib/phpray (use --purge to delete them)"
    fi
    restart_php_services
    log_step "Done"
    return 0
}

install_collector() {
    local arch tmp

    if [ "$NO_COLLECTOR" = "1" ]; then
        log_info "Skipping collector (--no-collector)"
        return 0
    fi

    log_step "Collector"
    arch="$(detect_arch)"

    if [ "$DRY_RUN" = "1" ]; then
        log_info "[dry-run] would install phpray-collector $VERSION_TAG ($arch)"
        if command -v dpkg >/dev/null 2>&1; then
            log_info "[dry-run] via .deb: phpray-collector_${VERSION}_$arch.deb"
        elif command -v rpm >/dev/null 2>&1; then
            log_info "[dry-run] via .rpm: phpray-collector-${VERSION}-${arch}.rpm"
        else
            log_info "[dry-run] via raw binary: phpray-collector-linux-$arch"
        fi
        log_info "[dry-run] would run: systemctl enable --now phpray-collector"
        COLLECTOR_INSTALLED=1
        return 0
    fi

    if command -v dpkg >/dev/null 2>&1; then
        # exact file name from the manifest (nfpm adds the package revision: _0.14.0-2_amd64.deb)
        local deb
        deb="$(grep -o "phpray-collector_[^ ]*_${arch}\.deb" "$WORKDIR/SHA256SUMS" 2>/dev/null | head -n 1 || true)"
        [ -n "$deb" ] || deb="phpray-collector_${VERSION}_${arch}.deb"
        tmp="$WORKDIR/$deb"
        log_info "Installing collector .deb from ${RELEASE_BASE}/${VERSION_TAG}/${deb}"
        local deb_out=""
        if download_file "${RELEASE_BASE}/${VERSION_TAG}/${deb}" "$tmp" \
            && deb_out="$(dpkg -i "$tmp" 2>&1)"; then
            log_ok "Collector .deb installed"
            COLLECTOR_INSTALLED=1
        else
            log_warn ".deb install failed (${deb_out:-download error}) - falling back to manual binary install"
            manual_collector "$arch"
        fi
    elif command -v rpm >/dev/null 2>&1; then
        # nfpm names rpms with the release and the rpm arch: phpray-collector-0.14.0-1.x86_64.rpm
        local rpmarch="$arch"
        case "$arch" in amd64) rpmarch=x86_64 ;; arm64) rpmarch=aarch64 ;; esac
        local rpmf
        rpmf="$(grep -o "phpray-collector-[^ ]*\.${rpmarch}\.rpm" "$WORKDIR/SHA256SUMS" 2>/dev/null | head -n 1 || true)"
        [ -n "$rpmf" ] || rpmf="phpray-collector-${VERSION}-1.${rpmarch}.rpm"
        local rpm_out=""
        tmp="$WORKDIR/$rpmf"
        log_info "Installing collector .rpm from ${RELEASE_BASE}/${VERSION_TAG}/${rpmf}"
        if download_file "${RELEASE_BASE}/${VERSION_TAG}/${rpmf}" "$tmp" \
            && rpm_out="$(rpm -U --replacepkgs --replacefiles "$tmp" 2>&1)"; then
            log_ok "Collector .rpm installed"
            COLLECTOR_INSTALLED=1
        else
            log_warn ".rpm install failed (${rpm_out:-download error}) - falling back to manual binary install"
            manual_collector "$arch"
        fi
    else
        log_info "No dpkg/rpm found - installing collector binary directly"
        manual_collector "$arch"
    fi

    # Enable and start the daemon (best-effort; a container may lack systemd).
    finalize_collector
    if command -v systemctl >/dev/null 2>&1; then
        systemctl daemon-reload 2>/dev/null || true
        if systemctl enable --now phpray-collector 2>/dev/null; then
            log_ok "phpray-collector enabled and started"
        else
            log_warn "systemctl enable --now phpray-collector failed - start it with:"
            log_warn "  phpray-collector serve -c /etc/phpray/collector.toml -addr ${LISTEN_ADDR}"
        fi
    else
        # "Start it manually" without the command is a dead end. Without systemd
        # (containers, WSL, macOS, some shared hosts) people run
        # `phpray-collector`, get the usage screen, and have no way to know the
        # daemon that serves the dashboard is `serve` with a config and an addr.
        log_warn "systemctl not available - start the collector yourself with:"
        log_warn "  phpray-collector serve -c /etc/phpray/collector.toml -addr ${LISTEN_ADDR}"
        log_warn "  (put it under your own supervisor so it survives a reboot)"
    fi
    return 0
}

# ─── PHP / web service restart ──────────────────────────────────────────────
restart_php_services() {
    local -a fpm=() web=()
    local svc s ans

    if [ "$NO_RESTART" = "1" ]; then
        log_info "Skipping PHP/web service restart (--no-restart)"
        return 0
    fi

    log_step "Reloading PHP / web services"

    if ! command -v systemctl >/dev/null 2>&1; then
        log_warn "systemctl not available - reload PHP-FPM / the web server yourself so the extension loads"
        return 0
    fi

    # PHP-FPM: a graceful reload (workers finish their request, then restart with the new ini).
    # Done without asking, also from a piped `curl | bash` - otherwise the install silently does nothing.
    for svc in php-fpm php8.1-fpm php8.2-fpm php8.3-fpm php8.4-fpm php8.5-fpm \
               php81-php-fpm php82-php-fpm php83-php-fpm php84-php-fpm php85-php-fpm; do
        if systemctl is-active --quiet "$svc" 2>/dev/null; then
            fpm+=("$svc")
        fi
    done
    # Web servers embedding PHP (mod_php, LiteSpeed): a restart interrupts connections, so ask (or --yes).
    for svc in apache2 httpd lsws lshttpd; do
        if systemctl is-active --quiet "$svc" 2>/dev/null; then
            web+=("$svc")
        fi
    done

    for s in ${fpm[@]+"${fpm[@]}"}; do
        if systemctl reload "$s" 2>/dev/null; then
            log_ok "Reloaded $s (graceful)"
        elif systemctl restart "$s" 2>/dev/null; then
            log_ok "Restarted $s"
        else
            log_warn "Could not reload $s - run: systemctl reload $s"
        fi
    done

    if [ "${#web[@]}" -eq 0 ]; then
        [ "${#fpm[@]}" -eq 0 ] && log_info "No running PHP-FPM / web services detected - nothing to reload"
        return 0
    fi

    log_info "Web server(s) embedding PHP detected: ${web[*]}"
    if [ "$ASSUME_YES" != "1" ]; then
        if [ -t 0 ]; then
            printf 'Restart %s now so the extension loads? [y/N] ' "${web[*]}"
            read -r ans || ans=""
        else
            ans="n"
        fi
        case "$ans" in
            y|Y|yes|YES) ;;
            *)
                log_warn "NOT restarted: ${web[*]}. The extension loads only after:  systemctl restart ${web[*]}   (or re-run with --yes)"
                return 0
                ;;
        esac
    fi
    for s in "${web[@]}"; do
        if systemctl restart "$s" 2>/dev/null; then
            log_ok "Restarted $s"
        else
            log_warn "Failed to restart $s"
        fi
    done
    return 0
}

# ─── Summary ────────────────────────────────────────────────────────────────
print_summary() {
    local i cv

    log_step "Summary"
    printf '%-40s %-8s %s\n' "PHP" "VER" "STATUS"
    printf '%-40s %-8s %s\n' "----------------------------------------" "--------" "----------------------------------------"

    if [ "${#RES_PHP[@]}" -eq 0 ]; then
        printf '%-40s %-8s %s\n' "-" "-" "no PHP installations detected"
    else
        for i in "${!RES_PHP[@]}"; do
            local ver="${RES_VER[$i]}"
            local status="${RES_STATUS[$i]}"
            printf '%-40s %-8s %s\n' "${RES_PHP[$i]}" "$ver" "$status"
        done
    fi

    # Wersja ROZSZERZENIA, nie tylko kolektora.
    #
    # 23.09.2026 zainstalowalem PHPRay-a na czystym Debianie dokladnie tak, jak
    # mowi dokumentacja. Instalator napisal "Release: v0.15.8", a `php -m`
    # i `phpversion("phpray")` pokazaly **0.15.5** — bo plik .so publikowany
    # jako 0.15.6, 0.15.7 i 0.15.8 jest bajt w bajt tym samym plikiem co 0.15.5
    # (ta sama suma SHA-256). Rozszerzenie po prostu nie zmienialo sie miedzy
    # tymi wydaniami i binarka zostala przeniesiona dalej.
    #
    # Kopiowanie niezmienionej binarki jest w porzadku. Niepowiedzenie o tym
    # nie jest: uwazny deweloper — czyli dokladnie nasz odbiorca — widzi
    # rozbieznosc i uznaje, ze instalacja sie nie udala. Wypisujemy wiec
    # wersje, ktora NAPRAWDE siedzi w PHP.
    for i in "${!RES_PHP[@]}"; do
        if [ "${RES_STATUS[$i]}" = "installed" ]; then
            ev="$("${RES_PHP[$i]}" -r 'echo phpversion("phpray");' 2>/dev/null || true)"
            if [ -n "$ev" ]; then
                if [ -n "${VERSION:-}" ] && [ "$ev" != "$VERSION" ]; then
                    log_info "Extension: $ev (release $VERSION ships this build unchanged)"
                else
                    log_info "Extension: $ev"
                fi
            fi
            break
        fi
    done

    if [ "$COLLECTOR_INSTALLED" = "1" ]; then
        if command -v phpray-collector >/dev/null 2>&1; then
            cv="$(phpray-collector --version 2>/dev/null || true)"
            if [ -z "$cv" ]; then
                cv="$(phpray-collector version 2>/dev/null || true)"
            fi
            if [ -n "$cv" ]; then
                log_info "Collector: $cv"
            else
                log_info "Collector: installed (version query returned nothing)"
            fi
        else
            log_info "Collector: installed but not on PATH yet (re-open a shell to use it)"
        fi
    else
        log_info "Collector: not installed"
    fi
    return 0
}

# ─── Argument parsing ───────────────────────────────────────────────────────
parse_args() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --version)
                if [ $# -lt 2 ]; then
                    die "--version requires a value, e.g. --version v1.2.3"
                fi
                VERSION_ARG="$2"
                shift 2
                ;;
            --no-collector)
                NO_COLLECTOR=1
                shift
                ;;
            --no-restart)
                NO_RESTART=1
                shift
                ;;
            --yes)
                ASSUME_YES=1
                shift
                ;;
            --cagefs)
                CAGEFS_MODE=1
                shift
                ;;
            --no-cagefs)
                CAGEFS_MODE=0
                shift
                ;;
            --listen)
                [ -n "${2:-}" ] || die "--listen needs an address, e.g. 0.0.0.0:9191"
                LISTEN_ADDR="$2"
                shift 2
                ;;
            --auth)
                AUTH=1
                shift
                ;;
            --uninstall)
                UNINSTALL=1
                shift
                ;;
            --purge)
                PURGE=1
                shift
                ;;
            --dry-run)
                DRY_RUN=1
                shift
                ;;
            -h|--help)
                usage
                exit 0
                ;;
            *)
                die "Unknown option: $1 (see --help)"
                ;;
        esac
    done
}

# ─── Root check ─────────────────────────────────────────────────────────────
check_root() {
    if [ "$DRY_RUN" = "1" ]; then
        return 0
    fi
    if [ "${EUID:-$(id -u)}" -ne 0 ]; then
        die "This installer must run as root. Re-run with: sudo bash -s -- ... (or use --dry-run as a regular user)"
    fi
    return 0
}

# ─── Main ───────────────────────────────────────────────────────────────────
main() {
    parse_args "$@"

    log_step "PHPRay installer"
    if [ "$DRY_RUN" = "1" ]; then
        log_info "DRY RUN - no changes will be made"
    fi

    check_root
    if [ "$UNINSTALL" = "1" ]; then
        uninstall_all
        exit 0
    fi

    # Scratch space for downloads (removed separately with rm -rf in cleanup,
    # so it is intentionally NOT added to TMPFILES which uses rm -f).
    WORKDIR="$(mktemp -d /tmp/phpray-install.XXXXXX)"

    resolve_version

    # Download the checksum manifest once, up front.
    if [ "$DRY_RUN" != "1" ] && [ "$VERSION_TAG" != "(latest)" ]; then
        log_info "Downloading SHA256SUMS ..."
        if ! download_file "${RELEASE_BASE}/${VERSION_TAG}/SHA256SUMS" "$WORKDIR/SHA256SUMS"; then
            die "Could not download SHA256SUMS from ${RELEASE_BASE}/${VERSION_TAG}/SHA256SUMS"
        fi
    fi

    detect_cagefs
    setup_cagefs_dir

    log_step "PHP extensions"
    local -a php_bins=()
    local -a unique_bins=()
    local -a seen_targets=()
    local b key k
    while IFS= read -r b; do
        if [ -n "$b" ]; then
            php_bins+=("$b")
        fi
    done <<< "$(find_php_bins)"

    if [ "${#php_bins[@]}" -eq 0 ]; then
        log_warn "No PHP installations detected (looked on PATH and in the common cPanel/Plesk/LiteSpeed/CloudLinux/DirectAdmin locations)"
    fi

    # Collapse PHP paths that share the same effective install target so we do
    # not download/install/verify the identical .so more than once.
    for b in ${php_bins[@]+"${php_bins[@]}"}; do
        key="$(php_target_key "$b")"
        if [ -z "$key" ]; then
            key="<unknown>:$b"
        fi
        k=0
        for seen in ${seen_targets[@]+"${seen_targets[@]}"}; do
            if [ "$seen" = "$key" ]; then
                k=1
                break
            fi
        done
        if [ "$k" -eq 0 ]; then
            seen_targets+=("$key")
            unique_bins+=("$b")
        fi
    done

    for b in ${unique_bins[@]+"${unique_bins[@]}"}; do
        process_php "$b"
    done

    install_collector
    restart_php_services
    print_summary

    # Surface a hard failure if nothing usable happened (and we were asked to
    # install an extension).
    if [ "$DRY_RUN" != "1" ]; then
        local ok=0
        for i in ${RES_STATUS[@]+"${RES_STATUS[@]}"}; do
            case "$i" in
                installed*) ok=1 ;;
            esac
        done
        if [ "$ok" -eq 0 ] && [ "${#unique_bins[@]}" -gt 0 ]; then
            die "No PHP extension could be installed - see the errors above"
        fi
    fi

    log_step "Done"
    log_info "If you just installed the extension, open a new shell and verify with: php -m | grep phpray"
    print_next_steps
    return 0
}

# What to do now: where the local dashboard is and how to connect to the cloud console.
print_next_steps() {
    log_step "Next steps"
    if [ "$CAGEFS" = "1" ]; then
        printf '%s\n' \
            "Shared hosting (CageFS): the extension is loaded for every account but OFF by default." \
            "  turn it on for one account:  echo 'phpray.enabled=1' >> /home/<account>/domains/<domain>/public_html/.user.ini" \
            "  (mod_php instead of FPM/lsphp: php_value phpray.enabled 1 in .htaccess; the worker picks it up within ~60 s)" \
            "  each account writes its own ring ${CAGEFS_DIR}/ring-<uid> (0600); the collector reads them all" \
            ""
        if [ "$CAGEFS_REMOUNT_PENDING" = "1" ]; then
            printf '%s\n' \
                "  ACTION REQUIRED: run  cagefsctl --force-update  - until then PHP inside the cages cannot see ${CAGEFS_DIR}" \
                ""
        fi
    fi
    local port="${LISTEN_ADDR##*:}" host="${LISTEN_ADDR%:*}" pubip
    case "$host" in
        0.0.0.0|"[::]"|"") pubip="$(hostname -I 2>/dev/null | awk '{print $1}')"; [ -n "$pubip" ] || pubip="<server-ip>" ;;
        *) pubip="$host" ;;
    esac
    if [ "$AUTH" = "1" ]; then
        printf '%s\n' \
            "Dashboard:  http://${pubip}:${port}/   (login token below - keep it; also in /etc/phpray/dashboard-token)" \
            "  token:  ${AUTH_TOKEN:-<see /etc/phpray/dashboard-token>}" \
            "  firewall: allow TCP ${port} only from your own IP; the dashboard is plain HTTP - put it behind HTTPS (Caddy/nginx) for the internet" \
            "  status / traces:   curl -s -H \"Authorization: Bearer <token>\" http://${pubip}:${port}/api/v1/health   |   phpray top" \
            ""
    else
        printf '%s\n' \
            "Local dashboard (this server only):  http://127.0.0.1:${port}/" \
            "  from your laptop:  ssh -L ${port}:127.0.0.1:${port} root@$(hostname -f 2>/dev/null || hostname)   then open http://localhost:${port}/" \
            "  public IP with a login instead:  re-run with  --listen 0.0.0.0:${port}" \
            "  status / traces:   curl -s http://127.0.0.1:${port}/api/v1/health   |   phpray top" \
            ""
    fi
    printf '%s\n' \
        "PHPRay Cloud (all servers in one console, https://app.phpray.dev):" \
        "  1. get a server key (prk_...) for this server from your PHPRay Cloud account" \
        "  2. edit the [cloud] section that is ALREADY in /etc/phpray/collector.toml" \
        "     (do not add a second one):  enabled = true  server_key = \"prk_...\"" \
        "  3. systemctl restart phpray-collector   (the dashboard header then shows \"Cloud connected\")" \
        "" \
        "Docs: https://phpray.dev/docs/   Enable/disable per site: phpray.enabled=0 in .user.ini or .htaccess"
    return 0
}

main "$@"
