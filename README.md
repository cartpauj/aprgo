# aprgo

A self-contained APRS iGate, digipeater, and operator console in a single Go binary.

One process owns the TNC, talks to APRS-IS, makes gating decisions, runs beacons, and serves the operator web console — no separate daemon, no log tailing, no IPC.

**Status:** Alpha. Running stably on Debian 12 + a Mobilinkd TNC3.

> **New to APRS?** APRS is the ham radio digital data network for position reports, short messages, weather telemetry, and emergency comms over VHF. An **iGate** is a station that bridges radio traffic to the internet (APRS-IS) and back. A **digipeater** is a station that retransmits packets so they reach further than a single hop. aprgo can be either, or both.

---

# For operators

## Quick start (Debian / Ubuntu)

```bash
# 1. Build (Go 1.26+ required)
git clone https://github.com/cartpauj/aprgo
cd aprgo
CGO_ENABLED=0 go build \
  -ldflags="-s -w -X main.Version=$(git describe --tags --always)" \
  -trimpath -o aprgo ./cmd/aprgo

# 2. Install
sudo apt install bluez bluez-tools     # only if you'll use a Bluetooth TNC
sudo ./deploy/install.sh ./aprgo
sudo systemctl start aprgo

# 3. Open the console
# http://<host>:14473/    user: admin   pass: admin  (change immediately)
#
# After first-run setup completes, aprgo redirects all non-loopback HTTP to
# https://<host>:14439/ — the operator console is HTTPS-only post-setup.
# The cert is self-signed; your browser will warn once. Click through.
```

Cross-compile for Raspberry Pi (arm64):

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -trimpath -o aprgo-arm64 ./cmd/aprgo
```

Pure-Go SQLite means no C toolchain required for cross-builds.

## First-run wizard

Open `http://<host>:14439/` after install and a setup wizard intercepts. Each step pre-fills sensible defaults; you can change everything later under **Settings**.

| Step | What you set | Notes |
|---|---|---|
| **Identity** | Callsign-SSID (e.g. `N0CALL-10`) + APRS-IS passcode | Generate the passcode at [apps.magicbug.co.uk/passcode](https://apps.magicbug.co.uk/passcode/). Passcode is tied to the *base* callsign — `N0CALL-10` and `N0CALL-1` share the same number. Use `-1` to listen without ever uploading to APRS-IS. |
| **Location** | Click the map (or type lat/lon) + APRS-IS filter radius (km) | Position is used for the map view and to scope the APRS-IS firehose to your area. 150 km is a sensible default. |
| **TNC** | Pick: detected serial device, paired Bluetooth, or TCP host:port | Bluetooth section auto-discovers paired TNCs and offers a Scan button for new ones. TCP path is for Direwolf or any networked KISS server. |
| **Mode** | Pick one of seven operating modes | See [Modes](#operating-modes) below. RX-only is the safest first run. |
| **Flags** *(Advanced only)* | TX enable, RF↔IS gating flags, digi flags, viscous delay, etc. | This step only appears if you picked Advanced mode. The other modes set the flags automatically. |
| **Beacon** | Comment + interval (min 10 minutes) | Skipped automatically in RX-only mode. |
| **Done** | Summary | Click "Go to dashboard" to start. |

You can re-run any individual step later — Settings has **Change location…**, **Change TNC…**, **Switch mode…** buttons that drop you back into the relevant wizard step without redoing the others.

## Operating modes

Pick the role that matches what you want your station to do.

| Mode | Best when… |
|---|---|
| **RX-only iGate** | First run, or you don't have a transmit-licensed setup yet. Listens on RF, forwards to APRS-IS, never transmits. Use passcode `-1`. |
| **TX iGate** | You want to gate RF↔IS *and* relay APRS-IS messages back to stations you've heard recently on the air. No digipeating. |
| **Fill-in digipeater + iGate** *(recommended for most home iGates)* | You want to fill coverage gaps for low-altitude stations near you. Repeats `WIDE1-1` only (the "I need a fill-in" path), with a viscous delay so you defer to higher digis. Also gates to IS. |
| **Full digipeater** | **Mountaintop sites only.** Repeats both `WIDE1-1` and `WIDE2-N`. A low-elevation full digi clogs the channel and gets you talked to by your local APRS coordinator. |
| **Messaging-only iGate** | Your area already has plenty of iGates and you don't want to add channel noise. Bridges person-to-person messages and acks only, skipping beacons / weather / telemetry. Lowest TX duty cycle. |
| **Off-grid digipeater** | EMCOMM field-day, mountaintop with no internet, or any standalone RF relay. Pure RF, no APRS-IS connection at all. |
| **Advanced / Custom** | You know APRS intimately. Skip the presets — manage each gating + digipeating flag yourself. |

Switching modes via Settings → **Switch mode** sets the underlying flags for you. If you want to fine-tune later, pick Advanced and adjust individual flags.

## Console tour

Once setup is done, the navigation has these pages:

- **Dashboard (Live)** — Top-down stream of packets as they arrive, color-coded by origin (RF / IS / your own TX). Filter chips switch between all, RF only, IS only, TX only. Click any callsign to open its station detail.
- **Map** — Leaflet map showing heard stations in your selected time window (30 min – 3 days). Click a marker to focus on one station's trail. Includes your own filter-radius ring when an APRS-IS filter is active.
- **Messages** — Two-pane chat view. Conversations on the left, thread on the right. Auto-acks messages addressed to your station; out-of-band acks for messages you've sent are tracked and the retry queue resends up to five times.
- **Stations** — Table of every callsign your iGate has decoded, with search across callsign / comment / message bodies. Time-window chips for "heard in the last X minutes / hours / days."
- **Diagnostics** — Live ring buffer of packets that were *dropped* and why (rate-limited source, malformed, filtered out, etc.). Helpful when traffic looks lower than expected.
- **Settings** — All configuration in one place: identity, position, mode, gating flags, TNC, APRS-IS server (region dropdown), beacons, retention. Inline `(i)` bubbles explain each field.

## Hardware compatibility

| TNC | Connection | Notes |
|---|---|---|
| Mobilinkd TNC3 | Bluetooth Classic (RFCOMM) | Wizard handles pair + bind + supervisor |
| Mobilinkd TNC4 | Bluetooth Classic or WiFi/TCP KISS | Dual-mode |
| Kenwood TH-D74 / TH-D75A | USB serial or Bluetooth | KISS mode |
| Kenwood TM-D710G / TM-D750A | USB serial via cable | Built-in TNC |
| NinoTNC | USB serial | Popular DIY |
| TNC-Pi / TNC-X (discontinued) | USB serial or Pi GPIO | Huge installed base |
| MFJ-1270X | USB serial | KISS mode |
| ESP32 KISS TNC | USB CDC-ACM at 115200 baud | DIY |
| VR-N76 handheld | USB serial or Bluetooth | Built-in KISS |
| **Soundcard** (Digirig, SignaLink USB, DRAWS, UDRC, DMK URI, RA-35, DINAH, SHARI, etc.) | Run [Direwolf](https://github.com/wb2osz/direwolf) on the same box | aprgo connects to Direwolf's TCP KISS port (default `localhost:8001`) |
| Any networked KISS source ([Direwolf](https://github.com/wb2osz/direwolf), [tnc-server](https://github.com/chrissnell/tnc-server), kissnetd) | TCP KISS | Wizard's "Network (TCP KISS)" option |

**What about audio / PTT?** aprgo does not do audio or PTT — it speaks KISS to *something else* that owns the modem. For hardware TNCs that's the TNC. For soundcard setups, that's Direwolf (which supports CM108-HID, GPIO, serial DTR/RTS, hamlib, VOX — every PTT mechanism you'd want). See the project memory or Direwolf's docs for setup specifics.

## Security model

aprgo serves the console over **HTTPS by default** with a self-signed cert generated on first start. It listens on two ports:

| Port | Scheme | Purpose |
|---|---|---|
| **14473** | HTTP | Redirects everything to HTTPS once setup is complete. `/healthz` and `/readyz` stay plain so monitors don't need TLS. |
| **14439** | HTTPS | The operator console. Self-signed cert — your browser warns once, then remembers. |

The two ports double as a `144.39 MHz` reference (HTTPS) and the `:443`-flavoured HTTP redirect port (`14473`).

**First-run carve-out.** Until the setup wizard completes, the HTTP port stays fully open so a new operator can finish onboarding without first wrestling with a cert warning. The moment the wizard's "Done" step commits, the transport gate engages and non-loopback HTTP starts redirecting.

**Operator credentials + lockdown** live in `/var/lib/aprgo/aprgo.conf` (mode 0600). Settings → Account is the UI that writes that file: username (`[a-z0-9_-]`), bcrypt password, session HMAC key, and a set of hardening checkboxes. Once you enable a lockdown flag, the corresponding handler returns 403 — and the *only* way to undo it is to edit `aprgo.conf` on the box and restart aprgo. That's the deliberate recovery path; there is no web-side override, because that would defeat the threat model.

Hardening checkboxes:

- **Lock settings** — Settings page (and the wizard) become read-only.
- **Disable messaging** — Send / cancel / retry message handlers refuse.
- **Disable bulletins** — Bulletin compose, send, and subscription-edit handlers all refuse.
- **Lock everything** — Master view-only mode (implies all of the above).

Other invariants:

- aprgo runs as `root` (required for `rfcomm` + `bluetoothctl`) with systemd hardening (`ProtectSystem=strict`, `NoNewPrivileges`, `LockPersonality`, `MemoryDenyWriteExecute`, restricted address families and capabilities).
- APRS-IS passcode is stored plaintext in `/var/lib/aprgo/state.json` (mode 0600). Protocol limitation — the IS server needs the cleartext value.
- Default web login is `admin` / `admin`. **Change it immediately on first login** — the dashboard banner reminds you.
- Web UI has CSRF (token + Origin check + `SameSite=Strict`), session HMAC, per-IP login rate limiting, and password-change session invalidation.
- Self-signed TLS protects the LAN segment from passive sniffing. **It does not protect against a determined attacker on the path** — there's no chain of trust. For public-internet exposure, run aprgo behind Caddy / nginx with a real cert, or front it with Tailscale (which issues `*.ts.net` certs for free).

### Regenerating the cert

If you move the box to a new hostname, restart with `--regen-tls`:

```bash
sudo /usr/local/bin/aprgo --regen-tls   # one-shot regen, then restart normally
```

Or just remove `/var/lib/aprgo/tls/` and restart — aprgo regenerates on the next boot. The fingerprint is logged on every start so you can verify it out-of-band over SSH.

### The config file: `/var/lib/aprgo/aprgo.conf`

JSON, mode `0600`, owned root. Created on first start with default `admin/admin` + a fresh random session key. Every field:

```json
{
  "username":      "admin",
  "password_hash": "$2a$10$…",
  "session_key":   "<base64>",
  "lockdown": {
    "lock_settings":     false,
    "disable_messaging": false,
    "disable_bulletins": false,
    "lock_all":          false
  }
}
```

| Field | What it is | How to change it from the CLI |
|---|---|---|
| `username` | The single admin account name. `[a-z0-9_-]{1,32}`. | Edit the string. |
| `password_hash` | bcrypt hash of the password. | `sudo aprgo --set-password 'your-new-password'` writes the hash directly into `aprgo.conf`. Restart aprgo afterwards. |
| `session_key` | 32-byte HMAC key, base64-encoded. Signs session cookies and CSRF tokens. **Don't share this** — anyone who has it can forge sessions. To rotate after a suspected compromise, **blank the field** (`"session_key": ""`) and restart aprgo — a fresh key is minted and persisted automatically. Every existing session is invalidated. |
| `lockdown.*` | The UI hardening flags. Flip any to `true` to enable, back to `false` to undo. Restart aprgo for the change to take effect. |

### Recovering from a lockout

```bash
sudo systemctl stop aprgo
sudo nano /var/lib/aprgo/aprgo.conf        # flip "lockdown" flags to false
sudo systemctl start aprgo
```

If you've also forgotten the password:

```bash
sudo systemctl stop aprgo
# Single quotes so the shell doesn't interpret special characters.
# Prefix the command with a space if HISTCONTROL=ignorespace is set on
# your shell, otherwise the new password lands in shell history.
 sudo aprgo --set-password 'your-new-password'
sudo systemctl start aprgo
```

`--set-password` writes the new bcrypt hash directly into `aprgo.conf` and exits — no manual JSON editing required.

## Files

- `/usr/local/bin/aprgo` — the binary
- `/etc/systemd/system/aprgo.service` — service unit
- `/var/lib/aprgo/state.json` — operating config (callsign, passcode, location, beacon text, gating flags)
- `/var/lib/aprgo/aprgo.conf` — credentials + lockdown flags (mode 0600). Edit this to recover from a UI lockout.
- `/var/lib/aprgo/tls/{cert.pem,key.pem}` — self-signed TLS material (key mode 0600)
- `/var/lib/aprgo/db.sqlite[-wal,-shm]` — SQLite store (heard stations, packets, messages)

## Day-2 operations

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
sudo cp /var/lib/aprgo/aprgo.conf     ~/aprgo-conf-$(date +%F).conf
sudo cp /var/lib/aprgo/db.sqlite      ~/aprgo-db-$(date +%F).sqlite
sudo cp -a /var/lib/aprgo/tls         ~/aprgo-tls-$(date +%F)
sudo systemctl start aprgo
```

### Upgrade

```bash
sudo systemctl stop aprgo
sudo install -m 0755 ./aprgo /usr/local/bin/aprgo
sudo systemctl start aprgo
```

`state.json` is forward-compatible (new fields are added with sensible defaults on first read). DB schema migrations between phases may require deleting `/var/lib/aprgo/db.sqlite*` until a migrations table is added — back it up first if you want to keep packet history.

### Uninstall

```bash
sudo systemctl disable --now aprgo
sudo rm /usr/local/bin/aprgo
sudo rm /etc/systemd/system/aprgo.service
sudo rm -rf /var/lib/aprgo      # WARNING: deletes config + DB
```

## Troubleshooting

**TNC won't connect:**
- USB: check `ls /dev/ttyUSB* /dev/ttyACM*` — does the device exist? Can the `aprgo` process (root) open it?
- Bluetooth: check pairing with `sudo bluetoothctl paired-devices`; verify `bluez` and `bluez-tools` are installed (`apt install bluez bluez-tools`). The settings page surfaces the last error.
- TCP: confirm Direwolf / tnc-server is running on the configured host:port (`ss -ltn | grep 8001`).

**APRS-IS rejected your passcode (red banner on Settings):**
- Your passcode doesn't match your callsign. Generate the right one at [apps.magicbug.co.uk/passcode](https://apps.magicbug.co.uk/passcode/) — use your base callsign with no SSID. Paste it into Settings and save.
- aprgo will still appear "connected" but the server silently drops (qAX's) every packet you send.

**APRS-IS won't connect at all:**
- Network: confirm outbound TCP 14580 is open. `nc -zv noam.aprs2.net 14580`.
- Regional latency: pick a closer server from the Settings → APRS-IS dropdown (Europe → `euro.aprs2.net`, Oceania → `aunz.aprs2.net`, etc.).

**No packets received (dashboard stays empty):**
- The TNC may be connected but failing to decode (squelch closed, wrong band, deviation off). For soundcard setups, tune Direwolf's audio levels first.
- Verify the TNC is actually in KISS mode — some radios boot into command mode and need an explicit KISS-enter command sent by their config tool.

**Wrong passcode confusion:**
- Passcode is callsign-specific (without SSID). `N0CALL-10` and `N0CALL-1` share the same passcode.
- `-1` means "RX-only at the server level" — uploads will be rejected; IS-side will work as a listener.

---

# For developers

## Architecture

```
┌─── RF (KISS over serial / Bluetooth / TCP) ──┐    ┌── APRS-IS ───┐
│                                              │    │              │
│  internal/rf  (transport-agnostic KISS I/O)  │    │ internal/    │
│              │                                │    │ igate        │
│              ▼ ax25.Frame                    │    │              │
│  internal/ax25  (UI frame decode/encode)     │    │              │
│              │                                │    │              │
│              ▼                                │    │              │
│  internal/aprs  (Decode info field →         │    │              │
│                  position / weather /        │    │              │
│                  telemetry / message /       │    │              │
│                  PHG / Mic-E / 3rd-party)    │    │              │
└──────────────┬───────────────────────────────┘    └──────┬───────┘
               │                                            │
               ▼ aprs.Packet                               │
        ┌──────────────────────────────────────┐           │
        │ internal/gate  (pure functions,      │◄──────────┘
        │   Decide(packet, state) → []Action)  │
        │   • RF→IS gate                       │
        │   • IS→RF gate                       │
        │   • WIDE1-1 / WIDE2-N digipeat       │
        │   • Viscous delay                    │
        │   • Preemptive digipeat (MARK)       │
        │   • Source rate-limit                │
        │   ~37 unit tests                     │
        └──────────────┬───────────────────────┘
                       │
        ┌──────────────┼───────────────┬─────────────┐
        ▼              ▼               ▼             ▼
      Drop          rf.TX           igate.Send    store.Insert
      (logged)      (1s spacing)    (queue)       (SQLite)

                       ▲
        ┌──────────────┴───────────────────────────┐
        │ internal/server  (HTTP routes, polling   │
        │   /api/feed every 2.5s, /api/stations,   │
        │   /api/trails — NOT SSE)                 │
        │ web/  (embed.FS templates + static)      │
        └──────────────────────────────────────────┘
```

## Package map

| Package | Responsibility | Pure? | Tests |
|---|---|---|---|
| `internal/ax25` | KISS framing, AX.25 UI frame encode/decode, callsign grammar | ✓ | — |
| `internal/aprs` | Info-field parser (position, Mic-E, weather, PHG/RNG, telemetry, message, third-party, path, tocall device lookup) | ✓ | 15 |
| `internal/gate` | Gating + digipeat decision tree. Pure functions only — caller executes returned actions | ✓ | 21 |
| `internal/bus` | In-memory pub/sub fanout (Frames, Packets) | ✓ | — |
| `internal/state` | Persistent JSON config + live-reload subscribers. Atomic writes with directory fsync | — | partial |
| `internal/store` | SQLite store (stations, packets, messages). Pure-Go `modernc.org/sqlite`. Pragmas tuned for SD-card deploys | — | — |
| `internal/auth` | Cookie session (HMAC) + bcrypt password + per-IP login rate limit | — | — |
| `internal/igate` | APRS-IS client: connect, login, filter, logresp parsing, auto-reconnect | — | — |
| `internal/rf` | KISS reader/writer for serial / Bluetooth / TCP behind one `io.ReadWriteCloser`. Includes `btbind` rfcomm supervisor | — | — |
| `internal/btle` | BLE-KISS GATT client (kept around but not active path; see "Deferred non-features") | — | — |
| `internal/tnc` | BlueZ subprocess wrappers — scan / pair / SDP / rfcomm | — | — |
| `internal/beacon` | Per-beacon periodic scheduler with jitter | — | — |
| `internal/server` | HTTP routes, wizard, SSE-style polling, rate limiters, CSRF, sanitizers — the orchestrator | — | — |
| `cmd/aprgo` | Binary entry + `main` | — | — |
| `cmd/trailcheck` | Aux dev tool | — | — |

The "Pure?" column matters: pure packages have no I/O and are unit-testable in isolation. **All decision logic that affects the on-air behavior of the station lives in `internal/gate/`** and is exhaustively tested. Effectful packages (`rf`, `igate`, `beacon`, `store`, `server`) own the side effects.

## Build / test / cross-compile

```bash
# Build (Go 1.26+)
CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o aprgo ./cmd/aprgo

# Tests (~37 across gate, aprs, state)
go test ./...

# Cross-compile to arm64 (Raspberry Pi)
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -trimpath -o aprgo-arm64 ./cmd/aprgo
```

No CGO anywhere — `modernc.org/sqlite` is pure-Go, `golang.org/x/crypto` is pure-Go. Cross-compile needs no C toolchain. The shipped binary is one static file.

## Key invariants (please preserve)

1. **Single-process design.** No IPC, no helper daemons. The one allowed external is Direwolf, which aprgo connects to over TCP KISS just like any other networked TNC. Don't grow IPC mechanisms — if you find yourself wanting a sidecar process, find another path.
2. **`internal/gate/` is pure.** All on-air decisions are pure functions taking `(packet, state, heardChecker callback) → []Action`. No I/O, no timers, no logging from inside `gate`. Caller executes actions. If you need to change digipeat policy, the change goes in `gate.go` and gets unit tests in `gate_test.go`.
3. **All goroutines have panic recovery.** `internal/server` spawns long-running workers wrapped in `defer recover()`. Add new goroutines using the same pattern — a panic in one component must not take down the process.
4. **`state.json` is forward-compatible.** New fields default to zero on read so older clients can still parse newer files. Atomic write via temp file + rename + directory fsync.
5. **HTTP UI is polling, not SSE.** Dashboard polls `/api/feed?since=N` every 2.5 s. SSE was deliberately reverted (NAT/proxy timeouts). Don't reintroduce SSE without a real reason.
6. **TX inter-frame spacing.** `rf.writeLoop` enforces a 1-second minimum gap between successive RF writes (`internal/rf/rf.go`). APRS channel courtesy. Don't remove.
7. **Heard-stations table excludes our own callsign.** Digipeated copies of our own beacons would otherwise pollute that list. The intake path checks for self before insert.

## Where to make common changes

| Goal | Files to touch |
|---|---|
| Add a new APRS data type to the parser | `internal/aprs/info.go` (or new `xxxx.go` like `weather.go`), add tests in `internal/aprs/parsers_test.go`. Surface in templates / popup / feed. |
| Add a new operating mode | `internal/state/state.go` (Mode enum + `applyModeDefaults`), `internal/server/wizard.go` (step copy), `web/templates/setup.html` (radio card). |
| Add a wizard step | `internal/server/wizard.go` (add to `wizardSteps`, add save case, add to renderStep extras), `web/templates/setup.html` (define the step template + dispatch in main switch). |
| Add a gating / digipeat rule | `internal/gate/gate.go` (function + state flag if user-tunable). Always pair with unit tests in `gate_test.go`. |
| Add an HTTP endpoint | `internal/server/routes.go` (HandleFunc) + handler. Templates in `web/templates/`. |
| Add a persistent setting | `internal/state/state.go` (struct field), Settings UI in `web/templates/settings.html`, save case in `internal/server/settings.go`. |
| Add a TNC transport | `internal/state/state.go` (TNCKind enum), `internal/rf/rf.go` (open/dial logic), `web/templates/setup.html` (wizard fieldset). |
| Add a beacon-style packet | `internal/beacon/beacon.go` (build function) + state schema + Settings UI. |

## Testing

Strongest coverage where it matters most:

- `internal/gate/gate_test.go` — 21 tests covering WIDE-N parsing, N-capping, decrement, MARK mode (preemptive), path length, viscous flag, skip-self.
- `internal/aprs/parsers_test.go` — 15 tests covering weather, PHG, RNG, tocall lookup (exact + wildcard + SSID strip), path parsing (used hops + q-construct).
- `internal/state/` — config validation tests.

HTTP routes, RF goroutines, and the IS client are exercised by integration testing on the Wyse target rather than unit tests. New code that touches `gate/`, `aprs/`, or `ax25/` should always come with tests — they're the ones operators can't see go wrong on their end.

## Deployment loop

Standard dev workflow:

```bash
# 1. Build
CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o /tmp/aprgo-linux ./cmd/aprgo

# 2. scp to target
scp /tmp/aprgo-linux user@host:/tmp/

# 3. Hot-swap
ssh user@host '
  sudo systemctl stop aprgo &&
  sudo install -m 0755 /tmp/aprgo-linux /usr/local/bin/aprgo &&
  sudo systemctl start aprgo &&
  sudo systemctl is-active aprgo
'

# 4. Watch logs
ssh user@host 'journalctl -u aprgo -f'
```

`/var/lib/aprgo/` survives the swap. State.json's forward-compat means schema additions don't break the running config.

## Project layout

```
cmd/aprgo/         binary entry + main
cmd/trailcheck/    aux dev tool
internal/
  ax25/            KISS framing + AX.25 UI frame encode/decode
  aprs/            info-field parser (position, Mic-E, weather, PHG, telemetry,
                     message, third-party, tocall, path)
                   data/  embedded aprsorg/aprs-deviceid tocall registry
  bus/             typed pub/sub fanout
  state/           persistent config + live-reload subscribers
  store/           SQLite stations/packets/messages
  auth/            cookie session (HMAC + password-generation binding)
  igate/           APRS-IS client (reconnect, filter, logresp parsing)
  rf/              KISS reader/writer for serial / Bluetooth / TCP, plus btbind supervisor
  btle/            BLE-KISS GATT client (retired path)
  tnc/             BlueZ subprocess wrappers (scan / pair / SDP / rfcomm)
  gate/            RF↔IS gating + digipeat decision engine (pure functions)
  beacon/          periodic beacon scheduler
  server/          HTTP routes, polling feed, wizard, rate limiters, CSRF, sanitizers
deploy/            systemd unit, install.sh, debian/control skeleton
web/               embed.FS for templates + static assets
```

## Roadmap

See [TODO.md](TODO.md) for the current open work. Top items right now:
- Global TX rate cap (per-source rate limit exists; need a global backstop)
- SSn-N regional digipeat aliases (`ARIZ1-1`, `MASS2-2`, etc.)
- APRS-IS auth-test wizard step (verify passcode before letting the user proceed)
- TNC test wizard step (tail the TNC for 5 s and report frame count)
- `.deb` / `.rpm` / Arch packaging
- aprx → aprgo migration subcommand
- GitHub release workflow

## Deliberately out of scope

These came up in audits but are explicitly punted on — please don't open PRs for them without discussing first:

- **AFSK soundcard modem in pure Go** — Direwolf is 30k lines of refined DSP we'd take years to match. aprgo uses Direwolf as a TCP-KISS source.
- **AGW PE protocol** — APRS doesn't need connected-mode AX.25. Out of scope unless aprgo grows into Winlink territory.
- **Smart-beaconing for mobile stations** — aprgo targets fixed iGates + digipeaters; mobile stations have different needs.
- **TLS APRS-IS (`:24580`)** — operator can put aprgo behind a reverse proxy if exposing the web UI beyond LAN; APRS-IS plaintext over LAN is the common case.
- **Bundled Direwolf in `.deb`** — declared as `Recommends`, not `Depends`. Users who need it install it; aprgo doesn't manage child processes.
- **Automatic offline-mode flip when IS goes down** — paternalistic. The operator chose a mode; aprgo shouldn't silently switch it.
- **BLE-KISS GATT support** — code is still here for archival but inactive. BlueZ D-Bus quirks made it too fragile on desktop Linux for no benefit over Classic SPP.

## License

MIT — see [LICENSE](LICENSE).

The APRS info-field parser is derived from aprx's `parse_aprs.c` (Matti Aarnio OH2MQK, MIT-licensed).

The embedded tocall device-identification database (`internal/aprs/data/tocalls.yaml`) is from the [aprsorg/aprs-deviceid](https://github.com/aprsorg/aprs-deviceid) project, licensed under [CC BY-SA 2.0](https://creativecommons.org/licenses/by-sa/2.0/).
