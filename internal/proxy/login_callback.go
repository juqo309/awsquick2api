package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const loginCallbackPort = "18080"

var callbackHTTP = &http.Client{
	Transport:     &http.Transport{DisableKeepAlives: true}, // never use an outbound proxy
	Timeout:       70 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func parseLoginCallback(raw string) (*url.URL, error) {
	invalid := errors.New("paste the full local callback URL from the sign-in tab")
	if len(raw) > 16384 {
		return nil, invalid
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "http" || u.User != nil || u.Port() != loginCallbackPort || u.Fragment != "" || u.RawPath != "" {
		return nil, invalid
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
	default:
		return nil, invalid
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, invalid
	}
	switch u.Path {
	case "/callback":
		if len(query["nonce"]) != 1 || query.Get("nonce") == "" {
			return nil, invalid
		}
	case "", "/", "/index.html":
		if len(query["state"]) != 1 || query.Get("state") == "" || (query.Get("code") == "" && query.Get("error") == "") {
			return nil, invalid
		}
	default:
		return nil, invalid
	}
	// Never send a request to a host supplied by the browser. Only relay the
	// validated callback path/query to the existing local sign-in helper.
	return &url.URL{Scheme: "http", Host: "127.0.0.1:" + loginCallbackPort, Path: u.Path, RawQuery: u.RawQuery}, nil
}

func (s *Server) adminLoginCallback(w http.ResponseWriter, r *http.Request) {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if site := r.Header.Get("Sec-Fetch-Site"); mediaType != "application/json" || (site != "" && site != "same-origin") {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "submit the callback from this dashboard"})
		return
	}
	var body struct {
		URL string `json:"callback_url"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid callback request"})
		return
	}
	u, err := parseLoginCallback(body.URL)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	j := s.login
	j.mu.Lock()
	if j.state != "waiting" || j.url == "" || j.relaying {
		j.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{"error": "no sign-in is ready for a callback"})
		return
	}
	j.relaying = true
	j.mu.Unlock()
	defer func() { j.mu.Lock(); j.relaying = false; j.mu.Unlock() }()

	status, authorize := relayLoginCallback(r.Context(), u)
	switch status {
	case http.StatusFound:
		j.mu.Lock()
		if j.state == "waiting" {
			j.authorizeURL = authorize
		}
		j.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"authorize_url": authorize})
	case http.StatusOK:
		writeJSON(w, http.StatusOK, map[string]any{"submitted": true})
	case http.StatusBadRequest:
		writeJSON(w, status, map[string]any{"error": "callback does not match this sign-in. Copy the full URL from the current sign-in tab."})
	default:
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "the sign-in helper could not accept the callback"})
	}
}

func relayLoginCallback(ctx context.Context, u *url.URL) (int, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return http.StatusBadGateway, ""
	}
	req.Header.Set("X-Quick-Callback-Relay", "1")
	res, err := callbackHTTP.Do(req)
	if err != nil {
		return http.StatusBadGateway, ""
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 16<<10))
	if res.StatusCode != http.StatusFound {
		return res.StatusCode, ""
	}
	location, err := url.Parse(res.Header.Get("Location"))
	if err != nil || location.Scheme != "https" || location.Hostname() == "" || location.User != nil {
		return http.StatusBadGateway, ""
	}
	return http.StatusFound, location.String()
}
