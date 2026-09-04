// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package nodes

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func newMock(t *testing.T) (*Service, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	return NewService(db), mock, func() { db.Close() }
}

// A minted token stores only a hash, and the class is whatever the operator
// chose — proving class is fixed server-side at mint time.
func TestCreateEnrollmentToken_StoresHashAndClass(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectQuery(`INSERT INTO compute\.node_enrollment_tokens`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "datacenter", "rack-1", "op", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(uuid.New(), time.Now()))

	plaintext, tok, err := s.CreateEnrollmentToken(context.Background(), "rack-1", "datacenter", "op", time.Hour)
	if err != nil {
		t.Fatalf("CreateEnrollmentToken: %v", err)
	}
	if tok.Class != "datacenter" {
		t.Errorf("class = %q, want datacenter", tok.Class)
	}
	if plaintext[:4] != enrollTokenPrefix {
		t.Errorf("token %q lacks enroll prefix", plaintext)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestCreateEnrollmentToken_Validation(t *testing.T) {
	s := NewService(nil) // rejected before any query
	if _, _, err := s.CreateEnrollmentToken(context.Background(), "  ", "home", "op", time.Hour); err == nil {
		t.Error("blank label accepted")
	}
	if _, _, err := s.CreateEnrollmentToken(context.Background(), "x", "bogus", "op", time.Hour); err == nil {
		t.Error("invalid class accepted")
	}
}

// Enroll takes the class from the TOKEN row, not from the agent — the core
// class-integrity guarantee. Here the token says 'datacenter' and the node
// is created with that class regardless of anything in specs.
func TestEnroll_ClassComesFromToken(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	// We must present a token whose hash matches a stored row. Mint one
	// out-of-band by generating a secret and feeding back its hash.
	secret, hash, prefix, _ := generateSecret(enrollTokenPrefix)
	tokenID := uuid.New()
	nodeID := uuid.New()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id, token_hash, class, expires_at, consumed_at\s+FROM compute\.node_enrollment_tokens\s+WHERE token_prefix = \$1\s+FOR UPDATE`).
		WithArgs(prefix).
		WillReturnRows(sqlmock.NewRows([]string{"id", "token_hash", "class", "expires_at", "consumed_at"}).
			AddRow(tokenID, hash, "datacenter", time.Now().Add(time.Hour), nil))
	mock.ExpectQuery(`INSERT INTO compute\.nodes`).
		WithArgs("node-a", "prov-a", "datacenter", sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), 0, false, sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "node_name", "created_at", "updated_at"}).
			AddRow(nodeID, "node-a", time.Now(), time.Now()))
	mock.ExpectExec(`UPDATE compute\.node_enrollment_tokens\s+SET consumed_at = NOW\(\), node_id`).
		WithArgs(nodeID, tokenID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	cred, node, err := s.Enroll(context.Background(), secret, NodeSpecs{NodeName: "node-a", ProviderID: "prov-a"})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if node.Class != "datacenter" {
		t.Errorf("node class = %q, want datacenter (from token)", node.Class)
	}
	if node.NodeName != "node-a" {
		t.Errorf("node name = %q, want node-a (from the RETURNING clause, not blindly echoing the input)", node.NodeName)
	}
	if cred[:4] != credentialPrefix {
		t.Errorf("credential %q lacks cred prefix", cred)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// A token already consumed is rejected — single-use.
func TestEnroll_ConsumedTokenRejected(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	secret, hash, prefix, _ := generateSecret(enrollTokenPrefix)
	consumed := time.Now().Add(-time.Minute)

	mock.ExpectBegin()
	mock.ExpectQuery(`FROM compute\.node_enrollment_tokens\s+WHERE token_prefix`).
		WithArgs(prefix).
		WillReturnRows(sqlmock.NewRows([]string{"id", "token_hash", "class", "expires_at", "consumed_at"}).
			AddRow(uuid.New(), hash, "home", time.Now().Add(time.Hour), consumed))
	mock.ExpectRollback()

	if _, _, err := s.Enroll(context.Background(), secret, NodeSpecs{NodeName: "n", ProviderID: "p"}); !errors.Is(err, ErrTokenConsumed) {
		t.Fatalf("err = %v, want ErrTokenConsumed", err)
	}
}

// An expired token is rejected even if never consumed.
func TestEnroll_ExpiredTokenRejected(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	secret, hash, prefix, _ := generateSecret(enrollTokenPrefix)

	mock.ExpectBegin()
	mock.ExpectQuery(`FROM compute\.node_enrollment_tokens\s+WHERE token_prefix`).
		WithArgs(prefix).
		WillReturnRows(sqlmock.NewRows([]string{"id", "token_hash", "class", "expires_at", "consumed_at"}).
			AddRow(uuid.New(), hash, "home", time.Now().Add(-time.Minute), nil))
	mock.ExpectRollback()

	if _, _, err := s.Enroll(context.Background(), secret, NodeSpecs{NodeName: "n", ProviderID: "p"}); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("err = %v, want ErrTokenExpired", err)
	}
}

// A token whose plaintext does not hash-match any stored row is rejected —
// no timing/format leak, and no accidental match on prefix collision.
func TestEnroll_WrongSecretRejected(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	// Present one secret, store a DIFFERENT secret's hash under same prefix.
	present, _, prefix, _ := generateSecret(enrollTokenPrefix)
	_, otherHash, _, _ := generateSecret(enrollTokenPrefix)

	mock.ExpectBegin()
	mock.ExpectQuery(`FROM compute\.node_enrollment_tokens\s+WHERE token_prefix`).
		WithArgs(prefix).
		WillReturnRows(sqlmock.NewRows([]string{"id", "token_hash", "class", "expires_at", "consumed_at"}).
			AddRow(uuid.New(), otherHash, "home", time.Now().Add(time.Hour), nil))
	mock.ExpectRollback()

	if _, _, err := s.Enroll(context.Background(), present, NodeSpecs{NodeName: "n", ProviderID: "p"}); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
}

// A malformed credential is rejected before any DB call.
func TestAuthenticateNode_MalformedRejected(t *testing.T) {
	s := NewService(nil)
	if _, err := s.AuthenticateNode(context.Background(), "not-a-cred"); !errors.Is(err, ErrNodeInvalid) {
		t.Fatalf("err = %v, want ErrNodeInvalid", err)
	}
}

// A valid credential resolves to its node.
func TestAuthenticateNode_Valid(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	cred, hash, prefix, _ := generateSecret(credentialPrefix)
	id := uuid.New()

	mock.ExpectQuery(`FROM compute\.nodes\s+WHERE credential_prefix`).
		WithArgs(prefix).
		WillReturnRows(nodeAuthRow(id, hash, "online", nil))

	node, err := s.AuthenticateNode(context.Background(), cred)
	if err != nil {
		t.Fatalf("AuthenticateNode: %v", err)
	}
	if node.ID != id {
		t.Errorf("resolved wrong node")
	}
}

// A revoked credential is rejected even though the hash matches.
func TestAuthenticateNode_RevokedRejected(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	cred, hash, prefix, _ := generateSecret(credentialPrefix)
	revoked := time.Now()

	mock.ExpectQuery(`FROM compute\.nodes\s+WHERE credential_prefix`).
		WithArgs(prefix).
		WillReturnRows(nodeAuthRow(uuid.New(), hash, "online", &revoked))

	if _, err := s.AuthenticateNode(context.Background(), cred); !errors.Is(err, ErrNodeRevoked) {
		t.Fatalf("err = %v, want ErrNodeRevoked", err)
	}
}

// A disabled node is rejected.
func TestAuthenticateNode_DisabledRejected(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	cred, hash, prefix, _ := generateSecret(credentialPrefix)

	mock.ExpectQuery(`FROM compute\.nodes\s+WHERE credential_prefix`).
		WithArgs(prefix).
		WillReturnRows(nodeAuthRow(uuid.New(), hash, "disabled", nil))

	if _, err := s.AuthenticateNode(context.Background(), cred); !errors.Is(err, ErrNodeDisabled) {
		t.Fatalf("err = %v, want ErrNodeDisabled", err)
	}
}

// UpsertSeen issues a single INSERT ... ON CONFLICT — the write-through that
// gives every connected node a durable row without disturbing an enrolled
// node's class or credential.
func TestUpsertSeen(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	nodeID := uuid.New()
	mock.ExpectQuery(`(?s)INSERT INTO compute\.nodes.*ON CONFLICT \(provider_id\) DO UPDATE`).
		WithArgs("gpu-node-1", "dc-provider", "datacenter", sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), 8, true,
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), true).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(nodeID))
	mock.ExpectExec(`INSERT INTO compute\.node_metrics`).
		WithArgs(nodeID, 0.0, 0.0, 0, 0.0, 0.0, 0.0, 0.0).
		WillReturnResult(sqlmock.NewResult(1, 1))

	err := s.UpsertSeen(context.Background(), "datacenter", NodeSpecs{
		NodeName: "gpu-node-1", ProviderID: "dc-provider", GPUCount: 8, MIGCapable: true,
		K8sReady: true,
	})
	if err != nil {
		t.Fatalf("UpsertSeen: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// TestUpsertSeen_KeyedByProviderIDNotNodeName is the regression test for a
// real duplicate-row bug found live 2026-09-03: RenameNode only ever
// changes node_name, but the agent's own heartbeat re-identifies itself by
// its ORIGINAL name (it has no way to learn about a console-side rename).
// Keying this upsert's ON CONFLICT on node_name meant the very next
// heartbeat after a rename found no row under the agent's stale name and
// inserted a duplicate instead of updating the renamed row. provider_id is
// the actual stable identity (set once at enroll, never touched by a
// rename) — this asserts the conflict target AND that node_name is absent
// from the UPDATE SET clause, so even a genuinely conflicting heartbeat can
// never silently undo an operator's rename.
func TestUpsertSeen_KeyedByProviderIDNotNodeName(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	// Proves the conflict target: sqlmock fails this test if UpsertSeen's
	// actual query no longer matches ON CONFLICT (provider_id).
	nodeID := uuid.New()
	mock.ExpectQuery(`(?s)INSERT INTO compute\.nodes.*ON CONFLICT \(provider_id\) DO UPDATE`).
		WithArgs("stale-agent-reported-name", "stable-provider-id", "home", sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), 0, false,
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), true).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(nodeID))
	mock.ExpectExec(`INSERT INTO compute\.node_metrics`).
		WithArgs(nodeID, 0.0, 0.0, 0, 0.0, 0.0, 0.0, 0.0).
		WillReturnResult(sqlmock.NewResult(1, 1))

	err := s.UpsertSeen(context.Background(), "home", NodeSpecs{
		NodeName: "stale-agent-reported-name", ProviderID: "stable-provider-id", K8sReady: true,
	})
	if err != nil {
		t.Fatalf("UpsertSeen: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// TestUpsertSeen_RecordsUtilizationHistory proves the actual utilization
// VALUES get written to compute.node_metrics, not just that some insert
// happens — the whole point of this table (stats/graphs/status page/
// marketing globe — ROADMAP.md's 2026-09-03 entry) depends on the
// numbers being the ones the agent actually reported, not the CPU/memory
// CAPACITY fields it is easy to confuse them with. Includes GPU VRAM
// usage (added 2026-09-04 after an audit found it was being collected
// live by the allocator but never threaded into this table at all) and
// network/storage throughput (added the same day once the customer asked
// whether those were covered too).
func TestUpsertSeen_RecordsUtilizationHistory(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	nodeID := uuid.New()
	mock.ExpectQuery(`(?s)INSERT INTO compute\.nodes.*ON CONFLICT \(provider_id\) DO UPDATE`).
		WithArgs("srialla", "srialla", "home", sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), 0, false,
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), true).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(nodeID))
	mock.ExpectExec(`INSERT INTO compute\.node_metrics`).
		WithArgs(nodeID, 42.5, 12.75, 20, 5.5, 1.25, 3.0, 0.75).
		WillReturnResult(sqlmock.NewResult(1, 1))

	err := s.UpsertSeen(context.Background(), "home", NodeSpecs{
		NodeName: "srialla", ProviderID: "srialla", K8sReady: true,
		CPUUsedPercent: 42.5, MemoryUsedGB: 12.75, GPUUsedVRAMGB: 20,
		NetworkRxMbps: 5.5, NetworkTxMbps: 1.25, StorageReadMbps: 3.0, StorageWriteMbps: 0.75,
	})
	if err != nil {
		t.Fatalf("UpsertSeen: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// TestUpsertSeen_MetricsHistoryFailureIsNonFatal proves the best-effort
// property: a failure recording utilization history must never turn an
// otherwise-successful heartbeat into an error. That matters beyond just
// "the caller sees a spurious error" — MarkStaleOffline flips a node
// offline on a stale last_seen_at, so if a metrics-write hiccup made
// UpsertSeen return an error, a genuinely-alive node's heartbeat would
// stop counting as "seen" purely because a secondary observability write
// failed.
func TestUpsertSeen_MetricsHistoryFailureIsNonFatal(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	nodeID := uuid.New()
	mock.ExpectQuery(`(?s)INSERT INTO compute\.nodes.*ON CONFLICT \(provider_id\) DO UPDATE`).
		WithArgs("srialla", "srialla", "home", sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), 0, false,
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), true).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(nodeID))
	mock.ExpectExec(`INSERT INTO compute\.node_metrics`).
		WithArgs(nodeID, 0.0, 0.0, 0, 0.0, 0.0, 0.0, 0.0).
		WillReturnError(errors.New("connection reset"))

	err := s.UpsertSeen(context.Background(), "home", NodeSpecs{
		NodeName: "srialla", ProviderID: "srialla", K8sReady: true,
	})
	if err != nil {
		t.Fatalf("UpsertSeen must succeed even when metrics history recording fails, got: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// TestUpsertSeenQuery_NeverReassignsNodeName is a direct, static check on
// the query text itself (not a mock round trip): a rename is only safe
// from being undone by the next heartbeat if node_name never appears on
// the left side of an assignment in UpsertSeen's UPDATE SET clause. Reads
// the source file directly rather than trying to express "this substring
// is absent" as a regex passed through sqlmock (Go's RE2-based regexp has
// no lookahead, so that isn't expressible as one ExpectExec pattern).
func TestUpsertSeenQuery_NeverReassignsNodeName(t *testing.T) {
	src, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("read service.go: %v", err)
	}
	fnStart := bytes.Index(src, []byte("func (s *Service) UpsertSeen"))
	if fnStart == -1 {
		t.Fatal("UpsertSeen not found in service.go")
	}
	fnEnd := bytes.Index(src[fnStart:], []byte("\nfunc "))
	if fnEnd == -1 {
		fnEnd = len(src) - fnStart
	}
	body := string(src[fnStart : fnStart+fnEnd])

	if regexp.MustCompile(`node_name\s*=\s*EXCLUDED`).MatchString(body) {
		t.Error("UpsertSeen's SET clause must never reassign node_name — that would silently undo a rename on the next heartbeat")
	}
	if !strings.Contains(body, "ON CONFLICT (provider_id)") {
		t.Error("UpsertSeen must key its ON CONFLICT on provider_id, not node_name")
	}
}

func TestUpsertSeen_RequiresProviderID(t *testing.T) {
	s, _, done := newMock(t)
	defer done()

	err := s.UpsertSeen(context.Background(), "home", NodeSpecs{NodeName: "n"})
	if err == nil {
		t.Error("expected an error when provider_id is empty")
	}
}

func TestMarkStaleOffline(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectExec(`UPDATE compute\.nodes\s+SET status = 'offline'`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 2))

	n, err := s.MarkStaleOffline(context.Background(), 90*time.Second)
	if err != nil {
		t.Fatalf("MarkStaleOffline: %v", err)
	}
	if n != 2 {
		t.Errorf("transitioned %d, want 2", n)
	}
}

func TestListMetrics_ReturnsOldestFirst(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	nodeID := uuid.New()
	t1 := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 9, 4, 10, 0, 15, 0, time.UTC)
	mock.ExpectQuery(`SELECT recorded_at, cpu_used_percent, memory_used_gb, gpu_vram_used_gb,\s+network_rx_mbps, network_tx_mbps, storage_read_mbps, storage_write_mbps\s+FROM compute\.node_metrics`).
		WithArgs(nodeID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"recorded_at", "cpu_used_percent", "memory_used_gb", "gpu_vram_used_gb",
			"network_rx_mbps", "network_tx_mbps", "storage_read_mbps", "storage_write_mbps"}).
			AddRow(t1, 12.5, 4.0, 0, 1.0, 0.5, 2.0, 0.25).
			AddRow(t2, 15.0, 4.2, 20, 1.5, 0.75, 2.5, 0.5))

	samples, err := s.ListMetrics(context.Background(), nodeID, time.Hour)
	if err != nil {
		t.Fatalf("ListMetrics: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("got %d samples, want 2", len(samples))
	}
	if !samples[0].RecordedAt.Equal(t1) || samples[0].CPUUsedPercent != 12.5 {
		t.Errorf("first sample = %+v, want t1/12.5", samples[0])
	}
	if !samples[1].RecordedAt.Equal(t2) || samples[1].MemoryUsedGB != 4.2 || samples[1].GPUUsedVRAMGB != 20 {
		t.Errorf("second sample = %+v, want t2/4.2/20", samples[1])
	}
}

// nearCutoffArg implements sqlmock.Argument to assert a time.Time
// argument is within a tolerance of an expected cutoff — used instead of
// sqlmock.AnyArg() where the actual clamped value is what the test needs
// to prove, not merely that some value was passed.
type nearCutoffArg struct{ want time.Time }

func (a nearCutoffArg) Match(v driver.Value) bool {
	got, ok := v.(time.Time)
	if !ok {
		return false
	}
	d := got.Sub(a.want)
	if d < 0 {
		d = -d
	}
	return d < 5*time.Second
}

// TestListMetrics_SinceClamping locks in the actual clamped cutoff value
// sent to the database — WithArgs asserts the real timestamp (within a
// tolerance for test execution time), not just that A query ran. Two
// DIFFERENT thresholds apply depending on the case (see
// DefaultMetricsWindow's own doc comment for why): an out-of-range
// "since" (an implausibly large window) clamps DOWN to MaxMetricsWindow,
// while a non-positive one (0/negative — what an omitted or unparsed
// query param becomes) uses the smaller DefaultMetricsWindow instead,
// not the max.
func TestListMetrics_SinceClamping(t *testing.T) {
	cases := []struct {
		name       string
		since      time.Duration
		wantCutoff time.Duration
	}{
		{"way past MaxMetricsWindow clamps down to it", 365 * 24 * time.Hour, MaxMetricsWindow},
		{"zero uses the smaller default, not the max", 0, DefaultMetricsWindow},
		{"negative uses the smaller default, not the max", -time.Hour, DefaultMetricsWindow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, mock, done := newMock(t)
			defer done()

			nodeID := uuid.New()
			mock.ExpectQuery(`SELECT recorded_at, cpu_used_percent, memory_used_gb, gpu_vram_used_gb,\s+network_rx_mbps, network_tx_mbps, storage_read_mbps, storage_write_mbps\s+FROM compute\.node_metrics`).
				WithArgs(nodeID, nearCutoffArg{want: time.Now().Add(-tc.wantCutoff)}).
				WillReturnRows(sqlmock.NewRows([]string{"recorded_at", "cpu_used_percent", "memory_used_gb", "gpu_vram_used_gb",
					"network_rx_mbps", "network_tx_mbps", "storage_read_mbps", "storage_write_mbps"}))

			if _, err := s.ListMetrics(context.Background(), nodeID, tc.since); err != nil {
				t.Fatalf("ListMetrics: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unmet (cutoff was not clamped to the expected window): %v", err)
			}
		})
	}
}

func TestNodeExists(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	nodeID := uuid.New()
	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM compute\.nodes WHERE id = \$1\)`).
		WithArgs(nodeID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	exists, err := s.NodeExists(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("NodeExists: %v", err)
	}
	if !exists {
		t.Error("got false, want true")
	}
}

func TestNodeExists_NotFound(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	nodeID := uuid.New()
	mock.ExpectQuery(`SELECT EXISTS \(SELECT 1 FROM compute\.nodes WHERE id = \$1\)`).
		WithArgs(nodeID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	exists, err := s.NodeExists(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("NodeExists: %v", err)
	}
	if exists {
		t.Error("got true, want false")
	}
}

func TestPurgeOldMetrics(t *testing.T) {
	s, mock, done := newMock(t)
	defer done()

	mock.ExpectExec(`DELETE FROM compute\.node_metrics WHERE recorded_at < \$1`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 7))

	n, err := s.PurgeOldMetrics(context.Background(), MetricsRetentionWindow)
	if err != nil {
		t.Fatalf("PurgeOldMetrics: %v", err)
	}
	if n != 7 {
		t.Errorf("got %d purged, want 7", n)
	}
}

// nodeAuthRow builds the row shape AuthenticateNode scans.
func nodeAuthRow(id uuid.UUID, hash, status string, revoked *time.Time) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "node_name", "provider_id", "class", "region",
		"cpu_cores", "memory_gb", "gpu_model", "gpu_count", "mig_capable",
		"os", "arch", "agent_version", "status", "credential_hash", "revoked_at",
	}).AddRow(id, "node-x", "prov-x", "home", "", 4, 8, "", 0, false,
		"linux", "amd64", "0.1.0", status, hash, revoked)
}
