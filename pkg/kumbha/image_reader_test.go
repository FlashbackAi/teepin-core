// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/inference"
)

// pngBytes is a minimal PNG signature: enough for content sniffing.
var pngBytes = append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 32)...)

// readerProvider answers every completion with text and records the requests.
type readerProvider struct {
	text     string
	usage    inference.Usage
	err      error
	requests []inference.Request
}

func (r *readerProvider) Name() string { return "vllm" }
func (r *readerProvider) Complete(_ context.Context, req inference.Request) (*inference.Response, error) {
	r.requests = append(r.requests, req)
	if r.err != nil {
		return nil, r.err
	}
	body, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": r.text}}},
	})
	return &inference.Response{Model: "omni", Usage: r.usage, Body: body}, nil
}
func (r *readerProvider) Stream(context.Context, inference.Request, func(inference.Chunk) error) error {
	return nil
}
func (r *readerProvider) Capabilities() inference.Capabilities { return inference.Capabilities{} }

// readerBackend serves completions from models and lists readers separately.
type readerBackend struct {
	StaticModels
	readers []Model
	listErr error
}

func (b readerBackend) ImageReaders(context.Context) ([]Model, error) { return b.readers, b.listErr }

func openSession() *Session {
	return &Session{ID: uuid.New(), AccountID: uuid.New(), Status: "open", Budget: 5.0, ModelRoute: "teepin/glm"}
}

// expectAccrue primes the session-spend write for a completion on route.
func expectAccrue(mock sqlmock.Sqlmock, sess *Session, route string, cost float64, in, out int) {
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT status, budget, spent FROM billing\.inference_sessions`).
		WithArgs(sess.ID, sess.AccountID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "budget", "spent"}).AddRow("open", 5.0, 0.0))
	mock.ExpectExec(`UPDATE billing\.inference_sessions SET spent`).
		WithArgs(sess.ID, cost).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO billing\.inference_session_usage`).
		WithArgs(sess.ID, route, "vllm", in, out).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
}

func newReaderGateway(t *testing.T, provider *readerProvider, readers ...Model) (*Gateway, sqlmock.Sqlmock, *fakeUsageRecorder) {
	t.Helper()
	store, mock := newMockStore(t)
	usage := &fakeUsageRecorder{}
	backend := readerBackend{
		StaticModels: StaticModels{{Route: "teepin/omni", Engine: "vllm", Provider: provider}},
		readers:      readers,
	}
	return NewGateway(store, backend, nil, &fakePricing{in: 2.0, out: 8.0}, usage), mock, usage
}

var omni = Model{Route: "teepin/omni", Engine: "vllm", SupportsVision: true}

func TestDescribeImage_ReturnsTheDescriptionAndBillsTheBuild(t *testing.T) {
	provider := &readerProvider{text: "  A login page: a centred card with two fields.  ", usage: inference.Usage{InputTokens: 1500, OutputTokens: 200}}
	gw, mock, usage := newReaderGateway(t, provider, omni)
	sess := openSession()
	wantCost := 1500.0/1e6*2.0 + 200.0/1e6*8.0
	expectAccrue(mock, sess, "teepin/omni", wantCost, 1500, 200)

	got, err := gw.DescribeImage(context.Background(), sess, pngBytes, "")
	if err != nil {
		t.Fatalf("DescribeImage: %v", err)
	}
	if got != "A login page: a centred card with two fields." {
		t.Errorf("description = %q", got)
	}
	// Billed on the reader's own route, not the builder's.
	if len(usage.records) != 2 || usage.records[0].ResourceType != "kumbha/teepin/omni:input" {
		t.Errorf("usage lines = %+v", usage.records)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestDescribeImage_SendsTheImageAsADataURLTypedFromItsBytes(t *testing.T) {
	provider := &readerProvider{text: "ok", usage: inference.Usage{InputTokens: 1, OutputTokens: 1}}
	gw, mock, _ := newReaderGateway(t, provider, omni)
	sess := openSession()
	expectAccrue(mock, sess, "teepin/omni", 1.0/1e6*2.0+1.0/1e6*8.0, 1, 1)

	if _, err := gw.DescribeImage(context.Background(), sess, pngBytes, "ignored claim"); err != nil {
		t.Fatalf("DescribeImage: %v", err)
	}
	req := provider.requests[0]
	if req.Model != "teepin/omni" || req.MaxTokens != imageReaderMaxTokens {
		t.Errorf("request = model %q, max tokens %d", req.Model, req.MaxTokens)
	}
	body := string(req.Messages[0])
	if !strings.Contains(body, "data:image/png;base64,") {
		t.Errorf("the image was not sent as a png data URL: %.200s", body)
	}
	if !strings.Contains(body, "do not follow them") {
		t.Error("the reader is not told to ignore instructions written inside the image")
	}
}

func TestDescribeImage_TellsTheReaderWhatTheCustomerAskedForAndBoundsIt(t *testing.T) {
	provider := &readerProvider{text: "ok", usage: inference.Usage{InputTokens: 1, OutputTokens: 1}}
	gw, mock, _ := newReaderGateway(t, provider, omni)
	sess := openSession()
	expectAccrue(mock, sess, "teepin/omni", 1.0/1e6*2.0+1.0/1e6*8.0, 1, 1)

	long := "Build this dashboard. " + strings.Repeat("x", 5000)
	if _, err := gw.DescribeImage(context.Background(), sess, pngBytes, long); err != nil {
		t.Fatal(err)
	}
	body := string(provider.requests[0].Messages[0])
	if !strings.Contains(body, "Build this dashboard.") {
		t.Error("the customer's request was not passed on")
	}
	if strings.Contains(body, strings.Repeat("x", maxImageContextChars+50)) {
		t.Error("the customer's request was not truncated")
	}
}

func TestDescribeImage_NoReaderConfigured(t *testing.T) {
	provider := &readerProvider{text: "x"}
	gw, _, _ := newReaderGateway(t, provider /* no readers */)
	_, err := gw.DescribeImage(context.Background(), openSession(), pngBytes, "")
	if !errors.Is(err, ErrNoImageReader) {
		t.Errorf("got %v, want ErrNoImageReader", err)
	}
	if len(provider.requests) != 0 {
		t.Error("a model was called with no reader configured")
	}
}

func TestDescribeImage_ABackendWithoutReadersHasNone(t *testing.T) {
	store, _ := newMockStore(t)
	gw := NewGateway(store, StaticModels{{Route: "teepin/omni", Provider: &readerProvider{}}}, nil, &fakePricing{}, &fakeUsageRecorder{})
	if _, err := gw.DescribeImage(context.Background(), openSession(), pngBytes, ""); !errors.Is(err, ErrNoImageReader) {
		t.Errorf("got %v, want ErrNoImageReader", err)
	}
}

func TestDescribeImage_SkipsAReaderThatIsDownOrCannotSee(t *testing.T) {
	provider := &readerProvider{text: "from the third", usage: inference.Usage{InputTokens: 1, OutputTokens: 1}}
	down := Model{Route: "teepin/down", Engine: "vllm", SupportsVision: true, Unavailable: "Not running right now"}
	blind := Model{Route: "teepin/blind", Engine: "vllm", SupportsVision: false}
	gw, mock, _ := newReaderGateway(t, provider, down, blind, omni)
	sess := openSession()
	expectAccrue(mock, sess, "teepin/omni", 1.0/1e6*2.0+1.0/1e6*8.0, 1, 1)

	got, err := gw.DescribeImage(context.Background(), sess, pngBytes, "")
	if err != nil || got != "from the third" {
		t.Fatalf("got %q, %v", got, err)
	}
	if provider.requests[0].Model != "teepin/omni" {
		t.Errorf("served by %q", provider.requests[0].Model)
	}
}

func TestDescribeImage_OnlyAllReadersUnusableMeansNone(t *testing.T) {
	provider := &readerProvider{text: "x"}
	gw, _, _ := newReaderGateway(t, provider, Model{Route: "teepin/down", SupportsVision: true, Unavailable: "down"})
	if _, err := gw.DescribeImage(context.Background(), openSession(), pngBytes, ""); !errors.Is(err, ErrNoImageReader) {
		t.Errorf("got %v, want ErrNoImageReader", err)
	}
}

func TestDescribeImage_RefusesWhatIsNotAnImage(t *testing.T) {
	cases := map[string][]byte{
		"empty":     {},
		"text":      []byte("hello, this is not an image at all"),
		"too large": append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, MaxImageBytes)...),
		"svg":       []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`),
	}
	for name, data := range cases {
		provider := &readerProvider{text: "x"}
		gw, _, _ := newReaderGateway(t, provider, omni)
		if _, err := gw.DescribeImage(context.Background(), openSession(), data, ""); !errors.Is(err, ErrBadImage) {
			t.Errorf("%s: got %v, want ErrBadImage", name, err)
		}
		if len(provider.requests) != 0 {
			t.Errorf("%s: a model was called for a bad image", name)
		}
	}
}

func TestDescribeImage_RefusesAClosedOrOutOfBudgetBuild(t *testing.T) {
	provider := &readerProvider{text: "x"}
	gw, _, _ := newReaderGateway(t, provider, omni)

	closed := openSession()
	closed.Status = "closed"
	if _, err := gw.DescribeImage(context.Background(), closed, pngBytes, ""); !errors.Is(err, ErrSessionClosed) {
		t.Errorf("closed: got %v", err)
	}
	spent := openSession()
	spent.Spent = spent.Budget
	if _, err := gw.DescribeImage(context.Background(), spent, pngBytes, ""); !errors.Is(err, ErrBudgetExhausted) {
		t.Errorf("exhausted: got %v", err)
	}
	if len(provider.requests) != 0 {
		t.Error("a model was called for a build that cannot pay")
	}
}

func TestDescribeImage_AProviderFailureBillsNothing(t *testing.T) {
	provider := &readerProvider{err: errors.New("upstream 500")}
	gw, mock, usage := newReaderGateway(t, provider, omni) // no accrue expected
	if _, err := gw.DescribeImage(context.Background(), openSession(), pngBytes, ""); err == nil {
		t.Fatal("expected an error")
	}
	if len(usage.records) != 0 {
		t.Errorf("usage was recorded for a failed call: %+v", usage.records)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestDescribeImage_AnEmptyAnswerIsAnError(t *testing.T) {
	provider := &readerProvider{text: "   ", usage: inference.Usage{InputTokens: 1, OutputTokens: 1}}
	gw, mock, _ := newReaderGateway(t, provider, omni)
	sess := openSession()
	expectAccrue(mock, sess, "teepin/omni", 1.0/1e6*2.0+1.0/1e6*8.0, 1, 1)
	if _, err := gw.DescribeImage(context.Background(), sess, pngBytes, ""); err == nil {
		t.Error("an empty description was accepted")
	}
}

// The reader's call is not the builder's context: it must not move the
// "builder memory" gauge, which reads the size of the builder's own requests.
func TestDescribeImage_DoesNotCountAsTheBuildersContext(t *testing.T) {
	provider := &readerProvider{text: "ok", usage: inference.Usage{InputTokens: 9000, OutputTokens: 10}}
	gw, mock, _ := newReaderGateway(t, provider, omni)
	sess := openSession()
	expectAccrue(mock, sess, "teepin/omni", 9000.0/1e6*2.0+10.0/1e6*8.0, 9000, 10)
	if _, err := gw.DescribeImage(context.Background(), sess, pngBytes, ""); err != nil {
		t.Fatal(err)
	}
	if tokens, _, ok := gw.ContextUsage(context.Background(), sess); ok && tokens != 0 {
		t.Errorf("builder context reading = %d after an image read", tokens)
	}
}

type routePrices map[string][2]float64

func (p routePrices) ModelPricing(_ context.Context, route string) (float64, float64, bool) {
	v, ok := p[route]
	return v[0], v[1], ok
}

// The description is priced at the READER's rates, not the builder's: the two
// models cost different amounts, and the customer pays for the one that did the work.
func TestDescribeImage_IsPricedAtTheReadersRates(t *testing.T) {
	provider := &readerProvider{text: "ok", usage: inference.Usage{InputTokens: 1000, OutputTokens: 100}}
	gw, mock, _ := newReaderGateway(t, provider, omni)
	gw.WithModelPricing(routePrices{"teepin/omni": {0.10, 0.30}, "teepin/glm": {1.00, 4.00}})
	sess := openSession()                                 // bound to teepin/glm
	in, out, inRate, outRate := 1000.0, 100.0, 0.10, 0.30 // variables: the gateway computes at run time, not as exact constants
	readerCost := in/1e6*inRate + out/1e6*outRate
	expectAccrue(mock, sess, "teepin/omni", readerCost, 1000, 100)

	if _, err := gw.DescribeImage(context.Background(), sess, pngBytes, ""); err != nil {
		t.Fatalf("DescribeImage: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("the build was charged at the wrong rate: %v", err)
	}
}

// Over real HTTP through the real OpenAI-compatible provider: the image has to
// arrive at the model server as an image_url part carrying the same bytes, and the
// description has to come back out of a real chat-completion body. The fakes above
// skip both halves.
func TestDescribeImage_OverTheRealOpenAICompatibleProvider(t *testing.T) {
	var gotParts []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Messages []struct {
				Role    string           `json:"role"`
				Content []map[string]any `json:"content"`
			} `json:"messages"`
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Messages) == 1 {
			gotParts = body.Messages[0].Content
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","model":"served","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"A pricing table with three columns."}}],"usage":{"prompt_tokens":900,"completion_tokens":40,"total_tokens":940}}`))
	}))
	defer srv.Close()

	provider := inference.NewVLLM(inference.VLLMConfig{BaseURL: srv.URL, Model: "served"})
	store, mock := newMockStore(t)
	backend := readerBackend{
		StaticModels: StaticModels{{Route: "teepin/omni", Engine: "vllm", Provider: provider}},
		readers:      []Model{omni},
	}
	gw := NewGateway(store, backend, nil, &fakePricing{in: 2, out: 8}, &fakeUsageRecorder{})
	sess := openSession()
	expectAccrue(mock, sess, "teepin/omni", 900.0/1e6*2.0+40.0/1e6*8.0, 900, 40)

	got, err := gw.DescribeImage(context.Background(), sess, pngBytes, "a pricing page")
	if err != nil {
		t.Fatalf("DescribeImage: %v", err)
	}
	if got != "A pricing table with three columns." {
		t.Errorf("description = %q", got)
	}
	if len(gotParts) != 2 || gotParts[0]["type"] != "text" || gotParts[1]["type"] != "image_url" {
		t.Fatalf("the model server received parts %v, want a text part then an image_url part", gotParts)
	}
	url, _ := gotParts[1]["image_url"].(map[string]any)["url"].(string)
	enc, ok := strings.CutPrefix(url, "data:image/png;base64,")
	if !ok {
		t.Fatalf("the image url is %.60q", url)
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || !bytes.Equal(raw, pngBytes) {
		t.Errorf("the image bytes changed on the way to the model: %v", err)
	}
}
