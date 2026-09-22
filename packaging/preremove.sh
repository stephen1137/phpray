#!/bin/sh
# Pre-remove script for the phpray-collector package.
#
# Contract (per packaging spec):
#   - stop the collector (so it is not running while its files are deleted)
#   - disable the unit so it does not come back on next boot
#
# Runs as root inside the package manager's script environment. POSIX sh, and
# tolerant of non-systemd environments (best-effort, never fails the removal).
set -eu

if command -v systemctl >/dev/null 2>&1; then
    # Stop only if it is currently active; stop on a stopped unit is harmless
    # but we guard to keep the log quiet on systems where it never ran.
    if systemctl is-active --quiet phpray-collector.service 2>/dev/null; then
        systemctl stop phpray-collector.service 2>/dev/null || true
    fi
    systemctl disable phpray-collector.service 2>/dev/null || true
    systemctl daemon-reload 2>/dev/null || true
fi

exit 0
