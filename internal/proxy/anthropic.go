package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type anthropicOutputConfig struct {
	Effort string `json:"effort"`
}

type anthropicRequest struct {
	Model    string          `json:"model"`
	System   json.RawMessage `json:"system"`
	Messages []struct {
		Role         string                `json:"role"`
		Content      json.RawMessage       `json:"content"`
		ClearAt      string                `json:"clear_at"`
		OutputConfig anthropicOutputConfig `json:"output_config"`
	} `json:"messages"`
	Tools []struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	} `json:"tools"`
	ToolChoice struct {
		Type       string `json:"type"`
		Name       string `json:"name"`
		NoParallel bool   `json:"disable_parallel_tool_use"`
	} `json:"tool_choice"`
	Stream          bool                  `json:"stream"`
	MaxTokens       int                   `json:"max_tokens"`
	StopSequences   []string              `json:"stop_sequences"`
	ThinkingEffort  string                `json:"thinking_effort"`
	ReasoningEffort string                `json:"reasoning_effort"`
	OutputConfig    anthropicOutputConfig `json:"output_config"`
	Thinking        struct {
		Type   string `json:"type"`
		Budget int    `json:"budget_tokens"`
	} `json:"thinking"`
}

type anthropicSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
	FileID    string `json:"file_id"`
}

type anthropicBlock struct {
	Type      string           `json:"type"`
	Text      *string          `json:"text,omitempty"`
	ID        string           `json:"id,omitempty"`
	Name      string           `json:"name,omitempty"`
	Input     json.RawMessage  `json:"input,omitempty"`
	Content   json.RawMessage  `json:"content,omitempty"`
	ToolUseID string           `json:"tool_use_id,omitempty"`
	IsError   bool             `json:"is_error,omitempty"`
	Source    *anthropicSource `json:"source,omitempty"`
}

func anthropicText(text string) anthropicBlock { return anthropicBlock{Type: "text", Text: &text} }

func anthropicBlocks(raw json.RawMessage) ([]anthropicBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []anthropicBlock{anthropicText(text)}, nil
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil, fmt.Errorf("content must be a string or an array of blocks")
	}
	return blocks, nil
}

func anthropicTextOnly(raw json.RawMessage) (string, error) {
	blocks, err := anthropicBlocks(raw)
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for _, block := range blocks {
		if block.Type == "text" && block.Text != nil {
			text.WriteString(*block.Text)
		}
	}
	return text.String(), nil
}

func appendRoleContent(messages *[]Message, role string, part any) {
	msgs := *messages
	if n := len(msgs); n > 0 && msgs[n-1].Role == role && len(msgs[n-1].ToolCalls) == 0 && msgs[n-1].ToolCallID == "" {
		msgs[n-1].Content = mergeContent(msgs[n-1].Content, part)
		return
	}
	*messages = append(msgs, Message{Role: role, Content: part})
}

func anthropicMediaPart(b anthropicBlock) (any, error) {
	if b.Source == nil {
		return nil, fmt.Errorf("%s block missing source", b.Type)
	}
	media, ok := mediaFromSource(*b.Source)
	if !ok {
		return mediaMarker(b.Type), nil
	}
	return mediaPart(media), nil
}

func (r anthropicRequest) transcript() ([]Message, error) {
	system, err := anthropicTextOnly(r.System)
	if err != nil {
		return nil, fmt.Errorf("system: %w", err)
	}
	var messages []Message
	var instructions []string
	if system != "" {
		instructions = append(instructions, system)
	}
	lastUser := -1
	for i, msg := range r.Messages {
		if msg.Role == "user" {
			lastUser = i
		}
	}
	for i, msg := range r.Messages {
		blocks, err := anthropicBlocks(msg.Content)
		if err != nil {
			return nil, err
		}
		if msg.Role == "system" {
			var text strings.Builder
			for _, block := range blocks {
				if block.Type != "text" || block.Text == nil {
					return nil, fmt.Errorf("system messages only support text blocks")
				}
				text.WriteString(*block.Text)
			}
			if text.Len() > 0 && (msg.ClearAt != "next_user_message" || i > lastUser) {
				instructions = append(instructions, text.String())
			}
			continue
		}
		if msg.Role != "user" && msg.Role != "assistant" {
			return nil, fmt.Errorf("messages must use user, assistant, or system roles")
		}
		if len(blocks) == 0 {
			return nil, fmt.Errorf("messages must have content")
		}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if b.Text == nil {
					return nil, fmt.Errorf("text block missing text")
				}
				appendRoleContent(&messages, msg.Role, *b.Text)
			case "image", "document":
				part, err := anthropicMediaPart(b)
				if err != nil {
					return nil, err
				}
				appendRoleContent(&messages, msg.Role, part)
			case "tool_use":
				var input map[string]any
				if msg.Role != "assistant" || b.ID == "" || b.Name == "" || json.Unmarshal(b.Input, &input) != nil || input == nil {
					return nil, fmt.Errorf("invalid assistant tool_use block")
				}
				messages = append(messages, Message{Role: "assistant", ToolCalls: []ToolCall{{ID: b.ID, Type: "function", Function: FunctionCall{Name: b.Name, Arguments: string(b.Input)}}}})
			case "tool_result":
				if msg.Role != "user" || b.ToolUseID == "" {
					return nil, fmt.Errorf("invalid user tool_result block")
				}
				nested, err := anthropicBlocks(b.Content)
				if err != nil {
					return nil, fmt.Errorf("tool_result: %w", err)
				}
				text, _ := anthropicTextOnly(b.Content)
				if b.IsError {
					text = "Error: " + text
				}
				var content any = text
				var parts []any
				if text != "" {
					parts = append(parts, map[string]any{"type": "text", "text": text})
				}
				for _, nb := range nested {
					if nb.Type != "image" && nb.Type != "document" {
						continue
					}
					part, err := anthropicMediaPart(nb)
					if err != nil {
						return nil, fmt.Errorf("tool_result: %w", err)
					}
					parts = append(parts, part)
				}
				if len(parts) > 1 || (len(parts) == 1 && text == "") {
					content = parts
				}
				messages = append(messages, Message{Role: "tool", ToolCallID: b.ToolUseID, Content: content})
			case "thinking", "redacted_thinking":
				if msg.Role != "assistant" {
					return nil, fmt.Errorf("thinking blocks require the assistant role")
				}
			default:
				return nil, fmt.Errorf("unsupported content block %q", b.Type)
			}
		}
	}
	// Quick generates only the next turn and has a single system instruction.
	// Fold active system messages into it in order, with the latest last.
	if len(instructions) > 0 {
		messages = append([]Message{{Role: "system", Content: strings.Join(instructions, "\n\n")}}, messages...)
	}
	return messages, nil
}

func (r anthropicRequest) toolPrompt() (harnessTools, error) {
	allowed := make(map[string]bool)
	for _, t := range r.Tools {
		// Some clients send Anthropic-hosted web search next to local tools.
		// Quick cannot execute the hosted tool, but local tools still work.
		if t.Name == "web_search" && (t.Type == "web_search_20250305" || t.Type == "web_search_20260209") {
			continue
		}
		var schema map[string]any
		if (t.Type != "" && t.Type != "custom") || t.Name == "" || allowed[t.Name] || json.Unmarshal(t.InputSchema, &schema) != nil || schema == nil {
			return harnessTools{}, fmt.Errorf("tools must be uniquely named client tools with an input_schema object")
		}
		allowed[t.Name] = true
	}
	choice := r.ToolChoice.Type
	switch choice {
	case "none":
		return harnessTools{}, nil
	case "", "auto":
		if len(allowed) == 0 {
			return harnessTools{}, nil
		}
	case "any":
		if len(allowed) == 0 {
			return harnessTools{}, fmt.Errorf("tool_choice any requires tools")
		}
	case "tool":
		if !allowed[r.ToolChoice.Name] {
			return harnessTools{}, fmt.Errorf("tool_choice name must match a declared tool")
		}
	default:
		return harnessTools{}, fmt.Errorf("invalid tool_choice type")
	}
	var extra string
	if choice == "any" {
		extra += " You must request a tool call."
	}
	if choice == "tool" {
		name, _ := json.Marshal(r.ToolChoice.Name)
		extra += " You must use only the tool named " + string(name) + "."
		allowed = map[string]bool{r.ToolChoice.Name: true}
	}
	if r.ToolChoice.NoParallel {
		extra += " Request at most one tool call."
	}
	specs := make([]clientToolSpec, 0, len(allowed))
	for _, tool := range r.Tools {
		if allowed[tool.Name] {
			specs = append(specs, clientToolSpec{tool.Name, tool.Description, tool.InputSchema})
		}
	}
	return makeHarnessTools(specs, extra, allowed), nil
}

func (r anthropicRequest) outputEffort() string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		msg := r.Messages[i]
		if msg.Role == "system" && msg.OutputConfig.Effort != "" {
			return msg.OutputConfig.Effort
		}
	}
	return r.OutputConfig.Effort
}

func (r anthropicRequest) effort() string {
	switch r.Thinking.Type {
	case "disabled":
		return "off"
	case "enabled":
		switch {
		case r.Thinking.Budget >= 8192:
			return "high"
		case r.Thinking.Budget >= 4096:
			return "medium"
		default:
			return "low"
		}
	default:
		return ""
	}
}

func anthropicError(w http.ResponseWriter, status int, kind, message string) {
	writeJSON(w, status, anthropicErrorBody(kind, message))
}

func anthropicErrorBody(kind, message string) map[string]any {
	return map[string]any{"type": "error", "error": map[string]string{"type": kind, "message": message}}
}

func (s *Server) readAnthropic(w http.ResponseWriter, req *http.Request) (anthropicRequest, bool) {
	var r anthropicRequest
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		anthropicError(w, 405, "invalid_request_error", "POST required")
		return r, false
	}
	if !s.tokens.Allowed(req) {
		anthropicError(w, 401, "authentication_error", "invalid proxy API key")
		return r, false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, maxChatBodyBytes))
	if dec.Decode(&r) != nil || dec.Decode(new(json.RawMessage)) != io.EOF || strings.TrimSpace(r.Model) == "" || len(r.Messages) == 0 {
		anthropicError(w, 400, "invalid_request_error", "a JSON body with model and messages is required")
		return r, false
	}
	return r, true
}

func (s *Server) handleAnthropicCount(w http.ResponseWriter, req *http.Request) {
	r, ok := s.readAnthropic(w, req)
	if !ok {
		return
	}
	prompt, system, tools, _, _, err := r.prepare(s.settings.snapshot())
	if err != nil {
		anthropicError(w, 400, "invalid_request_error", err.Error())
		return
	}
	// The upstream has no count endpoint. This is a rough local estimate.
	w.Header().Set("X-Token-Count-Estimated", "true")
	writeJSON(w, 200, map[string]int{"input_tokens": max(1, (utf8.RuneCountInString(prompt)+utf8.RuneCountInString(system)+utf8.RuneCountInString(tools.prompt)+utf8.RuneCountInString(tools.catalog)+3)/4)})
}

func (r anthropicRequest) prepare(settings runtimeSettings) (prompt, system string, tools harnessTools, media []Media, clientTurn bool, err error) {
	messages, err := r.transcript()
	if err != nil {
		return "", "", harnessTools{}, nil, false, err
	}
	prompt, err = formatPrompt(messages)
	if err != nil {
		return "", "", harnessTools{}, nil, false, err
	}
	explicit := systemPrompt(messages)
	tools, err = r.toolPrompt()
	if err != nil {
		return "", "", harnessTools{}, nil, false, err
	}
	clientTurn = explicit != "" || len(tools.allowed) > 0
	system = settings.effectiveSystem(explicit)
	return prompt, system, tools, currentTurnMedia(messages), clientTurn, nil
}

func anthropicUsage(u Usage) map[string]int {
	return map[string]int{"input_tokens": u.PromptTokens, "output_tokens": u.CompletionTokens, "cache_read_input_tokens": u.CacheReadTokens, "cache_creation_input_tokens": u.CacheWriteTokens}
}

func anthropicMessage(id, model string, blocks []anthropicBlock, stop, sequence any, u Usage) map[string]any {
	return map[string]any{"id": id, "type": "message", "role": "assistant", "model": model, "content": blocks, "stop_reason": stop, "stop_sequence": sequence, "usage": anthropicUsage(u)}
}

type anthropicStops struct {
	sequences        []string
	pending, matched string
}

func (s *anthropicStops) push(text string, final bool) string {
	if s.matched != "" {
		return ""
	}
	s.pending += text
	pos := len(s.pending)
	for _, seq := range s.sequences {
		if i := strings.Index(s.pending, seq); i >= 0 && i < pos {
			pos, s.matched = i, seq
		}
	}
	if s.matched != "" {
		return s.pending[:pos]
	}
	if !final {
		for _, seq := range s.sequences {
			for n := min(len(seq)-1, len(s.pending)); n > 0; n-- {
				if strings.HasSuffix(s.pending, seq[:n]) {
					pos = min(pos, len(s.pending)-n)
					break
				}
			}
		}
		for pos > 0 && pos < len(s.pending) && !utf8.RuneStart(s.pending[pos]) {
			pos--
		}
	}
	out := s.pending[:pos]
	s.pending = s.pending[pos:]
	return out
}

func (s *Server) handleAnthropicMessages(w http.ResponseWriter, req *http.Request) {
	start := time.Now()
	r, ok := s.readAnthropic(w, req)
	if !ok {
		return
	}
	if r.MaxTokens < 1 {
		anthropicError(w, 400, "invalid_request_error", "max_tokens must be positive")
		return
	}
	if len(r.StopSequences) > 32 {
		anthropicError(w, 400, "invalid_request_error", "too many stop sequences")
		return
	}
	for _, seq := range r.StopSequences {
		if seq == "" || len(seq) > 256 {
			anthropicError(w, 400, "invalid_request_error", "stop sequences must be 1 to 256 bytes")
			return
		}
	}
	settings := s.settings.snapshot()
	prompt, system, tools, media, clientTurn, err := r.prepare(settings)
	if err != nil {
		anthropicError(w, 400, "invalid_request_error", err.Error())
		return
	}
	cfg := settings.config(s.cfg)
	cfg.ThinkingEffort = settings.thinking(r.ThinkingEffort, r.ReasoningEffort, r.outputEffort(), r.effort())
	model := settings.mapModel(r.Model)
	// The service does not expose a native token budget. Pass this as guidance,
	// not as an exact limit or a claim about why generation stopped.
	prompt += fmt.Sprintf("\n\n[Response length: aim for at most %d output tokens.]", r.MaxTokens)
	tap := &callTap{start: start, model: model, stream: r.Stream}
	s.stats.Begin()
	defer s.stats.End()
	defer s.finishCall(tap)
	ctx, cancel := context.WithTimeout(req.Context(), cfg.Timeout)
	defer cancel()
	sess, proxyURL, accountID, err := s.resolveAccount(req)
	if err != nil {
		anthropicError(w, 503, "api_error", tap.fail(err, "no usable AWS Quick account"))
		return
	}
	tap.accountID = accountID
	sess = s.sessionSurface(sess, cfg)
	threadID := strings.TrimSpace(req.Header.Get("X-Thread-Id"))
	in := ChatInput{Prompt: prompt, SystemPrompt: system, ToolPrompt: tools.prompt, HarnessCatalog: tools.catalog, Model: model, Title: titleFor(prompt), Proxy: proxyURL, AccountID: accountID, ChatAgentID: s.chatAgentFor(accountID), ThreadID: threadID, KeepThread: threadID != "", Media: media, ClientTurn: clientTurn}
	id := "msg_" + strings.ReplaceAll(uuid(), "-", "")
	w.Header().Set("Request-Id", "req_"+uuid())
	var stream *anthropicStream
	stopKeepalive := func() {}
	if r.Stream {
		flusher, ok := w.(http.Flusher)
		if !ok {
			anthropicError(w, 500, "api_error", "streaming unavailable")
			return
		}
		stream = &anthropicStream{w: w, flush: flusher, id: id, model: r.Model}
		stopKeepalive = startSSEKeepalive(ctx, stream.start, stream.ping)
		defer stopKeepalive()
	}
	stops := anthropicStops{sequences: r.StopSequences}
	var text strings.Builder
	var sawText, sawOutput bool
	emit := func(delta string) {
		if strings.TrimSpace(delta) != "" {
			tap.firstToken()
		}
		if stream != nil && delta != "" {
			sawOutput = true
			stream.text(delta)
		}
	}
	reply := harnessReply{tools: tools, single: r.ToolChoice.NoParallel, emit: emit}
	receive := func(delta string, final bool) {
		out := stops.push(delta, final)
		if len(tools.allowed) != 0 {
			reply.push(out)
		} else {
			text.WriteString(out)
			emit(out)
		}
	}
	res, err := NewUpstream(cfg).Chat(ctx, &sess, in, func(ev StreamEvent) error {
		if ev.Type == "text" && ev.Text != "" {
			sawText = true
			receive(ev.Text, false)
		}
		return nil
	})
	stopKeepalive()
	tap.usage = res.Usage
	fail := func(message string) {
		tap.status = "error"
		tap.err = message
		if stream != nil && stream.isStarted() {
			stream.event("error", anthropicErrorBody("api_error", message))
		} else {
			anthropicError(w, 502, "api_error", message)
		}
	}
	if err != nil {
		if stream != nil && sawOutput && errors.Is(err, context.Canceled) {
			tap.status = "ok"
			return
		}
		fail(describeErr(err, "AWS Quick request failed"))
		return
	}
	if !sawText {
		receive(res.Text, false)
	}
	receive("", true)
	blocks := []anthropicBlock{}
	reason := "end_turn"
	var sequence any
	if len(tools.allowed) != 0 {
		blocks = reply.finish()
		if reply.calls > 0 {
			reason = "tool_use"
		} else if reply.invalid && stops.matched == "" {
			fail(invalidClientToolReply)
			return
		}
	} else if text.Len() != 0 {
		blocks = append(blocks, anthropicText(text.String()))
	}
	if stops.matched != "" {
		reason, sequence = "stop_sequence", stops.matched
		prose := blocks[:0]
		for _, block := range blocks {
			if block.Text != nil {
				prose = append(prose, block)
			}
		}
		blocks = prose
	}
	if len(blocks) == 0 && reason != "stop_sequence" {
		fail(res.emptyReplyErr())
		return
	}
	if (r.ToolChoice.Type == "any" || r.ToolChoice.Type == "tool") && reason != "tool_use" && reason != "stop_sequence" {
		fail("AWS Quick did not return the requested tool call")
		return
	}
	tap.status = "ok"
	if reason == "tool_use" {
		tap.firstToken()
	}
	if stream != nil {
		stream.finish(blocks, reason, sequence, res.Usage)
		return
	}
	writeJSON(w, 200, anthropicMessage(id, r.Model, blocks, reason, sequence, res.Usage))
}

var (
	streamStartDelay   = 8 * time.Second
	streamPingInterval = 4 * time.Second
)

func startSSEKeepalive(ctx context.Context, onStart, onPing func()) func() {
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		startTimer := time.NewTimer(streamStartDelay)
		defer startTimer.Stop()
		pingTicker := time.NewTicker(streamPingInterval)
		defer pingTicker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ctx.Done():
				return
			case <-startTimer.C:
				if onStart != nil {
					onStart()
				}
			case <-pingTicker.C:
				if onPing != nil {
					onPing()
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stopCh)
			<-doneCh
		})
	}
}

type anthropicStream struct {
	mu                sync.Mutex
	w                 http.ResponseWriter
	flush             http.Flusher
	id, model         string
	started, textOpen bool
	index, textBytes  int
	lastWrite         time.Time
}

func (s *anthropicStream) isStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

func (s *anthropicStream) eventLocked(kind string, body map[string]any) {
	body["type"] = kind
	data, _ := json.Marshal(body)
	_, _ = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", kind, data)
	s.flush.Flush()
	s.lastWrite = time.Now()
}

func (s *anthropicStream) event(kind string, body map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eventLocked(kind, body)
}

func (s *anthropicStream) startLocked() {
	if s.started {
		return
	}
	s.started = true
	s.w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	s.w.Header().Set("Cache-Control", "no-cache")
	s.w.Header().Set("Connection", "keep-alive")
	s.w.Header().Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
	s.flush.Flush()
	s.eventLocked("message_start", map[string]any{"message": anthropicMessage(s.id, s.model, []anthropicBlock{}, nil, nil, Usage{})})
}

func (s *anthropicStream) start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startLocked()
}

func (s *anthropicStream) ping() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started {
		return
	}
	if time.Since(s.lastWrite) < streamPingInterval*3/4 {
		return
	}
	s.eventLocked("ping", map[string]any{})
}

func (s *anthropicStream) textLocked(text string) {
	s.startLocked()
	s.textBytes += len(text)
	if !s.textOpen {
		s.textOpen = true
		s.eventLocked("content_block_start", map[string]any{"index": s.index, "content_block": anthropicText("")})
	}
	s.eventLocked("content_block_delta", map[string]any{"index": s.index, "delta": map[string]string{"type": "text_delta", "text": text}})
}

func (s *anthropicStream) text(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.textLocked(text)
}

func (s *anthropicStream) closeBlockLocked() {
	s.eventLocked("content_block_stop", map[string]any{"index": s.index})
	s.index++
	s.textOpen = false
	s.textBytes = 0
}

func (s *anthropicStream) finish(blocks []anthropicBlock, reason string, sequence any, usage Usage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startLocked()
	for _, block := range blocks {
		if block.Text != nil {
			if !s.textOpen || len(*block.Text) > s.textBytes {
				s.textLocked((*block.Text)[s.textBytes:])
			}
			s.closeBlockLocked()
			continue
		}
		if s.textOpen {
			s.closeBlockLocked()
		}
		input := block.Input
		block.Input = json.RawMessage(`{}`)
		s.eventLocked("content_block_start", map[string]any{"index": s.index, "content_block": block})
		s.eventLocked("content_block_delta", map[string]any{"index": s.index, "delta": map[string]string{"type": "input_json_delta", "partial_json": string(input)}})
		s.closeBlockLocked()
	}
	if s.textOpen {
		s.closeBlockLocked()
	}
	s.eventLocked("message_delta", map[string]any{"delta": map[string]any{"stop_reason": reason, "stop_sequence": sequence}, "usage": anthropicUsage(usage)})
	s.eventLocked("message_stop", map[string]any{})
}
