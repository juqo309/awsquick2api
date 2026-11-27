package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAdminAccountAgentCRUDAndBulk(t *testing.T) {
	type record struct{ id, owner, name, description, welcome, instructions string }
	agents := map[string]record{"existing": {id: "existing", owner: "acc1", name: "Claude", welcome: "Hello"}, "other": {id: "other", owner: "acc1", name: "Desk", instructions: "Keep this"}}
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var env struct {
			Type string                    `json:"eventType"`
			Body map[string]map[string]any `json:"eventBody"`
		}
		if json.NewDecoder(r.Body).Decode(&env) != nil {
			http.Error(w, "invalid", 400)
			return
		}
		body := env.Body[env.Type]
		id := strAny(body["agentId"])
		agent, exists := agents[id]
		owner := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		raw := func(a record) map[string]any {
			return map[string]any{"agentId": a.id, "internalAgentId": "PUBLISHED-" + a.id, "agentStatus": "ACTIVE", "agentLifecycle": "PUBLISHED", "version": 1, "name": a.name, "description": a.description, "welcomeMessage": a.welcome, "customPromptInterface": map[string]any{"customInstructions": a.instructions}}
		}
		result := map[string]any{}
		switch env.Type {
		case "searchAgents":
			rows := []any{}
			for _, a := range agents {
				if a.owner == owner {
					rows = append(rows, raw(a))
				}
			}
			result["agentSummaries"] = rows
		case "createAgent":
			if exists || id == "" {
				http.Error(w, "invalid id", 400)
				return
			}
			agents[id] = record{id: id, owner: owner, name: strAny(body["name"])}
			result["agentId"] = id
		case "describeAgent", "updateAgent", "deleteAgent":
			if !exists || agent.owner != owner {
				http.Error(w, "not found", 404)
				return
			}
			if env.Type == "describeAgent" {
				result["agent"] = raw(agent)
			}
			if env.Type == "updateAgent" {
				if v, ok := body["name"].(string); ok {
					agent.name = v
				}
				if v, ok := body["description"].(string); ok {
					agent.description = v
				}
				if v, ok := body["welcomeMessage"].(string); ok {
					agent.welcome = v
				}
				if prompt := asMap(asMap(body["customPromptInput"])["newPrompt"]); prompt != nil {
					agent.instructions = strAny(prompt["customInstructions"])
				}
				agents[id] = agent
			}
			if env.Type == "deleteAgent" {
				delete(agents, id)
			}
		default:
			http.Error(w, "unknown event", 400)
			return
		}
		writeJSON(w, 200, map[string]any{"eventResponse": map[string]any{env.Type + "Response": result}})
	}))
	defer ts.Close()
	dir := t.TempDir()
	srv := NewServer(&Config{AccountsFile: filepath.Join(dir, "accounts.json"), StatsFile: filepath.Join(dir, "stats.json"), Models: []string{"smart"}, DefaultModel: "smart", Timeout: time.Minute})
	for _, id := range []string{"acc1", "acc2"} {
		a := fixtureAccount(id, time.Now().Add(time.Hour).Unix())
		a.Session.TenantURL = ts.URL
		a.Session.JWT = id
		srv.accounts.data.Accounts = append(srv.accounts.data.Accounts, a)
	}
	call := func(method, path string, body any, out any) {
		t.Helper()
		b, _ := json.Marshal(body)
		r := httptest.NewRequest(method, "/admin/accounts/"+path, bytes.NewReader(b))
		r.RemoteAddr = "127.0.0.1:9"
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
		}
		if out != nil {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatal(err)
			}
		}
	}
	post := func(action, id string, fields map[string]any) string {
		t.Helper()
		if fields == nil {
			fields = map[string]any{}
		}
		fields["action"], fields["id"], fields["agent_id"] = action, "acc1", id
		var out map[string]string
		call("POST", "agent", fields, &out)
		return out["agent_id"]
	}
	var list struct {
		Agents   []publicAgent `json:"agents"`
		Selected string        `json:"selected_id"`
		Complete bool          `json:"complete"`
	}
	first := post("create", "", map[string]any{"name": "Test", "instructions": "Old"})
	second := post("create", "", map[string]any{"name": "Test 2"})
	if first == "" || first == second || srv.chatAgentFor("acc1") != "" {
		t.Fatal("creation selected or duplicated an agent")
	}
	call("GET", "agent?id=acc1", nil, &list)
	if len(list.Agents) != 4 || !list.Complete {
		t.Fatalf("list: %+v", list)
	}
	post("select", first, nil)
	post("update", second, map[string]any{"name": "Renamed", "instructions": "New", "welcome_message": "Greetings"})
	var detail publicAgent
	call("GET", "agent?id=acc1&agent_id="+second, nil, &detail)
	if detail.Name != "Renamed" || detail.Instructions != "New" || detail.Welcome != "Greetings" || srv.chatAgentFor("acc1") != first {
		t.Fatalf("edit: %+v", detail)
	}
	post("delete", second, nil)
	if srv.chatAgentFor("acc1") != first {
		t.Fatal("deleting another agent changed selection")
	}
	post("delete", first, nil)
	if srv.chatAgentFor("acc1") != "" {
		t.Fatal("deleting the selected agent did not clear it")
	}
	post("select", "other", nil)
	var bulk struct {
		Results []claudeApplyResult `json:"results"`
	}
	call("POST", "agents/apply-claude", map[string]string{"instructions": defaultSystemPrompt}, &bulk)
	if len(bulk.Results) != 2 || bulk.Results[0].Updated != 1 || bulk.Results[1].Created != 1 || len(bulk.Results[0].Errors)+len(bulk.Results[1].Errors) != 0 {
		t.Fatalf("apply: %+v", bulk)
	}
	if srv.chatAgentFor("acc1") != "other" || srv.chatAgentFor("acc2") == "" {
		t.Fatal("bulk selection was wrong")
	}
	call("GET", "agent?id=acc1&agent_id=existing", nil, &detail)
	if detail.Welcome != "Hello" || detail.Instructions != defaultSystemPrompt {
		t.Fatalf("bulk modified welcome or did not save instructions: %+v", detail)
	}
	call("POST", "agents/apply-claude", map[string]string{"instructions": defaultSystemPrompt}, &bulk)
	if bulk.Results[0].Unchanged != 1 || bulk.Results[1].Unchanged != 1 {
		t.Fatalf("apply should be idempotent: %+v", bulk)
	}
}

func TestChatAgentIDSurvivesReimport(t *testing.T) {
	store := &AccountStore{path: filepath.Join(t.TempDir(), "accounts.json")}
	dir := t.TempDir()
	a := fixtureAccount("builder", time.Now().Add(time.Hour).Unix())
	a.ChatAgentID = "keep-me"
	a.Upstream = json.RawMessage(`{"user_info":{"email":"builder@example.test"},"tokens":{"refresh_token":"quick-only"}}`)
	if err := a.WriteWorkDir(dir); err != nil {
		t.Fatal(err)
	}
	first, err := store.ImportLogin(dir, "", "", "builder@example.test", "Dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetChatAgentID(first.ID, "keep-me"); err != nil {
		t.Fatal(err)
	}
	second, err := store.ImportLogin(dir, "", "", "builder@example.test", "Other")
	if err != nil || second.ChatAgentID != "keep-me" {
		t.Fatalf("agent id not preserved: %+v %v", second, err)
	}
}
