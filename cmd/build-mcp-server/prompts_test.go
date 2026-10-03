// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func promptClient(t *testing.T) *teepinClient { return newTestClient(t, http.NewServeMux()) }

func twoOptions() []questionOption {
	return []questionOption{{Label: "Amadeus", Description: "free test keys"}, {Label: "Duffel"}}
}

func askText(t *testing.T, args askUserArgs) string {
	t.Helper()
	res, _, err := promptClient(t).askUser(context.Background(), &mcp.CallToolRequest{}, args)
	if err != nil {
		t.Fatal(err)
	}
	return res.Content[0].(*mcp.TextContent).Text
}

func secretText(t *testing.T, args requestSecretArgs) string {
	t.Helper()
	res, _, err := promptClient(t).requestSecret(context.Background(), &mcp.CallToolRequest{}, args)
	if err != nil {
		t.Fatal(err)
	}
	return res.Content[0].(*mcp.TextContent).Text
}

func isCard(text, kind string) bool {
	var v struct {
		Kind string `json:"kind"`
	}
	return json.Unmarshal([]byte(text), &v) == nil && v.Kind == kind
}

func TestAskUser_ReturnsAQuestionCardAndTellsTheAgentToStop(t *testing.T) {
	text := askText(t, askUserArgs{Question: "Which flight API should we wire up?", Options: twoOptions(), AllowOther: true})
	if !isCard(text, "question") {
		t.Fatalf("not a question card: %s", text)
	}
	var card struct {
		Question   string           `json:"question"`
		Options    []questionOption `json:"options"`
		AllowOther bool             `json:"allow_other"`
		AgentNote  string           `json:"agent_note"`
	}
	if err := json.Unmarshal([]byte(text), &card); err != nil {
		t.Fatal(err)
	}
	if len(card.Options) != 2 || card.Options[0].Label != "Amadeus" || !card.AllowOther {
		t.Errorf("card = %+v", card)
	}
	if !strings.Contains(card.AgentNote, "End your turn") {
		t.Errorf("the agent is not told to stop: %q", card.AgentNote)
	}
}

func TestAskUser_Validation(t *testing.T) {
	cases := []struct {
		name string
		args askUserArgs
	}{
		{"no question", askUserArgs{Options: twoOptions()}},
		{"long question", askUserArgs{Question: strings.Repeat("q", maxQuestionChars+1), Options: twoOptions()}},
		{"one option", askUserArgs{Question: "q?", Options: []questionOption{{Label: "only"}}}},
		{"five options", askUserArgs{Question: "q?", Options: []questionOption{{Label: "a"}, {Label: "b"}, {Label: "c"}, {Label: "d"}, {Label: "e"}}}},
		{"empty label", askUserArgs{Question: "q?", Options: []questionOption{{Label: "a"}, {Label: "  "}}}},
		{"duplicate labels", askUserArgs{Question: "q?", Options: []questionOption{{Label: "Same"}, {Label: "same"}}}},
		{"long label", askUserArgs{Question: "q?", Options: []questionOption{{Label: "a"}, {Label: strings.Repeat("l", maxOptionLabelChars+1)}}}},
	}
	for _, tc := range cases {
		if text := askText(t, tc.args); isCard(text, "question") {
			t.Errorf("%s: produced a card instead of an error: %s", tc.name, text)
		}
	}
}

// A refusal that only says what is wrong, without a valid call to copy, let a
// live build send the same option-less question 10 times. The example it shows
// must itself be accepted, or the refusal teaches the wrong thing.
func TestAskUser_RefusalShowsAWorkingCall(t *testing.T) {
	var ex askUserArgs
	if err := json.Unmarshal([]byte(askUserExample), &ex); err != nil {
		t.Fatalf("the example is not valid JSON for the tool: %v", err)
	}
	if text := askText(t, ex); !isCard(text, "question") {
		t.Fatalf("the example call is itself refused: %s", text)
	}

	refusal := askText(t, askUserArgs{Question: "How should I fill the table?", AllowOther: true})
	if isCard(refusal, "question") {
		t.Fatalf("no options produced a card: %s", refusal)
	}
	for _, want := range []string{"got 0", "allow_other", askUserExample} {
		if !strings.Contains(refusal, want) {
			t.Errorf("the refusal does not mention %q: %s", want, refusal)
		}
	}
}

func TestRequestSecret_ReturnsACardWithNoValueFieldAndNamesTheVariable(t *testing.T) {
	text := secretText(t, requestSecretArgs{
		Name: "AMADEUS_CLIENT_ID", Label: "Amadeus Client ID",
		Description: "From your Amadeus Self-Service app.", HelpURL: "https://developers.amadeus.com/",
	})
	if !isCard(text, "secret_request") {
		t.Fatalf("not a secret card: %s", text)
	}
	var raw map[string]any
	_ = json.Unmarshal([]byte(text), &raw)
	for k := range raw {
		if strings.Contains(strings.ToLower(k), "value") {
			t.Errorf("the card has a %q field; a secret request must never carry a value", k)
		}
	}
	note, _ := raw["agent_note"].(string)
	if !strings.Contains(note, "NEVER see the value") || !strings.Contains(note, "AMADEUS_CLIENT_ID") || !strings.Contains(note, "End your turn") {
		t.Errorf("agent note = %q", note)
	}
}

func TestRequestSecret_Validation(t *testing.T) {
	cases := []struct {
		name string
		args requestSecretArgs
	}{
		{"lower-case name", requestSecretArgs{Name: "api_key", Label: "Key"}},
		{"reserved name", requestSecretArgs{Name: "TEEPIN_SESSION_TOKEN", Label: "Key"}},
		{"no label", requestSecretArgs{Name: "API_KEY"}},
		{"http link", requestSecretArgs{Name: "API_KEY", Label: "Key", HelpURL: "http://example.com"}},
		{"javascript link", requestSecretArgs{Name: "API_KEY", Label: "Key", HelpURL: "javascript:alert(1)"}},
		{"data link", requestSecretArgs{Name: "API_KEY", Label: "Key", HelpURL: "data:text/html,x"}},
		{"credentials in link", requestSecretArgs{Name: "API_KEY", Label: "Key", HelpURL: "https://user:pw@example.com"}},
		{"hostless link", requestSecretArgs{Name: "API_KEY", Label: "Key", HelpURL: "https:///path"}},
		{"long description", requestSecretArgs{Name: "API_KEY", Label: "Key", Description: strings.Repeat("d", maxDescriptionChars+1)}},
	}
	for _, tc := range cases {
		if text := secretText(t, tc.args); isCard(text, "secret_request") {
			t.Errorf("%s: produced a card instead of an error: %s", tc.name, text)
		}
	}
	// Whitespace around a valid name is tolerated, not rejected.
	if text := secretText(t, requestSecretArgs{Name: "  API_KEY ", Label: "Key"}); !isCard(text, "secret_request") {
		t.Errorf("a padded name should be trimmed and accepted: %s", text)
	}
}
