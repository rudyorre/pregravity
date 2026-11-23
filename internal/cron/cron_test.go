package cron

import (
	"testing"
	"time"
)

func TestCronParseAndMatch(t *testing.T) {
	expr, err := Parse("0 9 * * 1-5")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Monday 2026-11-23 09:00 UTC -> match
	mon9 := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	if !expr.Matches(mon9) {
		t.Fatalf("expected Monday 09:00 to match")
	}
	// Monday 09:01 -> no match
	if expr.Matches(mon9.Add(time.Minute)) {
		t.Fatalf("did not expect Monday 09:01 to match")
	}
	// Sunday 2026-11-22 09:00 UTC -> no match
	sun9 := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	if expr.Matches(sun9) {
		t.Fatalf("did not expect Sunday 09:00 to match")
	}

	// Step + Sunday=7 wrap
	stepExpr, err := Parse("*/15 * * * 7")
	if err != nil {
		t.Fatalf("parse step: %v", err)
	}
	if !stepExpr.Matches(time.Date(2026, 11, 22, 14, 30, 0, 0, time.UTC)) {
		t.Fatalf("expected Sunday 14:30 to match */15 * * * 7")
	}
	if stepExpr.Matches(time.Date(2026, 11, 22, 14, 31, 0, 0, time.UTC)) {
		t.Fatalf("did not expect Sunday 14:31 to match")
	}
}

func TestCronInvalid(t *testing.T) {
	bad := []string{
		"",
		"* * *",
		"60 * * * *",
		"* 24 * * *",
		"* * 0 * *",
		"* * * 13 *",
		"*/0 * * * *",
		"10-5 * * * *",
	}
	for _, s := range bad {
		if _, err := Parse(s); err == nil {
			t.Errorf("expected parse error for %q", s)
		}
	}
}
