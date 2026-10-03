// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/teepinbuild"
)

const secretValueUnderTest = "amadeus-super-secret-value-1234"

func newSecretsServer(t *testing.T) (*Server, sqlmock.Sqlmock, *teepinbuild.SecretVault, *fakeCluster) {
	t.Helper()
	mock, kStore, cStore := newMockBuildDB(t)
	vault, err := teepinbuild.NewSecretVault("test-encryption-key")
	if err != nil {
		t.Fatal(err)
	}
	kStore.WithSecretVault(vault)
	gw := teepinbuild.NewGateway(kStore, nil, allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	fc := newFakeCluster()
	return (&Server{store: cStore, cluster: fc}).WithBuild(gw), mock, vault, fc
}

func putSecret(server *Server, sessionID uuid.UUID, name, body string, asAgent bool) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/v1/build/sessions/x/secrets/"+name, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: sessionID.String()}, {Key: "name", Value: name}}
	c.Set(string(auth.ProjectIDKey), uuid.New())
	c.Set(string(auth.AccountIDKey), testAccountID)
	if asAgent {
		c.Set(string(auth.SessionIDKey), sessionID)
	}
	server.PutBuildSecret(c)
	return w
}

// The value is stored, and it must not come back in the response.
func TestPutBuildSecret_StoresAndNeverEchoesTheValue(t *testing.T) {
	server, mock, _, _ := newSecretsServer(t)
	sessionID := uuid.New()
	mock.ExpectExec(`INSERT INTO billing\.build_session_secrets`).
		WithArgs(sessionID, "AMADEUS_CLIENT_ID", sqlmock.AnyArg(), testAccountID, teepinbuild.MaxSecretsPerSession).
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := putSecret(server, sessionID, "AMADEUS_CLIENT_ID", `{"value":"`+secretValueUnderTest+`"}`, false)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), secretValueUnderTest) {
		t.Errorf("the response echoed the secret: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "AMADEUS_CLIENT_ID") {
		t.Errorf("the response should name the saved secret: %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// An agent credential must never be able to write a secret, whatever the
// route allowlist says: the handler refuses it too.
func TestPutBuildSecret_RefusesAnAgentCredential(t *testing.T) {
	server, mock, _, _ := newSecretsServer(t)
	sessionID := uuid.New()

	w := putSecret(server, sessionID, "API_KEY", `{"value":"x"}`, true)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the database was touched for an agent credential: %v", err)
	}
}

func TestPutBuildSecret_RejectsBadInput(t *testing.T) {
	server, _, _, _ := newSecretsServer(t)
	sessionID := uuid.New()
	cases := []struct{ name, key, body string }{
		{"lower-case name", "api_key", `{"value":"x"}`},
		{"reserved name", "TEEPIN_SESSION_TOKEN", `{"value":"x"}`},
		{"empty value", "API_KEY", `{"value":""}`},
		{"not json", "API_KEY", `nope`},
	}
	for _, tc := range cases {
		w := putSecret(server, sessionID, tc.key, tc.body, false)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body: %s)", tc.name, w.Code, w.Body.String())
		}
	}
	// A rejected body must not be quoted back: it can be the secret itself.
	w := putSecret(server, sessionID, "API_KEY", `{"value": "`+secretValueUnderTest+`"`, false) // truncated JSON
	if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), secretValueUnderTest) {
		t.Errorf("a malformed body must be rejected without quoting it: %d %s", w.Code, w.Body.String())
	}
}

func TestPutBuildSecret_WithoutAVaultIs404(t *testing.T) {
	mock, kStore, cStore := newMockBuildDB(t) // no vault
	_ = mock
	gw := teepinbuild.NewGateway(kStore, nil, allowGate{}, &fakeKPricing{}, noopUsageRecorder{})
	server := (&Server{store: cStore}).WithBuild(gw)
	if w := putSecret(server, uuid.New(), "API_KEY", `{"value":"x"}`, false); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestListBuildSecrets_ReturnsNamesOnlyAndRefusesAgents(t *testing.T) {
	server, mock, vault, _ := newSecretsServer(t)
	sessionID := uuid.New()
	_, _ = vault.Seal(sessionID, "X", "y")
	mock.ExpectQuery(`SELECT k\.name, k\.updated_at`).WithArgs(sessionID, testAccountID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "updated_at"}).AddRow("AMADEUS_CLIENT_ID", time.Now()))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Params = gin.Params{{Key: "id", Value: sessionID.String()}}
	c.Set(string(auth.ProjectIDKey), uuid.New())
	c.Set(string(auth.AccountIDKey), testAccountID)
	server.ListBuildSecrets(c)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "AMADEUS_CLIENT_ID") {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}

	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	c2.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c2.Params = gin.Params{{Key: "id", Value: sessionID.String()}}
	c2.Set(string(auth.ProjectIDKey), uuid.New())
	c2.Set(string(auth.AccountIDKey), testAccountID)
	c2.Set(string(auth.SessionIDKey), sessionID)
	server.ListBuildSecrets(c2)
	if w2.Code != http.StatusForbidden {
		t.Errorf("an agent credential listed secrets: %d", w2.Code)
	}
}

// The whole point of the feature: a secret the customer saved reaches the
// spec of the app the agent deploys, the agent supplies no value for it, and
// nothing the agent can read carries it.
func TestCreateInstance_InjectsTheCustomersSecretsIntoTheAppSpec(t *testing.T) {
	server, mock, vault, fc := newSecretsServer(t)
	sessionID := uuid.New()

	sealed, err := vault.Seal(sessionID, "AMADEUS_CLIENT_ID", secretValueUnderTest)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).
		WithArgs(sessionID, testAccountID).
		WillReturnRows(approvedSessionRow(sessionID))
	expectNoApprovedPlan(mock, sessionID)
	mock.ExpectQuery(`SELECT k\.name, k\.sealed`).WithArgs(sessionID, testAccountID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "sealed"}).AddRow("AMADEUS_CLIENT_ID", sealed))
	mock.ExpectQuery(`INSERT INTO compute\.instances`).WillReturnRows(
		sqlmock.NewRows([]string{"created_at", "updated_at"}).AddRow(time.Now(), time.Now()))
	mock.ExpectExec(`UPDATE billing\.build_workspace_versions`).WithArgs(sessionID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE billing\.inference_sessions\s+SET last_deployed_version`).WithArgs(sessionID).WillReturnResult(sqlmock.NewResult(0, 1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	// The agent names the variable with a placeholder, as it would while it
	// cannot know the real value.
	c.Request = httptest.NewRequest(http.MethodPost, "/",
		strings.NewReader(`{"name":"t","image":"nginx","cpu_units":1,"memory":"1GB","env":{"AMADEUS_CLIENT_ID":"placeholder-from-agent"}}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(auth.ProjectIDKey), uuid.New())
	c.Set(string(auth.AccountIDKey), testAccountID)
	c.Set(string(auth.SessionIDKey), sessionID)
	server.CreateInstance(c)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %s)", w.Code, w.Body.String())
	}
	if got := fc.lastSpec.Env["AMADEUS_CLIENT_ID"]; got != secretValueUnderTest {
		t.Errorf("the customer's secret did not reach the app's environment: %v", fc.lastSpec.Env)
	}
	if strings.Contains(w.Body.String(), secretValueUnderTest) {
		t.Errorf("the create-instance response, which the agent reads, carries the secret: %s", w.Body.String())
	}
}

// withBuildSecrets is where the merge happens; a saved secret beats a
// same-named placeholder the agent may have supplied.
func TestWithBuildSecrets_SavedSecretWinsOverAPlaceholder(t *testing.T) {
	server, mock, vault, _ := newSecretsServer(t)
	sessionID := uuid.New()
	sealed, _ := vault.Seal(sessionID, "API_KEY", "real-value")
	mock.ExpectQuery(`SELECT k\.name, k\.sealed`).WithArgs(sessionID, testAccountID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "sealed"}).AddRow("API_KEY", sealed))

	in := map[string]string{"API_KEY": "placeholder", "MODE": "production"}
	out, err := server.withBuildSecrets(t.Context(), sessionID, testAccountID, in)
	if err != nil {
		t.Fatal(err)
	}
	if out["API_KEY"] != "real-value" || out["MODE"] != "production" {
		t.Errorf("merged = %v", out)
	}
	if in["API_KEY"] != "placeholder" {
		t.Error("the caller's map was mutated")
	}
}

func TestWithBuildSecrets_NoSessionMeansNoLookup(t *testing.T) {
	server, mock, _, _ := newSecretsServer(t)
	in := map[string]string{"A": "b"}
	out, err := server.withBuildSecrets(t.Context(), uuid.Nil, testAccountID, in)
	if err != nil || out["A"] != "b" {
		t.Errorf("out = %v, err = %v", out, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("a non-Teepin Build create looked up secrets: %v", err)
	}
}
