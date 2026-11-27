package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type LoginJob struct {
	mu            sync.Mutex
	cmd           *exec.Cmd
	state         string
	url           string
	authorizeURL  string
	relaying      bool
	err           string
	provider      string
	dataDir       string
	proxy         string
	replaceID     string
	accountID     string
	expectedEmail string
	nickname      string
}

func newLoginJob() *LoginJob {
	return &LoginJob{state: "idle"}
}

func (j *LoginJob) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	return map[string]any{
		"state":         j.state,
		"url":           j.url,
		"authorize_url": j.authorizeURL,
		"error":         j.err,
		"provider":      j.provider,
		"account_id":    j.accountID,
	}
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.panel.Enabled() && !isLoopback(r) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "set a dashboard password to use the admin API remotely"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/admin")
	switch {
	case r.Method == http.MethodGet && (path == "/account" || path == "/accounts"):
		s.adminAccounts(w)
	case r.Method == http.MethodPost && path == "/accounts/active":
		writeJSON(w, http.StatusGone, map[string]any{"error": "accounts are selected automatically; pass X-Account-Id to select one explicitly"})
	case r.Method == http.MethodPost && path == "/accounts/proxy":
		s.adminAccountProxy(w, r)
	case r.Method == http.MethodPost && path == "/accounts/enabled":
		s.adminAccountEnabled(w, r)
	case r.Method == http.MethodDelete && path == "/accounts":
		s.adminAccountDelete(w, r)
	case r.Method == http.MethodPost && path == "/accounts/refresh":
		s.adminAccountRefresh(w, r)
	case r.Method == http.MethodPost && path == "/accounts/import":
		s.adminAccountsImport(w, r)
	case r.Method == http.MethodGet && path == "/accounts/export":
		s.adminAccountsExport(w, r)
	case (r.Method == http.MethodGet || r.Method == http.MethodPost) && path == "/accounts/agent":
		s.adminAccountAgent(w, r)
	case r.Method == http.MethodPost && path == "/accounts/agents/apply-claude":
		s.adminApplyClaude(w, r)
	case r.Method == http.MethodGet && path == "/stats":
		s.adminStats(w, r)
	case r.Method == http.MethodGet && path == "/requests":
		s.adminRequests(w, r)
	case r.Method == http.MethodPost && path == "/stats/reset":
		s.stats.Reset()
		writeJSON(w, http.StatusOK, s.stats.Snapshot(50))
	case r.Method == http.MethodPost && path == "/stats/clear-errors":
		s.stats.ClearErrors()
		writeJSON(w, http.StatusOK, s.stats.Snapshot(50))
	case r.Method == http.MethodGet && path == "/keys":
		s.adminKeysList(w)
	case r.Method == http.MethodPost && path == "/keys":
		s.adminKeysCreate(w, r)
	case r.Method == http.MethodDelete && path == "/keys":
		s.adminKeysRevoke(w, r)
	case r.Method == http.MethodPost && path == "/refresh":
		s.adminAccountRefresh(w, r)
	case r.Method == http.MethodPost && path == "/logout":
		s.adminLogout(w)
	case r.Method == http.MethodPost && path == "/login/start":
		s.adminLoginStart(w, r)
	case r.Method == http.MethodGet && path == "/login/status":
		writeJSON(w, http.StatusOK, s.login.snapshot())
	case r.Method == http.MethodPost && path == "/login/callback":
		s.adminLoginCallback(w, r)
	case r.Method == http.MethodPost && path == "/login/cancel":
		s.adminLoginCancel(w)
	case r.Method == http.MethodPost && (path == "/probe" || path == "/chat"):
		s.adminProbe(w, r)
	case (r.Method == http.MethodGet || r.Method == http.MethodPost) && path == "/settings":
		s.adminSettings(w, r)
	case r.Method == http.MethodGet && path == "/config":
		s.adminConfig(w)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown admin route"})
	}
}

func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) adminConfig(w http.ResponseWriter) {
	settings := s.settings.snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"addr":            s.cfg.Addr,
		"models":          settings.Models,
		"default_model":   settings.DefaultModel,
		"timezone":        settings.Timezone,
		"locale":          settings.Locale,
		"surface":         settings.Surface,
		"app_version":     settings.AppVersion,
		"keep_sessions":   settings.KeepSessions,
		"full_context":    settings.FullContext,
		"timeout_sec":     settings.TimeoutSec,
		"thinking_effort": settings.ThinkingEffort,
	})
}

func (s *Server) adminAccounts(w http.ResponseWriter) {
	now := time.Now().Unix()
	accounts := s.accounts.Snapshot()
	usage := s.stats.AccountUsage()
	list := make([]map[string]any, 0, len(accounts))
	for _, account := range accounts {
		row := publicAccount(account, now)
		row["refresh"] = s.refreshes.status(account.ID)
		row["usage"] = usage[account.ID]
		list = append(list, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": list, "login": s.login.snapshot(), "now": now, "signed_in": len(list) > 0})
}

func (s *Server) adminAccountProxy(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID      string `json:"id"`
		Proxy   string `json:"proxy"`
		Label   string `json:"label"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id is required"})
		return
	}
	if err := s.accounts.SetSettings(body.ID, body.Proxy, body.Label, body.Enabled); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	s.adminAccounts(w)
}

func (s *Server) adminAccountEnabled(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID      string `json:"id"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" || body.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id and enabled are required"})
		return
	}
	if err := s.accounts.SetEnabled(strings.TrimSpace(body.ID), *body.Enabled); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	s.adminAccounts(w)
}

func (s *Server) adminAccountDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id is required"})
		return
	}
	if err := s.accounts.Delete(body.ID); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	s.adminAccounts(w)
}

func (s *Server) adminAccountRefresh(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "account id is required"})
		return
	}
	if err := s.refreshAccount(r.Context(), strings.TrimSpace(body.ID), true); err != nil {
		status := http.StatusBadGateway
		if err == errRefreshRunning {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]any{"error": err.Error()})
		return
	}
	s.adminAccounts(w)
}

func strAny(v any) string {
	s, _ := v.(string)
	return s
}

func (s *Server) adminStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.stats.Snapshot(40))
}

func (s *Server) adminRequests(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"recent": s.stats.Snapshot(200).Recent})
}

func (s *Server) adminKeysList(w http.ResponseWriter) {
	keys, required, corrupt := s.tokens.List()
	if keys == nil {
		keys = []KeyView{}
	}
	safe := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		safe = append(safe, map[string]any{
			"hint":       k.Hint,
			"email":      k.Email,
			"created_at": k.CreatedAt,
			"token":      k.Token,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"keys":     safe,
		"required": required,
		"corrupt":  corrupt,
	})
}

func (s *Server) adminKeysCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)
	token, view, err := s.tokens.Create(body.Email)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":      token,
		"hint":       view.Hint,
		"email":      view.Email,
		"created_at": view.CreatedAt,
	})
}

func (s *Server) adminKeysRevoke(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || strings.TrimSpace(body.Token) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "token is required"})
		return
	}
	if err := s.tokens.Revoke(body.Token); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) runAuthDir(ctx context.Context, dir, proxy string, args ...string) error {
	py := s.cfg.Python
	if py == "" {
		py = "python3"
	}
	script := s.cfg.AuthScript
	if script == "" {
		script = "auth/quickauth.py"
	}
	cmd := exec.CommandContext(ctx, py, append([]string{script}, args...)...)
	if wd, err := os.Getwd(); err == nil {
		cmd.Dir = wd
	}
	env := os.Environ()
	if dir != "" {
		env = append(env, "QW_DATA_DIR="+dir)
	}
	if proxy != "" {
		env = append(env, "QW_PROXY="+proxy, "HTTP_PROXY="+proxy, "HTTPS_PROXY="+proxy, "ALL_PROXY="+proxy)
	}
	cmd.Env = env
	return cmd.Run()
}

func (s *Server) adminLogout(w http.ResponseWriter) {
	s.sessions.invalidate()
	writeJSON(w, http.StatusBadRequest, map[string]any{"error": "remove a specific account in Accounts"})
}

func (s *Server) adminLoginStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider      string `json:"provider"`
		Proxy         string `json:"proxy"`
		AccountID     string `json:"account_id"`
		ExpectedEmail string `json:"expected_email"`
		Nickname      string `json:"nickname"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid sign-in request"})
		return
	}
	provider := strings.TrimSpace(body.Provider)
	if provider == "" {
		provider = "social"
	}
	if provider != "social" && provider != "enterprise" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported sign-in method"})
		return
	}
	body.ExpectedEmail = strings.TrimSpace(body.ExpectedEmail)
	body.AccountID = strings.TrimSpace(body.AccountID)
	body.Nickname = strings.TrimSpace(body.Nickname)
	if len(body.ExpectedEmail) > 254 || len(body.Nickname) > 200 || (body.ExpectedEmail != "" && (provider != "social" || !strings.Contains(body.ExpectedEmail, "@") || strings.ContainsAny(body.ExpectedEmail, " \r\n\t"))) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid imported identity"})
		return
	}
	if body.AccountID != "" {
		a, err := s.accounts.Get(body.AccountID)
		if err != nil || a.Email == "" || a.Provider != provider || (body.ExpectedEmail != "" && !strings.EqualFold(body.ExpectedEmail, a.Email)) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "cannot reconnect this identity"})
			return
		}
		body.ExpectedEmail = a.Email
		if strings.TrimSpace(body.Proxy) == "" {
			body.Proxy = a.Proxy
		}
	}
	proxy, err := normalizeProxyURL(body.Proxy)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := s.login.start(s, provider, proxy, body.AccountID, body.ExpectedEmail, body.Nickname); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "login": s.login.snapshot()})
		return
	}
	writeJSON(w, http.StatusOK, s.login.snapshot())
}

func (s *Server) adminLoginCancel(w http.ResponseWriter) {
	s.login.cancel()
	writeJSON(w, http.StatusOK, s.login.snapshot())
}

func (j *LoginJob) start(s *Server, provider, proxy, replaceID, expectedEmail, nickname string) error {
	j.mu.Lock()
	if j.cmd != nil || j.relaying {
		j.mu.Unlock()
		return fmt.Errorf("login already waiting")
	}
	cfg := s.cfg
	py := cfg.Python
	if py == "" {
		py = "python3"
	}
	script := cfg.AuthScript
	if script == "" {
		script = "auth/quickauth.py"
	}
	dir, err := os.MkdirTemp("", "qwp-login-*")
	if err != nil {
		j.mu.Unlock()
		return err
	}
	cmd := exec.Command(py, script, "upstream-login", "--provider", provider, "--port", loginCallbackPort, "--timeout", "3600")
	if wd, err := os.Getwd(); err == nil {
		cmd.Dir = wd
	}
	env := append(os.Environ(), "QW_DATA_DIR="+dir)
	if proxy != "" {
		env = append(env, "QW_PROXY="+proxy, "HTTP_PROXY="+proxy, "HTTPS_PROXY="+proxy, "ALL_PROXY="+proxy)
	}
	cmd.Env = env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = os.RemoveAll(dir)
		j.mu.Unlock()
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(dir)
		j.mu.Unlock()
		return err
	}
	j.cmd = cmd
	j.state = "waiting"
	j.url = ""
	j.authorizeURL = ""
	j.err = ""
	j.provider = provider
	j.dataDir = dir
	j.proxy = proxy
	j.replaceID = replaceID
	j.expectedEmail = expectedEmail
	j.nickname = nickname
	j.accountID = ""
	j.mu.Unlock()
	go j.watch(s, stdout, cmd)
	return nil
}

func (j *LoginJob) watch(s *Server, r io.Reader, cmd *exec.Cmd) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	success := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "https://") || strings.HasPrefix(line, "http://") {
			j.mu.Lock()
			if j.state == "waiting" {
				j.url = line
			}
			j.mu.Unlock()
		}
		if strings.HasPrefix(line, "[quickauth] LOGIN_OK") {
			success = true
		}
	}
	err := cmd.Wait()
	j.mu.Lock()
	defer j.mu.Unlock()
	defer os.RemoveAll(j.dataDir)
	j.cmd = nil
	j.url = ""
	j.authorizeURL = ""
	if j.state != "waiting" {
		return
	}
	if err != nil || !success {
		j.state, j.err = "failed", "Quick sign-in did not complete. Start sign-in again."
		return
	}
	a, err := s.accounts.ImportLogin(j.dataDir, j.proxy, j.replaceID, j.expectedEmail, j.nickname)
	if err != nil {
		j.state, j.err = "failed", err.Error()
		return
	}
	j.state, j.accountID = "ok", a.ID
}

func (j *LoginJob) cancel() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state == "waiting" && j.cmd != nil {
		j.state, j.err, j.url, j.authorizeURL = "idle", "", "", ""
		_ = j.cmd.Process.Kill()
	}
}

func (s *Server) adminProbe(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var body struct {
		Model           string    `json:"model"`
		Message         string    `json:"message"`
		Stream          bool      `json:"stream"`
		System          string    `json:"system"`
		ThinkingEffort  string    `json:"thinking_effort"`
		ReasoningEffort string    `json:"reasoning_effort"`
		AccountID       string    `json:"account_id"`
		ThreadID        string    `json:"thread_id"`
		History         []Message `json:"history"`
		BuiltInTools    bool      `json:"built_in_tools"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	msg := strings.TrimSpace(body.Message)
	if msg == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "message is required"})
		return
	}
	settings := s.settings.snapshot()
	cfg := settings.config(s.cfg)
	cfg.ThinkingEffort = settings.thinking(body.ThinkingEffort, body.ReasoningEffort)
	upstream := NewUpstream(cfg)
	model := settings.mapModel(body.Model)
	ctx, cancel := context.WithTimeout(r.Context(), cfg.Timeout)
	defer cancel()
	if body.AccountID != "" {
		r.Header.Set("X-Account-Id", body.AccountID)
	}
	if body.ThreadID != "" {
		r.Header.Set("X-Thread-Id", body.ThreadID)
	}
	sess, proxyURL, accID, err := s.resolveAccount(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": err.Error()})
		return
	}
	prompt := msg
	sys := strings.TrimSpace(body.System)
	if len(body.History) > 0 {
		hist := append(append([]Message{}, body.History...), Message{Role: "user", Content: msg})
		if p, err := formatPrompt(hist); err == nil {
			prompt = p
		}
		if sys == "" {
			sys = systemPrompt(hist)
		}
	}
	explicit := sys
	sys = settings.effectiveSystem(sys)
	usePanelTools := r.URL.Path == "/admin/chat" && body.BuiltInTools
	sess = s.sessionSurface(sess, cfg)
	in := ChatInput{
		Prompt:       prompt,
		Model:        model,
		Title:        titleFor(msg),
		SystemPrompt: sys,
		Proxy:        proxyURL,
		ThreadID:     strings.TrimSpace(body.ThreadID),
		KeepThread:   strings.TrimSpace(body.ThreadID) != "",
		AccountID:    accID,
		ChatAgentID:  s.chatAgentFor(accID),
		ClientTurn:   explicit != "",
	}
	if usePanelTools {
		in.SystemPrompt = joinSystemAndTools(sys, panelToolsPrompt)
		in.ClientTurn = true
	}
	tap := &callTap{model: model, stream: body.Stream, start: start, accountID: accID}
	s.stats.Begin()
	defer s.stats.End()
	defer s.finishCall(tap)

	if !body.Stream {
		res, err := upstream.Chat(ctx, &sess, in, tap.observeText)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": tap.fail(err, "AWS Quick request failed")})
			return
		}
		tap.status = "ok"
		tap.usage = res.Usage
		reply := map[string]any{
			"model":      model,
			"text":       res.Text,
			"usage":      usageMap(res.Usage),
			"ms":         time.Since(tap.start).Milliseconds(),
			"account_id": accID,
			"thread_id":  in.ThreadID,
		}
		if usePanelTools {
			reply["text"], reply["artifacts"] = panelReply(res.Text)
		}
		writeJSON(w, http.StatusOK, reply)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming unavailable"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Account-Id", accID)
	w.WriteHeader(http.StatusOK)
	writeEvent := func(event map[string]any) {
		b, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	var gate toolStreamGate
	res, err := upstream.Chat(ctx, &sess, in, func(ev StreamEvent) error {
		if ev.Type == "text" && ev.Text != "" {
			tap.firstToken()
			delta := ev.Text
			if usePanelTools {
				delta = gate.Push(delta)
			}
			if delta != "" {
				writeEvent(map[string]any{"text": delta})
			}
		}
		return nil
	})
	if err != nil {
		writeEvent(map[string]any{"error": tap.fail(err, "AWS Quick request failed")})
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}
	tap.status = "ok"
	tap.usage = res.Usage
	reply := map[string]any{"done": true, "usage": usageMap(res.Usage), "text": res.Text, "account_id": accID}
	if usePanelTools {
		reply["text"], reply["artifacts"] = panelReply(res.Text)
	}
	writeEvent(reply)
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (s *Server) adminAccountsImport(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read import payload"})
		return
	}
	records, err := parseQuickBundlePayload(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	summary := s.importQuickRecords(r.Context(), records)
	if summary["uploaded"] == 0 && summary["failed"].(int) > 0 {
		errs, _ := summary["errors"].([]string)
		msg := "no accounts imported"
		if len(errs) > 0 {
			msg = errs[0]
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": msg, "summary": summary})
		return
	}
	now := time.Now().Unix()
	accounts := s.accounts.Snapshot()
	usage := s.stats.AccountUsage()
	list := make([]map[string]any, 0, len(accounts))
	for _, account := range accounts {
		row := publicAccount(account, now)
		row["refresh"] = s.refreshes.status(account.ID)
		row["usage"] = usage[account.ID]
		list = append(list, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":  list,
		"login":     s.login.snapshot(),
		"now":       now,
		"signed_in": len(list) > 0,
		"summary":   summary,
	})
}

func (s *Server) adminAccountsExport(w http.ResponseWriter, r *http.Request) {
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "jsonl" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "format must be json or jsonl"})
		return
	}
	rawIDs := strings.TrimSpace(r.URL.Query().Get("ids"))
	wantIDs := map[string]bool{}
	if rawIDs != "" {
		for _, id := range strings.Split(rawIDs, ",") {
			if id = strings.TrimSpace(id); id != "" {
				wantIDs[id] = true
			}
		}
	}
	all := s.accounts.Snapshot()
	records := make([]QuickAccountRecord, 0, len(all))
	for _, acc := range all {
		if len(wantIDs) > 0 && !wantIDs[acc.ID] {
			continue
		}
		records = append(records, accountToQuickRecord(acc))
	}
	if format == "jsonl" {
		var buf strings.Builder
		for _, rec := range records {
			line, err := json.Marshal(rec)
			if err != nil {
				continue
			}
			buf.Write(line)
			buf.WriteByte('\n')
		}
		w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=aws-quick-accounts.jsonl")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, buf.String())
		return
	}
	bundle := map[string]any{
		"format":      quickFormatVersion,
		"exported_at": time.Now().UTC().Format(time.RFC3339),
		"accounts":    records,
	}
	body, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=aws-quick-accounts.json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(append(body, '\n'))
}
