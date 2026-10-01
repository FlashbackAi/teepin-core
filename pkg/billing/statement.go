// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
)

// A statement is one calendar month (UTC) of an account's credit: what it
// started with, what came in, what was used, and what it ended with. It is
// built from the append-only credit ledger, so it reconciles by construction:
//
//	closing = opening + credit added + credit granted - used - expired - revoked
//
// "Used" is what was actually drawn from credit (the consumption rows), which
// is what a customer paid for. It can be less than the metered value of the
// usage when the balance ran out first.

// ErrBadStatementMonth means the month is not a valid, non-future YYYY-MM.
var ErrBadStatementMonth = errors.New("month must be YYYY-MM, not in the future")

// StatementResource is one line of usage within a service.
type StatementResource struct {
	ResourceType string
	Title        string
	Quantity     float64
	Unit         string
	Amount       float64
}

// StatementService groups a project's usage by service ("GPU compute",
// "Teepin Build", "Object storage", ...).
type StatementService struct {
	Service   string
	Amount    float64
	Resources []StatementResource
}

// StatementProject groups usage by project. ProjectID is nil for usage with no
// project (or one since deleted); Name then says so.
type StatementProject struct {
	ProjectID *uuid.UUID
	Name      string
	Amount    float64
	Services  []StatementService
}

// Statement is one month of an account's credit.
type Statement struct {
	Month     string
	From, To  time.Time // From inclusive, To exclusive (UTC)
	Opening   float64
	Purchased float64
	Granted   float64
	Used      float64
	Expired   float64
	Revoked   float64
	Closing   float64
	Projects  []StatementProject
}

// ParseStatementMonth turns "2026-09" into its UTC bounds. A month later than
// the current one is refused; so is one before the platform existed.
func ParseStatementMonth(month string, now time.Time) (from, to time.Time, err error) {
	t, perr := time.Parse("2006-01", month)
	if perr != nil {
		return from, to, ErrBadStatementMonth
	}
	from = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	if from.After(monthStartUTC(now)) || from.Year() < 2020 {
		return from, to, ErrBadStatementMonth
	}
	return from, from.AddDate(0, 1, 0), nil
}

// StatementMonths lists the months an account has ledger activity in, newest
// first, always including the current month.
func (s *Service) StatementMonths(ctx context.Context, accountID uuid.UUID) ([]string, error) {
	var first sql.NullTime
	if err := s.db.QueryRowContext(ctx,
		`SELECT MIN(created_at) FROM billing.credit_transactions WHERE account_id = $1`, accountID).Scan(&first); err != nil {
		return nil, fmt.Errorf("failed to read statement months: %w", err)
	}
	now := monthStartUTC(time.Now())
	start := now
	if first.Valid {
		start = monthStartUTC(first.Time)
	}
	var out []string
	for m := now; !m.Before(start); m = m.AddDate(0, -1, 0) {
		out = append(out, m.Format("2006-01"))
	}
	return out, nil
}

// Statement builds the account's statement for a month.
func (s *Service) Statement(ctx context.Context, accountID uuid.UUID, month string) (*Statement, error) {
	from, to, err := ParseStatementMonth(month, time.Now())
	if err != nil {
		return nil, err
	}
	st := &Statement{Month: month, From: from, To: to}

	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount), 0) FROM billing.credit_transactions
		WHERE account_id = $1 AND created_at < $2`, accountID, from).Scan(&st.Opening); err != nil {
		return nil, fmt.Errorf("failed to read the opening balance: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT kind, COALESCE(SUM(amount), 0) FROM billing.credit_transactions
		WHERE account_id = $1 AND created_at >= $2 AND created_at < $3
		GROUP BY kind`, accountID, from, to)
	if err != nil {
		return nil, fmt.Errorf("failed to read the month's movements: %w", err)
	}
	for rows.Next() {
		var kind string
		var sum float64
		if err := rows.Scan(&kind, &sum); err != nil {
			rows.Close()
			return nil, err
		}
		switch kind {
		case "purchase":
			st.Purchased = sum
		case "grant":
			st.Granted = sum
		case "consumption":
			st.Used = -sum
		case "expiry":
			st.Expired = -sum
		case "revocation":
			st.Revoked = -sum
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	st.Closing = st.Opening + st.Purchased + st.Granted - st.Used - st.Expired - st.Revoked

	st.Projects, err = s.statementUsage(ctx, accountID, from, to)
	if err != nil {
		return nil, err
	}
	return st, nil
}

// statementUsage groups the month's consumption by project, then service, then
// resource. A usage record that was drawn across several credit lots has one
// consumption row per lot, so amounts are summed per record first and the
// record's quantity is counted once.
func (s *Service) statementUsage(ctx context.Context, accountID uuid.UUID, from, to time.Time) ([]StatementProject, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ur.project_id, p.name, ur.resource_type, ur.unit,
		       SUM(u.used) AS used, SUM(ur.quantity) AS quantity
		FROM (
			SELECT usage_record_id, -SUM(amount) AS used
			FROM billing.credit_transactions
			WHERE account_id = $1 AND kind = 'consumption' AND usage_record_id IS NOT NULL
			  AND created_at >= $2 AND created_at < $3
			GROUP BY usage_record_id
		) u
		JOIN billing.usage_records ur ON ur.id = u.usage_record_id
		LEFT JOIN auth.projects p ON p.id = ur.project_id
		GROUP BY ur.project_id, p.name, ur.resource_type, ur.unit`, accountID, from, to)
	if err != nil {
		return nil, fmt.Errorf("failed to read the month's usage: %w", err)
	}
	defer rows.Close()

	type key struct {
		project uuid.UUID
		has     bool
	}
	projects := map[key]*StatementProject{}
	for rows.Next() {
		var pid uuid.NullUUID
		var pname, rtype, unit sql.NullString
		var used, qty float64
		if err := rows.Scan(&pid, &pname, &rtype, &unit, &used, &qty); err != nil {
			return nil, err
		}
		k := key{project: pid.UUID, has: pid.Valid}
		p := projects[k]
		if p == nil {
			p = &StatementProject{Name: "No project"}
			if pid.Valid {
				id := pid.UUID
				p.ProjectID = &id
				p.Name = pname.String
				if p.Name == "" {
					p.Name = "Deleted project"
				}
			}
			projects[k] = p
		}
		pres := Classify(rtype.String)
		var svc *StatementService
		for i := range p.Services {
			if p.Services[i].Service == pres.Service {
				svc = &p.Services[i]
			}
		}
		if svc == nil {
			p.Services = append(p.Services, StatementService{Service: pres.Service})
			svc = &p.Services[len(p.Services)-1]
		}
		svc.Resources = append(svc.Resources, StatementResource{
			ResourceType: rtype.String, Title: pres.Title, Quantity: qty, Unit: unit.String, Amount: used,
		})
		svc.Amount += used
		p.Amount += used
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]StatementProject, 0, len(projects))
	for _, p := range projects {
		sort.Slice(p.Services, func(i, j int) bool { return p.Services[i].Amount > p.Services[j].Amount })
		for i := range p.Services {
			r := p.Services[i].Resources
			sort.Slice(r, func(a, b int) bool { return r[a].Amount > r[b].Amount })
		}
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Amount > out[j].Amount })
	return out, nil
}

// StatementLine is one ledger row of a month, for the CSV export.
type StatementLine struct {
	When        time.Time
	Kind        string
	Description string
	Project     string
	Service     string
	Resource    string
	Quantity    float64
	Unit        string
	// Amount is signed: credit added is positive, credit used negative.
	Amount float64
	// Balance is the running balance after this line.
	Balance float64
}

// EachStatementLine streams every ledger row of the month, oldest first, with
// a running balance that starts at the month's opening balance, so the export
// reconciles to the statement page. Streamed rather than collected: a busy
// account has a row per metered interval per instance.
func (s *Service) EachStatementLine(ctx context.Context, accountID uuid.UUID, month string, fn func(StatementLine) error) error {
	from, to, err := ParseStatementMonth(month, time.Now())
	if err != nil {
		return err
	}
	var balance float64
	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount), 0) FROM billing.credit_transactions
		WHERE account_id = $1 AND created_at < $2`, accountID, from).Scan(&balance); err != nil {
		return fmt.Errorf("failed to read the opening balance: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT ct.created_at, ct.kind, COALESCE(ct.reason, ''), ct.amount,
		       p.name, ur.resource_type, ur.quantity, ur.unit
		FROM billing.credit_transactions ct
		LEFT JOIN billing.usage_records ur ON ur.id = ct.usage_record_id
		LEFT JOIN auth.projects p ON p.id = ur.project_id
		WHERE ct.account_id = $1 AND ct.created_at >= $2 AND ct.created_at < $3
		ORDER BY ct.created_at, ct.id`, accountID, from, to)
	if err != nil {
		return fmt.Errorf("failed to read the month's ledger: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var l StatementLine
		var pname, rtype, unit sql.NullString
		var qty sql.NullFloat64
		if err := rows.Scan(&l.When, &l.Kind, &l.Description, &l.Amount, &pname, &rtype, &qty, &unit); err != nil {
			return err
		}
		balance += l.Amount
		l.Balance = balance
		l.Project = pname.String
		if rtype.Valid {
			pres := Classify(rtype.String)
			l.Service, l.Resource = pres.Service, pres.Title
			l.Quantity, l.Unit = qty.Float64, unit.String
		}
		if err := fn(l); err != nil {
			return err
		}
	}
	return rows.Err()
}
