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

# 2. systemd integration — register the unit. Don't auto-start; the
#    operator should reach the first-run wizard at their own pace.
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
    if [ "$1" = "configure" ] || [ "$1" = "1" ] || [ "$1" = "2" ]; then
        # deb passes "configure" on install + upgrade.
        # rpm passes "1" on install, "2" on upgrade.
        systemctl enable aprgo.service >/dev/null 2>&1 || true
    fi
else
    echo "aprgo: WARNING — systemctl not found. The package installed the binary"
    echo "       and the systemd unit, but you'll need to wire up startup yourself."
fi

# 3. Soft-warn if Bluetooth tooling is missing. The package declared
#    bluez/bluez-tools as Recommends; this catches the case where the
#    operator opted out and might be confused why the wizard's BT step
#    fails later.
if ! command -v bluetoothctl >/dev/null 2>&1; then
    echo "aprgo: NOTE — bluetoothctl was not found in PATH."
    echo "       Bluetooth TNCs (Mobilinkd, etc.) won't work until you install:"
    echo "         apt install bluez bluez-tools     # Debian/Ubuntu/RPi OS"
    echo "         dnf install bluez bluez-tools     # Fedora/RHEL"
    echo "       Serial and TCP-KISS TNCs are unaffected."
fi

# 4. Friendly first-run hint. Only on fresh install (not upgrades).
if [ "$1" = "configure" ] && [ -z "${2:-}" ]; then
    # First-time deb install (no previously-installed version arg).
    show_hint=1
elif [ "$1" = "1" ]; then
    # First-time rpm install.
    show_hint=1
else
    show_hint=0
fi

if [ "$show_hint" = "1" ]; then
    IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
    [ -z "$IP" ] && IP="$(hostname 2>/dev/null)"
    [ -z "$IP" ] && IP="localhost"
    echo
    echo "  aprgo installed."
    echo
    echo "  Start it:        sudo systemctl start aprgo"
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
    echo "  Default login:   admin / admin   (change on first sign-in)"
    echo "  Logs:            journalctl -u aprgo -f"
    echo
fi

exit 0
