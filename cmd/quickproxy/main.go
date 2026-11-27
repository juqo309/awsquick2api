package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"quickwork-openai/internal/proxy"
)

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

type outpostInfo struct {
	URL       string `json:"url"`
	TokenFile string `json:"token_file"`
	PID       int    `json:"pid"`
}

func main() {
	addr := flag.String("addr", env("QW_PROXY_ADDR", "127.0.0.1:8080"), "listen address")
	sessionFile := flag.String("session", env("QW_SESSION_FILE", "data/session.json"), "outpost session JSON (written by the Python login helper)")
	upstreamFile := flag.String("upstream", env("QW_UPSTREAM_FILE", "data/upstream.json"), "refresh-state JSON written by the login helper")
	statsFile := flag.String("stats", env("QW_STATS_FILE", "data/stats.json"), "request ledger file")
	accountsFile := flag.String("accounts", env("QW_ACCOUNTS_FILE", "data/accounts.json"), "multi-account store")
	fullContext := flag.Bool("full-context", env("QW_FULL_CONTEXT", "") == "1", "send QuickWork's default memory/knowledge prompt instead of the lean override")
	authScript := flag.String("auth-script", env("QW_AUTH_SCRIPT", "auth/quickauth.py"), "Python login helper")
	python := flag.String("python", env("QW_PYTHON", "python3"), "Python interpreter for the login helper")
	outpostURL := flag.String("outpost", env("QW_OUTPOST_URL", ""), "local outpost base URL, e.g. http://127.0.0.1:8780")
	tokenFile := flag.String("instance-token-file", env("QW_INSTANCE_TOKEN_FILE", "data/instance_token"), "outpost instance token file")
	tokensFile := flag.String("tokens", env("QW_TOKENS_FILE", "data/tokens.json"), "proxy API key file (empty disables auth)")
	modelsCSV := flag.String("models", env("QW_MODELS", "smart,fast,balanced,auto"), "comma-separated model aliases")
	defaultModel := flag.String("default-model", env("QW_DEFAULT_MODEL", "smart"), "agent mode used when the request model is unknown")
	timeout := flag.Duration("timeout", 5*time.Minute, "per-request upstream timeout")
	appVersion := flag.String("app-version", env("QW_APP_VERSION", "1.2.4660"), "appVersion sent in the WS handshake")
	surface := flag.String("surface", env("QW_SURFACE", defaultSurface()), "surfaceType sent in the WS handshake")
	outpostID := flag.String("outpost-id", env("QW_OUTPOST_ID", ""), "outpostId sent in the WS handshake")
	workingSessionID := flag.String("working-session-id", env("QW_WORKING_SESSION_ID", ""), "workingSessionId sent in the WS handshake")
	timezone := flag.String("timezone", env("QW_TIMEZONE", defaultTimezone()), "user_timezone in user_message")
	locale := flag.String("locale", env("QW_LOCALE", defaultLocale()), "user_locale in user_message")
	thinking := flag.String("thinking", env("QW_THINKING_EFFORT", ""), "thinking_effort: off|low|medium|high")
	panelAuthFile := flag.String("panel-auth", env("QW_PANEL_AUTH_FILE", "data/panel_auth.json"), "dashboard password file (missing leaves the dashboard open)")
	setPanelPassword := flag.Bool("set-panel-password", false, "read a new dashboard password from stdin, write it to -panel-auth, and exit")
	keepSessions := flag.Bool("keep-sessions", false, "keep upstream chat sessions instead of deleting them after each request")
	sessionProof := flag.Bool("session-proof", env("QW_SESSION_PROOF", "1") != "0", "prove the Quick session via quickwork-session before pooling an account")
	flag.Parse()

	if *setPanelPassword {
		raw, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
		if err != nil {
			log.Fatalf("read password: %v", err)
		}
		password := strings.TrimRight(string(raw), "\r\n")
		if err := proxy.SetPanelPassword(*panelAuthFile, password); err != nil {
			log.Fatalf("set dashboard password: %v", err)
		}
		log.Printf("dashboard password written to %s", absOrSame(*panelAuthFile))
		return
	}

	// If the Python helper left an outpost descriptor, pick up its URL/token.
	if *outpostURL == "" || *tokenFile == "data/instance_token" {
		if body, err := os.ReadFile("data/outpost.json"); err == nil {
			var info outpostInfo
			if json.Unmarshal(body, &info) == nil {
				if *outpostURL == "" && info.URL != "" {
					*outpostURL = info.URL
				}
				if info.TokenFile != "" {
					*tokenFile = info.TokenFile
				}
			}
		}
	}

	models := make([]string, 0)
	for _, m := range strings.Split(*modelsCSV, ",") {
		if m = strings.TrimSpace(m); m != "" {
			models = append(models, m)
		}
	}
	cfg := &proxy.Config{
		Addr:              *addr,
		SessionFile:       *sessionFile,
		UpstreamFile:      *upstreamFile,
		StatsFile:         *statsFile,
		OutpostURL:        strings.TrimRight(*outpostURL, "/"),
		InstanceTokenFile: *tokenFile,
		TokensFile:        *tokensFile,
		PanelAuthFile:     *panelAuthFile,
		AccountsFile:      *accountsFile,
		AuthScript:        *authScript,
		Python:            *python,
		Models:            models,
		DefaultModel:      *defaultModel,
		Timeout:           *timeout,
		AppVersion:        *appVersion,
		Surface:           *surface,
		OutpostID:         *outpostID,
		WorkingSessionID:  *workingSessionID,
		Timezone:          *timezone,
		Locale:            *locale,
		ThinkingEffort:    *thinking,
		KeepSessions:      *keepSessions,
		FullContext:       *fullContext,
		SessionProof:      *sessionProof,
	}

	app := proxy.NewServer(cfg)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		app.RunAccountRefresher(ctx)
	}()
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("quickwork-openai listening on %s panel=http://%s/ (session=%s outpost=%s auth=%v panel_password=%v)",
		cfg.Addr, cfg.Addr, absOrSame(cfg.SessionFile), emptyAs(cfg.OutpostURL, "file"), fileExists(cfg.TokensFile), fileExists(cfg.PanelAuthFile))
	if !fileExists(cfg.PanelAuthFile) {
		log.Printf("warning: no dashboard password set; the dashboard and /admin API are open on loopback. Set one with -set-panel-password before binding a public address")
	}
	if host, _, err := net.SplitHostPort(cfg.Addr); err == nil && host != "" && host != "127.0.0.1" && host != "localhost" && host != "::1" {
		if !fileExists(cfg.PanelAuthFile) {
			log.Printf("warning: %s is not loopback and no dashboard password is set", cfg.Addr)
		}
		if !fileExists(cfg.TokensFile) {
			log.Printf("warning: %s is not loopback and inference API keys are not configured", cfg.Addr)
		}
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("shutting down")
	_ = server.Close()
	<-refreshDone
}

func absOrSame(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

func emptyAs(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func defaultSurface() string {
	return "QUICK_WORK_DESKTOP_WINDOWS"
}

func defaultTimezone() string {
	if v := strings.TrimSpace(os.Getenv("TZ")); v != "" {
		return v
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		for _, marker := range []string{"/zoneinfo/", "zoneinfo/"} {
			if i := strings.LastIndex(target, marker); i >= 0 {
				name := strings.Trim(target[i+len(marker):], "/")
				if name != "" {
					return name
				}
			}
		}
	}
	return "UTC"
}

func defaultLocale() string {
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" || v == "C" || v == "POSIX" || strings.HasPrefix(v, "C.") {
			continue
		}
		v = strings.SplitN(v, ".", 2)[0]
		v = strings.ReplaceAll(v, "_", "-")
		if v != "" {
			return v
		}
	}
	return "en-US"
}
