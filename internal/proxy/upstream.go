package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	xproxy "golang.org/x/net/proxy"
)

const (
	defaultWSPath        = "/streaming/quickwork-chat-bidirectional-stream"
	defaultGatewayWSPath = "/ws/service/SpaceNeedleConversationService/operation/ChatBidirectionalStream"
)

type Upstream struct {
	cfg *Config
}

func NewUpstream(cfg *Config) *Upstream {
	return &Upstream{cfg: cfg}
}

// ChatInput is one upstream turn.
type ChatInput struct {
	Prompt         string
	Model          string
	ThreadID       string
	Title          string
	SystemPrompt   string
	ToolPrompt     string
	HarnessCatalog string
	Proxy          string
	KeepThread     bool
	AccountID      string
	ChatAgentID    string
	Media          []Media
	ClientTurn     bool
}

// ToolCallState is a tool invocation reported by the QuickWork agent.
type ToolCallState struct {
	ID        string
	Name      string
	Arguments string
	Result    string
	Status    string
}

// Usage aggregates turn_usage events across a multi-step tool turn.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CacheReadTokens  int
	CacheWriteTokens int
}

func (u *Usage) add(v Usage) {
	u.PromptTokens += v.PromptTokens
	u.CompletionTokens += v.CompletionTokens
	u.TotalTokens += v.TotalTokens
	u.CacheReadTokens += v.CacheReadTokens
	u.CacheWriteTokens += v.CacheWriteTokens
}

func (u Usage) empty() bool {
	return u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0 && u.CacheReadTokens == 0 && u.CacheWriteTokens == 0
}

type usageWire struct {
	InputTokens       int `json:"input_tokens"`
	InputTokensCamel  int `json:"inputTokens"`
	OutputTokens      int `json:"output_tokens"`
	OutputTokensCamel int `json:"outputTokens"`
	TotalTokens       int `json:"total_tokens"`
	TotalTokensCamel  int `json:"totalTokens"`
	CacheRead         int `json:"cache_read_tokens"`
	CacheReadCamel    int `json:"cacheReadTokens"`
	CacheWrite        int `json:"cache_write_tokens"`
	CacheWriteCamel   int `json:"cacheWriteTokens"`
}

func (p usageWire) asUsage() Usage {
	in, out, total := p.InputTokens, p.OutputTokens, p.TotalTokens
	if in == 0 {
		in = p.InputTokensCamel
	}
	if out == 0 {
		out = p.OutputTokensCamel
	}
	if total == 0 {
		total = p.TotalTokensCamel
	}
	read, write := p.CacheRead, p.CacheWrite
	if read == 0 {
		read = p.CacheReadCamel
	}
	if write == 0 {
		write = p.CacheWriteCamel
	}
	return Usage{PromptTokens: in, CompletionTokens: out, TotalTokens: total, CacheReadTokens: read, CacheWriteTokens: write}
}

func parseUsage(raw json.RawMessage) Usage {
	var p usageWire
	_ = json.Unmarshal(raw, &p)
	if u := p.asUsage(); !u.empty() {
		return u
	}
	var wrap struct {
		Usage usageWire `json:"usage"`
	}
	_ = json.Unmarshal(raw, &wrap)
	return wrap.Usage.asUsage()
}

// StreamEvent is a text delta or a completed tool call.
type StreamEvent struct {
	Type     string
	Text     string
	ToolCall *ToolCallState
}

type EventFunc func(StreamEvent) error

type ChatResult struct {
	Text      string
	ToolCalls []ToolCallState
	Usage     Usage
	Events    []string
}

func (r ChatResult) emptyReplyErr() string {
	if strings.TrimSpace(r.Text) != "" || len(r.ToolCalls) > 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("AWS Quick returned no text or tool calls")
	switch {
	case r.Usage.CompletionTokens > 0:
		fmt.Fprintf(&b, " (upstream billed %d output tokens that were not in text_delta/message_complete.text)", r.Usage.CompletionTokens)
	case r.Usage.PromptTokens > 0 || r.Usage.TotalTokens > 0:
		fmt.Fprintf(&b, " (upstream billed %d input tokens, 0 output)", r.Usage.PromptTokens)
	}
	if len(r.Events) > 0 {
		b.WriteString("; events: ")
		b.WriteString(strings.Join(r.Events, ", "))
	}
	return b.String()
}

// Chat bridges client tool requests through Quick's text transport.
func (u *Upstream) Chat(ctx context.Context, sess *Session, in ChatInput, onEvent EventFunc) (ChatResult, error) {
	if in.HarnessCatalog != "" || in.ToolPrompt == "" || in.ThreadID != "" {
		return u.chatWithFilterRetry(ctx, sess, in, onEvent)
	}
	res, denied, err := u.chatWithToolCheck(ctx, sess, in, onEvent)
	if err != nil || !denied {
		return res, err
	}
	// Quick sometimes mistakes the client adapter's output format for a
	// request to use Quick's built-in workspace. Try one fresh turn.
	log.Printf("upstream did not recognize declared client tools; retrying model=%s", in.Model)
	in.ToolPrompt += "\n\nThe declared tools are executable through the client API. For a requested client action, issue the structured tool call and wait for its result."
	retry, denied, err := u.chatWithToolCheck(ctx, sess, in, onEvent)
	retry.Usage.add(res.Usage)
	if err == nil && denied {
		err = fmt.Errorf("upstream did not recognize the declared client tools after retrying")
	}
	return retry, err
}

func (u *Upstream) chatWithToolCheck(ctx context.Context, sess *Session, in ChatInput, onEvent EventFunc) (ChatResult, bool, error) {
	const probeBytes = 2048
	var initial strings.Builder
	var pending []StreamEvent
	var released bool
	emit := func(ev StreamEvent) error {
		if onEvent != nil {
			return onEvent(ev)
		}
		return nil
	}
	flush := func() error {
		released = true
		for _, ev := range pending {
			if err := emit(ev); err != nil {
				return err
			}
		}
		pending = nil
		return nil
	}
	res, err := u.chatWithFilterRetry(ctx, sess, in, func(ev StreamEvent) error {
		if released {
			return emit(ev)
		}
		pending = append(pending, ev)
		if ev.ToolCall != nil {
			return flush()
		}
		if ev.Type == "text" && initial.Len() < probeBytes {
			initial.WriteString(ev.Text[:min(len(ev.Text), probeBytes-initial.Len())])
		}
		if initial.Len() >= probeBytes && !deniesClientTools(initial.String()) {
			return flush()
		}
		return nil
	})
	if err != nil || released {
		return res, false, err
	}
	if len(res.ToolCalls) == 0 && deniesClientTools(res.Text[:min(len(res.Text), probeBytes)]) && extractToolJSON(res.Text) == "" {
		return res, true, nil
	}
	return res, false, flush()
}

// Match capability denials about the adapter, never generic refusals.
func deniesClientTools(text string) bool {
	text = strings.ToLower(strings.ReplaceAll(text, "’", "'"))
	if strings.Contains(text, "i can't reach your computer's files") ||
		strings.Contains(text, "i can't reach any files on your computer") {
		return true
	}
	adapter := strings.Contains(text, "tool calls") || strings.Contains(text, "tool_calls") || strings.Contains(text, "coding assistant")
	return adapter && (strings.Contains(text, "can't run anything from here") ||
		strings.Contains(text, "outside the tools i actually have") ||
		strings.Contains(text, "can't reach your machine from this chat") ||
		strings.Contains(text, "no devices are connected to this chat") ||
		strings.Contains(text, "aren't connected to me") ||
		strings.Contains(text, "pretending to call them"))
}

func (u *Upstream) chatWithFilterRetry(ctx context.Context, sess *Session, in ChatInput, onEvent EventFunc) (ChatResult, error) {
	const maxAttempts = 3
	var totalUsage Usage
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		var emitted bool
		wrapped := onEvent
		if onEvent != nil {
			wrapped = func(ev StreamEvent) error {
				if (ev.Type == "text" && ev.Text != "") || (in.HarnessCatalog == "" && ev.ToolCall != nil) {
					emitted = true
				}
				return onEvent(ev)
			}
		}
		res, err := u.chat(ctx, sess, in, wrapped)
		totalUsage.add(res.Usage)
		res.Usage = totalUsage
		if err == nil || emitted || ctx.Err() != nil || in.ThreadID != "" || attempt == maxAttempts || !isContentFilter(err.Error()) {
			return res, err
		}
		log.Printf("upstream content filter before output; retrying in new session model=%s attempt=%d/%d: %v", in.Model, attempt, maxAttempts, err)
	}
	return ChatResult{Usage: totalUsage}, nil
}

// chat sends a single turn through the QuickWork WebSocket.
func (u *Upstream) chat(ctx context.Context, sess *Session, in ChatInput, onEvent EventFunc) (ChatResult, error) {
	if strings.TrimSpace(in.Prompt) == "" && len(in.Media) == 0 {
		return ChatResult{}, fmt.Errorf("prompt is empty")
	}
	if strings.TrimSpace(in.Prompt) == "" {
		in.Prompt = "See attached image."
	}
	threadID := strings.TrimSpace(in.ThreadID)
	if threadID == "" {
		threadID = uuid()
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = titleFor(in.Prompt)
	}
	messageID := uuid()
	traceID := uuid()

	wsURL, headers, gateway, err := u.dialInfo(sess)
	if err != nil {
		return ChatResult{}, err
	}

	dialer := u.wsDialer(in.Proxy)
	conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		detail := ""
		if resp != nil {
			detail = fmt.Sprintf(" HTTP %d", resp.StatusCode)
		}
		return ChatResult{}, fmt.Errorf("upstream WebSocket dial failed%s: %w", detail, err)
	}
	defer conn.Close()

	writeMu := &sync.Mutex{}
	encode := func(v any) ([]byte, error) {
		body, err := marshalWireJSON(v)
		if err != nil {
			return nil, err
		}
		if gateway {
			frame := gatewayEncode(string(body))
			if frame == nil {
				return nil, fmt.Errorf("encode gateway frame")
			}
			return frame, nil
		}
		// AIDP (enterprise and social) uses the block-style binary envelope:
		// {"eventType":"rawInputEvent","payload":{"payload":base64(json)}}.
		return aidpEncode(string(body)), nil
	}
	messageType := websocket.BinaryMessage
	send := func(v any) error {
		frame, err := encode(v)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(messageType, frame)
	}

	// Keep the socket alive while the agent is thinking. Quick closes idle
	// streams with "Client sent no frames for >95 seconds" unless it receives
	// application-layer frames (WebSocket control pings alone do not reset it).
	stopPing := make(chan struct{})
	defer close(stopPing)
	sendPong := func() {
		_ = send(map[string]any{"type": "pong", "payload": map[string]any{}})
		writeMu.Lock()
		_ = conn.WriteControl(websocket.PongMessage, []byte("pong"), time.Now().Add(3*time.Second))
		writeMu.Unlock()
	}
	go func() {
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = send(map[string]any{"type": "ping", "payload": map[string]any{}})
				writeMu.Lock()
				_ = conn.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(5*time.Second))
				writeMu.Unlock()
			}
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopPing:
		}
	}()

	// Handshake. The AIDP and Gateway paths use different session_connect shapes.
	if gateway {
		_ = send(map[string]any{"type": "session_connect", "session_id": sess.OutpostID})
	} else {
		_ = send(map[string]any{
			"type": "session_connect",
			"payload": map[string]any{
				"actor_id":               sess.ActorARN,
				"surfaceType":            sess.Surface,
				"outpostId":              sess.OutpostID,
				"appVersion":             u.cfg.AppVersion,
				"external_agent_sign_in": 1,
				"recents_promotion":      0,
			},
		})
	}
	_ = send(map[string]any{"type": "federate_status", "payload": map[string]any{}})
	_ = send(map[string]any{
		"type":    "session_create",
		"payload": map[string]any{"session_id": threadID, "title": title},
	})

	in.Prompt = applyTurnIdentity(in.Prompt, u.cfg.FullContext, in.ClientTurn || in.ToolPrompt != "")
	sys := strings.TrimSpace(in.SystemPrompt)
	if sys == "" && !u.cfg.FullContext {
		sys = defaultSystemPrompt
	}
	strict := in.HarnessCatalog != ""
	var harness harnessContext
	if strict {
		harness = harnessContext{instructions: sys, schemas: in.HarnessCatalog}
		sys = in.ToolPrompt
	}
	attachmentInstructions := sys
	if !strict {
		attachmentInstructions = joinSystemAndTools(sys, in.ToolPrompt)
	}
	attachments, note := prepareAttachments(ctx, sess, in.Proxy, threadID, in.Prompt, attachmentInstructions, in.Media)
	if note != "" {
		in.Prompt += note
	}
	harness.conversation = in.Prompt

	payload := map[string]any{
		"text":               in.Prompt,
		"send_origin":        "programmatic",
		"had_physical_input": false,
		"surfaceType":        sess.Surface,
		"outpostId":          sess.OutpostID,
		"appVersion":         u.cfg.AppVersion,
		"traceId":            traceID,
		"message_id":         messageID,
		"thread_id":          threadID,
		"agent_mode":         in.Model,
		"user_timezone":      u.cfg.Timezone,
		"user_locale":        u.cfg.Locale,
		"auto_routing":       false,
		"web_search_enabled": false,
		"pre_approved_tools": []any{},
		"preload_skills":     []any{},
		"panel_edits":        []any{},
		"incognito":          !u.cfg.FullContext || in.ToolPrompt != "",
	}
	if !u.cfg.FullContext && strings.TrimSpace(in.ChatAgentID) != "" {
		payload["chat_agent_id"] = strings.TrimSpace(in.ChatAgentID)
	} else {
		payload["chat_agent_id"] = nil
	}
	if strict {
		payload["custom_prompt"] = sys
	} else if sys != "" {
		payload["system_prompt"] = sys
		payload["custom_prompt"] = sys
	} else {
		payload["custom_prompt"] = nil
	}
	if u.cfg.ThinkingEffort != "" {
		payload["thinking_effort"] = u.cfg.ThinkingEffort
	}
	if len(attachments) > 0 {
		payload["attachments"] = attachments
	}
	var result ChatResult
	if strict {
		upload := func(name, text string) (Attachment, error) {
			return uploadRaw(ctx, sess, in.Proxy, threadID, name, "text/plain", "file", []byte(text))
		}
		if err := harnessMessage(payload, harness, harnessAttachmentName(messageID), upload); err != nil {
			return result, err
		}
		if err := send(map[string]any{"type": "user_message", "payload": payload}); err != nil {
			return result, fmt.Errorf("send harness message: %w", err)
		}
		result, err = u.readEvents(ctx, conn, gateway, true, sendPong, onEvent)
		if err != nil {
			return result, err
		}
	} else {
		if err := packUserMessage(ctx, sess, in.Proxy, threadID, messageID, in.Prompt, payload, in.ToolPrompt); err != nil {
			return result, err
		}
		if err := send(map[string]any{"type": "user_message", "payload": payload}); err != nil {
			return result, fmt.Errorf("send user_message: %w", err)
		}
		result, err = u.readEvents(ctx, conn, gateway, false, sendPong, onEvent)
		if err != nil {
			return result, err
		}
	}

	if !u.cfg.KeepSessions && !in.KeepThread {
		_ = send(map[string]any{"type": "session_delete", "payload": map[string]any{"session_id": threadID}})
	}
	return result, nil
}

func (u *Upstream) wsDialer(proxyURL string) *websocket.Dialer {
	d := &websocket.Dialer{HandshakeTimeout: 20 * time.Second, Proxy: http.ProxyFromEnvironment}
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return d
	}
	pu, err := url.Parse(proxyURL)
	if err != nil {
		return d
	}
	scheme := strings.ToLower(pu.Scheme)
	if scheme == "socks5" || scheme == "socks5h" {
		var auth *xproxy.Auth
		if pu.User != nil {
			pass, _ := pu.User.Password()
			auth = &xproxy.Auth{User: pu.User.Username(), Password: pass}
		}
		sd, err := xproxy.SOCKS5("tcp", pu.Host, auth, xproxy.Direct)
		if err != nil {
			return d
		}
		d.Proxy = nil
		if cd, ok := sd.(xproxy.ContextDialer); ok {
			d.NetDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return cd.DialContext(ctx, network, addr)
			}
		} else {
			d.NetDial = sd.Dial
		}
		return d
	}
	d.Proxy = http.ProxyURL(pu)
	return d
}

func (u *Upstream) dialInfo(sess *Session) (wsURL string, headers http.Header, gateway bool, err error) {
	jwt := sess.jwt()
	if jwt == "" {
		return "", nil, false, fmt.Errorf("missing jwt")
	}
	gateway = isGatewayProvider(sess.Provider)

	host := strings.TrimSpace(sess.WSHost)
	path := strings.TrimSpace(sess.WSPath)
	if path == "" {
		if gateway {
			path = defaultGatewayWSPath
		} else {
			path = defaultWSPath
		}
	}
	switch {
	case strings.HasPrefix(host, "wss://") || strings.HasPrefix(host, "ws://"):
		host = strings.TrimRight(host, "/")
	case strings.HasPrefix(host, "https://"):
		host = "wss://" + strings.TrimPrefix(host, "https://")
	default:
		host = "wss://" + strings.TrimRight(host, "/")
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		wsURL = path
	} else {
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		wsURL = host + path
	}

	parsed, err := url.Parse(wsURL)
	if err != nil {
		return "", nil, false, fmt.Errorf("invalid ws_url: %w", err)
	}
	if !gateway {
		q := parsed.Query()
		if sess.Surface != "" {
			q.Set("surfaceType", sess.Surface)
		}
		if sess.OutpostID != "" {
			q.Set("outpostId", sess.OutpostID)
		}
		if sess.WorkingSessionID != "" {
			q.Set("workingSessionId", sess.WorkingSessionID)
		}
		// Social AIDP transports accept the bearer in the query; enterprise
		// relies on the Authorization header injected below.
		if strings.EqualFold(sess.Provider, "social") {
			q.Set("token", jwt)
		}
		parsed.RawQuery = q.Encode()
	}

	headers = http.Header{}
	headers.Set("Authorization", "Bearer "+jwt)
	headers.Set("Origin", "file://")
	headers.Set("X-Surface-Type", sess.Surface)
	headers.Set("X-Outpost-Id", sess.OutpostID)
	headers.Set("X-Agent-Service-Type", "UNIFIED_QUICK")
	if sess.WorkingSessionID != "" {
		headers.Set("X-Working-Session-Id", sess.WorkingSessionID)
	}
	return parsed.String(), headers, gateway, nil
}

func (u *Upstream) readEvents(ctx context.Context, conn *websocket.Conn, gateway, strict bool, sendPong func(), onEvent EventFunc) (ChatResult, error) {
	var result ChatResult
	var text, thinking strings.Builder
	sawMessageComplete := false
	var completeDeadline time.Time
	var completedText string

	toolCalls := make(map[string]*ToolCallState)
	toolOrder := make([]string, 0, 2)
	toolSent := make(map[string]bool)
	partialArgs := make(map[string]string)

	getTool := func(id, name string) *ToolCallState {
		id = strings.TrimSpace(id)
		if id == "" {
			id = "call_" + strings.ReplaceAll(uuid(), "-", "")
		}
		tc := toolCalls[id]
		if tc == nil {
			tc = &ToolCallState{ID: id}
			toolCalls[id] = tc
			toolOrder = append(toolOrder, id)
		}
		if n := strings.TrimSpace(name); n != "" {
			tc.Name = n
		}
		return tc
	}
	emitTool := func(tc *ToolCallState) error {
		if tc == nil || onEvent == nil || toolSent[tc.ID] {
			return nil
		}
		toolSent[tc.ID] = true
		copy := *tc
		return onEvent(StreamEvent{Type: "tool_call", ToolCall: &copy})
	}
	emitToolUpdate := func(tc *ToolCallState) error {
		if tc == nil || onEvent == nil {
			return nil
		}
		copy := *tc
		return onEvent(StreamEvent{Type: "tool_call_update", ToolCall: &copy})
	}
	emitOrUpdate := func(tc *ToolCallState) error {
		if toolSent[tc.ID] {
			return emitToolUpdate(tc)
		}
		return emitTool(tc)
	}
	note := func(kind string) {
		if kind == "" {
			return
		}
		result.Events = append(result.Events, kind)
		if len(result.Events) > 16 {
			result.Events = result.Events[len(result.Events)-16:]
		}
	}
	emitText := func(s string) error {
		if s == "" {
			return nil
		}
		text.WriteString(s)
		if onEvent == nil {
			return nil
		}
		return onEvent(StreamEvent{Type: "text", Text: s})
	}
	finish := func() (ChatResult, error) {
		result.Text = text.String()
		if !strict && strings.TrimSpace(result.Text) == "" && thinking.Len() > 0 {
			result.Text = thinking.String()
		}
		result.ToolCalls = result.ToolCalls[:0]
		for _, id := range toolOrder {
			tc := toolCalls[id]
			if tc == nil {
				continue
			}
			if strings.TrimSpace(tc.Name) == "" {
				tc.Name = "unknown"
			}
			if strings.TrimSpace(tc.Arguments) == "" {
				if p := strings.TrimSpace(partialArgs[id]); p != "" && json.Valid([]byte(p)) {
					tc.Arguments = p
				} else {
					tc.Arguments = "{}"
				}
			}
			result.ToolCalls = append(result.ToolCalls, *tc)
			if err := emitTool(tc); err != nil {
				return result, err
			}
		}
		return result, nil
	}

	terminal := map[string]bool{
		"turn_completed": true,
		"turn_aborted":   true,
		"turn_ended":     true,
		"stream_ended":   true,
		"chat_complete":  true,
	}
	for {
		select {
		case <-ctx.Done():
			res, _ := finish()
			return res, ctx.Err()
		default:
		}
		readWait := 2 * time.Minute
		if !completeDeadline.IsZero() {
			if remaining := time.Until(completeDeadline); remaining > 0 {
				readWait = remaining
			} else {
				return finish()
			}
		}
		_ = conn.SetReadDeadline(time.Now().Add(readWait))
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				res, _ := finish()
				return res, ctx.Err()
			}
			res, emitErr := finish()
			if emitErr != nil {
				return res, emitErr
			}
			if !strict && (text.Len() > 0 || len(toolOrder) > 0) {
				return res, nil
			}
			return res, fmt.Errorf("upstream WebSocket read: %w", err)
		}

		var jsonStr string
		if msgType == websocket.TextMessage {
			jsonStr = string(data)
		} else {
			if gateway {
				jsonStr, err = gatewayDecode(data)
			} else {
				jsonStr, err = aidpDecode(data)
			}
			if err != nil {
				log.Printf("upstream frame decode: %v", err)
				if strings.Contains(err.Error(), "upstream error") {
					res, _ := finish()
					return res, err
				}
				continue
			}
		}

		var env struct {
			Type      string          `json:"type"`
			Payload   json.RawMessage `json:"payload"`
			MessageID string          `json:"message_id"`
			Error     json.RawMessage `json:"error"`
			ErrorCode string          `json:"error_code"`
			Message   string          `json:"message"`
			Reason    string          `json:"reason"`
			Code      string          `json:"code"`
		}
		if err := json.Unmarshal([]byte(jsonStr), &env); err != nil {
			continue
		}
		note(env.Type)
		switch env.Type {
		case "text_delta":
			delta := deltaText(env.Payload)
			if delta == "" {
				continue
			}
			if err := emitText(delta); err != nil {
				res, _ := finish()
				return res, err
			}
		case "thinking_delta":
			if d := deltaText(env.Payload); d != "" {
				thinking.WriteString(d)
			}
		case "text_complete":
			if strict {
				completedText = completeText(env.Payload)
				result.Usage.add(parseUsage(env.Payload))
				if text.Len() == 0 && completedText != "" {
					if err := emitText(completedText); err != nil {
						return result, err
					}
				}
				continue
			}
			if text.Len() == 0 {
				if err := emitText(completeText(env.Payload)); err != nil {
					res, _ := finish()
					return res, err
				}
			}
			result.Usage.add(parseUsage(env.Payload))
			sawMessageComplete = true
		case "turn_usage", "usage":
			result.Usage.add(parseUsage(env.Payload))
			if !completeDeadline.IsZero() && sawMessageComplete {
				return finish()
			}
		case "tool_start":
			var p struct {
				ToolName   string          `json:"tool_name"`
				ToolID     string          `json:"tool_id"`
				ToolCallID string          `json:"tool_call_id"`
				Arguments  json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			id := p.ToolID
			if id == "" {
				id = p.ToolCallID
			}
			tc := getTool(id, p.ToolName)
			if args := argumentString(p.Arguments); args != "{}" {
				tc.Arguments = args
			}
		case "tool_arguments", "tool_arguments_edited":
			var p struct {
				ToolID     string          `json:"tool_id"`
				ToolCallID string          `json:"tool_call_id"`
				ToolName   string          `json:"tool_name"`
				Arguments  json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			id := p.ToolID
			if id == "" {
				id = p.ToolCallID
			}
			tc := getTool(id, p.ToolName)
			if len(p.Arguments) > 0 {
				tc.Arguments = argumentString(p.Arguments)
			}
			if err := emitOrUpdate(tc); err != nil {
				res, _ := finish()
				return res, err
			}
		case "tool_progress":
			var p struct {
				ToolID     string `json:"tool_id"`
				ToolCallID string `json:"tool_call_id"`
				InputDelta string `json:"input_delta"`
				Progress   struct {
					InputDelta string `json:"input_delta"`
				} `json:"progress"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			id := p.ToolID
			if id == "" {
				id = p.ToolCallID
			}
			delta := p.InputDelta
			if delta == "" {
				delta = p.Progress.InputDelta
			}
			if id != "" && delta != "" {
				partialArgs[id] += delta
			}
		case "tool_result":
			var p struct {
				ToolID     string          `json:"tool_id"`
				ToolCallID string          `json:"tool_call_id"`
				ToolName   string          `json:"tool_name"`
				Arguments  json.RawMessage `json:"arguments"`
				Result     json.RawMessage `json:"result"`
				Error      string          `json:"error"`
				Status     string          `json:"status"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			id := p.ToolID
			if id == "" {
				id = p.ToolCallID
			}
			tc := getTool(id, p.ToolName)
			if len(p.Arguments) > 0 {
				tc.Arguments = argumentString(p.Arguments)
			}
			tc.Result = rawText(p.Result)
			if p.Error != "" {
				tc.Result = p.Error
				tc.Status = "error"
			} else {
				tc.Status = p.Status
			}
			if err := emitTool(tc); err != nil {
				res, _ := finish()
				return res, err
			}
		case "tool_error":
			var p struct {
				ToolID     string `json:"tool_id"`
				ToolCallID string `json:"tool_call_id"`
				ToolName   string `json:"tool_name"`
				Error      string `json:"error"`
				Message    string `json:"message"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			id := p.ToolID
			if id == "" {
				id = p.ToolCallID
			}
			tc := getTool(id, p.ToolName)
			tc.Status = "error"
			tc.Result = p.Error
			if tc.Result == "" {
				tc.Result = p.Message
			}
			if err := emitTool(tc); err != nil {
				res, _ := finish()
				return res, err
			}
		case "tool_complete":
			var p struct {
				ToolID     string `json:"tool_id"`
				ToolCallID string `json:"tool_call_id"`
				ToolName   string `json:"tool_name"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			id := p.ToolID
			if id == "" {
				id = p.ToolCallID
			}
			tc := getTool(id, p.ToolName)
			if tc.Status == "" {
				tc.Status = "complete"
			}
			if err := emitTool(tc); err != nil {
				res, _ := finish()
				return res, err
			}
		case "tool_call", "tool_call_update":
			var p struct {
				ToolCallID      string          `json:"toolCallId"`
				ToolCallIDSnake string          `json:"tool_call_id"`
				ToolID          string          `json:"tool_id"`
				ToolName        string          `json:"tool_name"`
				Title           string          `json:"title"`
				RawInput        json.RawMessage `json:"rawInput"`
				RawInputSnake   json.RawMessage `json:"raw_input"`
				Arguments       json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			id := p.ToolID
			if id == "" {
				id = p.ToolCallID
			}
			if id == "" {
				id = p.ToolCallIDSnake
			}
			name := p.ToolName
			if name == "" {
				name = p.Title
			}
			tc := getTool(id, name)
			args := p.RawInput
			if len(args) == 0 {
				args = p.RawInputSnake
			}
			if len(args) == 0 {
				args = p.Arguments
			}
			if len(args) > 0 {
				tc.Arguments = argumentString(args)
			}
			if err := emitOrUpdate(tc); err != nil {
				res, _ := finish()
				return res, err
			}
		case "message_complete":
			sawMessageComplete = true
			if strict {
				var p struct {
					ContinuationExpected bool `json:"continuation_expected"`
				}
				_ = json.Unmarshal(env.Payload, &p)
				result.Usage.add(parseUsage(env.Payload))
				if p.ContinuationExpected {
					continue
				}
				final := completeText(env.Payload)
				if final == "" {
					final = completedText
				}
				if suffix, matches := strings.CutPrefix(final, text.String()); matches {
					if err := emitText(suffix); err != nil {
						return result, err
					}
				} else if final != "" && strings.TrimSpace(text.String()) != strings.TrimSpace(final) {
					return result, fmt.Errorf("AWS Quick final text differs from the streamed reply")
				}
				return finish()
			}
			if text.Len() == 0 {
				if err := emitText(completeText(env.Payload)); err != nil {
					res, _ := finish()
					return res, err
				}
			}
			result.Usage.add(parseUsage(env.Payload))
			var p struct {
				ContinuationExpected bool `json:"continuation_expected"`
				ToolCalls            []struct {
					ID         string          `json:"id"`
					ToolCallID string          `json:"tool_call_id"`
					Name       string          `json:"name"`
					ToolName   string          `json:"tool_name"`
					Arguments  json.RawMessage `json:"arguments"`
					Function   struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			for _, item := range p.ToolCalls {
				id := item.ID
				if id == "" {
					id = item.ToolCallID
				}
				name := item.Name
				if name == "" {
					name = item.ToolName
				}
				if name == "" {
					name = item.Function.Name
				}
				tc := getTool(id, name)
				args := item.Arguments
				if len(args) == 0 {
					args = item.Function.Arguments
				}
				if len(args) > 0 {
					tc.Arguments = argumentString(args)
				}
				if err := emitTool(tc); err != nil {
					res, _ := finish()
					return res, err
				}
			}
			if p.ContinuationExpected {
				completeDeadline = time.Time{}
			} else if !result.Usage.empty() {
				completeDeadline = time.Now().Add(200 * time.Millisecond)
			} else {
				completeDeadline = time.Now().Add(2 * time.Second)
			}
		case "error", "session_error", "stream_error", "turn_failed", "guardrail_blocked", "stream_truncated":
			msg := errorText(env.Payload, extractErrorValue(env.Error), env.Message, env.Reason, env.Code, env.ErrorCode)
			if msg == "" {
				msg = "empty error event"
				log.Printf("upstream %s raw: %s", env.Type, trimLog(jsonStr, 800))
			}
			res, _ := finish()
			if !strict && isContentFilter(msg) && (strings.TrimSpace(res.Text) != "" || len(res.ToolCalls) > 0) {
				log.Printf("content filter after output; returning partial reply")
				return res, nil
			}
			return res, fmt.Errorf("upstream %s: %s", env.Type, msg)
		default:
			if terminal[env.Type] {
				if strict && env.Type == "turn_aborted" {
					return result, fmt.Errorf("AWS Quick aborted the harness turn")
				}
				if strict && text.Len() == 0 && completedText != "" {
					if err := emitText(completedText); err != nil {
						return result, err
					}
				}
				return finish()
			}
			if env.Type == "ping" && sendPong != nil {
				sendPong()
			}
			if sawMessageComplete && env.Type == "session_list" {
				continue
			}
		}
	}
}

func deltaText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var p struct {
		Delta   json.RawMessage `json:"delta"`
		Text    string          `json:"text"`
		Content string          `json:"content"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	if t := nestedText(p.Delta); t != "" {
		return t
	}
	if p.Text != "" {
		return p.Text
	}
	return p.Content
}

func completeText(raw json.RawMessage) string {
	if t := deltaText(raw); t != "" {
		return t
	}
	var p struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &p)
	return p.Message
}

func nestedText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var p struct {
		Text    string `json:"text"`
		Content string `json:"content"`
	}
	_ = json.Unmarshal(raw, &p)
	if p.Text != "" {
		return p.Text
	}
	return p.Content
}

func rawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

func errorText(raw json.RawMessage, extras ...string) string {
	if s := extractErrorValue(raw); s != "" {
		return s
	}
	for _, s := range extras {
		if t := strings.TrimSpace(s); t != "" {
			return t
		}
	}
	compact := strings.TrimSpace(string(bytes.TrimSpace(raw)))
	if compact != "" && compact != "null" && compact != "{}" && compact != "[]" {
		if len(compact) > 240 {
			compact = compact[:240] + "…"
		}
		return compact
	}
	return ""
}

func extractErrorValue(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return strings.TrimSpace(string(raw))
	}
	return pickError(v, 0)
}

func pickError(v any, depth int) string {
	if depth > 6 || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return usefulError(t)
	case map[string]any:
		for _, k := range []string{"message", "error_message", "errorMessage", "detail", "details", "reason", "description", "msg", "cause", "error", "exception", "statusText", "type", "code", "error_code", "errorCode"} {
			if s := pickError(t[k], depth+1); s != "" {
				return s
			}
		}
	case []any:
		for _, item := range t {
			if s := pickError(item, depth+1); s != "" {
				return s
			}
		}
	}
	return ""
}

func isContentFilter(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "content filter") || strings.Contains(m, "guardrail") || strings.Contains(m, "blocked by")
}

func usefulError(s string) string {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "", "error", "errors", "exception", "fail", "failed", "unknown", "unknown error", "unknown upstream error":
		return ""
	}
	return s
}

func trimLog(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if n > 0 && len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func titleFor(prompt string) string {
	clean := strings.Join(strings.Fields(prompt), " ")
	if len(clean) > 60 {
		clean = clean[:60] + "…"
	}
	if clean == "" {
		clean = "New Conversation"
	}
	return clean
}
