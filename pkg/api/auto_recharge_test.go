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

func putAutoRecharge(t *testing.T, mock func(sqlmock.Sqlmock), body string, viaAPIKey bool) *httptest.ResponseRecorder {
	t.Helper()
	db, m, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if mock != nil {
		mock(m)
	}
	h := NewBillingHandler(billing.NewService(db), nil)
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(auth.AccountIDKey), uuid.New())
	c.Set(string(auth.UserIDKey), uuid.New())
	if viaAPIKey {
		c.Set(string(auth.ViaAPIKeyKey), true)
		c.Set(string(auth.ScopesKey), []string{"compute:read"})
	}
	h.PutAutoRecharge(c)
	return w
}

// Automatic recharge authorises charging the saved card with nobody present,
// so only a signed-in person may turn it on.
func TestPutAutoRecharge_APIKeyCannotAuthoriseCharges(t *testing.T) {
	w := putAutoRecharge(t, nil, `{"enabled":true,"threshold":10,"amount":50,"monthly_cap":250}`, true)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", w.Code)
	}
}

func TestPutAutoRecharge_RejectsOutOfRangeSettingsBeforeTouchingTheDatabase(t *testing.T) {
	w := putAutoRecharge(t, nil, `{"enabled":true,"threshold":10,"amount":5,"monthly_cap":250}`, false)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_auto_recharge") {
		t.Fatalf("status %d body %s, want 400 invalid_auto_recharge", w.Code, w.Body)
	}
}

func TestPutAutoRecharge_NeedsASavedCard(t *testing.T) {
	w := putAutoRecharge(t, func(m sqlmock.Sqlmock) {
		m.ExpectQuery(`FROM billing\.payment_methods`).WillReturnRows(sqlmock.NewRows([]string{"pm", "m", "y"}))
	}, `{"enabled":true,"threshold":10,"amount":50,"monthly_cap":250}`, false)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "no_default_card") {
		t.Fatalf("status %d body %s, want 409 no_default_card", w.Code, w.Body)
	}
}

func TestPutAutoRecharge_MalformedBodyIs400(t *testing.T) {
	if w := putAutoRecharge(t, nil, `not json`, false); w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", w.Code)
	}
}
