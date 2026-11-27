package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type CallRecord struct {
	ID               string `json:"id"`
	At               int64  `json:"at"`
	Model            string `json:"model"`
	Stream           bool   `json:"stream"`
	Status           string `json:"status"`
	Error            string `json:"error,omitempty"`
	DurationMs       int64  `json:"duration_ms"`
	TTFTMs           *int64 `json:"ttft_ms,omitempty"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	CacheReadTokens  int    `json:"cache_read_tokens"`
	BilledTokens     int    `json:"billed_tokens"`
	AccountID        string `json:"account_id,omitempty"`
}

type ModelStat struct {
	Requests         int   `json:"requests"`
	Errors           int   `json:"errors"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	DurationMs       int64 `json:"duration_ms"`
}

type HourBucket struct {
	Hour     int64 `json:"hour"`
	Requests int   `json:"requests"`
	Errors   int   `json:"errors"`
	Tokens   int64 `json:"tokens"`
}

type StatsSnapshot struct {
	StartedAt        int64                `json:"started_at"`
	UptimeSec        int64                `json:"uptime_sec"`
	InFlight         int                  `json:"in_flight"`
	Requests         int                  `json:"requests"`
	Success          int                  `json:"success"`
	Errors           int                  `json:"errors"`
	PromptTokens     int64                `json:"prompt_tokens"`
	CompletionTokens int64                `json:"completion_tokens"`
	TotalTokens      int64                `json:"total_tokens"`
	CacheReadTokens  int64                `json:"cache_read_tokens"`
	BilledTokens     int64                `json:"billed_tokens"`
	AvgMs            float64              `json:"avg_ms"`
	P50Ms            int64                `json:"p50_ms"`
	P95Ms            int64                `json:"p95_ms"`
	ByModel          map[string]ModelStat `json:"by_model"`
	Hourly           []HourBucket         `json:"hourly"`
	Recent           []CallRecord         `json:"recent"`
}

type AccountUsage struct {
	ModelStat
	LastUsed int64 `json:"last_used"`
}

type statsFile struct {
	ByAccount        map[string]AccountUsage `json:"by_account,omitempty"`
	StartedAt        int64                   `json:"started_at"`
	Requests         int                     `json:"requests"`
	Success          int                     `json:"success"`
	Errors           int                     `json:"errors"`
	PromptTokens     int64                   `json:"prompt_tokens"`
	CompletionTokens int64                   `json:"completion_tokens"`
	TotalTokens      int64                   `json:"total_tokens"`
	CacheReadTokens  int64                   `json:"cache_read_tokens"`
	DurationMs       int64                   `json:"duration_ms"`
	ByModel          map[string]ModelStat    `json:"by_model"`
	Hourly           []HourBucket            `json:"hourly"`
	Recent           []CallRecord            `json:"recent"`
}

type Stats struct {
	path      string
	mu        sync.Mutex
	startedAt int64
	inFlight  int
	data      statsFile
}

func NewStats(path string) *Stats {
	s := &Stats{path: path, startedAt: time.Now().Unix()}
	s.data.ByModel = map[string]ModelStat{}
	s.data.Hourly = []HourBucket{}
	s.data.Recent = []CallRecord{}
	s.data.StartedAt = s.startedAt
	s.load()
	if s.data.StartedAt == 0 {
		s.data.StartedAt = s.startedAt
	} else {
		s.startedAt = s.data.StartedAt
	}
	return s
}

func (s *Stats) load() {
	if s.path == "" {
		return
	}
	body, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var f statsFile
	if json.Unmarshal(body, &f) != nil {
		return
	}
	if f.ByModel == nil {
		f.ByModel = map[string]ModelStat{}
	}
	if f.ByAccount == nil {
		// Older files only have per-account data in retained requests.
		for i := len(f.Recent) - 1; i >= 0; i-- {
			f.recordAccount(f.Recent[i])
		}
	}
	s.data = f
}

func (s *Stats) persistLocked() {
	if s.path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o700)
	body, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, append(body, '\n'), 0o600) != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}

func (s *Stats) Begin() {
	s.mu.Lock()
	s.inFlight++
	s.mu.Unlock()
}

func (s *Stats) End() {
	s.mu.Lock()
	if s.inFlight > 0 {
		s.inFlight--
	}
	s.mu.Unlock()
}

func (s *Stats) Record(rec CallRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.At == 0 {
		rec.At = time.Now().UnixMilli()
	}
	if rec.Status == "" {
		rec.Status = "error"
	}
	s.data.Requests++
	if rec.Status == "ok" {
		s.data.Success++
	} else {
		s.data.Errors++
	}
	s.data.PromptTokens += int64(rec.PromptTokens)
	s.data.CompletionTokens += int64(rec.CompletionTokens)
	s.data.TotalTokens += int64(rec.TotalTokens)
	s.data.CacheReadTokens += int64(rec.CacheReadTokens)
	billed := rec.PromptTokens - rec.CacheReadTokens
	if billed < 0 {
		billed = 0
	}
	rec.BilledTokens = billed
	s.data.DurationMs += rec.DurationMs
	ms := s.data.ByModel[rec.Model]
	ms.Requests++
	if rec.Status != "ok" {
		ms.Errors++
	}
	ms.PromptTokens += int64(rec.PromptTokens)
	ms.CompletionTokens += int64(rec.CompletionTokens)
	ms.TotalTokens += int64(rec.TotalTokens)
	ms.CacheReadTokens += int64(rec.CacheReadTokens)
	ms.DurationMs += rec.DurationMs
	s.data.ByModel[rec.Model] = ms
	s.data.recordAccount(rec)
	hour := rec.At / 1000
	hour = hour - (hour % 3600)
	found := false
	for i := range s.data.Hourly {
		if s.data.Hourly[i].Hour == hour {
			s.data.Hourly[i].Requests++
			if rec.Status != "ok" {
				s.data.Hourly[i].Errors++
			}
			s.data.Hourly[i].Tokens += int64(rec.TotalTokens)
			found = true
			break
		}
	}
	if !found {
		b := HourBucket{Hour: hour, Requests: 1, Tokens: int64(rec.TotalTokens)}
		if rec.Status != "ok" {
			b.Errors = 1
		}
		s.data.Hourly = append(s.data.Hourly, b)
	}
	cutoff := time.Now().Add(-48 * time.Hour).Unix()
	trimmed := s.data.Hourly[:0]
	for _, b := range s.data.Hourly {
		if b.Hour >= cutoff {
			trimmed = append(trimmed, b)
		}
	}
	s.data.Hourly = trimmed
	s.data.Recent = append([]CallRecord{rec}, s.data.Recent...)
	if len(s.data.Recent) > 400 {
		s.data.Recent = s.data.Recent[:400]
	}
	s.persistLocked()
}

func (f *statsFile) recordAccount(rec CallRecord) {
	if rec.AccountID == "" {
		return
	}
	if f.ByAccount == nil {
		f.ByAccount = make(map[string]AccountUsage)
	}
	u := f.ByAccount[rec.AccountID]
	u.Requests++
	if rec.Status != "ok" {
		u.Errors++
	}
	u.PromptTokens += int64(rec.PromptTokens)
	u.CompletionTokens += int64(rec.CompletionTokens)
	u.TotalTokens += int64(rec.TotalTokens)
	u.CacheReadTokens += int64(rec.CacheReadTokens)
	u.DurationMs += rec.DurationMs
	if rec.At > u.LastUsed {
		u.LastUsed = rec.At
	}
	f.ByAccount[rec.AccountID] = u
}

func (s *Stats) AccountUsage() map[string]AccountUsage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]AccountUsage, len(s.data.ByAccount))
	for id, usage := range s.data.ByAccount {
		out[id] = usage
	}
	return out
}

func (s *Stats) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	s.startedAt = now
	s.data = statsFile{
		StartedAt: now,
		ByModel:   map[string]ModelStat{},
		Hourly:    []HourBucket{},
		Recent:    []CallRecord{},
	}
	s.persistLocked()
}

func dropErrorCount(requests, errors int) (int, int) {
	if errors < 0 {
		errors = 0
	}
	if requests > errors {
		return requests - errors, 0
	}
	if requests < 0 {
		return 0, 0
	}
	return 0, 0
}

// ClearErrors drops failed requests from the traffic log and zeroes error
// counters everywhere they are shown (usage, models, hours, accounts).
func (s *Stats) ClearErrors() {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := make([]CallRecord, 0, len(s.data.Recent))
	for _, rec := range s.data.Recent {
		if rec.Status == "ok" {
			kept = append(kept, rec)
		}
	}
	s.data.Recent = kept
	s.data.Requests, s.data.Errors = dropErrorCount(s.data.Requests, s.data.Errors)
	if s.data.Success >= 0 && s.data.Requests != s.data.Success {
		s.data.Requests = s.data.Success
	}
	for name, ms := range s.data.ByModel {
		ms.Requests, ms.Errors = dropErrorCount(ms.Requests, ms.Errors)
		s.data.ByModel[name] = ms
	}
	for i := range s.data.Hourly {
		s.data.Hourly[i].Requests, s.data.Hourly[i].Errors = dropErrorCount(s.data.Hourly[i].Requests, s.data.Hourly[i].Errors)
	}
	for id, u := range s.data.ByAccount {
		u.Requests, u.Errors = dropErrorCount(u.Requests, u.Errors)
		s.data.ByAccount[id] = u
	}
	s.persistLocked()
}

func (s *Stats) Snapshot(limit int) StatsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > len(s.data.Recent) {
		limit = len(s.data.Recent)
	}
	recent := make([]CallRecord, limit)
	copy(recent, s.data.Recent[:limit])
	byModel := make(map[string]ModelStat, len(s.data.ByModel))
	for k, v := range s.data.ByModel {
		byModel[k] = v
	}
	hourly := make([]HourBucket, len(s.data.Hourly))
	copy(hourly, s.data.Hourly)
	sort.Slice(hourly, func(i, j int) bool { return hourly[i].Hour < hourly[j].Hour })
	avg := 0.0
	if s.data.Requests > 0 {
		avg = float64(s.data.DurationMs) / float64(s.data.Requests)
	}
	durs := make([]int64, 0, len(s.data.Recent))
	for _, r := range s.data.Recent {
		durs = append(durs, r.DurationMs)
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	return StatsSnapshot{
		StartedAt:        s.startedAt,
		UptimeSec:        time.Now().Unix() - s.startedAt,
		InFlight:         s.inFlight,
		Requests:         s.data.Requests,
		Success:          s.data.Success,
		Errors:           s.data.Errors,
		PromptTokens:     s.data.PromptTokens,
		CompletionTokens: s.data.CompletionTokens,
		TotalTokens:      s.data.TotalTokens,
		CacheReadTokens:  s.data.CacheReadTokens,
		BilledTokens: func() int64 {
			b := s.data.PromptTokens - s.data.CacheReadTokens
			if b < 0 {
				return 0
			}
			return b
		}(),
		AvgMs:   avg,
		P50Ms:   percentile(durs, 50),
		P95Ms:   percentile(durs, 95),
		ByModel: byModel,
		Hourly:  hourly,
		Recent:  recent,
	}
}

func percentile(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := (p * len(sorted)) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
