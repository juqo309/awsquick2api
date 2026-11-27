package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	claudeAgentName      = "Claude"
	maxAgentInstructions = 5000
)

type publicAgent struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Welcome      string `json:"welcome_message"`
	Instructions string `json:"instructions"`
	Status       string `json:"status"`
	Lifecycle    string `json:"lifecycle"`
	Version      int    `json:"version"`
	CreatedAt    int64  `json:"created_at,omitempty"`
	UpdatedAt    int64  `json:"updated_at,omitempty"`
	Error        string `json:"error,omitempty"`
}

func (s *Server) chatAgentFor(accountID string) string {
	if accountID == "" || s.accounts == nil {
		return ""
	}
	acc, err := s.accounts.Get(accountID)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(acc.ChatAgentID)
}

type agentFields struct {
	Name         *string `json:"name"`
	Description  *string `json:"description"`
	Welcome      *string `json:"welcome_message"`
	Instructions *string `json:"instructions"`
}

func (f agentFields) validate() error {
	if f.Name != nil && (strings.TrimSpace(*f.Name) == "" || len(*f.Name) > 80) {
		return fmt.Errorf("name must be 1–80 bytes")
	}
	for _, x := range []struct {
		value *string
		label string
		limit int
	}{{f.Description, "description", 500}, {f.Welcome, "welcome message", 500}, {f.Instructions, "instructions", maxAgentInstructions}} {
		if x.value != nil && len(*x.value) > x.limit {
			return fmt.Errorf("%s must be at most %d bytes", x.label, x.limit)
		}
	}
	return nil
}

func agentError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func decodeAgentJSON(w http.ResponseWriter, r *http.Request, dest any) bool {
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if site := r.Header.Get("Sec-Fetch-Site"); media != "application/json" || site != "" && site != "same-origin" {
		agentError(w, 403, fmt.Errorf("submit changes from this dashboard"))
		return false
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<17))
	if d.Decode(dest) != nil || d.Decode(new(json.RawMessage)) != io.EOF {
		agentError(w, 400, fmt.Errorf("invalid JSON"))
		return false
	}
	return true
}

func (s *Server) adminAccountAgent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), 55*time.Second)
	defer cancel()
	if r.Method == http.MethodGet {
		acc, err := s.accounts.Get(r.URL.Query().Get("id"))
		if err != nil {
			agentError(w, 400, err)
			return
		}
		if id := strings.TrimSpace(r.URL.Query().Get("agent_id")); id != "" {
			agent, _, err := describeClaudeAgent(ctx, &acc.Session, acc.Proxy, id)
			if err != nil {
				agentError(w, 502, err)
				return
			}
			writeJSON(w, 200, agent)
			return
		}
		agents, complete, err := listAccountAgents(ctx, acc)
		if err != nil {
			agentError(w, 502, err)
			return
		}
		writeJSON(w, 200, map[string]any{"agents": agents, "selected_id": acc.ChatAgentID, "complete": complete, "claude_instructions": s.settings.snapshot().claudeInstructions()})
		return
	}
	var body struct {
		ID      string `json:"id"`
		Action  string `json:"action"`
		AgentID string `json:"agent_id"`
		agentFields
	}
	if !decodeAgentJSON(w, r, &body) {
		return
	}
	if err := body.validate(); err != nil {
		agentError(w, 400, err)
		return
	}
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	acc, err := s.accounts.Get(body.ID)
	if err != nil {
		agentError(w, 400, err)
		return
	}
	id := strings.TrimSpace(body.AgentID)
	status := 502
	switch body.Action {
	case "select":
		if id != "" {
			var agent publicAgent
			agent, _, err = describeClaudeAgent(ctx, &acc.Session, acc.Proxy, id)
			if err == nil && (agent.Status != "ACTIVE" || agent.Lifecycle != "PUBLISHED") {
				agentError(w, 400, fmt.Errorf("only active, published agents can be selected"))
				return
			}
		}
		if err == nil {
			err, status = s.accounts.SetChatAgentID(acc.ID, id), 500
		}
	case "create":
		if body.Name == nil {
			agentError(w, 400, fmt.Errorf("name is required"))
			return
		}
		id, err = createAccountAgent(ctx, acc, body.agentFields, s.accounts)
	case "update", "delete":
		if id == "" {
			agentError(w, 400, fmt.Errorf("agent_id is required"))
			return
		}
		if body.Action == "update" {
			err = updateAccountAgent(ctx, &acc.Session, acc.Proxy, id, body.agentFields)
		} else {
			err = deleteClaudeAgent(ctx, &acc.Session, acc.Proxy, id)
			if err == nil {
				err, status = s.accounts.trackAgent(acc.ID, id, false), 500
			}
		}
	default:
		agentError(w, 400, fmt.Errorf("unknown agent action"))
		return
	}
	if err != nil {
		agentError(w, status, err)
		return
	}
	writeJSON(w, 200, map[string]string{"agent_id": id})
}

func listAccountAgents(ctx context.Context, acc Account) ([]publicAgent, bool, error) {
	// Start the fallback lookup alongside the native search. Some sessions
	// reject native search, so waiting to start the fallback costs a full
	// extra round trip even for an account with just one agent.
	type recentResult struct {
		data map[string]any
		err  error
	}
	recentCtx, cancelRecent := context.WithCancel(ctx)
	defer cancelRecent()
	recentLookup := make(chan recentResult, 1)
	go func() {
		data, err := quickEvent(recentCtx, &acc.Session, acc.Proxy, "listRecentAgents", map[string]any{})
		recentLookup <- recentResult{data: data, err: err}
	}()

	agents := []publicAgent{}
	seen := map[string]bool{}
	recent := map[string]publicAgent{}
	body := map[string]any{"maxResults": 100, "filterParams": map[string]any{"roleFilter": map[string]any{"role": "QUICKSIGHT_VIEWER"}}}
	complete := true
	var listErr error
	tokens := map[string]bool{}
	for {
		result, err := quickEvent(ctx, &acc.Session, acc.Proxy, "searchAgents", body)
		if err != nil || result == nil {
			complete = false
			listErr = err
			break
		}
		rows, _ := result["agentSummaries"].([]any)
		for _, row := range rows {
			a := parsePublicAgent(asMap(row), "")
			if a.ID != "" && !seen[a.ID] {
				seen[a.ID] = true
				agents = append(agents, a)
			}
		}
		token := strAny(result["nextToken"])
		if token == "" {
			break
		}
		if tokens[token] {
			return nil, false, fmt.Errorf("agent search repeated its pagination token")
		}
		tokens[token] = true
		body["nextToken"] = token
	}
	if !complete {
		fallback := <-recentLookup
		if fallback.err != nil {
			if listErr != nil {
				return nil, false, fmt.Errorf("list agents: %w", listErr)
			}
			return nil, false, fallback.err
		}
		rows, _ := fallback.data["agents"].([]any)
		for _, row := range rows {
			a := parsePublicAgent(asMap(row), "")
			if a.ID != "" {
				recent[a.ID] = a
			}
		}
	} else {
		cancelRecent()
	}
	for _, id := range append(append([]string{}, acc.ManagedAgentIDs...), acc.ChatAgentID) {
		if id != "" && !seen[id] {
			if _, ok := recent[id]; !ok {
				recent[id] = publicAgent{ID: id}
			}
		}
	}
	// Each fallback row needs a describe request for its status. Load them
	// concurrently so proxy round trips don't add up, with a per-list cap.
	pending := make([]publicAgent, 0, len(recent))
	for id, summary := range recent {
		if !seen[id] {
			pending = append(pending, summary)
		}
	}
	workers := min(4, len(pending))
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(start int) {
			defer wg.Done()
			for i := start; i < len(pending); i += workers {
				summary := pending[i]
				agent, _, err := describeClaudeAgent(ctx, &acc.Session, acc.Proxy, summary.ID)
				if err != nil {
					pending[i].Error = err.Error()
				} else {
					pending[i] = agent
				}
			}
		}(worker)
	}
	wg.Wait()
	agents = append(agents, pending...)
	sort.Slice(agents, func(i, j int) bool {
		a, b := strings.ToLower(agents[i].Name), strings.ToLower(agents[j].Name)
		if a == b {
			return agents[i].ID < agents[j].ID
		}
		return a < b
	})
	return agents, complete, nil
}

func createAccountAgent(ctx context.Context, acc Account, f agentFields, store *AccountStore) (string, error) {
	name := strings.TrimSpace(*f.Name)
	id := uuid()
	result, err := quickEvent(ctx, &acc.Session, acc.Proxy, "createAgent", map[string]any{"agentId": id, "agentLifecycle": "PUBLISHED", "name": name})
	if err != nil {
		return "", fmt.Errorf("create agent: %w", err)
	}
	id = firstNonEmpty(strAny(result["agentId"]), id)
	if err = store.trackAgent(acc.ID, id, true); err != nil {
		return id, fmt.Errorf("agent %s was created but could not be saved locally: %w", id, err)
	}
	if f.Description != nil || f.Welcome != nil || f.Instructions != nil {
		if err = updateAccountAgent(ctx, &acc.Session, acc.Proxy, id, f); err != nil {
			return id, fmt.Errorf("agent %s was created but not fully configured: %w", id, err)
		}
	}
	return id, nil
}

func describeClaudeAgent(ctx context.Context, sess *Session, proxyURL, agentID string) (publicAgent, map[string]any, error) {
	result, err := quickEvent(ctx, sess, proxyURL, "describeAgent", map[string]any{"agentId": agentID})
	if err != nil {
		return publicAgent{ID: agentID}, nil, err
	}
	raw := asMap(result["agent"])
	if raw == nil {
		return publicAgent{ID: agentID}, nil, fmt.Errorf("agent was not returned")
	}
	return parsePublicAgent(raw, agentID), raw, nil
}

func parsePublicAgent(raw map[string]any, agentID string) publicAgent {
	return publicAgent{
		Instructions: strAny(asMap(raw["customPromptInterface"])["customInstructions"]),
		ID:           firstNonEmpty(strAny(raw["agentId"]), agentID),
		Name:         strAny(raw["name"]),
		Description:  strAny(raw["description"]),
		Welcome:      strAny(raw["welcomeMessage"]),
		Status:       strAny(raw["agentStatus"]),
		Lifecycle:    strAny(raw["agentLifecycle"]),
		Version:      jsonInt(raw["version"]),
		CreatedAt:    unixAny(raw["createdAt"]),
		UpdatedAt:    unixAny(raw["updatedAt"]),
	}
}

func updateAccountAgent(ctx context.Context, sess *Session, proxyURL, id string, f agentFields) error {
	agent, raw, err := describeClaudeAgent(ctx, sess, proxyURL, id)
	if err != nil {
		return fmt.Errorf("load agent: %w", err)
	}
	internal := strAny(raw["internalAgentId"])
	if internal == "" || agent.Version < 1 {
		return fmt.Errorf("agent is missing its internal id or version")
	}
	// Quick's editor sends the current profile fields even for an instructions-only change.
	// Preserve them rather than sending a sparse update or replacing the agent's greeting.
	starters, ok := raw["starterPrompts"].([]any)
	if !ok {
		starters = []any{"", "", ""}
	}
	update := map[string]any{
		"agentId": id, "internalAgentId": internal, "version": agent.Version,
		"name": agent.Name, "description": agent.Description, "welcomeMessage": agent.Welcome,
		"iconId": firstNonEmpty(strAny(raw["iconId"]), "id_agent_icon_ai"), "starterPrompts": starters,
	}
	if f.Name != nil {
		update["name"] = strings.TrimSpace(*f.Name)
	}
	if f.Description != nil {
		update["description"] = *f.Description
	}
	if f.Welcome != nil {
		update["welcomeMessage"] = *f.Welcome
	}
	if f.Instructions != nil {
		prompt := map[string]any{"customInstructions": *f.Instructions}
		if files, ok := asMap(raw["customPromptInterface"])["fileContextList"].([]any); ok && len(files) != 0 {
			refs := make([]map[string]any, 0, len(files))
			for _, file := range files {
				ref := asMap(file)
				refs = append(refs, map[string]any{"fileId": ref["fileId"], "fileMetadata": ref["fileMetadata"]})
			}
			prompt["fileContextList"] = refs
		}
		update["customPromptInput"] = map[string]any{"newPrompt": prompt}
	}
	if _, err = quickEvent(ctx, sess, proxyURL, "updateAgent", update); err != nil {
		return fmt.Errorf("update agent: %w", err)
	}
	if f.Instructions != nil {
		saved, _, err := describeClaudeAgent(ctx, sess, proxyURL, id)
		if err != nil {
			return fmt.Errorf("verify instructions: %w", err)
		}
		if saved.Instructions != *f.Instructions {
			return fmt.Errorf("upstream did not save the instructions")
		}
	}
	return nil
}

func deleteClaudeAgent(ctx context.Context, sess *Session, proxyURL, agentID string) error {
	_, raw, err := describeClaudeAgent(ctx, sess, proxyURL, agentID)
	if err != nil {
		return fmt.Errorf("load agent: %w", err)
	}
	internal := strAny(raw["internalAgentId"])
	if internal == "" {
		return fmt.Errorf("agent is missing an internal id")
	}
	if _, err := quickEvent(ctx, sess, proxyURL, "deleteAgent", map[string]any{
		"agentId":         agentID,
		"internalAgentId": internal,
	}); err != nil {
		return fmt.Errorf("delete agent: %w", err)
	}
	return nil
}

func quickEvent(ctx context.Context, sess *Session, proxyURL, eventType string, body map[string]any) (map[string]any, error) {
	base := attachBase(sess)
	if base == "" {
		return nil, fmt.Errorf("session has no tenant URL")
	}
	token := sess.jwt()
	if token == "" {
		return nil, fmt.Errorf("session has no token")
	}
	payload, err := json.Marshal(map[string]any{
		"eventType": eventType,
		"eventBody": map[string]any{eventType: body},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"qbs-event", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-amzn-qs-internal-trace-id", uuid())
	client := proxyHTTPClient(proxyURL, 20*time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	resp.Body.Close()
	var envelope map[string]any
	_ = json.Unmarshal(raw, &envelope)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(strAny(envelope["message"]))
		if msg == "" {
			msg = strings.TrimSpace(strAny(envelope["errorMessage"]))
		}
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("%s", msg)
	}
	if msg := strings.TrimSpace(strAny(envelope["errorMessage"])); msg != "" && !strings.EqualFold(msg, "null") {
		return nil, fmt.Errorf("%s", msg)
	}
	result := asMap(lookupAny(envelope, "eventResponse", eventType+"Response"))
	if result == nil {
		if er := asMap(envelope["eventResponse"]); er != nil {
			result = asMap(er[eventType+"Response"])
		}
	}
	if result == nil {
		return nil, fmt.Errorf("%s: upstream returned no response", eventType)
	}
	return result, nil
}

func jsonInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}

func unixAny(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		if n > 1e12 {
			return int64(n / 1000)
		}
		return int64(n)
	case json.Number:
		f, _ := n.Float64()
		return unixAny(f)
	default:
		return 0
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
