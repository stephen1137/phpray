#!/usr/bin/env bash
# PHPRay Uninstall Script — convenience wrapper
# Usage: sudo ./scripts/uninstall.sh
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec "$SCRIPT_DIR/install.sh" --uninstall
