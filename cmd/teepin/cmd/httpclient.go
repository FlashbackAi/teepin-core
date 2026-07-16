// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cmd

import (
	"io"
	"net/http"
	"time"
)

// apiClient is the shared HTTP client for all CLI commands.
var apiClient = &http.Client{Timeout: 60 * time.Second}

// apiDo issues an authenticated request against the TEEPIN API. The
// API key from `teepin login` (or TEEPIN_API_KEY) is attached as a
// Bearer token; without one the request is sent unauthenticated, which
// works only against a local standalone server.
func apiDo(method, url string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key, err := loadAPIKey(); err == nil && key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return apiClient.Do(req)
}

// authHint returns a login suggestion for 401 responses.
func authHint(statusCode int) string {
	if statusCode == http.StatusUnauthorized {
		return "\n   Hint: authenticate first with `teepin auth login` (or set TEEPIN_API_KEY)"
	}
	return ""
}
