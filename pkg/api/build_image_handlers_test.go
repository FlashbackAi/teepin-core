// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/inference"
	"github.com/FlashbackAi/teepin-core/pkg/teepinbuild"
)

// imageBackend serves completions from models and lists readers separately.
type imageBackend struct {
	teepinbuild.StaticModels
	readers []teepinbuild.Model
}

func (b imageBackend) ImageReaders(context.Context) ([]teepinbuild.Model, error) {
	return b.readers, nil
}

var tinyPNG = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)

func describeBody(image []byte, request string) string {
	b, _ := json.Marshal(map[string]string{"image_base64": base64.StdEncoding.EncodeToString(image), "request": request})
	return string(b)
}

// describeAsSession calls DescribeBuildImage with a session credential for
// credentialSession, addressed at pathSession.
func describeAsSession(server *Server, pathSession, credentialSession uuid.UUID, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: pathSession.String()}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(auth.ProjectIDKey), uuid.New())
	c.Set(string(auth.AccountIDKey), testAccountID)
	if credentialSession != uuid.Nil {
		c.Set(string(auth.SessionIDKey), credentialSession)
	}
	server.DescribeBuildImage(c)
	return w
}

func newImageServer(t *testing.T, readers []teepinbuild.Model, provider *stubProvider) (*Server, sqlmock.Sqlmock) {
	t.Helper()
	mock, kStore, cStore := newMockBuildDB(t)
	backend := imageBackend{
		StaticModels: teepinbuild.StaticModels{{Route: "teepin/omni", Engine: "vllm", Provider: provider}},
		readers:      readers,
	}
	gw := teepinbuild.NewGateway(kStore, backend, allowGate{}, &fakeKPricing{in: 2, out: 8}, noopUsageRecorder{})
	return (&Server{store: cStore}).WithBuild(gw), mock
}

var visionReader = teepinbuild.Model{Route: "teepin/omni", Engine: "vllm", SupportsVision: true}

func TestDescribeImage_NeedsTheSessionsOwnCredential(t *testing.T) {
	server, _ := newImageServer(t, []teepinbuild.Model{visionReader}, &stubProvider{})
	id := uuid.New()

	if w := describeAsSession(server, id, uuid.Nil, describeBody(tinyPNG, "")); w.Code != http.StatusForbidden {
		t.Errorf("no session credential: status = %d, want 403", w.Code)
	}
	if w := describeAsSession(server, id, uuid.New(), describeBody(tinyPNG, "")); w.Code != http.StatusForbidden {
		t.Errorf("another session's credential: status = %d, want 403", w.Code)
	}
}

func TestDescribeImage_RejectsBadInput(t *testing.T) {
	server, mock := newImageServer(t, []teepinbuild.Model{visionReader}, &stubProvider{})
	id := uuid.New()

	if w := describeAsSession(server, id, id, `{"image_base64": "***not base64***"}`); w.Code != http.StatusBadRequest {
		t.Errorf("bad base64: status = %d body = %s", w.Code, w.Body.String())
	}
	if w := describeAsSession(server, id, id, `{`); w.Code != http.StatusBadRequest {
		t.Errorf("bad json: status = %d", w.Code)
	}
	// Not an image: refused by the gateway after the session is loaded.
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).WithArgs(id, testAccountID).WillReturnRows(sessionRow(id))
	w := describeAsSession(server, id, id, describeBody([]byte("this is plain text, not an image"), ""))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "bad_image") {
		t.Errorf("not an image: status = %d body = %s", w.Code, w.Body.String())
	}
}

func TestDescribeImage_NoReaderIsServiceUnavailableWithItsOwnCode(t *testing.T) {
	server, mock := newImageServer(t, nil, &stubProvider{})
	id := uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).WithArgs(id, testAccountID).WillReturnRows(sessionRow(id))

	w := describeAsSession(server, id, id, describeBody(tinyPNG, ""))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "no_image_reader") {
		t.Errorf("status = %d body = %s, want 503 no_image_reader", w.Code, w.Body.String())
	}
}

func TestDescribeImage_ReturnsTheDescriptionAndBillsTheSession(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":"A sign-in page with two fields."}}]}`
	server, mock := newImageServer(t, []teepinbuild.Model{visionReader}, &stubProvider{body: []byte(body), usage: inference.Usage{InputTokens: 1000, OutputTokens: 100}})
	id := uuid.New()
	mock.ExpectQuery(`SELECT .+ FROM billing\.inference_sessions`).WithArgs(id, testAccountID).WillReturnRows(sessionRow(id))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT status, budget, spent FROM billing\.inference_sessions`).
		WithArgs(id, testAccountID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "budget", "spent"}).AddRow("open", 5.0, 0.0))
	mock.ExpectExec(`UPDATE billing\.inference_sessions SET spent`).WithArgs(id, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO billing\.inference_session_usage`).
		WithArgs(id, "teepin/omni", "vllm", 1000, 100).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	w := describeAsSession(server, id, id, describeBody(tinyPNG, "A sign-in page"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Description != "A sign-in page with two fields." {
		t.Errorf("body = %s", w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the session was not billed for the read: %v", err)
	}
}

func TestDescribeImage_IsOnTheSessionAllowlist(t *testing.T) {
	if !auth.SessionMayCall("POST", "/v1/build/sessions/:id/describe-image") {
		t.Error("the builder pod cannot call describe-image with its session credential")
	}
}
