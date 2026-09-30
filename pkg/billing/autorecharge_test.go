// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestValidateAutoRecharge(t *testing.T) {
	ok := AutoRechargeSettings{Enabled: true, Threshold: 10, Amount: 50, MonthlyCap: 250}
	if err := validateAutoRecharge(ok); err != nil {
		t.Fatalf("valid settings refused: %v", err)
	}
	cases := map[string]func(*AutoRechargeSettings){
		"amount below the top-up minimum": func(s *AutoRechargeSettings) { s.Amount = 19.99 },
		"amount above the top-up maximum": func(s *AutoRechargeSettings) { s.Amount = 1000.01; s.MonthlyCap = 5000 },
		"amount with fractions of a cent": func(s *AutoRechargeSettings) { s.Amount = 50.001 },
		"threshold below the minimum":     func(s *AutoRechargeSettings) { s.Threshold = 4.99 },
		"negative threshold":              func(s *AutoRechargeSettings) { s.Threshold = -1 },
		"cap smaller than one charge":     func(s *AutoRechargeSettings) { s.MonthlyCap = 49 },
		"cap above the maximum":           func(s *AutoRechargeSettings) { s.MonthlyCap = 10000.01 },
	}
	for name, mutate := range cases {
		s := ok
		mutate(&s)
		if err := validateAutoRecharge(s); !errors.Is(err, ErrAutoRechargeInvalid) {
			t.Errorf("%s: err = %v, want ErrAutoRechargeInvalid", name, err)
		}
	}
	// Boundaries are inclusive.
	for _, s := range []AutoRechargeSettings{
		{Threshold: 5, Amount: 20, MonthlyCap: 20},
		{Threshold: 5, Amount: 1000, MonthlyCap: 10000},
	} {
		if err := validateAutoRecharge(s); err != nil {
			t.Errorf("%+v refused: %v", s, err)
		}
	}
}

func TestMonthStartUTC(t *testing.T) {
	got := monthStartUTC(time.Date(2026, 3, 31, 23, 59, 0, 0, time.FixedZone("x", 5*3600)))
	if !got.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("monthStartUTC = %v", got)
	}
}

func TestAutoRechargeFailedMessage_RetryVersusSwitchedOff(t *testing.T) {
	retry := AutoRechargeFailedMessage(AutoRechargeFailure{Amount: 50, Reason: "insufficient funds", RetryAfter: 2 * time.Hour}, "https://c/credits")
	if retry.Subject != "Your automatic recharge payment failed" ||
		!strings.Contains(retry.Text, "$50.00") || !strings.Contains(retry.Text, "insufficient funds") ||
		!strings.Contains(retry.Text, "try again in about 2 hours") || !strings.Contains(retry.Text, "https://c/credits") {
		t.Errorf("retry email:\n%s\n%s", retry.Subject, retry.Text)
	}
	off := AutoRechargeFailedMessage(AutoRechargeFailure{Amount: 50, Reason: "card expired", Disabled: true}, "https://c/credits")
	if !strings.Contains(off.Subject, "is off") || !strings.Contains(off.Text, "switched off") ||
		strings.Contains(off.Text, "try again") || !strings.Contains(off.Text, "running services stop") {
		t.Errorf("switched-off email:\n%s\n%s", off.Subject, off.Text)
	}
}

func TestAutoRechargeCapMessage(t *testing.T) {
	m := AutoRechargeCapMessage(AutoRechargeCap{Amount: 50, Cap: 100, Spent: 60}, "https://c/credits")
	for _, want := range []string{"$50.00", "$100.00", "$60.00", "until next month", "raise the cap"} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("cap email lacks %q:\n%s", want, m.Text)
		}
	}
}

func TestAutoRechargeMessages_EscapeHTML(t *testing.T) {
	m := AutoRechargeFailedMessage(AutoRechargeFailure{Amount: 50, Reason: "<b>x</b>"}, "https://c/?a=1&b=2")
	if strings.Contains(m.HTML, "<b>x</b>") {
		t.Errorf("reason not escaped: %s", m.HTML)
	}
}

func TestReceiptMessage_SaysWhenItWasAutomatic(t *testing.T) {
	m := TopUpReceiptMessage(TopUpReceiptNotice{ReceiptNumber: "RCT-1", Amount: 50, Currency: "usd", Automatic: true}, 60, false, "https://c/r")
	if m.Subject != "Receipt RCT-1 - $50.00 automatic recharge" || !strings.Contains(m.Text, "Your automatic recharge added $50.00") {
		t.Errorf("automatic receipt:\n%s\n%s", m.Subject, m.Text)
	}
	manual := TopUpReceiptMessage(TopUpReceiptNotice{ReceiptNumber: "RCT-1", Amount: 50, Currency: "usd"}, 60, false, "https://c/r")
	if strings.Contains(manual.Text, "automatic") {
		t.Errorf("a manual purchase was called automatic:\n%s", manual.Text)
	}
}

func TestHumanWait(t *testing.T) {
	for d, want := range map[time.Duration]string{30 * time.Minute: "an hour", time.Hour: "an hour", 2 * time.Hour: "2 hours", 8 * time.Hour: "8 hours"} {
		if got := humanWait(d); got != want {
			t.Errorf("humanWait(%v) = %q, want %q", d, got, want)
		}
	}
}
