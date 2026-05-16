# aprgo

A self-contained APRS iGate, digipeater, and operator console in a single Go binary.

A single process that owns the TNC, talks to APRS-IS, makes gating decisions, runs beacons, and serves the operator web console — no separate daemon, no log tailing, no IPC.

## Status

Alpha. Running stably as a fill-in iGate + digi on Debian 12 + a Mobilinkd TNC3.

## Quick start (Debian / Ubuntu)

```bash
# 1. Build (Go 1.22+ required)
git clone https://github.com/cartpauj/aprgo
cd aprgo
CGO_ENABLED=0 go build -ldflags="-s -w -X main.Version=$(git describe --tags --always)" -trimpath -o aprgo ./cmd/aprgo

# 2. Install
sudo apt install bluez bluez-tools     # only if you'll use a Bluetooth TNC
sudo ./deploy/install.sh ./aprgo
sudo systemctl start aprgo

# 3. Open the console
# http://<host>:14439/    user: admin   pass: admin  (change immediately)
```

The first time you open the UI, a wizard walks you through: callsign → location → TNC → mode → beacon → done.

Cross-compile for Raspberry Pi (arm64):

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -trimpath -o aprgo-arm64 ./cmd/aprgo
```

Pure-Go SQLite means no C toolchain required for cross-builds.

## Hardware compatibility

| TNC | Connection | Notes |
|---|---|---|
| Mobilinkd TNC3 | Bluetooth Classic (RFCOMM) | wizard handles pair + bind + supervisor |
| Mobilinkd TNC4 | Bluetooth Classic or WiFi/TCP KISS | dual-mode, BLE supported too |
| Mobilinkd Bluetooth APRS TNC (new) | BLE-KISS GATT | wizard's BLE scan finds it; iOS-only hardware works |
| Kenwood TH-D74 / TH-D75A | USB serial or Bluetooth | KISS mode |
| Kenwood TM-D710G / TM-D750A | USB serial via cable | built-in TNC |
| NinoTNC | USB serial | popular DIY |
| TNC-Pi / TNC-X (Coastal Chipworks, discontinued) | USB serial or Pi GPIO | huge installed base |
| MFJ-1270X | USB serial | KISS mode |
| ESP32 KISS TNC | USB CDC-ACM at 115200 baud | DIY |
| VR-N76 handheld | USB serial or Bluetooth | built-in KISS TNC |
| **Soundcard** (Digirig, SignaLink USB, DRAWS, UDRC, DMK URI, RA-35, DINAH, SHARI, etc.) | Run [Direwolf](https://github.com/wb2osz/direwolf) on the same box | aprgo connects to Direwolf's TCP KISS port (default `localhost:8001`) |
| Any networked KISS source ([Direwolf](https://github.com/wb2osz/direwolf), [tnc-server](https://github.com/chrissnell/tnc-server), kissnetd) | TCP KISS | wizard's "Network (TCP KISS)" option |

## Modes

- **RX-only iGate** — listen on RF, forward to APRS-IS. No TX. Safest first run; use passcode `-1`.
- **TX iGate** — gate RF→IS, optionally relay APRS-IS messages to local stations.
- **Fill-in digipeater + iGate** — repeats `WIDE1-1` only, gates to IS. Recommended for most home iGates.
- **Full digipeater** — currently handles `WIDE1-1` only; native `WIDE2-N` is on the roadmap. Only enable from a high mountaintop site.

## Security model

- aprgo runs as `root` (required for `rfcomm` + `bluetoothctl`) with systemd hardening (`ProtectSystem=strict`, `NoNewPrivileges`, `LockPersonality`, `MemoryDenyWriteExecute`, restricted address families and capabilities).
- APRS-IS passcode is stored plaintext in `/var/lib/aprgo/state.json` (mode 0600). This is a protocol limitation — the IS server needs the cleartext value.
- Default web login is `admin` / `admin`. **Change it immediately on first login** — the dashboard banner reminds you.
- Web UI has CSRF (token + Origin check + `SameSite=Strict`), session HMAC, per-IP login rate limiting, and password-change session invalidation.
- HTTP is plaintext by default. Put aprgo behind a reverse proxy (nginx, Caddy, Traefik) for TLS if exposed beyond a trusted LAN.

## Files

- `/usr/local/bin/aprgo` — the binary
- `/etc/systemd/system/aprgo.service` — service unit
- `/var/lib/aprgo/state.json` — config (callsign, passcode, location, beacon text, gating flags, password hash, session key)
- `/var/lib/aprgo/db.sqlite[-wal,-shm]` — SQLite store (heard stations, packets, messages)

## Operations

### Logs

```bash
journalctl -u aprgo          # all
journalctl -u aprgo -f       # follow
journalctl -u aprgo --since '1 hour ago'
```

### Backup

```bash
sudo systemctl stop aprgo
sudo cp /var/lib/aprgo/state.json     ~/aprgo-state-$(date +%F).json
sudo cp /var/lib/aprgo/db.sqlite      ~/aprgo-db-$(date +%F).sqlite
sudo systemctl start aprgo
```

### Upgrade

```bash
sudo systemctl stop aprgo
sudo install -m 0755 ./aprgo /usr/local/bin/aprgo
sudo systemctl start aprgo
```

`state.json` is forward-compatible (new fields added with sensible defaults on first read). DB schema migrations between phases may require deleting `/var/lib/aprgo/db.sqlite*` until a migrations table is added — back it up first if you want to keep history.

### Uninstall

```bash
sudo systemctl disable --now aprgo
sudo rm /usr/local/bin/aprgo
sudo rm /etc/systemd/system/aprgo.service
sudo rm -rf /var/lib/aprgo      # WARNING: deletes config + DB
```

## Troubleshooting

**"TNC won't connect":**
- USB: check `ls /dev/ttyUSB* /dev/ttyACM*`, verify the device exists and the `aprgo` process (root) can open it.
- Bluetooth: check pairing with `sudo bluetoothctl paired-devices`; verify `bluez` and `bluez-tools` are installed (`apt install bluez bluez-tools`). The settings page surfaces the last error.
- TCP: confirm Direwolf/tnc-server is running on the host:port you configured (`ss -ltn | grep 8001`).

**"APRS-IS won't connect":**
- Check the settings page — if it shows the red banner *"APRS-IS rejected your passcode"*, your passcode doesn't match your callsign. Generate the correct one at [apps.magicbug.co.uk/passcode](https://apps.magicbug.co.uk/passcode/).
- Network: confirm outbound TCP 14580 is open. `nc -zv noam.aprs2.net 14580`.

**"No packets received":**
- Check the dashboard — if it's empty after a few minutes, the TNC may be receiving audio but failing to decode (squelch closed, deviation off, wrong band). For soundcard setups, tune Direwolf's audio levels first.

**"Wrong passcode":**
- Passcode is callsign-specific (without SSID). `N0CALL-10` and `N0CALL-1` share the same passcode (it's for `N0CALL`).
- `-1` means "RX-only at the server level" — uploads will be rejected; IS-side will work as a listener.

## Project layout

```
cmd/aprgo/         binary entry + main
internal/
  ax25/            KISS framing + AX.25 UI frame encode/decode
  aprs/            info-field parser (position, Mic-E, message, telemetry)
  bus/             typed pub/sub fanout
  state/           persistent config + live-reload subscribers
  store/           SQLite stations/packets/messages
  auth/            cookie session (HMAC + password-generation binding)
  igate/           APRS-IS client (reconnect, filter, logresp parsing)
  rf/              KISS reader/writer for serial / Bluetooth / TCP, plus btbind supervisor
  btle/            BLE-KISS GATT client
  tnc/             BlueZ subprocess wrappers (scan / pair / SDP / rfcomm)
  gate/            RF↔IS gating + digipeat decision engine (pure functions)
  beacon/          periodic beacon scheduler
  server/          HTTP routes, SSE feed, wizard, rate limiters, CSRF, sanitizers
deploy/            systemd unit, install.sh, debian/control skeleton
web/               embed.FS for templates + static assets
```

## License

MIT — see [LICENSE](LICENSE).

The APRS info-field parser is derived from aprx's `parse_aprs.c` (Matti Aarnio OH2MQK, MIT-licensed).
