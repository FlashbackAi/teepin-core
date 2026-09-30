// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package main

import (
	"encoding/json"
	"testing"
)

func TestPortArgAcceptsNumberOrObject(t *testing.T) {
	var a deployArgs
	if err := json.Unmarshal([]byte(`{"name":"x","cpu_units":1,"memory_gb":1,"ports":[8080,{"container":9090,"protocol":"udp"}]}`), &a); err != nil {
		t.Fatal(err)
	}
	if len(a.Ports) != 2 || a.Ports[0].Container != 8080 || a.Ports[1].Container != 9090 || a.Ports[1].Protocol != "udp" {
		t.Fatalf("ports = %+v", a.Ports)
	}
	if err := json.Unmarshal([]byte(`{"ports":["abc"]}`), &a); err == nil {
		t.Fatal("a non-numeric port must be rejected")
	}
}

func TestInputSchemaAllowsBareIntegerPort(t *testing.T) {
	s, err := inputSchemaFor[deployArgs]()
	if err != nil {
		t.Fatal(err)
	}
	items := s.Properties["ports"].Items
	if items == nil || len(items.AnyOf) != 2 {
		t.Fatalf("ports items should be anyOf object|integer, got %+v", items)
	}
	rs, err := s.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{`{"name":"x","cpu_units":1,"memory_gb":1,"ports":[8080]}`, `{"name":"x","cpu_units":1,"memory_gb":1,"ports":[{"container":80}]}`} {
		var v map[string]any
		_ = json.Unmarshal([]byte(in), &v)
		if err := rs.Validate(&v); err != nil {
			t.Fatalf("%s rejected: %v", in, err)
		}
	}
}
