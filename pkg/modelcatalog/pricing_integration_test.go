// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build migrationdrill

// Runs the "priced before available" rule against a real Postgres, because the
// rule lives in the UPDATE statements themselves. Run with:
//
//	TEEPIN_DRILL_DSN='postgres://postgres:test@localhost:55433/teepin?sslmode=disable' \
//	  go test -tags migrationdrill -run PricingIntegration ./pkg/modelcatalog/ -v
package modelcatalog

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	_ "github.com/lib/pq"

	"github.com/FlashbackAi/teepin-core/migrations"
)

func pricingDB(t *testing.T) *Service {
	t.Helper()
	dsn := os.Getenv("TEEPIN_DRILL_DSN")
	if dsn == "" {
		t.Skip("set TEEPIN_DRILL_DSN to run the pricing integration test")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	drv, err := migratepg.WithInstance(db, &migratepg.Config{})
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", drv)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate up: %v", err)
	}
	return NewService(db)
}

func newRoute() string { return "it/" + uuid.NewString()[:8] }

func register(t *testing.T, s *Service, route string, enabled bool, in, out *float64) error {
	t.Helper()
	by := "it"
	return s.RegisterModelWithPricing(context.Background(), Model{
		ModelRoute: route, DisplayName: "M", CostClass: CostClassOwn, Engine: "vllm", Enabled: enabled, UpdatedBy: &by,
	}, in, out)
}

func f(v float64) *float64 { return &v }

func mustGet(t *testing.T, s *Service, route string) *Model {
	t.Helper()
	m, err := s.GetModel(context.Background(), route)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The whole journey: a model starts unpriced and off, cannot be switched on by any
// route until priced, and once live cannot have its price cleared.
func TestPricingIntegration_NoModelGoesLiveWithoutAPrice(t *testing.T) {
	s := pricingDB(t)
	ctx := context.Background()
	on, off := true, false
	route := newRoute()

	if err := register(t, s, route, false, nil, nil); err != nil {
		t.Fatalf("registering a model switched off should always work: %v", err)
	}
	if mustGet(t, s, route).Priced() {
		t.Fatal("a new model should start unpriced")
	}

	// Every way of making it available is refused while unpriced.
	if err := s.SetEnabled(ctx, route, true, "it"); !errors.Is(err, ErrPricingRequired) {
		t.Errorf("enable unpriced: %v, want ErrPricingRequired", err)
	}
	if err := s.SetAvailability(ctx, route, Availability{OfferedToCustomers: &on}, "it"); !errors.Is(err, ErrPricingRequired) {
		t.Errorf("offer unpriced: %v", err)
	}
	if err := s.SetAvailability(ctx, route, Availability{BuildEnabled: &on}, "it"); !errors.Is(err, ErrPricingRequired) {
		t.Errorf("add to Teepin Build unpriced: %v", err)
	}
	if err := register(t, s, route, true, nil, nil); !errors.Is(err, ErrPricingRequired) {
		t.Errorf("register enabled unpriced: %v", err)
	}
	// Half a price is not a price.
	if err := s.SetPricing(ctx, route, 1, 0, "it"); err != nil {
		t.Fatalf("pricing a switched-off model, even partly, is allowed: %v", err)
	}
	if err := s.SetEnabled(ctx, route, true, "it"); !errors.Is(err, ErrPricingRequired) {
		t.Errorf("enable with only an input price: %v", err)
	}
	// (A new row is "offered to customers" by default; it is not live until enabled.)
	if m := mustGet(t, s, route); m.Enabled || m.BuildEnabled {
		t.Errorf("a refused change still took effect: %+v", m)
	}

	// Switching things OFF never needs a price.
	if err := s.SetAvailability(ctx, route, Availability{OfferedToCustomers: &off, BuildEnabled: &off}, "it"); err != nil {
		t.Errorf("switching off needs no price: %v", err)
	}

	// Priced: everything works.
	if err := s.SetPricing(ctx, route, 1, 4, "it"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnabled(ctx, route, true, "it"); err != nil {
		t.Fatalf("enable priced: %v", err)
	}
	if err := s.SetAvailability(ctx, route, Availability{OfferedToCustomers: &on, BuildEnabled: &on}, "it"); err != nil {
		t.Fatalf("offer priced: %v", err)
	}

	// Live: the price cannot be cleared, but can be changed.
	if err := s.SetPricing(ctx, route, 0, 0, "it"); !errors.Is(err, ErrPricingRequired) {
		t.Errorf("clearing a live model's price: %v", err)
	}
	if err := s.SetPricing(ctx, route, 2, 0, "it"); !errors.Is(err, ErrPricingRequired) {
		t.Errorf("half-clearing a live model's price: %v", err)
	}
	if err := s.SetPricing(ctx, route, 2, 8, "it"); err != nil {
		t.Errorf("repricing a live model: %v", err)
	}
	if m := mustGet(t, s, route); m.InputPricePerMillion != 2 || m.OutputPricePerMillion != 8 {
		t.Errorf("price = %v / %v", m.InputPricePerMillion, m.OutputPricePerMillion)
	}

	// Once taken out of service, the price may be cleared.
	if err := s.SetEnabled(ctx, route, false, "it"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAvailability(ctx, route, Availability{OfferedToCustomers: &off, BuildEnabled: &off}, "it"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPricing(ctx, route, 0, 0, "it"); err != nil {
		t.Errorf("clearing the price of a model that is out of service: %v", err)
	}
}

// Registering with a price registers AND enables in one call; editing a priced
// model's capabilities later neither needs nor disturbs its price.
func TestPricingIntegration_RegisterWithPriceAndLaterEditsKeepIt(t *testing.T) {
	s := pricingDB(t)
	route := newRoute()
	if err := register(t, s, route, true, f(1.5), f(6)); err != nil {
		t.Fatalf("register enabled with a price: %v", err)
	}
	if m := mustGet(t, s, route); !m.Enabled || m.InputPricePerMillion != 1.5 || m.OutputPricePerMillion != 6 {
		t.Fatalf("model = %+v", m)
	}
	// An edit that supplies no price keeps the stored one, even while enabled.
	if err := register(t, s, route, true, nil, nil); err != nil {
		t.Fatalf("editing a priced, enabled model: %v", err)
	}
	if m := mustGet(t, s, route); m.InputPricePerMillion != 1.5 || m.OutputPricePerMillion != 6 {
		t.Errorf("the edit disturbed the price: %+v", m)
	}
	// A price of zero supplied on an edit of a live model is refused.
	if err := register(t, s, route, true, f(0), f(0)); !errors.Is(err, ErrPricingRequired) {
		t.Errorf("zero price on enabled edit: %v", err)
	}
	// Unknown route.
	if err := s.SetEnabled(context.Background(), "it/missing", true, "it"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown model: %v, want ErrNotFound (not pricing_required)", err)
	}
}

// The image-reader role is its own switch, follows the same "priced before
// available" rule, and is listed only while the model is enabled.
func TestPricingIntegration_ImageReaderIsItsOwnPricedSwitch(t *testing.T) {
	s := pricingDB(t)
	ctx := context.Background()
	on, off := true, false
	route := newRoute()

	if err := register(t, s, route, false, nil, nil); err != nil {
		t.Fatal(err)
	}
	if mustGet(t, s, route).BuildImageReader {
		t.Fatal("a new model must not be an image reader")
	}
	// Unpriced: refused, and nothing changed.
	if err := s.SetAvailability(ctx, route, Availability{BuildImageReader: &on}, "it"); !errors.Is(err, ErrPricingRequired) {
		t.Fatalf("image reader on, unpriced: %v, want ErrPricingRequired", err)
	}
	if mustGet(t, s, route).BuildImageReader {
		t.Fatal("a refused change still took effect")
	}

	if err := s.SetPricing(ctx, route, 0.1, 0.3, "it"); err != nil {
		t.Fatal(err)
	}
	// Priced but not enabled: the flag can be set, yet the model is not listed as a reader.
	if err := s.SetAvailability(ctx, route, Availability{BuildImageReader: &on}, "it"); err != nil {
		t.Fatalf("image reader on, priced: %v", err)
	}
	if got := readerRoutes(t, s); contains(got, route) {
		t.Fatalf("a disabled model was listed as a reader: %v", got)
	}
	if err := s.SetEnabled(ctx, route, true, "it"); err != nil {
		t.Fatal(err)
	}
	m := mustGet(t, s, route)
	if !m.BuildImageReader || m.BuildEnabled {
		t.Fatalf("reader=%v builder=%v: the roles must be independent", m.BuildImageReader, m.BuildEnabled)
	}
	if got := readerRoutes(t, s); !contains(got, route) {
		t.Fatalf("an enabled reader is not listed: %v", got)
	}
	if got := builderRoutes(t, s); contains(got, route) {
		t.Fatalf("a reader that is not a builder was listed as a builder: %v", got)
	}

	// Switching it off needs no price and delists it; the builder flag is untouched.
	if err := s.SetAvailability(ctx, route, Availability{BuildImageReader: &off}, "it"); err != nil {
		t.Fatal(err)
	}
	if got := readerRoutes(t, s); contains(got, route) {
		t.Fatalf("an ex-reader is still listed: %v", got)
	}
}

func readerRoutes(t *testing.T, s *Service) []string {
	t.Helper()
	ms, err := s.ListImageReaders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return routesOf(ms)
}

func builderRoutes(t *testing.T, s *Service) []string {
	t.Helper()
	ms, err := s.ListBuildModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return routesOf(ms)
}

func routesOf(ms []Model) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ModelRoute)
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
