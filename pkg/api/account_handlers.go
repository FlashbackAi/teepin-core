// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/FlashbackAi/teepin-core/pkg/auth"
)

// AccountHandler serves account and sub-user management.
type AccountHandler struct {
	authService *auth.Service
}

func NewAccountHandler(authService *auth.Service) *AccountHandler {
	return &AccountHandler{authService: authService}
}

// accountResponse shapes an account for the API, adding the formatted
// account number the console and support quote.
type accountResponse struct {
	*auth.Account
	AccountNumberFormatted string `json:"account_number_formatted"`
}

func newAccountResponse(a *auth.Account) accountResponse {
	return accountResponse{Account: a, AccountNumberFormatted: a.FormattedNumber()}
}

// Register creates an account and its owner user.
// POST /v1/accounts
//
// Replaces the old user-only registration: every user now belongs to an
// account, because the account is what gets billed.
func (h *AccountHandler) Register(c *gin.Context) {
	var req struct {
		Type        string `json:"type" binding:"required,oneof=personal organization"`
		DisplayName string `json:"display_name" binding:"required"`
		Alias       string `json:"alias"`
		Email       string `json:"email" binding:"required,email"`
		Password    string `json:"password" binding:"required,min=8"`
		FullName    string `json:"full_name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	account, user, err := h.authService.RegisterAccount(c.Request.Context(), auth.CreateAccountRequest{
		Type:        req.Type,
		DisplayName: req.DisplayName,
		Alias:       req.Alias,
		Email:       req.Email,
		Password:    req.Password,
		FullName:    req.FullName,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"account": newAccountResponse(account),
		"user":    user,
	})
}

// GetCurrent returns the signed-in caller's account.
// GET /v1/accounts/current
func (h *AccountHandler) GetCurrent(c *gin.Context) {
	accountID, ok := requireAccount(c)
	if !ok {
		return
	}

	account, err := h.authService.GetAccount(c.Request.Context(), accountID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "account not found"})
		return
	}

	c.JSON(http.StatusOK, newAccountResponse(account))
}

// UpdateCurrent edits account details.
// PATCH /v1/accounts/current
//
// Owner-only: these fields drive invoicing and tax treatment.
func (h *AccountHandler) UpdateCurrent(c *gin.Context) {
	accountID, ok := requireAccount(c)
	if !ok {
		return
	}
	if role, _ := auth.GetRole(c); role != auth.RoleOwner {
		c.JSON(http.StatusForbidden, gin.H{"error": "only the account owner can change account details"})
		return
	}

	var req struct {
		DisplayName    *string `json:"display_name"`
		LegalName      *string `json:"legal_name"`
		TaxID          *string `json:"tax_id"`
		BillingEmail   *string `json:"billing_email"`
		BillingAddress *string `json:"billing_address"`
		Country        *string `json:"country"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	account, err := h.authService.UpdateAccount(c.Request.Context(), accountID, auth.UpdateAccountRequest{
		DisplayName:    req.DisplayName,
		LegalName:      req.LegalName,
		TaxID:          req.TaxID,
		BillingEmail:   req.BillingEmail,
		BillingAddress: req.BillingAddress,
		Country:        req.Country,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, newAccountResponse(account))
}

// ConvertToOrganization upgrades a personal account.
// POST /v1/accounts/current/convert-to-organization
//
// One-way and owner-only: an organization with sub-users and invoices
// has no meaningful personal form to revert to.
func (h *AccountHandler) ConvertToOrganization(c *gin.Context) {
	accountID, ok := requireAccount(c)
	if !ok {
		return
	}
	if role, _ := auth.GetRole(c); role != auth.RoleOwner {
		c.JSON(http.StatusForbidden, gin.H{"error": "only the account owner can convert the account"})
		return
	}

	var req struct {
		LegalName string `json:"legal_name" binding:"required"`
		TaxID     string `json:"tax_id"`
		Country   string `json:"country"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	account, err := h.authService.ConvertToOrganization(c.Request.Context(), accountID, req.LegalName, req.TaxID, req.Country)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, newAccountResponse(account))
}

// ListUsers returns the account's users.
// GET /v1/accounts/current/users
func (h *AccountHandler) ListUsers(c *gin.Context) {
	accountID, ok := requireAccount(c)
	if !ok {
		return
	}

	users, err := h.authService.ListAccountUsers(c.Request.Context(), accountID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"users": users, "count": len(users)})
}

// CreateUser adds a sub-user to the account.
// POST /v1/accounts/current/users
func (h *AccountHandler) CreateUser(c *gin.Context) {
	accountID, ok := requireAccount(c)
	if !ok {
		return
	}
	if !h.requireUserAdmin(c) {
		return
	}

	var req struct {
		Username string `json:"username" binding:"required"`
		Email    string `json:"email" binding:"required,email"`
		Password string `json:"password" binding:"required,min=8"`
		FullName string `json:"full_name"`
		Role     string `json:"role" binding:"required,oneof=admin member viewer"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	user, err := h.authService.CreateSubUser(c.Request.Context(), accountID, auth.CreateSubUserRequest{
		Username: req.Username,
		Email:    req.Email,
		Password: req.Password,
		FullName: req.FullName,
		Role:     req.Role,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, user)
}

// UpdateUser changes a sub-user's role or status.
// PATCH /v1/accounts/current/users/:user_id
func (h *AccountHandler) UpdateUser(c *gin.Context) {
	accountID, ok := requireAccount(c)
	if !ok {
		return
	}
	if !h.requireUserAdmin(c) {
		return
	}

	userID, err := uuid.Parse(c.Param("user_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user ID"})
		return
	}

	var req struct {
		Role   *string `json:"role"`
		Status *string `json:"status"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if req.Role != nil {
		if err := h.authService.UpdateSubUserRole(c.Request.Context(), accountID, userID, *req.Role); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	if req.Status != nil {
		if err := h.authService.SetSubUserStatus(c.Request.Context(), accountID, userID, *req.Status); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"id": userID, "message": "user updated"})
}

// DeleteUser removes a sub-user.
// DELETE /v1/accounts/current/users/:user_id
func (h *AccountHandler) DeleteUser(c *gin.Context) {
	accountID, ok := requireAccount(c)
	if !ok {
		return
	}
	if !h.requireUserAdmin(c) {
		return
	}

	userID, err := uuid.Parse(c.Param("user_id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user ID"})
		return
	}

	// A caller removing themselves would lock themselves out mid-request.
	if callerID, _ := auth.GetUserID(c); callerID == userID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "you cannot remove your own login"})
		return
	}

	if err := h.authService.DeleteSubUser(c.Request.Context(), accountID, userID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"id": userID, "message": "user removed"})
}

// requireUserAdmin allows only owners and admins to manage users.
func (h *AccountHandler) requireUserAdmin(c *gin.Context) bool {
	role, _ := auth.GetRole(c)
	if role != auth.RoleOwner && role != auth.RoleAdmin {
		c.JSON(http.StatusForbidden, gin.H{"error": "managing users requires the owner or admin role"})
		return false
	}
	return true
}

// LoginSubUser authenticates alias + username + password.
// POST /v1/auth/login/sub-user
func (h *AccountHandler) LoginSubUser(c *gin.Context) {
	var req struct {
		Alias    string `json:"alias" binding:"required"`
		Username string `json:"username" binding:"required"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	accessToken, refreshToken, err := h.authService.LoginSubUser(
		c.Request.Context(), req.Alias, req.Username, req.Password)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"access_token":  accessToken,
		"refresh_token": refreshToken,
		"token_type":    "Bearer",
		"expires_in":    900,
	})
}
