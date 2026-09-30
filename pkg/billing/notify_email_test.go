// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestTopUpReceiptMessage(t *testing.T) {
	n := TopUpReceiptNotice{ReceiptID: uuid.New(), ReceiptNumber: "RCT-2026-000007", Amount: 50, Currency: "usd", PaymentMethod: "Visa ending 4242"}
	m := TopUpReceiptMessage(n, 62.5, false, "https://console.example/billing/invoices/abc")

	if m.Subject != "Receipt RCT-2026-000007 - $50.00 credit added" {
		t.Errorf("subject = %q", m.Subject)
	}
	for _, want := range []string{"RCT-2026-000007", "$50.00", "Visa ending 4242", "$62.50", "https://console.example/billing/invoices/abc"} {
		if !strings.Contains(m.Text, want) || !strings.Contains(m.HTML, strings.ReplaceAll(want, "&", "&amp;")) {
			t.Errorf("missing %q in\n%s\n%s", want, m.Text, m.HTML)
		}
	}
	if strings.Contains(m.Text, "stopped") {
		t.Errorf("mentions stopped instances though none are: %s", m.Text)
	}
}

func TestTopUpReceiptMessage_TellsYouToStartStoppedInstances(t *testing.T) {
	m := TopUpReceiptMessage(TopUpReceiptNotice{ReceiptNumber: "RCT-1", Amount: 20, Currency: "usd"}, 20, true, "https://c/x")
	if !strings.Contains(m.Text, "choose Start") || !strings.Contains(m.HTML, "choose Start") {
		t.Errorf("no instruction to start stopped instances:\n%s", m.Text)
	}
}

func TestTopUpReceiptMessage_OtherCurrencyIsLabelled(t *testing.T) {
	m := TopUpReceiptMessage(TopUpReceiptNotice{ReceiptNumber: "RCT-1", Amount: 20, Currency: "eur"}, 20, false, "https://c/x")
	if !strings.Contains(m.Subject, "20.00 EUR") {
		t.Errorf("subject = %q", m.Subject)
	}
}

func TestTopUpReceiptMessage_EscapesHTML(t *testing.T) {
	m := TopUpReceiptMessage(TopUpReceiptNotice{ReceiptNumber: "RCT-1", Amount: 20, Currency: "usd", PaymentMethod: `<script>x</script>`}, 20, false, `https://c/x?a=1&b=2`)
	if strings.Contains(m.HTML, "<script>") {
		t.Errorf("unescaped markup in html: %s", m.HTML)
	}
}

func TestTopUpFailedMessage(t *testing.T) {
	m := TopUpFailedMessage(TopUpFailureNotice{Amount: 100, Currency: "usd", Reason: "insufficient funds"}, "https://console.example/billing/credits")
	if m.Subject != "Your Teepin payment did not complete" {
		t.Errorf("subject = %q", m.Subject)
	}
	for _, want := range []string{"$100.00", "insufficient funds", "no credit was added", "https://console.example/billing/credits"} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("missing %q in\n%s", want, m.Text)
		}
	}
}
