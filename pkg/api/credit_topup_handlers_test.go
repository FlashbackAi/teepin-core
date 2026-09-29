// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/billing"
)

// topUpReq posts a top-up request as a caller authenticated the given way.
func topUpReq(h *BillingHandler, body string, viaAPIKey, viaSession bool) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/billing/credits/topups", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(auth.AccountIDKey), uuid.New())
	if viaAPIKey {
		c.Set(string(auth.ViaAPIKeyKey), true)
	}
	if viaSession {
		c.Set(string(auth.SessionIDKey), uuid.New())
	}
	h.CreateCreditTopUp(c)
	return w
}

// Buying credit spends a customer's money, so neither an API key nor an
// agent's session token may start it — and the refusal happens before
// anything is written.
func TestCreateCreditTopUp_OnlySignedInUsers(t *testing.T) {
	for _, tc := range []struct {
		name               string
		viaAPIKey, session bool
	}{
		{"api key", true, false},
		{"agent session token", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, _ := sqlmock.New()
			defer db.Close()
			h := NewBillingHandler(billing.NewService(db), nil)

			if w := topUpReq(h, `{"amount":50}`, tc.viaAPIKey, tc.session); w.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", w.Code)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unexpected queries: %v", err)
			}
		})
	}
}

func TestCreateCreditTopUp_BadAmountIs400(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	// Payments not configured either — the amount check must still win,
	// so the customer is told what to fix.
	h := NewBillingHandler(billing.NewService(db), nil)

	for _, body := range []string{`{"amount":5}`, `{"amount":"fifty"}`, `{}`} {
		if w := topUpReq(h, body, false, false); w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, w.Code)
		}
	}
}

func TestCreateCreditTopUp_NotConfiguredIs503(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	h := NewBillingHandler(billing.NewService(db), nil)

	if w := topUpReq(h, `{"amount":50}`, false, false); w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}
