// Package state holds aprgo's single source of configuration truth.
//
// Persisted as JSON at $StateDir/state.json (atomic write). Concurrent readers
// hold an RWMutex; writers commit through Update(fn) which atomically swaps the
// in-memory copy and broadcasts the new snapshot to any subscribers.
package state

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	defaultPassword = "admin"
	fileMode        = 0o600
)

// Mode controls the operating mode chosen by the user (informational; the
// actual behavior is driven by the individual flags below, but keeping the
// chosen mode lets the UI show "you're in fill-in digi mode").
type Mode string

const (
	ModeUnset     Mode = ""
	ModeRXOnly    Mode = "rx-only"
	ModeTXIGate   Mode = "tx-igate"
	ModeDigi      Mode = "digi"
	ModeFillinIG  Mode = "fillin-igate" // digipeat + igate
	ModeMessaging Mode = "messaging"    // selective gating: messages + acks only
	ModeOffline   Mode = "offline"      // RF-only digi, no APRS-IS at all
	ModeIS        Mode = "is"           // APRS-IS only: no TNC/RF — map + messaging + beacons via IS
	ModeAdvanced  Mode = "advanced"     // operator manages individual flags directly
)

// TNCKind discriminates the TNC connection family.
type TNCKind string

const (
	TNCNone   TNCKind = ""
	TNCSerial TNCKind = "serial" // e.g. /dev/rfcomm0, /dev/ttyUSB0
	TNCTCP    TNCKind = "tcp"    // host:port (Direwolf, tnc-server, WiFi TNC)
)

// Beacon is one independently-scheduled APRS beacon. Position is shared from
// the station-wide Lat/Lon (so changing your station's location updates all
// beacons). Symbol and Comment are the operator-facing pieces; the wire-format
// info field is generated at TX time by ComposeInfo(state) from these plus
// the station position.
type Beacon struct {
	Name     string   `json:"name"`              // human label, e.g. "position"
	Symbol   string   `json:"symbol,omitempty"`  // 2-char APRS symbol like "I&" (table or overlay char + code)
	Comment  string   `json:"comment,omitempty"` // free text appended to the position info
	Messages bool     `json:"messages"`          // true → "=" (position with messaging), false → "!"
	Dest     string   `json:"dest,omitempty"`    // tocall; defaults to DefaultBeaconDest
	Path     []string `json:"path,omitempty"`    // digipeater path, e.g. ["WIDE1-1"]
	EveryS   int      `json:"every_s"`           // interval seconds; 0 = disabled even if Enabled
	Enabled  bool     `json:"enabled"`
	// Callsign overrides the station's primary callsign on this beacon's
	// outgoing AX.25 frame. Empty = use State.Callsign. Useful when an
	// operator wants to run a separate SSID per beacon role
	// (e.g. N0CALL-10 for the iGate, N0CALL-1 for a weather beacon).
	Callsign string `json:"callsign,omitempty"`
	// AmbiguityLevel blanks the trailing digits of the broadcast position
	// for privacy, per APRS spec §6 (uncompressed position).
	//   0 = full precision (default)         — ~18 m at the equator
	//   1 = blank hundredths-of-minute       — ~185 m
	//   2 = blank all decimal minutes        — ~1.8 km
	//   3 = also blank units of minutes      — ~18 km
	//   4 = also blank tens of minutes       — ~111 km (degree-level)
	// Receivers interpret blanked digits as "unknown" and plot the
	// position at the midpoint of the resulting box.
	AmbiguityLevel int `json:"ambiguity_level,omitempty"`
}

// State is the JSON-persisted blob.
type State struct {
	// Identity
	Callsign string `json:"callsign"` // "N0CALL-10"
	Passcode string `json:"passcode"` // APRS-IS numeric passcode (or "-1" for receive-only)

	// Position (decimal degrees; 0 means unset)
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`

	// APRS-IS
	ISServer string `json:"is_server"` // "noam.aprs2.net:14580"
	ISFilter string `json:"is_filter"` // server-side filter, e.g. "r/LAT/LON/150"

	// RF / TNC
	TNCKind    TNCKind `json:"tnc_kind"`
	TNCSerial  string  `json:"tnc_serial"`   // device path
	TNCBaud    int     `json:"tnc_baud"`     // 0 = default 9600
	TNCAddr    string  `json:"tnc_addr"`     // for tcp: "host:port"; for serial+bluetooth: the BT address (informational)
	TNCChannel int     `json:"tnc_channel"`  // RFCOMM channel for Bluetooth TNCs; 0 = autodetect (try 1, then SDP)
	TXEnable   bool    `json:"tx_enable"`    // master switch

	// Advanced KISS TNC parameters. Sent on every TNC connect/reconnect via
	// the standard KISS command bytes (0x01-0x04) defined in the 1987 KISS
	// spec. Modern soundmodem/KISS TNCs (Direwolf, Mobilinkd, NinoTNC,
	// Kenwood TM-D710/TH-D74) honor these; legacy TNC-2 hardware ignores
	// them silently. All values 0 mean "leave the TNC at its current setting"
	// (don't emit the command at all).
	TNCTXDelayMs  int `json:"tnc_tx_delay_ms"`  // ms after PTT before data (default 300; many radios need ≥200)
	TNCPersist    int `json:"tnc_persist"`      // 0-255 CSMA p-persistence (default 63 ≈ 25% probability)
	TNCSlotTimeMs int `json:"tnc_slot_time_ms"` // ms between persist samples (default 100)
	TNCTXTailMs   int `json:"tnc_tx_tail_ms"`   // ms PTT held after data (deprecated; default 0)

	// Operating mode
	Mode Mode `json:"mode"`

	// Beacons: list of independently-scheduled beacons. Each can have its own
	// info field, dest, path, and interval — supports the common APRS pattern
	// of running e.g. one position beacon and one status beacon, or
	// per-direction paths.
	Beacons []Beacon `json:"beacons"`

	// Gating
	GateRFtoIS    bool `json:"gate_rf_to_is"`
	GateIStoRF    bool `json:"gate_is_to_rf"`
	// IGateTXPath is the via-path attached to the outer AX.25 frame on
	// IS→RF gated traffic. Per aprs-is.net IGateDetails this is operator-
	// configurable; reference iGates (APRX, Direwolf IGTXVIA, YAAC, PinPoint)
	// default to "WIDE1-1" — one fill-in hop for typical home iGates.
	// Empty string = direct only (appropriate for hilltop sites with wide
	// RF footprint). Validated to max 2 hops.
	IGateTXPath string `json:"igate_tx_path"`

	// NextMsgID is the next outbound APRS message ID to assign. Monotonic
	// decimal counter wrapping 1..99999 — matches APRSdroid / YAAC / UI-View32
	// convention. Persisted so IDs continue across restarts (most clients
	// dedupe by sender+ID, so reusing an ID right after restart would
	// confuse them). Bumped+saved on every outbound message.
	NextMsgID int `json:"next_msg_id"`

	// MessageGroups is the operator's bulletin-group subscription list per
	// Bruninga's PROTOCOL.TXT: empty = receive all groups; non-empty =
	// accept plain BLN0-9 always + only listed group bulletins (matched
	// case-insensitively on the 5-char group suffix of `BLNxGGGGG`).
	MessageGroups []string `json:"message_groups,omitempty"`
	// NWSSubscribed gates the display of NWS / SKY / CWA bulletins on
	// the /bulletins page. Default true; storage is unconditional so
	// flipping it on later still shows historical context.
	NWSSubscribed bool `json:"nws_subscribed"`
	// AllowSendBulletins gates the Compose-bulletin form on /bulletins.
	// Off by default because bulletins are broadcasts to every station
	// in range and the operator should consciously opt in. UI flow:
	// requires Advanced mode + explicit checkbox + JS confirm()
	// agreement that they understand bulletins are broadcasts and
	// shouldn't be abused.
	AllowSendBulletins bool `json:"allow_send_bulletins"`

	// Digipeating: split into two independent flags so fill-in and full-digi
	// roles can be configured separately. Both follow the standard APRS
	// New-N Paradigm decrement (see internal/gate).
	DigipeatWIDE1 bool `json:"digipeat_wide1"` // handle WIDE1-1 (fill-in role)
	DigipeatWIDE2 bool `json:"digipeat_wide2"` // handle WIDE2-N, N≤2 (full-digi role)
	// RegionalAliases is the operator-configured list of state/regional
	// flood-alias prefixes the full-digi role should honor (per fix14439).
	// Each entry is the alias prefix without the n-N suffix, e.g. "ARIZ",
	// "MASS", "NCN", "SAR". A digi configured for "ARIZ" honors ARIZ1-1
	// through ARIZ2-2 just like WIDE1-1 / WIDE2-2. Empty list = WIDE-only.
	// Only honored when DigipeatWIDE2 is true — these are WIDE-class, not
	// fill-in.
	RegionalAliases []string `json:"regional_aliases,omitempty"`
	// ViscousDelay: when handling WIDE1-1 as a fill-in, hold the TX 3–5 s
	// and cancel if a higher-elevation digi already retransmits the same
	// content. Standard APRS politeness — on by default.
	ViscousDelay bool `json:"viscous_delay"`
	// OfflineMode: when true, the APRS-IS client is never started — no
	// socket dial, no reconnect retries, no IS-related UI alarms. RF
	// receive + digipeating + own beacons all continue normally. Picked
	// via ModeOffline in the wizard; ModeAdvanced operators can also set
	// it directly.
	OfflineMode bool `json:"offline_mode"`
	// MessagingOnlyMode: when true, RF→IS gating is restricted to
	// message + ack frames only (no position beacons, weather, telemetry,
	// status etc. from RF get forwarded to APRS-IS). Reduces IS traffic
	// in areas already saturated with iGates while still bridging
	// person-to-person chat. Picked via ModeMessaging in the wizard.
	MessagingOnlyMode bool `json:"messaging_only_mode"`
	// PreemptiveDigipeat: when true, honor packets that list our callsign
	// anywhere in the unused portion of the path — not just as the next
	// hop. Uses MARK mode: prior unused hops are flagged used so the
	// emitted path preserves the operator's original intent. Never
	// triggers on generic WIDEn-N tokens (spec §preemptive-digipeating).
	// Advanced-mode-only; defaults off per Direwolf convention.
	PreemptiveDigipeat bool `json:"preemptive_digipeat"`

	// IGateRecentRFMinutes: how recently a remote station must have been
	// heard on RF before aprgo will IS→RF gate a message to them, and
	// before that station's IS-side traffic shows on the dashboard live
	// feed. Defaults to 360 (6 hours). Other iGate software typically uses
	// 30–60 min; longer is forgiving for fixed digipeaters that beacon
	// infrequently, shorter is safer for mobile stations passing through.
	// 0 means "use the default" (6 hours).
	IGateRecentRFMinutes int `json:"igate_recent_rf_minutes,omitempty"`

	// UI / housekeeping
	Theme             string `json:"theme"`              // auto|light|dark
	RetentionDays     int    `json:"retention_days"`     // 0 = forever
	// Timezone is the IANA name (e.g. "America/Denver"). Empty = use the
	// server's local zone. All stored timestamps are absolute (Unix epoch),
	// so this is purely a display preference — no migration needed when it
	// changes.
	Timezone          string `json:"timezone,omitempty"`
	// TimeFormat is "12h" or "24h" (default "24h"). Empty defaults to 24h.
	TimeFormat        string `json:"time_format,omitempty"`
	AdminPasswordHash string `json:"admin_password_hash"`
	SessionKey        string `json:"session_key"`        // base64

	// First-run wizard tracking
	SetupComplete bool `json:"setup_complete"`
}

const (
	DefaultISServer             = "rotate.aprs2.net:14580"
	DefaultBeaconDest           = "APRGO"
	DefaultBeaconEveryS         = 600 // 10 minutes (the APRS community minimum for fixed stations)
	DefaultRetentionDays        = 3
	DefaultTheme                = "auto"
	DefaultTimeFormat           = "24h"
	DefaultIGateRecentRFMinutes = 30 // APRS-IS IGating.aspx convention
	DefaultIGateTXPath          = "WIDE1-1"

	// KISS TNC parameter defaults (sent on connect if set in state). 0 means
	// "don't emit the command" so users can leave any field blank to defer
	// to the TNC's own default. Recommended starting values for unsure users.
	DefaultTNCTXDelayMs  = 300 // most modern radios need 200-500ms for clean keyup
	DefaultTNCPersist    = 63  // ≈25% probability, the KISS spec recommendation
	DefaultTNCSlotTimeMs = 100 // 100ms between persist samples
	DefaultTNCTXTailMs   = 0   // deprecated; keep at 0 unless TNC vendor docs say otherwise
)

// Store wraps the persisted State with an RWMutex and a subscriber list.
type Store struct {
	path string

	mu              sync.RWMutex
	cur             State
	isDefaultPass   bool // cached; refreshed only on SetPassword + Open
	passwordChanges uint64 // incremented every SetPassword; used by auth to invalidate sessions

	// msgIDCur / msgIDEnd: in-memory msg-ID block reserved from state.json.
	// We persist a watermark (`State.NextMsgID`) ahead of in-memory use so a
	// crash never reuses an ID. NextOutboundMsgID hands out from [cur, end),
	// reserving a new block from disk only when exhausted — turns the
	// per-message disk write into ~1 write per `msgIDBlock` outbound messages.
	msgIDMu  sync.Mutex
	msgIDCur int
	msgIDEnd int

	smu  sync.Mutex
	subs map[chan State]struct{}
}

// Open loads or initializes the state file.
func Open(path string) (*Store, error) {
	s := &Store{path: path, subs: make(map[chan State]struct{})}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir state dir: %w", err)
	}
	if err := s.load(); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(defaultPassword), bcrypt.DefaultCost)
		if err != nil {
			return nil, fmt.Errorf("hash default password: %w", err)
		}
		keyBytes := make([]byte, 32)
		if _, err := rand.Read(keyBytes); err != nil {
			return nil, fmt.Errorf("session key: %w", err)
		}
		s.cur = State{
			ISServer:          DefaultISServer,
			Theme:             DefaultTheme,
			RetentionDays:     DefaultRetentionDays,
			ViscousDelay:      true, // polite-fill-in default
			TNCTXDelayMs:      DefaultTNCTXDelayMs,
			TNCPersist:        DefaultTNCPersist,
			TNCSlotTimeMs:     DefaultTNCSlotTimeMs,
			TNCTXTailMs:       DefaultTNCTXTailMs,
			IGateTXPath:       DefaultIGateTXPath,
			NWSSubscribed:     true, // most operators want severe-weather alerts
			AdminPasswordHash: string(hash),
			SessionKey:        base64.StdEncoding.EncodeToString(keyBytes),
		}
		if err := s.save(); err != nil {
			return nil, err
		}
	} else {
		// Idempotent defaults
		changed := false
		if s.cur.ISServer == "" {
			s.cur.ISServer = DefaultISServer
			changed = true
		}
		if s.cur.Theme == "" {
			s.cur.Theme = DefaultTheme
			changed = true
		}
		if s.cur.RetentionDays == 0 {
			s.cur.RetentionDays = DefaultRetentionDays
			changed = true
		}
		// Default any beacon missing a symbol to the iGate diamond.
		for i := range s.cur.Beacons {
			b := &s.cur.Beacons[i]
			if b.Symbol == "" {
				b.Symbol = "I&"
				changed = true
			}
		}
		if s.cur.IGateTXPath == "" {
			s.cur.IGateTXPath = DefaultIGateTXPath
			changed = true
		}
		if s.cur.TNCTXDelayMs == 0 {
			s.cur.TNCTXDelayMs = DefaultTNCTXDelayMs
			changed = true
		}
		if s.cur.TNCPersist == 0 {
			s.cur.TNCPersist = DefaultTNCPersist
			changed = true
		}
		if s.cur.TNCSlotTimeMs == 0 {
			s.cur.TNCSlotTimeMs = DefaultTNCSlotTimeMs
			changed = true
		}
		// TNCTXTailMs defaults to 0 (KISS spec deprecates it); don't fill.
		if changed {
			_ = s.save()
		}
	}
	s.refreshDefaultPasswordFlag()
	return s, nil
}

// Snapshot returns a copy of the current state.
func (s *Store) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}

// Update runs fn against a mutable copy and commits if fn returns nil.
// Subscribers receive the post-commit snapshot.
func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	cp := s.cur
	if err := fn(&cp); err != nil {
		s.mu.Unlock()
		return err
	}
	s.cur = cp
	if err := s.save(); err != nil {
		s.mu.Unlock()
		return err
	}
	snap := s.cur
	s.mu.Unlock()
	s.notify(snap)
	return nil
}

// msgIDBlock is the reservation chunk size. Saving the state.json on every
// outbound message thrashes SD-card-backed deploys; instead we reserve a
// block of IDs ahead, hand them out from RAM, and only re-persist when the
// block runs out. Worst case after a crash: we skip up to msgIDBlock IDs.
// That's spec-friendly (peers dedupe on src+ID; an unused ID is harmless)
// and matches the SQL-sequence "reserve-block" pattern.
const msgIDBlock = 100

// NextOutboundMsgID hands out the next outbound APRS message ID, wrapping
// 1..99999. Reserves a new on-disk block of `msgIDBlock` IDs when the
// in-memory range is exhausted. Skips Update's subscriber notification
// because the heard list / dashboard don't care about msg-ID changes —
// notifying would cause an rf+is reconnect on every outbound message.
//
// Save errors from the block-reservation are surfaced to the log (previously
// the error was silently swallowed, which could lead to ID reuse across a
// disk-full crash). The returned ID is still valid in-memory; downstream
// peers dedupe by (source, msg_id) so reusing an ID on the *same* sender
// in rapid succession would be the actual hazard — block reservation
// prevents that.
func (s *Store) NextOutboundMsgID() int {
	s.msgIDMu.Lock()
	defer s.msgIDMu.Unlock()
	if s.msgIDCur >= s.msgIDEnd || s.msgIDCur <= 0 || s.msgIDCur > 99999 {
		s.reserveMsgIDBlock()
	}
	n := s.msgIDCur
	s.msgIDCur++
	if s.msgIDCur > 99999 {
		// Wrap mid-block: end of namespace, restart at 1. Force a fresh
		// disk reservation so the on-disk watermark matches reality.
		s.msgIDCur = 0
		s.msgIDEnd = 0
	}
	return n
}

// reserveMsgIDBlock claims the next msgIDBlock IDs on disk and updates
// the in-memory window. Caller holds s.msgIDMu.
func (s *Store) reserveMsgIDBlock() {
	s.mu.Lock()
	persisted := s.cur.NextMsgID
	if persisted <= 0 || persisted > 99999 {
		persisted = 1
	}
	// First in-memory ID = the on-disk watermark (a value we know hasn't
	// been handed out yet on any prior boot — see Open() initialization).
	s.msgIDCur = persisted
	// Advance the on-disk watermark by msgIDBlock, wrapping at the 99999
	// ceiling. Block-end is min(start+block, 100000) so we never hand out
	// 100000 itself — that wraps to 1 on the next reservation.
	end := persisted + msgIDBlock
	if end > 100000 {
		end = 100000
	}
	s.msgIDEnd = end
	if end >= 100000 {
		s.cur.NextMsgID = 1
	} else {
		s.cur.NextMsgID = end
	}
	err := s.save()
	s.mu.Unlock()
	if err != nil {
		// Don't drop the IDs (we already committed in-memory) but log so
		// operators see persistent disk problems. Next reservation will
		// retry the save.
		log.Printf("state: msg-ID block save failed: %v (counter advanced in-memory only)", err)
	}
}

// Subscribe returns a channel that receives a snapshot after every Update.
// Slow subscribers drop events rather than blocking publishers.
func (s *Store) Subscribe() (<-chan State, func()) {
	ch := make(chan State, 4)
	s.smu.Lock()
	s.subs[ch] = struct{}{}
	s.smu.Unlock()
	return ch, func() {
		s.smu.Lock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
		s.smu.Unlock()
	}
}

func (s *Store) notify(snap State) {
	s.smu.Lock()
	defer s.smu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- snap:
		default:
		}
	}
}

// SessionKey returns the persisted HMAC key.
func (s *Store) SessionKey() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, _ := base64.StdEncoding.DecodeString(s.cur.SessionKey)
	return b
}

// CheckPassword constant-time-compares pass against the stored hash.
func (s *Store) CheckPassword(pass string) bool {
	s.mu.RLock()
	hash := s.cur.AdminPasswordHash
	s.mu.RUnlock()
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass)) == nil
}

// SetPassword hashes and persists a new password.
func (s *Store) SetPassword(newPass string) error {
	if len(newPass) < 4 {
		return errors.New("password must be at least 4 characters")
	}
	// bcrypt outside the write lock — ~100ms otherwise blocks all readers.
	hash, err := bcrypt.GenerateFromPassword([]byte(newPass), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	err = s.Update(func(st *State) error {
		st.AdminPasswordHash = string(hash)
		return nil
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.passwordChanges++
	s.isDefaultPass = newPass == defaultPassword
	s.mu.Unlock()
	return nil
}

// IsDefaultPassword reports whether the password is still "admin".
// Cached on Open/SetPassword — does NOT bcrypt-compare on each call.
func (s *Store) IsDefaultPassword() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.isDefaultPass
}

// PasswordGeneration returns a counter that increments on every password
// change. Auth tokens may embed this so old cookies become invalid post-change.
func (s *Store) PasswordGeneration() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.passwordChanges
}

// refreshDefaultPasswordFlag does the (expensive) bcrypt compare exactly once.
// Caller must hold s.mu.
func (s *Store) refreshDefaultPasswordFlag() {
	s.isDefaultPass = bcrypt.CompareHashAndPassword([]byte(s.cur.AdminPasswordHash), []byte(defaultPassword)) == nil
}

// Interval returns the configured beacon interval as a time.Duration, or 0
// if disabled / malformed.
func (b Beacon) Interval() time.Duration {
	if b.EveryS <= 0 || !b.Enabled {
		return 0
	}
	return time.Duration(b.EveryS) * time.Second
}

// ComposeInfo builds the wire-format APRS info field for this beacon using
// the station's Lat/Lon plus the beacon's Symbol + Comment + Messages flag.
func (b Beacon) ComposeInfo(lat, lon float64) string {
	if lat == 0 && lon == 0 {
		return ""
	}
	sym := b.Symbol
	if len(sym) != 2 {
		sym = "I&"
	}
	posType := "="
	if !b.Messages {
		posType = "!"
	}

	// Decimal degrees → APRS DDMM.mm format
	latH := "N"
	if lat < 0 {
		latH = "S"
		lat = -lat
	}
	latDeg := int(lat)
	latMin := (lat - float64(latDeg)) * 60
	if latMin >= 59.995 {
		latMin = 59.99
	}
	lonH := "E"
	if lon < 0 {
		lonH = "W"
		lon = -lon
	}
	lonDeg := int(lon)
	lonMin := (lon - float64(lonDeg)) * 60
	if lonMin >= 59.995 {
		lonMin = 59.99
	}
	latS := fmt.Sprintf("%02d%05.2f%s", latDeg, latMin, latH)
	lonS := fmt.Sprintf("%03d%05.2f%s", lonDeg, lonMin, lonH)
	latS, lonS = applyAmbiguity(latS, lonS, b.AmbiguityLevel)
	return fmt.Sprintf("%s%s%c%s%c%s", posType, latS, sym[0], lonS, sym[1], sanitizeBeaconComment(b.Comment))
}

// sanitizeBeaconComment is a defensive last-line filter against malformed
// state.json edits: strip control chars + APRS-reserved `|`/`~`, cap at 43
// bytes (APRS101 §8). The UI sanitize layer normally catches all of this.
func sanitizeBeaconComment(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7F || c == '|' || c == '~' {
			continue
		}
		out = append(out, c)
	}
	if len(out) > 43 {
		out = out[:43]
	}
	return string(out)
}

// applyAmbiguity blanks trailing digits of the encoded lat/lon strings
// per APRS spec §6. lat is 8 chars (DDMM.mmH), lon is 9 chars (DDDMM.mmH).
// level is clamped to [0,4]; same level applied to both axes.
func applyAmbiguity(lat, lon string, level int) (string, string) {
	if level <= 0 {
		return lat, lon
	}
	if level > 4 {
		level = 4
	}
	if len(lat) != 8 || len(lon) != 9 {
		return lat, lon
	}
	latB := []byte(lat)
	lonB := []byte(lon)
	// Digits to blank, from least to most significant. The '.' separator
	// (lat[4], lon[5]) is preserved at every level.
	latPos := [4]int{6, 5, 3, 2}
	lonPos := [4]int{7, 6, 4, 3}
	for i := 0; i < level; i++ {
		latB[latPos[i]] = ' '
		lonB[lonPos[i]] = ' '
	}
	return string(latB), string(lonB)
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, &s.cur)
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(s.cur, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fileMode)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	// fsync the directory so the rename itself is durable across a power loss.
	if dir, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
