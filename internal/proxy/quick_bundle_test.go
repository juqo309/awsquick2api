package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUploadAndExportQuickBundleFormat(t *testing.T) {
	dir := t.TempDir()
	tokensPath := filepath.Join(dir, "tokens.json")
	if err := osWriteTokenFixture(tokensPath, "qw-secret-key"); err != nil {
		t.Fatal(err)
	}
	s := NewServer(&Config{
		AccountsFile: filepath.Join(dir, "accounts.json"),
		StatsFile:    filepath.Join(dir, "stats.json"),
		TokensFile:   tokensPath,
		Surface:      "QUICK_WORK_DESKTOP_WINDOWS",
	})

	payload := map[string]any{
		"format": quickFormatVersion,
		"accounts": []map[string]any{
			{
				"format":         quickFormatVersion,
				"email":          "quick@example.test",
				"label":          "Quick Tester",
				"provider":       "social",
				"build_stage":    "prod",
				"region":         "us-east-1",
				"idc_region":     "us-east-1",
				"issuer_url":     "https://identitycenter.amazonaws.com/ssoins-1234",
				"tenant_url":     socialDataPlaneProd,
				"aws_account_id": "123456789012",
				"client_id":      "cid-1",
				"client_secret":  "csec-1",
				"refresh_token":  "rt-1",
				"access_token":   "at-1",
				"id_token":       "id-1",
				"expires_in":     3600,
				"obtained_at":    time.Now().Unix(),
			},
		},
	}
	raw, _ := json.Marshal(payload)

	unauthReq := httptest.NewRequest(http.MethodPost, "/v1/accounts/upload", bytes.NewReader(raw))
	unauthRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", unauthRec.Code)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/accounts/upload", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer qw-secret-key")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload failed: %d %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if int(resp["created"].(float64)) != 1 || int(resp["uploaded"].(float64)) != 1 {
		t.Fatalf("unexpected upload summary: %#v", resp)
	}

	// Upsert same account again should update, not duplicate.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/accounts/upload", bytes.NewReader(raw))
	req2.Header.Set("Authorization", "Bearer qw-secret-key")
	rec2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("re-upload failed: %d", rec2.Code)
	}
	var resp2 map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &resp2)
	if int(resp2["updated"].(float64)) != 1 || len(s.accounts.Snapshot()) != 1 {
		t.Fatalf("expected single updated account, got %#v len=%d", resp2, len(s.accounts.Snapshot()))
	}

	expReq := httptest.NewRequest(http.MethodGet, "/admin/accounts/export?format=jsonl", nil)
	expReq.RemoteAddr = "127.0.0.1:1234"
	expRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(expRec, expReq)
	if expRec.Code != http.StatusOK {
		t.Fatalf("export failed: %d %s", expRec.Code, expRec.Body.String())
	}
	if !strings.Contains(expRec.Body.String(), `"refresh_token":"rt-1"`) {
		t.Fatalf("exported jsonl missing refresh_token: %s", expRec.Body.String())
	}
}

func osWriteTokenFixture(path, token string) error {
	store := NewTokenStore(path)
	store.mu.Lock()
	defer store.mu.Unlock()
	store.tokens = map[string]tokenEntry{token: {Email: "test", CreatedAt: time.Now().Unix()}}
	return store.saveLocked()
}
