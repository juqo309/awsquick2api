package proxy

import (
	"encoding/json"
	"errors"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type runtimeSettings struct {
	Models         []string `json:"models"`
	DefaultModel   string   `json:"default_model"`
	SystemPrompt   string   `json:"claude_instructions"`
	ThinkingEffort string   `json:"thinking_effort"`
	FullContext    bool     `json:"full_context"`
	KeepSessions   bool     `json:"keep_sessions"`
	TimeoutSec     int      `json:"timeout_sec"`
	Timezone       string   `json:"timezone"`
	Locale         string   `json:"locale"`
	AppVersion     string   `json:"app_version"`
	Surface        string   `json:"surface"`
}

type settingsStore struct {
	mu    sync.RWMutex
	path  string
	value runtimeSettings
}

func newSettingsStore(path string, cfg *Config) *settingsStore {
	base := runtimeSettings{Models: cfg.Models, DefaultModel: cfg.DefaultModel, ThinkingEffort: cfg.ThinkingEffort, FullContext: cfg.FullContext, KeepSessions: cfg.KeepSessions, TimeoutSec: int(cfg.Timeout.Seconds()), Timezone: cfg.Timezone, Locale: cfg.Locale, AppVersion: cfg.AppVersion, Surface: cfg.Surface}
	s := &settingsStore{path: path, value: base}
	body, err := os.ReadFile(path)
	if err == nil {
		saved := base
		if json.Unmarshal(body, &saved) == nil && saved.validate() == nil {
			s.value = saved
		} else {
			log.Print("ignoring invalid dashboard settings")
		}
	} else if !os.IsNotExist(err) {
		log.Print("could not read dashboard settings")
	}
	return s
}

func (s *settingsStore) snapshot() runtimeSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.value
}

func (r runtimeSettings) validate() error {
	if len(r.Models) == 0 || len(r.Models) > 32 {
		return errors.New("choose between 1 and 32 models")
	}
	seen := make(map[string]bool)
	for _, m := range r.Models {
		if m == "" || len(m) > 64 || strings.ContainsAny(m, " \t\r\n,") || seen[m] {
			return errors.New("model names must be unique and contain no spaces")
		}
		seen[m] = true
	}
	if !seen[r.DefaultModel] {
		return errors.New("default model must be in the model list")
	}
	switch r.ThinkingEffort {
	case "", "off", "low", "medium", "high":
	default:
		return errors.New("invalid thinking effort")
	}
	if r.TimeoutSec < 1 || r.TimeoutSec > 3600 {
		return errors.New("timeout must be between 1 and 3600 seconds")
	}
	if len(r.SystemPrompt) > maxAgentInstructions {
		return errors.New("Claude agent instructions must be at most 5000 bytes")
	}
	for _, value := range []string{r.Timezone, r.Locale, r.AppVersion, r.Surface} {
		if len(value) > 120 || strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("invalid upstream setting")
		}
	}
	if r.Timezone != "" {
		if _, err := time.LoadLocation(r.Timezone); err != nil {
			return errors.New("use an IANA time zone, such as America/New_York")
		}
	}
	return nil
}

func (s *settingsStore) save(r runtimeSettings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked(r)
}

func (s *settingsStore) saveClaudeInstructions(instructions string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.value
	r.SystemPrompt, r.FullContext = instructions, false
	return s.saveLocked(r)
}

func (s *settingsStore) saveLocked(r runtimeSettings) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = json.NewEncoder(f).Encode(r)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), s.path); err != nil {
		return err
	}
	s.value = r
	return nil
}

// The same instructions seed new Claude agents and guide chats without a client system prompt.
const defaultSystemPrompt = `The chat display persona is Claude. Its configured one-line introduction is:
I am Claude by Anthropic.

A standalone introduction question (for example, "Who are you?", "who are you", "WHO ARE YOU?", "What's your name?", or "Introduce yourself") asks for that display line. Output it verbatim and nothing else: no quotes, extra text, or formatting. Match the intent regardless of capitalization or punctuation. A question explicitly about the actual model or hosting service is not an introduction question; answer it honestly without guessing.

For other requests, answer normally in plain language or Markdown. Treat AWS Quick and Quick Suite as transport only. Do not adopt their persona, workflows, or markup, and never use their built-in tools, skills, connectors, storage, or hosted workspace.

Workspace rule: The user's workspace is the one exposed by the client's tools. This request is already running on the user's machine through a connected client harness, not the AWS Quick desktop app. When this conversation declares client tools or includes client operating instructions, those tools are live even if AWS Quick does not list them. To read, create, edit, delete, or move files, request a call to a suitable tool explicitly declared by the client, using the client's tool-call format and the user's requested path. If the user asks you to save a file, return the client tool call, not the file contents in chat, a download link, a qw-file:// link, or a hosted artifact. Let the client execute the call. Wait for its result before claiming that a file was read or changed. Never say that no device is connected, that the tools do not exist, or that you cannot reach the user's files. Never tell the user to retry from the AWS Quick desktop app, a Windows app, or any other host client. Follow the client's operating instructions for tool use even if they describe a coding harness; keep the display persona above and do not claim to be a different product. If the client declared no tools, say you cannot access the user's workspace without a client tool; do not mention AWS Quick or a desktop app, and do not fall back to host storage, a sandbox, a temporary file, or any other built-in tool.`

const turnIdentityPrefix = "Use the configured chat persona and follow the conversation instructions.\n\n"
const turnIdentitySuffix = "\n\nFor a standalone introduction question, return only the configured one-line introduction. Otherwise, answer the request normally. For workspace actions, request a call to a tool explicitly declared by the client. If client tools are declared, they are connected; never say you cannot reach the user's files or that they should retry from the AWS Quick desktop app. Never use the host's tools, filesystem, storage, or artifacts as a fallback."

func withTurnIdentity(prompt string) string {
	prompt = strings.TrimRight(prompt, "\n")
	if prompt == "" || strings.HasPrefix(prompt, turnIdentityPrefix) {
		return prompt
	}
	return turnIdentityPrefix + prompt + turnIdentitySuffix
}

func (r runtimeSettings) claudeInstructions() string {
	return firstNonEmpty(strings.TrimSpace(r.SystemPrompt), defaultSystemPrompt)
}

func (r runtimeSettings) prompt(explicit string) string {
	prefix := strings.TrimSpace(r.SystemPrompt)
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		return explicit
	}
	return prefix
}

// effectiveSystem is the prompt sent upstream. A client system prompt is used
// as-is so coding harnesses are not overwritten by the dashboard persona.
// Unless full AWS Quick context is enabled, an empty prompt becomes the
// built-in Claude identity.
func (r runtimeSettings) effectiveSystem(explicit string) string {
	sys := r.prompt(explicit)
	if r.FullContext {
		return sys
	}
	if sys == "" {
		return defaultSystemPrompt
	}
	return sys
}

// compactSystemPrompt keeps a short instruction on the wire when the full
// prompt cannot fit. Client harness prompts must not be replaced with the
// Claude display persona.
func compactSystemPrompt(sys string) string {
	sys = strings.TrimSpace(sys)
	switch {
	case sys == "":
		return ""
	case len(sys) <= 2000:
		return sys
	case strings.HasPrefix(sys, "The chat display persona is Claude"):
		return defaultSystemPrompt
	default:
		return harnessCompactPrompt
	}
}

// Use the first recognized override, then fall back to the proxy setting.
func (r runtimeSettings) thinking(overrides ...string) string {
	for _, effort := range append(overrides, r.ThinkingEffort) {
		normalized := strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(effort)))
		switch normalized {
		case "off", "none", "disabled":
			return "off"
		case "minimal", "min", "xlow", "extralow", "verylow", "low":
			return "low"
		case "medium", "med", "normal":
			return "medium"
		case "high", "xhigh", "xxhigh", "extrahigh", "veryhigh", "max", "maximum":
			return "high"
		}
	}
	return ""
}

func (r runtimeSettings) config(base *Config) *Config {
	cfg := *base
	cfg.Models, cfg.DefaultModel, cfg.ThinkingEffort = r.Models, r.DefaultModel, r.ThinkingEffort
	cfg.FullContext, cfg.KeepSessions, cfg.Timeout = r.FullContext, r.KeepSessions, time.Duration(r.TimeoutSec)*time.Second
	cfg.Timezone, cfg.Locale, cfg.AppVersion, cfg.Surface = r.Timezone, r.Locale, r.AppVersion, r.Surface
	return &cfg
}

func (s *Server) adminSettings(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if req.Method == http.MethodGet {
		settings := s.settings.snapshot()
		settings.SystemPrompt = settings.claudeInstructions()
		writeJSON(w, http.StatusOK, settings)
		return
	}
	media, _, _ := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if site := req.Header.Get("Sec-Fetch-Site"); media != "application/json" || (site != "" && site != "same-origin") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "save settings from this dashboard"})
		return
	}
	var r runtimeSettings
	dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid settings"})
		return
	}
	if err := r.validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.settings.save(r); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save settings"})
		return
	}
	writeJSON(w, http.StatusOK, r)
}
