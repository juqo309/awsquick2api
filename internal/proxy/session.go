package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Session mirrors the useful fields of GET /auth/sncs-session on the local
// QuickWork outpost, plus a few fields written by the Python login helper.
type Session struct {
	JWT              string `json:"jwt"`
	IDToken          string `json:"id_token"`
	WSHost           string `json:"ws_host"`
	WSPath           string `json:"ws_path"`
	TenantURL        string `json:"tenant_url"`
	Provider         string `json:"provider"`
	ActorARN         string `json:"actor_arn"`
	Surface          string `json:"surface"`
	OutpostID        string `json:"outpost_id"`
	WorkingSessionID string `json:"working_session_id"`
	AccountID        string `json:"aws_account_id"`
	Region           string `json:"region"`
	AccountName      string `json:"account_name"`
	EmbedHost        string `json:"embed_host"`
	UsableUntil      int64  `json:"usable_until"`
	ObtainedAt       int64  `json:"obtained_at"`
}

func (s *Session) jwt() string {
	if s == nil {
		return ""
	}
	if s.JWT != "" {
		return s.JWT
	}
	return s.IDToken
}

// SessionSource prefers a live outpost session (auto-refreshing) and falls
// back to the session file written by the Python login helper.
type SessionSource struct {
	cfg *Config

	mu       sync.Mutex
	cached   *Session
	cachedAt time.Time
	http     *http.Client
}

func NewSessionSource(cfg *Config) *SessionSource {
	return &SessionSource{
		cfg:  cfg,
		http: &http.Client{Timeout: 20 * time.Second},
	}
}

func (s *SessionSource) invalidate() {
	s.mu.Lock()
	s.cached = nil
	s.mu.Unlock()
}

// Get returns a usable upstream session. Outpost results are cached briefly so
// a chat request does not mint a new token on every frame.
func (s *SessionSource) Get(ctx context.Context) (*Session, error) {
	s.mu.Lock()
	if s.cached != nil && time.Since(s.cachedAt) < 20*time.Second {
		sess := *s.cached
		s.mu.Unlock()
		return &sess, nil
	}
	s.mu.Unlock()

	var outpostErr error
	if s.cfg.OutpostURL != "" && s.cfg.InstanceTokenFile != "" {
		sess, err := s.fromOutpost(ctx)
		if err == nil {
			s.mu.Lock()
			s.cached = sess
			s.cachedAt = time.Now()
			s.mu.Unlock()
			return sess, nil
		}
		outpostErr = err
	}

	sess, err := s.fromFile()
	if err == nil {
		return sess, nil
	}
	if outpostErr != nil {
		return nil, fmt.Errorf("outpost session failed: %v; session file failed: %w", outpostErr, err)
	}
	return nil, err
}

func (s *SessionSource) fromOutpost(ctx context.Context) (*Session, error) {
	tokenBytes, err := os.ReadFile(s.cfg.InstanceTokenFile)
	if err != nil {
		return nil, fmt.Errorf("read instance token: %w", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return nil, fmt.Errorf("instance token file is empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.cfg.OutpostURL, "/")+"/auth/sncs-session", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("outpost HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var sess Session
	if err := json.Unmarshal(body, &sess); err != nil {
		return nil, fmt.Errorf("decode outpost session: %w", err)
	}
	s.fillDefaults(&sess)
	if err := sess.validate(); err != nil {
		return nil, err
	}
	return &sess, nil
}

func (s *SessionSource) fromFile() (*Session, error) {
	body, err := os.ReadFile(s.cfg.SessionFile)
	if err != nil {
		return nil, fmt.Errorf("read session file: %w", err)
	}
	var sess Session
	if err := json.Unmarshal(body, &sess); err != nil {
		return nil, fmt.Errorf("decode session file: %w", err)
	}
	s.fillDefaults(&sess)
	if err := sess.validate(); err != nil {
		return nil, err
	}
	return &sess, nil
}

func (s *SessionSource) fillDefaults(sess *Session) {
	if sess.Surface == "" {
		sess.Surface = s.cfg.Surface
	}
	if sess.OutpostID == "" {
		sess.OutpostID = s.cfg.OutpostID
	}
	if sess.WorkingSessionID == "" {
		sess.WorkingSessionID = s.cfg.WorkingSessionID
	}
	if sess.Provider == "" {
		sess.Provider = "enterprise"
	}
}

func (sess *Session) validate() error {
	if sess.jwt() == "" {
		return fmt.Errorf("session has no jwt/id_token; run the Python login helper")
	}
	if sess.WSHost == "" {
		return fmt.Errorf("session has no ws_host; run the Python login helper")
	}
	return nil
}
