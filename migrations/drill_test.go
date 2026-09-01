// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Throwaway archivability drill for migration 016. Run explicitly against a
// disposable Postgres; it is behind a build tag so it never runs in CI or the
// normal `go test ./...`.
//
//	TEEPIN_DRILL_DSN='postgres://postgres:test@localhost:55433/teepin?sslmode=disable' \
//	  go test -tags migrationdrill -run TestArchivabilityDrill ./migrations/ -v
package migrations

import (
	"database/sql"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/lib/pq"
)

func migrator(t *testing.T, db *sql.DB) *migrate.Migrate {
	t.Helper()
	src, err := iofs.New(FS, ".")
	if err != nil {
		t.Fatalf("load embedded migrations: %v", err)
	}
	drv, err := migratepg.WithInstance(db, &migratepg.Config{})
	if err != nil {
		t.Fatalf("init driver: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", drv)
	if err != nil {
		t.Fatalf("init migrator: %v", err)
	}
	return m
}

func tableExists(t *testing.T, db *sql.DB, schema, table string) bool {
	t.Helper()
	var exists bool
	err := db.QueryRow(`
		SELECT EXISTS (SELECT 1 FROM information_schema.tables
		               WHERE table_schema = $1 AND table_name = $2)
	`, schema, table).Scan(&exists)
	if err != nil {
		t.Fatalf("check table %s.%s: %v", schema, table, err)
	}
	return exists
}

func columnExists(t *testing.T, db *sql.DB, schema, table, col string) bool {
	t.Helper()
	var exists bool
	err := db.QueryRow(`
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_schema = $1 AND table_name = $2 AND column_name = $3)
	`, schema, table, col).Scan(&exists)
	if err != nil {
		t.Fatalf("check column %s.%s.%s: %v", schema, table, col, err)
	}
	return exists
}

func columnNullable(t *testing.T, db *sql.DB, schema, table, col string) bool {
	t.Helper()
	var nullable string
	err := db.QueryRow(`
		SELECT is_nullable FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2 AND column_name = $3
	`, schema, table, col).Scan(&nullable)
	if err != nil {
		t.Fatalf("check nullability %s.%s.%s: %v", schema, table, col, err)
	}
	return nullable == "YES"
}

// TestArchivabilityDrill proves the home-compute pilot removes cleanly:
// full up → nodes objects exist → down one → they are gone AND the rest of
// the schema is intact → up again → they are back. This is the "flag off +
// revert 016 leaves the platform as before" guarantee, exercised for real.
func TestArchivabilityDrill(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// 1. Full up.
	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("initial up: %v", err)
	}
	if !tableExists(t, db, "compute", "nodes") {
		t.Fatal("after up: compute.nodes missing")
	}
	if !tableExists(t, db, "compute", "node_enrollment_tokens") {
		t.Fatal("after up: compute.node_enrollment_tokens missing")
	}
	if !columnExists(t, db, "compute", "instances", "node_id") {
		t.Fatal("after up: compute.instances.node_id missing")
	}

	// 2. Revert down to version 15 (the state AFTER 015, BEFORE 016). Using
	//    an absolute target rather than relative Steps keeps this robust as
	//    later migrations (017, 018, ...) are added on top.
	if err := migrator(t, db).Migrate(15); err != nil {
		t.Fatalf("migrate down to 015: %v", err)
	}
	if tableExists(t, db, "compute", "nodes") {
		t.Error("after down: compute.nodes still present")
	}
	if tableExists(t, db, "compute", "node_enrollment_tokens") {
		t.Error("after down: compute.node_enrollment_tokens still present")
	}
	if columnExists(t, db, "compute", "instances", "node_id") {
		t.Error("after down: compute.instances.node_id still present")
	}
	// The rest of the platform must be intact — spot-check core tables the
	// pilot must never have disturbed.
	for _, tbl := range [][2]string{
		{"compute", "instances"}, {"billing", "invoices"},
		{"billing", "payment_methods"}, {"auth", "accounts"},
	} {
		if !tableExists(t, db, tbl[0], tbl[1]) {
			t.Errorf("after down: %s.%s was wrongly removed", tbl[0], tbl[1])
		}
	}

	// 3. Up again — restorable.
	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	if !tableExists(t, db, "compute", "nodes") {
		t.Fatal("after re-up: compute.nodes missing")
	}
	t.Log("archivability drill passed: 016 applies, reverts cleanly, and re-applies")
}

// TestArchivabilityDrill017 proves Stage 2 (migration 017) is equally
// reversible: its columns exist after up, are gone after reverting one step,
// and the instances table and pricing row survive.
func TestArchivabilityDrill017(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("initial up: %v", err)
	}
	if !columnExists(t, db, "compute", "instances", "provider_id") {
		t.Fatal("after up: compute.instances.provider_id missing")
	}
	if !columnExists(t, db, "billing", "pricing", "cpu_price_per_core_hour") {
		t.Fatal("after up: billing.pricing.cpu_price_per_core_hour missing")
	}

	// Revert to version 16 (state after 016, before 017), absolute so later
	// migrations do not shift it.
	if err := migrator(t, db).Migrate(16); err != nil {
		t.Fatalf("migrate down to 016: %v", err)
	}
	if columnExists(t, db, "compute", "instances", "provider_id") {
		t.Error("after down: compute.instances.provider_id still present")
	}
	if columnExists(t, db, "billing", "pricing", "cpu_price_per_core_hour") {
		t.Error("after down: cpu_price_per_core_hour still present")
	}
	// The instances table and the pricing row must be intact — 017 must not
	// have removed anything it did not add.
	if !tableExists(t, db, "compute", "instances") {
		t.Error("after down: compute.instances was wrongly removed")
	}
	if !columnExists(t, db, "billing", "pricing", "vram_price_per_gb_hour") {
		t.Error("after down: the VRAM rate (predates 017) was wrongly removed")
	}

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	t.Log("archivability drill passed: 017 applies, reverts cleanly, and re-applies")
}

// TestArchivabilityDrill018 proves Stage 2.5 (migration 018) is reversible:
// the rentable columns exist after up, are gone after reverting one step, and
// the node's detected specs survive.
func TestArchivabilityDrill018(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("initial up: %v", err)
	}
	if !columnExists(t, db, "compute", "nodes", "rentable_cpu_cores") {
		t.Fatal("after up: compute.nodes.rentable_cpu_cores missing")
	}

	// Revert to version 17 (state after 017, before 018), absolute.
	if err := migrator(t, db).Migrate(17); err != nil {
		t.Fatalf("migrate down to 017: %v", err)
	}
	if columnExists(t, db, "compute", "nodes", "rentable_cpu_cores") {
		t.Error("after down: rentable_cpu_cores still present")
	}
	if columnExists(t, db, "compute", "nodes", "rentable_memory_gb") {
		t.Error("after down: rentable_memory_gb still present")
	}
	// Detected specs (migration 016) must survive.
	if !columnExists(t, db, "compute", "nodes", "cpu_cores") {
		t.Error("after down: detected cpu_cores was wrongly removed")
	}

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	t.Log("archivability drill passed: 018 applies, reverts cleanly, and re-applies")
}

// TestArchivabilityDrill019 proves the health-check-depth migration (019) is
// reversible: k8s_ready exists after up, is gone after reverting one step,
// and the rentable columns (018) survive.
func TestArchivabilityDrill019(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("initial up: %v", err)
	}
	if !columnExists(t, db, "compute", "nodes", "k8s_ready") {
		t.Fatal("after up: compute.nodes.k8s_ready missing")
	}

	// Revert to version 18 (state after 018, before 019), absolute.
	if err := migrator(t, db).Migrate(18); err != nil {
		t.Fatalf("migrate down to 018: %v", err)
	}
	if columnExists(t, db, "compute", "nodes", "k8s_ready") {
		t.Error("after down: k8s_ready still present")
	}
	// Rentable columns (018) must survive.
	if !columnExists(t, db, "compute", "nodes", "rentable_cpu_cores") {
		t.Error("after down: rentable_cpu_cores was wrongly removed")
	}

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	t.Log("archivability drill passed: 019 applies, reverts cleanly, and re-applies")
}

// TestArchivabilityDrill020 proves the Stage 3 endpoint-details migration
// (020) is reversible: the four new instance columns exist after up, are
// gone after reverting one step, and the pre-existing endpoint column (002)
// survives.
func TestArchivabilityDrill020(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("initial up: %v", err)
	}
	for _, col := range []string{"dns_name", "public_ip", "tls_enabled", "tls_ready"} {
		if !columnExists(t, db, "compute", "instances", col) {
			t.Fatalf("after up: compute.instances.%s missing", col)
		}
	}

	// Revert to version 19 (state after 019, before 020), absolute.
	if err := migrator(t, db).Migrate(19); err != nil {
		t.Fatalf("migrate down to 019: %v", err)
	}
	for _, col := range []string{"dns_name", "public_ip", "tls_enabled", "tls_ready"} {
		if columnExists(t, db, "compute", "instances", col) {
			t.Errorf("after down: %s still present", col)
		}
	}
	// The pre-existing endpoint column (migration 002) must survive.
	if !columnExists(t, db, "compute", "instances", "endpoint") {
		t.Error("after down: endpoint (002) was wrongly removed")
	}

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	t.Log("archivability drill passed: 020 applies, reverts cleanly, and re-applies")
}

// TestArchivabilityDrill021 proves the container_port migration (021,
// added when the Stage 3 tunnel surfaced that the customer's container
// port was never persisted anywhere post-create) is reversible: the
// column exists after up, is gone after reverting one step, and every
// column from 020 survives that revert untouched.
func TestArchivabilityDrill021(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("initial up: %v", err)
	}
	if !columnExists(t, db, "compute", "instances", "container_port") {
		t.Fatal("after up: compute.instances.container_port missing")
	}

	// Revert to version 20 (state after 020, before 021), absolute.
	if err := migrator(t, db).Migrate(20); err != nil {
		t.Fatalf("migrate down to 020: %v", err)
	}
	if columnExists(t, db, "compute", "instances", "container_port") {
		t.Error("after down: container_port still present")
	}
	// The 020 columns must survive this revert untouched.
	for _, col := range []string{"dns_name", "public_ip", "tls_enabled", "tls_ready"} {
		if !columnExists(t, db, "compute", "instances", col) {
			t.Errorf("after down: %s (020) was wrongly removed", col)
		}
	}

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	t.Log("archivability drill passed: 021 applies, reverts cleanly, and re-applies")
}

// TestArchivabilityDrill022 proves the exec_sessions migration (022,
// interactive exec's audit trail) is reversible: the table exists after
// up, is gone after reverting one step, and every 021 column survives
// that revert untouched.
func TestArchivabilityDrill022(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("initial up: %v", err)
	}
	if !tableExists(t, db, "compute", "exec_sessions") {
		t.Fatal("after up: compute.exec_sessions missing")
	}

	// Revert to version 21 (state after 021, before 022), absolute.
	if err := migrator(t, db).Migrate(21); err != nil {
		t.Fatalf("migrate down to 021: %v", err)
	}
	if tableExists(t, db, "compute", "exec_sessions") {
		t.Error("after down: exec_sessions still present")
	}
	if !columnExists(t, db, "compute", "instances", "container_port") {
		t.Error("after down: container_port (021) was wrongly removed")
	}

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	t.Log("archivability drill passed: 022 applies, reverts cleanly, and re-applies")
}

// TestArchivabilityDrill023 proves the instance-storage migration (023,
// persistent volumes + storage billing) is reversible: both new columns
// exist after up, are gone after reverting one step, and 022's
// exec_sessions table survives that revert untouched.
func TestArchivabilityDrill023(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("initial up: %v", err)
	}
	if !columnExists(t, db, "compute", "instances", "storage_gb") {
		t.Fatal("after up: compute.instances.storage_gb missing")
	}
	if !columnExists(t, db, "billing", "pricing", "storage_price_per_gb_month") {
		t.Fatal("after up: billing.pricing.storage_price_per_gb_month missing")
	}

	// Revert to version 22 (state after 022, before 023), absolute.
	if err := migrator(t, db).Migrate(22); err != nil {
		t.Fatalf("migrate down to 022: %v", err)
	}
	if columnExists(t, db, "compute", "instances", "storage_gb") {
		t.Error("after down: storage_gb still present")
	}
	if columnExists(t, db, "billing", "pricing", "storage_price_per_gb_month") {
		t.Error("after down: storage_price_per_gb_month still present")
	}
	if !tableExists(t, db, "compute", "exec_sessions") {
		t.Error("after down: exec_sessions (022) was wrongly removed")
	}

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	t.Log("archivability drill passed: 023 applies, reverts cleanly, and re-applies")
}

// TestArchivabilityDrill024 proves the usage-ledger generalisation (024 —
// polymorphic subject_type/subject_id, cost_basis/provider, widened
// decimals, billing.inference_sessions) is reversible: the new table and
// columns exist after up, are gone after reverting one step, and 023's
// storage columns survive that revert untouched.
func TestArchivabilityDrill024(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("initial up: %v", err)
	}
	if !tableExists(t, db, "billing", "inference_sessions") {
		t.Fatal("after up: billing.inference_sessions missing")
	}
	if !columnExists(t, db, "billing", "usage_records", "subject_type") {
		t.Fatal("after up: billing.usage_records.subject_type missing")
	}
	if !columnExists(t, db, "billing", "pricing", "llm_price_per_million_input") {
		t.Fatal("after up: billing.pricing.llm_price_per_million_input missing")
	}

	// Revert to version 23 (state after 023, before 024), absolute.
	if err := migrator(t, db).Migrate(23); err != nil {
		t.Fatalf("migrate down to 023: %v", err)
	}
	if tableExists(t, db, "billing", "inference_sessions") {
		t.Error("after down: inference_sessions still present")
	}
	if columnExists(t, db, "billing", "usage_records", "subject_type") {
		t.Error("after down: subject_type still present")
	}
	if columnExists(t, db, "billing", "pricing", "llm_price_per_million_input") {
		t.Error("after down: llm_price_per_million_input still present")
	}
	if !columnExists(t, db, "compute", "instances", "storage_gb") {
		t.Error("after down: storage_gb (023) was wrongly removed")
	}

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	t.Log("archivability drill passed: 024 applies, reverts cleanly, and re-applies")
}

// TestArchivabilityDrill025 proves the Kumbha workspace-version table
// (025) is reversible: it and inference_sessions.current_workspace_version
// exist after up, are gone after reverting one step, and 024's
// inference_sessions — which the version table references with ON DELETE
// CASCADE — survives that revert untouched. The cascade direction is the
// specific thing worth pinning: dropping workspace versions must never
// take the sessions (and therefore the billing records keyed to them)
// with it.
func TestArchivabilityDrill025(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("initial up: %v", err)
	}
	if !tableExists(t, db, "billing", "kumbha_workspace_versions") {
		t.Fatal("after up: billing.kumbha_workspace_versions missing")
	}
	if !columnExists(t, db, "billing", "inference_sessions", "current_workspace_version") {
		t.Fatal("after up: inference_sessions.current_workspace_version missing")
	}

	// Revert to version 24 (state after 024, before 025), absolute.
	if err := migrator(t, db).Migrate(24); err != nil {
		t.Fatalf("migrate down to 024: %v", err)
	}
	if tableExists(t, db, "billing", "kumbha_workspace_versions") {
		t.Error("after down: kumbha_workspace_versions still present")
	}
	if columnExists(t, db, "billing", "inference_sessions", "current_workspace_version") {
		t.Error("after down: current_workspace_version still present")
	}
	if !tableExists(t, db, "billing", "inference_sessions") {
		t.Error("after down: inference_sessions (024) was wrongly removed — the version table's FK must not cascade upwards")
	}

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	t.Log("archivability drill passed: 025 applies, reverts cleanly, and re-applies")
}

// TestArchivabilityDrill026 proves inference_sessions.app_instance_id (026)
// is reversible — added after up, gone after reverting one step, and
// inference_sessions itself (untouched by this migration beyond the one
// column) survives the round trip.
func TestArchivabilityDrill026(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("initial up: %v", err)
	}
	if !columnExists(t, db, "billing", "inference_sessions", "app_instance_id") {
		t.Fatal("after up: inference_sessions.app_instance_id missing")
	}

	if err := migrator(t, db).Migrate(25); err != nil {
		t.Fatalf("migrate down to 025: %v", err)
	}
	if columnExists(t, db, "billing", "inference_sessions", "app_instance_id") {
		t.Error("after down: app_instance_id still present")
	}
	if !tableExists(t, db, "billing", "inference_sessions") {
		t.Error("after down: inference_sessions was wrongly removed")
	}

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	t.Log("archivability drill passed: 026 applies, reverts cleanly, and re-applies")
}

// TestArchivabilityDrill027 proves billing.kumbha_messages (027) is
// reversible, and that inference_sessions survives the round trip
// untouched — the cascade direction that matters is dropping messages
// must never take sessions (and their billing records) with it.
func TestArchivabilityDrill027(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("initial up: %v", err)
	}
	if !tableExists(t, db, "billing", "kumbha_messages") {
		t.Fatal("after up: billing.kumbha_messages missing")
	}

	if err := migrator(t, db).Migrate(26); err != nil {
		t.Fatalf("migrate down to 026: %v", err)
	}
	if tableExists(t, db, "billing", "kumbha_messages") {
		t.Error("after down: kumbha_messages still present")
	}
	if !tableExists(t, db, "billing", "inference_sessions") {
		t.Error("after down: inference_sessions was wrongly removed — the message table's FK must not cascade upwards")
	}

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	t.Log("archivability drill passed: 027 applies, reverts cleanly, and re-applies")
}

// TestArchivabilityDrill029 proves inference_sessions.screenshot/
// screenshot_captured_at (029) are reversible, and that inference_sessions
// itself survives the round trip.
func TestArchivabilityDrill029(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("initial up: %v", err)
	}
	if !columnExists(t, db, "billing", "inference_sessions", "screenshot") {
		t.Fatal("after up: inference_sessions.screenshot missing")
	}
	if !columnExists(t, db, "billing", "inference_sessions", "screenshot_captured_at") {
		t.Fatal("after up: inference_sessions.screenshot_captured_at missing")
	}

	if err := migrator(t, db).Migrate(28); err != nil {
		t.Fatalf("migrate down to 028: %v", err)
	}
	if columnExists(t, db, "billing", "inference_sessions", "screenshot") {
		t.Error("after down: screenshot still present")
	}
	if columnExists(t, db, "billing", "inference_sessions", "screenshot_captured_at") {
		t.Error("after down: screenshot_captured_at still present")
	}
	if !tableExists(t, db, "billing", "inference_sessions") {
		t.Error("after down: inference_sessions was wrongly removed")
	}

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	t.Log("archivability drill passed: 029 applies, reverts cleanly, and re-applies")
}

// TestArchivabilityDrill030 proves inference_sessions.last_deploy_failed/
// last_deploy_error/last_deploy_at (030) are reversible, and that
// inference_sessions itself survives the round trip.
func TestArchivabilityDrill030(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("initial up: %v", err)
	}
	for _, col := range []string{"last_deploy_failed", "last_deploy_error", "last_deploy_at"} {
		if !columnExists(t, db, "billing", "inference_sessions", col) {
			t.Fatalf("after up: inference_sessions.%s missing", col)
		}
	}

	if err := migrator(t, db).Migrate(29); err != nil {
		t.Fatalf("migrate down to 029: %v", err)
	}
	for _, col := range []string{"last_deploy_failed", "last_deploy_error", "last_deploy_at"} {
		if columnExists(t, db, "billing", "inference_sessions", col) {
			t.Errorf("after down: %s still present", col)
		}
	}
	if !tableExists(t, db, "billing", "inference_sessions") {
		t.Error("after down: inference_sessions was wrongly removed")
	}

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	t.Log("archivability drill passed: 030 applies, reverts cleanly, and re-applies")
}

// TestArchivabilityDrill031 proves compute.instances.user_id becoming
// nullable (031) is reversible, and that compute.instances itself survives
// the round trip.
func TestArchivabilityDrill031(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("initial up: %v", err)
	}
	if !columnNullable(t, db, "compute", "instances", "user_id") {
		t.Fatal("after up: compute.instances.user_id is still NOT NULL")
	}

	if err := migrator(t, db).Migrate(30); err != nil {
		t.Fatalf("migrate down to 030: %v", err)
	}
	if columnNullable(t, db, "compute", "instances", "user_id") {
		t.Error("after down: compute.instances.user_id is still nullable")
	}
	if !tableExists(t, db, "compute", "instances") {
		t.Error("after down: compute.instances was wrongly removed")
	}

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	t.Log("archivability drill passed: 031 applies, reverts cleanly, and re-applies")
}

// TestArchivabilityDrill032 proves compute.instances.kumbha_session_id
// (032) is reversible, and that compute.instances itself survives the
// round trip.
func TestArchivabilityDrill032(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("initial up: %v", err)
	}
	if !columnExists(t, db, "compute", "instances", "kumbha_session_id") {
		t.Fatal("after up: compute.instances.kumbha_session_id missing")
	}

	if err := migrator(t, db).Migrate(31); err != nil {
		t.Fatalf("migrate down to 031: %v", err)
	}
	if columnExists(t, db, "compute", "instances", "kumbha_session_id") {
		t.Error("after down: compute.instances.kumbha_session_id still present")
	}
	if !tableExists(t, db, "compute", "instances") {
		t.Error("after down: compute.instances was wrongly removed")
	}

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	t.Log("archivability drill passed: 032 applies, reverts cleanly, and re-applies")
}

// TestArchivabilityDrill033 proves
// inference_sessions.last_deployed_version (033) is reversible, and that
// inference_sessions itself survives the round trip.
func TestArchivabilityDrill033(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("initial up: %v", err)
	}
	if !columnExists(t, db, "billing", "inference_sessions", "last_deployed_version") {
		t.Fatal("after up: inference_sessions.last_deployed_version missing")
	}

	if err := migrator(t, db).Migrate(32); err != nil {
		t.Fatalf("migrate down to 032: %v", err)
	}
	if columnExists(t, db, "billing", "inference_sessions", "last_deployed_version") {
		t.Error("after down: inference_sessions.last_deployed_version still present")
	}
	if !tableExists(t, db, "billing", "inference_sessions") {
		t.Error("after down: inference_sessions was wrongly removed")
	}

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	t.Log("archivability drill passed: 033 applies, reverts cleanly, and re-applies")
}

// TestArchivabilityDrill034 proves inference_sessions.github_repo (034) is
// reversible, and that inference_sessions itself survives the round trip.
func TestArchivabilityDrill034(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("initial up: %v", err)
	}
	if !columnExists(t, db, "billing", "inference_sessions", "github_repo") {
		t.Fatal("after up: inference_sessions.github_repo missing")
	}

	if err := migrator(t, db).Migrate(33); err != nil {
		t.Fatalf("migrate down to 033: %v", err)
	}
	if columnExists(t, db, "billing", "inference_sessions", "github_repo") {
		t.Error("after down: inference_sessions.github_repo still present")
	}
	if !tableExists(t, db, "billing", "inference_sessions") {
		t.Error("after down: inference_sessions was wrongly removed")
	}

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	t.Log("archivability drill passed: 034 applies, reverts cleanly, and re-applies")
}

// TestArchivabilityDrill035 proves
// inference_sessions.deploy_lock_acquired_at (035) is reversible, and
// that inference_sessions itself survives the round trip.
func TestArchivabilityDrill035(t *testing.T) {
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the migration drill")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("initial up: %v", err)
	}
	if !columnExists(t, db, "billing", "inference_sessions", "deploy_lock_acquired_at") {
		t.Fatal("after up: inference_sessions.deploy_lock_acquired_at missing")
	}

	if err := migrator(t, db).Migrate(34); err != nil {
		t.Fatalf("migrate down to 034: %v", err)
	}
	if columnExists(t, db, "billing", "inference_sessions", "deploy_lock_acquired_at") {
		t.Error("after down: inference_sessions.deploy_lock_acquired_at still present")
	}
	if !tableExists(t, db, "billing", "inference_sessions") {
		t.Error("after down: inference_sessions was wrongly removed")
	}

	if err := migrator(t, db).Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-up: %v", err)
	}
	t.Log("archivability drill passed: 035 applies, reverts cleanly, and re-applies")
}
