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
	// Digipeating: split into two independent flags so fill-in and full-digi
	// roles can be configured separately. Both follow the standard APRS
	// New-N Paradigm decrement (see internal/gate).
	DigipeatWIDE1 bool `json:"digipeat_wide1"` // handle WIDE1-1 (fill-in role)
	DigipeatWIDE2 bool `json:"digipeat_wide2"` // handle WIDE2-N, N≤2 (full-digi role)
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

	// UI / housekeeping
	Theme             string `json:"theme"`              // auto|light|dark
	RetentionDays     int    `json:"retention_days"`     // 0 = forever
	AdminPasswordHash string `json:"admin_password_hash"`
	SessionKey        string `json:"session_key"`        // base64

	// First-run wizard tracking
	SetupComplete bool `json:"setup_complete"`
}

const (
	DefaultISServer      = "rotate.aprs2.net:14580"
	DefaultBeaconDest    = "APRGO"
	DefaultBeaconEveryS  = 600 // 10 minutes (the APRS community minimum for fixed stations)
	DefaultRetentionDays = 30
	DefaultTheme         = "auto"
)

// Store wraps the persisted State with an RWMutex and a subscriber list.
type Store struct {
	path string

	mu              sync.RWMutex
	cur             State
	isDefaultPass   bool // cached; refreshed only on SetPassword + Open
	passwordChanges uint64 // incremented every SetPassword; used by auth to invalidate sessions

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
	return fmt.Sprintf("%s%02d%05.2f%s%c%03d%05.2f%s%c%s",
		posType, latDeg, latMin, latH, sym[0], lonDeg, lonMin, lonH, sym[1], b.Comment)
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
