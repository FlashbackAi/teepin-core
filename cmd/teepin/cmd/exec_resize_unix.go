// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

//go:build !windows

package cmd

import (
	"encoding/json"
	"os"
	"os/signal"
	"syscall"

	"github.com/gorilla/websocket"
	"golang.org/x/term"
)

// watchResize sends a resize frame whenever the local terminal's size
// changes, until stop is closed. On Unix the kernel tells us exactly
// when via SIGWINCH — no polling needed.
func watchResize(fd int, conn *websocket.Conn, stop <-chan struct{}) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	defer signal.Stop(sigCh)

	for {
		select {
		case <-stop:
			return
		case <-sigCh:
			w, h, err := term.GetSize(fd)
			if err != nil {
				continue
			}
			frame, err := json.Marshal(map[string]any{"type": "resize", "rows": h, "cols": w})
			if err != nil {
				continue
			}
			_ = conn.WriteMessage(websocket.TextMessage, frame)
		}
	}
}
