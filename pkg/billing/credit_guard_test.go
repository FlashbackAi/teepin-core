// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func newGuardService(t *testing.T) (*Service, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewService(db), mock
}

func expectBalance(mock sqlmock.Sqlmock, acct uuid.UUID, v float64) {
	mock.ExpectQuery(`billing\.credit_balance`).WithArgs(acct).
		WillReturnRows(sqlmock.NewRows([]string{"b"}).AddRow(v))
}

func TestCanAfford(t *testing.T) {
	cases := []struct {
		name      string
		balance   float64
		worstCase float64
		want      bool
	}{
		{"covers it", 5.00, 1.00, true},
		{"exactly covers it", 1.00, 1.00, true},
		{"short by a cent", 0.99, 1.00, false},
		{"zero balance, tiny request", 0, 0.0001, false},
		{"negative balance", -1, 0.0001, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, mock := newGuardService(t)
			acct := uuid.New()
			expectBalance(mock, acct, c.balance)
			ok, bal, err := svc.CanAfford(context.Background(), acct, c.worstCase)
			if err != nil || ok != c.want || bal != c.balance {
				t.Errorf("CanAfford = %v, %v, %v; want %v, %v, nil", ok, bal, err, c.want, c.balance)
			}
		})
	}
}

func TestCanAfford_UnpricedRequestReadsNothing(t *testing.T) {
	svc, mock := newGuardService(t)
	// No query is expected; sqlmock fails the call if one is made.
	ok, _, err := svc.CanAfford(context.Background(), uuid.New(), 0)
	if err != nil || !ok {
		t.Fatalf("free request refused: %v %v", ok, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestCanAfford_FailsClosedWhenBalanceUnreadable(t *testing.T) {
	svc, mock := newGuardService(t)
	acct := uuid.New()
	mock.ExpectQuery(`billing\.credit_balance`).WithArgs(acct).WillReturnError(errors.New("db down"))
	ok, _, err := svc.CanAfford(context.Background(), acct, 1.00)
	if ok || err == nil {
		t.Fatalf("a spend must not be approved when the balance cannot be read: ok=%v err=%v", ok, err)
	}
}

func TestCreditAvailable_CachesWithinTTL(t *testing.T) {
	svc, mock := newGuardService(t)
	acct := uuid.New()
	clock := time.Now()
	svc.balances.now = func() time.Time { return clock }
	expectBalance(mock, acct, 10) // one read only

	for i := 0; i < 3; i++ {
		if v, err := svc.CreditAvailable(context.Background(), acct); err != nil || v != 10 {
			t.Fatalf("read %d = %v, %v", i, v, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}

	// After the TTL the next read goes to the database again.
	clock = clock.Add(balanceCacheTTL + time.Second)
	expectBalance(mock, acct, 7)
	if v, _ := svc.CreditAvailable(context.Background(), acct); v != 7 {
		t.Errorf("stale value served past the TTL: %v", v)
	}
}

func TestBalanceCache_AdjustAndForget(t *testing.T) {
	var c balanceCache
	acct := uuid.New()

	c.adjust(acct, -5) // nothing cached: must not invent an entry
	if _, ok := c.get(acct); ok {
		t.Fatal("adjust created a cache entry from nothing")
	}

	c.put(acct, 10)
	c.adjust(acct, -3.5)
	if v, _ := c.get(acct); v != 6.5 {
		t.Errorf("after spend cached = %v, want 6.5", v)
	}

	c.forget(acct)
	if _, ok := c.get(acct); ok {
		t.Error("entry survived forget")
	}
}

func TestHasRunway(t *testing.T) {
	svc, mock := newGuardService(t)
	acct := uuid.New()
	// $8/hour for 15 minutes = $2.00.
	expectBalance(mock, acct, 1.99)
	ok, _, err := svc.HasRunway(context.Background(), acct, 8.00)
	if err != nil || ok {
		t.Errorf("$1.99 must not cover 15 minutes at $8/hour: ok=%v err=%v", ok, err)
	}
}
