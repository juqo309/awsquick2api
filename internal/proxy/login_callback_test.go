package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseLoginCallback(t *testing.T) {
	for _, raw := range []string{
		"http://localhost:18080/callback?nonce=fixture&region=us-east-1",
		"http://127.0.0.1:18080/?state=fixture&code=test%2Bcode",
		"http://[::1]:18080/?state=fixture&error=access_denied",
	} {
		u, err := parseLoginCallback(raw)
		if err != nil || u.Host != "127.0.0.1:18080" {
			t.Fatalf("valid callback %q: %v %v", raw, u, err)
		}
	}
	for _, raw := range []string{
		"https://localhost:18080/?state=fixture&code=test",
		"http://example.test:18080/?state=fixture&code=test",
		"http://localhost:8080/?state=fixture&code=test",
		"http://user@localhost:18080/?state=fixture&code=test",
		"http://localhost:18080/%2f?state=fixture&code=test",
		"http://localhost:18080/?state=fixture&state=other&code=test",
		"http://localhost:18080/?state=fixture",
		"http://localhost:18080/callback?code=test",
	} {
		if _, err := parseLoginCallback(raw); err == nil {
			t.Errorf("accepted invalid callback %q", raw)
		}
	}
}

func TestLoginCallbackRejectsCrossOriginOrInactiveFlow(t *testing.T) {
	s := &Server{login: newLoginJob()}
	for _, tc := range []struct {
		site   string
		status int
	}{
		{"cross-site", http.StatusForbidden},
		{"same-origin", http.StatusConflict},
	} {
		r := httptest.NewRequest(http.MethodPost, "/admin/login/callback", strings.NewReader(`{"callback_url":"http://localhost:18080/?state=fixture&code=test"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Sec-Fetch-Site", tc.site)
		w := httptest.NewRecorder()
		s.adminLoginCallback(w, r)
		if w.Code != tc.status {
			t.Fatalf("site %q: got %d want %d", tc.site, w.Code, tc.status)
		}
	}
}
