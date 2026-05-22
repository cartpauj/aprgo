# TODO

## ✅ Security / Lockdown via Config File — DONE (2026-05-22)

Shipped:
- New `internal/config` package owning `/var/lib/aprgo/aprgo.conf` (username,
  bcrypt password hash, session HMAC key, lockdown flags). Atomic write,
  mode 0600.
- Configurable admin username (`[a-z0-9_-]{1,32}`); the old hardcoded
  "admin" constant is gone.
- New `internal/tlscert` package — self-signed ECDSA P-256 cert
  generated on first run at `/var/lib/aprgo/tls/`, fingerprint logged.
- Dual listeners: HTTP on `:14473` (read pages + redirect for critical
  paths), HTTPS on `:14439`. Loopback bypasses both gates; first-run
  bypasses until `SetupComplete=true`.
- Hardening checkboxes in Settings → Account: lock_settings,
  disable_messaging, disable_bulletins, lock_all. One-way ratchet —
  once on, the checkbox disappears from UI and the only undo is
  editing `aprgo.conf` and restarting. UI surfaces (compose forms,
  cancel/retry buttons) hide when the matching flag is on; server
  handlers return 403 too.
- `aprgo --set-password 'newpass'` writes a bcrypt hash directly into
  `aprgo.conf` and prints the restart command. Recovery from lockout
  is a two-command shell flow.
- Session-key rotation: blank the field in `aprgo.conf` and restart;
  aprgo mints a fresh 32-byte key on load.

## Notifications

Add notification support for incoming messages, bulletins, and other configurable alerts.

- **Browser push notifications**
  - Web Push (VAPID) for incoming directed messages, bulletins matching filters, etc.
  - Per-user filters: callsign whitelist, bulletin group match, keyword match.
  - Toggle in web UI, but underlying VAPID keys live in the config file.

- **Email notifications over user-defined SMTP**
  - SMTP host / port / username / password / from-address configured in the **config
    file only** (not in the OTA settings web UI — credentials must not be editable
    remotely).
  - Same filter system as browser notifications (callsign / bulletin group / keywords).
  - Optional digest mode (batch every N minutes) vs. immediate send.
  - Test-send button in the web UI (action only — no credential editing).
