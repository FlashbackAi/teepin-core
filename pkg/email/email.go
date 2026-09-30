// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package email sends transactional email (billing notices today; account
// verification and invites later).
package email

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

// Message is one email. Text is required; HTML is optional and sent as the
// alternative body when present.
type Message struct {
	To      []string
	Subject string
	Text    string
	HTML    string
}

// Sender delivers a message. A nil error means the provider accepted it.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

func (m Message) validate() error {
	if len(m.To) == 0 {
		return errors.New("email has no recipients")
	}
	if m.Subject == "" || m.Text == "" {
		return errors.New("email needs a subject and a text body")
	}
	return nil
}

// LogSender records that an email would have been sent, without sending it:
// for local development and for a deployment where email is not set up yet.
// It logs the subject and recipient count, never addresses or the body.
type LogSender struct{}

// Send logs the message instead of delivering it.
func (LogSender) Send(_ context.Context, m Message) error {
	if err := m.validate(); err != nil {
		return err
	}
	log.Printf("email (not sent - no email provider configured): %q to %d recipient(s)", m.Subject, len(m.To))
	return nil
}

// sesAPI is the part of the SES v2 client used here.
type sesAPI interface {
	SendEmail(ctx context.Context, in *sesv2.SendEmailInput, opts ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
}

// SESSender sends through Amazon SES. The From address (or its domain) must
// be a verified SES identity, and the AWS account out of the SES sandbox to
// reach arbitrary customers.
type SESSender struct {
	client sesAPI
	from   string
}

// NewSESSender builds a sender from an SES v2 client and a From address such
// as "Teepin <support@example.com>".
func NewSESSender(client *sesv2.Client, from string) (*SESSender, error) {
	if from == "" {
		return nil, errors.New("a From address is required")
	}
	return &SESSender{client: client, from: from}, nil
}

// Send delivers the message through SES.
func (s *SESSender) Send(ctx context.Context, m Message) error {
	if err := m.validate(); err != nil {
		return err
	}
	body := &types.Body{Text: &types.Content{Data: aws.String(m.Text), Charset: aws.String("UTF-8")}}
	if m.HTML != "" {
		body.Html = &types.Content{Data: aws.String(m.HTML), Charset: aws.String("UTF-8")}
	}
	_, err := s.client.SendEmail(ctx, &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(s.from),
		Destination:      &types.Destination{ToAddresses: m.To},
		Content: &types.EmailContent{Simple: &types.Message{
			Subject: &types.Content{Data: aws.String(m.Subject), Charset: aws.String("UTF-8")},
			Body:    body,
		}},
	})
	if err != nil {
		return fmt.Errorf("ses send failed: %w", err)
	}
	return nil
}
