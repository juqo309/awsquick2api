package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	xproxy "golang.org/x/net/proxy"
)

// Attachment is one entry in user_message.payload.attachments.
type Attachment struct {
	Type          string `json:"type"`
	Name          string `json:"name"`
	Size          int    `json:"size"`
	MIMEType      string `json:"mimeType"`
	Data          string `json:"data,omitempty"`
	Path          string `json:"path,omitempty"`
	AttachmentID  string `json:"attachmentId,omitempty"`
	ContentSHA256 string `json:"contentSha256,omitempty"`
}

func inlineAttachment(m Media, seq int) Attachment {
	name := m.Name
	if name == "" {
		name = fmt.Sprintf("%s-%d.%s", m.kind(), seq, m.ext())
	}
	return Attachment{
		Type:     m.kind(),
		Name:     name,
		Size:     m.decodedSize(),
		MIMEType: m.MIME,
		Data:     m.Data,
	}
}

func attachmentRoom(prompt, system string) int {
	used := len(prompt) + len(system) + attachmentTextReserve
	room := upstreamFrameLimit - used
	if room < 0 {
		return 0
	}
	return room
}

func prepareAttachments(ctx context.Context, sess *Session, proxyURL, threadID, prompt, system string, media []Media) ([]Attachment, string) {
	if len(media) == 0 {
		return nil, ""
	}
	room := attachmentRoom(prompt, system)
	var (
		out         []Attachment
		toUpload    []Media
		uploadNames []string
		skipped     int
		seq         int
		uploadBytes int
		uploadCount int
	)
	for _, m := range media {
		seq++
		cost := len(m.Data) + attachmentOverhead
		if m.Data != "" && cost <= room {
			out = append(out, inlineAttachment(m, seq))
			room -= cost
			continue
		}
		size := m.decodedSize()
		if m.Data != "" && size > 0 && size <= maxUploadBytes && uploadCount < maxUploadCount && uploadBytes+size <= maxUploadTotalBytes {
			toUpload = append(toUpload, m)
			uploadNames = append(uploadNames, inlineAttachment(m, seq).Name)
			uploadCount++
			uploadBytes += size
			continue
		}
		skipped++
	}
	for i, m := range toUpload {
		att, err := uploadAttachment(ctx, sess, proxyURL, threadID, m, uploadNames[i])
		if err != nil {
			log.Printf("attachment upload failed: %v", err)
			skipped++
			continue
		}
		out = append(out, att)
	}
	return out, skippedMediaNote(skipped)
}

func attachBase(sess *Session) string {
	if sess == nil {
		return ""
	}
	tenant := strings.TrimRight(strings.TrimSpace(sess.TenantURL), "/")
	if tenant != "" {
		if !strings.HasPrefix(tenant, "http://") && !strings.HasPrefix(tenant, "https://") {
			tenant = "https://" + tenant
		}
		return tenant + "/integration/quick-work/"
	}
	ws := strings.TrimSpace(sess.WSHost)
	if ws == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(ws, "wss://"):
		ws = "https://" + strings.TrimPrefix(ws, "wss://")
	case strings.HasPrefix(ws, "ws://"):
		ws = "http://" + strings.TrimPrefix(ws, "ws://")
	case strings.HasPrefix(ws, "https://") || strings.HasPrefix(ws, "http://"):
	default:
		ws = "https://" + ws
	}
	return strings.TrimRight(ws, "/") + "/integration/quick-work/"
}

func uploadAttachment(ctx context.Context, sess *Session, proxyURL, threadID string, media Media, name string) (Attachment, error) {
	raw, err := base64.StdEncoding.DecodeString(compactBase64(media.Data))
	if err != nil {
		return Attachment{}, fmt.Errorf("decode attachment: %w", err)
	}
	if name == "" {
		name = inlineAttachment(media, 1).Name
	}
	return uploadRaw(ctx, sess, proxyURL, threadID, name, media.MIME, media.kind(), raw)
}

func uploadRaw(ctx context.Context, sess *Session, proxyURL, threadID, name, mime, kind string, raw []byte) (Attachment, error) {
	if sess == nil {
		return Attachment{}, fmt.Errorf("missing session")
	}
	token := sess.jwt()
	if token == "" {
		return Attachment{}, fmt.Errorf("session has no token")
	}
	base := attachBase(sess)
	if base == "" {
		return Attachment{}, fmt.Errorf("session has no tenant_url or ws_host")
	}
	if name == "" {
		name = "attachment"
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	if kind == "" {
		kind = "file"
	}
	client := proxyHTTPClient(proxyURL, 0)
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("x-amzn-qs-internal-trace-id", uuid())

	uploadURL := base + "upload-attachment?" + url.Values{
		"name":          {name},
		"contentType":   {mime},
		"contentLength": {fmt.Sprintf("%d", len(raw))},
	}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(raw))
	if err != nil {
		return Attachment{}, err
	}
	req.Header = headers.Clone()
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		return Attachment{}, fmt.Errorf("upload: %w", err)
	}
	upBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return Attachment{}, fmt.Errorf("upload rejected: HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Attachment{}, fmt.Errorf("upload failed: HTTP %d", resp.StatusCode)
	}
	var up struct {
		AttachmentID string `json:"attachmentId"`
		ID           string `json:"attachment_id"`
	}
	_ = json.Unmarshal(upBody, &up)
	attachmentID := strings.TrimSpace(up.AttachmentID)
	if attachmentID == "" {
		attachmentID = strings.TrimSpace(up.ID)
	}
	if attachmentID == "" {
		return Attachment{}, fmt.Errorf("upload returned no attachmentId")
	}

	if err := waitDLP(ctx, client, base, headers, attachmentID); err != nil {
		return Attachment{}, err
	}

	path, err := deliverAttachment(ctx, client, base, headers, sess, threadID, attachmentID)
	if err != nil {
		return Attachment{}, err
	}
	sum := sha256.Sum256(raw)
	return Attachment{
		Type:          kind,
		Name:          name,
		Size:          len(raw),
		MIMEType:      mime,
		Path:          path,
		AttachmentID:  attachmentID,
		ContentSHA256: hex.EncodeToString(sum[:]),
	}, nil
}

func waitDLP(ctx context.Context, client *http.Client, base string, headers http.Header, attachmentID string) error {
	const maxPolls = 40
	statusURL := base + "attachment-dlp-status?attachmentId=" + url.QueryEscape(attachmentID)
	for i := 0; i < maxPolls; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
		if err != nil {
			return err
		}
		req.Header = headers.Clone()
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("dlp status: %w", err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return fmt.Errorf("dlp-status rejected: HTTP %d", resp.StatusCode)
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			var st struct {
				Job    string `json:"dlpJobStatus"`
				JobAlt string `json:"dlp_job_status"`
				Action string `json:"policyAction"`
			}
			_ = json.Unmarshal(body, &st)
			job := st.Job
			if job == "" {
				job = st.JobAlt
			}
			action := strings.ToUpper(strings.TrimSpace(st.Action))
			job = strings.ToUpper(strings.TrimSpace(job))
			switch action {
			case "ALLOW":
				return nil
			case "BLOCK", "DENY", "REJECT", "WARN":
				return fmt.Errorf("dlp policy %s did not allow the file", action)
			}
			switch job {
			case "SKIPPED", "NOT_APPLICABLE", "DISABLED":
				return nil
			case "COMPLETED":
				return nil
			case "FAILED", "ERROR":
				return fmt.Errorf("dlp job %s", job)
			}
		}
		if i+1 == maxPolls {
			break
		}
		timer := time.NewTimer(1500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("dlp did not finish")
}

func deliverAttachment(ctx context.Context, client *http.Client, base string, headers http.Header, sess *Session, threadID, attachmentID string) (string, error) {
	outpostID := strings.TrimSpace(sess.OutpostID)
	if outpostID == "" {
		outpostID = uuid()
	}
	working := strings.TrimSpace(sess.WorkingSessionID)
	if working == "" {
		working = uuid()
	}
	evt, _ := json.Marshal(map[string]any{
		"eventType": "invokeAgent",
		"eventBody": map[string]any{
			"invokeAgent": map[string]any{
				"path":             "files/upload/" + threadID,
				"queryParams":      map[string]any{"_method": "POST"},
				"body":             map[string]any{"attachmentId": attachmentID},
				"workingSessionId": working,
				"outpostId":        outpostID,
			},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"qbs-event", bytes.NewReader(evt))
	if err != nil {
		return "", err
	}
	req.Header = headers.Clone()
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("deliver: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("deliver rejected: HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("deliver failed: HTTP %d", resp.StatusCode)
	}
	var envelope map[string]any
	if json.Unmarshal(body, &envelope) != nil {
		return "", fmt.Errorf("deliver returned invalid JSON")
	}
	res := lookupAny(envelope, "eventResponse", "invokeAgentResponse")
	res = unwrapJSON(res)
	if api := lookupAny(asMap(res), "api_response"); api != nil {
		res = unwrapJSON(api)
	}
	if m := asMap(res); m != nil {
		if st, ok := m["status"]; ok {
			switch n := st.(type) {
			case float64:
				if n < 200 || n >= 300 {
					return "", fmt.Errorf("deliver returned status %v", st)
				}
			case json.Number:
				v, _ := n.Int64()
				if v < 200 || v >= 300 {
					return "", fmt.Errorf("deliver returned status %v", st)
				}
			}
		}
	}
	rb := unwrapJSON(lookupAny(asMap(res), "body"))
	bm := asMap(rb)
	if path, _ := bm["path"].(string); strings.TrimSpace(path) != "" {
		return strings.TrimSpace(path), nil
	}
	if files, _ := bm["files"].([]any); len(files) > 0 {
		if fm := asMap(files[0]); fm != nil {
			if path, _ := fm["path"].(string); strings.TrimSpace(path) != "" {
				return strings.TrimSpace(path), nil
			}
		}
	}
	return "", fmt.Errorf("deliver returned no file path")
}

func lookupAny(m map[string]any, keys ...string) any {
	cur := any(m)
	for _, k := range keys {
		mm := asMap(cur)
		if mm == nil {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func unwrapJSON(v any) any {
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return v
	}
	var out any
	if json.Unmarshal([]byte(s), &out) != nil {
		return v
	}
	return out
}

func proxyHTTPClient(proxyURL string, timeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL != "" {
		if pu, err := url.Parse(proxyURL); err == nil {
			scheme := strings.ToLower(pu.Scheme)
			if scheme == "socks5" || scheme == "socks5h" {
				var auth *xproxy.Auth
				if pu.User != nil {
					pass, _ := pu.User.Password()
					auth = &xproxy.Auth{User: pu.User.Username(), Password: pass}
				}
				if sd, err := xproxy.SOCKS5("tcp", pu.Host, auth, xproxy.Direct); err == nil {
					tr.Proxy = nil
					if cd, ok := sd.(xproxy.ContextDialer); ok {
						tr.DialContext = cd.DialContext
					} else {
						tr.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
							return sd.Dial(network, addr)
						}
					}
				}
			} else {
				tr.Proxy = http.ProxyURL(pu)
			}
		}
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}

const textTrimMarker = "\n\n[earlier conversation trimmed to fit Quick's message frame]\n\n"

func userMessageSize(payload map[string]any) int {
	body, err := marshalWireJSON(map[string]any{"type": "user_message", "payload": payload})
	if err != nil {
		return 0
	}
	return len(body)
}

func attachmentPromptName(messageID string) string {
	suffix := strings.ReplaceAll(messageID, "-", "")
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	if suffix == "" {
		suffix = "turn"
	}
	return "prompt-" + suffix + ".txt"
}

func attachmentShortText(name string) string {
	return "The user's full message for this turn is in the attached file " + name + ". Read that file and respond to it. Do not ask the user to resend it."
}

func spillPromptAttachment(ctx context.Context, sess *Session, proxyURL, threadID, messageID, prompt string) (Attachment, error) {
	name := attachmentPromptName(messageID)
	return uploadRaw(ctx, sess, proxyURL, threadID, name, "text/plain", "file", []byte(prompt))
}

func appendPayloadAttachment(payload map[string]any, att Attachment) {
	switch existing := payload["attachments"].(type) {
	case []Attachment:
		payload["attachments"] = append(existing, att)
	case []any:
		payload["attachments"] = append(existing, att)
	default:
		payload["attachments"] = []Attachment{att}
	}
}

func fitSystemPrompt(payload map[string]any) {
	if userMessageSize(payload) <= upstreamFrameLimit {
		return
	}
	sys, _ := payload["system_prompt"].(string)
	if strings.TrimSpace(sys) == "" {
		return
	}
	custom, _ := payload["custom_prompt"].(string)
	// Two copies of a large client prompt blow the frame. Keep the full
	// instructions on custom_prompt and a compact copy on system_prompt.
	if custom == sys {
		payload["system_prompt"] = compactSystemPrompt(sys)
		if userMessageSize(payload) <= upstreamFrameLimit {
			return
		}
	}
	ident := compactSystemPrompt(sys)
	payload["system_prompt"] = ident
	payload["custom_prompt"] = ident
}

func joinSystemAndTools(system, tools string) string {
	return strings.TrimSpace(system + "\n\n" + tools)
}

func packToolInstructions(payload map[string]any, tools string) {
	system, _ := payload["system_prompt"].(string)
	full := joinSystemAndTools(system, tools)
	payload["system_prompt"], payload["custom_prompt"] = full, full
	if userMessageSize(payload) <= upstreamFrameLimit {
		return
	}
	// Keep a single full catalog when two copies exceed the frame. Compact
	// the client instructions independently of the tool definitions.
	payload["system_prompt"] = clientToolPreamble + " " + clientToolEnvelope
	encoded, _ := json.Marshal(full)
	if len(encoded) > upstreamFrameLimit*3/4 {
		payload["custom_prompt"] = joinSystemAndTools(compactSystemPrompt(system), tools)
	}
}

func packUserMessage(ctx context.Context, sess *Session, proxyURL, threadID, messageID, prompt string, payload map[string]any, tools string) error {
	if tools != "" {
		packToolInstructions(payload, tools)
		// Keep the latest user message and tool results in the conversation.
		// Replacing them with an attachment makes a client tool turn depend on
		// Quick opening a remote file before it can see the actual request.
		fitUserMessage(payload)
		if userMessageSize(payload) > upstreamFrameLimit {
			stripInlineAttachmentData(payload)
			fitUserMessage(payload)
		}
		if userMessageSize(payload) > upstreamFrameLimit {
			return fmt.Errorf("client tool definitions exceed the Quick message size limit")
		}
		return nil
	}
	if userMessageSize(payload) <= upstreamFrameLimit {
		return nil
	}
	text, _ := payload["text"].(string)
	if text == "" {
		text = prompt
	}
	// Spill only the user transcript. Bundling client instructions into a
	// user attachment makes the upstream treat them as an untrusted jailbreak.
	if len(text) >= 1024 {
		att, err := spillPromptAttachment(ctx, sess, proxyURL, threadID, messageID, text)
		if err != nil {
			log.Printf("prompt attachment unavailable thread=%s bytes=%d: %v", threadID, len(text), err)
		} else if att.Path != "" {
			payload["text"] = attachmentShortText(att.Name)
			appendPayloadAttachment(payload, att)
			log.Printf("prompt attachment thread=%s bytes=%d name=%s path=%s frame=%d", threadID, att.Size, att.Name, att.Path, userMessageSize(payload))
		}
	}
	fitSystemPrompt(payload)
	stripInlineAttachmentData(payload)
	fitUserMessage(payload)
	return nil
}

func stripInlineAttachmentData(payload map[string]any) {
	switch atts := payload["attachments"].(type) {
	case []Attachment:
		for i := range atts {
			atts[i].Data = ""
		}
		payload["attachments"] = atts
	case []any:
		for _, item := range atts {
			if m := asMap(item); m != nil {
				delete(m, "data")
			}
		}
	}
}

func fitUserMessage(payload map[string]any) {
	if userMessageSize(payload) <= upstreamFrameLimit {
		return
	}
	original := userMessageSize(payload)
	if text, ok := payload["text"].(string); ok && len(text) > 1024 {
		payload["text"] = shrinkToFrame(text, func(s string) int {
			payload["text"] = s
			return userMessageSize(payload)
		})
	}
	if userMessageSize(payload) > upstreamFrameLimit {
		if sys, ok := payload["system_prompt"].(string); ok && len(sys) > 1024 {
			payload["system_prompt"] = shrinkToFrame(sys, func(s string) int {
				payload["system_prompt"] = s
				return userMessageSize(payload)
			})
		}
	}
	if n := userMessageSize(payload); n > original {
		return
	} else if n > upstreamFrameLimit {
		log.Printf("user_message still %d bytes after trim (limit %d)", n, upstreamFrameLimit)
	} else if n < original {
		log.Printf("user_message trimmed %d -> %d bytes (limit %d)", original, n, upstreamFrameLimit)
	}
}

func shrinkToFrame(s string, size func(string) int) string {
	out := s
	for range 8 {
		n := size(out)
		if n <= upstreamFrameLimit {
			return out
		}
		next := shrinkMiddle(out, n-upstreamFrameLimit)
		if next == out {
			return out
		}
		out = next
	}
	return out
}

func shrinkMiddle(s string, remove int) string {
	keep := len(s) - remove - len(textTrimMarker)
	if keep < 4096 {
		keep = min(4096, len(s))
	}
	if keep >= len(s) {
		return s
	}
	head := keep / 4
	tail := keep - head
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	startTail := len(s) - tail
	for startTail < len(s) && startTail > 0 && !utf8.RuneStart(s[startTail]) {
		startTail++
	}
	if head <= 0 || startTail >= len(s) || head >= startTail {
		return s
	}
	return s[:head] + textTrimMarker + s[startTail:]
}
