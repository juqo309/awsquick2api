package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestPanelSVGPreviewKeepsStaticArt(t *testing.T) {
	raw := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100" onload="test()">
	<defs><linearGradient id="sky"><stop offset="0%" stop-color="#abc"/></linearGradient></defs>
	<rect width="100" height="100" fill="url(#sky)" style="display:none"/>
	<circle r="30" fill="u\72l(https://example.invalid/pixel)"/>
	<script>alert(1)</script><foreignObject><p>HTML</p></foreignObject>
	<image href="https://example.invalid/pixel"/><text>5 &lt; 10</text></svg>`
	text, artifacts := panelReply("Preview:\n\n```svg\n" + raw + "\n```\n\nThe end.")
	if strings.Contains(text, "<svg") || !strings.Contains(text, "Preview:") || !strings.Contains(text, "The end.") || len(artifacts) != 1 {
		t.Fatalf("preview not extracted: %q %#v", text, artifacts)
	}
	svg := artifacts[0].Source
	for _, retained := range []string{`xmlns="http://www.w3.org/2000/svg"`, `fill="url(#sky)"`, "<linearGradient", "5 &lt; 10"} {
		if !strings.Contains(svg, retained) {
			t.Errorf("expected %q in SVG: %s", retained, svg)
		}
	}
	for _, removed := range []string{"onload", "style=", "example.invalid", "<script", "<foreignObject", "<image"} {
		if strings.Contains(svg, removed) {
			t.Errorf("unexpected %q in SVG: %s", removed, svg)
		}
	}
	for _, invalid := range []string{`<svg><g></svg>`, `<svg/><svg/>`, `<!DOCTYPE svg><svg/>`, `<?xml-stylesheet href="https://example.invalid"?><svg/>`} {
		if _, err := staticPanelSVG(invalid); err == nil {
			t.Errorf("accepted invalid SVG: %s", invalid)
		}
	}
}

func TestPanelToolResultsPreserveProseAndValidateArguments(t *testing.T) {
	raw := `{"tool_calls":[{"name":"show_table","input":{"title":"Results","columns":["Name","Count"],"rows":[["A",9007199254740993]]}},{"name":"make_checklist","input":{"items":[" Book train ","Pack"]}},{"name":"create_file","input":{"filename":"notes.md","content":""}},{"name":"create_file","input":{"filename":"../script.html","content":"hello"}}]}`
	text, artifacts := panelReply("Here are the results.\n```json\n" + raw + "\n```")
	if text != "Here are the results." || len(artifacts) != 4 {
		t.Fatalf("unexpected reply: %q %#v", text, artifacts)
	}
	if artifacts[0].Kind != "table" || artifacts[0].Rows[0][1] != "9007199254740993" || artifacts[1].Items[0] != "Book train" || artifacts[2].Filename != "notes.md" || artifacts[3].Kind != "error" {
		t.Fatalf("unexpected artifacts: %#v", artifacts)
	}
	text, artifacts = panelReply(`Partial {"tool_calls":[{"name":"render_svg"`)
	if text != "Partial" || len(artifacts) != 1 || artifacts[0].Kind != "error" {
		t.Fatalf("partial tool leaked: %q %#v", text, artifacts)
	}
}

func TestPanelChatStreamsTextThenSVG(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	prompts := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			typ, payload, ok := decodeAIDPUserMessage(data)
			if !ok || typ != "user_message" {
				continue
			}
			prompt, _ := payload["system_prompt"].(string)
			prompts <- prompt
			for _, delta := range []string{`Here it is. {"tool_`, `calls":[{"name":"render_svg","input":{"title":"A circle","svg":"<svg viewBox='0 0 20 20'><circle cx='10' cy='10' r='5'/></svg>"}}]}`} {
				_ = conn.WriteJSON(map[string]any{"type": "text_delta", "payload": map[string]string{"text": delta}})
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"turn_completed"}`))
			return
		}
	}))
	defer upstream.Close()
	dir := t.TempDir()
	s := NewServer(&Config{AccountsFile: filepath.Join(dir, "accounts.json"), StatsFile: filepath.Join(dir, "stats.json"), Models: []string{"smart"}, DefaultModel: "smart", Timeout: 5 * time.Second})
	a := fixtureAccount("test", time.Now().Add(time.Hour).Unix())
	a.Session.WSHost = "ws" + strings.TrimPrefix(upstream.URL, "http")
	s.accounts.data.Accounts = []Account{a}
	req := httptest.NewRequest(http.MethodPost, "/admin/chat", strings.NewReader(`{"message":"Draw a circle","stream":true,"built_in_tools":true}`))
	req.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	body := w.Body.String()
	if w.Code != http.StatusOK || strings.Contains(body, "tool_calls") || !strings.Contains(body, `"text":"Here it is. "`) {
		t.Fatalf("unexpected stream: %d %s", w.Code, body)
	}
	prompt := <-prompts
	if !strings.Contains(prompt, "render_svg") || !strings.Contains(prompt, "create_file") {
		t.Fatalf("tool catalog missing from upstream prompt")
	}
	for _, event := range strings.Split(body, "\n\n") {
		var reply struct {
			Done      bool
			Text      string
			Artifacts []panelArtifact
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(event, "data: ")), &reply) == nil && reply.Done {
			if reply.Text != "Here it is." || len(reply.Artifacts) != 1 || reply.Artifacts[0].Kind != "svg" || reply.Artifacts[0].Title != "A circle" {
				t.Fatalf("unexpected final event: %s", event)
			}
			return
		}
	}
	t.Fatalf("final event missing: %s", body)
}
