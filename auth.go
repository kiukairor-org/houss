// Shared-household-password auth: a signed, stateless session cookie (survives restarts) and a
// failure-count login rate limit.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	cookieName    = "houss_session"
	sessionTTL    = 365 * 24 * time.Hour
	maxLoginFails = 10
	failWindow    = time.Minute
)

type auth struct {
	password []byte
	key      []byte
	now      func() time.Time

	mu    sync.Mutex
	fails []time.Time
}

func newAuth(password, secret string) *auth {
	if secret == "" {
		secret = password
	}
	k := sha256.Sum256([]byte("houss-session:" + secret))
	return &auth{password: []byte(password), key: k[:], now: time.Now}
}

func (a *auth) sign(exp string) string {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(exp))
	return exp + "." + hex.EncodeToString(m.Sum(nil))
}

func (a *auth) valid(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	exp, _, ok := strings.Cut(c.Value, ".")
	if !ok || subtle.ConstantTimeCompare([]byte(a.sign(exp)), []byte(c.Value)) != 1 {
		return false
	}
	n, err := strconv.ParseInt(exp, 10, 64)
	return err == nil && a.now().Unix() < n
}

// limited reports whether too many recent failures were recorded. The limit is global rather than
// per-IP: behind the ingress every request shares a peer address and X-Forwarded-For is spoofable.
func (a *auth) limited() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	cut := a.now().Add(-failWindow)
	kept := a.fails[:0]
	for _, t := range a.fails {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	a.fails = kept
	return len(a.fails) >= maxLoginFails
}

func (a *auth) fail() {
	a.mu.Lock()
	a.fails = append(a.fails, a.now())
	a.mu.Unlock()
}

func (a *auth) login(w http.ResponseWriter, r *http.Request) {
	if a.limited() {
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many attempts"})
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	// Hash first so the comparison is constant-time regardless of length.
	got, want := sha256.Sum256([]byte(body.Password)), sha256.Sum256(a.password)
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		a.fail()
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "wrong password"})
		return
	}
	exp := a.now().Add(sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: a.sign(strconv.FormatInt(exp.Unix(), 10)), Path: "/",
		Expires: exp, MaxAge: int(sessionTTL / time.Second),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true})
}

func (a *auth) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": false})
}

// require wraps an API handler with the session check.
func (a *auth) require(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.valid(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		h(w, r)
	}
}
