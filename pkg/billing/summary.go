// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ServiceLine is one service's spend within a project.
type ServiceLine struct {
	Service   string  `json:"service"`  // "GPU compute", "Storage", ...
	Quantity  float64 `json:"quantity"` // hours, GB-months, ...
	Unit      string  `json:"unit"`     // "hours", "GB-month"
	Cost      float64 `json:"cost"`
	Instances int     `json:"instances"` // distinct resources billed
}

// ProjectLine is one project's spend, broken down by service.
type ProjectLine struct {
	ProjectID   uuid.UUID     `json:"project_id"`
	ProjectName string        `json:"project_name"`
	Cost        float64       `json:"cost"`
	Services    []ServiceLine `json:"services"`
}

// AccountSummary is the account-level bill for a period: one total,
// attributed down to project and service. This is what the console
// billing screen renders.
type AccountSummary struct {
	AccountID   uuid.UUID     `json:"account_id"`
	PeriodStart time.Time     `json:"period_start"`
	PeriodEnd   time.Time     `json:"period_end"`
	TotalCost   float64       `json:"total_cost"`
	Currency    string        `json:"currency"`
	Projects    []ProjectLine `json:"projects"`
}

// GetAccountSummary aggregates usage for an account over a period.
//
// Billing attaches to the ACCOUNT — one payment method, one invoice —
// while projects act as cost centres, which is how AWS and GCP behave
// and what finance teams expect.
func (s *Service) GetAccountSummary(ctx context.Context, accountID uuid.UUID, start, end time.Time) (*AccountSummary, error) {
	// Grouped by resource type in SQL and named in Go through the service
	// catalog (Classify), so the Bills page, the statement and the invoice all
	// use ONE definition of "which service is this". This used to be a second,
	// hand-written CASE here that had drifted from the catalog: object-storage
	// usage and untyped compute both fell into "Other" on the Bills page while
	// the invoice named them properly.
	//
	// Rows are also split by instance, so the number of distinct instances of a
	// service is exact when several resource types map to the same service (an
	// instance whose type was recorded late appears under two types).
	const query = `
		SELECT p.id, p.name, COALESCE(u.resource_type, ''), u.unit, u.instance_id,
		       SUM(u.quantity)   AS quantity,
		       SUM(u.total_cost) AS cost
		FROM billing.usage_records u
		JOIN auth.projects p ON p.id = u.project_id
		WHERE u.account_id = $1
		  AND u.start_time >= $2
		  AND u.end_time   <= $3
		GROUP BY p.id, p.name, u.resource_type, u.unit, u.instance_id
		ORDER BY p.name, u.resource_type
	`

	rows, err := s.db.QueryContext(ctx, query, accountID, start, end)
	if err != nil {
		return nil, fmt.Errorf("failed to query usage: %w", err)
	}
	defer rows.Close()

	summary := &AccountSummary{
		AccountID:   accountID,
		PeriodStart: start,
		PeriodEnd:   end,
		Currency:    "USD",
		Projects:    []ProjectLine{},
	}

	type lineKey struct {
		project uuid.UUID
		service string
		unit    string
	}
	byProject := map[uuid.UUID]int{} // project ID -> index in Projects
	lineIdx := map[lineKey]int{}     // (project, service, unit) -> index in that project's Services
	instances := map[lineKey]map[string]bool{}

	for rows.Next() {
		var (
			projectID   uuid.UUID
			projectName string
			resource    string
			unit        string
			instanceID  sql.NullString
			quantity    float64
			cost        float64
		)
		if err := rows.Scan(&projectID, &projectName, &resource, &unit, &instanceID, &quantity, &cost); err != nil {
			return nil, fmt.Errorf("failed to scan usage row: %w", err)
		}

		idx, seen := byProject[projectID]
		if !seen {
			summary.Projects = append(summary.Projects, ProjectLine{
				ProjectID:   projectID,
				ProjectName: projectName,
				Services:    []ServiceLine{},
			})
			idx = len(summary.Projects) - 1
			byProject[projectID] = idx
		}

		service := Classify(resource).Service
		key := lineKey{projectID, service, unit}
		li, ok := lineIdx[key]
		if !ok {
			summary.Projects[idx].Services = append(summary.Projects[idx].Services, ServiceLine{Service: service, Unit: unit})
			li = len(summary.Projects[idx].Services) - 1
			lineIdx[key] = li
			instances[key] = map[string]bool{}
		}
		line := &summary.Projects[idx].Services[li]
		line.Quantity += quantity
		line.Cost += cost
		if instanceID.Valid && instanceID.String != "" {
			instances[key][instanceID.String] = true
			line.Instances = len(instances[key])
		}

		summary.Projects[idx].Cost += cost
		summary.TotalCost += cost
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read usage rows: %w", err)
	}

	return summary, nil
}

// CurrentMonthRange returns the month-to-date window used by the
// dashboard: first of the month (UTC) until now.
func CurrentMonthRange(now time.Time) (start, end time.Time) {
	now = now.UTC()
	start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start, now
}
