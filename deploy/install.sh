#!/usr/bin/env bash
# Install aprgo from a freshly-built binary into /usr/local/bin and set up
# the system user, state directory, and systemd service. Run as root.
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
chown root:root /var/lib/aprgo

# 2. Binary
install -m 0755 -o root -g root "$BIN_SRC" /usr/local/bin/aprgo

# 3. Systemd unit
install -m 0644 -o root -g root "$SCRIPT_DIR/aprgo.service" /etc/systemd/system/aprgo.service
systemctl daemon-reload
systemctl enable aprgo

IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
[[ -z "$IP" ]] && IP="$(hostname)"
[[ -z "$IP" ]] && IP="localhost"
echo
echo "  aprgo installed."
echo "  Start it with:   sudo systemctl start aprgo"
echo "  Then open:       http://${IP}:14439/"
echo "  Default login:   admin / admin (change immediately on first run)"
