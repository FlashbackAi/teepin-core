// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	execContainer string
	execCommand   string
)

var execCmd = &cobra.Command{
	Use:   "exec INSTANCE_ID",
	Short: "Open an interactive terminal in a running instance",
	Long: `Open an interactive terminal in a running instance — the same
mechanism the console's "Terminal" tab uses (no SSH keys, no open ports:
a short-lived ticket authenticates one WebSocket session tunneled over
the instance's own agent connection).

Examples:
  # Get a shell
  teepin exec inst-a82e7f3

  # A specific container, when the pod has more than one
  teepin exec inst-a82e7f3 --container sidecar

  # Run one command instead of a shell
  teepin exec inst-a82e7f3 --command "cat /etc/hostname"
`,
	Args: cobra.ExactArgs(1),
	Run:  runExec,
}

func init() {
	rootCmd.AddCommand(execCmd)
	execCmd.Flags().StringVar(&execContainer, "container", "",
		"container to attach to (default: the pod's only container)")
	execCmd.Flags().StringVar(&execCommand, "command", "",
		"command to run instead of a shell (default: probes for /bin/bash, then /bin/sh)")
}

func runExec(cmd *cobra.Command, args []string) {
	instanceID := args[0]

	ticket, err := requestExecTicket(instanceID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	attachURL, err := buildAttachURL(ticket.AttachPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	exitCode, err := runTerminalSession(attachURL, ticket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nError: %v\n", err)
		os.Exit(1)
	}
	os.Exit(exitCode)
}

// execTicket mirrors pkg/api's CreateExecSession response — see
// teepin-console/src/lib/api/types.ts's ExecTicket for the same shape on
// the console side; this is the second client speaking the identical
// protocol the plan called for.
type execTicket struct {
	TicketID     string `json:"ticket_id"`
	TicketSecret string `json:"ticket_secret"`
	AttachPath   string `json:"attach_path"`
	ExpiresIn    int    `json:"expires_in"`
}

func requestExecTicket(instanceID string) (*execTicket, error) {
	body := map[string]any{}
	if execContainer != "" {
		body["container"] = execContainer
	}
	if execCommand != "" {
		body["command"] = strings.Fields(execCommand)
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	apiURL := getAPIURL() + "/v1/compute/instances/" + instanceID + "/exec"
	resp, err := apiDo(http.MethodPost, apiURL, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("could not reach the API: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("could not read the response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(respBody, &apiErr)
		msg := apiErr.Error
		if msg == "" {
			msg = fmt.Sprintf("request failed (%d)", resp.StatusCode)
		}
		return nil, fmt.Errorf("%s%s", msg, authHint(resp.StatusCode))
	}

	var ticket execTicket
	if err := json.Unmarshal(respBody, &ticket); err != nil {
		return nil, fmt.Errorf("could not parse the response: %w", err)
	}
	return &ticket, nil
}

// buildAttachURL swaps the API's http(s) scheme for ws(s) and keeps the
// same host — the WebSocket endpoint lives on the same origin as the
// REST API, per pkg/cluster.ExecHandler's routing.
func buildAttachURL(attachPath string) (string, error) {
	base, err := url.Parse(getAPIURL())
	if err != nil {
		return "", fmt.Errorf("invalid API URL: %w", err)
	}
	scheme := "ws"
	if base.Scheme == "https" {
		scheme = "wss"
	}
	return scheme + "://" + base.Host + attachPath, nil
}

// execServerFrame mirrors pkg/cluster/exec_handler.go's execServerFrame —
// the control messages the attach socket sends alongside raw stdout
// bytes.
type execServerFrame struct {
	Type     string `json:"type"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
}

// runTerminalSession dials the attach WebSocket, authenticates with the
// ticket, puts the local terminal into raw mode, and pumps bytes in both
// directions until the session ends. Returns the remote command's exit
// code on a clean end.
func runTerminalSession(attachURL string, ticket *execTicket) (int, error) {
	conn, _, err := websocket.DefaultDialer.Dial(attachURL, nil)
	if err != nil {
		return 0, fmt.Errorf("could not open the terminal connection: %w", err)
	}
	defer conn.Close()

	fd := int(os.Stdin.Fd())
	rows, cols := 24, 80
	if w, h, sizeErr := term.GetSize(fd); sizeErr == nil {
		cols, rows = w, h
	}

	authFrame, err := json.Marshal(map[string]any{
		"type": "auth", "id": ticket.TicketID, "secret": ticket.TicketSecret,
		"rows": rows, "cols": cols,
	})
	if err != nil {
		return 0, err
	}
	if err := conn.WriteMessage(websocket.TextMessage, authFrame); err != nil {
		return 0, fmt.Errorf("could not authenticate: %w", err)
	}

	// Raw mode only from here — a failed dial or auth above must never
	// touch the local terminal at all.
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return 0, fmt.Errorf("could not put the terminal into raw mode: %w", err)
	}
	// Restored exactly once, explicitly, right before this function
	// returns (see the two exit points below) — not via defer, since an
	// error message printed after returning must render in normal
	// (cooked) mode, and restoring twice is worth avoiding.
	restore := func() { _ = term.Restore(fd, oldState) }

	type result struct {
		exitCode int
		err      error
	}
	done := make(chan result, 1)

	// stdout + control frames — the only goroutine reading the socket.
	go func() {
		for {
			mt, data, readErr := conn.ReadMessage()
			if readErr != nil {
				done <- result{}
				return
			}
			switch mt {
			case websocket.BinaryMessage:
				os.Stdout.Write(data)
			case websocket.TextMessage:
				var frame execServerFrame
				if json.Unmarshal(data, &frame) != nil {
					continue
				}
				switch frame.Type {
				case "exit":
					done <- result{exitCode: frame.ExitCode}
					return
				case "error":
					done <- result{err: fmt.Errorf("%s", frame.Message)}
					return
				}
			}
		}
	}()

	// stdin -> the socket.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, readErr := os.Stdin.Read(buf)
			if n > 0 {
				if writeErr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); writeErr != nil {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}()

	// Resize — platform-specific (SIGWINCH on Unix, polling on
	// Windows); see exec_resize_unix.go / exec_resize_windows.go.
	stopResize := make(chan struct{})
	go watchResize(fd, conn, stopResize)
	defer close(stopResize)

	res := <-done
	restore()
	return res.exitCode, res.err
}
