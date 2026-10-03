// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
)

// Models regularly write a port as a bare number ("ports": [8080]) instead of
// the {container, protocol} object the schema asks for. The agent framework
// validates arguments against the schema before the call ever reaches this
// server, so a strict "object only" schema turned that harmless slip into a
// failed deploy. Ports are therefore accepted either way: the schema says
// "a number or an object", and portArg reads both.

// UnmarshalJSON accepts 8080 or {"container": 8080, "protocol": "tcp"}.
func (p *portArg) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] != '{' {
		var n int
		if err := json.Unmarshal(b, &n); err != nil {
			return fmt.Errorf("a port must be a number or an object with container and protocol: %w", err)
		}
		*p = portArg{Container: n}
		return nil
	}
	type plain portArg
	var v plain
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*p = portArg(v)
	return nil
}

// inputSchemaFor builds a tool's input schema the way the MCP SDK would, except
// that a port may be a bare integer as well as an object.
func inputSchemaFor[T any]() (*jsonschema.Schema, error) {
	object, err := jsonschema.For[portArg](nil)
	if err != nil {
		return nil, err
	}
	object.Description = "the port your app listens on inside the container, with an optional protocol"
	number := &jsonschema.Schema{
		Type:        "integer",
		Description: "the port your app listens on inside the container (tcp)",
	}
	either := &jsonschema.Schema{AnyOf: []*jsonschema.Schema{object, number}}
	return jsonschema.For[T](&jsonschema.ForOptions{
		TypeSchemas: map[reflect.Type]*jsonschema.Schema{reflect.TypeFor[portArg](): either},
	})
}
