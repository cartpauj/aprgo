// Package auth: minimal single-admin session auth via signed cookies.
//
// Password and session key come from the state.Store so they persist across
// restarts and can be changed at runtime via the settings page.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aprgo/internal/state"
)

const (
	AdminUser    = "admin"
	cookieName   = "aprgo_session"
	// LAN-admin console — most operators leave it open in a browser tab and
	// expect not to be logged out. 30 days is long enough to never bother a
	// regular operator while still bounded so abandoned sessions expire.
	cookieMaxAge = 30 * 24 * time.Hour
)

// Authenticator wraps the state store for credentials.
type Authenticator struct {
	store *state.Store
}

func New(store *state.Store) *Authenticator {
	return &Authenticator{store: store}
}

// Check returns true iff the user is "admin" and the password matches.
func (a *Authenticator) Check(user, pass string) bool {
	if user != AdminUser {
		// Don't short-circuit on user mismatch (timing-equal-ish);
		// always run bcrypt against something so attackers can't time-detect user names.
		_ = a.store.CheckPassword(pass)
		return false
	}
	return a.store.CheckPassword(pass)
}

// IssueCookie writes a signed session cookie identifying user.
// Cookie payload includes the current PasswordGeneration so changing the
// password invalidates all outstanding sessions.
func (a *Authenticator) IssueCookie(w http.ResponseWriter, r *http.Request, user string) {
	exp := time.Now().Add(cookieMaxAge).Unix()
	gen := a.store.PasswordGeneration()
	payload := fmt.Sprintf("%s|%d|%d", user, gen, exp)
	mac := a.sign(payload)
	val := base64.RawURLEncoding.EncodeToString([]byte(payload + "|" + mac))
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    val,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// Secure is auto-enabled when the request reached us over TLS
		// (direct HTTPS or behind a trusted reverse proxy passing
		// X-Forwarded-Proto=https). For the typical LAN-HTTP deploy this
		// stays false; CSRF protection (token + Origin + SameSite=Strict)
		// covers the threat model in either case.
		Secure:  isTLSRequest(r),
		Expires: time.Now().Add(cookieMaxAge),
	})
}

// isTLSRequest reports whether the request was carried over TLS. Direct
// TLS sets r.TLS; behind a reverse proxy we check X-Forwarded-Proto. The
// X-Forwarded-Proto check is only meaningful when the proxy is trusted —
// callers should run aprgo behind a known proxy if they rely on that path,
// otherwise a malicious client could spoof the header to flip Secure=true
// (which would only HURT them by making the cookie un-readable over plain
// HTTP — so the spoof is self-defeating; we accept the simplicity).
func isTLSRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// ClearCookie expires the session cookie.
func (a *Authenticator) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
}

// Validate returns the username if the session cookie is valid, else "".
func (a *Authenticator) Validate(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return ""
	}
	parts := strings.Split(string(raw), "|")
	if len(parts) != 4 {
		return ""
	}
	user, genStr, expStr, gotMac := parts[0], parts[1], parts[2], parts[3]
	wantMac := a.sign(user + "|" + genStr + "|" + expStr)
	if !hmac.Equal([]byte(gotMac), []byte(wantMac)) {
		return ""
	}
	gen, err := strconv.ParseUint(genStr, 10, 64)
	if err != nil || gen != a.store.PasswordGeneration() {
		return ""
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return ""
	}
	return user
}

// RequireLogin is HTTP middleware: redirects to /login if not authed.
func (a *Authenticator) RequireLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Validate(r) == "" {
			http.Redirect(w, r, "/login?next="+r.URL.Path, http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Authenticator) sign(payload string) string {
	m := hmac.New(sha256.New, a.store.SessionKey())
	_, _ = m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
