package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
	"unicode/utf8"
)

const harnessTag = "bash run"

type harnessTools struct {
	prompt  string
	catalog string
	allowed map[string]bool
	shell   string
	command string
}

func makeHarnessTools(specs []clientToolSpec, extra string, allowed map[string]bool) harnessTools {
	catalog := compactClientCatalog(specs)
	tools := harnessTools{catalog: string(catalog), allowed: allowed}
	for _, tool := range specs {
		switch strings.ToLower(tool.Name) {
		case "bash", "shell", "sh", "zsh", "terminal", "execute_command", "run_terminal_command", "run_command", "shell_command":
		default:
			continue
		}
		raw, _ := json.Marshal(tool.InputSchema)
		var schema struct {
			Properties map[string]struct{ Type string }
			Required   []string
		}
		if json.Unmarshal(raw, &schema) != nil {
			continue
		}
		for _, arg := range []string{"command", "cmd", "commands"} {
			if schema.Properties[arg].Type == "string" &&
				(len(schema.Required) == 0 || (len(schema.Required) == 1 && schema.Required[0] == arg)) {
				tools.shell, tools.command = tool.Name, arg
				break
			}
		}
		if tools.shell != "" {
			break
		}
	}

	tools.prompt = "The client application executes the declared tools for the user. " +
		"To request a declared tool, start a new line with exactly \"```bash run\" and close the block with three backticks. " +
		"Use the full tag bash run, including the space and the word run. Example format, with the actual tool name and arguments substituted:\n" +
		"```bash run\nharness ToolName <<'HARNESS_JSON'\n{}\nHARNESS_JSON\n```\n" +
		"The harness handles permissions and returns the result in the conversation. " +
		"Ordinary bash fences are for display. Use ordinary Markdown for final replies and examples." + extra
	if tools.shell != "" {
		tools.prompt += " Plain shell lines in a " + harnessTag + " fence request the declared " + tools.shell + " tool."
	}
	var names strings.Builder
	for _, spec := range specs {
		line := "\n- " + spec.Name
		if names.Len()+len(line) > 6000 {
			break
		}
		names.WriteString(line)
	}
	tools.prompt += "\nAvailable harness tools:" + names.String()
	return tools
}

var (
	harnessFence   = regexp.MustCompile(`^[ \t]{0,3}(` + "`{3,}" + `|~{3,})([^\n]*)`)
	harnessPrefix  = regexp.MustCompile(`^[ \t]{0,3}(?:` + "`{3}" + `|~{3}|` + "`{0,2}" + `$|~{1,2}$)`)
	harnessLine    = regexp.MustCompile(`^[ \t]*harness([ \t]|$)`)
	harnessHeredoc = regexp.MustCompile(`^[ \t]*harness[ \t]+([A-Za-z0-9_.:\-]+)[ \t]*<<-?[ \t]*(['"]?)([A-Za-z_][A-Za-z0-9_]*)(['"]?)[ \t]*$`)
	harnessInline  = regexp.MustCompile(`^[ \t]*harness[ \t]+([A-Za-z0-9_.:\-]+)[ \t]+(['"]?)(\{.*\})(['"]?)[ \t]*$`)
)

func (tools harnessTools) parse(text string) []anthropicBlock {
	var blocks []anthropicBlock
	var command []string
	add := func(name string, args []byte) bool {
		if !tools.allowed[name] {
			return false
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(args, &fields) != nil || fields == nil {
			return false
		}
		var compact bytes.Buffer
		if json.Compact(&compact, args) != nil {
			return false
		}
		blocks = append(blocks, anthropicBlock{
			Type: "tool_use", ID: "toolu_" + strings.ReplaceAll(uuid(), "-", ""), Name: name, Input: compact.Bytes(),
		})
		return true
	}
	flush := func() bool {
		script := strings.Trim(strings.Join(command, "\n"), "\n")
		command = nil
		if strings.TrimSpace(script) == "" {
			return true
		}
		if tools.shell == "" {
			return false
		}
		args, _ := json.Marshal(map[string]string{tools.command: script})
		return add(tools.shell, args)
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if !harnessLine.MatchString(line) {
			command = append(command, line)
			continue
		}
		if !flush() {
			return nil
		}
		if match := harnessHeredoc.FindStringSubmatch(line); match != nil && match[2] == match[4] {
			from := i + 1
			i = from
			for i < len(lines) && strings.TrimSpace(lines[i]) != match[3] {
				i++
			}
			if i == len(lines) || !add(match[1], []byte(strings.Join(lines[from:i], "\n"))) {
				return nil
			}
		} else if match := harnessInline.FindStringSubmatch(line); match == nil || match[2] != match[4] || !add(match[1], []byte(match[3])) {
			return nil
		}
	}
	if !flush() {
		return nil
	}
	return blocks
}

type harnessReply struct {
	tools     harnessTools
	single    bool
	emit      func(string)
	pending   string
	partial   bool
	fence     string
	fenceLine string
	body      strings.Builder
	text      strings.Builder
	blocks    []anthropicBlock
	hold      bool
	invalid   bool
	calls     int
}

func (r *harnessReply) prose(text string) {
	r.text.WriteString(text)
	if !r.hold && text != "" && r.emit != nil {
		r.emit(text)
	}
}

func (r *harnessReply) closeText() {
	if r.text.Len() != 0 {
		r.blocks = append(r.blocks, anthropicText(r.text.String()))
		r.text.Reset()
	}
}

func (r *harnessReply) line(line string) {
	m := harnessFence.FindStringSubmatch(line)
	if r.fence != "" {
		closing := m != nil && m[1][0] == r.fence[0] && len(m[1]) >= len(r.fence) && strings.TrimSpace(m[2]) == ""
		if r.fenceLine == "" {
			r.prose(line)
		} else if closing {
			calls := r.tools.parse(r.body.String())
			if len(calls) == 0 || (r.single && r.calls+len(calls) > 1) {
				r.invalid = true
				r.prose(r.fenceLine + r.body.String() + line)
			} else {
				r.closeText()
				r.blocks = append(r.blocks, calls...)
				r.calls += len(calls)
			}
			r.body.Reset()
			r.fenceLine = ""
		} else {
			r.body.WriteString(line)
		}
		if closing {
			r.fence = ""
		}
		return
	}
	if m != nil && (m[1][0] != '`' || !strings.ContainsRune(m[2], '`')) {
		r.fence = m[1]
		if strings.EqualFold(strings.Join(strings.Fields(m[2]), " "), harnessTag) {
			r.hold = true
			r.fenceLine = line
			return
		}
	}
	r.prose(line)
}

func (r *harnessReply) push(delta string) {
	if r.partial {
		part, rest, found := strings.Cut(delta, "\n")
		if !found {
			r.prose(part)
			return
		}
		r.prose(part + "\n")
		r.partial = false
		delta = rest
	}
	r.pending += delta
	for {
		line, rest, found := strings.Cut(r.pending, "\n")
		if !found {
			break
		}
		r.line(line + "\n")
		r.pending = rest
	}
	if r.pending != "" && r.fenceLine == "" && !harnessPrefix.MatchString(r.pending) {
		r.prose(r.pending)
		r.pending = ""
		r.partial = true
	}
}

func (r *harnessReply) finish() []anthropicBlock {
	if r.pending != "" {
		r.line(r.pending)
	}
	if r.fenceLine != "" {
		r.invalid = true
		r.prose(r.fenceLine + r.body.String())
	}
	r.closeText()
	return r.blocks
}

// harnessContext is the client's harness setup and conversation for one turn.
// Quick receives it as the text of a single user message.
type harnessContext struct {
	instructions string
	schemas      string
	conversation string
}

const harnessMinTail = 4 * 1024

func (c harnessContext) text(schemas, conversation string) string {
	return "# Harness instructions\n" + c.instructions + "\n\n# Declared harness tool schemas\n" + schemas + "\n\n# Conversation\n" + conversation
}

func harnessAttachmentName(messageID string) string {
	return strings.Replace(attachmentPromptName(messageID), "prompt-", "conversation-", 1)
}

func harnessHistoryNote(name string) string {
	return "[Earlier conversation history is archived in the attached file " + name + ". Only consult it if the recent conversation below is missing context you need. The most recent part of the conversation follows.]\n\n"
}

// harnessMessage fits the harness context into one Quick user message.
//
// Quick caps a user message at 64 KB. Splitting the context across several
// messages that ask the model to reply "OK" until the last part arrives looks
// like a staged prompt injection, so Quick's guardrail blocks those turns even
// when the conversation is benign. Instead, oversized context is moved into an
// uploaded text attachment, in this order: older conversation, then tool
// schemas, then everything. The latest messages stay inline whenever possible,
// so the model sees the current request and tool results without opening a file.
func harnessMessage(payload map[string]any, c harnessContext, name string, upload func(name, text string) (Attachment, error)) error {
	payload["text"] = c.text(c.schemas, c.conversation)
	if userMessageSize(payload) <= upstreamFrameLimit {
		return nil
	}
	note := harnessHistoryNote(name)
	placeholder := Attachment{Type: "file", Name: name, Size: 1 << 30, MIMEType: "text/plain", Path: strings.Repeat("p", 320), AttachmentID: strings.Repeat("a", 64), ContentSHA256: strings.Repeat("0", 64)}
	appendPayloadAttachment(payload, placeholder)
	restore := payload["attachments"]
	fitTail := func(prefix string) int {
		lo, hi := 0, len(c.conversation)
		for lo < hi {
			mid := (lo + hi + 1) / 2
			payload["text"] = prefix + c.conversation[len(c.conversation)-mid:]
			if userMessageSize(payload) <= upstreamFrameLimit {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		return lo
	}
	send := func(inline, attached string) error {
		att, err := upload(name, attached)
		if err != nil {
			return err
		}
		removeLastAttachment(payload)
		appendPayloadAttachment(payload, att)
		payload["text"] = inline
		fitUserMessage(payload)
		return nil
	}

	var inline, attached string
	if n := fitTail(c.text(c.schemas, note)); n >= min(harnessMinTail, len(c.conversation)) {
		start := harnessBlockStart(c.conversation, len(c.conversation)-n)
		inline = c.text(c.schemas, note+c.conversation[start:])
		attached = "# Earlier conversation\n" + c.conversation[:start]
	} else if n := fitTail(c.text("[Full schemas are in the attached file "+name+".]", note)); n >= min(harnessMinTail, len(c.conversation)) {
		start := harnessBlockStart(c.conversation, len(c.conversation)-n)
		inline = c.text("[Full schemas are in the attached file "+name+".]", note+c.conversation[start:])
		attached = "# Declared harness tool schemas\n" + c.schemas + "\n\n# Earlier conversation\n" + c.conversation[:start]
	} else {
		// Instructions alone exceed the message. Keep a trimmed copy inline
		// and the complete context in the attachment.
		inline = "[The complete harness context is in the attached file " + name + ". Read it before acting.]\n\n" + c.text(c.schemas, c.conversation)
		attached = c.text(c.schemas, c.conversation)
	}
	payload["attachments"] = restore
	if err := send(inline, attached); err != nil {
		log.Printf("harness attachment unavailable bytes=%d: %v; trimming inline context", len(attached), err)
		removeLastAttachment(payload)
		payload["text"] = c.text(c.schemas, c.conversation)
		fitUserMessage(payload)
	} else {
		log.Printf("harness attachment name=%s bytes=%d inline=%d frame=%d", name, len(attached), len(inline), userMessageSize(payload))
	}
	if userMessageSize(payload) > upstreamFrameLimit {
		return fmt.Errorf("harness instructions or attachments exceed the Quick message size limit")
	}
	return nil
}

// harnessBlockStart moves a cut point forward to the next transcript block,
// so the inline tail starts at a message boundary when one is close by.
func harnessBlockStart(conversation string, cut int) int {
	limit := cut + (len(conversation)-cut)/4
	for i := cut; i < limit; {
		j := strings.Index(conversation[i:], "\n\n")
		if j < 0 || i+j+2 > limit {
			break
		}
		i += j + 2
		for _, prefix := range []string{"User: ", "Assistant: ", "Assistant tool call", "Assistant function call", "Tool result", "Function result"} {
			if strings.HasPrefix(conversation[i:], prefix) {
				return i
			}
		}
	}
	for cut < len(conversation) && !utf8.RuneStart(conversation[cut]) {
		cut++
	}
	return cut
}

func removeLastAttachment(payload map[string]any) {
	switch atts := payload["attachments"].(type) {
	case []Attachment:
		if len(atts) <= 1 {
			delete(payload, "attachments")
		} else {
			payload["attachments"] = atts[:len(atts)-1]
		}
	case []any:
		if len(atts) <= 1 {
			delete(payload, "attachments")
		} else {
			payload["attachments"] = atts[:len(atts)-1]
		}
	}
}
