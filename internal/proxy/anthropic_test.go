package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestAnthropicMessages(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	type payload struct {
		Text        string           `json:"text"`
		System      string           `json:"system_prompt"`
		Custom      string           `json:"custom_prompt"`
		Thinking    string           `json:"thinking_effort"`
		Mode        string           `json:"agent_mode"`
		ChatAgent   *string          `json:"chat_agent_id"`
		Incognito   bool             `json:"incognito"`
		Attachments []map[string]any `json:"attachments"`
	}
	seen := make(chan payload, 4)
	uploads := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "upload-attachment"):
			body, _ := io.ReadAll(r.Body)
			uploads <- string(body)
			writeJSON(w, 200, map[string]any{"attachmentId": "att-1"})
			return
		case strings.Contains(r.URL.Path, "attachment-dlp-status"):
			writeJSON(w, 200, map[string]any{"dlpJobStatus": "COMPLETED", "policyAction": "ALLOW"})
			return
		case strings.HasSuffix(r.URL.Path, "qbs-event"):
			writeJSON(w, 200, map[string]any{"eventResponse": map[string]any{"invokeAgentResponse": map[string]any{"api_response": map[string]any{
				"status": 200, "body": map[string]any{"path": "/data/s3files/users/x/sessions/t/workspace/attached_files/conversation.txt"},
			}}}})
			return
		}
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var outer struct {
				Payload struct {
					Payload string `json:"payload"`
				} `json:"payload"`
			}
			if json.Unmarshal(data, &outer) != nil {
				return
			}
			raw, err := base64.StdEncoding.DecodeString(outer.Payload.Payload)
			var msg struct {
				Type    string  `json:"type"`
				Payload payload `json:"payload"`
			}
			if err != nil || json.Unmarshal(raw, &msg) != nil || msg.Type != "user_message" {
				continue
			}
			seen <- msg.Payload
			deltas := []string{"hello ST", "OP hidden"}
			harness := strings.Contains(msg.Payload.Custom, "Available harness tools:")
			if harness {
				deltas = []string{"I'll check.\n``", "`bash run\nharness lookup <<'HARNESS_JSON'\n", "{\"id\":42}\nHARNESS_JSON\n``"}
			}
			for _, delta := range deltas {
				_ = conn.WriteJSON(map[string]any{"type": "text_delta", "payload": map[string]string{"text": delta}})
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"turn_usage","payload":{"input_tokens":12,"output_tokens":5,"cache_read_tokens":3}}`))
			if harness {
				_ = conn.WriteJSON(map[string]any{"type": "message_complete", "payload": map[string]string{"text": strings.Join(deltas, "") + "`"}})
			} else {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"turn_completed"}`))
			}
			return
		}
	}))
	defer upstream.Close()
	dir := t.TempDir()
	cfg := &Config{AccountsFile: filepath.Join(dir, "accounts.json"), StatsFile: filepath.Join(dir, "stats.json"), TokensFile: filepath.Join(dir, "keys.json"), Models: []string{"smart"}, DefaultModel: "smart", Timeout: 3 * time.Second}
	s := NewServer(cfg)
	a := fixtureAccount("test", time.Now().Add(time.Hour).Unix())
	a.ChatAgentID = "configured-dashboard-agent"
	a.Session.WSHost = "ws" + strings.TrimPrefix(upstream.URL, "http")
	s.accounts.data.Accounts = []Account{a}
	settings := s.settings.snapshot()
	settings.SystemPrompt = "Service instructions."
	if err := s.settings.save(settings); err != nil {
		t.Fatal(err)
	}
	key, _, err := s.tokens.Create("test")
	if err != nil {
		t.Fatal(err)
	}
	post := func(path, body, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("x-api-key", token)
		r.Header.Set("anthropic-version", "2023-06-01")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	const input = `{"model":"smart","max_tokens":100,"system":[{"type":"text","text":"Client instructions."}],"messages":[{"role":"user","content":"Earlier question"},{"role":"system","clear_at":"next_user_message","content":"Expired instructions."},{"role":"assistant","content":"Earlier answer"},{"role":"user","content":"Hi"},{"role":"system","content":[{"type":"text","text":"Current instructions."}],"output_config":{"effort":"xhigh"}}],"output_config":{"effort":"low"},"stop_sequences":["STOP"]`
	if w := post("/v1/messages", input+"}", ""); w.Code != 401 || !strings.Contains(w.Body.String(), "authentication_error") {
		t.Fatalf("auth: %d %s", w.Code, w.Body)
	}
	w := post("/v1/messages", input+"}", key)
	var result struct {
		Content  []anthropicBlock `json:"content"`
		Stop     string           `json:"stop_reason"`
		Sequence string           `json:"stop_sequence"`
		Usage    map[string]int   `json:"usage"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Content) != 1 || result.Content[0].Text == nil || *result.Content[0].Text != "hello " || result.Stop != "stop_sequence" || result.Sequence != "STOP" || result.Usage["output_tokens"] != 5 {
		t.Fatalf("message: %d %s", w.Code, w.Body)
	}
	if p := <-seen; p.System != "Client instructions.\n\nCurrent instructions." || p.Custom != p.System || p.Thinking != "high" || p.ChatAgent == nil || *p.ChatAgent != "configured-dashboard-agent" || strings.Contains(p.Text, "Current instructions.") {
		t.Fatalf("upstream options: %+v", p)
	}
	w = post("/v1/messages", input+`,"stream":true}`, key)
	assertEvents(t, w, "message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop")
	if strings.Contains(w.Body.String(), "hidden") || !strings.Contains(w.Body.String(), `"stop_reason":"stop_sequence"`) {
		t.Fatalf("incorrect streaming stop: %s", w.Body)
	}
	largeSystem, _ := json.Marshal(strings.Repeat("These are long client instructions. ", 2500))
	w = post("/v1/messages", `{"model":"smart","max_tokens":120,"stream":true,"system":`+string(largeSystem)+`,"tools":[{"name":"lookup","input_schema":{"type":"object"}},{"name":"web_search","type":"web_search_20250305"}],"tool_choice":{"type":"tool","name":"lookup"},"messages":[{"role":"user","content":"Look it up"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"previous result"}]}]}`, key)
	assertEvents(t, w, "message_start", "content_block_start", "content_block_delta", "content_block_stop", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop")
	if !strings.Contains(w.Body.String(), `"type":"input_json_delta"`) || !strings.Contains(w.Body.String(), `"stop_reason":"tool_use"`) || strings.Contains(w.Body.String(), "HARNESS_JSON") || !strings.Contains(w.Body.String(), `"output_tokens":5`) {
		t.Fatalf("tool stream: %s", w.Body)
	}
	<-seen // prior SSE text request
	// Oversized harness context goes out as one user message with an
	// attachment, never as a multipart "reply OK" sequence.
	p := <-seen
	select {
	case extra := <-seen:
		t.Fatalf("client tool turn sent more than one user message: %d bytes", len(extra.Text))
	default:
	}
	attached := <-uploads
	validAgent := p.ChatAgent != nil && *p.ChatAgent == "configured-dashboard-agent"
	if p.Mode != "smart" || p.Thinking == "off" || strings.Contains(p.Custom, "web_search") || !validAgent || !p.Incognito || p.System != "" || len(p.Attachments) != 1 {
		t.Fatalf("unexpected client tool message: mode=%s thinking=%s attachments=%d bytes=%d", p.Mode, p.Thinking, len(p.Attachments), len(p.Text))
	}
	if strings.Contains(p.Text, "reply only OK") ||
		!strings.Contains(attached, strings.TrimSpace(strings.Repeat("These are long client instructions. ", 2500))) ||
		!strings.Contains(p.Text, "Tool result (call_id=toolu_1, name=lookup): previous result") {
		t.Fatal("client tool message lost instructions or tool results")
	}
	if w = post("/v1/messages/count_tokens", `{"model":"smart","messages":[{"role":"user","content":"Hello"}]}`, key); w.Code != 200 || w.Header().Get("X-Token-Count-Estimated") != "true" {
		t.Fatalf("count: %d %s", w.Code, w.Body)
	}
	if w = post("/v1/messages", `{"model":"smart","max_tokens":20,"messages":[{"role":"user","content":[{"type":"image"}]}]}`, key); w.Code != 400 || !strings.Contains(w.Body.String(), "missing source") {
		t.Fatalf("invalid image: %d %s", w.Code, w.Body)
	}
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	w = post("/v1/messages", `{"model":"smart","max_tokens":20,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"`+png+`"}},{"type":"text","text":"What is this?"}]}]}`, key)
	if w.Code != 200 {
		t.Fatalf("image message: %d %s", w.Code, w.Body)
	}
	if p := <-seen; len(p.Attachments) != 1 || p.Attachments[0]["mimeType"] != "image/png" || p.Attachments[0]["data"] != png || !strings.Contains(p.Text, "What is this?") {
		t.Fatalf("image attachments: %+v", p)
	}
	if stats := s.stats.Snapshot(10); stats.Requests != 4 || stats.Recent[0].TTFTMs == nil {
		t.Fatalf("traffic was not recorded: %+v", stats)
	}
}

func assertEvents(t *testing.T, w *httptest.ResponseRecorder, expected ...string) {
	t.Helper()
	var events []string
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if event, found := strings.CutPrefix(line, "event: "); found {
			events = append(events, event)
		}
	}
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") || strings.Join(events, ",") != strings.Join(expected, ",") {
		t.Fatalf("unexpected SSE: %d %s", w.Code, w.Body)
	}
}

func TestErrorText(t *testing.T) {
	if got := errorText(json.RawMessage(`{"error":{"message":"quota exceeded"}}`)); got != "quota exceeded" {
		t.Fatalf("nested: %s", got)
	}
	if got := errorText(json.RawMessage(`{"reason":"throttled"}`)); got != "throttled" {
		t.Fatalf("reason: %s", got)
	}
	if got := errorText(nil, "fallback"); got != "fallback" {
		t.Fatalf("fallback: %s", got)
	}
	if got := errorText(json.RawMessage(`{"status":"denied"}`)); got != `{"status":"denied"}` {
		t.Fatalf("raw: %s", got)
	}
}

func TestParseUsageAndEmptyReply(t *testing.T) {
	u := parseUsage(json.RawMessage(`{"inputTokens":9,"outputTokens":2,"cacheReadTokens":3}`))
	if u.PromptTokens != 9 || u.CompletionTokens != 2 || u.CacheReadTokens != 3 {
		t.Fatalf("camel usage: %+v", u)
	}
	u = parseUsage(json.RawMessage(`{"usage":{"input_tokens":4,"output_tokens":1}}`))
	if u.PromptTokens != 4 || u.CompletionTokens != 1 {
		t.Fatalf("nested usage: %+v", u)
	}
	msg := ChatResult{Usage: Usage{PromptTokens: 10, CompletionTokens: 7}, Events: []string{"thinking_delta", "message_complete"}}.emptyReplyErr()
	if !strings.Contains(msg, "7 output tokens") || !strings.Contains(msg, "thinking_delta") {
		t.Fatalf("empty reply: %s", msg)
	}
}

func TestMessageCompleteTextFallback(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var outer struct {
				Payload struct {
					Payload string `json:"payload"`
				} `json:"payload"`
			}
			if json.Unmarshal(data, &outer) != nil {
				return
			}
			raw, err := base64.StdEncoding.DecodeString(outer.Payload.Payload)
			var msg struct {
				Type string `json:"type"`
			}
			if err != nil || json.Unmarshal(raw, &msg) != nil || msg.Type != "user_message" {
				continue
			}
			_ = conn.WriteJSON(map[string]any{"type": "message_complete", "payload": map[string]any{"text": "from complete", "continuation_expected": false}})
			_ = conn.WriteJSON(map[string]any{"type": "turn_usage", "payload": map[string]any{"input_tokens": 11, "output_tokens": 3, "cache_read_tokens": 2}})
			return
		}
	}))
	defer upstream.Close()
	dir := t.TempDir()
	cfg := &Config{AccountsFile: filepath.Join(dir, "accounts.json"), StatsFile: filepath.Join(dir, "stats.json"), TokensFile: filepath.Join(dir, "keys.json"), Models: []string{"smart"}, DefaultModel: "smart", Timeout: 3 * time.Second}
	s := NewServer(cfg)
	a := fixtureAccount("test", time.Now().Add(time.Hour).Unix())
	a.Session.WSHost = "ws" + strings.TrimPrefix(upstream.URL, "http")
	s.accounts.data.Accounts = []Account{a}
	key, _, err := s.tokens.Create("test")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"smart","max_tokens":20,"messages":[{"role":"user","content":"Hi"}]}`))
	r.Header.Set("x-api-key", key)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "from complete") || !strings.Contains(w.Body.String(), `"output_tokens":3`) {
		t.Fatalf("complete fallback: %d %s", w.Code, w.Body)
	}
}

func TestDescribeErr(t *testing.T) {
	if got := describeErr(nil, "AWS Quick request failed"); got != "AWS Quick request failed" {
		t.Fatalf("nil: %s", got)
	}
	if got := describeErr(context.Canceled, "AWS Quick request failed"); got != "canceled" {
		t.Fatalf("canceled: %s", got)
	}
	if got := describeErr(context.DeadlineExceeded, "AWS Quick request failed"); got != "AWS Quick request failed: timed out" {
		t.Fatalf("timeout: %s", got)
	}
	if got := describeErr(errors.New("upstream error [429] quota"), "AWS Quick request failed"); got != "AWS Quick request failed: upstream error [429] quota" {
		t.Fatalf("wrap: %s", got)
	}
}

func TestAnthropicTrafficLogsUpstreamError(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var outer struct {
				Payload struct {
					Payload string `json:"payload"`
				} `json:"payload"`
			}
			if json.Unmarshal(data, &outer) != nil {
				return
			}
			raw, err := base64.StdEncoding.DecodeString(outer.Payload.Payload)
			var msg struct {
				Type string `json:"type"`
			}
			if err != nil || json.Unmarshal(raw, &msg) != nil || msg.Type != "user_message" {
				continue
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","payload":{"message":"quota exceeded for this session"}}`))
			return
		}
	}))
	defer upstream.Close()
	dir := t.TempDir()
	cfg := &Config{AccountsFile: filepath.Join(dir, "accounts.json"), StatsFile: filepath.Join(dir, "stats.json"), TokensFile: filepath.Join(dir, "keys.json"), Models: []string{"smart"}, DefaultModel: "smart", Timeout: 3 * time.Second}
	s := NewServer(cfg)
	a := fixtureAccount("test", time.Now().Add(time.Hour).Unix())
	a.Session.WSHost = "ws" + strings.TrimPrefix(upstream.URL, "http")
	s.accounts.data.Accounts = []Account{a}
	key, _, err := s.tokens.Create("test")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"smart","max_tokens":20,"messages":[{"role":"user","content":"Hi"}]}`))
	r.Header.Set("x-api-key", key)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 502 || !strings.Contains(w.Body.String(), "quota exceeded for this session") {
		t.Fatalf("api error: %d %s", w.Code, w.Body)
	}
	rec := s.stats.Snapshot(1).Recent[0]
	if rec.Status != "error" || !strings.Contains(rec.Error, "quota exceeded for this session") {
		t.Fatalf("traffic error: %+v", rec)
	}
}
