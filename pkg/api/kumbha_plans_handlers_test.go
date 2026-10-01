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
	"github.com/FlashbackAi/teepin-core/pkg/kumbha"
)

// The customer approves one plan; a deploy that needs more than that plan
// covers is refused on the server, however the agent asks for it.

func TestCreateInstance_RefusedWhenItAsksForMoreThanTheApprovedPlan(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, nil, allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore, cluster: newFakeCluster()}).WithKumbha(gw)

	sessionID := uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(approvedSessionRow(sessionID))
	// The customer approved 1 vCPU / 1 GB; the request below wants 4 / 8.
	expectApprovedPlan(mock, sessionID, `[{"name":"app","cpu_units":1,"memory_gb":1,"storage_gb":0}]`)
	// No INSERT is expected: a refused request must not allocate anything.

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/",
		strings.NewReader(`{"name":"t","image":"nginx","cpu_units":4,"memory":"8GB"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(auth.ProjectIDKey), uuid.New())
	c.Set(string(auth.AccountIDKey), testAccountID)
	c.Set(string(auth.SessionIDKey), sessionID)
	server.CreateInstance(c)

	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "exceeds_approved_plan") {
		t.Fatalf("status = %d body = %s, want 403 exceeds_approved_plan", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestCreateInstance_UnreadablePlanFailsClosed(t *testing.T) {
	mock, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, nil, allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore, cluster: newFakeCluster()}).WithKumbha(gw)

	sessionID := uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(approvedSessionRow(sessionID))
	mock.ExpectQuery(`FROM billing\.inference_sessions s\s+JOIN billing\.kumbha_plans`).
		WithArgs(sessionID).WillReturnError(sqlmock.ErrCancelled)

	w := createInstanceAsSession(server, sessionID)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body = %s, want 503: a plan that cannot be read must not become permission", w.Code, w.Body.String())
	}
}

func TestParseApprovePlanID(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantID  string
		wantOK  bool
		wantNil bool
	}{
		{"no body means the newest plan", "", "", true, true},
		{"empty object means the newest plan", `{}`, "", true, true},
		{"a plan id", `{"plan_id":"6f1c2d1e-2a3b-4c5d-8e9f-0a1b2c3d4e5f"}`, "6f1c2d1e-2a3b-4c5d-8e9f-0a1b2c3d4e5f", true, false},
		{"not a uuid", `{"plan_id":"nope"}`, "", false, false},
		{"not json", `{`, "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			id, ok := parseApprovePlanID(c)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				if w.Code != http.StatusBadRequest {
					t.Errorf("status = %d, want 400", w.Code)
				}
				return
			}
			if tc.wantNil && id != uuid.Nil {
				t.Errorf("id = %s, want Nil", id)
			}
			if !tc.wantNil && id.String() != tc.wantID {
				t.Errorf("id = %s, want %s", id, tc.wantID)
			}
		})
	}
}

func TestRecordKumbhaPlan_RefusesACustomerCredentialAndAForeignSession(t *testing.T) {
	_, kStore, cStore := newMockKumbhaDB(t)
	gw := kumbha.NewGateway(kStore, nil, allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithKumbha(gw)
	sessionID := uuid.New()
	body := `{"resources":[{"name":"app","cpu_units":1,"memory_gb":1}]}`

	do := func(callerSession *uuid.UUID) int {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Params = gin.Params{{Key: "id", Value: sessionID.String()}}
		c.Set(string(auth.ProjectIDKey), uuid.New())
		c.Set(string(auth.AccountIDKey), testAccountID)
		if callerSession != nil {
			c.Set(string(auth.SessionIDKey), *callerSession)
		}
		server.RecordKumbhaPlan(c)
		return w.Code
	}
	if code := do(nil); code != http.StatusForbidden {
		t.Errorf("a customer credential recorded a plan: status %d, want 403", code)
	}
	other := uuid.New()
	if code := do(&other); code != http.StatusForbidden {
		t.Errorf("another session's credential recorded a plan: status %d, want 403", code)
	}
}
