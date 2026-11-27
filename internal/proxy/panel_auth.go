package proxy

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	panelCookieName     = "qp_session"
	panelSessionTTL     = 30 * 24 * time.Hour
	panelPBKDF2Iter     = 600_000
	panelMaxFailures    = 5
	panelFailureWindow  = 15 * time.Minute
	panelLockoutPeriod  = 15 * time.Minute
	panelMaxPasswordLen = 1024
)

// panelAuthFile is the on-disk password record. Only a salted PBKDF2 hash is
// stored; Secret signs session cookies and is rotated whenever the password
// changes, which signs out every existing session.
type panelAuthFile struct {
	Salt       string `json:"salt"`
	Hash       string `json:"hash"`
	Iterations int    `json:"iterations"`
	Secret     string `json:"secret"`
}

// PanelAuth gates the dashboard and /admin API behind a password. When its
// file is missing the dashboard stays open (legacy behavior).
type PanelAuth struct {
	path string

	mu       sync.Mutex
	mod      time.Time
	rec      *panelAuthFile
	bad      bool
	failures map[string]*panelFailure
}

type panelFailure struct {
	count       int
	first       time.Time
	lockedUntil time.Time
}

func NewPanelAuth(path string) *PanelAuth {
	return &PanelAuth{path: path, failures: map[string]*panelFailure{}}
}

// SetPanelPassword writes a new password record to path, replacing any old one.
func SetPanelPassword(path, password string) error {
	if password == "" {
		return errors.New("password must not be empty")
	}
	if len(password) > panelMaxPasswordLen {
		return errors.New("password is too long")
	}
	salt := make([]byte, 16)
	secret := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	hash, err := pbkdf2.Key(sha256.New, password, salt, panelPBKDF2Iter, 32)
	if err != nil {
		return err
	}
	body, err := json.MarshalIndent(panelAuthFile{
		Salt:       hex.EncodeToString(salt),
		Hash:       hex.EncodeToString(hash),
		Iterations: panelPBKDF2Iter,
		Secret:     hex.EncodeToString(secret),
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (p *PanelAuth) reloadLocked() {
	if p.path == "" {
		p.rec, p.bad = nil, false
		return
	}
	st, err := os.Stat(p.path)
	if err != nil {
		p.rec, p.bad, p.mod = nil, !errors.Is(err, os.ErrNotExist), time.Time{}
		return
	}
	if st.ModTime().Equal(p.mod) && (p.rec != nil || p.bad) {
		return
	}
	p.mod = st.ModTime()
	body, err := os.ReadFile(p.path)
	if err != nil {
		p.rec, p.bad = nil, true
		return
	}
	var rec panelAuthFile
	if err := json.Unmarshal(body, &rec); err != nil || rec.Hash == "" || rec.Salt == "" || rec.Secret == "" || rec.Iterations <= 0 {
		p.rec, p.bad = nil, true
		return
	}
	p.rec, p.bad = &rec, false
}

// state returns the current record; enabled is false only when no password
// file exists. A corrupt file keeps auth enabled and rejects everything.
func (p *PanelAuth) state() (rec *panelAuthFile, enabled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reloadLocked()
	return p.rec, p.rec != nil || p.bad
}

func (p *PanelAuth) Enabled() bool {
	_, enabled := p.state()
	return enabled
}

func (p *PanelAuth) sign(rec *panelAuthFile, exp int64) string {
	secret, _ := hex.DecodeString(rec.Secret)
	mac := hmac.New(sha256.New, secret)
	fmt.Fprintf(mac, "panel|%d", exp)
	return hex.EncodeToString(mac.Sum(nil))
}

// Authenticated reports whether the request carries a valid session cookie
// (or auth is disabled).
func (p *PanelAuth) Authenticated(r *http.Request) bool {
	rec, enabled := p.state()
	if !enabled {
		return true
	}
	if rec == nil {
		return false
	}
	c, err := r.Cookie(panelCookieName)
	if err != nil {
		return false
	}
	expStr, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(p.sign(rec, exp)))
}

func (p *PanelAuth) checkPassword(rec *panelAuthFile, password string) bool {
	salt, err1 := hex.DecodeString(rec.Salt)
	want, err2 := hex.DecodeString(rec.Hash)
	if err1 != nil || err2 != nil || len(password) > panelMaxPasswordLen {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, rec.Iterations, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// clientKey identifies the caller for login throttling. Behind the local
// reverse proxy the peer is loopback, so use the forwarded client address.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if f := strings.TrimSpace(first); f != "" {
				return f
			}
		}
	}
	return host
}

func (p *PanelAuth) lockedOut(key string, now time.Time) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if f := p.failures[key]; f != nil && now.Before(f.lockedUntil) {
		return f.lockedUntil.Sub(now)
	}
	return 0
}

func (p *PanelAuth) recordFailure(key string, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, f := range p.failures {
		if now.Sub(f.first) > panelFailureWindow && now.After(f.lockedUntil) {
			delete(p.failures, k)
		}
	}
	f := p.failures[key]
	if f == nil {
		f = &panelFailure{first: now}
		p.failures[key] = f
	}
	f.count++
	if f.count >= panelMaxFailures {
		f.lockedUntil = now.Add(panelLockoutPeriod)
		f.count = 0
		f.first = now
	}
}

func (p *PanelAuth) clearFailures(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.failures, key)
}

func isSecureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (p *PanelAuth) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	rec, enabled := p.state()
	if !enabled {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	key := clientKey(r)
	now := time.Now()
	if wait := p.lockedOut(key, now); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "Too many failed attempts. Try again later."})
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
		return
	}
	if rec == nil || !p.checkPassword(rec, body.Password) {
		p.recordFailure(key, now)
		time.Sleep(400 * time.Millisecond)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Incorrect password."})
		return
	}
	p.clearFailures(key)
	exp := now.Add(panelSessionTTL).Unix()
	http.SetCookie(w, &http.Cookie{
		Name:     panelCookieName,
		Value:    fmt.Sprintf("%d.%s", exp, p.sign(rec, exp)),
		Path:     "/",
		Expires:  time.Unix(exp, 0),
		MaxAge:   int(panelSessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *PanelAuth) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     panelCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *PanelAuth) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"enabled": p.Enabled(), "authenticated": p.Authenticated(r)})
}

// requirePage redirects unauthenticated page loads to the login screen.
func (p *PanelAuth) requirePage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !p.Enabled() && !isLoopback(r) {
			http.Error(w, "set a dashboard password to use the dashboard remotely", http.StatusForbidden)
			return
		}
		if !p.Authenticated(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireAPI rejects unauthenticated API calls with 401.
func (p *PanelAuth) requireAPI(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !p.Authenticated(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "dashboard login required", "login": "/login"})
			return
		}
		next(w, r)
	}
}
