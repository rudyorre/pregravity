package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ModelUsage tracks API request and token consumption for a single model on a given PT day.
type ModelUsage struct {
	Requests      int    `json:"requests"`
	PromptTokens  int    `json:"prompt_tokens"`
	OutputTokens  int    `json:"output_tokens"`
	TotalTokens   int    `json:"total_tokens"`
	RateLimitHits int    `json:"rate_limit_hits,omitempty"`
	ServiceTier   string `json:"service_tier,omitempty"`
}

// UsageLedger persists client-side Gemini API usage bucketed by Pacific Time day
// (matching Google AI Studio's midnight PT daily quota reset).
type UsageLedger struct {
	Days                map[string]map[string]*ModelUsage `json:"days"`
	ObservedDailyLimits map[string]int                    `json:"observed_daily_limits,omitempty"`
	UpdatedAt           time.Time                         `json:"updated_at"`
}

// UsageSummary is a snapshot of API usage and remaining daily budget.
type UsageSummary struct {
	Model              string        `json:"model"`
	PTDate             string        `json:"pt_date"`
	ResetIn            time.Duration `json:"reset_in_ns"`
	ResetInHuman       string        `json:"reset_in"`
	TodayModel         ModelUsage    `json:"today_model"`
	TodayAllModels     ModelUsage    `json:"today_all_models"`
	Last7Days          ModelUsage    `json:"last_7_days"`
	AllTime            ModelUsage    `json:"all_time"`
	DailyLimit         int           `json:"daily_limit"`
	DailyLimitSource   string        `json:"daily_limit_source,omitempty"` // "config", "observed_429", or ""
	RemainingRequests  int           `json:"remaining_requests"`
	PercentUsed        int           `json:"percent_used"`
	RateLimitDashboard string        `json:"rate_limit_dashboard"`
}

func (s *Store) usageJSONPath() string { return filepath.Join(s.rootDir, "usage.json") }

func pacificLocation() *time.Location {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		return time.FixedZone("PST", -8*3600)
	}
	return loc
}

func (s *Store) loadUsageLocked() (*UsageLedger, error) {
	raw, err := os.ReadFile(s.usageJSONPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &UsageLedger{
				Days:                map[string]map[string]*ModelUsage{},
				ObservedDailyLimits: map[string]int{},
			}, nil
		}
		return nil, err
	}
	var ledger UsageLedger
	if err := json.Unmarshal(raw, &ledger); err != nil {
		return nil, fmt.Errorf("parse usage.json: %w", err)
	}
	if ledger.Days == nil {
		ledger.Days = map[string]map[string]*ModelUsage{}
	}
	if ledger.ObservedDailyLimits == nil {
		ledger.ObservedDailyLimits = map[string]int{}
	}
	return &ledger, nil
}

func (s *Store) saveUsageLocked(ledger *UsageLedger) error {
	ledger.UpdatedAt = time.Now().UTC()
	raw, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(s.usageJSONPath(), append(raw, '\n'), 0o600)
}

// RecordAPIUsage records one Gemini generateContent call under the current Pacific Time day.
func (s *Store) RecordAPIUsage(model string, promptTok, outputTok, totalTok int, serviceTier string, rateLimited bool, observedLimit int) error {
	return s.recordAPIUsageAt(time.Now(), model, promptTok, outputTok, totalTok, serviceTier, rateLimited, observedLimit)
}

func (s *Store) recordAPIUsageAt(now time.Time, model string, promptTok, outputTok, totalTok int, serviceTier string, rateLimited bool, observedLimit int) error {
	if strings.TrimSpace(model) == "" {
		model = "unknown"
	}
	ptDay := now.In(pacificLocation()).Format("2006-01-02")

	s.mu.Lock()
	defer s.mu.Unlock()

	ledger, err := s.loadUsageLocked()
	if err != nil {
		return err
	}
	if ledger.Days[ptDay] == nil {
		ledger.Days[ptDay] = map[string]*ModelUsage{}
	}
	mu := ledger.Days[ptDay][model]
	if mu == nil {
		mu = &ModelUsage{}
		ledger.Days[ptDay][model] = mu
	}
	if rateLimited {
		mu.RateLimitHits++
	} else {
		mu.Requests++
		mu.PromptTokens += promptTok
		mu.OutputTokens += outputTok
		if totalTok <= 0 {
			totalTok = promptTok + outputTok
		}
		mu.TotalTokens += totalTok
		if serviceTier != "" {
			mu.ServiceTier = serviceTier
		}
	}
	if observedLimit > 0 {
		ledger.ObservedDailyLimits[model] = observedLimit
	}
	return s.saveUsageLocked(ledger)
}

// GetUsageSummary computes today's PT usage, remaining free-tier budget, reset countdown,
// 7-day rolling usage, and all-time totals.
func (s *Store) GetUsageSummary(now time.Time, model string, configuredLimit int) (UsageSummary, error) {
	ptLoc := pacificLocation()
	nowPT := now.In(ptLoc)
	ptDay := nowPT.Format("2006-01-02")
	nextMidnight := time.Date(nowPT.Year(), nowPT.Month(), nowPT.Day()+1, 0, 0, 0, 0, ptLoc)
	resetIn := nextMidnight.Sub(nowPT)
	if resetIn < 0 {
		resetIn = 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	ledger, err := s.loadUsageLocked()
	if err != nil {
		return UsageSummary{}, err
	}

	summary := UsageSummary{
		Model:              model,
		PTDate:             ptDay,
		ResetIn:            resetIn,
		ResetInHuman:       formatDurationHM(resetIn),
		RateLimitDashboard: "https://aistudio.google.com/rate-limit",
	}

	cutoff7d := nowPT.AddDate(0, 0, -6).Format("2006-01-02")

	for day, models := range ledger.Days {
		for m, u := range models {
			if u == nil {
				continue
			}
			addUsage(&summary.AllTime, *u)
			if day >= cutoff7d && day <= ptDay {
				addUsage(&summary.Last7Days, *u)
			}
			if day == ptDay {
				addUsage(&summary.TodayAllModels, *u)
				if m == model {
					addUsage(&summary.TodayModel, *u)
					if u.ServiceTier != "" {
						summary.TodayModel.ServiceTier = u.ServiceTier
					}
				}
			}
		}
	}

	if configuredLimit > 0 {
		summary.DailyLimit = configuredLimit
		summary.DailyLimitSource = "config"
	} else if obs := ledger.ObservedDailyLimits[model]; obs > 0 {
		summary.DailyLimit = obs
		summary.DailyLimitSource = "observed_429"
	}

	if summary.DailyLimit > 0 {
		rem := summary.DailyLimit - summary.TodayModel.Requests
		if rem < 0 {
			rem = 0
		}
		summary.RemainingRequests = rem
		pct := (summary.TodayModel.Requests * 100) / summary.DailyLimit
		if pct > 100 {
			pct = 100
		}
		summary.PercentUsed = pct
	}

	return summary, nil
}

// FormatQuotaReport renders a concise, Telegram- and terminal-friendly API usage summary.
func FormatQuotaReport(s UsageSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Gemini API Usage (%s, PT day %s)\n", s.Model, s.PTDate)
	if s.DailyLimit > 0 {
		bar := renderBar(s.PercentUsed, 10)
		fmt.Fprintf(&b, "Daily requests: %d / %d (%d left) %s %d%%\n",
			s.TodayModel.Requests, s.DailyLimit, s.RemainingRequests, bar, s.PercentUsed)
	} else {
		fmt.Fprintf(&b, "Daily requests: %d used (no RPD cap configured — run `pregravity quota set-limit <N>` using your AI Studio limit)\n",
			s.TodayModel.Requests)
	}
	fmt.Fprintf(&b, "Today tokens:   %s total (%s in / %s out)\n",
		formatInt(s.TodayModel.TotalTokens), formatInt(s.TodayModel.PromptTokens), formatInt(s.TodayModel.OutputTokens))
	if s.TodayModel.RateLimitHits > 0 {
		fmt.Fprintf(&b, "429 hits today: %d\n", s.TodayModel.RateLimitHits)
	}
	fmt.Fprintf(&b, "Resets in:      %s (midnight PT)\n", s.ResetInHuman)
	fmt.Fprintf(&b, "Last 7 days:    %d reqs | %s tokens\n", s.Last7Days.Requests, formatInt(s.Last7Days.TotalTokens))
	fmt.Fprintf(&b, "All-time:       %d reqs | %s tokens\n", s.AllTime.Requests, formatInt(s.AllTime.TotalTokens))
	fmt.Fprintf(&b, "Live AI Studio limits: %s", s.RateLimitDashboard)
	return b.String()
}

func addUsage(dst *ModelUsage, src ModelUsage) {
	dst.Requests += src.Requests
	dst.PromptTokens += src.PromptTokens
	dst.OutputTokens += src.OutputTokens
	dst.TotalTokens += src.TotalTokens
	dst.RateLimitHits += src.RateLimitHits
}

func renderBar(pct, width int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := (pct * width) / 100
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", width-filled) + "]"
}

func formatDurationHM(d time.Duration) string {
	mins := int(d.Round(time.Minute) / time.Minute)
	h := mins / 60
	m := mins % 60
	return fmt.Sprintf("%dh %02dm", h, m)
}

func formatInt(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	rem := len(s) % 3
	if rem > 0 {
		out = append(out, s[:rem]...)
		if len(s) > rem {
			out = append(out, ',')
		}
	}
	for i := rem; i < len(s); i += 3 {
		if i > rem {
			out = append(out, ',')
		}
		out = append(out, s[i:i+3]...)
	}
	return string(out)
}
