// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package email

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/sesv2"
)

type fakeSES struct {
	in  *sesv2.SendEmailInput
	err error
}

func (f *fakeSES) SendEmail(_ context.Context, in *sesv2.SendEmailInput, _ ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
	f.in = in
	return &sesv2.SendEmailOutput{}, f.err
}

func TestSESSender_BuildsTheRequest(t *testing.T) {
	f := &fakeSES{}
	s := &SESSender{client: f, from: "Teepin <billing@example.com>"}

	err := s.Send(context.Background(), Message{
		To: []string{"a@example.com", "b@example.com"}, Subject: "Hello", Text: "plain", HTML: "<p>rich</p>",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if *f.in.FromEmailAddress != "Teepin <billing@example.com>" {
		t.Errorf("from = %q", *f.in.FromEmailAddress)
	}
	if len(f.in.Destination.ToAddresses) != 2 {
		t.Errorf("to = %v", f.in.Destination.ToAddresses)
	}
	simple := f.in.Content.Simple
	if *simple.Subject.Data != "Hello" || *simple.Body.Text.Data != "plain" || *simple.Body.Html.Data != "<p>rich</p>" {
		t.Errorf("content = %+v", simple)
	}
}

func TestSESSender_TextOnlyHasNoHTMLPart(t *testing.T) {
	f := &fakeSES{}
	s := &SESSender{client: f, from: "x@example.com"}
	if err := s.Send(context.Background(), Message{To: []string{"a@example.com"}, Subject: "s", Text: "t"}); err != nil {
		t.Fatal(err)
	}
	if f.in.Content.Simple.Body.Html != nil {
		t.Error("an empty HTML body was sent as a part")
	}
}

func TestSESSender_ProviderFailureIsReturned(t *testing.T) {
	s := &SESSender{client: &fakeSES{err: errors.New("throttled")}, from: "x@example.com"}
	if err := s.Send(context.Background(), Message{To: []string{"a@example.com"}, Subject: "s", Text: "t"}); err == nil {
		t.Fatal("a provider failure was reported as success (the alerter would never retry it)")
	}
}

func TestSend_RejectsIncompleteMessages(t *testing.T) {
	f := &fakeSES{}
	s := &SESSender{client: f, from: "x@example.com"}
	for _, m := range []Message{
		{Subject: "s", Text: "t"},
		{To: []string{"a@example.com"}, Text: "t"},
		{To: []string{"a@example.com"}, Subject: "s"},
	} {
		if err := s.Send(context.Background(), m); err == nil {
			t.Errorf("accepted %+v", m)
		}
		if err := (LogSender{}).Send(context.Background(), m); err == nil {
			t.Errorf("log sender accepted %+v", m)
		}
	}
	if f.in != nil {
		t.Error("SES was called for an invalid message")
	}
}

func TestNewSESSender_RequiresAFromAddress(t *testing.T) {
	if _, err := NewSESSender(nil, ""); err == nil {
		t.Error("a sender with no From address was built")
	}
}
