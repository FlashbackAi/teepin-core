// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Runs the follow-up message queue against a real Postgres. The ordinary
// tests mock the database, which checks the shape of a query but not what the
// driver actually sends: a message with no attachments was being sent as an
// empty string into a JSONB column and refused ("invalid input syntax for
// type json"), which no mocked test could see. Behind the migration-drill tag:
//
//	TEEPIN_DRILL_DSN='postgres://postgres:test@localhost:55444/teepin?sslmode=disable' \
//	  go test -tags migrationdrill -run TestMessagesIntegration ./pkg/teepinbuild/ -v
package teepinbuild

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestMessagesIntegration(t *testing.T) {
	db := secretsIntegrationDB(t)
	ctx := context.Background()
	store := NewStore(db)
	_, sess := seedSession(t, db, uuid.NewString()[:8])

	// No attachments, both ways the caller can express it.
	for _, tc := range []struct {
		name        string
		attachments []Attachment
	}{
		{"nil attachments", nil},
		{"empty attachments", []Attachment{}},
	} {
		if _, err := store.SendMessage(ctx, sess, "deploy it ("+tc.name+")", tc.attachments); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}

	// With attachments.
	want := []Attachment{{URL: "https://example.com/a.png", Type: AttachmentImage, Filename: "a.png"}}
	if _, err := store.SendMessage(ctx, sess, "look at this", want); err != nil {
		t.Fatalf("with attachments: %v", err)
	}

	// The agent's poll gets all three, oldest first, attachments intact, and
	// "none" comes back as none.
	got, err := store.PollMessages(ctx, sess)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("polled %d messages, want 3", len(got))
	}
	if len(got[0].Attachments) != 0 || len(got[1].Attachments) != 0 {
		t.Errorf("a message sent with no attachments came back with %v / %v", got[0].Attachments, got[1].Attachments)
	}
	if len(got[2].Attachments) != 1 || got[2].Attachments[0] != want[0] {
		t.Errorf("attachments did not round-trip: %+v", got[2].Attachments)
	}
	// Delivered once only.
	if again, _ := store.PollMessages(ctx, sess); len(again) != 0 {
		t.Errorf("messages were delivered twice: %v", again)
	}

	// A closed session refuses the message instead of queueing it forever.
	if _, err := db.Exec(`UPDATE billing.inference_sessions SET status = 'closed' WHERE id = $1`, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SendMessage(ctx, sess, "too late", nil); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("closed session: err = %v, want ErrSessionClosed", err)
	}
}
