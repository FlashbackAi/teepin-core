// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/billing"
)

func TestGetCreditStatus_ReportsRunwayAndLevel(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.MatchExpectationsInOrder(false)

	acct := uuid.New()
	// $24 left at a $1/hour trailing spend = a day.
	mock.ExpectQuery(`billing\.credit_balance`).WillReturnRows(sqlmock.NewRows([]string{"b"}).AddRow(24.0))
	for _, col := range []string{"vram_price_per_gb_hour", "cpu_price_per_core_hour", "memory_price_per_gb_hour",
		"storage_price_per_gb_month", "p_core_price_per_hour", "e_core_price_per_hour", "object_storage_price_per_gb_month"} {
		mock.ExpectQuery(`SELECT ` + col + ` FROM billing\.pricing`).WillReturnRows(sqlmock.NewRows([]string{col}).AddRow(0.0))
	}
	mock.ExpectQuery(`FROM compute\.instances\s+WHERE account_id = \$1 AND status = 'running'`).
		WillReturnRows(sqlmock.NewRows([]string{"a", "b", "c", "d", "e", "f"}))
	mock.ExpectQuery(`FROM compute\.instances\s+WHERE account_id = \$1 AND status = 'stopped'`).
		WillReturnRows(sqlmock.NewRows([]string{"gb", "n"}).AddRow(0, 0))
	mock.ExpectQuery(`SELECT SUM\(total_bytes\) FROM storage\.buckets`).
		WillReturnRows(sqlmock.NewRows([]string{"s"}).AddRow(nil))
	mock.ExpectQuery(`SELECT -SUM\(amount\) FROM billing\.credit_transactions`).
		WillReturnRows(sqlmock.NewRows([]string{"s"}).AddRow(24.0)) // $24 over 24h
	deleteAfter := time.Date(2026, 10, 6, 9, 30, 0, 0, time.UTC)
	mock.ExpectQuery(`FROM billing\.storage_holds`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "account_id", "held_since", "delete_after"}).
			AddRow(uuid.New(), acct, time.Now(), deleteAfter))
	mock.ExpectQuery(`FROM billing\.storage_holds`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "account_id", "held_since", "delete_after"}).
			AddRow(uuid.New(), acct, time.Now(), deleteAfter))

	h := NewBillingHandler(billing.NewService(db), nil)
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Set(string(auth.AccountIDKey), acct)
	h.GetCreditStatus(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["level"] != "day" || got["balance"] != 24.0 || got["burn_per_hour"] != 1.0 || got["runway_hours"] != 24.0 {
		t.Errorf("body = %v, want a day-level report ($24 at $1/hour)", got)
	}
	if got["impacted"] != true || got["storage_delete_after"] != "2026-10-06T09:30:00Z" {
		t.Errorf("body = %v, want the hold and its deletion date", got)
	}
}
