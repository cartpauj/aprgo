// Package config owns aprgo's security-sensitive settings: the admin
// username, bcrypt password hash, session HMAC key, and the lockdown
// flags that restrict what the web UI can do.
//
// Stored as JSON at $ConfigPath (default /var/lib/aprgo/aprgo.conf,
// mode 0600). Written atomically via temp+rename+dir-fsync. The file is
// the canonical recovery path: if an operator locks themselves out by
// enabling Lock Settings in the UI, the only way back in is to edit
// this file directly over SSH and restart aprgo.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

const (
	DefaultUsername = "admin"
	DefaultPassword = "admin"
	fileMode        = 0o600
)

// usernameRE enforces the operator-facing rule: lowercase letters, digits,
// dashes, underscores. Length 1-32. Matches what the Account settings UI
// validates client-side too.
var usernameRE = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// ValidateUsername returns nil if the username is acceptable.
func ValidateUsername(u string) error {
	if !usernameRE.MatchString(u) {
		return errors.New("username must be 1-32 chars of [a-z0-9_-]")
	}
	return nil
}

// Lockdown is the set of UI capabilities the operator has chosen to disable.
// All flags default to false (no lockdown). LockAll is a master that implies
// every other flag — the request handlers consult Effective() so they don't
// need to know the LockAll rule individually.
//
// DisableBulletins covers BOTH bulletin compose / subscription edits AND
// bulletin send (TX). Radio-level TX of beacons + digipeats + gating is
// governed by Settings → Master TX, not by lockdown, so there is no
// separate "disable TX" flag here.
type Lockdown struct {
	LockSettings     bool `json:"lock_settings"`
	DisableMessaging bool `json:"disable_messaging"`
	DisableBulletins bool `json:"disable_bulletins"`
	LockAll          bool `json:"lock_all"`
}

// Effective returns the lockdown flags with LockAll fanned out. Callers
// check the resulting fields directly instead of OR-ing with LockAll every
// time.
func (l Lockdown) Effective() Lockdown {
	if l.LockAll {
		return Lockdown{
			LockSettings:     true,
			DisableMessaging: true,
			DisableBulletins: true,
			LockAll:          true,
		}
	}
	return l
}

// Snapshot is the on-disk form.
type Snapshot struct {
	Username     string   `json:"username"`
	PasswordHash string   `json:"password_hash"`
	SessionKey   string   `json:"session_key"` // base64
	Lockdown     Lockdown `json:"lockdown"`
}

// Store wraps the persisted snapshot with an RWMutex. Single-process owner;
// no subscribers — the values here either persist across boots or get
// re-read by handlers on each request (cheap; the snapshot is tiny).
type Store struct {
	path string

	mu  sync.RWMutex
	cur Snapshot

	// passwordChanges increments on every SetPassword; embedded in session
	// cookies so changing the password invalidates outstanding sessions in
	// the current process. In-memory only — restart resets it, which is
	// fine because cookies also embed an absolute expiry.
	passwordChanges uint64

	// isDefaultPass is cached: the bcrypt compare is expensive enough that
	// the layout-banner check on every request shouldn't pay for it.
	// Refreshed on Open and SetPassword.
	isDefaultPass bool
}

// Open loads or initializes the config file. First-run creates it with
// the default admin/admin credentials and a fresh random session key so
// the operator can log in and reach the setup wizard.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir config dir: %w", err)
	}
	if err := s.load(); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(DefaultPassword), bcrypt.DefaultCost)
		if err != nil {
			return nil, fmt.Errorf("hash default password: %w", err)
		}
		s.cur = Snapshot{
			Username:     DefaultUsername,
			PasswordHash: string(hash),
			SessionKey:   newSessionKey(),
		}
		if err := s.save(); err != nil {
			return nil, err
		}
	} else if !validSessionKey(s.cur.SessionKey) {
		// Existing config but the session_key field is blank, missing, or
		// too short. Operator-friendly rotation: blank the field in
		// aprgo.conf, restart aprgo, and a fresh 32-byte key is minted and
		// persisted on the spot. Existing sessions are invalidated (they
		// were signed with the old key), so everyone is forced to log in.
		s.cur.SessionKey = newSessionKey()
		if err := s.save(); err != nil {
			return nil, err
		}
	}
	s.refreshDefaultPasswordFlag()
	return s, nil
}

// validSessionKey reports whether the persisted session_key is a 32-byte
// (or larger) base64 string. 32 bytes matches what newSessionKey produces
// and is what HMAC-SHA256 wants; shorter keys are treated as "needs
// regeneration" so an operator can rotate just by blanking the field.
func validSessionKey(s string) bool {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return false
	}
	return len(b) >= 32
}

// newSessionKey returns a base64-encoded 32 random bytes suitable for the
// session_key field. Panics on rand.Read failure, which on Linux means
// the kernel CSPRNG is broken — there's no useful recovery.
func newSessionKey() string {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(fmt.Errorf("session key: %w", err))
	}
	return base64.StdEncoding.EncodeToString(key)
}

// Snapshot returns a copy of the current config.
func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}

// Username returns the current admin username.
func (s *Store) Username() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur.Username
}

// SessionKey returns the raw HMAC key.
func (s *Store) SessionKey() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, _ := base64.StdEncoding.DecodeString(s.cur.SessionKey)
	return b
}

// LockdownEffective returns the lockdown flags with LockAll fanned out.
func (s *Store) LockdownEffective() Lockdown {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur.Lockdown.Effective()
}

// CheckPassword constant-time-compares pass against the stored hash.
func (s *Store) CheckPassword(pass string) bool {
	s.mu.RLock()
	hash := s.cur.PasswordHash
	s.mu.RUnlock()
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pass)) == nil
}

// SetPassword hashes and persists a new password. Increments the
// password-change counter so live sessions invalidate.
func (s *Store) SetPassword(newPass string) error {
	if len(newPass) < 4 {
		return errors.New("password must be at least 4 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPass), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.cur.PasswordHash = string(hash)
	if err := s.save(); err != nil {
		s.mu.Unlock()
		return err
	}
	s.passwordChanges++
	s.isDefaultPass = newPass == DefaultPassword
	s.mu.Unlock()
	return nil
}

// SetUsername persists a new admin username. Validated against the
// lowercase-alnum-dash-underscore rule.
func (s *Store) SetUsername(u string) error {
	if err := ValidateUsername(u); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur.Username = u
	return s.save()
}

// SetLockdown persists a new lockdown flag set verbatim (no Effective
// fan-out — the raw flags are what gets stored so an operator who
// unchecks LockAll can recover individual flags they had set before).
func (s *Store) SetLockdown(l Lockdown) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur.Lockdown = l
	return s.save()
}

// IsDefaultPassword reports whether the password is still "admin".
func (s *Store) IsDefaultPassword() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.isDefaultPass
}

// PasswordGeneration returns the in-process counter that increments on
// every password change. Embedded in session cookies so changing the
// password invalidates outstanding ones.
func (s *Store) PasswordGeneration() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.passwordChanges
}

// refreshDefaultPasswordFlag does the expensive bcrypt-compare exactly
// once on Open + SetPassword. Caller must hold s.mu (or be in Open).
func (s *Store) refreshDefaultPasswordFlag() {
	s.isDefaultPass = bcrypt.CompareHashAndPassword(
		[]byte(s.cur.PasswordHash), []byte(DefaultPassword)) == nil
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, &s.cur)
}

// save writes via temp+rename+dir-fsync, same pattern as state.json.
// Caller must hold s.mu.
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
	if dir, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
