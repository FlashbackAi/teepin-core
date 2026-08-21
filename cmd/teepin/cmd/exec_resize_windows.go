// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build windows

package cmd

import (
	"encoding/json"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/term"
)

// watchResize sends a resize frame whenever the local terminal's size
// changes, until stop is closed. Windows has no SIGWINCH — the console
// host itself doesn't deliver a resize signal to the process — so this
// polls instead. 500ms is frequent enough that a resize feels immediate
// without meaningfully loading the connection.
func watchResize(fd int, conn *websocket.Conn, stop <-chan struct{}) {
	lastW, lastH, _ := term.GetSize(fd)

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			w, h, err := term.GetSize(fd)
			if err != nil || (w == lastW && h == lastH) {
				continue
			}
			lastW, lastH = w, h
			frame, err := json.Marshal(map[string]any{"type": "resize", "rows": h, "cols": w})
			if err != nil {
				continue
			}
			_ = conn.WriteMessage(websocket.TextMessage, frame)
		}
	}
}
