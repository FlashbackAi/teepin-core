// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package solana

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// RPC is the part of the Solana JSON-RPC API a merchant needs: find the
// transactions that mention a reference, and read one back. An interface so the
// payment logic is tested against a fake and never against the network.
type RPC interface {
	// SignaturesForAddress lists finalized transactions that mention address,
	// newest first.
	SignaturesForAddress(ctx context.Context, address string, limit int) ([]SignatureInfo, error)
	// Transaction reads one finalized transaction. It returns (nil, nil) when
	// the chain has no such finalized transaction (yet).
	Transaction(ctx context.Context, signature string) (*Transaction, error)
}

// SignatureInfo is one item of getSignaturesForAddress.
type SignatureInfo struct {
	Signature string          `json:"signature"`
	Slot      uint64          `json:"slot"`
	Err       json.RawMessage `json:"err"`
	BlockTime *int64          `json:"blockTime"`
}

// Failed reports whether the transaction errored on chain.
func (s SignatureInfo) Failed() bool { return len(s.Err) > 0 && string(s.Err) != "null" }

// Transaction is a finalized transaction read with jsonParsed encoding.
type Transaction struct {
	Slot        uint64  `json:"slot"`
	BlockTime   *int64  `json:"blockTime"`
	Meta        *TxMeta `json:"meta"`
	Transaction TxBody  `json:"transaction"`
}

// TxBody is the transaction body (signatures and message).
type TxBody struct {
	Signatures []string `json:"signatures"`
	Message    struct {
		AccountKeys []AccountKey `json:"accountKeys"`
	} `json:"message"`
}

// TxMeta is the execution metadata we rely on.
type TxMeta struct {
	Err               json.RawMessage `json:"err"`
	PreTokenBalances  []TokenBalance  `json:"preTokenBalances"`
	PostTokenBalances []TokenBalance  `json:"postTokenBalances"`
}

// Failed reports whether the transaction errored on chain.
func (m *TxMeta) Failed() bool { return len(m.Err) > 0 && string(m.Err) != "null" }

// TokenBalance is one token account's balance in the transaction.
type TokenBalance struct {
	AccountIndex  int         `json:"accountIndex"`
	Mint          string      `json:"mint"`
	Owner         string      `json:"owner"`
	UITokenAmount TokenAmount `json:"uiTokenAmount"`
}

// TokenAmount is a token amount in base units, as a decimal string (it can
// exceed what a JSON number holds exactly).
type TokenAmount struct {
	Amount   string `json:"amount"`
	Decimals int    `json:"decimals"`
}

// AccountKey is one account referenced by the transaction. With jsonParsed
// encoding each key is an object; a bare string is accepted too.
type AccountKey struct {
	Pubkey   string
	Signer   bool
	Writable bool
}

// UnmarshalJSON accepts both {"pubkey": ...} objects and plain strings.
func (k *AccountKey) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*k = AccountKey{Pubkey: s}
		return nil
	}
	var o struct {
		Pubkey   string `json:"pubkey"`
		Signer   bool   `json:"signer"`
		Writable bool   `json:"writable"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return err
	}
	*k = AccountKey{Pubkey: o.Pubkey, Signer: o.Signer, Writable: o.Writable}
	return nil
}

// RPCError is an error object returned by the node.
type RPCError struct {
	Method  string
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("solana rpc %s: error %d: %s", e.Method, e.Code, e.Message)
}

const (
	// maxResponseBytes bounds one RPC response: a node must not be able to make
	// us buffer an unbounded body.
	maxResponseBytes = 8 << 20
	defaultTimeout   = 15 * time.Second
	maxAttempts      = 3
)

// HTTPClient talks JSON-RPC to a Solana node over HTTP.
//
// The endpoint URL usually carries the provider's API key, so it is treated as
// a secret: it is never logged and never appears in an error (Go's own HTTP
// errors include the full URL, which is stripped here).
type HTTPClient struct {
	endpoint string
	http     *http.Client
	sleep    func(time.Duration)
}

// NewHTTPClient builds a client for an RPC endpoint URL.
func NewHTTPClient(endpoint string) (*HTTPClient, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		// The error text deliberately omits the value: it may hold a key.
		return nil, errors.New("solana rpc endpoint must be an http(s) URL")
	}
	return &HTTPClient{
		endpoint: endpoint,
		http:     &http.Client{Timeout: defaultTimeout},
		sleep:    time.Sleep,
	}, nil
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// call performs one JSON-RPC request, retrying transport failures, rate limits
// and server errors with a short backoff. A node-reported error is not retried:
// it will not change.
func (c *HTTPClient) call(ctx context.Context, method string, params []any, out any) error {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			c.sleep(time.Duration(attempt) * 500 * time.Millisecond)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		retry, err := c.once(ctx, method, body, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retry {
			return err
		}
	}
	return lastErr
}

// once makes a single attempt and says whether trying again could help.
func (c *HTTPClient) once(ctx context.Context, method string, body []byte, out any) (retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("solana rpc %s: could not build request", method)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		// Go's transport errors carry the full URL (and so the API key) and the
		// resolved host, and a provider's hostname can itself identify the
		// account. Report only the class of failure.
		return true, fmt.Errorf("solana rpc %s: %s", method, failureClass(err))
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return true, fmt.Errorf("solana rpc %s: reading response: %w", method, err)
	}
	if len(data) > maxResponseBytes {
		return false, fmt.Errorf("solana rpc %s: response larger than %d bytes", method, maxResponseBytes)
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return true, fmt.Errorf("solana rpc %s: http %d", method, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return false, fmt.Errorf("solana rpc %s: http %d", method, resp.StatusCode)
	}

	var envelope rpcResponse
	if err := json.Unmarshal(data, &envelope); err != nil {
		return false, fmt.Errorf("solana rpc %s: malformed response", method)
	}
	if envelope.Error != nil {
		return false, &RPCError{Method: method, Code: envelope.Error.Code, Message: envelope.Error.Message}
	}
	if err := json.Unmarshal(envelope.Result, out); err != nil {
		return false, fmt.Errorf("solana rpc %s: unexpected result shape: %w", method, err)
	}
	return false, nil
}

// failureClass names a transport failure without any of its detail.
func failureClass(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "request timed out"
	}
	return "connection failed"
}

// SignaturesForAddress implements RPC.
func (c *HTTPClient) SignaturesForAddress(ctx context.Context, address string, limit int) ([]SignatureInfo, error) {
	if _, err := ParsePublicKey(address); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 1000 {
		limit = 20
	}
	var out []SignatureInfo
	err := c.call(ctx, "getSignaturesForAddress",
		[]any{address, map[string]any{"commitment": "finalized", "limit": limit}}, &out)
	return out, err
}

// Transaction implements RPC.
func (c *HTTPClient) Transaction(ctx context.Context, signature string) (*Transaction, error) {
	var out *Transaction
	err := c.call(ctx, "getTransaction", []any{signature, map[string]any{
		"encoding":                       "jsonParsed",
		"commitment":                     "finalized",
		"maxSupportedTransactionVersion": 0,
	}}, &out)
	return out, err
}
