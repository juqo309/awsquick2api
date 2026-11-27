package proxy

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Account struct {
	ID              string          `json:"id"`
	Label           string          `json:"label"`
	Email           string          `json:"email"`
	DisplayName     string          `json:"display_name"`
	Provider        string          `json:"provider"`
	Region          string          `json:"region"`
	AWSAccount      string          `json:"aws_account_id"`
	Proxy           string          `json:"proxy"`
	Enabled         bool            `json:"enabled"`
	CreatedAt       int64           `json:"created_at"`
	UpdatedAt       int64           `json:"updated_at"`
	Session         Session         `json:"session"`
	Upstream        json.RawMessage `json:"upstream"`
	Quota           *AccountQuota   `json:"quota,omitempty"`
	ChatAgentID     string          `json:"chat_agent_id,omitempty"`
	ManagedAgentIDs []string        `json:"managed_agent_ids,omitempty"`
}

type accountFile struct {
	Accounts []Account `json:"accounts"`
}

type AccountStore struct {
	path       string
	mu         sync.Mutex
	data       accountFile
	cursor     uint64
	admitCheck func(Account) (Account, error)
}

func (s *AccountStore) SetAdmitCheck(fn func(Account) (Account, error)) {
	s.admitCheck = fn
}

func NewAccountStore(path, sessionFile, upstreamFile string) *AccountStore {
	s := &AccountStore{path: path}
	s.data.Accounts = []Account{}
	if !s.load() {
		s.migrate(sessionFile, upstreamFile)
	}
	return s
}

func (s *AccountStore) load() bool {
	if s.path == "" {
		return false
	}
	body, err := os.ReadFile(s.path)
	if err != nil {
		return !os.IsNotExist(err)
	}
	var f accountFile
	if json.Unmarshal(body, &f) != nil {
		return true
	}
	if f.Accounts == nil {
		f.Accounts = []Account{}
	}
	s.data = f
	return true
}

func (s *AccountStore) persistLocked() error {
	if s.path == "" {
		return fmt.Errorf("no accounts file")
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *AccountStore) migrate(sessionFile, upstreamFile string) {
	body, err := os.ReadFile(sessionFile)
	if err != nil {
		return
	}
	var sess Session
	if json.Unmarshal(body, &sess) != nil || sess.validate() != nil {
		return
	}
	up, _ := os.ReadFile(upstreamFile)
	if len(up) == 0 {
		up = []byte("{}")
	}
	user := map[string]any{}
	var blob map[string]any
	if json.Unmarshal(up, &blob) == nil {
		if u, ok := blob["user_info"].(map[string]any); ok {
			user = u
		}
	}
	now := time.Now().Unix()
	acc := Account{
		Email:       strAny(user["email"]),
		DisplayName: strAny(user["display_name"]),
		Provider:    sess.Provider,
		Region:      sess.Region,
		AWSAccount:  sess.AccountID,
		Enabled:     true,
		CreatedAt:   now,
		UpdatedAt:   now,
		Session:     sess,
		Upstream:    json.RawMessage(up),
		Quota:       quotaFromUpstream(up, now),
	}
	if acc.DisplayName == "" {
		acc.DisplayName = strAny(user["user_name"])
	}
	_, _, _ = s.proveThenAdmit(acc, -1)
}

func newAccountID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "acc_" + hex.EncodeToString(b[:])
}

func (s *AccountStore) Snapshot() []Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Account(nil), s.data.Accounts...)
}

func publicAccount(a Account, now int64) map[string]any {
	exp := a.Session.UsableUntil
	return map[string]any{
		"id":            a.ID,
		"label":         a.Label,
		"email":         a.Email,
		"display_name":  a.DisplayName,
		"provider":      a.Provider,
		"region":        a.Region,
		"aws_account":   a.AWSAccount,
		"proxy":         a.Proxy,
		"enabled":       a.Enabled,
		"account_name":  a.Session.AccountName,
		"actor_arn":     a.Session.ActorARN,
		"obtained_at":   a.Session.ObtainedAt,
		"expires_at":    exp,
		"expires_in":    exp - now,
		"expired":       exp > 0 && exp <= now,
		"has_refresh":   hasRefresh(a.Upstream),
		"ws_host":       a.Session.WSHost,
		"created_at":    a.CreatedAt,
		"updated_at":    a.UpdatedAt,
		"quota":         a.Quota,
		"chat_agent_id": a.ChatAgentID,
	}
}

func hasRefresh(raw json.RawMessage) bool {
	var blob map[string]any
	if json.Unmarshal(raw, &blob) != nil {
		return false
	}
	tok, _ := blob["tokens"].(map[string]any)
	if tok == nil {
		return false
	}
	rt, _ := tok["refresh_token"].(string)
	return strings.TrimSpace(rt) != ""
}

func (s *AccountStore) Get(id string) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(id)
}

func (s *AccountStore) getLocked(id string) (Account, error) {
	for _, a := range s.data.Accounts {
		if a.ID == id {
			return a, nil
		}
	}
	return Account{}, fmt.Errorf("account not found")
}

func requestedAccount(r *http.Request) string {
	if id := strings.TrimSpace(r.Header.Get("X-Account-Id")); id != "" {
		return id
	}
	return strings.TrimSpace(r.URL.Query().Get("account_id"))
}

// Without an explicit selection, route stateless calls round-robin. A thread
// ID picks a stable account while that account remains eligible.
func (s *AccountStore) Select(thread string, now int64) (Account, bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	eligible := make([]Account, 0, len(s.data.Accounts))
	for _, a := range s.data.Accounts {
		if a.Enabled && (a.Session.UsableUntil == 0 || a.Session.UsableUntil > now) && a.Session.validate() == nil {
			eligible = append(eligible, a)
		}
	}
	if len(eligible) == 0 {
		return Account{}, false, len(s.data.Accounts) > 0
	}
	if thread == "" {
		a := eligible[s.cursor%uint64(len(eligible))]
		s.cursor++
		return a, true, true
	}
	best := eligible[0]
	score := sha256.Sum256([]byte(thread + "\x00" + best.ID))
	for _, a := range eligible[1:] {
		candidate := sha256.Sum256([]byte(thread + "\x00" + a.ID))
		if bytes.Compare(candidate[:], score[:]) > 0 {
			best, score = a, candidate
		}
	}
	return best, true, true
}

func (s *AccountStore) SetSettings(id, proxy, label string, enabled *bool) error {
	proxy, err := normalizeProxyURL(proxy)
	if err != nil {
		return err
	}
	return s.updateSettings(id, func(a *Account) {
		a.Proxy = proxy
		if label != "" {
			a.Label = strings.TrimSpace(label)
		}
		if enabled != nil {
			a.Enabled = *enabled
		}
	})
}

func (s *AccountStore) SetEnabled(id string, enabled bool) error {
	return s.updateSettings(id, func(a *Account) { a.Enabled = enabled })
}

func (s *AccountStore) SetChatAgentID(id, agentID string) error {
	agentID = strings.TrimSpace(agentID)
	if len(agentID) > 80 || strings.ContainsAny(agentID, " \t\r\n") {
		return fmt.Errorf("invalid chat agent id")
	}
	return s.updateSettings(id, func(a *Account) {
		// Keep legacy selections discoverable even after switching to another agent.
		for _, id := range []string{a.ChatAgentID, agentID} {
			if id != "" && !slices.Contains(a.ManagedAgentIDs, id) {
				a.ManagedAgentIDs = append(slices.Clone(a.ManagedAgentIDs), id)
			}
		}
		a.ChatAgentID = agentID
	})
}

func (s *AccountStore) trackAgent(accountID, agentID string, keep bool) error {
	return s.updateSettings(accountID, func(a *Account) {
		if keep {
			if !slices.Contains(a.ManagedAgentIDs, agentID) {
				a.ManagedAgentIDs = append(slices.Clone(a.ManagedAgentIDs), agentID)
			}
			return
		}
		a.ManagedAgentIDs = slices.DeleteFunc(slices.Clone(a.ManagedAgentIDs), func(id string) bool { return id == agentID })
		if a.ChatAgentID == agentID {
			a.ChatAgentID = ""
		}
	})
}

func (s *AccountStore) UpdateQuota(id string, quota AccountQuota) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Accounts {
		if s.data.Accounts[i].ID != id {
			continue
		}
		previous := s.data.Accounts[i]
		q := quota
		s.data.Accounts[i].Quota = &q
		if err := s.persistLocked(); err != nil {
			s.data.Accounts[i] = previous
			return err
		}
		return nil
	}
	return fmt.Errorf("account not found")
}

func (s *AccountStore) updateSettings(id string, update func(*Account)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Accounts {
		if s.data.Accounts[i].ID != id {
			continue
		}
		previous := s.data.Accounts[i]
		update(&s.data.Accounts[i])
		s.data.Accounts[i].UpdatedAt = time.Now().Unix()
		if err := s.persistLocked(); err != nil {
			s.data.Accounts[i] = previous
			return err
		}
		return nil
	}
	return fmt.Errorf("account not found")
}

func (s *AccountStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.data.Accounts {
		if a.ID != id {
			continue
		}
		previous := s.data.Accounts
		s.data.Accounts = append(append([]Account(nil), previous[:i]...), previous[i+1:]...)
		if err := s.persistLocked(); err != nil {
			s.data.Accounts = previous
			return err
		}
		return nil
	}
	return fmt.Errorf("account not found")
}

func loadLogin(dir string) (Account, error) {
	sessBody, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil {
		return Account{}, fmt.Errorf("read session: %w", err)
	}
	var sess Session
	if err := json.Unmarshal(sessBody, &sess); err != nil {
		return Account{}, err
	}
	if err := sess.validate(); err != nil {
		return Account{}, err
	}
	up, _ := os.ReadFile(filepath.Join(dir, "upstream.json"))
	if len(up) == 0 {
		up = []byte("{}")
	}
	user := map[string]any{}
	var blob map[string]any
	if json.Unmarshal(up, &blob) == nil {
		if u, ok := blob["user_info"].(map[string]any); ok {
			user = u
		}
	}
	email := strAny(user["email"])
	name := strAny(user["display_name"])
	if name == "" {
		name = strAny(user["user_name"])
	}
	return Account{Email: email, DisplayName: name, Provider: sess.Provider, Region: sess.Region,
		AWSAccount: sess.AccountID, Session: sess, Upstream: json.RawMessage(up), Quota: quotaFromUpstream(up, time.Now().Unix())}, nil
}

func (s *AccountStore) ImportLogin(dir, proxy, replaceID, expectedEmail, nickname string) (Account, error) {
	identity, err := loadLogin(dir)
	if err != nil {
		return Account{}, err
	}
	if expectedEmail != "" && !strings.EqualFold(expectedEmail, strings.TrimSpace(identity.Email)) {
		return Account{}, fmt.Errorf("the Quick sign-in email did not match the selected Kiro account")
	}
	now := time.Now().Unix()
	s.mu.Lock()
	idx := -1
	if replaceID != "" {
		for i := range s.data.Accounts {
			if s.data.Accounts[i].ID == replaceID {
				idx = i
				break
			}
		}
		if idx < 0 {
			s.mu.Unlock()
			return Account{}, fmt.Errorf("account was removed during sign-in")
		}
	}
	if idx < 0 && identity.Email != "" {
		for i := range s.data.Accounts {
			if strings.EqualFold(s.data.Accounts[i].Email, identity.Email) && s.data.Accounts[i].Provider == identity.Provider &&
				s.data.Accounts[i].AWSAccount == identity.AWSAccount && s.data.Accounts[i].Session.TenantURL == identity.Session.TenantURL {
				idx = i
				break
			}
		}
	}
	if idx >= 0 {
		previous := s.data.Accounts[idx]
		if replaceID != "" && (previous.Provider != identity.Provider || (previous.AWSAccount != "" && previous.AWSAccount != identity.AWSAccount)) {
			s.mu.Unlock()
			return Account{}, fmt.Errorf("the Quick sign-in did not match the original AWS account")
		}
		identity.ID, identity.Label, identity.CreatedAt = previous.ID, previous.Label, previous.CreatedAt
		identity.Proxy = previous.Proxy
		if proxy != "" {
			identity.Proxy = proxy
		}
		identity.Enabled = previous.Enabled
		identity.ChatAgentID = previous.ChatAgentID
		identity.ManagedAgentIDs = previous.ManagedAgentIDs
		if identity.Quota == nil {
			identity.Quota = previous.Quota
		}
		identity.UpdatedAt = now
	} else {
		identity.ID = newAccountID()
		identity.Label = nickname
		identity.Proxy, identity.Enabled = proxy, true
		identity.CreatedAt, identity.UpdatedAt = now, now
		idx = -1
	}
	s.mu.Unlock()
	admitted, _, err := s.proveThenAdmit(identity, idx)
	return admitted, err
}

// admitUsableLocked is the only place a fully credentialed, usable account
// enters the rotation pool. idx < 0 inserts; idx >= 0 replaces that slot.
// Caller holds s.mu. identity must already carry session + upstream secrets.
func (s *AccountStore) admitUsableLocked(identity Account, idx int) (Account, bool, error) {
	if err := identity.Session.validate(); err != nil {
		return Account{}, false, err
	}
	created := idx < 0
	var previous Account
	if created {
		if identity.ID == "" {
			identity.ID = newAccountID()
		}
		s.data.Accounts = append(s.data.Accounts, identity)
		idx = len(s.data.Accounts) - 1
	} else {
		previous = s.data.Accounts[idx]
		s.data.Accounts[idx] = identity
	}
	if err := s.persistLocked(); err != nil {
		if created {
			s.data.Accounts = s.data.Accounts[:len(s.data.Accounts)-1]
		} else {
			s.data.Accounts[idx] = previous
		}
		return Account{}, false, err
	}
	return identity, created, nil
}

func (s *AccountStore) UpsertImported(identity Account) (Account, bool, error) {
	now := time.Now().Unix()
	s.mu.Lock()
	idx := -1
	if identity.Email != "" {
		for i := range s.data.Accounts {
			if strings.EqualFold(s.data.Accounts[i].Email, identity.Email) && s.data.Accounts[i].Provider == identity.Provider &&
				(identity.AWSAccount == "" || s.data.Accounts[i].AWSAccount == "" || s.data.Accounts[i].AWSAccount == identity.AWSAccount) {
				idx = i
				break
			}
		}
	}
	if idx >= 0 {
		previous := s.data.Accounts[idx]
		identity.ID = previous.ID
		if identity.Label == "" {
			identity.Label = previous.Label
		}
		if identity.Proxy == "" {
			identity.Proxy = previous.Proxy
		}
		identity.CreatedAt = previous.CreatedAt
		identity.ChatAgentID = previous.ChatAgentID
		identity.ManagedAgentIDs = previous.ManagedAgentIDs
		if identity.Quota == nil {
			identity.Quota = previous.Quota
		}
		identity.UpdatedAt = now
	} else {
		if identity.ID == "" {
			identity.ID = newAccountID()
		}
		identity.CreatedAt, identity.UpdatedAt = now, now
	}
	s.mu.Unlock()
	return s.proveThenAdmit(identity, idx)
}

func (s *AccountStore) UpdateRefreshed(previous, fresh Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.data.Accounts {
		if a.ID != previous.ID {
			continue
		}
		if a.Session != previous.Session || !bytes.Equal(a.Upstream, previous.Upstream) {
			return fmt.Errorf("account was signed in again during refresh")
		}
		fresh.ID, fresh.Label, fresh.CreatedAt = a.ID, a.Label, a.CreatedAt
		fresh.ManagedAgentIDs = a.ManagedAgentIDs
		fresh.Proxy, fresh.Enabled, fresh.ChatAgentID, fresh.UpdatedAt = a.Proxy, a.Enabled, a.ChatAgentID, time.Now().Unix()
		if fresh.Quota == nil {
			fresh.Quota = a.Quota
		}
		s.data.Accounts[i] = fresh
		if err := s.persistLocked(); err != nil {
			s.data.Accounts[i] = a
			return err
		}
		return nil
	}
	return fmt.Errorf("account was removed during refresh")
}

func (acc Account) WriteWorkDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	sb, err := json.MarshalIndent(acc.Session, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "session.json"), append(sb, '\n'), 0o600); err != nil {
		return err
	}
	up := acc.Upstream
	if len(up) == 0 {
		up = []byte("{}\n")
	}
	return os.WriteFile(filepath.Join(dir, "upstream.json"), up, 0o600)
}

func normalizeProxyURL(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	invalid := fmt.Errorf("use an HTTP, HTTPS, SOCKS5, or SOCKS5H proxy URL or host:port:username:password")
	host, rest, found := strings.Cut(value, ":")
	if !strings.HasPrefix(rest, "//") {
		if strings.HasPrefix(value, "[") {
			host, rest, found = strings.Cut(value, "]:")
			host += "]"
		}
		// Split only the first two separators after the host so passwords can contain colons.
		parts := strings.SplitN(rest, ":", 3)
		if !found || host == "" || strings.ContainsAny(host, " \t\r\n/?#@\\") || len(parts) != 3 || parts[0] == "" || strings.Trim(parts[0], "0123456789") != "" || parts[1] == "" {
			return "", invalid
		}
		proxy := url.URL{Scheme: "http", Host: host + ":" + parts[0], User: url.UserPassword(parts[1], parts[2])}
		value = proxy.String()
	}
	proxy, err := url.Parse(value)
	if err != nil || proxy.Hostname() == "" || strings.HasSuffix(proxy.Host, ":") || proxy.RawQuery != "" || proxy.Fragment != "" || (proxy.Path != "" && proxy.Path != "/") {
		return "", invalid
	}
	switch proxy.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", invalid
	}
	if port := proxy.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", invalid
		}
	}
	return proxy.String(), nil
}
