package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixtureAccount(id string, expiry int64) Account {
	return Account{ID: id, Email: id + "@example.test", Enabled: true, Provider: "social", Session: Session{JWT: "fixture", WSHost: "example.test", Provider: "social", UsableUntil: expiry}, Upstream: json.RawMessage(`{"tokens":{"refresh_token":"fixture"}}`)}
}

func TestRoutingWithoutDefault(t *testing.T) {
	now := time.Now().Unix()
	disabled := fixtureAccount("disabled", now+100)
	disabled.Enabled = false
	s := &AccountStore{data: accountFile{Accounts: []Account{disabled, fixtureAccount("expired", now-1), fixtureAccount("a", now+100), fixtureAccount("b", now+100)}}}
	for i, want := range []string{"a", "b", "a"} {
		a, ok, managed := s.Select("", now)
		if !ok || !managed || a.ID != want {
			t.Fatalf("request %d: got %q, want %q", i, a.ID, want)
		}
	}
	a, _, _ := s.Select("thread-1", now)
	b, _, _ := s.Select("thread-1", now)
	if a.ID != b.ID {
		t.Fatal("thread account changed")
	}
	srv := &Server{cfg: &Config{}, accounts: s, sessions: NewSessionSource(&Config{})}
	for _, id := range []string{"missing", "disabled", "expired"} {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("X-Account-Id", id)
		if _, _, _, err := srv.resolveAccount(r); err == nil {
			t.Fatalf("explicit %s account should not fall back", id)
		}
	}
}

func TestKiroIdentityRequiresMatchingQuickSignIn(t *testing.T) {
	store := &AccountStore{path: filepath.Join(t.TempDir(), "accounts.json")}
	dir := t.TempDir()
	a := fixtureAccount("builder", time.Now().Add(time.Hour).Unix())
	a.Upstream = json.RawMessage(`{"user_info":{"email":"builder@example.test"},"tokens":{"refresh_token":"quick-only"}}`)
	if err := a.WriteWorkDir(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportLogin(dir, "", "", "other@example.test", "Dev"); err == nil {
		t.Fatal("mismatched identity imported")
	}
	if len(store.Snapshot()) != 0 {
		t.Fatal("mismatched identity was persisted")
	}
	first, err := store.ImportLogin(dir, "", "", "Builder@example.test", "Dev")
	if err != nil || first.Label != "Dev" {
		t.Fatalf("matching Quick login not imported: %v", err)
	}
	second, err := store.ImportLogin(dir, "", "", "builder@example.test", "New label")
	if err != nil || second.ID != first.ID || second.Label != "Dev" || len(store.Snapshot()) != 1 {
		t.Fatalf("existing identity was not reused: %v", err)
	}
}

func TestAutomaticRefreshAndBackoff(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "refresh.sh")
	fixture := `#!/bin/sh
case "$1" in refresh) ;; *) exit 1 ;; esac
if grep -q failed.example.test "$QW_DATA_DIR/session.json"; then exit 1; fi
printf '%s\n' '{"jwt":"renewed","ws_host":"example.test","provider":"social","usable_until":4102444800}' > "$QW_DATA_DIR/session.json"
`
	if err := os.WriteFile(script, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	soon := fixtureAccount("soon", now+60)
	failed := fixtureAccount("failed", now-1)
	failed.Session.WSHost = "failed.example.test"
	disabled := fixtureAccount("disabled", now-1)
	disabled.Enabled = false
	fresh := fixtureAccount("fresh", now+1800)
	s := NewServer(&Config{AccountsFile: filepath.Join(dir, "accounts.json"), StatsFile: filepath.Join(dir, "stats.json"), AuthScript: script, Python: "sh"})
	s.accounts.data.Accounts = []Account{soon, failed, disabled, fresh}
	s.refreshDue(context.Background())
	got, _ := s.accounts.Get("soon")
	if got.Session.JWT != "renewed" || s.refreshes.status("soon").LastSuccess == 0 {
		t.Fatal("scheduled refresh did not update session")
	}
	failure := s.refreshes.status("failed")
	if failure.LastFailure == 0 || failure.NextRetry <= now || failure.Running {
		t.Fatalf("missing refresh backoff: %+v", failure)
	}
	s.refreshDue(context.Background())
	if s.refreshes.status("failed") != failure {
		t.Fatal("backoff was not respected")
	}
	if s.refreshes.status("disabled") != (refreshStatus{}) || s.refreshes.status("fresh") != (refreshStatus{}) {
		t.Fatal("unnecessary refresh attempted")
	}
	if _, err := s.accounts.Get("soon"); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshDoesNotRestoreDeletedAccountOrOverwriteSettings(t *testing.T) {
	dir := t.TempDir()
	a := fixtureAccount("a", time.Now().Unix())
	s := &AccountStore{path: filepath.Join(dir, "accounts.json"), data: accountFile{Accounts: []Account{a}}}
	enabled := false
	if err := s.SetSettings("a", "http://example.test:80", "New label", &enabled); err != nil {
		t.Fatal(err)
	}
	next := a
	next.Session.JWT = "renewed"
	if err := s.UpdateRefreshed(a, next); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("a")
	if got.Enabled || got.Label != "New label" || got.Proxy != "http://example.test:80" || got.Session.JWT != "renewed" {
		t.Fatal("refresh overwrote account settings")
	}
	if err := s.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRefreshed(got, next); err == nil {
		t.Fatal("refresh restored a removed account")
	}
	sessionFile := filepath.Join(dir, "session.json")
	encoded, _ := json.Marshal(a.Session)
	if err := os.WriteFile(sessionFile, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if got := NewAccountStore(s.path, sessionFile, "").Snapshot(); len(got) != 0 {
		t.Fatal("legacy migration restored removed accounts")
	}
}

func TestNormalizeProxyURL(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{" ", ""},
		{"proxy.example.test:6366:alice:secret", "http://alice:secret@proxy.example.test:6366"},
		{"[2001:db8::1]:8080:alice:p@ss:/", "http://alice:p%40ss%3A%2F@[2001:db8::1]:8080"},
		{"socks5://alice:secret@proxy.example.test:1080", "socks5://alice:secret@proxy.example.test:1080"},
	} {
		got, err := normalizeProxyURL(tc.input)
		if err != nil || got != tc.want {
			t.Errorf("normalizeProxyURL(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
		}
	}
	for _, input := range []string{
		"proxy.example.test:65536:alice:bad-secret",
		"proxy.example.test:80@other.example.test:alice:bad-secret",
		"proxy.example.test:8080:alice",
		"ftp://proxy.example.test:8080",
	} {
		if _, err := normalizeProxyURL(input); err == nil || strings.Contains(err.Error(), "bad-secret") {
			t.Errorf("expected a credential-free validation error for %q", input)
		}
	}
}

func TestAccountUsageAndPublicList(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{AccountsFile: filepath.Join(dir, "accounts.json"), StatsFile: filepath.Join(dir, "stats.json")}
	s := NewServer(cfg)
	a := fixtureAccount("a", time.Now().Add(time.Hour).Unix())
	s.accounts.data.Accounts = []Account{a}
	s.stats.Record(CallRecord{AccountID: "a", Status: "ok", PromptTokens: 5, CacheReadTokens: 2})
	s.stats.Record(CallRecord{AccountID: "a", Status: "error"})
	got := NewStats(cfg.StatsFile).AccountUsage()["a"]
	if got.Requests != 2 || got.Errors != 1 || got.PromptTokens != 5 || got.CacheReadTokens != 2 {
		t.Fatalf("unexpected stats: %+v", got)
	}
	w := httptest.NewRecorder()
	s.adminAccounts(w)
	body := w.Body.String()
	for _, missing := range []string{"active_id", "refresh_token", "fixture"} {
		if strings.Contains(body, missing) {
			t.Fatalf("public account list exposes %q", missing)
		}
	}
	if !strings.Contains(body, `"requests":2`) {
		t.Fatal("missing per-account usage")
	}
}

func TestClearErrors(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{AccountsFile: filepath.Join(dir, "accounts.json"), StatsFile: filepath.Join(dir, "stats.json")}
	s := NewServer(cfg)
	s.accounts.data.Accounts = []Account{fixtureAccount("a", time.Now().Add(time.Hour).Unix())}
	s.stats.Record(CallRecord{AccountID: "a", Model: "smart", Status: "ok", PromptTokens: 4})
	s.stats.Record(CallRecord{AccountID: "a", Model: "smart", Status: "error", Error: "boom"})
	r := httptest.NewRequest(http.MethodPost, "/admin/stats/clear-errors", nil)
	r.RemoteAddr = "127.0.0.1:1111"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("clear-errors: %d %s", w.Code, w.Body)
	}
	snap := s.stats.Snapshot(10)
	if snap.Errors != 0 || snap.Requests != 1 || snap.Success != 1 || len(snap.Recent) != 1 || snap.Recent[0].Status != "ok" {
		t.Fatalf("stats: %+v", snap)
	}
	if snap.ByModel["smart"].Errors != 0 || snap.ByModel["smart"].Requests != 1 {
		t.Fatalf("model: %+v", snap.ByModel["smart"])
	}
	usage := s.stats.AccountUsage()["a"]
	if usage.Errors != 0 || usage.Requests != 1 {
		t.Fatalf("account: %+v", usage)
	}
}
