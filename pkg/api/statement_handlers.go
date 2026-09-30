// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"encoding/csv"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
	"github.com/FlashbackAi/teepin-core/pkg/billing"
)

// requireBillingViewer allows only the account's owners and admins: spend by
// project and the credit ledger are sensitive, and a member or viewer sees only
// the resources of their own projects. API keys carry no role and are refused.
func requireBillingViewer(c *gin.Context) (uuid.UUID, bool) {
	accountID, ok := auth.GetAccountID(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "account authentication required"})
		return uuid.Nil, false
	}
	role, _ := auth.GetRole(c)
	if role != auth.RoleOwner && role != auth.RoleAdmin {
		c.JSON(http.StatusForbidden, gin.H{"error": "usage and statements are visible to account owners and admins"})
		return uuid.Nil, false
	}
	return accountID, true
}

// ListStatements handles GET /v1/billing/statements: the months that have a
// statement, newest first.
func (h *BillingHandler) ListStatements(c *gin.Context) {
	accountID, ok := requireBillingViewer(c)
	if !ok {
		return
	}
	months, err := h.billingService.StatementMonths(c.Request.Context(), accountID)
	if err != nil {
		log.Printf("api: statement months for %s: %v", accountID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not list statements"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"months": months})
}

// GetStatement handles GET /v1/billing/statements/:month (YYYY-MM).
func (h *BillingHandler) GetStatement(c *gin.Context) {
	accountID, ok := requireBillingViewer(c)
	if !ok {
		return
	}
	st, err := h.billingService.Statement(c.Request.Context(), accountID, c.Param("month"))
	if errors.Is(err, billing.ErrBadStatementMonth) {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err != nil {
		log.Printf("api: statement %s for %s: %v", c.Param("month"), accountID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not build the statement"})
		return
	}
	c.JSON(http.StatusOK, statementView(st))
}

func statementView(st *billing.Statement) gin.H {
	projects := make([]gin.H, 0, len(st.Projects))
	for _, p := range st.Projects {
		services := make([]gin.H, 0, len(p.Services))
		for _, s := range p.Services {
			resources := make([]gin.H, 0, len(s.Resources))
			for _, r := range s.Resources {
				resources = append(resources, gin.H{
					"resource_type": r.ResourceType, "title": r.Title,
					"quantity": r.Quantity, "unit": r.Unit, "amount": r.Amount,
				})
			}
			services = append(services, gin.H{"service": s.Service, "amount": s.Amount, "resources": resources})
		}
		var pid any
		if p.ProjectID != nil {
			pid = p.ProjectID.String()
		}
		projects = append(projects, gin.H{"project_id": pid, "name": p.Name, "amount": p.Amount, "services": services})
	}
	return gin.H{
		"month":     st.Month,
		"from":      st.From.Format("2006-01-02"),
		"to":        st.To.AddDate(0, 0, -1).Format("2006-01-02"),
		"opening":   st.Opening,
		"purchased": st.Purchased,
		"granted":   st.Granted,
		"used":      st.Used,
		"expired":   st.Expired,
		"revoked":   st.Revoked,
		"closing":   st.Closing,
		"projects":  projects,
	}
}

// csvSafe defuses spreadsheet formulas. A cell that begins with =, +, - or @
// is run as a formula by Excel and Sheets, and several of these columns hold
// text a customer chose (project names, descriptions), so a hostile name could
// otherwise execute in the account owner's spreadsheet. A leading apostrophe
// makes the spreadsheet treat it as text.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// GetStatementCSV handles GET /v1/billing/statements/:month/csv: every ledger
// row of the month with a running balance that starts at the statement's
// opening balance.
func (h *BillingHandler) GetStatementCSV(c *gin.Context) {
	accountID, ok := requireBillingViewer(c)
	if !ok {
		return
	}
	month := c.Param("month")
	// Validate before any output so a bad month is a clean 400, not a broken file.
	if _, _, err := billing.ParseStatementMonth(month, time.Now()); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="teepin-statement-%s.csv"`, month))
	w := csv.NewWriter(c.Writer)
	_ = w.Write([]string{"date", "type", "description", "project", "service", "resource", "quantity", "unit", "amount_usd", "balance_usd"})

	err := h.billingService.EachStatementLine(c.Request.Context(), accountID, month, func(l billing.StatementLine) error {
		qty := ""
		if l.Unit != "" || l.Quantity != 0 {
			qty = fmt.Sprintf("%.6f", l.Quantity)
		}
		return w.Write([]string{
			l.When.UTC().Format("2006-01-02 15:04:05"), l.Kind, csvSafe(l.Description), csvSafe(l.Project),
			csvSafe(l.Service), csvSafe(l.Resource), qty, csvSafe(l.Unit),
			fmt.Sprintf("%.6f", l.Amount), fmt.Sprintf("%.6f", l.Balance),
		})
	})
	w.Flush()
	if err != nil {
		// Headers are already sent; the truncated file is the only signal left.
		log.Printf("api: statement csv %s for %s: %v", month, accountID, err)
	}
}
