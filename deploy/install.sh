#!/usr/bin/env bash
# Install aprgo from a freshly-built binary into /usr/bin and set up
# the state directory and systemd service. Run as root.
set -euo pipefail

BIN_SRC="${1:-./aprgo}"
if [[ ! -x "$BIN_SRC" ]]; then
    echo "usage: $0 <path to aprgo binary>" >&2
    exit 1
fi

# Resolve relative script dir so install.sh works from anywhere.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# 1. State dir (owned root since service runs as root for rfcomm/bluetoothctl).
install -d -o root -g root -m 0700 /var/lib/aprgo

# 2. Binary
install -m 0755 -o root -g root "$BIN_SRC" /usr/bin/aprgo

# 3. Systemd unit. /etc/systemd/system is the documented location for
#    locally-installed units; it also overrides /usr/lib/systemd/system
#    so a later package install won't fight this one.
install -m 0644 -o root -g root "$SCRIPT_DIR/aprgo.service" /etc/systemd/system/aprgo.service
systemctl daemon-reload
systemctl enable aprgo

# 4. Start now (or restart if already running from a prior install).
#    Skip in chroots / container builds where PID 1 isn't systemd.
if [[ -d /run/systemd/system ]]; then
    if systemctl is-active --quiet aprgo; then
        systemctl try-restart aprgo || true
    else
        systemctl start aprgo || true
    fi
fi

IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
[[ -z "$IP" ]] && IP="$(hostname)"
[[ -z "$IP" ]] && IP="localhost"
echo
echo "  aprgo installed and started."
echo
echo "  Open the web console (recommended):"
echo "    https://${IP}:14439/   (accept the self-signed cert warning once — full access)"
echo
echo "  Restricted fallback if you can't reach HTTPS:"
echo "    http://${IP}:14473/    (read-only — Settings/Messages/Bulletins redirect to HTTPS)"
echo
echo "  Default login:   admin / admin   (change immediately on first sign-in)"
