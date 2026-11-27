package proxy

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestHarnessReplyAcrossChunks(t *testing.T) {
	tools := harnessTools{allowed: map[string]bool{"Read": true, "Bash": true}, shell: "Bash", command: "command"}
	fence := "```"
	command := fence + "bash run\n"
	literal := "`" + fence + "text\n" + command + "uname -s\n" + fence + "\n" + fence + "`\n\n"
	read := command + "harness Read <<'HARNESS_JSON'\n{\"path\":\"café\"}\nHARNESS_JSON\n" + fence
	text := "Intro.\n\n" + literal + read + "\n\nDone."
	want := []anthropicBlock{
		anthropicText("Intro.\n\n" + literal),
		{Type: "tool_use", Name: "Read", Input: json.RawMessage(`{"path":"café"}`)},
		anthropicText("\nDone."),
	}
	for i := range len(text) + 1 {
		var emitted strings.Builder
		reply := harnessReply{tools: tools, emit: func(text string) { emitted.WriteString(text) }}
		reply.push(text[:i])
		reply.push(text[i:])
		if got := emitted.String(); got != "Intro.\n\n"+literal {
			t.Fatalf("split %d streamed %q", i, got)
		}
		got := reply.finish()
		for j := range got {
			got[j].ID = ""
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("split %d produced %#v", i, got)
		}
	}
	for _, input := range []string{
		command + "echo ready",
		command + "harness Read <<'HARNESS_JSON'\n{}\n" + fence,
		command + "harness Missing {}\necho ready\n" + fence,
		command + "harness Read '{\"a\":1}\"\n" + fence,
		command + "uname -s\nharness Read {}\n" + fence,
	} {
		reply := harnessReply{tools: tools, single: true}
		for _, r := range input {
			reply.push(string(r))
		}
		got := reply.finish()
		if reply.calls != 0 || !reply.invalid || len(got) != 1 || got[0].Text == nil || *got[0].Text != input {
			t.Fatalf("accepted malformed or disallowed tool block %q", input)
		}
	}
}

func harnessTestContext(conversation string) harnessContext {
	return harnessContext{instructions: "Client instructions.", schemas: `[{"name":"lookup"}]`, conversation: conversation}
}

func TestHarnessMessageFitsInline(t *testing.T) {
	c := harnessTestContext(strings.Repeat("<p>Example</p>\n", 1850))
	payload := map[string]any{"custom_prompt": "tool protocol"}
	err := harnessMessage(payload, c, "conversation-1.txt", func(string, string) (Attachment, error) {
		t.Fatal("small context must not be uploaded")
		return Attachment{}, nil
	})
	if err != nil || payload["text"] != c.text(c.schemas, c.conversation) || payload["attachments"] != nil {
		t.Fatalf("inline context changed: %v", err)
	}
}

func TestHarnessMessageAttachesEarlierConversation(t *testing.T) {
	var blocks []string
	for i := 0; i < 400; i++ {
		blocks = append(blocks, fmt.Sprintf("User: question %d \"\\🐈\x00", i), "Assistant: "+strings.Repeat("answer ", 40))
	}
	blocks = append(blocks, "Tool result (call_id=toolu_1, name=lookup): Final tool result.")
	c := harnessTestContext(strings.Join(blocks, "\n\n"))
	payload := map[string]any{"custom_prompt": "tool protocol"}
	var attached string
	uploads := 0
	err := harnessMessage(payload, c, "conversation-1.txt", func(name, text string) (Attachment, error) {
		uploads++
		attached = text
		return Attachment{Type: "file", Name: name, Size: len(text), MIMEType: "text/plain", Path: "/data/" + name, AttachmentID: "att-1"}, nil
	})
	text, _ := payload["text"].(string)
	atts, _ := payload["attachments"].([]Attachment)
	if err != nil || uploads != 1 || len(atts) != 1 || atts[0].AttachmentID != "att-1" {
		t.Fatalf("expected one attachment: uploads=%d attachments=%+v err=%v", uploads, atts, err)
	}
	if userMessageSize(payload) > upstreamFrameLimit || !utf8.ValidString(text) || strings.Contains(text, "reply only OK") {
		t.Fatalf("invalid message: %d bytes", userMessageSize(payload))
	}
	if !strings.HasPrefix(text, "# Harness instructions\nClient instructions.\n\n# Declared harness tool schemas\n"+c.schemas) ||
		!strings.HasSuffix(text, "Final tool result.") || !strings.Contains(text, harnessHistoryNote("conversation-1.txt")) {
		t.Fatal("inline message lost instructions, schemas, or the latest tool result")
	}
	earlier := strings.TrimPrefix(attached, "# Earlier conversation\n")
	tail := text[strings.Index(text, harnessHistoryNote("conversation-1.txt"))+len(harnessHistoryNote("conversation-1.txt")):]
	if earlier+tail != c.conversation || !strings.HasPrefix(tail, "User: ") && !strings.HasPrefix(tail, "Assistant: ") {
		t.Fatal("attachment and inline tail do not reconstruct the conversation at a block boundary")
	}
}

func TestHarnessMessageTrimsWhenUploadFails(t *testing.T) {
	c := harnessTestContext(strings.Repeat("Assistant: long answer\n\n", 8000) + "Tool result: latest")
	payload := map[string]any{"custom_prompt": "tool protocol"}
	err := harnessMessage(payload, c, "conversation-1.txt", func(string, string) (Attachment, error) {
		return Attachment{}, fmt.Errorf("offline")
	})
	text, _ := payload["text"].(string)
	if err != nil || payload["attachments"] != nil || userMessageSize(payload) > upstreamFrameLimit ||
		!strings.HasPrefix(text, "# Harness instructions\nClient instructions.") || !strings.HasSuffix(text, "Tool result: latest") {
		t.Fatalf("fallback message: %d bytes, err=%v", userMessageSize(payload), err)
	}
}
