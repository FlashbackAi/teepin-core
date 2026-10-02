// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package inference

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
)

// Found live 2026-10-02 by the capability check: asked to name the colours in
// an image, Claude answered "blue, red" for green and yellow, because the
// adapter dropped the image and the model guessed. The picture must reach it.
func TestAnthropic_Complete_SendsImagesToTheModel(t *testing.T) {
	var got struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type   string `json:"type"`
				Text   string `json:"text"`
				Source struct {
					Type      string `json:"type"`
					MediaType string `json:"media_type"`
					Data      string `json:"data"`
					URL       string `json:"url"`
				} `json:"source"`
			} `json:"content"`
		} `json:"messages"`
	}
	p, _ := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okAnthropicReply))
	})

	msg := `{"role":"user","content":[
		{"type":"text","text":"What colour?"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}},
		{"type":"image_url","image_url":{"url":"https://example.com/a.jpg"}},
		{"type":"input_audio","input_audio":{"data":"x","format":"wav"}}]}`
	if _, err := p.Complete(context.Background(), Request{Model: "m", Messages: []json.RawMessage{json.RawMessage(msg)}, MaxTokens: 10}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(got.Messages) != 1 || len(got.Messages[0].Content) != 3 {
		t.Fatalf("upstream got %+v, want one user turn with text + 2 images (audio has no Claude equivalent)", got.Messages)
	}
	c := got.Messages[0].Content
	if c[0].Type != "text" || c[0].Text != "What colour?" {
		t.Errorf("text block: %+v", c[0])
	}
	if c[1].Type != "image" || c[1].Source.Type != "base64" || c[1].Source.MediaType != "image/png" || c[1].Source.Data != "QUJD" {
		t.Errorf("data URL image: %+v", c[1])
	}
	if c[2].Type != "image" || c[2].Source.Type != "url" || c[2].Source.URL != "https://example.com/a.jpg" {
		t.Errorf("https image: %+v", c[2])
	}
}

func TestAnthropic_Complete_RefusesImagesItCannotSend(t *testing.T) {
	p, _ := newTestAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("an untranslatable image must be refused before anything is sent")
	})
	for _, url := range []string{"http://example.com/a.png", "data:image/bmp;base64,QUJD", "file:///etc/passwd", "data:image/png,notbase64"} {
		msg := `{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + url + `"}}]}`
		_, err := p.Complete(context.Background(), Request{Model: "m", Messages: []json.RawMessage{json.RawMessage(msg)}, MaxTokens: 10})
		if !errors.Is(err, ErrProviderRejected) {
			t.Errorf("%s: err = %v, want ErrProviderRejected", url, err)
		}
	}
}
