// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/FlashbackAi/teepin-core/pkg/envname"
)

// ask_user and request_secret let the agent put a question or a secure field
// in front of the customer instead of guessing, or asking for a credential in
// chat where it would be sent to the model provider and kept in logs.
//
// Both are deliberately stateless: each validates its arguments and returns a
// JSON card, which the console renders (the tool's result reaches it as an
// ordinary observation event, the same route present_deployment_plan takes).
// The agent is told to end its turn; the customer's answer arrives as its next
// message through the existing follow-up path. So a question survives the pod
// being replaced while it waits, and nothing here blocks or polls.
//
// request_secret returns no value and can never receive one. The customer
// types it into a masked field in the console, which sends it straight to the
// control plane (PUT /v1/kumbha/sessions/:id/secrets/:name — a route the
// agent's token cannot call). The control plane injects it into the deployed
// app's environment. The agent learns only that a name was saved.

const (
	maxQuestionChars    = 300
	maxOptionLabelChars = 80
	maxOptionDescChars  = 200
	minOptions          = 2
	maxOptions          = 4
	maxLabelChars       = 80
	maxDescriptionChars = 400
)

const askUserNote = "The question is now shown to the customer. End your turn now without doing anything else: " +
	"their answer will arrive as your next message."

const requestSecretNote = "The customer is entering this in a secure field. You will NEVER see the value. " +
	"When the app is deployed, the platform sets it as the environment variable %s inside the app's container. " +
	"Read it in code from the environment; do not put a value in any file. While testing here it will be " +
	"missing, so make the app work without it (a clear message or a mock) and check that. " +
	"End your turn now: you will get a message when it is saved."

type questionOption struct {
	Label       string `json:"label" jsonschema:"a short answer the customer can pick"`
	Description string `json:"description,omitempty" jsonschema:"one line saying what picking it means"`
}

type askUserArgs struct {
	Question   string           `json:"question" jsonschema:"one clear question"`
	Options    []questionOption `json:"options" jsonschema:"2 to 4 distinct answers, best recommendation first"`
	AllowOther bool             `json:"allow_other,omitempty" jsonschema:"let the customer type their own answer instead"`
}

func (c *teepinClient) askUser(_ context.Context, _ *mcp.CallToolRequest, args askUserArgs) (*mcp.CallToolResult, any, error) {
	q := strings.TrimSpace(args.Question)
	if q == "" {
		return textResult("a question is required")
	}
	if len([]rune(q)) > maxQuestionChars {
		return textResult("keep the question under %d characters", maxQuestionChars)
	}
	if n := len(args.Options); n < minOptions || n > maxOptions {
		return textResult("give between %d and %d options (got %d). If there are more than %d good candidates, "+
			"offer your best %d and set allow_other to true so the customer can name another; do not list "+
			"options in a chat message instead.", minOptions, maxOptions, n, maxOptions, maxOptions)
	}
	seen := map[string]bool{}
	opts := make([]questionOption, 0, len(args.Options))
	for _, o := range args.Options {
		label := strings.TrimSpace(o.Label)
		if label == "" {
			return textResult("every option needs a label")
		}
		if len([]rune(label)) > maxOptionLabelChars {
			return textResult("keep each option label under %d characters", maxOptionLabelChars)
		}
		key := strings.ToLower(label)
		if seen[key] {
			return textResult("option labels must be distinct; %q appears twice", label)
		}
		seen[key] = true
		desc := strings.TrimSpace(o.Description)
		if len([]rune(desc)) > maxOptionDescChars {
			return textResult("keep each option description under %d characters", maxOptionDescChars)
		}
		opts = append(opts, questionOption{Label: label, Description: desc})
	}

	card := struct {
		Kind       string           `json:"kind"`
		Question   string           `json:"question"`
		Options    []questionOption `json:"options"`
		AllowOther bool             `json:"allow_other"`
		AgentNote  string           `json:"agent_note"`
	}{"question", q, opts, args.AllowOther, askUserNote}
	return jsonResult(card)
}

type requestSecretArgs struct {
	Name        string `json:"name" jsonschema:"the environment variable name your code will read, for example AMADEUS_CLIENT_ID"`
	Label       string `json:"label" jsonschema:"what to call it for the customer, for example Amadeus Client ID"`
	Description string `json:"description,omitempty" jsonschema:"one or two sentences on where to find it"`
	HelpURL     string `json:"help_url,omitempty" jsonschema:"an https link to where the customer can get one"`
}

func (c *teepinClient) requestSecret(_ context.Context, _ *mcp.CallToolRequest, args requestSecretArgs) (*mcp.CallToolResult, any, error) {
	name := strings.TrimSpace(args.Name)
	if err := envname.Validate(name); err != nil {
		return textResult("invalid name: %v", err)
	}
	label := strings.TrimSpace(args.Label)
	if label == "" {
		return textResult("a label is required")
	}
	if len([]rune(label)) > maxLabelChars {
		return textResult("keep the label under %d characters", maxLabelChars)
	}
	desc := strings.TrimSpace(args.Description)
	if len([]rune(desc)) > maxDescriptionChars {
		return textResult("keep the description under %d characters", maxDescriptionChars)
	}
	helpURL := strings.TrimSpace(args.HelpURL)
	if helpURL != "" {
		// The console renders this as a link the customer clicks. Only a
		// plain https URL is acceptable: a javascript: or data: URL from a
		// prompt-injected agent must never become a clickable link.
		u, err := url.Parse(helpURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return textResult("help_url must be a plain https link")
		}
	}

	card := struct {
		Kind        string `json:"kind"`
		Name        string `json:"name"`
		Label       string `json:"label"`
		Description string `json:"description,omitempty"`
		HelpURL     string `json:"help_url,omitempty"`
		AgentNote   string `json:"agent_note"`
	}{"secret_request", name, label, desc, helpURL, fmt.Sprintf(requestSecretNote, name)}
	return jsonResult(card)
}

// jsonResult returns v as a single JSON text block, the shape the console's
// event handler parses out of an observation.
func jsonResult(v any) (*mcp.CallToolResult, any, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, nil, fmt.Errorf("encode result: %w", err)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}, nil, nil
}

func registerPromptTools(server *mcp.Server, client *teepinClient) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "ask_user",
		Description: "Ask the customer a question with 2 to 4 answers to pick from (and optionally let " +
			"them type their own) when a decision is theirs to make: which service to use, what the app " +
			"should do next, a preference you cannot infer. Prefer this to guessing or asking in plain " +
			"chat, and never put the options in a chat message or a table. If there are more than four " +
			"good candidates, offer your best three or four and set allow_other so the customer can name " +
			"another. After calling it, END YOUR TURN: the answer arrives as your next message.",
	}, client.askUser)

	mcp.AddTool(server, &mcp.Tool{
		Name: "request_secret",
		Description: "Ask the customer for a credential your app needs (an API key, a client secret, a " +
			"database URL) through a secure field. You will NEVER see the value; the platform injects it " +
			"into the deployed app as the environment variable you name. ALWAYS use this instead of asking " +
			"for a secret in chat, and never write a secret into any file. After calling it, END YOUR TURN: " +
			"you will get a message when it is saved.",
	}, client.requestSecret)
}
