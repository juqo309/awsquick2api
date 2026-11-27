package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrepareAttachmentsInline(t *testing.T) {
	media := []Media{{MIME: "image/png", Data: tinyPNG}}
	atts, note := prepareAttachments(context.Background(), nil, "", "thread", "hi", "", media)
	if note != "" || len(atts) != 1 || atts[0].Data != tinyPNG || atts[0].Type != "image" || atts[0].MIMEType != "image/png" {
		t.Fatalf("inline: %+v %q", atts, note)
	}
}

func TestUploadAttachment(t *testing.T) {
	var uploaded bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "upload-attachment"):
			uploaded = true
			if r.URL.Query().Get("contentType") != "image/png" {
				t.Errorf("contentType: %s", r.URL.Query().Get("contentType"))
			}
			body, _ := io.ReadAll(r.Body)
			if len(body) == 0 {
				t.Errorf("empty upload body")
			}
			writeJSON(w, 200, map[string]any{"attachmentId": "att-1"})
		case strings.Contains(r.URL.Path, "attachment-dlp-status"):
			writeJSON(w, 200, map[string]any{"dlpJobStatus": "COMPLETED", "policyAction": "ALLOW"})
		case strings.HasSuffix(r.URL.Path, "qbs-event"):
			writeJSON(w, 200, map[string]any{
				"eventResponse": map[string]any{
					"invokeAgentResponse": map[string]any{
						"api_response": map[string]any{
							"status": 200,
							"body":   map[string]any{"path": "/data/s3files/users/x/sessions/t/workspace/attached_files/image-1.png"},
						},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	sess := &Session{JWT: "tok", TenantURL: ts.URL, OutpostID: "11111111-1111-4111-8111-111111111111"}
	att, err := uploadAttachment(context.Background(), sess, "", "thread-1", Media{MIME: "image/png", Data: tinyPNG}, "image-1.png")
	if err != nil {
		t.Fatal(err)
	}
	if !uploaded || att.AttachmentID != "att-1" || att.Path == "" || att.Data != "" || att.ContentSHA256 == "" {
		raw, _ := json.Marshal(att)
		t.Fatalf("upload result: %s", raw)
	}
}

func TestAttachBaseFromWSHost(t *testing.T) {
	got := attachBase(&Session{WSHost: "wss://example.test"})
	if got != "https://example.test/integration/quick-work/" {
		t.Fatalf("base: %s", got)
	}
}

func TestPackUserMessageSpillsPrompt(t *testing.T) {
	var uploaded string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "upload-attachment"):
			body, _ := io.ReadAll(r.Body)
			uploaded = string(body)
			writeJSON(w, 200, map[string]any{"attachmentId": "att-prompt"})
		case strings.Contains(r.URL.Path, "attachment-dlp-status"):
			writeJSON(w, 200, map[string]any{"dlpJobStatus": "SKIPPED"})
		case strings.HasSuffix(r.URL.Path, "qbs-event"):
			writeJSON(w, 200, map[string]any{
				"eventResponse": map[string]any{
					"invokeAgentResponse": map[string]any{
						"api_response": map[string]any{
							"status": 200,
							"body":   map[string]any{"path": "/workspace/attached_files/prompt-abcd.txt"},
						},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	prompt := strings.Repeat("context line\n", 8000)
	system := strings.Repeat("tool schema line\n", 4000)
	payload := map[string]any{"text": prompt, "system_prompt": system}
	sess := &Session{JWT: "tok", TenantURL: ts.URL, OutpostID: "11111111-1111-4111-8111-111111111111"}
	if err := packUserMessage(context.Background(), sess, "", "thread-1", "msg-abcd-efgh", prompt, payload, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(uploaded, prompt) {
		t.Fatalf("did not upload user prompt: %d", len(uploaded))
	}
	if strings.Contains(uploaded, system) {
		t.Fatal("uploaded client system prompt as a user attachment")
	}
	text, _ := payload["text"].(string)
	if payload["system_prompt"] != harnessCompactPrompt || payload["custom_prompt"] != harnessCompactPrompt {
		t.Fatalf("spilled prompt lost harness compact: %#v", payload["system_prompt"])
	}
	if !strings.Contains(text, "prompt-") || userMessageSize(payload) > 8*1024 {
		raw, _ := json.Marshal(payload["attachments"])
		t.Fatalf("frame still large: size=%d text=%q atts=%s", userMessageSize(payload), text, raw)
	}
}

func TestPackUserMessageKeepsClientSystem(t *testing.T) {
	var uploaded string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "upload-attachment"):
			body, _ := io.ReadAll(r.Body)
			uploaded = string(body)
			writeJSON(w, 200, map[string]any{"attachmentId": "att-prompt"})
		case strings.Contains(r.URL.Path, "attachment-dlp-status"):
			writeJSON(w, 200, map[string]any{"dlpJobStatus": "SKIPPED"})
		case strings.HasSuffix(r.URL.Path, "qbs-event"):
			writeJSON(w, 200, map[string]any{
				"eventResponse": map[string]any{
					"invokeAgentResponse": map[string]any{
						"api_response": map[string]any{
							"status": 200,
							"body":   map[string]any{"path": "/workspace/attached_files/prompt-abcd.txt"},
						},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	prompt := strings.Repeat("user line\n", 2500)
	system := "You are an expert coding assistant operating inside a local CLI.\n" + strings.Repeat("tool schema line\n", 900)
	payload := map[string]any{"text": prompt, "system_prompt": system, "custom_prompt": system}
	sess := &Session{JWT: "tok", TenantURL: ts.URL, OutpostID: "11111111-1111-4111-8111-111111111111"}
	if err := packUserMessage(context.Background(), sess, "", "thread-1", "msg-abcd-efgh", prompt, payload, ""); err != nil {
		t.Fatal(err)
	}
	if uploaded != "" && strings.Contains(uploaded, "a local CLI") {
		t.Fatal("client instructions were spilled into the user attachment")
	}
	if sys, _ := payload["system_prompt"].(string); !strings.Contains(sys, "a local CLI") && sys != harnessCompactPrompt {
		t.Fatalf("lost client system: %#v", payload["system_prompt"])
	}
	if custom, _ := payload["custom_prompt"].(string); !strings.Contains(custom, "a local CLI") && custom != harnessCompactPrompt {
		t.Fatalf("lost client custom prompt: %#v", payload["custom_prompt"])
	}
	if payload["system_prompt"] == defaultSystemPrompt || payload["custom_prompt"] == defaultSystemPrompt {
		t.Fatal("replaced client prompt with Claude identity")
	}
	if userMessageSize(payload) > upstreamFrameLimit {
		t.Fatalf("frame still large: %d", userMessageSize(payload))
	}
}

func TestShrinkMiddleKeepsHeadAndTail(t *testing.T) {
	s := strings.Repeat("A", 2000) + "MIDDLE" + strings.Repeat("Z", 6000)
	got := shrinkMiddle(s, 3000)
	if !strings.Contains(got, textTrimMarker) || !strings.HasPrefix(got, "AAA") || !strings.HasSuffix(got, "ZZZ") || strings.Contains(got, "MIDDLE") {
		t.Fatalf("shrink: %q", got)
	}
}
