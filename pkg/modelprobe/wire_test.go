// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package modelprobe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FlashbackAi/teepin-core/pkg/inference"
)

// The same capable fake model, but reached over real HTTP through the real
// vLLM/OpenAI-compatible provider: proves the probes survive the provider's
// own request building (messages, tools, tool_choice, multimodal parts) and
// that the model list is read for metadata, the way a deployed backend would be.
func TestProbes_OverTheRealOpenAICompatibleProvider(t *testing.T) {
	model := &fakeModel{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "served-model", "max_model_len": 40960}}})
		case "/v1/chat/completions":
			var body struct {
				Messages []json.RawMessage          `json:"messages"`
				MaxTok   int                        `json:"max_tokens"`
				Extra    map[string]json.RawMessage `json:"-"`
			}
			var raw map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&raw)
			_ = json.Unmarshal(raw["messages"], &body.Messages)
			req := inference.Request{Messages: body.Messages, Extra: map[string]json.RawMessage{}}
			for k, v := range raw {
				if k != "messages" && k != "model" && k != "max_tokens" {
					req.Extra[k] = v
				}
			}
			resp, err := model.Complete(r.Context(), req)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(resp.Body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := inference.NewVLLM(inference.VLLMConfig{BaseURL: srv.URL, Model: "served-model", SupportsTools: true})
	r := runner(p)
	for _, c := range AllCapabilities {
		if got := r.RunCapability(context.Background(), c); got.Status != StatusPassed {
			t.Errorf("%s over HTTP: %s (%s)", c, got.Status, got.Detail)
		}
	}

	md := metadataOf(context.Background(), p)
	if md.Error != "" || md.ContextWindow != 40960 {
		t.Errorf("metadata = %+v, want context window 40960 from /v1/models", md)
	}
}
