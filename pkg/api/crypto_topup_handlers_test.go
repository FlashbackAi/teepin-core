// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/billing"
	"github.com/FlashbackAi/teepin-core/pkg/solana"
)

type noChain struct{}

func (noChain) SignaturesForAddress(context.Context, string, int) ([]solana.SignatureInfo, error) {
	return nil, nil
}
func (noChain) Transaction(context.Context, string) (*solana.Transaction, error) { return nil, nil }

func cryptoReq(h *BillingHandler, body string, viaAPIKey, viaSession bool) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/billing/credits/crypto-topups", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(auth.AccountIDKey), uuid.New())
	if viaAPIKey {
		c.Set(string(auth.ViaAPIKeyKey), true)
	}
	if viaSession {
		c.Set(string(auth.SessionIDKey), uuid.New())
	}
	h.CreateCryptoTopUp(c)
	return w
}

func solanaService(t *testing.T, _ *sql.DB, svc *billing.Service) *billing.Service {
	t.Helper()
	recipient, _ := solana.NewReference()
	mint, _ := solana.USDCMint(solana.Devnet)
	out, err := svc.WithSolana(billing.SolanaConfig{Network: solana.Devnet, Recipient: recipient, Mint: mint}, noChain{})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Paying with USDC spends a customer's money, so, like card top-ups, neither an
// API key nor an agent's session token may start it, and nothing is written.
func TestCreateCryptoTopUp_OnlySignedInUsers(t *testing.T) {
	for _, tc := range []struct {
		name               string
		viaAPIKey, session bool
	}{{"api key", true, false}, {"agent session token", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, _ := sqlmock.New()
			defer db.Close()
			h := NewBillingHandler(solanaService(t, db, billing.NewService(db)), nil)
			if w := cryptoReq(h, `{"amount":50}`, tc.viaAPIKey, tc.session); w.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", w.Code)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unexpected queries: %v", err)
			}
		})
	}
}

func TestCreateCryptoTopUp_BadAmountIs400_EvenWhenNotConfigured(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	h := NewBillingHandler(billing.NewService(db), nil)
	for _, body := range []string{`{"amount":5}`, `{"amount":19.99}`, `{"amount":5000.01}`, `{"amount":"fifty"}`, `{}`} {
		if w := cryptoReq(h, body, false, false); w.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, w.Code)
		}
	}
}

func TestCreateCryptoTopUp_NotConfiguredIs503(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	h := NewBillingHandler(billing.NewService(db), nil)
	if w := cryptoReq(h, `{"amount":50}`, false, false); w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}

func TestCreateCryptoTopUp_ReturnsAPaymentRequest(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery(`SELECT status FROM auth.accounts`).WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("active"))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM billing.credit_topups`).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	id := uuid.New()
	mock.ExpectQuery(`INSERT INTO billing.credit_topups`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(id))

	h := NewBillingHandler(solanaService(t, db, billing.NewService(db)), nil)
	w := cryptoReq(h, `{"amount":25}`, false, false)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	url, _ := got["pay_url"].(string)
	if !strings.HasPrefix(url, "solana:") || !strings.Contains(url, "amount=25") || !strings.Contains(url, "spl-token=") {
		t.Errorf("pay_url = %q; want a Solana Pay USDC request for 25", url)
	}
	if got["topup_id"] != id.String() || got["reference"] == "" || got["recipient"] == "" {
		t.Errorf("response = %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestGetCryptoTopUpConfig(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	for name, tc := range map[string]struct {
		svc  *billing.Service
		want bool
	}{"off": {billing.NewService(db), false}, "on": {solanaService(t, db, billing.NewService(db)), true}} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/billing/credits/crypto", nil)
		c.Set(string(auth.AccountIDKey), uuid.New())
		NewBillingHandler(tc.svc, nil).GetCryptoTopUpConfig(c)
		var got struct {
			Enabled bool    `json:"enabled"`
			Min     float64 `json:"min_amount"`
			Max     float64 `json:"max_amount"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
		}
		if got.Enabled != tc.want || got.Min != billing.MinTopUp || got.Max != billing.MaxCryptoTopUp {
			t.Errorf("%s: %+v", name, got)
		}
	}
}
