// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
)

func TestCapEventPayload(t *testing.T) {
	small := json.RawMessage(`{"type":"action","summary":"hi"}`)
	if string(capEventPayload(small)) != string(small) {
		t.Fatal("a small payload must pass through unchanged")
	}

	big, _ := json.Marshal(map[string]string{
		"type":      "observation",
		"summary":   strings.Repeat("a", MaxStoredEventBytes+10),
		"diff":      "x",
		"reasoning": "y",
	})
	out := capEventPayload(big)
	if out == nil || len(out) > MaxStoredEventBytes {
		t.Fatalf("oversized payload not capped: %d bytes", len(out))
	}
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("capped payload is not valid JSON: %v", err)
	}
	if got["type"] != "observation" || !strings.HasSuffix(got["summary"], "[trimmed]") {
		t.Errorf("type/summary not kept as expected: %v", got["type"])
	}
	if _, ok := got["diff"]; ok {
		t.Error("diff should be dropped from an oversized payload")
	}
}

func TestCapEventPayload_NeverSplitsARune(t *testing.T) {
	// A summary of multi-byte runes, cut mid-rune, would make invalid UTF-8.
	big, _ := json.Marshal(map[string]string{"type": "x", "summary": strings.Repeat("€", MaxStoredEventBytes)})
	out := capEventPayload(big)
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if strings.ContainsRune(got["summary"], '�') {
		t.Error("summary was cut inside a rune")
	}
}

func TestLogLineWriter_NumbersEveryLineAndReportsAccepted(t *testing.T) {
	var gotNos []int
	w := &logLineWriter{onLine: func(n int, _ json.RawMessage) { gotNos = append(gotNos, n) }}
	// Line 2 is not JSON: it is counted but not reported.
	_, _ = w.Write([]byte("{\"type\":\"action\"}\nnoise\n{\"type\":\"idle\"}\n{\"type\":\"act"))
	_, _ = w.Write([]byte("ion\"}\n"))
	want := []int{1, 3, 4}
	if len(gotNos) != len(want) {
		t.Fatalf("lines reported = %v, want %v", gotNos, want)
	}
	for i := range want {
		if gotNos[i] != want[i] {
			t.Fatalf("lines reported = %v, want %v", gotNos, want)
		}
	}
}

// fakeSink records what the recorder stores.
type fakeSink struct {
	mu   sync.Mutex
	have int
	rows map[int]string
}

func (f *fakeSink) RecordEvent(_ context.Context, _ uuid.UUID, _ int, lineNo int, payload json.RawMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows == nil {
		f.rows = map[int]string{}
	}
	f.rows[lineNo] = string(payload)
	return nil
}

func (f *fakeSink) MaxRecordedLine(context.Context, uuid.UUID, int) (int, error) {
	return f.have, nil
}

func (f *fakeSink) snapshot() map[int]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int]string{}
	for k, v := range f.rows {
		out[k] = v
	}
	return out
}

func TestRecorder_StoresEventsWithoutABrowser(t *testing.T) {
	log := "{\"type\":\"action\",\"summary\":\"one\"}\n" +
		"{\"type\":\"thinking\",\"seconds\":10}\n" +
		"garbage\n" +
		"{\"type\":\"observation\",\"summary\":\"two\"}\n"
	fc := &streamRetryCluster{
		calls:        []scriptedStreamCall{{write: []byte(log)}},
		instanceStat: &cluster.InstanceStatus{Status: "running"},
	}
	sink := &fakeSink{}
	rec := NewRecorder(newTestEventsHandler(fc), sink)
	sess := uuid.New()
	rec.Ensure(sess, uuid.New(), "kumbha-agent-x", 1)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sink.snapshot()) == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := sink.snapshot()
	if len(got) != 2 {
		t.Fatalf("stored %v, want lines 1 and 4 only (a heartbeat and a garbage line are not history)", got)
	}
	if !strings.Contains(got[1], "one") || !strings.Contains(got[4], "two") {
		t.Errorf("wrong lines stored: %v", got)
	}
}

func TestRecorder_SkipsLinesAlreadyStored(t *testing.T) {
	log := "{\"type\":\"action\",\"summary\":\"one\"}\n{\"type\":\"action\",\"summary\":\"two\"}\n{\"type\":\"action\",\"summary\":\"three\"}\n"
	fc := &streamRetryCluster{
		calls:        []scriptedStreamCall{{write: []byte(log)}},
		instanceStat: &cluster.InstanceStatus{Status: "running"},
	}
	sink := &fakeSink{have: 2} // a restart: lines 1 and 2 are already in the database
	rec := NewRecorder(newTestEventsHandler(fc), sink)
	rec.Ensure(uuid.New(), uuid.New(), "kumbha-agent-x", 1)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(sink.snapshot()) < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	got := sink.snapshot()
	if len(got) != 1 || !strings.Contains(got[3], "three") {
		t.Fatalf("stored %v, want only line 3", got)
	}
}

// fakeHistory serves stored events by launch.
type fakeHistory struct {
	seq    int
	stored map[int][]json.RawMessage // launch -> events
}

func (f *fakeHistory) AgentLaunchSeq(context.Context, uuid.UUID) (int, error) { return f.seq, nil }

func (f *fakeHistory) ListEvents(_ context.Context, _ uuid.UUID, minSeq, maxSeq int) ([]json.RawMessage, error) {
	var out []json.RawMessage
	for seq := minSeq; seq <= maxSeq; seq++ {
		out = append(out, f.stored[seq]...)
	}
	return out, nil
}

// dialRelay serves h.ServeSession over a test server and returns a connected,
// authenticated client.
func dialRelay(t *testing.T, h *EventsHandler, ticket EventTicket) *websocket.Conn {
	t.Helper()
	id, secret, err := h.tickets.Issue(ticket)
	if err != nil {
		t.Fatalf("issue ticket: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeSession(w, r, ticket.SessionID.String())
	}))
	t.Cleanup(srv.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.WriteJSON(wsAuthFrame{Type: "auth", ID: id, Secret: secret}); err != nil {
		t.Fatalf("auth: %v", err)
	}
	return conn
}

// readFrames collects frames until the server closes, returning each as text.
func readFrames(t *testing.T, conn *websocket.Conn) []string {
	t.Helper()
	var out []string
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return out
		}
		out = append(out, string(msg))
	}
}

func summaryOf(t *testing.T, frame string) string {
	t.Helper()
	var f struct {
		Summary string `json:"summary"`
		Type    string `json:"type"`
	}
	if err := json.Unmarshal([]byte(frame), &f); err != nil {
		t.Fatalf("bad frame %q: %v", frame, err)
	}
	if f.Summary != "" {
		return f.Summary
	}
	return "<" + f.Type + ">"
}

func TestServeSession_ReplaysEarlierLaunchesBeforeTheLiveTail(t *testing.T) {
	fc := &streamRetryCluster{
		calls:        []scriptedStreamCall{{write: []byte("{\"type\":\"action\",\"summary\":\"live\"}\n")}},
		instanceStat: &cluster.InstanceStatus{Status: "running"},
	}
	h := newTestEventsHandler(fc).WithHistory(&fakeHistory{
		seq: 2,
		stored: map[int][]json.RawMessage{
			1: {json.RawMessage(`{"type":"action","summary":"old-1"}`), json.RawMessage(`{"type":"action","summary":"old-2"}`)},
			2: {json.RawMessage(`{"type":"action","summary":"stored-current"}`)}, // the live tail provides this launch
		},
	}, nil)
	sess := uuid.New()
	conn := dialRelay(t, h, EventTicket{SessionID: sess, ProjectID: uuid.New(), AgentInstanceID: "kumbha-agent-x"})

	var got []string
	for _, f := range readFrames(t, conn) {
		got = append(got, summaryOf(t, f))
	}
	want := []string{"old-1", "old-2", "live", "<closed>"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("frames = %v, want %v (earlier launches from the store, then the live pod, and the current launch's stored copy NOT repeated)", got, want)
	}
}

func TestServeSession_FallsBackToStoredEventsWhenThePodIsGone(t *testing.T) {
	fc := &streamRetryCluster{
		// Every attempt fails with no bytes: the pod's log is unreadable.
		calls:        []scriptedStreamCall{{err: errString("pod not found")}},
		instanceStat: &cluster.InstanceStatus{Status: "terminated"},
	}
	h := newTestEventsHandler(fc).WithHistory(&fakeHistory{
		seq: 1,
		stored: map[int][]json.RawMessage{
			1: {json.RawMessage(`{"type":"action","summary":"kept-1"}`), json.RawMessage(`{"type":"idle","summary":"kept-2"}`)},
		},
	}, nil)
	conn := dialRelay(t, h, EventTicket{SessionID: uuid.New(), ProjectID: uuid.New(), AgentInstanceID: "kumbha-agent-x"})

	var got []string
	for _, f := range readFrames(t, conn) {
		got = append(got, summaryOf(t, f))
	}
	want := []string{"kept-1", "kept-2", "<closed>"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("frames = %v, want %v", got, want)
	}
}

func TestServeSession_WithoutHistoryStillReportsAGoneAgent(t *testing.T) {
	fc := &streamRetryCluster{
		calls:        []scriptedStreamCall{{err: errString("pod not found")}},
		instanceStat: &cluster.InstanceStatus{Status: "terminated"},
	}
	h := newTestEventsHandler(fc) // no history: behaviour is unchanged
	conn := dialRelay(t, h, EventTicket{SessionID: uuid.New(), ProjectID: uuid.New(), AgentInstanceID: "kumbha-agent-x"})
	frames := readFrames(t, conn)
	if len(frames) == 0 || !strings.Contains(frames[len(frames)-1], "stream_ended") {
		t.Fatalf("frames = %v, want a stream_ended error as before", frames)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
