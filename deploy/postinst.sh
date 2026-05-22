#!/bin/sh
# Postinstall — runs after the package is unpacked by dpkg or rpm.
#
# Idempotent: safe to re-run on upgrade. Existing /var/lib/aprgo contents
# are preserved.
set -e

# 1. State directory (root:root 0700). Holds state.json, aprgo.conf,
#    db.sqlite, and tls/. Created here so package upgrades don't have to
#    list the dir (dpkg would complain on ownership/mode mismatch).
if [ ! -d /var/lib/aprgo ]; then
    install -d -o root -g root -m 0700 /var/lib/aprgo
else
    chown root:root /var/lib/aprgo
    chmod 0700      /var/lib/aprgo
fi

# 2. systemd integration — enable for boot, then start (fresh install) or
#    restart (upgrade) so the operator doesn't have to do it manually.
#    deb passes "configure" on install + upgrade, with $2 = previous version
#    on upgrade and empty on first install.
#    rpm passes "1" on install, "2" on upgrade.
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true

    is_install=0
    is_upgrade=0
    case "${1:-}" in
        configure)
            if [ -z "${2:-}" ]; then is_install=1; else is_upgrade=1; fi
            ;;
        1) is_install=1 ;;
        2) is_upgrade=1 ;;
    esac

    if [ "$is_install" = "1" ] || [ "$is_upgrade" = "1" ]; then
        systemctl enable aprgo.service >/dev/null 2>&1 || true
    fi

    # Skip start/restart in chroots / container builds where PID 1 isn't
    # systemd — /run/systemd/system only exists under a real systemd PID 1.
    if [ -d /run/systemd/system ]; then
        if [ "$is_install" = "1" ]; then
            systemctl start aprgo.service >/dev/null 2>&1 || true
        elif [ "$is_upgrade" = "1" ]; then
            # If the operator was running it, restart to pick up the new
            # binary. If it's currently stopped (including the v0.0.6 case
            # where the prior postinst forgot to start it), start it now —
            # the unit is enabled, so the operator's clear intent is that
            # it should be running.
            if systemctl is-active --quiet aprgo.service; then
                systemctl try-restart aprgo.service >/dev/null 2>&1 || true
            else
                systemctl start aprgo.service >/dev/null 2>&1 || true
            fi
        fi
    fi
else
    echo "aprgo: WARNING — systemctl not found. The package installed the binary"
    echo "       and the systemd unit, but you'll need to wire up startup yourself."
fi

# 3. Friendly banner — printed on both install and upgrade so the operator
#    always sees how to reach the console. Wording adapts to which case.
if [ "$is_install" = "1" ]; then
    banner_head="aprgo installed and started."
elif [ "$is_upgrade" = "1" ]; then
    banner_head="aprgo upgraded and restarted."
else
    banner_head=""
fi

if [ -n "$banner_head" ]; then
    IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
    [ -z "$IP" ] && IP="$(hostname 2>/dev/null)"
    [ -z "$IP" ] && IP="localhost"
    echo
    echo "  ${banner_head}"
    echo
    echo "  Web console — aprgo listens on two ports:"
    echo
    echo "    HTTP  (http://${IP}:14473/) — read-only browsing of Dashboard,"
    echo "          Map, Stations, Stats, Logs, plus the first-run setup wizard."
    echo "          Settings, Messages, Bulletins will redirect to HTTPS."
    echo
    echo "    HTTPS (https://${IP}:14439/) — full access. The cert is self-signed,"
    echo "          so your browser will warn once — click through to continue."
    echo
    if [ "$is_install" = "1" ]; then
        echo "  Default login:   admin / admin   (change on first sign-in)"
    fi
    echo "  Logs:            journalctl -u aprgo -f"
    echo
fi

exit 0
