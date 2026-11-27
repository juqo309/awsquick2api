package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func decodeAIDPUserMessage(data []byte) (string, map[string]any, bool) {
	var outer struct {
		Payload struct {
			Payload string `json:"payload"`
		} `json:"payload"`
	}
	if json.Unmarshal(data, &outer) != nil || outer.Payload.Payload == "" {
		return "", nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(outer.Payload.Payload)
	if err != nil {
		return "", nil, false
	}
	var msg struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return "", nil, false
	}
	return msg.Type, msg.Payload, true
}

func TestAnthropicStreamDelayedStartReturnsHTTP502OnPreOutputError(t *testing.T) {
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
			typ, _, ok := decodeAIDPUserMessage(data)
			if !ok || typ != "user_message" {
				continue
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","payload":{"message":"quota exceeded for this session"}}`))
			return
		}
	}))
	defer upstream.Close()
	dir := t.TempDir()
	cfg := &Config{AccountsFile: filepath.Join(dir, "accounts.json"), StatsFile: filepath.Join(dir, "stats.json"), TokensFile: filepath.Join(dir, "keys.json"), Models: []string{"smart"}, DefaultModel: "smart", Timeout: 5 * time.Second}
	s := NewServer(cfg)
	a := fixtureAccount("test", time.Now().Add(time.Hour).Unix())
	a.Session.WSHost = "ws" + strings.TrimPrefix(upstream.URL, "http")
	s.accounts.data.Accounts = []Account{a}
	key, _, err := s.tokens.Create("test")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"smart","max_tokens":20,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`))
	r.Header.Set("x-api-key", key)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502 for pre-output stream error, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "quota exceeded for this session") {
		t.Fatalf("unexpected error body: %s", w.Body.String())
	}
}

func TestAnthropicStreamKeepalivePings(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	oldStart, oldPing := streamStartDelay, streamPingInterval
	streamStartDelay = 30 * time.Millisecond
	streamPingInterval = 25 * time.Millisecond
	t.Cleanup(func() {
		streamStartDelay = oldStart
		streamPingInterval = oldPing
	})

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
			typ, _, ok := decodeAIDPUserMessage(data)
			if !ok || typ != "user_message" {
				continue
			}
			time.Sleep(130 * time.Millisecond)
			_ = conn.WriteJSON(map[string]any{"type": "text_delta", "payload": map[string]string{"text": "hello"}})
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"turn_completed"}`))
			return
		}
	}))
	defer upstream.Close()
	dir := t.TempDir()
	cfg := &Config{AccountsFile: filepath.Join(dir, "accounts.json"), StatsFile: filepath.Join(dir, "stats.json"), TokensFile: filepath.Join(dir, "keys.json"), Models: []string{"smart"}, DefaultModel: "smart", Timeout: 5 * time.Second}
	s := NewServer(cfg)
	a := fixtureAccount("test", time.Now().Add(time.Hour).Unix())
	a.Session.WSHost = "ws" + strings.TrimPrefix(upstream.URL, "http")
	s.accounts.data.Accounts = []Account{a}
	key, _, err := s.tokens.Create("test")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"smart","max_tokens":20,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`))
	r.Header.Set("x-api-key", key)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, body)
	}
	if !strings.Contains(body, "event: ping\ndata: {\"type\":\"ping\"}\n\n") {
		t.Fatalf("expected SSE ping keepalive during slow TTFT, got:\n%s", body)
	}
	if !strings.Contains(body, `"text":"hello"`) || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("missing expected reply or message_stop:\n%s", body)
	}
}

func TestUpstreamReadCanceledReturnsContextCanceled(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	gotMessage := make(chan struct{}, 1)
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
			typ, _, ok := decodeAIDPUserMessage(data)
			if ok && typ == "user_message" {
				select {
				case gotMessage <- struct{}{}:
				default:
				}
			}
		}
	}))
	defer upstream.Close()
	cfg := &Config{Models: []string{"smart"}, DefaultModel: "smart", Timeout: 5 * time.Second}
	sess := &Session{JWT: "tok", Provider: "social", WSHost: "ws" + strings.TrimPrefix(upstream.URL, "http"), OutpostID: "op-1"}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-gotMessage
		time.Sleep(25 * time.Millisecond)
		cancel()
	}()
	_, err := NewUpstream(cfg).Chat(ctx, sess, ChatInput{Prompt: "Hi", Model: "smart"}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

func TestUpstreamContentFilterRetryInFreshSession(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	var attempts atomic.Int32
	var mu sync.Mutex
	var threads []string
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
			typ, payload, ok := decodeAIDPUserMessage(data)
			if !ok || typ != "user_message" {
				continue
			}
			tid, _ := payload["thread_id"].(string)
			mu.Lock()
			threads = append(threads, tid)
			mu.Unlock()
			n := attempts.Add(1)
			if n == 1 {
				// Simulate internal Quick tool call followed by content_filtered before any text output.
				_ = conn.WriteJSON(map[string]any{"type": "tool_start", "payload": map[string]any{"tool_id": "t1", "tool_name": "file_read"}})
				_ = conn.WriteJSON(map[string]any{"type": "tool_result", "payload": map[string]any{"tool_id": "t1", "tool_name": "file_read", "result": "ok"}})
				_ = conn.WriteJSON(map[string]any{"type": "turn_usage", "payload": map[string]any{"input_tokens": 6, "output_tokens": 300}})
				_ = conn.WriteJSON(map[string]any{"type": "error", "payload": map[string]any{"error": "The model's content filter stopped this reply. Rephrase, or start a new chat.", "error_code": "content_filtered"}})
				return
			}
			_ = conn.WriteJSON(map[string]any{"type": "text_delta", "payload": map[string]string{"text": "recovered reply"}})
			_ = conn.WriteJSON(map[string]any{"type": "turn_usage", "payload": map[string]any{"input_tokens": 4, "output_tokens": 20}})
			_ = conn.WriteJSON(map[string]any{"type": "message_complete", "payload": map[string]string{"text": "recovered reply"}})
			return
		}
	}))
	defer upstream.Close()
	cfg := &Config{Models: []string{"smart"}, DefaultModel: "smart", Timeout: 5 * time.Second}
	sess := &Session{JWT: "tok", Provider: "social", WSHost: "ws" + strings.TrimPrefix(upstream.URL, "http"), OutpostID: "op-1"}
	res, err := NewUpstream(cfg).Chat(context.Background(), sess, ChatInput{
		Prompt:         "User: Hi",
		Model:          "smart",
		ToolPrompt:     "tools",
		HarnessCatalog: `[{"name":"Read"}]`,
		ClientTurn:     true,
	}, func(ev StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("expected retry to recover from pre-output content filter, got err: %v", err)
	}
	if res.Text != "recovered reply" {
		t.Fatalf("unexpected text: %q", res.Text)
	}
	if attempts.Load() != 2 || len(threads) != 2 || threads[0] == "" || threads[0] == threads[1] {
		t.Fatalf("expected 2 attempts on distinct threads, got attempts=%d threads=%v", attempts.Load(), threads)
	}
	if res.Usage.CompletionTokens != 320 {
		t.Fatalf("expected aggregated completion tokens 320, got %+v", res.Usage)
	}
}

func TestUpstreamContentFilterNoRetryAfterTextEmitted(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	var attempts atomic.Int32
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
			typ, _, ok := decodeAIDPUserMessage(data)
			if !ok || typ != "user_message" {
				continue
			}
			attempts.Add(1)
			_ = conn.WriteJSON(map[string]any{"type": "text_delta", "payload": map[string]string{"text": "partial"}})
			_ = conn.WriteJSON(map[string]any{"type": "error", "payload": map[string]any{"error": "The model's content filter stopped this reply.", "error_code": "content_filtered"}})
			return
		}
	}))
	defer upstream.Close()
	cfg := &Config{Models: []string{"smart"}, DefaultModel: "smart", Timeout: 5 * time.Second}
	sess := &Session{JWT: "tok", Provider: "social", WSHost: "ws" + strings.TrimPrefix(upstream.URL, "http"), OutpostID: "op-1"}
	_, err := NewUpstream(cfg).Chat(context.Background(), sess, ChatInput{
		Prompt:         "User: Hi",
		Model:          "smart",
		ToolPrompt:     "tools",
		HarnessCatalog: `[{"name":"Read"}]`,
		ClientTurn:     true,
	}, func(ev StreamEvent) error { return nil })
	if err == nil || !isContentFilter(err.Error()) {
		t.Fatalf("expected content filter error without retry after text emitted, got: %v", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("expected 1 attempt after text emitted, got %d", attempts.Load())
	}
}

type cancelOnSubstringWriter struct {
	*httptest.ResponseRecorder
	substr string
	cancel context.CancelFunc
	called bool
}

func (w *cancelOnSubstringWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	if !w.called && strings.Contains(w.ResponseRecorder.Body.String(), w.substr) {
		w.called = true
		w.cancel()
	}
	return n, err
}

func TestStreamClientCancelAfterOutputRecordsOK(t *testing.T) {
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
			typ, _, ok := decodeAIDPUserMessage(data)
			if !ok || typ != "user_message" {
				continue
			}
			_ = conn.WriteJSON(map[string]any{"type": "text_delta", "payload": map[string]string{"text": "hello"}})
			// Wait for client disconnect instead of completing.
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}
	}))
	defer upstream.Close()

	cases := []struct {
		name   string
		path   string
		body   string
		substr string
	}{
		{
			name:   "anthropic",
			path:   "/v1/messages",
			body:   `{"model":"smart","max_tokens":20,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`,
			substr: `"text":"hello"`,
		},
		{
			name:   "openai",
			path:   "/v1/chat/completions",
			body:   `{"model":"smart","stream":true,"messages":[{"role":"user","content":"Hi"}]}`,
			substr: `"content":"hello"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := &Config{AccountsFile: filepath.Join(dir, "accounts.json"), StatsFile: filepath.Join(dir, "stats.json"), TokensFile: filepath.Join(dir, "keys.json"), Models: []string{"smart"}, DefaultModel: "smart", Timeout: 5 * time.Second}
			s := NewServer(cfg)
			a := fixtureAccount("test", time.Now().Add(time.Hour).Unix())
			a.Session.WSHost = "ws" + strings.TrimPrefix(upstream.URL, "http")
			s.accounts.data.Accounts = []Account{a}
			key, _, err := s.tokens.Create("test")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)).WithContext(ctx)
			r.Header.Set("x-api-key", key)
			w := &cancelOnSubstringWriter{ResponseRecorder: httptest.NewRecorder(), substr: tc.substr, cancel: cancel}
			s.Handler().ServeHTTP(w, r)
			if !w.called {
				t.Fatalf("expected cancel to trigger on %q, body:\n%s", tc.substr, w.Body.String())
			}
			if strings.Contains(w.Body.String(), "event: error") || strings.Contains(w.Body.String(), `"upstream_error"`) {
				t.Fatalf("unexpected error event after client cancel:\n%s", w.Body.String())
			}
			rec := s.stats.Snapshot(1).Recent[0]
			if rec.Status != "ok" || rec.Error != "" || rec.TTFTMs == nil {
				t.Fatalf("expected ok record after streamed output cancel, got %+v", rec)
			}
		})
	}
}
