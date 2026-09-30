// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"strings"
	"testing"
	"time"
)

func TestLevelFor(t *testing.T) {
	cases := []struct {
		name     string
		balance  float64
		burn     float64
		impacted bool
		want     AlertLevel
	}{
		{"plenty of runway", 500, 1, false, LevelNone},
		{"exactly three days", 72, 1, false, LevelLow},
		{"under $20 is low even with a long runway", 19, 0.01, false, LevelLow},
		{"a day left", 24, 1, false, LevelDay},
		{"six hours left", 6, 1, false, LevelSixHours},
		{"zero while spending", 0, 2, false, LevelOutOfCredit},
		{"zero with things on hold", 0, 0, true, LevelOutOfCredit},
		{"zero, nothing running, nothing held", 0, 0, false, LevelNone},
		{"idle account with a small balance is not warned", 5, 0, false, LevelNone},
		{"idle account with things held and credit is not out of credit", 5, 0, true, LevelNone},
	}
	for _, c := range cases {
		if got := levelFor(c.balance, c.burn, c.impacted); got != c.want {
			t.Errorf("%s: level %v, want %v", c.name, got, c.want)
		}
	}
}

func rr(balance float64, level AlertLevel, impacted bool) RunwayReport {
	return RunwayReport{Balance: balance, BurnPerHour: 1, Level: level, Impacted: impacted}
}

func TestDecideAlert(t *testing.T) {
	cases := []struct {
		name string
		st   alertState
		r    RunwayReport
		want alertDecision
	}{
		{"first sight at a worrying level warns", alertState{}, rr(50, LevelDay, false), alertDecision{LevelDay, true}},
		{"first sight when healthy is silent", alertState{}, rr(5000, LevelNone, false), alertDecision{LevelNone, false}},
		{"escalation warns", alertState{Level: LevelLow, LastBalance: 60, Exists: true}, rr(20, LevelDay, false), alertDecision{LevelDay, true}},
		{"jumping straight to out of credit warns once", alertState{Level: LevelLow, LastBalance: 5, Exists: true}, rr(0, LevelOutOfCredit, false), alertDecision{LevelOutOfCredit, true}},
		{"the same level is not repeated", alertState{Level: LevelDay, LastBalance: 24, Exists: true}, rr(23, LevelDay, false), alertDecision{LevelDay, false}},
		{"a better reading without a top-up does not re-arm", alertState{Level: LevelSixHours, LastBalance: 6, Exists: true}, rr(5.9, LevelLow, false), alertDecision{LevelSixHours, false}},
		{"a top-up re-arms silently at the new level", alertState{Level: LevelOutOfCredit, LastBalance: 0, Exists: true}, rr(100, LevelNone, false), alertDecision{LevelNone, false}},
		{"a top-up that is still thin re-arms at that level without warning", alertState{Level: LevelSixHours, LastBalance: 5, Exists: true}, rr(30, LevelDay, false), alertDecision{LevelDay, false}},
		{"after a top-up the next slide warns again", alertState{Level: LevelNone, LastBalance: 100, Exists: true}, rr(70, LevelLow, false), alertDecision{LevelLow, true}},
		{"nothing at risk resets the ladder", alertState{Level: LevelLow, LastBalance: 10, Exists: true}, rr(10, LevelNone, false), alertDecision{LevelNone, false}},
		{"things still held keep the level (no repeat of the out-of-credit notice)", alertState{Level: LevelOutOfCredit, LastBalance: 0.03, Exists: true}, rr(0.03, LevelNone, true), alertDecision{LevelOutOfCredit, false}},
	}
	for _, c := range cases {
		if got := decideAlert(c.st, c.r); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestAlertMessage_OutOfCreditNamesTheDeadline(t *testing.T) {
	when := time.Date(2026, 10, 6, 9, 30, 0, 0, time.UTC)
	m := AlertMessage(LevelOutOfCredit, RunwayReport{Balance: 0}, "https://console.example/billing/credits", &when)
	if !strings.Contains(m.Subject, "run out") {
		t.Errorf("subject %q", m.Subject)
	}
	for _, want := range []string{"6 Oct 2026 09:30 UTC", "https://console.example/billing/credits", "permanently deleted", "start your stopped instances"} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, m.Text)
		}
	}
	if !strings.Contains(m.HTML, `href="https://console.example/billing/credits"`) {
		t.Errorf("html lacks the link: %s", m.HTML)
	}
}

func TestAlertMessage_WarningSaysHowLongAndWhatHappensAtZero(t *testing.T) {
	m := AlertMessage(LevelDay, RunwayReport{Balance: 24, BurnPerHour: 1}, "https://c/x", nil)
	if !strings.Contains(m.Text, "$24.00") || !strings.Contains(m.Text, "24 hours") {
		t.Errorf("text lacks the balance and runway:\n%s", m.Text)
	}
	if !strings.Contains(m.Text, "held for 7 days") {
		t.Errorf("text does not say what happens at zero:\n%s", m.Text)
	}
}

// The message must never inject markup from any field into the HTML body.
func TestAlertMessage_EscapesHTML(t *testing.T) {
	m := AlertMessage(LevelLow, RunwayReport{Balance: 10, BurnPerHour: 1}, `https://c/x?a=1&b="2"`, nil)
	if strings.Contains(m.HTML, `b="2"`) {
		t.Errorf("unescaped quote in html: %s", m.HTML)
	}
}

func TestHumanRunway(t *testing.T) {
	cases := []struct {
		balance, burn float64
		want          string
	}{
		{0.5, 1, "less than an hour"},
		{1, 1, "1 hour"},
		{5, 1, "5 hours"},
		{47, 1, "47 hours"},
		{72, 1, "3 days"},
		{10, 0, "an unknown time"},
	}
	for _, c := range cases {
		if got := humanRunway(RunwayReport{Balance: c.balance, BurnPerHour: c.burn}); got != c.want {
			t.Errorf("humanRunway(%v, %v) = %q, want %q", c.balance, c.burn, got, c.want)
		}
	}
}
