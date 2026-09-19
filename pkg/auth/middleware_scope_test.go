// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package auth

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func TestStore_PublishesAPIKeyScopes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	store(c, &Principal{AccountID: uuid.New(), ViaAPIKey: true, Scopes: []string{"inference:invoke"}})
	scopes, via := GetAPIKeyScopes(c)
	if !via || len(scopes) != 1 || scopes[0] != "inference:invoke" {
		t.Fatalf("scopes=%v via=%v", scopes, via)
	}

	// A signed-in user has no scope list; it must not look like a key with
	// zero permissions.
	c2, _ := gin.CreateTestContext(httptest.NewRecorder())
	store(c2, &Principal{AccountID: uuid.New()})
	if _, via := GetAPIKeyScopes(c2); via {
		t.Fatal("a JWT principal was reported as an API key")
	}
}
