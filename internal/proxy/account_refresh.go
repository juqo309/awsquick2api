package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

const refreshWindow = 5 * time.Minute

type refreshStatus struct {
	Running     bool  `json:"running"`
	LastSuccess int64 `json:"last_success,omitempty"`
	LastFailure int64 `json:"last_failure,omitempty"`
	NextRetry   int64 `json:"next_retry,omitempty"`
}

var errRefreshRunning = errors.New("session refresh is already running")

type refreshManager struct {
	mu   sync.Mutex
	jobs map[string]refreshStatus
}

func (m *refreshManager) status(id string) refreshStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[id]
}

// RunAccountRefresher runs independently of the dashboard and stops on shutdown.
func (s *Server) RunAccountRefresher(ctx context.Context) {
	s.quotas.enableBackground()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		s.refreshDue(ctx)
		s.refreshDueQuotas(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func needsRefresh(a Account, now time.Time) bool {
	return a.Enabled && a.Session.UsableUntil > 0 && a.Session.UsableUntil <= now.Add(refreshWindow).Unix() && hasRefresh(a.Upstream)
}

func (s *Server) refreshDue(ctx context.Context) {
	for _, a := range s.accounts.Snapshot() {
		if ctx.Err() != nil {
			return
		}
		if needsRefresh(a, time.Now()) {
			// Run at most one background refresh at a time; manual refreshes of
			// other accounts may run independently.
			_ = s.refreshAccount(ctx, a.ID, false)
		}
	}
}

func (s *Server) refreshAccount(ctx context.Context, id string, manual bool) (err error) {
	m := &s.refreshes
	m.mu.Lock()
	st := m.jobs[id]
	if st.Running {
		m.mu.Unlock()
		return errRefreshRunning
	}
	if !manual && st.NextRetry > time.Now().Unix() {
		m.mu.Unlock()
		return nil
	}
	a, err := s.accounts.Get(id)
	if err != nil || (!manual && !needsRefresh(a, time.Now())) {
		m.mu.Unlock()
		return err
	}
	if !hasRefresh(a.Upstream) {
		m.mu.Unlock()
		return errors.New("no refresh credential; connect the account again")
	}
	if m.jobs == nil {
		m.jobs = make(map[string]refreshStatus)
	}
	st.Running = true
	m.jobs[id] = st
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if _, exists := s.accounts.Get(id); exists != nil {
			delete(m.jobs, id)
			return
		}
		now := time.Now()
		st.Running = false
		if err == nil {
			st.LastSuccess, st.LastFailure, st.NextRetry = now.Unix(), 0, now.Add(time.Minute).Unix()
		} else if ctx.Err() == nil {
			// Do not store upstream error bodies: they can contain credential material.
			st.LastFailure, st.NextRetry = now.Unix(), now.Add(5*time.Minute).Unix()
		}
		m.jobs[id] = st
	}()

	dir, err := os.MkdirTemp("", "qwp-refresh-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := a.WriteWorkDir(dir); err != nil {
		return err
	}
	jobCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := s.runAuthDir(jobCtx, dir, a.Proxy, "refresh"); err != nil {
		return fmt.Errorf("Quick session refresh failed; if it persists, connect the account again")
	}
	fresh, err := loadLogin(dir)
	if err != nil {
		return errors.New("Quick did not return a usable session")
	}
	if fresh.Session.UsableUntil <= time.Now().Unix() {
		return errors.New("Quick returned an expired session")
	}
	return s.accounts.UpdateRefreshed(a, fresh)
}
