package proxy

import (
	"encoding/base64"
	"strings"
	"testing"
)

const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func TestExtractMediaFromClientParts(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: []any{
			map[string]any{"type": "text", "text": "earlier"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + tinyPNG}},
		}},
		{Role: "assistant", Content: "ok"},
		{Role: "user", Content: []any{
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/jpeg", "data": tinyPNG}},
			map[string]any{"type": "text", "text": "now"},
		}},
	}
	got := currentTurnMedia(msgs)
	if len(got) != 1 || got[0].MIME != "image/jpeg" || got[0].Data != tinyPNG {
		t.Fatalf("current turn: %+v", got)
	}
	all := extractMedia(msgs[0].Content)
	if len(all) != 1 || all[0].MIME != "image/png" {
		t.Fatalf("history extract: %+v", all)
	}
}

func TestParseDataURLAndSniff(t *testing.T) {
	mime, data, ok := parseDataURL("data:image/png;base64, " + tinyPNG)
	if !ok || mime != "image/png" || data != tinyPNG {
		t.Fatalf("parse: %q %q %v", mime, data, ok)
	}
	raw, err := decodeTestPNG()
	if err != nil || sniffMIME(raw) != "image/png" {
		t.Fatalf("sniff: %v %v", sniffMIME(raw), err)
	}
	if m, ok := mediaFromBase64("application/octet-stream", tinyPNG, ""); !ok || m.MIME != "image/png" {
		t.Fatalf("sniffed media: %+v %v", m, ok)
	}
}

func TestContentTextDoesNotDumpImageBytes(t *testing.T) {
	text := contentText([]any{
		map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": tinyPNG}},
		map[string]any{"type": "text", "text": "caption"},
	})
	if text != "[image]caption" || strings.Contains(text, tinyPNG) {
		t.Fatalf("contentText: %q", text)
	}
}

func decodeTestPNG() ([]byte, error) {
	return base64.StdEncoding.DecodeString(tinyPNG)
}
