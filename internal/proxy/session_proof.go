package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type sessionProof struct {
	OK          bool            `json:"ok"`
	Error       string          `json:"error"`
	Email       string          `json:"email"`
	DisplayName string          `json:"display_name"`
	AWSAccount  string          `json:"aws_account_id"`
	Session     *Session        `json:"session"`
	Upstream    json.RawMessage `json:"upstream"`
}

func (s *AccountStore) proveThenAdmit(identity Account, idx int) (Account, bool, error) {
	if err := identity.Session.validate(); err != nil {
		return Account{}, false, err
	}
	if s.admitCheck != nil {
		proved, err := s.admitCheck(identity)
		if err != nil {
			return Account{}, false, err
		}
		identity = proved
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx = -1
	if identity.ID != "" {
		for i := range s.data.Accounts {
			if s.data.Accounts[i].ID == identity.ID {
				idx = i
				break
			}
		}
	}
	return s.admitUsableLocked(identity, idx)
}

func pythonSessionProof(cfg *Config) func(Account) (Account, error) {
	py := "python3"
	if cfg != nil && strings.TrimSpace(cfg.Python) != "" {
		py = cfg.Python
	}
	src := sessionProofSourceDir()
	return func(acc Account) (Account, error) {
		payload, err := json.Marshal(map[string]any{"account": acc})
		if err != nil {
			return Account{}, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, py, "-m", "quickwork_session", "prove")
		cmd.Stdin = bytes.NewReader(payload)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		cmd.Env = sessionProofEnv(src)
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = strings.TrimSpace(stdout.String())
			}
			if msg == "" {
				msg = err.Error()
			}
			return Account{}, fmt.Errorf("Quick session proof failed: %s", msg)
		}
		var proof sessionProof
		if err := json.Unmarshal(stdout.Bytes(), &proof); err != nil {
			return Account{}, fmt.Errorf("Quick session proof returned invalid json")
		}
		if !proof.OK {
			msg := strings.TrimSpace(proof.Error)
			if msg == "" {
				msg = "Amazon Quick rejected the session"
			}
			return Account{}, fmt.Errorf("Quick session proof failed: %s", msg)
		}
		return applySessionProof(acc, proof), nil
	}
}

func applySessionProof(acc Account, proof sessionProof) Account {
	if proof.Session != nil {
		if proof.Session.JWT != "" {
			acc.Session.JWT = proof.Session.JWT
		}
		if proof.Session.IDToken != "" {
			acc.Session.IDToken = proof.Session.IDToken
		}
		if proof.Session.UsableUntil > 0 {
			acc.Session.UsableUntil = proof.Session.UsableUntil
		}
		if proof.Session.ObtainedAt > 0 {
			acc.Session.ObtainedAt = proof.Session.ObtainedAt
		}
	}
	if len(bytes.TrimSpace(proof.Upstream)) > 0 && !bytes.Equal(bytes.TrimSpace(proof.Upstream), []byte("null")) {
		acc.Upstream = json.RawMessage(proof.Upstream)
	}
	if proof.Email != "" {
		acc.Email = proof.Email
	}
	if proof.DisplayName != "" {
		acc.DisplayName = proof.DisplayName
	}
	if proof.AWSAccount != "" {
		acc.AWSAccount = proof.AWSAccount
		acc.Session.AccountID = proof.AWSAccount
	}
	return acc
}

func sessionProofSourceDir() string {
	if p := strings.TrimSpace(os.Getenv("QW_SESSION_PROOF_PATH")); p != "" {
		return p
	}
	candidates := []string{
		filepath.Join("..", "pypi", "src"),
		filepath.Join("pypi", "src"),
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "..", "pypi", "src"))
	}
	for _, c := range candidates {
		if st, err := os.Stat(filepath.Join(c, "quickwork_session")); err == nil && st.IsDir() {
			abs, err := filepath.Abs(c)
			if err == nil {
				return abs
			}
			return c
		}
	}
	return ""
}

func sessionProofEnv(src string) []string {
	env := os.Environ()
	if src == "" {
		return env
	}
	prefix := "PYTHONPATH=" + src
	for i, kv := range env {
		if strings.HasPrefix(kv, "PYTHONPATH=") {
			env[i] = prefix + string(os.PathListSeparator) + strings.TrimPrefix(kv, "PYTHONPATH=")
			return env
		}
	}
	return append(env, prefix)
}
