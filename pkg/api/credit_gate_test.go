// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/billing"
)

func runRequireCredit(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/x", func(c *gin.Context) {
		if s.requireCredit(c, uuid.New()) {
			c.Status(http.StatusNoContent)
		}
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/x", nil))
	return rec
}

func TestRequireCredit_AllowsWithBalance(t *testing.T) {
	rec := runRequireCredit(t, &Server{credit: &fakeCredit{balance: 0.01}})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, want the action to proceed", rec.Code)
	}
}

func TestRequireCredit_RefusesAtZero(t *testing.T) {
	rec := runRequireCredit(t, &Server{credit: &fakeCredit{balance: 0}})
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status %d, want 402", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"insufficient_credit"`) {
		t.Errorf("body %s lacks the insufficient_credit code the console keys on", rec.Body)
	}
}

func TestRequireCredit_FailsClosedWhenUnreadable(t *testing.T) {
	rec := runRequireCredit(t, &Server{credit: &fakeCredit{err: errors.New("db down")}})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
}

func TestRequireCredit_NoGuardAllowsEverything(t *testing.T) {
	// Standalone mode has no billing at all.
	rec := runRequireCredit(t, &Server{})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, want the action to proceed", rec.Code)
	}
}

type fakeHolds struct{ hold *billing.StorageHold }

func (f fakeHolds) ActiveStorageHold(context.Context, uuid.UUID) (*billing.StorageHold, error) {
	return f.hold, nil
}

func runStorageAccess(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", func(c *gin.Context) {
		if s.requireStorageAccess(c, uuid.New()) {
			c.Status(http.StatusNoContent)
		}
	})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	return rec
}

func TestStorageAccess_AllowedWithCredit(t *testing.T) {
	rec := runStorageAccess(t, &Server{credit: &fakeCredit{balance: 1}})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status %d, want access", rec.Code)
	}
}

func TestStorageAccess_HeldAtZeroCreditNamesTheDeletionDate(t *testing.T) {
	when := time.Date(2026, 10, 6, 9, 30, 0, 0, time.UTC)
	s := &Server{
		credit: &fakeCredit{balance: 0},
		holds:  fakeHolds{hold: &billing.StorageHold{DeleteAfter: when}},
	}
	rec := runStorageAccess(t, s)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status %d, want 402", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"storage_on_hold"`) || !strings.Contains(body, `"delete_after":"2026-10-06T09:30:00Z"`) {
		t.Errorf("body %s must carry the storage_on_hold code and the deletion date", body)
	}
}

// Before the sweeper has created the hold row the data is already
// inaccessible: access is decided by the balance, not by the row.
func TestStorageAccess_HeldEvenBeforeTheHoldRowExists(t *testing.T) {
	rec := runStorageAccess(t, &Server{credit: &fakeCredit{balance: 0}, holds: fakeHolds{}})
	if rec.Code != http.StatusPaymentRequired || !strings.Contains(rec.Body.String(), `"storage_on_hold"`) {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
}

func TestStorageAccess_FailsClosedWhenUnreadable(t *testing.T) {
	rec := runStorageAccess(t, &Server{credit: &fakeCredit{err: errors.New("db down")}})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
}
