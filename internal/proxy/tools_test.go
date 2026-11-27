package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestApplyTurnIdentity(t *testing.T) {
	prompt := "Read /tmp/x"
	if got := applyTurnIdentity(prompt, false, true); got != prompt {
		t.Fatalf("client turn: %q", got)
	}
	if got := applyTurnIdentity(prompt, true, false); got != prompt {
		t.Fatalf("full context was wrapped: %q", got)
	}
	if got := applyTurnIdentity(prompt, false, false); !strings.HasPrefix(got, turnIdentityPrefix) || !strings.Contains(got, prompt) {
		t.Fatalf("dashboard turn was not wrapped: %q", got)
	}
}

func TestLargeHarnessPromptRetainsToolSchema(t *testing.T) {
	const systemStart = "Follow the harness instructions. "
	system := systemStart + strings.Repeat("Very long client instructions. ", 3000)
	const schema = `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`
	tools := formatClientToolPrompt([]clientToolSpec{{
		Name: "lookup", Description: "Look up a file.\n" + strings.Repeat("Verbose help. ", 3000), InputSchema: json.RawMessage(schema),
	}}, " You must request a tool call.")
	payload := map[string]any{"text": "Look it up.", "system_prompt": system, "custom_prompt": system}
	if err := packUserMessage(context.Background(), nil, "", "thread", "msg", "Look it up.", payload, tools); err != nil {
		t.Fatal(err)
	}
	custom := payload["custom_prompt"].(string)
	for _, expected := range []string{"Look up a file.", `"name":"lookup"`, `"path":{"type":"string"}`, `"required":["path"]`, "You must request a tool call.", clientToolEnvelope} {
		if !strings.Contains(custom, expected) {
			t.Errorf("packed tool instructions missing %q", expected)
		}
	}
	if payload["text"] != "Look it up." || userMessageSize(payload) > upstreamFrameLimit {
		t.Fatalf("packing changed the current turn or exceeded the frame: size=%d", userMessageSize(payload))
	}
}

func TestLongHarnessHistoryAndNestedToolDescriptions(t *testing.T) {
	const task = "User: update settings.json and check the result"
	const result = "Tool result (call_id=toolu_1, name=read): current settings"
	var specs []clientToolSpec
	for _, name := range []string{"read", "edit", "bash", "write"} {
		specs = append(specs, clientToolSpec{Name: name, InputSchema: map[string]any{
			"type": "object", "required": []string{"description"},
			"properties": map[string]any{"description": map[string]any{
				"type": "string", "description": "Content to save.\n" + strings.Repeat("Long nested argument help. ", 1600),
				"examples": []string{"VERBOSE EXAMPLE"},
				"const":    "literal value",
			}},
		}})
	}
	tools := formatClientToolPrompt(specs, "")
	text := strings.Repeat("Earlier transcript contents.\n", 22000) + "\n\n" + task + "\n\n" + result
	payload := map[string]any{"text": text, "system_prompt": "Client instructions."}
	if err := packUserMessage(context.Background(), nil, "", "thread", "msg", text, payload, tools); err != nil {
		t.Fatal(err)
	}
	if packed := payload["text"].(string); !strings.Contains(packed, textTrimMarker) || !strings.HasSuffix(packed, task+"\n\n"+result) || payload["attachments"] != nil || userMessageSize(payload) > upstreamFrameLimit {
		t.Fatalf("failed to pack the active turn inline: size=%d", userMessageSize(payload))
	}
	for _, expected := range []string{`"name":"edit"`, `"name":"bash"`, `"required":["description"]`, `"const":"literal value"`, "Content to save."} {
		if !strings.Contains(payload["custom_prompt"].(string), expected) {
			t.Errorf("packed catalog is missing %q", expected)
		}
	}
	if strings.Contains(payload["custom_prompt"].(string), "VERBOSE EXAMPLE") {
		t.Error("packed catalog retained verbose examples")
	}
}

func TestOversizedStructuralToolCatalogFits(t *testing.T) {
	local := clientToolSpec{Name: "edit", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)}
	values := make([]string, 6000)
	for i := range values {
		values[i] = fmt.Sprintf("structural-enum-value-%d", i)
	}
	large := clientToolSpec{Name: "other_tool", InputSchema: map[string]any{
		"type": "object", "required": []string{"code"}, "properties": map[string]any{"code": map[string]any{"type": "string", "enum": values}},
	}}
	tools := formatClientToolPrompt([]clientToolSpec{local, large}, "")
	prompt := strings.Repeat("Earlier unrelated messages.\n", 24000) + "\n\nUser: update settings.json and check the result"
	payload := map[string]any{"text": prompt, "system_prompt": "Client instructions."}
	if err := packUserMessage(context.Background(), nil, "", "thread", "msg", prompt, payload, tools); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"name":"edit"`, `"additionalProperties":false`, `"name":"other_tool"`, `"code":{"type":"string"}`, `"required":["code"]`} {
		if !strings.Contains(payload["custom_prompt"].(string), want) {
			t.Errorf("tool catalog lost %s", want)
		}
	}
	if !strings.HasSuffix(payload["text"].(string), "User: update settings.json and check the result") || userMessageSize(payload) > upstreamFrameLimit {
		t.Fatal("frame lost the current task or exceeded the Quick limit")
	}
}

func TestDeniesClientTools(t *testing.T) {
	if !deniesClientTools("I can’t reach your computer’s files from this chat.") || !deniesClientTools("I won't make tool calls outside the tools I actually have.") {
		t.Fatal("missed a client tool capability denial")
	}
	if deniesClientTools("I can't help with that request.") || deniesClientTools("The read tool reported that I can't access the file.") {
		t.Fatal("matched an unrelated refusal or a real tool failure")
	}
}

func TestParseClientToolCalls(t *testing.T) {
	allowed := map[string]bool{"bash": true, "read": true}
	calls := parseClientToolCalls("I'll check.\n```json\n{\"tool_calls\":[{\"name\":\"read\",\"input\":{\"path\":\"/tmp/x\"}},{\"name\":\"bash\",\"input\":{\"command\":\"ls\"}}]}\n```", allowed, true)
	if len(calls) != 2 || calls[0].Function.Name != "read" || !strings.Contains(calls[0].Function.Arguments, "/tmp/x") || calls[1].Function.Name != "bash" {
		t.Fatalf("fenced: %+v", calls)
	}
	calls = parseClientToolCalls(`Sure. {"tool_calls":[{"name":"bash","arguments":{"command":"pwd"}}]}`, allowed, true)
	if len(calls) != 1 || calls[0].Function.Name != "bash" || !strings.Contains(calls[0].Function.Arguments, "pwd") {
		t.Fatalf("embedded: %+v", calls)
	}
	calls = parseClientToolCalls(`{"tool_calls":[{"name":"bash","input":{"command":"a"}},{"name":"read","input":{"path":"b"}}]}`, allowed, false)
	if len(calls) != 1 || calls[0].Function.Name != "bash" {
		t.Fatalf("no parallel: %+v", calls)
	}
	calls = parseClientToolCalls(`{"tool_calls":[{"type":"function","function":{"name":"functions.read","arguments":"{\"path\":\"/tmp/x\",\"offset\":9007199254740993}"}}]}`, allowed, true)
	if len(calls) != 1 || calls[0].Function.Name != "read" || calls[0].Function.Arguments != `{"path":"/tmp/x","offset":9007199254740993}` {
		t.Fatalf("nested function arguments: %+v", calls)
	}
	if parseClientToolCalls(`{"tool_calls":[{"name":"functions.missing","input":{}}]}`, allowed, true) != nil {
		t.Fatal("undeclared tool")
	}
}

func TestToolStreamGate(t *testing.T) {
	var g toolStreamGate
	if got := g.Push("Hel"); got != "Hel" || g.Tool() {
		t.Fatalf("prose: %q tool=%v", got, g.Tool())
	}
	if got := g.Push("lo"); got != "lo" {
		t.Fatalf("follow-on prose: %q", got)
	}
	g = toolStreamGate{}
	if got := g.Push(`{"`); got != "" || g.Tool() {
		t.Fatalf("prefix: %q tool=%v", got, g.Tool())
	}
	if got := g.Push(`tool_calls":[{"name":"read","input":{"path":"a"}}]}`); got != "" || !g.Tool() {
		t.Fatalf("json: %q tool=%v held=%q", got, g.Tool(), g.Held())
	}
	g = toolStreamGate{}
	if got := g.Push(`{"foo":1}`); got != `{"foo":1}` || g.Tool() {
		t.Fatalf("other json: %q tool=%v", got, g.Tool())
	}
	g = toolStreamGate{}
	if got := g.Push("```json\n{\"tool_calls\":[]}"); got != "" || !g.Tool() {
		t.Fatalf("fence: %q tool=%v", got, g.Tool())
	}
}
