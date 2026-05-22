# aprgo — Self-Hosted APRS Suite

A self-hosted APRS suite in a single Go binary. iGate, digipeater, operator console, map, messaging, bulletins, and admin web UI. One process, one config directory, no sidecars.

The binary owns the TNC (serial, Bluetooth, or TCP-KISS), talks to APRS-IS, runs gating and digipeat logic, fires your beacons, stores history in SQLite, and serves the whole console over HTTPS. Runs unattended on a Raspberry Pi, a Wyse thin client, or any other Linux box.

**Status:** Alpha. Running stably on Debian 12 with a Mobilinkd TNC3.

> **New to APRS?** APRS is the ham radio digital data network: position reports, short messages, weather telemetry, emergency comms over VHF. An *iGate* is a station that bridges radio traffic to the internet (APRS-IS) and back. A *digipeater* repeats packets so they reach further than a single hop. aprgo can be either, both, or neither — it also runs as a pure APRS-IS client with no radio at all.

---

## Install

One-line installer:

```bash
curl -fsSL https://raw.githubusercontent.com/cartpauj/aprgo/main/get.sh | sudo sh
```

It detects your distro family and CPU arch, downloads the matching `.deb` or `.rpm` from the latest GitHub release, installs it (pulling in `bluez`, `bluez-tools`, and `direwolf` as recommended dependencies), and prints how to reach the console.

### Supported platforms

| Filename suffix | `.deb` | `.rpm` | Typical hardware |
|---|---|---|---|
| `amd64` / `x86_64` | ✓ | ✓ | x86 servers, PCs, Wyse 3040 / 5070, Intel NUC, cloud VPS |
| `arm64` / `aarch64` | ✓ | ✓ | Pi 3 (64-bit OS), Pi 4, Pi 5, Pi Zero 2 W, AWS Graviton, ARM SBCs |
| `armhf` (ARMv7) | ✓ | ✓ as `armv7hl` | Pi 2, Pi 3 / 4 on 32-bit RPi OS, BeagleBone Black |
| `armhf-armv6` | ✓ | — | Pi 1, Pi Zero, Pi Zero W (Raspberry Pi OS only) |
| `i386` / `i686` | ✓ | ✓ | Old 32-bit x86 thin clients (Wyse 3010-class Atom), netbooks |

Both ARMv7 and ARMv6 .debs carry `Architecture: armhf` in their metadata, since Debian has no separate ARMv6 arch. Pick the right one by filename: Pi Zero / Pi 1 users want `armhf-armv6`, everyone else on 32-bit RPi OS wants plain `armhf`.

aprgo targets Linux with systemd. macOS, Windows, and the BSDs aren't supported: Bluetooth pairing uses BlueZ, and the installer wires up a systemd unit. For anything not in the table above, [build from source](#building-from-source).

#### Verified configurations

End-to-end tested (pair, gate, beacon, console) on:

| Hardware | OS | TNC |
|---|---|---|
| Raspberry Pi Zero W (ARMv6) | Raspberry Pi OS 32-bit (no desktop) | Mobilinkd TNC3 over Bluetooth |
| x86_64 with Bluetooth | Debian 13 Trixie (no desktop) | Mobilinkd TNC3 over Bluetooth |

If you bring up aprgo on hardware/OS not listed and it works (or doesn't), please open an issue — happy to expand this list.

### Reaching the console

aprgo auto-starts on install. The web console is **HTTPS by default** — start at `https://<host>:14439/` and click through the self-signed cert warning once. Plain HTTP on 14473 is a restricted fallback for hosts where TLS isn't usable (e.g. a browser that refuses self-signed certs, or a captive network that strips TLS).

| Port | URL | Use |
|---|---|---|
| **14439** (HTTPS) | `https://<host>:14439/` | **Recommended.** Full access. Self-signed cert; accept the browser warning once. |
| **14473** (HTTP)  | `http://<host>:14473/`  | Restricted fallback. Read-only views (Dashboard, Map, Stations, Stats, Logs) and the first-run setup wizard. Settings, Messages, and Bulletins redirect to HTTPS. |

Default login is `admin` / `admin`. Change it on first sign-in.

### Connecting a radio

aprgo speaks KISS over whatever transport your TNC offers. What you do depends on the hardware:

- **Bluetooth KISS TNCs** (Mobilinkd TNC3 / TNC4): turn the TNC on, open the first-run wizard, click *Scan*, pair. Done.
- **USB / serial KISS TNCs** (NinoTNC, Kenwood TH-D74 / TM-D710G, MFJ-1270X, VR-N76, ESP32 KISS TNCs): plug in. The wizard lists them as `/dev/ttyUSB0` or `/dev/ttyACM0`.
- **Soundcard + radio** (Digirig, SignaLink USB, DRAWS, UDRC, DMK URI, SHARI, DINAH, RA-35, RTL-SDR): aprgo does not do AFSK / FSK modulation. Install [Direwolf](https://github.com/wb2osz/direwolf) (or another KISS soundmodem) and point aprgo at its TCP-KISS port (`localhost:8001` by default).
- **Older or non-KISS TNCs**, networked TNC hubs, or anything talking AGW/PE/TNC-2 command mode: bridge them through [`tnc-server`](https://github.com/chrissnell/tnc-server), `kissnetd`, or similar.

If your TNC is Bluetooth or USB KISS, just run aprgo. Otherwise set up Direwolf or tnc-server first, then point aprgo at it via the wizard's TCP option.

User documentation (operating modes, hardware deep-dive, security hardening, troubleshooting, day-2 operations) lives in the project wiki.

---

# For developers

The rest of this README is for contributors.

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
| `internal/gate` | Gating + digipeat decision tree. Pure functions; the caller executes returned actions | ✓ | 21 |
| `internal/bus` | In-memory pub/sub fanout (Frames, Packets) | ✓ | — |
| `internal/state` | Persistent JSON config plus live-reload subscribers. Atomic writes with directory fsync | — | partial |
| `internal/config` | Credentials and lockdown flags (`aprgo.conf`). Bcrypt password, HMAC session key, ratcheted UI lockdown switches | — | — |
| `internal/tlscert` | Load-or-generate self-signed ECDSA P-256 cert under `/var/lib/aprgo/tls/` | — | — |
| `internal/store` | SQLite store (stations, packets, messages). Pure-Go `modernc.org/sqlite`. Pragmas tuned for SD-card deploys | — | — |
| `internal/auth` | Cookie session (HMAC) plus bcrypt password and per-IP login rate limit | — | — |
| `internal/igate` | APRS-IS client: connect, login, filter, logresp parsing, auto-reconnect | — | — |
| `internal/rf` | KISS reader/writer for serial / Bluetooth / TCP behind one `io.ReadWriteCloser`. Includes `btbind` rfcomm supervisor | — | — |
| `internal/tnc` | BlueZ subprocess wrappers: scan, pair, SDP, rfcomm | — | — |
| `internal/beacon` | Per-beacon periodic scheduler with jitter | — | — |
| `internal/server` | HTTP routes, wizard, polling feed, rate limiters, CSRF, transport gate (HTTP→HTTPS), lockdown enforcement | — | — |
| `cmd/aprgo` | Binary entry plus `main` (handles `--set-password`, `--regen-tls`, `--version`) | — | — |
| `cmd/trailcheck` | Auxiliary dev tool | — | — |

The "Pure?" column is load-bearing. Pure packages have no I/O and are unit-testable in isolation. **All decision logic affecting the station's on-air behavior lives in `internal/gate/`** and is exhaustively tested. Effectful packages (`rf`, `igate`, `beacon`, `store`, `server`) own the side effects.

## Building from source

```bash
# Go 1.26+ required. Pure-Go build, no CGO, no C toolchain.
git clone https://github.com/cartpauj/aprgo
cd aprgo
CGO_ENABLED=0 go build \
  -ldflags="-s -w -X main.Version=$(git describe --tags --always)" \
  -trimpath -o aprgo ./cmd/aprgo

# Tests (gate, aprs, state, server passcode helper).
go test ./...

# Cross-compile to arm64 (Pi 3 / 4 / 5 / Zero 2 W):
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -trimpath -o aprgo-arm64 ./cmd/aprgo
```

No CGO anywhere. `modernc.org/sqlite` is pure Go, `golang.org/x/crypto` is pure Go. Cross-compilation needs no C toolchain. The shipped binary is one static file.

### Installing a locally-built binary

```bash
sudo ./deploy/install.sh ./aprgo
sudo systemctl start aprgo
```

The install script creates `/var/lib/aprgo/` (mode 0700), copies the binary to `/usr/bin/aprgo`, installs the systemd unit, and enables it.

## Key invariants (please preserve)

1. **Single-process design.** No IPC, no helper daemons. The one external is Direwolf, which aprgo connects to over TCP KISS like any other networked TNC. Don't add IPC mechanisms; if you want a sidecar process, find another path.
2. **`internal/gate/` is pure.** All on-air decisions are pure functions taking `(packet, state, heardChecker callback)` and returning `[]Action`. No I/O, no timers, no logging from inside `gate`. The caller executes the actions. Digipeat policy changes go in `gate.go` with unit tests in `gate_test.go`.
3. **All goroutines have panic recovery.** `internal/server` spawns long-running workers wrapped in `defer recover()`. Add new goroutines using the same pattern. A panic in one component must not take down the process.
4. **`state.json` is forward-compatible.** New fields default to zero on read, so older clients still parse newer files. Atomic write via temp file, rename, and directory fsync. Same pattern for `aprgo.conf` and the TLS material.
5. **HTTP UI is polling, not SSE.** The dashboard polls `/api/feed?since=N` every 2.5 s. SSE was tried and reverted (NAT/proxy timeouts). Don't reintroduce SSE without a real reason.
6. **TX inter-frame spacing.** `rf.writeLoop` enforces a 1-second minimum gap between successive RF writes (`internal/rf/rf.go`). APRS channel courtesy. Leave it alone.
7. **Heard-stations table excludes our own callsign.** Digipeated copies of our own beacons would otherwise pollute the list; the intake path checks for self before insert.
8. **Lockdown ratchet.** UI lockdown flags in `aprgo.conf` can only go OFF→ON via the web UI. The handler `OR`s any incoming form value against the existing raw value, so a hand-crafted POST can never clear a locked flag. The only way back to off is editing `aprgo.conf` and restarting.

## Where to make common changes

| Goal | Files to touch |
|---|---|
| New APRS data type in the parser | `internal/aprs/info.go` (or a new file like `weather.go`). Tests in `internal/aprs/parsers_test.go`. Surface in templates, popup, feed. |
| New operating mode | `internal/state/state.go` (Mode enum and `applyModeDefaults`), `internal/server/wizard.go` (step copy), `web/templates/setup.html` (radio card). |
| New wizard step | `internal/server/wizard.go` (add to `wizardSteps`, save case, renderStep extras), `web/templates/setup.html` (step template plus dispatch in main switch). |
| New gating / digipeat rule | `internal/gate/gate.go` (function plus state flag if user-tunable). Pair with unit tests in `gate_test.go`. |
| New HTTP endpoint | `internal/server/routes.go` (HandleFunc plus handler). Templates in `web/templates/`. Add to the transport gate's `isCriticalPath()` allowlist if it mutates state. |
| New persistent setting | `internal/state/state.go` (struct field), Settings UI in `web/templates/settings.html`, save case in `internal/server/routes.go handleSettingsSave`. |
| New TNC transport | `internal/state/state.go` (TNCKind enum), `internal/rf/rf.go` (open/dial logic), `web/templates/setup.html` (wizard fieldset). |
| New beacon-style packet | `internal/beacon/beacon.go` (build function), state schema, Settings UI. |
| New lockdown flag | `internal/config/config.go` (Lockdown struct plus `Effective()`), 403 checks in handlers via `s.requireUnlocked`, UI surfaces in `web/templates/settings.html`. |

## Testing

Coverage is heaviest where it matters most:

- `internal/gate/gate_test.go` — 21 tests covering WIDE-N parsing, N-capping, decrement, MARK mode (preemptive), path length, viscous flag, skip-self.
- `internal/aprs/parsers_test.go` — 15 tests covering weather, PHG, RNG, tocall lookup (exact, wildcard, SSID strip), path parsing (used hops, q-construct).
- `internal/state/` — config validation tests.

HTTP routes, RF goroutines, and the IS client are exercised by integration testing on a real Pi or Wyse target rather than unit tests. New code touching `gate/`, `aprs/`, or `ax25/` should always come with tests. Those are the places operators can't see things go wrong.

## Deployment loop (dev to real target)

Inner loop for testing on a real Pi or thin client:

```bash
# 1. Build for the target arch (arm64 example).
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -trimpath -o /tmp/aprgo-linux ./cmd/aprgo

# 2. scp to target.
scp /tmp/aprgo-linux user@host:/tmp/

# 3. Hot-swap.
ssh user@host '
  sudo systemctl stop aprgo &&
  sudo install -m 0755 /tmp/aprgo-linux /usr/bin/aprgo &&
  sudo systemctl start aprgo &&
  sudo systemctl is-active aprgo
'

# 4. Watch logs.
ssh user@host 'journalctl -u aprgo -f'
```

`/var/lib/aprgo/` survives the swap. `state.json` and `aprgo.conf` are forward-compatible, so new fields default to zero on read.

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
  state/           persistent operating config (state.json)
  config/          credentials + lockdown flags (aprgo.conf)
  tlscert/         self-signed cert load-or-generate
  store/           SQLite stations/packets/messages
  auth/            cookie session (HMAC + password-generation binding)
  igate/           APRS-IS client (reconnect, filter, logresp parsing)
  rf/              KISS reader/writer for serial / Bluetooth / TCP, plus btbind supervisor
  tnc/             BlueZ subprocess wrappers (scan / pair / SDP / rfcomm)
  gate/            RF↔IS gating + digipeat decision engine (pure functions)
  beacon/          periodic beacon scheduler
  server/          HTTP routes, polling feed, wizard, rate limiters, CSRF, transport gate, lockdown enforcement
deploy/            systemd unit, install.sh, nfpm.yaml, postinst/prerm/postrm scripts
.github/workflows/ release.yml — builds .deb/.rpm matrix on v* tags
web/               embed.FS for templates + static assets
get.sh             one-line installer used in the README
```

## License

MIT. See [LICENSE](LICENSE).

## Attributions

aprgo is built on a lot of other people's work.

### Code ported or derived

| Source | License | Where it lives |
|---|---|---|
| **aprx** `parse_aprs.c` by Matti Aarnio (OH2MQK) | MIT | `internal/aprs/info.go`. Mic-E plus position and uncompressed-position decoders, comment cleanup, character validation. |

### Bundled assets (`web/static/`)

| Project | Version | License | Files |
|---|---|---|---|
| [Leaflet](https://leafletjs.com/) by Vladimir Agafonkin / CloudMade | 1.9.4 | BSD-2-Clause | `leaflet.js`, `leaflet.css` |
| [htmx](https://htmx.org/) by Big Sky Software | 1.9.10 | BSD-2-Clause / 0BSD | `htmx.min.js` |
| [hessu/aprs-symbols](https://github.com/hessu/aprs-symbols) by Heikki Siltala (OH7LZB) | — | MIT | `aprs-symbols-48-0.png`, `aprs-symbols-48-1.png`, `aprs-symbols-48-2.png` |
| [IBM Plex Sans + IBM Plex Mono](https://github.com/IBM/plex) by IBM | — | SIL Open Font License 1.1 | `fonts/plex-*.woff2` |

### Embedded data (`internal/aprs/data/`)

| Project | License | Files |
|---|---|---|
| [aprsorg/aprs-deviceid](https://github.com/aprsorg/aprs-deviceid). APRS tocall device identification registry. | [CC BY-SA 2.0](https://creativecommons.org/licenses/by-sa/2.0/) | `tocalls.yaml`, `tocalls.json` |

### Go module dependencies

Pure Go, no CGO. Pulled in by `go.mod`:

| Module | License | What it does |
|---|---|---|
| `golang.org/x/crypto` | BSD-3-Clause | bcrypt for the admin password hash |
| `golang.org/x/sys` | BSD-3-Clause | Low-level syscalls (serial port termios via `unix.Termios`) |
| `modernc.org/sqlite` | BSD-3-Clause (SQLite itself is public domain) | The SQLite store. Pure-Go translation of the C source, so CGO isn't needed |
| `modernc.org/libc`, `modernc.org/memory`, `modernc.org/mathutil` | BSD-3-Clause | Transitive support packages for `modernc.org/sqlite` |
| `github.com/dustin/go-humanize` | MIT | Human-readable byte sizes and time deltas on the stats page |
| `github.com/google/uuid` | BSD-3-Clause | Random UUIDs (transitive) |
| `github.com/mattn/go-isatty` | MIT | TTY detection (transitive) |
| `github.com/ncruces/go-strftime` | MIT | strftime-style date formatting (transitive) |
| `github.com/remyoudompheng/bigfft` | BSD-3-Clause | Large-integer FFT (transitive, via SQLite) |

Version pins and checksums are in [`go.mod`](go.mod) and [`go.sum`](go.sum).

### Specification sources

aprgo's protocol behavior tracks publicly available APRS documentation. None of these specs are bundled in the repo:

- APRS Protocol Reference v1.0.1 (Bob Bruninga, WB4APR)
- APRS aprs11 / aprs12 addenda at [aprs.org](http://www.aprs.org): fix14439, preemptive-digipeating, RFlimits, SSIDs, mic-e-types, spec-wx, datum, replyacks
- APRS-IS specifications at [aprs-is.net](https://www.aprs-is.net): IGating, IGateDetails, q-construct rules, Connecting
- AX.25 Link Access Protocol v2.2 (TAPR)
- base-91 telemetry as documented at [he.fi](http://he.fi)

Thanks to everyone who's reported bugs from the air. APRS still works because operators share.
