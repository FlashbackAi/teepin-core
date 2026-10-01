// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package kumbha

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/cluster"
)

// EventSink is where the recorder keeps event lines (Store implements it).
type EventSink interface {
	RecordEvent(ctx context.Context, sessionID uuid.UUID, launchSeq, lineNo int, payload json.RawMessage) error
	MaxRecordedLine(ctx context.Context, sessionID uuid.UUID, launchSeq int) (int, error)
}

// EventHistory is what the event relay replays from when a browser connects.
type EventHistory interface {
	AgentLaunchSeq(ctx context.Context, sessionID uuid.UUID) (int, error)
	ListEvents(ctx context.Context, sessionID uuid.UUID, minSeq, maxSeq int) ([]json.RawMessage, error)
}

// recorderMaxLife bounds one tail. An agent pod idles out long before this.
const recorderMaxLife = 12 * time.Hour

// Recorder keeps a session's activity feed on the control plane by tailing the
// agent pod's log itself, independent of any browser. Without it, events were
// stored only while someone was watching, and a build left running in a closed
// tab lost its history when the pod went.
//
// Like the other in-memory registries here it assumes one control-plane
// process; with several, each would tail and the duplicate inserts would be
// absorbed by the table's unique key.
type Recorder struct {
	tail *EventsHandler
	sink EventSink

	mu     sync.Mutex
	active map[uuid.UUID]int // session -> launch being tailed
}

func NewRecorder(tail *EventsHandler, sink EventSink) *Recorder {
	return &Recorder{tail: tail, sink: sink, active: make(map[uuid.UUID]int)}
}

// Ensure starts recording the session's current agent launch, if it is not
// already being recorded. Safe to call repeatedly (every launch, every
// browser connection).
func (r *Recorder) Ensure(sessionID, projectID uuid.UUID, instanceID string, launchSeq int) {
	if r == nil || instanceID == "" {
		return
	}
	r.mu.Lock()
	if cur, ok := r.active[sessionID]; ok && cur == launchSeq {
		r.mu.Unlock()
		return
	}
	r.active[sessionID] = launchSeq
	r.mu.Unlock()

	go func() {
		defer func() {
			r.mu.Lock()
			if r.active[sessionID] == launchSeq {
				delete(r.active, sessionID)
			}
			r.mu.Unlock()
		}()
		r.run(sessionID, projectID, instanceID, launchSeq)
	}()
}

func (r *Recorder) run(sessionID, projectID uuid.UUID, instanceID string, launchSeq int) {
	ctx, cancel := context.WithTimeout(context.Background(), recorderMaxLife)
	defer cancel()

	have, err := r.sink.MaxRecordedLine(ctx, sessionID, launchSeq)
	if err != nil {
		log.Printf("WARN: kumbha event recorder: %v", err)
		have = 0
	}
	failures := 0
	onLine := func(lineNo int, payload json.RawMessage) {
		if lineNo <= have {
			return // already stored; the pod's log was replayed
		}
		var head struct {
			Type string `json:"type"`
		}
		// "thinking" is a transient heartbeat, not history.
		if json.Unmarshal(payload, &head) == nil && head.Type == "thinking" {
			return
		}
		wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
		defer wcancel()
		if err := r.sink.RecordEvent(wctx, sessionID, launchSeq, lineNo, payload); err != nil {
			// Logged a few times only: a database outage would otherwise write
			// one line per event.
			if failures < 3 {
				log.Printf("WARN: kumbha event recorder (session %s): %v", sessionID, err)
			}
			failures++
		}
	}
	_, _ = r.tail.tailLogs(ctx, cluster.ProjectScope(projectID.String()), instanceID, nil, onLine)
}

// WithAgentLaunchHook registers fn to be called after every successful agent
// launch with the session's new launch number. The server uses it to start the
// event recorder for the new pod.
func (g *Gateway) WithAgentLaunchHook(fn func(sess *Session, launchSeq int)) *Gateway {
	g.agentLaunched = fn
	return g
}

// noteAgentLaunched counts the launch and tells the hook. Best effort: the pod
// is already running, so a bookkeeping failure must not fail the launch (the
// feed would then simply not be kept for this launch).
func (g *Gateway) noteAgentLaunched(ctx context.Context, sess *Session) {
	seq, err := g.store.BumpLaunchSeq(ctx, sess.ID)
	if err != nil {
		log.Printf("WARN: could not count agent launch for Kumbha session %s: %v", sess.ID, err)
		return
	}
	if g.agentLaunched != nil {
		g.agentLaunched(sess, seq)
	}
}
