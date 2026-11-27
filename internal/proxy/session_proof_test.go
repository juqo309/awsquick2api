package proxy

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestAdmitCheckSeesFullAccountAndProxy(t *testing.T) {
	dir := t.TempDir()
	store := NewAccountStore(filepath.Join(dir, "accounts.json"), "", "")
	var seen Account
	store.SetAdmitCheck(func(acc Account) (Account, error) {
		seen = acc
		if acc.Session.JWT == "" || acc.Session.WSHost == "" {
			return Account{}, fmt.Errorf("missing session")
		}
		if acc.Proxy != "http://user:pass@proxy.example.test:8080" {
			return Account{}, fmt.Errorf("proxy not passed: %q", acc.Proxy)
		}
		acc.Email = "proved@example.test"
		return acc, nil
	})
	now := time.Now().Unix()
	in := Account{
		Email:      "raw@example.test",
		Provider:   "social",
		AWSAccount: "123",
		Proxy:      "http://user:pass@proxy.example.test:8080",
		Enabled:    true,
		Session:    Session{JWT: "live-jwt", WSHost: "example.test", Provider: "social", AccountID: "123", UsableUntil: now + 3600},
		Upstream:   []byte(`{"tokens":{"access_token":"live-jwt","refresh_token":"rt"}}`),
	}
	got, created, err := store.proveThenAdmit(in, -1)
	if err != nil {
		t.Fatal(err)
	}
	if !created || got.Email != "proved@example.test" {
		t.Fatalf("got %+v created=%v", got, created)
	}
	if seen.Proxy == "" || seen.Session.JWT != "live-jwt" {
		t.Fatalf("check did not receive creds: %+v", seen)
	}
	if len(store.Snapshot()) != 1 {
		t.Fatal("account was not pooled")
	}
}

func TestAdmitCheckRejectsDeadSession(t *testing.T) {
	dir := t.TempDir()
	store := NewAccountStore(filepath.Join(dir, "accounts.json"), "", "")
	store.SetAdmitCheck(func(Account) (Account, error) {
		return Account{}, fmt.Errorf("user-info HTTP 401")
	})
	in := Account{
		Email:    "dead@example.test",
		Enabled:  true,
		Session:  Session{JWT: "dead", WSHost: "example.test"},
		Upstream: []byte(`{}`),
	}
	if _, _, err := store.proveThenAdmit(in, -1); err == nil {
		t.Fatal("expected proof failure")
	}
	if len(store.Snapshot()) != 0 {
		t.Fatal("dead session was pooled")
	}
}
