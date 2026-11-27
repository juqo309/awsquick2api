package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type tokenEntry struct {
	Email     string `json:"email"`
	CreatedAt int64  `json:"created_at"`
}

type tokenFile struct {
	Tokens map[string]tokenEntry `json:"tokens"`
}

// TokenStore validates proxy API keys minted by the Python auth helper.
// When no token file exists (or it contains no tokens) authentication is
// disabled, which keeps a purely local deployment frictionless.
type TokenStore struct {
	path string

	mu     sync.Mutex
	mod    time.Time
	tokens map[string]tokenEntry
	bad    bool
}

func NewTokenStore(path string) *TokenStore {
	return &TokenStore{path: path}
}

func (t *TokenStore) reload() {
	if t.path == "" {
		return
	}
	st, err := os.Stat(t.path)
	if err != nil {
		t.tokens = nil
		t.mod = time.Time{}
		t.bad = false
		return
	}
	if st.ModTime().Equal(t.mod) {
		return
	}
	body, err := os.ReadFile(t.path)
	if err != nil {
		t.bad = true
		return
	}
	var tf tokenFile
	if err := json.Unmarshal(body, &tf); err != nil {
		t.bad = true
		t.tokens = nil
		t.mod = st.ModTime()
		return
	}
	t.tokens = tf.Tokens
	t.bad = false
	t.mod = st.ModTime()
}

func (t *TokenStore) tokenFromRequest(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
			return strings.TrimSpace(h[7:])
		}
	}
	if h := r.Header.Get("X-Api-Key"); h != "" {
		return strings.TrimSpace(h)
	}
	return ""
}

// Allowed reports whether the request may use the proxy.
func (t *TokenStore) Allowed(r *http.Request) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reload()
	if t.bad {
		return false
	}
	if len(t.tokens) == 0 {
		return true
	}
	token := t.tokenFromRequest(r)
	if token == "" {
		return false
	}
	_, ok := t.tokens[token]
	return ok
}

type KeyView struct {
	Token     string `json:"token"`
	Hint      string `json:"hint"`
	Email     string `json:"email"`
	CreatedAt int64  `json:"created_at"`
}

func maskToken(token string) string {
	if len(token) <= 8 {
		return "••••"
	}
	return token[:4] + "…" + token[len(token)-4:]
}

func (t *TokenStore) List() (keys []KeyView, required bool, corrupt bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reload()
	required = len(t.tokens) > 0
	corrupt = t.bad
	for token, e := range t.tokens {
		keys = append(keys, KeyView{
			Token:     token,
			Hint:      maskToken(token),
			Email:     e.Email,
			CreatedAt: e.CreatedAt,
		})
	}
	return keys, required, corrupt
}

func (t *TokenStore) Create(email string) (string, KeyView, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reload()
	if t.bad {
		return "", KeyView{}, fmt.Errorf("token file is unreadable")
	}
	var raw [18]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", KeyView{}, err
	}
	token := "sk-" + hex.EncodeToString(raw[:])
	if t.tokens == nil {
		t.tokens = map[string]tokenEntry{}
	}
	entry := tokenEntry{Email: strings.TrimSpace(email), CreatedAt: time.Now().Unix()}
	t.tokens[token] = entry
	if err := t.saveLocked(); err != nil {
		delete(t.tokens, token)
		return "", KeyView{}, err
	}
	return token, KeyView{Token: token, Hint: maskToken(token), Email: entry.Email, CreatedAt: entry.CreatedAt}, nil
}

func (t *TokenStore) Revoke(token string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reload()
	if t.bad {
		return fmt.Errorf("token file is unreadable")
	}
	if _, ok := t.tokens[token]; !ok {
		return fmt.Errorf("key not found")
	}
	delete(t.tokens, token)
	return t.saveLocked()
}

func (t *TokenStore) saveLocked() error {
	if t.path == "" {
		return fmt.Errorf("no token file configured")
	}
	if t.tokens == nil {
		t.tokens = map[string]tokenEntry{}
	}
	body, err := json.MarshalIndent(tokenFile{Tokens: t.tokens}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(t.path), 0o700); err != nil {
		return err
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, t.path); err != nil {
		return err
	}
	st, err := os.Stat(t.path)
	if err == nil {
		t.mod = st.ModTime()
	}
	t.bad = false
	return nil
}
