package store

import (
	"strings"
	"testing"
	"time"
)

func TestUsageTrackingAndPTReset(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ptLoc := pacificLocation()
	day1 := time.Date(2026, 11, 23, 18, 30, 0, 0, ptLoc) // 5h 30m until midnight PT

	if err := st.recordAPIUsageAt(day1, "gemini-3-flash-preview", 1000, 200, 1200, "standard", false, 0); err != nil {
		t.Fatalf("record 1: %v", err)
	}
	if err := st.recordAPIUsageAt(day1.Add(time.Minute), "gemini-3-flash-preview", 500, 100, 600, "standard", false, 0); err != nil {
		t.Fatalf("record 2: %v", err)
	}
	// Simulate an observed 429 cap from Google's QuotaFailure detail.
	if err := st.recordAPIUsageAt(day1.Add(2*time.Minute), "gemini-3-flash-preview", 0, 0, 0, "", true, 20); err != nil {
		t.Fatalf("record 429: %v", err)
	}

	// Without configured limit, falls back to observed_429 limit (20).
	sum, err := st.GetUsageSummary(day1.Add(3*time.Minute), "gemini-3-flash-preview", 0)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum.TodayModel.Requests != 2 || sum.TodayModel.TotalTokens != 1800 || sum.TodayModel.RateLimitHits != 1 {
		t.Fatalf("unexpected today usage: %+v", sum.TodayModel)
	}
	if sum.DailyLimit != 20 || sum.DailyLimitSource != "observed_429" || sum.RemainingRequests != 18 || sum.PercentUsed != 10 {
		t.Fatalf("unexpected quota math: %+v", sum)
	}

	// Explicit config limit overrides observed limit.
	sumCfg, err := st.GetUsageSummary(day1.Add(3*time.Minute), "gemini-3-flash-preview", 250)
	if err != nil {
		t.Fatalf("summary cfg: %v", err)
	}
	if sumCfg.DailyLimit != 250 || sumCfg.DailyLimitSource != "config" || sumCfg.RemainingRequests != 248 {
		t.Fatalf("unexpected configured quota math: %+v", sumCfg)
	}

	report := FormatQuotaReport(sumCfg)
	if !strings.Contains(report, "2 / 250 (248 left)") || !strings.Contains(report, "aistudio.google.com/rate-limit") {
		t.Fatalf("unexpected formatted report:\n%s", report)
	}

	// Next PT day starts fresh at 0 today while preserving 7-day & all-time counts.
	day2 := time.Date(2026, 11, 24, 0, 5, 0, 0, ptLoc)
	sumDay2, err := st.GetUsageSummary(day2, "gemini-3-flash-preview", 250)
	if err != nil {
		t.Fatalf("summary day2: %v", err)
	}
	if sumDay2.TodayModel.Requests != 0 || sumDay2.Last7Days.Requests != 2 || sumDay2.AllTime.Requests != 2 {
		t.Fatalf("unexpected rollover math: %+v", sumDay2)
	}
}
