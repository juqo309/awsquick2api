package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestParseUsageSummaryAndUpstreamExtraction(t *testing.T) {
	raw := json.RawMessage(`{
		"user_info": {
			"email": "user@example.test",
			"usage_summary": {
				"entitlementStatus": "ALLOWED",
				"overageEnabled": false,
				"monthlyUsage": {
					"availableUnits": 597,
					"provisionedUnits": 600,
					"resetsAt": 1796083200,
					"usedPercentage": 0
				},
				"sessionUsage": {
					"resumeInMinutes": 0,
					"usedPercentage": 12.5
				},
				"subscription": {
					"subscriptionType": "QUICK_LITE_FREE_TRIAL",
					"freeTrialMetadata": {
						"expiryDate": 1798008323,
						"status": "ACTIVE"
					}
				}
			}
		}
	}`)
	q := quotaFromUpstream(raw, 1795420900)
	if q == nil {
		t.Fatal("expected quota from upstream")
	}
	if q.SubscriptionType != "QUICK_LITE_FREE_TRIAL" || q.TrialStatus != "ACTIVE" || q.TrialExpiresAt != 1798008323 {
		t.Fatalf("unexpected subscription fields: %+v", q)
	}
	if q.MonthlyLimit != 600 || q.MonthlyAvailable != 597 || q.MonthlyUsed != 3 || q.MonthlyPct != 0.5 || q.MonthlyResetsAt != 1796083200 {
		t.Fatalf("unexpected monthly fields: %+v", q)
	}
	if q.SessionPct == nil || *q.SessionPct != 12.5 || q.SessionResumeMin != 0 || q.UpdatedAt != 1795420900 {
		t.Fatalf("unexpected session fields: %+v", q)
	}
}

func TestRefreshAccountQuotaFetchesAndPersists(t *testing.T) {
	var gotAuth string
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"usageSummary": {
				"entitlementStatus": "ALLOWED",
				"monthlyUsage": {
					"availableUnits": 450,
					"provisionedUnits": 600,
					"resetsAt": 1796083200,
					"usedPercentage": 25
				},
				"sessionUsage": {
					"resumeInMinutes": 10,
					"usedPercentage": 40
				},
				"subscription": {
					"subscriptionType": "QUICK_PLUS"
				}
			}
		}`))
	}))
	defer ts.Close()

	dir := t.TempDir()
	srv := NewServer(&Config{
		AccountsFile: filepath.Join(dir, "accounts.json"),
		StatsFile:    filepath.Join(dir, "stats.json"),
	})
	acc := Account{
		ID:       "acc_quota_test",
		Email:    "user@example.test",
		Provider: "enterprise",
		Enabled:  true,
		Session: Session{
			JWT:         "session-jwt",
			WSHost:      "example.test",
			TenantURL:   ts.URL,
			Provider:    "enterprise",
			UsableUntil: time.Now().Add(time.Hour).Unix(),
		},
		Upstream: json.RawMessage(`{"provider":"enterprise","tenant_url":"` + ts.URL + `","tokens":{"access_token":"tok-123"}}`),
	}
	srv.accounts.data.Accounts = []Account{acc}
	if err := srv.refreshAccountQuota(context.Background(), acc.ID, true); err != nil {
		t.Fatalf("refreshAccountQuota failed: %v", err)
	}
	if gotAuth != "Bearer tok-123" || gotPath != "/integration/quick-work/user-info" {
		t.Fatalf("unexpected upstream request: auth=%q path=%q", gotAuth, gotPath)
	}
	updated, err := srv.accounts.Get(acc.ID)
	if err != nil || updated.Quota == nil {
		t.Fatalf("quota not persisted: err=%v quota=%+v", err, updated.Quota)
	}
	if updated.Quota.MonthlyUsed != 150 || updated.Quota.MonthlyPct != 25 || updated.Quota.SessionPct == nil || *updated.Quota.SessionPct != 40 || updated.Quota.SessionResumeMin != 10 {
		t.Fatalf("unexpected persisted quota: %+v", updated.Quota)
	}
	pub := publicAccount(updated, time.Now().Unix())
	if pub["quota"] == nil {
		t.Fatal("publicAccount missing quota field")
	}
}
