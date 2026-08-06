// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package billing

import (
	"context"
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
	// resource_type holds the instance type ("gpu.h100.1g.10gb"), which
	// maps to a customer-facing service name. Grouping in SQL keeps the
	// whole summary to a single round trip.
	const query = `
		SELECT p.id, p.name,
		       CASE
		         WHEN u.resource_type LIKE 'gpu.%'     THEN 'GPU compute'
		         WHEN u.resource_type LIKE 'cpu.%'     THEN 'CPU compute'
		         WHEN u.resource_type LIKE 'storage%'  THEN 'Storage'
		         WHEN u.resource_type LIKE 'network%'  THEN 'Networking'
		         ELSE 'Other'
		       END AS service,
		       u.unit,
		       SUM(u.quantity)                AS quantity,
		       SUM(u.total_cost)              AS cost,
		       COUNT(DISTINCT u.instance_id)  AS instances
		FROM billing.usage_records u
		JOIN auth.projects p ON p.id = u.project_id
		WHERE u.account_id = $1
		  AND u.start_time >= $2
		  AND u.end_time   <= $3
		GROUP BY p.id, p.name, service, u.unit
		ORDER BY p.name, service
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

	// Rows arrive ordered by project, so accumulate into the current one.
	byProject := map[uuid.UUID]int{} // project ID -> index in Projects

	for rows.Next() {
		var (
			projectID   uuid.UUID
			projectName string
			line        ServiceLine
		)
		if err := rows.Scan(&projectID, &projectName, &line.Service, &line.Unit,
			&line.Quantity, &line.Cost, &line.Instances); err != nil {
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

		summary.Projects[idx].Services = append(summary.Projects[idx].Services, line)
		summary.Projects[idx].Cost += line.Cost
		summary.TotalCost += line.Cost
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
