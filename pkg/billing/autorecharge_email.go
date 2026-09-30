// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"fmt"
	"html"
	"log"
	"time"

	"github.com/FlashbackAi/teepin-core/pkg/email"
)

// AutoRechargeFailed emails that an automatic charge did not go through, and
// whether it will be tried again or has been switched off.
func (m *BillingMailer) AutoRechargeFailed(ctx context.Context, n AutoRechargeFailure) {
	to, err := accountRecipients(ctx, m.db, n.AccountID)
	if err != nil || len(to) == 0 {
		if err != nil {
			log.Printf("WARN: automatic recharge failure email: %v", err)
		}
		return
	}
	msg := AutoRechargeFailedMessage(n, m.consoleURL+"/billing/credits")
	msg.To = to
	if err := m.sender.Send(ctx, msg); err != nil {
		log.Printf("WARN: automatic recharge failure email not sent: %v", err)
	}
}

// AutoRechargeCapReached emails that the monthly cap has paused automatic
// recharge for the rest of the month.
func (m *BillingMailer) AutoRechargeCapReached(ctx context.Context, n AutoRechargeCap) {
	to, err := accountRecipients(ctx, m.db, n.AccountID)
	if err != nil || len(to) == 0 {
		if err != nil {
			log.Printf("WARN: automatic recharge cap email: %v", err)
		}
		return
	}
	msg := AutoRechargeCapMessage(n, m.consoleURL+"/billing/credits")
	msg.To = to
	if err := m.sender.Send(ctx, msg); err != nil {
		log.Printf("WARN: automatic recharge cap email not sent: %v", err)
	}
}

const autoRechargeFooter = "You are receiving this because you are an owner or admin of this Teepin account."

// AutoRechargeFailedMessage writes the email for a failed automatic charge.
func AutoRechargeFailedMessage(n AutoRechargeFailure, creditsURL string) email.Message {
	amount := fmt.Sprintf("$%.2f", n.Amount)
	var subject, lead, next string
	if n.Disabled {
		subject = "Automatic recharge is off - your payment failed"
		lead = "We could not charge " + amount + " to your saved card to top up your Teepin credit, and automatic recharge has been switched off."
		next = "Your credit will not be topped up automatically. Update your card and turn automatic recharge back on, or add credit yourself, before it runs out - when it reaches zero, running services stop."
	} else {
		subject = "Your automatic recharge payment failed"
		lead = "We could not charge " + amount + " to your saved card to top up your Teepin credit."
		next = "We will try again in about " + humanWait(n.RetryAfter) + ". To avoid running out of credit, update your card or add credit yourself."
	}
	text := lead + "\n\nReason: " + n.Reason + "\n\n" + next +
		"\n\nManage credit and your card: " + creditsURL + "\n\n" + autoRechargeFooter + "\n"
	htmlBody := "<p>" + html.EscapeString(lead) + "</p>" +
		"<p>Reason: " + html.EscapeString(n.Reason) + "</p>" +
		"<p>" + html.EscapeString(next) + "</p>" +
		"<p><a href=\"" + html.EscapeString(creditsURL) + "\">Manage credit and your card</a></p>" +
		"<p style=\"color:#666;font-size:12px\">" + autoRechargeFooter + "</p>"
	return email.Message{Subject: subject, Text: text, HTML: htmlBody}
}

// AutoRechargeCapMessage writes the email for a paused automatic recharge.
func AutoRechargeCapMessage(n AutoRechargeCap, creditsURL string) email.Message {
	body := fmt.Sprintf("Your credit is below your automatic recharge threshold, but recharging another $%.2f would go over your monthly cap of $%.2f (already used this month: $%.2f), so it is paused until next month.",
		n.Amount, n.Cap, n.Spent)
	next := "To keep running, add credit yourself or raise the cap."
	text := body + "\n\n" + next + "\n\nManage credit: " + creditsURL + "\n\n" + autoRechargeFooter + "\n"
	htmlBody := "<p>" + html.EscapeString(body) + "</p>" +
		"<p>" + html.EscapeString(next) + " <a href=\"" + html.EscapeString(creditsURL) + "\">Manage credit</a></p>" +
		"<p style=\"color:#666;font-size:12px\">" + autoRechargeFooter + "</p>"
	return email.Message{Subject: "Automatic recharge paused - monthly cap reached", Text: text, HTML: htmlBody}
}

// humanWait words a retry delay: "2 hours".
func humanWait(d time.Duration) string {
	h := int(d.Hours() + 0.5)
	if h <= 1 {
		return "an hour"
	}
	return fmt.Sprintf("%d hours", h)
}
