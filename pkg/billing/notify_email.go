// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"database/sql"
	"fmt"
	"html"
	"log"
	"strings"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/email"
)

// accountRecipients returns the addresses billing email goes to: the active
// owners and admins of the account. Members and viewers are not sent billing
// mail.
func accountRecipients(ctx context.Context, db *sql.DB, accountID uuid.UUID) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT email FROM auth.users
		WHERE account_id = $1 AND role IN ('owner', 'admin') AND status = 'active' AND deleted_at IS NULL
		  AND email <> ''`, accountID)
	if err != nil {
		return nil, fmt.Errorf("cannot read recipients: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// BillingMailer emails payment outcomes: the receipt for a credit purchase,
// and the failure of a payment that was accepted and later returned. Like the
// low-credit warnings these are mandatory service mail to owners and admins.
type BillingMailer struct {
	db         *sql.DB
	billing    *Service
	sender     email.Sender
	consoleURL string
}

// NewBillingMailer builds the mailer. consoleURL is the console base URL, used
// for links to the receipt and to add credit.
func NewBillingMailer(db *sql.DB, billing *Service, sender email.Sender, consoleURL string) *BillingMailer {
	return &BillingMailer{db: db, billing: billing, sender: sender, consoleURL: strings.TrimRight(consoleURL, "/")}
}

// TopUpSettled emails the receipt for a credit purchase. Best effort: the
// credit and the receipt already exist and are always in the console, so a
// failed email is logged and nothing else.
func (m *BillingMailer) TopUpSettled(ctx context.Context, n TopUpReceiptNotice) {
	to, err := accountRecipients(ctx, m.db, n.AccountID)
	if err != nil {
		log.Printf("WARN: receipt email for %s: %v", n.ReceiptNumber, err)
		return
	}
	if len(to) == 0 {
		log.Printf("WARN: receipt email for %s: account %s has no owner or admin with an email address", n.ReceiptNumber, n.AccountID)
		return
	}
	balance, err := m.billing.CreditBalance(ctx, n.AccountID)
	if err != nil {
		log.Printf("WARN: receipt email for %s: %v", n.ReceiptNumber, err)
		return
	}
	// If anything was stopped for lack of credit, say what to do about it.
	_, stopped, err := m.billing.stoppedDiskTotals(ctx, n.AccountID)
	if err != nil {
		stopped = 0
	}
	msg := TopUpReceiptMessage(n, balance, stopped > 0,
		m.consoleURL+"/billing/invoices/"+n.ReceiptID.String())
	msg.To = to
	if err := m.sender.Send(ctx, msg); err != nil {
		log.Printf("WARN: receipt email for %s not sent: %v", n.ReceiptNumber, err)
	}
}

// TopUpFailed emails that a payment which had been accepted for processing
// (a bank debit) did not complete, so no credit was added.
func (m *BillingMailer) TopUpFailed(ctx context.Context, n TopUpFailureNotice) {
	to, err := accountRecipients(ctx, m.db, n.AccountID)
	if err != nil || len(to) == 0 {
		if err != nil {
			log.Printf("WARN: payment failure email: %v", err)
		}
		return
	}
	msg := TopUpFailedMessage(n, m.consoleURL+"/billing/credits")
	msg.To = to
	if err := m.sender.Send(ctx, msg); err != nil {
		log.Printf("WARN: payment failure email not sent: %v", err)
	}
}

func money(currency string, amount float64) string {
	if strings.EqualFold(currency, "usd") || currency == "" {
		return fmt.Sprintf("$%.2f", amount)
	}
	return fmt.Sprintf("%.2f %s", amount, strings.ToUpper(currency))
}

// TopUpReceiptMessage writes the receipt email. It summarises the purchase
// and links to the full receipt in the console; the receipt document itself
// (numbered, with the issuer's legal details) lives there.
func TopUpReceiptMessage(n TopUpReceiptNotice, balance float64, hasStopped bool, receiptURL string) email.Message {
	amount := money(n.Currency, n.Amount)
	lines := []string{
		"Thanks - we received your payment and added " + amount + " of credit to your Teepin account.",
		"",
		"Receipt:        " + n.ReceiptNumber,
		"Amount:         " + amount,
	}
	if n.PaymentMethod != "" {
		lines = append(lines, "Paid with:      "+n.PaymentMethod)
	}
	lines = append(lines, "Credit balance: "+money(n.Currency, balance))
	extra := ""
	if hasStopped {
		extra = "Some of your instances were stopped when your credit ran out. Open them in the console and choose Start to bring them back."
		lines = append(lines, "", extra)
	}
	lines = append(lines, "", "View or download the receipt: "+receiptURL,
		"", "You are receiving this because you are an owner or admin of this Teepin account.")

	var b strings.Builder
	b.WriteString("<p>Thanks - we received your payment and added <strong>" + html.EscapeString(amount) + "</strong> of credit to your Teepin account.</p>")
	b.WriteString("<table cellpadding=\"4\" style=\"border-collapse:collapse\">")
	row := func(k, v string) {
		b.WriteString("<tr><td style=\"color:#666\">" + html.EscapeString(k) + "</td><td>" + html.EscapeString(v) + "</td></tr>")
	}
	row("Receipt", n.ReceiptNumber)
	row("Amount", amount)
	if n.PaymentMethod != "" {
		row("Paid with", n.PaymentMethod)
	}
	row("Credit balance", money(n.Currency, balance))
	b.WriteString("</table>")
	if extra != "" {
		b.WriteString("<p>" + html.EscapeString(extra) + "</p>")
	}
	b.WriteString("<p><a href=\"" + html.EscapeString(receiptURL) + "\">View or download the receipt</a></p>")
	b.WriteString("<p style=\"color:#666;font-size:12px\">You are receiving this because you are an owner or admin of this Teepin account.</p>")

	return email.Message{
		Subject: "Receipt " + n.ReceiptNumber + " - " + amount + " credit added",
		Text:    strings.Join(lines, "\n") + "\n",
		HTML:    b.String(),
	}
}

// TopUpFailedMessage writes the email for a payment that was accepted and
// then did not complete.
func TopUpFailedMessage(n TopUpFailureNotice, addCreditURL string) email.Message {
	amount := money(n.Currency, n.Amount)
	text := "Your payment of " + amount + " did not complete, so no credit was added to your Teepin account.\n\n" +
		"Reason: " + n.Reason + "\n\n" +
		"Nothing was charged for credit. To add credit, try again with another payment method: " + addCreditURL +
		"\n\nYou are receiving this because you are an owner or admin of this Teepin account.\n"
	htmlBody := "<p>Your payment of <strong>" + html.EscapeString(amount) + "</strong> did not complete, so no credit was added to your Teepin account.</p>" +
		"<p>Reason: " + html.EscapeString(n.Reason) + "</p>" +
		"<p>To add credit, <a href=\"" + html.EscapeString(addCreditURL) + "\">try again with another payment method</a>.</p>" +
		"<p style=\"color:#666;font-size:12px\">You are receiving this because you are an owner or admin of this Teepin account.</p>"
	return email.Message{Subject: "Your Teepin payment did not complete", Text: text, HTML: htmlBody}
}
