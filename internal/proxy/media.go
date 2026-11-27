package proxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	maxChatBodyBytes      = 32 << 20
	upstreamFrameLimit    = 60 * 1024
	attachmentOverhead    = 256
	attachmentTextReserve = 8 * 1024
	maxUploadBytes        = 3 << 20
	maxUploadCount        = 4
	maxUploadTotalBytes   = 5 << 20
)

// Media is one inline image or PDF extracted from a client request.
type Media struct {
	MIME string
	Data string // raw base64, no data: prefix
	Name string
}

func (m Media) dataURL() string {
	return "data:" + m.MIME + ";base64," + m.Data
}

func (m Media) kind() string {
	if isClaudeImageMIME(m.MIME) {
		return "image"
	}
	return "file"
}

func (m Media) ext() string {
	switch normalizeMIME(m.MIME) {
	case "image/jpeg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	case "application/pdf":
		return "pdf"
	default:
		if _, rest, ok := strings.Cut(m.MIME, "/"); ok {
			rest = strings.ToLower(rest)
			var b strings.Builder
			for _, r := range rest {
				if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
					b.WriteRune(r)
				}
			}
			if b.Len() > 0 {
				return b.String()
			}
		}
		return "bin"
	}
}

func (m Media) decodedSize() int {
	n := len(m.Data)
	pad := 0
	if n > 0 && m.Data[n-1] == '=' {
		pad++
		if n > 1 && m.Data[n-2] == '=' {
			pad++
		}
	}
	if size := n/4*3 - pad; size > 0 {
		return size
	}
	return 0
}

func normalizeMIME(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	if m == "image/jpg" {
		return "image/jpeg"
	}
	return m
}

func isClaudeImageMIME(m string) bool {
	switch normalizeMIME(m) {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

func isClaudeDocumentMIME(m string) bool {
	return normalizeMIME(m) == "application/pdf"
}

func isClaudeMediaMIME(m string) bool {
	return isClaudeImageMIME(m) || isClaudeDocumentMIME(m)
}

func compactBase64(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, s)
}

func parseDataURL(s string) (mime, data string, ok bool) {
	s = strings.TrimSpace(s)
	rest, found := strings.CutPrefix(s, "data:")
	if !found {
		return "", "", false
	}
	header, payload, found := strings.Cut(rest, ",")
	if !found || strings.TrimSpace(payload) == "" {
		return "", "", false
	}
	mime = header
	if i := strings.Index(header, ";"); i >= 0 {
		mime = header[:i]
	}
	if !strings.Contains(strings.ToLower(header), "base64") {
		return "", "", false
	}
	data = compactBase64(payload)
	return normalizeMIME(mime), data, data != ""
}

func sniffMIME(raw []byte) string {
	if len(raw) >= 8 && string(raw[:8]) == "\x89PNG\r\n\x1a\n" {
		return "image/png"
	}
	if len(raw) >= 3 && raw[0] == 0xff && raw[1] == 0xd8 && raw[2] == 0xff {
		return "image/jpeg"
	}
	if len(raw) >= 6 && (string(raw[:6]) == "GIF87a" || string(raw[:6]) == "GIF89a") {
		return "image/gif"
	}
	if len(raw) >= 12 && string(raw[:4]) == "RIFF" && string(raw[8:12]) == "WEBP" {
		return "image/webp"
	}
	if len(raw) >= 5 && string(raw[:5]) == "%PDF-" {
		return "application/pdf"
	}
	return ""
}

func mediaFromBase64(mime, data, name string) (Media, bool) {
	data = compactBase64(data)
	if data == "" {
		return Media{}, false
	}
	mime = normalizeMIME(mime)
	if !isClaudeMediaMIME(mime) {
		if raw, err := base64.StdEncoding.DecodeString(data); err == nil {
			if sniffed := sniffMIME(raw); sniffed != "" {
				mime = sniffed
			}
		}
	}
	if !isClaudeMediaMIME(mime) {
		return Media{}, false
	}
	return Media{MIME: mime, Data: data, Name: strings.TrimSpace(name)}, true
}

func mediaFromDataURL(url, name string) (Media, bool) {
	mime, data, ok := parseDataURL(url)
	if !ok {
		return Media{}, false
	}
	return mediaFromBase64(mime, data, name)
}

func currentTurnMedia(msgs []Message) []Media {
	last := -1
	for i, m := range msgs {
		if strings.EqualFold(strings.TrimSpace(m.Role), "assistant") {
			last = i
		}
	}
	var out []Media
	seen := map[string]bool{}
	for _, m := range msgs[last+1:] {
		for _, media := range extractMedia(m.Content) {
			key := media.MIME + "\x00" + media.Data
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, media)
		}
	}
	return out
}

func extractMedia(v any) []Media {
	switch c := v.(type) {
	case []any:
		var out []Media
		for _, part := range c {
			out = append(out, extractMedia(part)...)
		}
		return out
	case map[string]any:
		return extractMediaPart(c)
	default:
		return nil
	}
}

func extractMediaPart(p map[string]any) []Media {
	typ, _ := p["type"].(string)
	switch typ {
	case "image_url", "input_image", "image":
		if src, ok := p["source"].(map[string]any); ok {
			if m, ok := mediaFromSourceMap(src); ok {
				return []Media{m}
			}
		}
		u := imagePartURL(p)
		if u == "" {
			u, _ = p["url"].(string)
		}
		if m, ok := mediaFromDataURL(u, stringField(p, "name", "filename")); ok {
			return []Media{m}
		}
	case "file", "input_file", "document":
		if src, ok := p["source"].(map[string]any); ok {
			if m, ok := mediaFromSourceMap(src); ok {
				return []Media{m}
			}
		}
		file, _ := p["file"].(map[string]any)
		if file == nil {
			file = p
		}
		name := stringField(file, "filename", "name")
		if name == "" {
			name = stringField(p, "filename", "name")
		}
		if data, _ := file["file_data"].(string); data != "" {
			if m, ok := mediaFromDataURL(data, name); ok {
				return []Media{m}
			}
		}
		if data, _ := file["data"].(string); data != "" {
			mime, _ := file["media_type"].(string)
			if mime == "" {
				mime, _ = file["mime_type"].(string)
			}
			if m, ok := mediaFromBase64(mime, data, name); ok {
				return []Media{m}
			}
		}
	}
	return nil
}

func stringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func mediaFromSourceMap(src map[string]any) (Media, bool) {
	raw, err := json.Marshal(src)
	if err != nil {
		return Media{}, false
	}
	var source anthropicSource
	if json.Unmarshal(raw, &source) != nil {
		return Media{}, false
	}
	return mediaFromSource(source)
}

func mediaFromSource(src anthropicSource) (Media, bool) {
	switch strings.ToLower(strings.TrimSpace(src.Type)) {
	case "base64", "":
		return mediaFromBase64(src.MediaType, src.Data, "")
	case "url":
		return mediaFromDataURL(src.URL, "")
	default:
		return Media{}, false
	}
}

func mergeContent(existing any, part any) any {
	if existing == nil || existing == "" {
		return part
	}
	es, eok := existing.(string)
	ps, pok := part.(string)
	if eok && pok {
		return es + ps
	}
	parts := contentAsParts(existing)
	if pok {
		if n := len(parts); n > 0 {
			if m, ok := parts[n-1].(map[string]any); ok {
				if t, _ := m["type"].(string); t == "text" {
					if s, ok := m["text"].(string); ok {
						m["text"] = s + ps
						return parts
					}
				}
			}
		}
		return append(parts, map[string]any{"type": "text", "text": ps})
	}
	return append(parts, part)
}

func contentAsParts(v any) []any {
	switch c := v.(type) {
	case string:
		if c == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": c}}
	case []any:
		return append([]any{}, c...)
	case nil:
		return nil
	default:
		return []any{c}
	}
}

func mediaPart(m Media) map[string]any {
	if isClaudeDocumentMIME(m.MIME) {
		name := m.Name
		if name == "" {
			name = "document.pdf"
		}
		return map[string]any{
			"type": "file",
			"file": map[string]any{
				"filename":  name,
				"file_data": m.dataURL(),
			},
		}
	}
	return map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": m.dataURL()},
	}
}

func mediaMarker(kind string) string {
	if kind == "document" || kind == "file" {
		return "[file]"
	}
	return "[image]"
}

func skippedMediaNote(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("\n\n[%d image(s) not delivered: too large, upload failed, or not inline data]", n)
}
