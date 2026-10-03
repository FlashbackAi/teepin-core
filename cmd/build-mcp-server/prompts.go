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
// control plane (PUT /v1/build/sessions/:id/secrets/:name — a route the
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

// A call that passes validation, shown to the agent in the tool description and
// in every refusal. The options are top-level fields, not a list: GLM-5.3's tool
// calls arrive with every list of objects empty ("options": [] in the model's raw
// arguments, 10 times in a row in one build, 2026-10-02), while plain text
// fields come through.
const askUserExample = `{"question":"How should I fill the process table's rows?",` +
	`"option_1":"Sample data","option_1_description":"Invent realistic process names and values",` +
	`"option_2":"Totals only","option_2_description":"Show only the system totals from the screenshot","allow_other":true}`

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
	Question           string `json:"question" jsonschema:"one clear question"`
	Option1            string `json:"option_1,omitempty" jsonschema:"the first answer the customer can pick, your best recommendation"`
	Option1Description string `json:"option_1_description,omitempty" jsonschema:"one line saying what picking option_1 means"`
	Option2            string `json:"option_2,omitempty" jsonschema:"the second answer the customer can pick"`
	Option2Description string `json:"option_2_description,omitempty" jsonschema:"one line saying what picking option_2 means"`
	Option3            string `json:"option_3,omitempty" jsonschema:"a third answer, if there is one"`
	Option3Description string `json:"option_3_description,omitempty"`
	Option4            string `json:"option_4,omitempty" jsonschema:"a fourth answer, if there is one"`
	Option4Description string `json:"option_4_description,omitempty"`
	// Options is the older list form, still read for a model that fills it.
	Options    []questionOption `json:"options,omitempty" jsonschema:"leave out: use option_1 to option_4 instead"`
	AllowOther bool             `json:"allow_other,omitempty" jsonschema:"let the customer type their own answer instead"`
}

// allOptions is the answers in order: option_1 to option_4 that are set, then
// any given in the older "options" list.
func (a askUserArgs) allOptions() []questionOption {
	var out []questionOption
	for _, o := range []questionOption{
		{a.Option1, a.Option1Description}, {a.Option2, a.Option2Description},
		{a.Option3, a.Option3Description}, {a.Option4, a.Option4Description},
	} {
		if strings.TrimSpace(o.Label) != "" {
			out = append(out, o)
		}
	}
	return append(out, a.Options...)
}

func (c *teepinClient) askUser(_ context.Context, _ *mcp.CallToolRequest, args askUserArgs) (*mcp.CallToolResult, any, error) {
	q := strings.TrimSpace(args.Question)
	if q == "" {
		return textResult("a question is required")
	}
	if len([]rune(q)) > maxQuestionChars {
		return textResult("keep the question under %d characters", maxQuestionChars)
	}
	given := args.allOptions()
	if n := len(given); n < minOptions || n > maxOptions {
		return textResult("give between %d and %d answers to pick from (got %d), as option_1, option_2 and so on at the "+
			"top level of the call. Even an open question needs your best %d to %d likely answers; set allow_other to "+
			"true so the customer can type their own instead. Do not list them in a chat message instead. "+
			"A working call: %s",
			minOptions, maxOptions, n, minOptions, maxOptions, askUserExample)
	}
	seen := map[string]bool{}
	opts := make([]questionOption, 0, len(given))
	for _, o := range given {
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
		Description: "Ask the customer a question with 2 to 4 answers to pick from, given as option_1, option_2 " +
			"(up to option_4), each with an optional option_N_description (and optionally let " +
			"them type their own) when a decision is theirs to make: which service to use, what the app " +
			"should do next, a preference you cannot infer. Prefer this to guessing or asking in plain " +
			"chat, and never put the options in a chat message or a table. If there are more than four " +
			"good candidates, offer your best three or four and set allow_other so the customer can name " +
			"another. Options are required even for an open question: give your best two or more likely " +
			"answers and set allow_other. Example: " + askUserExample + " " +
			"After calling it, END YOUR TURN: the answer arrives as your next message.",
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
