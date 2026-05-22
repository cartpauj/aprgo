#!/bin/sh
# Preremove — runs before files are deleted.
set -e

# Stop + disable the unit so a leftover service doesn't try to start on
# the next reboot after the binary is gone. Idempotent.
if command -v systemctl >/dev/null 2>&1; then
    # deb passes "remove" or "upgrade"; rpm passes "0" for full remove,
    # "1" for upgrade. We only stop on outright removal.
    case "${1:-}" in
        remove|0)
            systemctl stop    aprgo.service >/dev/null 2>&1 || true
            systemctl disable aprgo.service >/dev/null 2>&1 || true
            ;;
    esac
fi

exit 0
