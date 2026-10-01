// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package solana

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.HandlerFunc) (*HTTPClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := NewHTTPClient(srv.URL + "/?api-key=SECRET-KEY-123")
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(time.Duration) {} // no real waiting in tests
	return c, srv
}

func TestSignaturesForAddress_RequestAndResult(t *testing.T) {
	var gotBody map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":[
			{"signature":"sigA","slot":10,"err":null,"blockTime":1790000000},
			{"signature":"sigB","slot":9,"err":{"InstructionError":[0,"x"]},"blockTime":null}]}`))
	})
	got, err := c.SignaturesForAddress(context.Background(), tRef, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Signature != "sigA" || got[0].Failed() || !got[1].Failed() {
		t.Fatalf("result = %+v", got)
	}
	if gotBody["method"] != "getSignaturesForAddress" {
		t.Errorf("method = %v", gotBody["method"])
	}
	params := gotBody["params"].([]any)
	cfg := params[1].(map[string]any)
	if params[0] != tRef || cfg["commitment"] != "finalized" || cfg["limit"] != float64(5) {
		t.Errorf("params = %v; want the reference, finalized commitment and the limit", params)
	}
}

func TestTransaction_AsksForParsedFinalizedAndDecodes(t *testing.T) {
	var cfg map[string]any
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Params []any }
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		cfg = req.Params[1].(map[string]any)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"slot":7,"blockTime":1,
			"meta":{"err":null,"preTokenBalances":[],"postTokenBalances":[]},
			"transaction":{"signatures":["sigA"],"message":{"accountKeys":[{"pubkey":"` + tPayer + `","signer":true,"writable":true}]}}}}`))
	})
	tx, err := c.Transaction(context.Background(), "sigA")
	if err != nil || tx == nil || tx.Slot != 7 || tx.Transaction.Signatures[0] != "sigA" {
		t.Fatalf("tx = %+v, err = %v", tx, err)
	}
	if cfg["encoding"] != "jsonParsed" || cfg["commitment"] != "finalized" || cfg["maxSupportedTransactionVersion"] != float64(0) {
		t.Errorf("config = %v; want jsonParsed, finalized, version 0", cfg)
	}
}

// A transaction the chain does not (yet) have comes back as null.
func TestTransaction_NotFoundIsNilNotAnError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`))
	})
	tx, err := c.Transaction(context.Background(), "nope")
	if err != nil || tx != nil {
		t.Fatalf("tx = %+v, err = %v; want (nil, nil)", tx, err)
	}
}

func TestRetriesRateLimitsAndServerErrorsThenSucceeds(t *testing.T) {
	var calls int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(http.StatusBadGateway)
		default:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":[]}`))
		}
	})
	if _, err := c.SignaturesForAddress(context.Background(), tRef, 1); err != nil {
		t.Fatalf("a transient failure was not retried: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestGivesUpAfterTheAttemptLimit(t *testing.T) {
	var calls int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	if _, err := c.SignaturesForAddress(context.Background(), tRef, 1); err == nil {
		t.Fatal("persistent failure reported as success")
	}
	if calls != maxAttempts {
		t.Errorf("calls = %d, want %d", calls, maxAttempts)
	}
}

// A node-reported error will not change on retry.
func TestNodeErrorIsNotRetried(t *testing.T) {
	var calls int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Invalid param"}}`))
	})
	_, err := c.SignaturesForAddress(context.Background(), tRef, 1)
	var re *RPCError
	if !errors.As(err, &re) || re.Code != -32602 {
		t.Fatalf("err = %v, want the node's RPCError", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want exactly 1", calls)
	}
}

// The endpoint URL holds the provider's API key. It must not surface in any
// error that could reach a log.
func TestErrorsNeverContainTheEndpointOrItsKey(t *testing.T) {
	// A server that is closed: the transport error would normally embed the URL.
	srv := httptest.NewServer(http.NotFoundHandler())
	endpoint := srv.URL + "/?api-key=SECRET-KEY-123"
	srv.Close()
	c, err := NewHTTPClient(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	c.sleep = func(time.Duration) {}
	_, err = c.SignaturesForAddress(context.Background(), tRef, 1)
	if err == nil {
		t.Fatal("expected a connection error")
	}
	for _, leak := range []string{"SECRET-KEY-123", "api-key", "127.0.0.1"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error leaks %q: %v", leak, err)
		}
	}

	c2, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	_, err = c2.SignaturesForAddress(context.Background(), tRef, 1)
	if err == nil || strings.Contains(err.Error(), "SECRET-KEY-123") {
		t.Errorf("http error: %v", err)
	}
}

func TestRejectsBadEndpointsWithoutEchoingThem(t *testing.T) {
	for _, bad := range []string{"", "not a url", "ftp://x/y?key=SECRET", "https://"} {
		_, err := NewHTTPClient(bad)
		if err == nil {
			t.Errorf("accepted %q", bad)
			continue
		}
		if strings.Contains(err.Error(), "SECRET") {
			t.Errorf("error echoes the endpoint: %v", err)
		}
	}
}

func TestOversizedResponseIsRefused(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"`))
		chunk := strings.Repeat("a", 1<<20)
		for i := 0; i < 9; i++ {
			_, _ = w.Write([]byte(chunk))
		}
		_, _ = w.Write([]byte(`"}`))
	})
	if _, err := c.SignaturesForAddress(context.Background(), tRef, 1); err == nil {
		t.Fatal("an 9MB response was accepted")
	}
}

func TestRespectsContextCancellation(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.SignaturesForAddress(ctx, tRef, 1); err == nil {
		t.Fatal("a cancelled request reported success")
	}
}

func TestSignaturesForAddressRejectsAMalformedAddress(t *testing.T) {
	c, _ := newTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("the node was called") })
	if _, err := c.SignaturesForAddress(context.Background(), "not-a-key", 1); err == nil {
		t.Fatal("a malformed address was sent to the node")
	}
}
