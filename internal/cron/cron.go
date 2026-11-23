package cron

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// Expr represents a parsed standard 5-field cron expression:
// minute (0-59), hour (0-23), day-of-month (1-31), month (1-12), day-of-week (0-6, 0=Sunday, 7=Sunday).
type Expr struct {
	Raw         string
	minutes     [60]bool
	hours       [24]bool
	daysOfMonth [32]bool
	months      [13]bool
	daysOfWeek  [7]bool
	domWild     bool
	dowWild     bool
}

// Parse parses a 5-field cron expression (supports *, lists, ranges, steps).
func Parse(raw string) (*Expr, error) {
	fields := strings.Fields(strings.TrimSpace(raw))
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron expression %q must have exactly 5 fields, got %d", raw, len(fields))
	}

	e := &Expr{
		Raw:     raw,
		domWild: fields[2] == "*",
		dowWild: fields[4] == "*",
	}

	if err := parseField(fields[0], 0, 59, false, e.minutes[:]); err != nil {
		return nil, fmt.Errorf("minute field %q: %w", fields[0], err)
	}
	if err := parseField(fields[1], 0, 23, false, e.hours[:]); err != nil {
		return nil, fmt.Errorf("hour field %q: %w", fields[1], err)
	}
	if err := parseField(fields[2], 1, 31, false, e.daysOfMonth[:]); err != nil {
		return nil, fmt.Errorf("day-of-month field %q: %w", fields[2], err)
	}
	if err := parseField(fields[3], 1, 12, false, e.months[:]); err != nil {
		return nil, fmt.Errorf("month field %q: %w", fields[3], err)
	}
	if err := parseField(fields[4], 0, 7, true, e.daysOfWeek[:]); err != nil {
		return nil, fmt.Errorf("day-of-week field %q: %w", fields[4], err)
	}
	return e, nil
}

// Matches reports whether t satisfies the cron expression (evaluated at minute granularity).
func (e *Expr) Matches(t time.Time) bool {
	if !e.minutes[t.Minute()] || !e.hours[t.Hour()] || !e.months[int(t.Month())] {
		return false
	}
	domMatch := e.daysOfMonth[t.Day()]
	dowMatch := e.daysOfWeek[int(t.Weekday())]

	// Standard Vixie cron semantics: when both DOM and DOW are restricted,
	// either matching fires the job; otherwise both must match.
	if !e.domWild && !e.dowWild {
		return domMatch || dowMatch
	}
	return domMatch && dowMatch
}

func parseField(field string, minVal, maxVal int, wrapSeven bool, dest []bool) error {
	parts := strings.Split(field, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return fmt.Errorf("empty list entry")
		}
		step := 1
		rangePart := part
		if base, stepStr, hasStep := strings.Cut(part, "/"); hasStep {
			s, err := strconv.Atoi(stepStr)
			if err != nil || s <= 0 {
				return fmt.Errorf("invalid step %q", stepStr)
			}
			step = s
			rangePart = base
		}

		var start, end int
		switch {
		case rangePart == "*":
			start, end = minVal, maxVal
			if wrapSeven {
				end = 6
			}
		case strings.Contains(rangePart, "-"):
			loStr, hiStr, _ := strings.Cut(rangePart, "-")
			lo, err := strconv.Atoi(loStr)
			if err != nil {
				return fmt.Errorf("invalid range start %q", loStr)
			}
			hi, err := strconv.Atoi(hiStr)
			if err != nil {
				return fmt.Errorf("invalid range end %q", hiStr)
			}
			if lo < minVal || hi > maxVal || lo > hi {
				return fmt.Errorf("range %d-%d out of bounds [%d,%d]", lo, hi, minVal, maxVal)
			}
			start, end = lo, hi
		default:
			v, err := strconv.Atoi(rangePart)
			if err != nil {
				return fmt.Errorf("invalid value %q", rangePart)
			}
			if v < minVal || v > maxVal {
				return fmt.Errorf("value %d out of bounds [%d,%d]", v, minVal, maxVal)
			}
			start, end = v, v
		}

		for v := start; v <= end; v += step {
			idx := v
			if wrapSeven && idx == 7 {
				idx = 0
			}
			dest[idx] = true
		}
	}
	return nil
}

// Job is a scheduled task executed by Scheduler.
type Job struct {
	Name   string
	Expr   *Expr
	Prompt string
	Fn     func(ctx context.Context, firedAt time.Time)
}

// Scheduler fires Jobs once per matching minute in the configured timezone.
type Scheduler struct {
	loc    *time.Location
	jobs   []Job
	logger *slog.Logger
}

// NewScheduler creates a new Scheduler.
func NewScheduler(loc *time.Location, logger *slog.Logger) *Scheduler {
	if loc == nil {
		loc = time.Local
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{loc: loc, logger: logger}
}

// Add registers a cron job.
func (s *Scheduler) Add(name, expr, prompt string, fn func(ctx context.Context, firedAt time.Time)) error {
	parsed, err := Parse(expr)
	if err != nil {
		return fmt.Errorf("job %q: %w", name, err)
	}
	s.jobs = append(s.jobs, Job{
		Name:   name,
		Expr:   parsed,
		Prompt: prompt,
		Fn:     fn,
	})
	return nil
}

// Run blocks until ctx is canceled, checking jobs once at the top of each minute.
func (s *Scheduler) Run(ctx context.Context) {
	var lastMinute int64 = -1

	for {
		now := time.Now().In(s.loc)
		nextMinute := now.Truncate(time.Minute).Add(time.Minute)
		wait := time.Until(nextMinute)
		if wait < 10*time.Millisecond {
			wait = 10 * time.Millisecond
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case tick := <-timer.C:
			ts := tick.In(s.loc).Truncate(time.Minute)
			minuteKey := ts.Unix() / 60
			if minuteKey == lastMinute {
				continue
			}
			lastMinute = minuteKey
			s.FireMatching(ctx, ts)
		}
	}
}

// FireMatching runs all jobs matching ts in separate goroutines.
func (s *Scheduler) FireMatching(ctx context.Context, ts time.Time) {
	for _, job := range s.jobs {
		if !job.Expr.Matches(ts) {
			continue
		}
		j := job
		go func() {
			defer func() {
				if r := recover(); r != nil {
					s.logger.Error("cron job panicked", "job", j.Name, "panic", r)
				}
			}()
			s.logger.Info("firing scheduled check-in", "job", j.Name, "time", ts.Format(time.RFC3339))
			j.Fn(ctx, ts)
		}()
	}
}
