// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Package migrations embeds the SQL schema migrations into the API
// binary so they can be applied automatically at startup — no separate
// migration tool needs to exist on production servers.
package migrations

import "embed"

// FS holds every versioned migration (NNN_name.up.sql / .down.sql).
//
//go:embed *.sql
var FS embed.FS
