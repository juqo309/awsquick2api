package proxy

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type claudeApplyResult struct {
	ID        string   `json:"id"`
	Created   int      `json:"created"`
	Updated   int      `json:"updated"`
	Unchanged int      `json:"unchanged"`
	Selected  string   `json:"selected_id,omitempty"`
	Partial   bool     `json:"partial,omitempty"`
	Errors    []string `json:"errors,omitempty"`
}

func (s *Server) adminApplyClaude(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var body struct {
		Instructions *string `json:"instructions"`
	}
	if !decodeAgentJSON(w, r, &body) {
		return
	}
	if body.Instructions == nil {
		agentError(w, 400, fmt.Errorf("instructions are required"))
		return
	}
	instructions := strings.TrimSpace(*body.Instructions)
	if instructions == "" {
		instructions = defaultSystemPrompt
	}
	if err := (agentFields{Instructions: &instructions}).validate(); err != nil {
		agentError(w, 400, err)
		return
	}
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	accounts := s.accounts.Snapshot()
	if len(accounts) == 0 {
		agentError(w, 400, fmt.Errorf("connect an account first"))
		return
	}
	if err := s.settings.saveClaudeInstructions(instructions); err != nil {
		agentError(w, 500, fmt.Errorf("could not save Claude instructions"))
		return
	}
	results := make([]claudeApplyResult, len(accounts))
	var wg sync.WaitGroup
	for worker := 0; worker < min(len(accounts), 4); worker++ {
		wg.Add(1)
		go func(first int) {
			defer wg.Done()
			for i := first; i < len(accounts); i += min(len(accounts), 4) {
				ctx, cancel := context.WithTimeout(r.Context(), 75*time.Second)
				results[i] = s.applyClaudeToAccount(ctx, accounts[i], instructions)
				cancel()
			}
		}(worker)
	}
	wg.Wait()
	writeJSON(w, 200, map[string]any{"results": results, "claude_instructions": instructions, "full_context": false})
}

func (s *Server) applyClaudeToAccount(ctx context.Context, acc Account, instructions string) claudeApplyResult {
	result := claudeApplyResult{ID: acc.ID, Selected: acc.ChatAgentID}
	fail := func(err error) { result.Errors = append(result.Errors, err.Error()) }
	agents, complete, err := listAccountAgents(ctx, acc)
	result.Partial = !complete
	if err != nil {
		fail(err)
		return result
	}
	var selectedClaude string
	found := false
	for _, agent := range agents {
		if agent.Error != "" {
			if agent.ID == acc.ChatAgentID || strings.EqualFold(strings.TrimSpace(agent.Name), claudeAgentName) || agent.Name == "" {
				fail(fmt.Errorf("could not load agent %s: %s", agent.ID, agent.Error))
			}
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(agent.Name), claudeAgentName) {
			continue
		}
		found = true
		fresh, _, err := describeClaudeAgent(ctx, &acc.Session, acc.Proxy, agent.ID)
		if err != nil {
			fail(fmt.Errorf("load Claude agent %s: %w", agent.ID, err))
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(fresh.Name), claudeAgentName) {
			continue
		}
		if fresh.Instructions == instructions {
			result.Unchanged++
		} else if err := updateAccountAgent(ctx, &acc.Session, acc.Proxy, agent.ID, agentFields{Instructions: &instructions}); err != nil {
			fail(fmt.Errorf("Claude agent %s: %w", agent.ID, err))
			continue
		} else {
			result.Updated++
		}
		if fresh.Status == "ACTIVE" && fresh.Lifecycle == "PUBLISHED" && (selectedClaude == "" || acc.ChatAgentID == agent.ID) {
			selectedClaude = agent.ID
		}
	}
	if !found && len(result.Errors) == 0 {
		name, description := claudeAgentName, "General-purpose assistant"
		id, err := createAccountAgent(ctx, acc, agentFields{Name: &name, Description: &description, Instructions: &instructions}, s.accounts)
		if err != nil {
			fail(err)
			return result
		}
		selectedClaude = id
		result.Created++
	}
	// Preserve a deliberately selected non-Claude agent. If no agent is selected,
	// use an active Claude agent after its instructions have been verified.
	if selectedClaude != "" && acc.ChatAgentID == "" {
		if err := s.accounts.SetChatAgentID(acc.ID, selectedClaude); err != nil {
			fail(err)
		} else {
			result.Selected = selectedClaude
		}
	}
	return result
}
