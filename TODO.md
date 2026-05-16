# aprgo — TODO / Roadmap

Snapshot of unfinished work as of phase1.98.

Items are grouped by impact, with the highest-ROI work near the top of each
group. Anything marked **(deferred)** has a known reason it isn't being built
right now — note the reason next to the item.

---

## On-air behavior

- [ ] **Global TX rate cap.** We have per-source rate limits but no global
  "no more than N TXs/min total" backstop. Add a global token bucket.
- [ ] **SSn-N regional aliases.** State-WIDE equivalents (`ARIZ1-1`,
  `MASS2-2`, `NCN1-1`, …). Make the alias list configurable in state and
  honor it in `digipeatAction`. Opt-in by region.

---

## Wizard / settings UX

- [ ] **APRS-IS auth-test step.** Open a short-lived IS connection with the
  entered passcode and report verified/unverified before letting the user
  proceed. Today they only learn it failed via the red banner after first save.
- [ ] **TNC test step.** After binding/connecting, tail the TNC for 5 s and
  report frame count so the user sees activity (or the lack of it).
- [ ] **Mode escalation confirmation.** When the wizard's mode step jumps from
  `rx-only` → `digi` (or any TX-enabling mode), surface a confirmation page
  reminding the operator about TX prerequisites + license-class authorization.
- [ ] **TX-enable toggle confirmation.** In the settings page, toggling
  `TXEnable` off/on should warn that pending beacons may be dropped.
- [ ] **Regional ISServer defaults.** Today the wizard always seeds
  `rotate.aprs2.net:14580`. Smart default: derive from lat/lon → `euro.aprs2.net`
  for EU, `aunz.aprs2.net` for AU/NZ, `asia.aprs2.net` for AS. Or add a
  region dropdown to the location step.
- [ ] **Auto-degrade UI when IS has been down a long time.** Cosmetic: after
  ~30 min of `ISConnected=false`, soften the red "DISCONNECTED" banner to a
  neutral "Operating RF-only — internet down ~Xh ago" chip. Runtime keeps
  retrying at the existing cadence. No automatic mode flip.
- [ ] **Multi-beacon role templates.** "Add beacon" gives a blank position
  beacon. Pre-templates for "status beacon" (`>` data type), "weather beacon",
  "telemetry data" would speed common multi-beacon setups.
- [ ] **Beacon comment composition helpers.** APRS comments support
  altitude (`/A=NNNNNN`), wind (`_NNN/NNN`), frequency, etc. Today the
  comment is a single freeform string. Optional structured editors would
  generate the encoded form.

---

## Observability / ops

- [ ] **`/metrics` Prometheus endpoint.** Counters for packets-gated,
  digipeated, dropped-by-rate-limit, queue depths, RF/IS connection
  uptime, DB row counts. Trivial to add via `prometheus/client_golang`.
- [ ] **TNC-not-in-KISS detection.** If bytes flow but the splitter never
  produces a valid AX.25 frame for 30 s, log a warning and surface a
  diagnostic to the UI: "TNC may not be in KISS mode — check device config."
- [ ] **Crash-loop counter visible.** Write a small "boot record" file on
  startup with last-startup-time + crash count, render in the dashboard if
  the process has restarted more than N times in the last hour.
- [ ] **Beacon TX log line.** `beacon.transmit` records a UI timestamp on
  success (shipped) but emits no journal log line. Add `log.Printf` for
  grep-able per-fire observability.
- [ ] **APRS-IS keepalive on RX-only mode.** Server `#` heartbeats every 20 s
  reset our 120 s read deadline, so the connection stays up. Worth a code
  comment confirming this; no work needed unless an operator reports drops.

---

## Build / packaging / distribution

- [ ] **Debian `.deb` packaging.** `deploy/debian/control` is a skeleton.
  Add `rules`, `changelog`, `compat`, `postinst` (creates user/dir),
  `prerm` (stops service). Target: `dpkg-buildpackage -uc -us` produces
  an installable `.deb`.
- [ ] **RHEL / Fedora `.rpm`.** Mirror the deb work for `rpmbuild`.
- [ ] **Arch `PKGBUILD`.** AUR-installable.
- [ ] **Cross-platform install.sh.** Detect distro family, install
  appropriate bluez packages, fall back to `useradd` on RHEL/Arch.
- [ ] **Migration subcommand.** `aprgo migrate --from-aprx /etc/aprx.conf`
  reads the existing aprx config, derives callsign/passcode/position/filter/
  paths, writes a starter state.json. Removes the manual cutover dance.
- [ ] **GitHub release workflow.** GH Actions building amd64 + arm64
  binaries, attaching `.deb`s to releases.

---

## Documentation

- [ ] **`docs/APRS-PRIMER.md`.** Audience: hams new to APRS deploying their
  first iGate. Cover: passcode purpose + how to get one; APRS-IS overview +
  q-constructs; digipeater concept (WIDE1-1 vs WIDE2-N); fill-in vs
  high-elevation digi; FCC ID requirement (10 min during TX); regional APRS
  frequencies (144.39 NA / 144.800 EU / 145.175 AU). Link from README and
  from the wizard intro page.
- [ ] **CONTRIBUTING.md.** Build commands, test layout, code style, branch
  naming, where to file issues.
- [ ] **CHANGELOG.md.** Once releases start.
- [ ] **Troubleshooting matrix expansion.** README has the basics. Edge cases
  to add: "bluez 5.50 vs 5.66 differences", "rfcomm bind fails after kernel
  upgrade (need `modprobe rfcomm`)", "Direwolf decode rate too low",
  "APRS-IS `# logresp` says unverified", "passcode for callsign vs SSID
  confusion."
- [ ] **`SECURITY.md`.** Threat model + how to report vulns.
- [ ] **APRGO tocall registration.** File issue at
  [aprs-deviceid](https://github.com/aprsorg/aprs-deviceid/issues) requesting
  allocation of `APRGO` (or `APAGRO` if `APRGO` denied) with project
  metadata. Paperwork, not code.

---

## Niche features (low priority)

- [ ] **Position ambiguity option.** APRS allows replacing trailing digits of
  lat/lon with spaces for privacy. Add a per-beacon "ambiguity level" (0-4).
- [ ] **Compressed position emission.** 13 bytes vs 19. Modern decoders
  prefer compressed. Encoder per APRS spec §9.
- [ ] **`bluetoothctl --timeout` compat fallback.** On bluez < 5.55 the flag
  is silently ignored, scan runs full duration. Detect via `--version` and
  fall back to interactive `scan on/off` via stdin.
- [ ] **`sdptool` multi-record parsing.** Today we pick the first `Channel:`
  line from `sdptool search`. Multi-profile devices (DUN + SPP + headset)
  could have the wrong channel land first. Parse by service-record block.
- [ ] **`gate.Drop` instrumentation as Prometheus labels.** Per-reason
  counters once metrics endpoint exists.
- [ ] **Mic-E speed extreme-value wraparound.** Spec subtracts 800 once when
  encoded value ≥ 800. Malformed input could exceed; loop subtraction would
  be more defensive.
- [ ] **Telemetry PARM/UNIT/EQNS/BITS pairing.** Decoder recognizes telemetry
  data packets and decodes the 5 analog + 8 binary channels. Doesn't pair
  them with the matching PARM (param names) / UNIT (units) / EQNS
  (calibration equations) / BITS (bit-channel names) metadata messages.
  Per-station telemetry view would show "Temperature: 72°F" not "A1: 72".
- [ ] **Path validation richer feedback.** "WIDE2-9 is illegal, did you mean
  WIDE2-1?" instead of generic invalid-token error.
- [ ] **Per-beacon position override.** Beacons currently share
  `state.Lat/Lon`. Edge case: user wants different beacons from different
  positions (rare, e.g. a relay broadcasting a fixed remote location).

---

## Deferred (intentional non-features)

These came up in audits but were explicitly punted on:

- **AFSK soundcard modem in pure Go** — Direwolf is 30k lines of refined
  DSP we'd take years to match. Use Direwolf as TCP-KISS source instead.
- **AGW PE protocol** — APRS doesn't need connected-mode AX.25. Out of
  scope unless aprgo grows into Winlink territory.
- **Smart-beaconing for mobile stations** — aprgo targets fixed iGates +
  digipeaters. Mobile stations have different needs.
- **TLS APRS-IS (`:24580`)** — operator can put aprgo behind a reverse proxy
  if exposing the web UI beyond LAN; APRS-IS plaintext over LAN to a local
  daemon is the common case.
- **Bundled Direwolf in the `.deb`** — declared as `Recommends`, not
  `Depends`. Users who need it install it; we don't manage a child process.
- **Automatic offline-mode flip when IS goes down** — was discussed; deemed
  paternalistic (operator chose a mode, we shouldn't silently switch it).
  Soft UI degradation in the wizard/settings UX section above handles the
  real complaint without touching runtime behavior.
- **BLE-KISS GATT support** — retired in phase1.39. BlueZ D-Bus quirks made
  it too fragile on desktop Linux for no benefit over Classic SPP.

---

## Recently shipped (phase1.x highlights)

What's done so we don't redo:

### Modes + on-air behavior
- 7 operating modes: rx-only, tx-igate, fillin-igate, full-digi,
  messaging-only (selective gating: messages + acks only), offline (no
  APRS-IS at all — off-grid digi), advanced (operator manages all flags)
- Full WIDE2-N digipeating per New-N Paradigm — `WIDEn-N` → `MYCALL*,WIDEn-(N-1)`
  with N>2 trap and 8-hop path cap
- Viscous delay for fill-in WIDE1-1 (randomized 3–5 s hold + cancel-on-RF-echo)
- Per-source token-bucket rate limiter
- Dupe table on content hash + 15-min message-body dedupe fallback
- Auto-ACK for messages addressed to us (RF→RF, IS→IS)
- Outbound-message retry queue (5 attempts × 30 s, cancel on ack)
- Third-party packet attribution fix (relayed positions no longer credited
  to relay station)

### Modes + role configuration
- Wizard-driven setup with async BT pairing + map-pick location
- Settings page hides gating/digi flags when not in Advanced mode
- Per-beacon callsign override (run different beacons under different SSIDs)
- Per-beacon visual symbol picker with rendered APRS icons + custom-2-char
  fallback

### Storage + observability
- Pure-Go SQLite (`modernc.org/sqlite`) for arm64 cross-compile
- Tuned pragmas for SD-card-backed deploys (cache_size, mmap_size, temp_store)
- `packets.lat`/`packets.lon` columns populated at intake for fast trail queries
- Gate-drop reason ring buffer + `/diagnostics` page (live-polled)
- Per-beacon "Last fired: X ago" indicator on Settings

### Web UI
- Operator-console aesthetic: IBM Plex Sans/Mono, phosphor-on-ink,
  hand-rolled CSS variables for light/dark/auto themes
- Live dashboard via 2.5 s polling (replaces SSE; survives NAT/proxy timeouts)
- Two-pane chat-style messages page with conversation list + thread, polling
- Map: 15 s auto-refresh, station markers, movement trails (red lines for
  mobiles), "view only this station" focus chip, APRS-IS filter-radius ring
- Station detail / messages / stations / settings / setup / diagnostics
  templates all consistent with shared partials
- Per-beacon Settings sections + multi-beacon support
- HTMX polling for chat panes + diagnostics with byte-identical no-swap
  optimization

### Hardening
- Hardened systemd unit (RestrictAddressFamilies, LockPersonality,
  MemoryDenyWriteExecute, ProtectSystem=strict, etc.)
- CSRF token + Origin check + SameSite=Strict cookie
- Per-IP login rate limiting (5 fails → 10-min lockout)
- Session HMAC bound to password generation
- Open-redirect prevention, method guards on mutating endpoints
- Field validation: callsign grammar (now strict SSID 0–15), passcode,
  beacon path, beacon symbol
- `panic()` recovery in every spawned goroutine
- `/readyz` endpoint
- LastError/LastErrorAt surfaced for RF + IS disconnects
- Atomic state.json writes with directory fsync after rename

### TNC support
- Multi-TNC: USB/serial, Bluetooth Classic via in-process rfcomm-supervisor,
  TCP KISS to Direwolf/tnc-server/WiFi TNCs
- Bluetooth pairing via dedicated `bt-agent` process (works in non-TTY
  contexts where bluetoothctl agent registration fails)
- SDP channel discovery with retry-backoff (handles BlueZ cache lag
  immediately after pair)
- rfcomm slot reuse for same-MAC re-pairs (no rfcomm0/rfcomm1/rfcomm2 sprawl)

### Misc
- LICENSE (MIT), README with hardware compat, security model,
  troubleshooting, install/upgrade/backup procedures
- `internal/gate/gate_test.go` — 16 unit tests covering digipeat decrement,
  viscous flag, WIDE-N edge cases
