package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	xproxy "golang.org/x/net/proxy"
)

const (
	quotaRefreshInterval = 5 * time.Minute
	quotaErrorBackoff    = 2 * time.Minute
	quotaPostChatMinGap  = 15 * time.Second
	quotaRequestTimeout  = 15 * time.Second

	socialDataPlaneProd = "https://cell001.dp.appintegrations.us-east-1.prod.plato.ai.aws.dev"
	socialDataPlaneBeta = "https://cell001.dp.appintegrations.us-west-2.beta.plato.ai.aws.dev"
)

type AccountQuota struct {
	SubscriptionType string   `json:"subscription_type,omitempty"`
	Entitlement      string   `json:"entitlement_status,omitempty"`
	TrialStatus      string   `json:"trial_status,omitempty"`
	TrialExpiresAt   int64    `json:"trial_expires_at,omitempty"`
	MonthlyLimit     float64  `json:"monthly_limit,omitempty"`
	MonthlyAvailable float64  `json:"monthly_available,omitempty"`
	MonthlyUsed      float64  `json:"monthly_used,omitempty"`
	MonthlyPct       float64  `json:"monthly_pct,omitempty"`
	MonthlyResetsAt  int64    `json:"monthly_resets_at,omitempty"`
	SessionPct       *float64 `json:"session_pct,omitempty"`
	SessionResumeMin int      `json:"session_resume_minutes,omitempty"`
	OverageEnabled   bool     `json:"overage_enabled,omitempty"`
	UpdatedAt        int64    `json:"updated_at,omitempty"`
}

type usageSummaryPayload struct {
	EntitlementStatus string `json:"entitlementStatus"`
	OverageEnabled    bool   `json:"overageEnabled"`
	MonthlyUsage      *struct {
		AvailableUnits   float64  `json:"availableUnits"`
		ProvisionedUnits float64  `json:"provisionedUnits"`
		ResetsAt         int64    `json:"resetsAt"`
		UsedPercentage   *float64 `json:"usedPercentage"`
	} `json:"monthlyUsage"`
	SessionUsage *struct {
		ResumeInMinutes int      `json:"resumeInMinutes"`
		UsedPercentage  *float64 `json:"usedPercentage"`
	} `json:"sessionUsage"`
	Subscription *struct {
		SubscriptionType  string `json:"subscriptionType"`
		FreeTrialMetadata *struct {
			ExpiryDate int64  `json:"expiryDate"`
			Status     string `json:"status"`
		} `json:"freeTrialMetadata"`
	} `json:"subscription"`
}

func clampPct(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return math.Round(v*10) / 10
}

func parseUsageSummary(raw json.RawMessage, now int64) *AccountQuota {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("{}")) {
		return nil
	}
	var p usageSummaryPayload
	if err := json.Unmarshal(trimmed, &p); err != nil {
		return nil
	}
	q := &AccountQuota{
		Entitlement:    strings.TrimSpace(p.EntitlementStatus),
		OverageEnabled: p.OverageEnabled,
		UpdatedAt:      now,
	}
	hasData := q.Entitlement != ""
	if p.MonthlyUsage != nil {
		hasData = true
		q.MonthlyLimit = p.MonthlyUsage.ProvisionedUnits
		q.MonthlyAvailable = p.MonthlyUsage.AvailableUnits
		q.MonthlyResetsAt = p.MonthlyUsage.ResetsAt
		if q.MonthlyLimit > 0 {
			used := q.MonthlyLimit - q.MonthlyAvailable
			if used < 0 {
				used = 0
			}
			q.MonthlyUsed = math.Round(used*100) / 100
			q.MonthlyPct = clampPct((used / q.MonthlyLimit) * 100)
		} else if p.MonthlyUsage.UsedPercentage != nil {
			q.MonthlyPct = clampPct(*p.MonthlyUsage.UsedPercentage)
		}
	}
	if p.SessionUsage != nil {
		hasData = true
		q.SessionResumeMin = p.SessionUsage.ResumeInMinutes
		if p.SessionUsage.UsedPercentage != nil {
			pct := clampPct(*p.SessionUsage.UsedPercentage)
			q.SessionPct = &pct
		}
	}
	if p.Subscription != nil {
		hasData = true
		q.SubscriptionType = strings.TrimSpace(p.Subscription.SubscriptionType)
		if p.Subscription.FreeTrialMetadata != nil {
			q.TrialStatus = strings.TrimSpace(p.Subscription.FreeTrialMetadata.Status)
			q.TrialExpiresAt = p.Subscription.FreeTrialMetadata.ExpiryDate
		}
	}
	if !hasData {
		return nil
	}
	return q
}

func quotaFromUpstream(raw json.RawMessage, now int64) *AccountQuota {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var blob struct {
		UserInfo struct {
			UsageSummary      json.RawMessage `json:"usage_summary"`
			UsageSummaryCamel json.RawMessage `json:"usageSummary"`
		} `json:"user_info"`
		UsageSummary json.RawMessage `json:"usage_summary"`
	}
	if err := json.Unmarshal(raw, &blob); err != nil {
		return nil
	}
	for _, candidate := range []json.RawMessage{blob.UserInfo.UsageSummary, blob.UserInfo.UsageSummaryCamel, blob.UsageSummary} {
		if q := parseUsageSummary(candidate, now); q != nil {
			return q
		}
	}
	return nil
}

func accountAccessToken(a Account) string {
	var blob struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if len(a.Upstream) > 0 && json.Unmarshal(a.Upstream, &blob) == nil {
		if tok := strings.TrimSpace(blob.Tokens.AccessToken); tok != "" {
			return tok
		}
	}
	return strings.TrimSpace(a.Session.JWT)
}

func quotaEndpointURL(a Account) string {
	var blob struct {
		Provider   string `json:"provider"`
		BuildStage string `json:"build_stage"`
		TenantURL  string `json:"tenant_url"`
		Discovery  struct {
			TenantURL string `json:"tenant_url"`
		} `json:"discovery"`
	}
	if len(a.Upstream) > 0 {
		_ = json.Unmarshal(a.Upstream, &blob)
	}
	provider := strings.ToLower(strings.TrimSpace(a.Provider))
	if provider == "" {
		provider = strings.ToLower(strings.TrimSpace(blob.Provider))
	}
	tenant := strings.TrimSpace(blob.TenantURL)
	if tenant == "" {
		tenant = strings.TrimSpace(a.Session.TenantURL)
	}
	var base string
	if provider == "enterprise" || provider == "gateway" {
		base = strings.TrimSpace(blob.Discovery.TenantURL)
		if base == "" {
			base = tenant
		}
	} else {
		if strings.Contains(tenant, "dp.appintegrations") {
			base = tenant
		} else if blob.BuildStage != "" && blob.BuildStage != "prod" {
			base = socialDataPlaneBeta
		} else {
			base = socialDataPlaneProd
		}
	}
	base = strings.TrimRight(base, "/")
	if base == "" {
		return ""
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "https://" + base
	}
	return base + "/integration/quick-work/user-info"
}

func quotaHTTPClient(proxyURL string) *http.Client {
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
	return &http.Client{Timeout: quotaRequestTimeout, Transport: tr}
}

func fetchAccountQuota(ctx context.Context, a Account) (*AccountQuota, error) {
	endpoint := quotaEndpointURL(a)
	if endpoint == "" {
		return nil, fmt.Errorf("missing quota endpoint")
	}
	token := accountAccessToken(a)
	if token == "" {
		return nil, fmt.Errorf("missing access token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := quotaHTTPClient(a.Proxy).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("quota HTTP %d", resp.StatusCode)
	}
	var envelope struct {
		UsageSummary json.RawMessage `json:"usageSummary"`
		UserInfo     *struct {
			UsageSummary json.RawMessage `json:"usageSummary"`
		} `json:"userInfo"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	rawSummary := envelope.UsageSummary
	if len(bytes.TrimSpace(rawSummary)) == 0 && envelope.UserInfo != nil {
		rawSummary = envelope.UserInfo.UsageSummary
	}
	q := parseUsageSummary(rawSummary, time.Now().Unix())
	if q == nil {
		return nil, fmt.Errorf("no usageSummary in response")
	}
	return q, nil
}

type quotaState struct {
	running     bool
	lastAttempt int64
	nextRetry   int64
}

type quotaManager struct {
	mu        sync.Mutex
	bgEnabled bool
	states    map[string]quotaState
}

func (m *quotaManager) enableBackground() {
	m.mu.Lock()
	m.bgEnabled = true
	m.mu.Unlock()
}

func (s *Server) refreshDueQuotas(ctx context.Context) {
	now := time.Now().Unix()
	for _, a := range s.accounts.Snapshot() {
		if ctx.Err() != nil {
			return
		}
		if !a.Enabled || (a.Session.UsableUntil > 0 && a.Session.UsableUntil <= now) {
			continue
		}
		if a.Quota != nil && now-a.Quota.UpdatedAt < int64(quotaRefreshInterval.Seconds()) {
			continue
		}
		_ = s.refreshAccountQuota(ctx, a.ID, false)
	}
}

func (s *Server) scheduleQuotaRefresh(id string) {
	if id == "" || s.accounts == nil {
		return
	}
	m := &s.quotas
	m.mu.Lock()
	if !m.bgEnabled {
		m.mu.Unlock()
		return
	}
	st := m.states[id]
	now := time.Now().Unix()
	if st.running || now-st.lastAttempt < int64(quotaPostChatMinGap.Seconds()) {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), quotaRequestTimeout)
		defer cancel()
		_ = s.refreshAccountQuota(ctx, id, true)
	}()
}

func (s *Server) refreshAccountQuota(ctx context.Context, id string, force bool) (err error) {
	m := &s.quotas
	m.mu.Lock()
	if m.states == nil {
		m.states = make(map[string]quotaState)
	}
	st := m.states[id]
	now := time.Now().Unix()
	if st.running || (!force && st.nextRetry > now) {
		m.mu.Unlock()
		return nil
	}
	a, err := s.accounts.Get(id)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if a.Session.UsableUntil > 0 && a.Session.UsableUntil <= now {
		m.mu.Unlock()
		return nil
	}
	st.running = true
	st.lastAttempt = now
	m.states[id] = st
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if _, exists := s.accounts.Get(id); exists != nil {
			delete(m.states, id)
			return
		}
		st.running = false
		if err == nil {
			st.nextRetry = time.Now().Add(quotaPostChatMinGap).Unix()
		} else if ctx.Err() == nil {
			st.nextRetry = time.Now().Add(quotaErrorBackoff).Unix()
		}
		m.states[id] = st
	}()

	q, err := fetchAccountQuota(ctx, a)
	if err != nil {
		return err
	}
	return s.accounts.UpdateQuota(id, *q)
}
