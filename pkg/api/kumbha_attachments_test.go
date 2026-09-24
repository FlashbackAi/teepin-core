// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestSanitizeAttachmentFilename(t *testing.T) {
	cases := map[string]string{
		"photo.png":                "photo.png",
		"../../etc/passwd":         ".._.._etc_passwd",
		"a/b\\c":                   "a_b_c",
		"":                         "attachment",
		".":                        "attachment",
		"..":                       "attachment",
		"with\x00control\x1fchars": "withcontrolchars",
	}
	for in, want := range cases {
		if got := sanitizeAttachmentFilename(in); got != want {
			t.Errorf("sanitizeAttachmentFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeAttachmentFilename_TruncatesVeryLongNames(t *testing.T) {
	long := ""
	for i := 0; i < 300; i++ {
		long += "a"
	}
	got := sanitizeAttachmentFilename(long)
	if len(got) != 200 {
		t.Errorf("len(got) = %d, want 200", len(got))
	}
}

// CreateKumbhaAttachment must refuse cleanly (404, never a panic or a
// misleading error) when object storage, its signer, or the public base
// URL isn't configured — the three-way guard is load-bearing: a URL minted
// without publicBaseURL would be unreachable by the external LLM provider
// that's actually meant to fetch it (see the handler's own doc comment).
func TestCreateKumbhaAttachment_404sWhenNotConfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &Server{}
	r := gin.New()
	r.POST("/v1/kumbha/attachments", s.CreateKumbhaAttachment)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/kumbha/attachments?filename=a.png", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
