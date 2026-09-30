// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/billing"
)

func callStatement(t *testing.T, role string, month string, csv bool) *httptest.ResponseRecorder {
	t.Helper()
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h := NewBillingHandler(billing.NewService(db), nil)
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Params = gin.Params{{Key: "month", Value: month}}
	c.Set(string(auth.AccountIDKey), uuid.New())
	if role != "" {
		c.Set(string(auth.RoleKey), role)
	}
	if csv {
		h.GetStatementCSV(c)
	} else {
		h.GetStatement(c)
	}
	return w
}

// Spend by project and the ledger are for the people who run the account.
func TestStatements_OnlyOwnersAndAdminsMaySee(t *testing.T) {
	for _, role := range []string{"member", "viewer", ""} {
		if w := callStatement(t, role, "2026-09", false); w.Code != http.StatusForbidden {
			t.Errorf("role %q: status %d, want 403", role, w.Code)
		}
		if w := callStatement(t, role, "2026-09", true); w.Code != http.StatusForbidden {
			t.Errorf("role %q csv: status %d, want 403", role, w.Code)
		}
	}
}

// A bad month is a clean 400 before any file is started.
func TestStatements_BadMonthIs400(t *testing.T) {
	for _, month := range []string{"nonsense", "2026-13", "2999-01", "2019-12", "2026-9"} {
		w := callStatement(t, auth.RoleOwner, month, false)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%q: status %d, want 400", month, w.Code)
		}
		w = callStatement(t, auth.RoleAdmin, month, true)
		if w.Code != http.StatusBadRequest || w.Header().Get("Content-Type") == "text/csv; charset=utf-8" {
			t.Errorf("%q csv: status %d content-type %q, want a JSON 400", month, w.Code, w.Header().Get("Content-Type"))
		}
	}
}

// Project names and descriptions are chosen by customers; a cell that starts
// with =, +, - or @ would run as a formula when the owner opens the CSV.
func TestCSVSafe(t *testing.T) {
	cases := map[string]string{
		"":                        "",
		"web":                     "web",
		"=HYPERLINK(\"x\")":       "'=HYPERLINK(\"x\")",
		"+cmd|' /C calc'!A0":      "'+cmd|' /C calc'!A0",
		"-2+3":                    "'-2+3",
		"@SUM(A1)":                "'@SUM(A1)",
		"\tsneaky":                "'\tsneaky",
		"has = inside":            "has = inside",
		"Credit purchase (RCT-1)": "Credit purchase (RCT-1)",
	}
	for in, want := range cases {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
}
