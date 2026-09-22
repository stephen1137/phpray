#!/bin/sh
# Post-install script for the phpray-collector package.
#
# Contract (per packaging spec):
#   - daemon-reload so systemd picks up the new unit
#   - enable the unit, but do NOT start it (the caller / user decides when to
#     start, e.g. the one-line installer runs `systemctl enable --now` later)
#
# This runs as root inside the package manager's script environment. It is
# written to be POSIX sh and must not fail the install on a best-effort step.
set -eu

# Reload systemd unit files if systemd is present. Ignore failure on
# non-systemd distros (some minimal images have no running systemd).
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload 2>/dev/null || true
fi

# Enable, but do not start, so a fresh install does not surprise the user with
# a running daemon before they have configured it.
if command -v systemctl >/dev/null 2>&1; then
    systemctl enable phpray-collector.service 2>/dev/null || true
fi

exit 0


echo "PHPRay collector installed. Local dashboard: http://127.0.0.1:9191/ (bind is localhost; use an SSH tunnel)."
echo "Cloud console: add [cloud] with your server key to /etc/phpray/collector.toml, then: systemctl restart phpray-collector"
