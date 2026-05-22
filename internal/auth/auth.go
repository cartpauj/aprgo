// Package auth: minimal single-admin session auth via signed cookies.
//
// Username, password hash, and session HMAC key all come from the
// config.Store (loaded at startup, persisted to aprgo.conf). The username
// is operator-configurable — there is no hardcoded "admin" any longer.
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

	"aprgo/internal/config"
)

const (
	cookieName = "aprgo_session"
	// LAN-admin console — most operators leave it open in a browser tab and
	// expect not to be logged out. 30 days is long enough to never bother a
	// regular operator while still bounded so abandoned sessions expire.
	cookieMaxAge = 30 * 24 * time.Hour
)

// Authenticator wraps the config store for credentials.
type Authenticator struct {
	cfg *config.Store
}

func New(cfg *config.Store) *Authenticator {
	return &Authenticator{cfg: cfg}
}

// Check returns true iff `user` matches the configured admin username and
// `pass` matches the configured password hash. Always runs the bcrypt
// compare even on a username mismatch so attackers can't time-detect
// valid usernames.
func (a *Authenticator) Check(user, pass string) bool {
	want := a.cfg.Username()
	ok := a.cfg.CheckPassword(pass)
	return ok && user == want
}

// IssueCookie writes a signed session cookie identifying user.
// Cookie payload includes the current PasswordGeneration so changing the
// password invalidates all outstanding sessions.
func (a *Authenticator) IssueCookie(w http.ResponseWriter, r *http.Request, user string) {
	exp := time.Now().Add(cookieMaxAge).Unix()
	gen := a.cfg.PasswordGeneration()
	payload := fmt.Sprintf("%s|%d|%d", user, gen, exp)
	mac := a.sign(payload)
	val := base64.RawURLEncoding.EncodeToString([]byte(payload + "|" + mac))
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    val,
		Path:     "/",
		HttpOnly: true,
		// Lax (not Strict): Strict blocks the cookie on the redirected GET
		// after the login POST, leaving the user stuck on a redirect loop
		// back to /login. Lax allows top-level GET navigations to carry
		// the cookie while still blocking cross-site POSTs — which is the
		// CSRF surface we actually care about; requireCSRF enforces a
		// synchronizer token on top of that for mutating routes.
		SameSite: http.SameSiteLaxMode,
		// Secure is auto-enabled when the request reached us over TLS
		// (direct HTTPS or behind a trusted reverse proxy passing
		// X-Forwarded-Proto=https). The transport gate already redirects
		// non-loopback HTTP to HTTPS for critical paths, so under normal
		// operation logins on those paths happen over TLS and the cookie
		// picks up Secure here.
		Secure:  isTLSRequest(r),
		Expires: time.Now().Add(cookieMaxAge),
	})
}

// isTLSRequest reports whether the request was carried over TLS. Direct
// TLS sets r.TLS; behind a reverse proxy we check X-Forwarded-Proto.
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
	if err != nil || gen != a.cfg.PasswordGeneration() {
		return ""
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return ""
	}
	// Reject cookies whose embedded username no longer matches the
	// configured admin username — covers the case where the operator
	// renames the account: existing sessions for the old username
	// invalidate immediately on the next request.
	if user != a.cfg.Username() {
		return ""
	}
	return user
}

// RequireLogin is HTTP middleware: redirects to /login if not authed.
//
// Two redirect strategies, picked by request type:
//
//   - HTMX request (has HX-Request header): respond 200 with HX-Redirect:
//     /login so the browser does a full-page navigation. A bare 302 here
//     would be silently followed by HTMX's XHR and the login page's HTML
//     would land in whatever target the original swap pointed at —
//     producing the "login card overlapping the Save button" mangling
//     reported in the wild.
//
//   - Normal browser navigation: 302 to /login?next=<path>. We only embed
//     next= when the original method is GET — POST URLs (handleAccount,
//     handleSettingsSave, etc.) aren't safely reachable via a post-login
//     redirect (the body is gone), so we'd just land on a "POST required"
//     stub. For non-GET, drop next= and let the user land at /.
func (a *Authenticator) RequireLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Validate(r) == "" {
			target := "/login"
			if r.Method == http.MethodGet {
				target = "/login?next=" + r.URL.Path
			}
			if r.Header.Get("HX-Request") != "" {
				w.Header().Set("HX-Redirect", target)
				w.WriteHeader(http.StatusOK)
				return
			}
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *Authenticator) sign(payload string) string {
	m := hmac.New(sha256.New, a.cfg.SessionKey())
	_, _ = m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
