# TODO

## Security / Lockdown via Config File

Add a config file (e.g. `aprgo.conf` or `~/.aprgo/config.yaml`) for security-sensitive
settings that should NOT be editable from the web UI (OTA). The goal: a remote/public
server can be locked down so a compromised web session cannot be used to spam APRS-IS
or change credentials.

Config-file-only settings (not editable in web UI):

- **Web UI username / password** — credentials live in the config file. Optional toggle
  to allow editing them OTA (default: off for remote servers).
- **Disable settings page OTA entirely** — read-only settings view; all changes must be
  made by editing the config file on disk.
- **Lockdown flags** for individual OTA capabilities:
  - `lockdown.messaging` — disable sending APRS messages from the web UI
  - `lockdown.bulletins` — disable sending/editing bulletins from the web UI
  - `lockdown.settings` — disable all settings edits from the web UI
  - `lockdown.all` — view-only mode: everything is read-only, no sending, no edits
- **Require login for everything** — option to gate the entire web UI (including stats /
  diagnostics / map) behind login, not just the settings/messaging pages.

Implementation notes:
- Config file should be loaded at startup; OTA settings UI should reflect which fields
  are locked (grey out + tooltip "locked by config file").
- Changing a locked field via the API should return 403, not just hide it in the UI.
- Document a "remote server hardening" preset in the README.

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
