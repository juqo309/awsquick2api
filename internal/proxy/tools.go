package proxy

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"unicode"
)

const clientToolPreamble = `This API turn has client-side tools. Use the declared client tools for the user's local files and commands. Tool result messages report actions executed by the client.`

const clientToolEnvelope = `To invoke a client tool, return {"tool_calls":[{"name":"declared_tool","input":{}}]} using the exact name and argument schema. The API parses this as a tool call and the client executes it. Wait for the result before reporting success. For a final reply, use ordinary text.`

// harnessCompactPrompt stays in system_prompt when a large client prompt cannot
// fit on the wire. It must not replace the client with a Claude persona.
const harnessCompactPrompt = `Follow the client's instructions for the current task. The client handles workspace actions through declared tools and reports their results.`

const invalidClientToolReply = "AWS Quick returned an invalid or undeclared client tool call"

func applyTurnIdentity(prompt string, fullContext, clientTurn bool) string {
	if fullContext || clientTurn {
		return prompt
	}
	return withTurnIdentity(prompt)
}

type clientToolSpec struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema,omitempty"`
}

func formatClientToolPrompt(specs []clientToolSpec, extra string) string {
	return clientToolPreamble + " " + clientToolEnvelope + extra + "\n\nDeclared tool schemas: " + string(compactClientCatalog(specs))
}

func compactClientCatalog(specs []clientToolSpec) []byte {
	args, _ := marshalWireJSON(specs)
	encoded, _ := marshalWireJSON(string(args))
	// Leave room for the active turn and client instructions in Quick's frame.
	if len(encoded) > 32*1024 {
		compact := append([]clientToolSpec(nil), specs...)
		for i, spec := range compact {
			compact[i].Description = shortToolDescription(spec.Description, 256)
			raw, _ := json.Marshal(spec.InputSchema)
			decoder := json.NewDecoder(strings.NewReader(string(raw)))
			decoder.UseNumber()
			var schema any
			if decoder.Decode(&schema) == nil {
				compactToolSchema(schema)
				compact[i].InputSchema = schema
			}
		}
		args = fitToolCatalog(compact)
	}
	return args
}

// Limit the escaped catalog independently of the transcript. Prefer full
// schemas for ordinary tools and shorten the largest definitions first.
func fitToolCatalog(specs []clientToolSpec) []byte {
	const budget = 40 * 1024
	costs := make([]int, len(specs))
	bySize := make([]int, len(specs))
	total := 4 + len(specs)
	for i, spec := range specs {
		costs[i] = toolSpecSize(spec)
		total += costs[i]
		bySize[i] = i
	}
	if total > budget {
		sort.SliceStable(bySize, func(i, j int) bool { return costs[bySize[i]] > costs[bySize[j]] })
		for _, i := range bySize {
			outline := specs[i]
			outline.InputSchema = toolSchemaOutline(outline.InputSchema, 2)
			cost := toolSpecSize(outline)
			if cost < costs[i] {
				specs[i] = outline
				total -= costs[i] - cost
				costs[i] = cost
			}
			if total <= budget {
				break
			}
		}
	}
	if total > budget {
		sort.SliceStable(bySize, func(i, j int) bool { return costs[bySize[i]] > costs[bySize[j]] })
		for _, i := range bySize {
			// A name and synopsis still let the model identify the tool.
			specs[i].InputSchema = nil
			specs[i].Description = shortToolDescription(specs[i].Description, 96)
			total -= costs[i] - toolSpecSize(specs[i])
			if total <= budget {
				break
			}
		}
	}
	args, _ := marshalWireJSON(specs)
	return args
}

func toolSpecSize(spec clientToolSpec) int {
	raw, _ := json.Marshal(spec)
	encoded, _ := json.Marshal(string(raw))
	return len(encoded) - 2
}

func toolSchemaOutline(value any, depth int) any {
	schema, ok := value.(map[string]any)
	if !ok {
		return value
	}
	outline := map[string]any{}
	for _, key := range []string{"type", "required"} {
		if value, exists := schema[key]; exists {
			outline[key] = value
		}
	}
	if depth > 0 {
		if properties, ok := schema["properties"].(map[string]any); ok {
			fields := make(map[string]any, len(properties))
			for name, child := range properties {
				fields[name] = toolSchemaOutline(child, depth-1)
			}
			outline["properties"] = fields
		}
		if items, ok := schema["items"].(map[string]any); ok {
			outline["items"] = toolSchemaOutline(items, depth-1)
		}
	}
	return outline
}

func shortToolDescription(text string, limit int) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if len(line) > limit {
		line = strings.ToValidUTF8(line[:limit], "")
	}
	return line
}

// Compact annotations only at schema nodes. Property names and values in
// keywords such as enum, const and default are application data.
func compactToolSchema(value any) {
	schema, ok := value.(map[string]any)
	if !ok {
		return
	}
	for key, child := range schema {
		switch key {
		case "description":
			if text, ok := child.(string); ok {
				schema[key] = shortToolDescription(text, 96)
			}
		case "$comment", "examples", "title":
			delete(schema, key)
		case "properties", "patternProperties", "$defs", "definitions", "dependentSchemas", "dependencies":
			if nodes, ok := child.(map[string]any); ok {
				for _, node := range nodes {
					compactToolSchema(node)
				}
			}
		case "allOf", "anyOf", "oneOf", "prefixItems", "items":
			if nodes, ok := child.([]any); ok {
				for _, node := range nodes {
					compactToolSchema(node)
				}
			} else {
				compactToolSchema(child)
			}
		case "additionalProperties", "additionalItems", "unevaluatedProperties", "unevaluatedItems", "contains", "propertyNames", "not", "if", "then", "else":
			compactToolSchema(child)
		}
	}
}

func parseClientToolCalls(text string, allowed map[string]bool, parallel bool) []ToolCall {
	raw := extractToolJSON(text)
	if raw == "" || len(allowed) == 0 {
		return nil
	}
	var envelope struct {
		ToolCalls []struct {
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			Arguments json.RawMessage `json:"arguments"`
			Function  struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if json.Unmarshal([]byte(raw), &envelope) != nil {
		return nil
	}
	out := make([]ToolCall, 0, len(envelope.ToolCalls))
	for _, tool := range envelope.ToolCalls {
		name := tool.Name
		if name == "" {
			name = tool.Function.Name
		}
		if !allowed[name] {
			// Some coding prompts present the same declared names under the
			// functions namespace. Never resolve an undeclared base name.
			name = strings.TrimPrefix(name, "functions.")
			if !allowed[name] {
				continue
			}
		}
		args := tool.Input
		if len(args) == 0 || string(args) == "null" {
			args = tool.Arguments
		}
		if len(args) == 0 || string(args) == "null" {
			args = tool.Function.Arguments
		}
		var encoded string
		if json.Unmarshal(args, &encoded) == nil && encoded != "" {
			args = json.RawMessage(encoded)
		}
		var input map[string]json.RawMessage
		if json.Unmarshal(args, &input) != nil || input == nil {
			continue
		}
		var body bytes.Buffer
		if json.Compact(&body, args) != nil {
			continue
		}
		out = append(out, ToolCall{
			ID:   "toolu_" + strings.ReplaceAll(uuid(), "-", ""),
			Type: "function",
			Function: FunctionCall{
				Name:      name,
				Arguments: body.String(),
			},
		})
		if !parallel {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func extractToolJSON(text string) string {
	s := strings.TrimSpace(text)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		if len(s) >= 4 && strings.EqualFold(s[:4], "json") {
			s = s[4:]
		}
		s = strings.TrimLeftFunc(s, unicode.IsSpace)
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = s[:i]
		}
		s = strings.TrimSpace(s)
	}
	if looksLikeToolJSON(s) {
		return s
	}
	for i := strings.IndexByte(s, '{'); i >= 0; i = strings.IndexByte(s, '{') {
		s = s[i:]
		if toolJSONStart(s) {
			if raw := matchJSONObject(s); looksLikeToolJSON(raw) {
				return raw
			}
		}
		s = s[1:]
	}
	return ""
}

func looksLikeToolJSON(s string) bool {
	if s == "" || !strings.Contains(s, `"tool_calls"`) {
		return false
	}
	var probe struct {
		ToolCalls json.RawMessage `json:"tool_calls"`
	}
	return json.Unmarshal([]byte(s), &probe) == nil && len(probe.ToolCalls) > 0
}

func matchJSONObject(s string) string {
	depth := 0
	inString := false
	escape := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			if escape {
				escape = false
				continue
			}
			if c == '\\' {
				escape = true
				continue
			}
			if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[:i+1]
			}
		}
	}
	return ""
}

// toolStreamGate streams prose while withholding a tool envelope, including
// when Quick emits an introduction or splits the envelope across deltas.
type toolStreamGate struct {
	buf  strings.Builder
	tool bool
}

func (g *toolStreamGate) Push(delta string) string {
	g.buf.WriteString(delta)
	if g.tool {
		return ""
	}
	s := g.buf.String()
	g.buf.Reset()
	for offset := 0; offset < len(s); {
		i := strings.IndexAny(s[offset:], "{`")
		if i < 0 {
			break
		}
		i += offset
		g.tool = toolJSONStart(s[i:])
		if g.tool || holdingToolPrefix(s[i:]) {
			g.buf.WriteString(s[i:])
			return s[:i]
		}
		offset = i + 1
	}
	return s
}

func (g *toolStreamGate) FlushText() string {
	if g.tool {
		return ""
	}
	s := g.buf.String()
	g.buf.Reset()
	return s
}

func (g *toolStreamGate) Held() string { return g.buf.String() }

func (g *toolStreamGate) Tool() bool { return g.tool }

func toolJSONStart(s string) bool {
	s = strings.TrimLeftFunc(s, unicode.IsSpace)
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "```") {
		rest := s[3:]
		if len(rest) >= 4 && strings.EqualFold(rest[:4], "json") {
			rest = rest[4:]
		}
		rest = strings.TrimLeftFunc(rest, unicode.IsSpace)
		return rest != "" && toolJSONStart(rest)
	}
	if !strings.HasPrefix(s, "{") {
		return false
	}
	rest := strings.TrimLeftFunc(s[1:], unicode.IsSpace)
	return strings.HasPrefix(rest, `"tool_calls"`)
}

func holdingToolPrefix(s string) bool {
	s = strings.TrimLeftFunc(s, unicode.IsSpace)
	if s == "" {
		return true
	}
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "```") {
		body := s[3:]
		if len(body) >= 4 && strings.EqualFold(body[:4], "json") {
			body = body[4:]
		} else if strings.HasPrefix("json", strings.ToLower(body)) {
			return true
		} else if body != "" && !strings.HasPrefix(strings.ToLower(body), "json") && !unicode.IsSpace(rune(body[0])) && body[0] != '{' {
			return false
		}
		body = strings.TrimLeftFunc(body, unicode.IsSpace)
		return body == "" || holdingToolPrefix(body) || toolJSONStart(body)
	}
	if strings.HasPrefix("```json", lower) {
		return true
	}
	if s[0] != '{' {
		return false
	}
	rest := strings.TrimLeftFunc(s[1:], unicode.IsSpace)
	if rest == "" {
		return true
	}
	const key = `"tool_calls"`
	return len(rest) < len(key) && strings.HasPrefix(key, rest)
}
