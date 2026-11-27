package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestPanelAuthFlow(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "panel_auth.json")
	cfg := &Config{
		AccountsFile:  filepath.Join(dir, "accounts.json"),
		StatsFile:     filepath.Join(dir, "stats.json"),
		TokensFile:    filepath.Join(dir, "tokens.json"),
		PanelAuthFile: authPath,
		Models:        []string{"smart"},
		DefaultModel:  "smart",
	}
	h := NewServer(cfg).Handler()
	do := func(method, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		for _, c := range cookies {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// No password file: dashboard stays open.
	if rec := do("GET", "/admin/config", ""); rec.Code != http.StatusOK {
		t.Fatalf("open admin = %d", rec.Code)
	}

	if err := SetPanelPassword(authPath, "s3cret pass"); err != nil {
		t.Fatal(err)
	}
	if rec := do("GET", "/admin/config", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("admin without login = %d", rec.Code)
	}
	if rec := do("GET", "/", ""); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("index without login = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := do("GET", "/login", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "login-form") {
		t.Fatalf("login page = %d", rec.Code)
	}
	if rec := do("GET", "/healthz", ""); rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d", rec.Code)
	}
	if rec := do("POST", "/auth/login", `{"password":"wrong"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d", rec.Code)
	}
	rec := do("POST", "/auth/login", `{"password":"s3cret pass"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d %s", rec.Code, rec.Body.String())
	}
	var session *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == panelCookieName {
			session = c
		}
	}
	if session == nil || !session.HttpOnly {
		t.Fatalf("missing httponly session cookie")
	}
	if rec := do("GET", "/admin/config", "", session); rec.Code != http.StatusOK {
		t.Fatalf("admin with login = %d", rec.Code)
	}
	if rec := do("GET", "/", "", session); rec.Code != http.StatusOK {
		t.Fatalf("index with login = %d", rec.Code)
	}
	forged := &http.Cookie{Name: panelCookieName, Value: "99999999999.deadbeef"}
	if rec := do("GET", "/admin/config", "", forged); rec.Code != http.StatusUnauthorized {
		t.Fatalf("forged cookie = %d", rec.Code)
	}

	// Changing the password rotates the signing secret and ends sessions.
	if err := SetPanelPassword(authPath, "another"); err != nil {
		t.Fatal(err)
	}
	if rec := do("GET", "/admin/config", "", session); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old session after password change = %d", rec.Code)
	}
}

func TestAdminRemoteRequiresPassword(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "panel_auth.json")
	cfg := &Config{
		AccountsFile:  filepath.Join(dir, "accounts.json"),
		StatsFile:     filepath.Join(dir, "stats.json"),
		TokensFile:    filepath.Join(dir, "tokens.json"),
		PanelAuthFile: authPath,
		Models:        []string{"smart"},
		DefaultModel:  "smart",
	}
	h := NewServer(cfg).Handler()
	remote := func(method, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = "203.0.113.10:443"
		for _, c := range cookies {
			req.AddCookie(c)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := remote("GET", "/admin/config"); rec.Code != http.StatusForbidden {
		t.Fatalf("open admin from remote = %d %s", rec.Code, rec.Body.String())
	}

	if err := SetPanelPassword(authPath, "s3cret pass"); err != nil {
		t.Fatal(err)
	}
	if rec := remote("GET", "/admin/config"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("passworded admin without cookie = %d", rec.Code)
	}

	login := httptest.NewRequest("POST", "/auth/login", strings.NewReader(`{"password":"s3cret pass"}`))
	login.RemoteAddr = "203.0.113.10:443"
	loginRec := httptest.NewRecorder()
	h.ServeHTTP(loginRec, login)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("remote login = %d %s", loginRec.Code, loginRec.Body.String())
	}
	var session *http.Cookie
	for _, c := range loginRec.Result().Cookies() {
		if c.Name == panelCookieName {
			session = c
		}
	}
	if session == nil {
		t.Fatal("missing session cookie")
	}
	if rec := remote("GET", "/admin/config", session); rec.Code != http.StatusOK {
		t.Fatalf("remote admin with login = %d %s", rec.Code, rec.Body.String())
	}
}

func TestPanelAuthLockout(t *testing.T) {
	authPath := filepath.Join(t.TempDir(), "panel_auth.json")
	if err := SetPanelPassword(authPath, "right"); err != nil {
		t.Fatal(err)
	}
	p := NewPanelAuth(authPath)
	try := func(pw string) int {
		req := httptest.NewRequest("POST", "/auth/login", strings.NewReader(`{"password":"`+pw+`"}`))
		req.RemoteAddr = "127.0.0.1:1"
		req.Header.Set("X-Forwarded-For", "203.0.113.9")
		rec := httptest.NewRecorder()
		p.handleLogin(rec, req)
		return rec.Code
	}
	for i := 0; i < panelMaxFailures; i++ {
		if code := try("nope"); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d", i, code)
		}
	}
	if code := try("right"); code != http.StatusTooManyRequests {
		t.Fatalf("locked out login = %d", code)
	}
}
