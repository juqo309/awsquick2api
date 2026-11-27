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

func TestRuntimeSettingsPersistAndApply(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{AccountsFile: filepath.Join(dir, "accounts.json"), StatsFile: filepath.Join(dir, "stats.json"), Models: []string{"smart", "fast"}, DefaultModel: "smart", Timeout: time.Minute}
	server := NewServer(cfg)
	settings := server.settings.snapshot()
	settings.DefaultModel, settings.SystemPrompt, settings.ThinkingEffort = "fast", "Reply in one paragraph.", "high"
	settings.TimeoutSec, settings.KeepSessions = 90, true
	post := func(value runtimeSettings, site string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(value)
		r := httptest.NewRequest(http.MethodPost, "/admin/settings", bytes.NewReader(body))
		r.RemoteAddr = "127.0.0.1:1111"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Sec-Fetch-Site", site)
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, r)
		return w
	}
	if w := post(settings, "cross-site"); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site save returned %d", w.Code)
	}
	if w := post(settings, "same-origin"); w.Code != http.StatusOK {
		t.Fatalf("save returned %d: %s", w.Code, w.Body)
	}
	bad := settings
	bad.DefaultModel = "missing"
	if w := post(bad, "same-origin"); w.Code != http.StatusBadRequest {
		t.Fatalf("bad model returned %d", w.Code)
	}
	saved := NewServer(cfg).settings.snapshot()
	effective := saved.config(cfg)
	if saved.SystemPrompt != settings.SystemPrompt || saved.mapModel("") != "fast" || effective.Timeout != 90*time.Second || !effective.KeepSessions || effective.ThinkingEffort != "high" {
		t.Fatalf("incorrect settings: %+v", saved)
	}
	if got := saved.mapModel("quickwork/smart"); got != "smart" {
		t.Fatalf("wrong alias: %q", got)
	}
	if saved.prompt("") != settings.SystemPrompt || saved.prompt("request instructions") != "request instructions" || (runtimeSettings{}).prompt("request only") != "request only" {
		t.Fatal("system prompt precedence is wrong")
	}
	if len(defaultSystemPrompt) > maxAgentInstructions {
		t.Fatalf("default Claude instructions are %d bytes, limit %d", len(defaultSystemPrompt), maxAgentInstructions)
	}
	if cfg.DefaultModel != "smart" {
		t.Fatal("initial config mutated")
	}
}

func TestEffectiveSystemPrompt(t *testing.T) {
	empty := runtimeSettings{}
	if got := empty.effectiveSystem(""); got != defaultSystemPrompt {
		t.Fatalf("empty prompt: %q", got)
	}
	if got := empty.effectiveSystem("Be terse."); got != "Be terse." {
		t.Fatalf("explicit prompt: %q", got)
	}
	already := runtimeSettings{SystemPrompt: defaultSystemPrompt}
	if got := already.effectiveSystem("Client note."); got != "Client note." {
		t.Fatalf("client prompt was prefixed: %q", got)
	}
	full := runtimeSettings{SystemPrompt: "Stay in Quick.", FullContext: true}
	if got := full.effectiveSystem(""); got != "Stay in Quick." {
		t.Fatalf("full context still overrode identity: %q", got)
	}
	if got := (runtimeSettings{FullContext: true}).effectiveSystem(""); got != "" {
		t.Fatalf("full context injected a prompt: %q", got)
	}
	if got := compactSystemPrompt(strings.Repeat("x", 2001)); got != harnessCompactPrompt {
		t.Fatalf("compact huge client prompt: %q", got)
	}
	if got := compactSystemPrompt(defaultSystemPrompt + strings.Repeat("x", 2000)); got != defaultSystemPrompt {
		t.Fatalf("compact huge Claude prompt: %q", got)
	}
	wrapped := withTurnIdentity("Who are you?")
	if !strings.HasPrefix(wrapped, turnIdentityPrefix) || !strings.Contains(wrapped, "Who are you?") || !strings.HasSuffix(wrapped, turnIdentitySuffix) || withTurnIdentity(wrapped) != wrapped {
		t.Fatalf("turn identity wrap: %q", wrapped)
	}
}

func TestThinkingEffortMapping(t *testing.T) {
	settings := runtimeSettings{ThinkingEffort: "medium"}
	for _, tc := range []struct {
		input []string
		want  string
	}{
		{nil, "medium"},
		{[]string{"", "xhigh"}, "high"},
		{[]string{"unknown", "extra-high"}, "high"},
		{[]string{"OFF", "high"}, "off"},
		{[]string{"minimal"}, "low"},
		{[]string{"none"}, "off"},
		{[]string{"unknown"}, "medium"},
	} {
		if got := settings.thinking(tc.input...); got != tc.want {
			t.Errorf("thinking(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestTrafficTimeToFirstToken(t *testing.T) {
	server := &Server{stats: NewStats("")}
	tap := &callTap{start: time.Now().Add(-100 * time.Millisecond), status: "ok"}
	_ = tap.observeText(StreamEvent{Type: "text"})
	if tap.ttftMS != nil {
		t.Fatal("empty delta recorded as a token")
	}
	_ = tap.observeText(StreamEvent{Type: "text", Text: "first"})
	first := tap.ttftMS
	_ = tap.observeText(StreamEvent{Type: "text", Text: "second"})
	if tap.ttftMS != first {
		t.Fatal("second delta changed TTFT")
	}
	server.finishCall(tap)
	rec := server.stats.Snapshot(1).Recent[0]
	if rec.TTFTMs == nil || *rec.TTFTMs < 100 || rec.DurationMs < *rec.TTFTMs {
		t.Fatalf("incorrect timing: %+v", rec)
	}
	server.finishCall(&callTap{start: time.Now(), status: "error"})
	if server.stats.Snapshot(1).Recent[0].TTFTMs != nil {
		t.Fatal("recorded TTFT for request with no output")
	}
}
