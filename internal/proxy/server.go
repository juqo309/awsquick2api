package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Server struct {
	cfg       *Config
	sessions  *SessionSource
	accounts  *AccountStore
	tokens    *TokenStore
	panel     *PanelAuth
	settings  *settingsStore
	stats     *Stats
	login     *LoginJob
	refreshes refreshManager
	quotas    quotaManager
	agentMu   sync.Mutex
}

type callTap struct {
	model     string
	stream    bool
	start     time.Time
	status    string
	err       string
	usage     Usage
	accountID string
	ttftMS    *int64
}

func (t *callTap) firstToken() {
	if t.ttftMS == nil {
		ms := time.Since(t.start).Milliseconds()
		t.ttftMS = &ms
	}
}

func (t *callTap) observeText(ev StreamEvent) error {
	if ev.Type == "text" && ev.Text != "" {
		t.firstToken()
	}
	return nil
}

func (t *callTap) fail(err error, fallback string) string {
	msg := describeErr(err, fallback)
	t.status = "error"
	t.err = msg
	return msg
}

func describeErr(err error, fallback string) string {
	fallback = strings.TrimSpace(fallback)
	if fallback == "" {
		fallback = "request failed"
	}
	if err == nil {
		return fallback
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fallback + ": timed out"
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" || strings.EqualFold(msg, fallback) {
		return fallback
	}
	if strings.Contains(msg, fallback) {
		return msg
	}
	return fallback + ": " + msg
}

func NewServer(cfg *Config) *Server {
	statsPath := cfg.StatsFile
	if statsPath == "" {
		statsPath = "data/stats.json"
	}
	accPath := cfg.AccountsFile
	if accPath == "" {
		accPath = "data/accounts.json"
	}
	return &Server{
		cfg:      cfg,
		sessions: NewSessionSource(cfg),
		accounts: NewAccountStore(accPath, cfg.SessionFile, cfg.UpstreamFile),
		tokens:   NewTokenStore(cfg.TokensFile),
		panel:    NewPanelAuth(cfg.PanelAuthFile),
		settings: newSettingsStore(filepath.Join(filepath.Dir(accPath), "settings.json"), cfg),
		stats:    NewStats(statsPath),
		login:    newLoginJob(),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/v1/messages", s.handleAnthropicMessages)
	mux.HandleFunc("/v1/messages/count_tokens", s.handleAnthropicCount)
	mux.HandleFunc("/v1/accounts/upload", s.handleAccountsUpload)
	mux.HandleFunc("/admin/", s.panel.requireAPI(s.handleAdmin))
	mountPanel(mux, s.panel)
	return s.withCORS(mux)
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Api-Key, X-Thread-Id, X-Account-Id, Anthropic-Version, Anthropic-Beta")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "quickwork-openai"})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}
	if !s.tokens.Allowed(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid proxy API key")
		return
	}
	created := time.Now().Unix()
	models := s.settings.snapshot().Models
	data := make([]map[string]any, 0, len(models))
	for _, id := range models {
		data = append(data, map[string]any{
			"id":       id,
			"object":   "model",
			"created":  created,
			"owned_by": "quickwork",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	if !s.tokens.Allowed(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid proxy API key")
		return
	}

	tap := &callTap{start: time.Now(), stream: false}
	s.stats.Begin()
	defer s.stats.End()
	defer s.finishCall(tap)

	var req ChatRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxChatBodyBytes))
	if err := dec.Decode(&req); err != nil {
		tap.status, tap.err = "error", err.Error()
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	if req.N > 1 {
		tap.status, tap.err = "error", "n > 1 is not supported"
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "n > 1 is not supported")
		return
	}

	prompt, err := formatPrompt(req.Messages)
	if err != nil {
		tap.status, tap.err = "error", err.Error()
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	settings := s.settings.snapshot()
	cfg := settings.config(s.cfg)
	cfg.ThinkingEffort = settings.thinking(req.ThinkingEffort, req.ReasoningEffort)
	upstream := NewUpstream(cfg)
	explicit := systemPrompt(req.Messages)
	systemPromptText := settings.effectiveSystem(explicit)
	model := settings.mapModel(req.Model)
	tap.model = model
	tap.stream = req.Stream
	allowedTools := requestedToolNames(&req)
	choice := req.ToolChoice
	if choice == nil {
		choice = req.FunctionCall
	}
	choiceMode, choiceName := parseToolChoice(choice)
	if choiceMode == "none" {
		allowedTools = nil
	} else if choiceMode == "function" {
		if !allowedTools[choiceName] {
			msg := "tool_choice name must match a declared tool"
			tap.status, tap.err = "error", msg
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", msg)
			return
		}
		allowedTools = map[string]bool{choiceName: true}
	}
	parallelTools := true
	if req.ParallelToolCalls != nil {
		parallelTools = *req.ParallelToolCalls
	}
	tools := openaiToolPrompt(&req, allowedTools, choiceMode, choiceName)

	ctx, cancel := context.WithTimeout(r.Context(), cfg.Timeout)
	defer cancel()

	sess, proxyURL, accID, err := s.resolveAccount(r)
	if err != nil {
		s.sessions.invalidate()
		tap.status, tap.err = "error", err.Error()
		writeOpenAIError(w, http.StatusUnauthorized, "upstream_session_unavailable",
			"no usable QuickWork session: "+err.Error()+"; add an account in the manager")
		return
	}
	tap.accountID = accID

	threadID := strings.TrimSpace(r.Header.Get("X-Thread-Id"))
	id := "chatcmpl-" + strings.ReplaceAll(uuid(), "-", "")
	created := time.Now().Unix()
	in := ChatInput{
		Prompt:       prompt,
		Model:        model,
		ThreadID:     threadID,
		Title:        titleFor(lastUserText(req.Messages)),
		SystemPrompt: systemPromptText,
		ToolPrompt:   tools,
		Proxy:        proxyURL,
		KeepThread:   threadID != "",
		AccountID:    accID,
		ChatAgentID:  s.chatAgentFor(accID),
		Media:        currentTurnMedia(req.Messages),
		ClientTurn:   explicit != "" || len(allowedTools) > 0,
	}

	sess = s.sessionSurface(sess, cfg)
	if !req.Stream {
		res, err := upstream.Chat(ctx, &sess, in, func(ev StreamEvent) error {
			if ev.ToolCall != nil && toolAllowed(*ev.ToolCall, allowedTools, choiceMode, choiceName) {
				tap.firstToken()
			}
			return tap.observeText(ev)
		})
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_error", tap.fail(err, "AWS Quick request failed"))
			return
		}
		toolCalls := filterToolCalls(res.ToolCalls, allowedTools, choiceMode, choiceName, parallelTools)
		if len(toolCalls) == 0 {
			toolCalls = parseClientToolCalls(res.Text, allowedTools, parallelTools)
			if len(toolCalls) > 0 {
				res.Text = ""
			} else if len(allowedTools) > 0 && extractToolJSON(res.Text) != "" {
				writeOpenAIError(w, http.StatusBadGateway, "upstream_error", tap.fail(nil, invalidClientToolReply))
				return
			}
		}
		if strings.TrimSpace(res.Text) == "" && len(toolCalls) == 0 {
			msg := res.emptyReplyErr()
			tap.status, tap.err = "error", msg
			writeOpenAIError(w, http.StatusBadGateway, "upstream_error", msg)
			return
		}
		tap.status = "ok"
		tap.usage = res.Usage
		message := map[string]any{"role": "assistant"}
		if strings.TrimSpace(res.Text) == "" {
			message["content"] = nil
		} else {
			message["content"] = res.Text
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
		}
		finish := "stop"
		if len(toolCalls) > 0 {
			finish = "tool_calls"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      id,
			"object":  "chat.completion",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{
				"index":         0,
				"message":       message,
				"finish_reason": finish,
			}},
			"usage": usageMap(res.Usage),
		})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming_unavailable", "response writer does not support flushing")
		return
	}
	var (
		streamMu      sync.Mutex
		streamStarted bool
		lastWrite     time.Time
	)
	writePayloadLocked := func(payload map[string]any) {
		body, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", body)
		flusher.Flush()
		lastWrite = time.Now()
	}
	startStreamLocked := func() {
		if streamStarted {
			return
		}
		streamStarted = true
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		if threadID != "" {
			w.Header().Set("X-Thread-Id", threadID)
		}
		w.WriteHeader(http.StatusOK)
		writePayloadLocked(map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]any{"role": "assistant"},
				"finish_reason": nil,
			}},
		})
	}
	writePayload := func(payload map[string]any) {
		streamMu.Lock()
		defer streamMu.Unlock()
		startStreamLocked()
		writePayloadLocked(payload)
	}
	writeChunk := func(delta map[string]any, finish any) {
		writePayload(map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}},
		})
	}
	writeStreamError := func(message string) {
		streamMu.Lock()
		started := streamStarted
		streamMu.Unlock()
		if !started {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_error", message)
			return
		}
		writePayload(map[string]any{"error": map[string]any{
			"message": message,
			"type":    "upstream_error",
			"code":    "upstream_error",
		}})
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}
	stopKeepalive := startSSEKeepalive(ctx, func() {
		streamMu.Lock()
		defer streamMu.Unlock()
		startStreamLocked()
	}, func() {
		streamMu.Lock()
		defer streamMu.Unlock()
		if !streamStarted || time.Since(lastWrite) < streamPingInterval*3/4 {
			return
		}
		_, _ = fmt.Fprint(w, ": keepalive\n\n")
		flusher.Flush()
		lastWrite = time.Now()
	})
	defer stopKeepalive()

	sawText := false
	sawTool := false
	toolIndex := 0
	toolIndexes := make(map[string]int)
	sentArgs := make(map[string]string)
	emitTool := func(tc ToolCallState) {
		if !toolAllowed(tc, allowedTools, choiceMode, choiceName) {
			return
		}
		idx, seen := toolIndexes[tc.ID]
		if !seen {
			if !parallelTools && sawTool {
				return
			}
			idx = toolIndex
			toolIndex++
			toolIndexes[tc.ID] = idx
			args := strings.TrimSpace(tc.Arguments)
			if args == "" {
				args = "{}"
			}
			sentArgs[tc.ID] = args
			tap.firstToken()
			writeChunk(map[string]any{"tool_calls": []map[string]any{{
				"index": idx,
				"id":    tc.ID,
				"type":  "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": args,
				},
			}}}, nil)
			sawTool = true
			return
		}
		args := strings.TrimSpace(tc.Arguments)
		if args == "" {
			args = "{}"
		}
		prev := sentArgs[tc.ID]
		if args == prev {
			return
		}
		delta := args
		if strings.HasPrefix(args, prev) {
			delta = args[len(prev):]
		}
		sentArgs[tc.ID] = args
		if delta == "" {
			return
		}
		writeChunk(map[string]any{"tool_calls": []map[string]any{{
			"index":    idx,
			"function": map[string]any{"arguments": delta},
		}}}, nil)
		sawTool = true
	}

	var gate toolStreamGate
	res, err := upstream.Chat(ctx, &sess, in, func(ev StreamEvent) error {
		switch ev.Type {
		case "text":
			if ev.Text == "" {
				return nil
			}
			out := ev.Text
			if len(allowedTools) > 0 {
				out = gate.Push(ev.Text)
			}
			if out == "" {
				return nil
			}
			tap.firstToken()
			sawText = true
			writeChunk(map[string]any{"content": out}, nil)
		case "tool_call":
			if ev.ToolCall != nil {
				emitTool(*ev.ToolCall)
			}
		case "tool_call_update":
			if ev.ToolCall != nil {
				emitTool(*ev.ToolCall)
			}
		}
		return nil
	})
	stopKeepalive()
	if err != nil {
		if (sawText || sawTool) && errors.Is(err, context.Canceled) {
			tap.status = "ok"
			tap.usage = res.Usage
			return
		}
		writeStreamError(tap.fail(err, "AWS Quick request failed"))
		return
	}
	if !sawTool {
		source := res.Text
		if gate.Tool() {
			source = gate.Held()
		}
		for _, tc := range parseClientToolCalls(source, allowedTools, parallelTools) {
			emitTool(ToolCallState{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
		}
	}
	if !sawTool && gate.Tool() {
		writeStreamError(tap.fail(nil, invalidClientToolReply))
		return
	}
	if !sawTool {
		if leftover := gate.FlushText(); leftover != "" {
			tap.firstToken()
			sawText = true
			writeChunk(map[string]any{"content": leftover}, nil)
		}
	}
	if !sawText && !sawTool {
		msg := res.emptyReplyErr()
		tap.status, tap.err = "error", msg
		writeStreamError(msg)
		return
	}
	tap.status = "ok"
	tap.usage = res.Usage
	finish := "stop"
	if sawTool {
		finish = "tool_calls"
	}
	writeChunk(map[string]any{}, finish)
	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		writePayload(map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []any{},
			"usage":   usageMap(res.Usage),
		})
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (s *Server) finishCall(t *callTap) {
	if t == nil {
		return
	}
	if t.status == "" {
		t.status = "error"
	}
	if t.status != "ok" && strings.TrimSpace(t.err) == "" {
		t.err = "aborted"
	}
	if t.status != "ok" {
		log.Printf("request error model=%s stream=%v account=%s: %s", t.model, t.stream, t.accountID, t.err)
	}
	prompt := t.usage.PromptTokens
	if prompt == 0 && t.usage.TotalTokens > t.usage.CompletionTokens {
		prompt = t.usage.TotalTokens - t.usage.CompletionTokens
	}
	total := t.usage.TotalTokens
	if total == 0 {
		total = prompt + t.usage.CompletionTokens
	}
	s.stats.Record(CallRecord{
		ID:               uuid(),
		At:               t.start.UnixMilli(),
		Model:            t.model,
		Stream:           t.stream,
		Status:           t.status,
		Error:            t.err,
		DurationMs:       time.Since(t.start).Milliseconds(),
		TTFTMs:           t.ttftMS,
		PromptTokens:     prompt,
		CompletionTokens: t.usage.CompletionTokens,
		TotalTokens:      total,
		CacheReadTokens:  t.usage.CacheReadTokens,
		AccountID:        t.accountID,
	})
	if t.accountID != "" && t.status == "ok" {
		s.scheduleQuotaRefresh(t.accountID)
	}
}

func (s *Server) resolveAccount(r *http.Request) (Session, string, string, error) {
	now := time.Now().Unix()
	var acc Account
	if id := requestedAccount(r); id != "" {
		var err error
		acc, err = s.accounts.Get(id)
		if err != nil {
			return Session{}, "", "", err
		}
		if !acc.Enabled {
			return Session{}, "", "", fmt.Errorf("account is disabled")
		}
		if acc.Session.UsableUntil > 0 && acc.Session.UsableUntil <= now {
			return Session{}, "", "", fmt.Errorf("account session has expired")
		}
	} else {
		selected, ok, managed := s.accounts.Select(strings.TrimSpace(r.Header.Get("X-Thread-Id")), now)
		if !ok {
			if managed || s.cfg.OutpostURL == "" {
				return Session{}, "", "", fmt.Errorf("no usable account")
			}
			sess, err := s.sessions.Get(r.Context())
			if err != nil {
				return Session{}, "", "", err
			}
			return *sess, "", "", nil
		}
		acc = selected
	}
	sess := acc.Session
	s.sessions.fillDefaults(&sess)
	if err := sess.validate(); err != nil {
		return Session{}, "", "", err
	}
	return sess, acc.Proxy, acc.ID, nil
}

func (s *Server) sessionSurface(sess Session, cfg *Config) Session {
	if cfg.Surface != s.cfg.Surface || sess.Surface == "" {
		sess.Surface = cfg.Surface
	}
	return sess
}

func (s runtimeSettings) mapModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	m = strings.TrimPrefix(m, "quickwork/")
	m = strings.TrimPrefix(m, "qw-")
	for _, allowed := range s.Models {
		if strings.EqualFold(model, allowed) || strings.EqualFold(m, allowed) {
			return allowed
		}
	}
	if m != "" {
		log.Printf("unknown model %q, using %s", model, s.DefaultModel)
	}
	return s.DefaultModel
}

func openaiToolPrompt(req *ChatRequest, allowed map[string]bool, mode, name string) string {
	if len(allowed) == 0 {
		return ""
	}
	var specs []clientToolSpec
	for _, tool := range req.Tools {
		f := tool.Function
		if name := strings.TrimSpace(f.Name); allowed[name] {
			specs = append(specs, clientToolSpec{name, f.Description, f.Parameters})
		}
	}
	for _, f := range req.Functions {
		if name := strings.TrimSpace(f.Name); allowed[name] {
			specs = append(specs, clientToolSpec{name, f.Description, f.Parameters})
		}
	}
	var extra string
	switch mode {
	case "required", "any":
		extra += " You must request a tool call."
	case "function":
		if name != "" {
			encoded, _ := json.Marshal(name)
			extra += " You must use only the tool named " + string(encoded) + "."
		}
	}
	if req.ParallelToolCalls != nil && !*req.ParallelToolCalls {
		extra += " Request at most one tool call."
	}
	return formatClientToolPrompt(specs, extra)
}

func requestedToolNames(req *ChatRequest) map[string]bool {
	names := make(map[string]bool)
	for _, t := range req.Tools {
		if name := strings.TrimSpace(t.Function.Name); name != "" {
			names[name] = true
		}
	}
	for _, f := range req.Functions {
		if name := strings.TrimSpace(f.Name); name != "" {
			names[name] = true
		}
	}
	return names
}

func parseToolChoice(v any) (mode, name string) {
	switch x := v.(type) {
	case string:
		return strings.ToLower(strings.TrimSpace(x)), ""
	case map[string]any:
		if fn, ok := x["function"].(map[string]any); ok {
			if n, ok := fn["name"].(string); ok {
				return "function", strings.TrimSpace(n)
			}
		}
		if n, ok := x["name"].(string); ok {
			return "function", strings.TrimSpace(n)
		}
		if t, ok := x["type"].(string); ok {
			return strings.ToLower(strings.TrimSpace(t)), ""
		}
	}
	return "auto", ""
}

func toolAllowed(tc ToolCallState, allowed map[string]bool, mode, name string) bool {
	if mode == "none" || len(allowed) == 0 {
		return false
	}
	if !allowed[tc.Name] {
		return false
	}
	if mode == "function" && name != "" && tc.Name != name {
		return false
	}
	return true
}

func filterToolCalls(calls []ToolCallState, allowed map[string]bool, mode, name string, parallel bool) []ToolCall {
	out := make([]ToolCall, 0, len(calls))
	for _, tc := range calls {
		if !toolAllowed(tc, allowed, mode, name) {
			continue
		}
		if !parallel && len(out) > 0 {
			break
		}
		id := strings.TrimSpace(tc.ID)
		if id == "" {
			id = "call_" + strings.ReplaceAll(uuid(), "-", "")
		}
		toolName := strings.TrimSpace(tc.Name)
		if toolName == "" {
			toolName = "unknown"
		}
		args := strings.TrimSpace(tc.Arguments)
		if args == "" {
			args = "{}"
		}
		out = append(out, ToolCall{
			ID:   id,
			Type: "function",
			Function: FunctionCall{
				Name:      toolName,
				Arguments: args,
			},
		})
	}
	return out
}

func usageMap(u Usage) map[string]any {
	completion := u.CompletionTokens
	total := u.TotalTokens
	if total == 0 {
		total = u.PromptTokens + u.CacheReadTokens + u.CacheWriteTokens + completion
	}
	prompt := total - completion
	if prompt < 0 {
		prompt = u.PromptTokens + u.CacheReadTokens + u.CacheWriteTokens
	}
	m := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      total,
	}
	if u.CacheReadTokens > 0 {
		m["prompt_tokens_details"] = map[string]any{"cached_tokens": u.CacheReadTokens}
	}
	return m
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    code,
			"code":    code,
		},
	})
}
