// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

// Command kumbha-screenshot is a one-shot headless-browser capture: it
// navigates to TEEPIN_TARGET_URL, screenshots the rendered page, and
// uploads the PNG to TEEPIN_UPLOAD_URL — what backs the console's Preview
// tab thumbnail (see pkg/kumbha.Gateway.CaptureScreenshot, which launches
// one pod running this binary per successful deploy).
//
// Deliberately a separate, minimal image from the Kumbha agent itself
// (deploy/kumbha-agent): the agent image ships OpenHands and everything
// it needs to run an autonomous coding session; this one ships nothing
// but Chromium and this binary, and runs for seconds, not the length of
// a whole build. Runs as an ephemeral pod, same pattern as the Kaniko
// build pod (pkg/build.Service.buildInstanceSpec) — provisioned and torn
// down by the control plane, never customer-visible or long-lived.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/chromedp/chromedp"
)

// navigateTimeout bounds how long this process waits for the target page
// to render before giving up — deliberately shorter than
// kumbha.captureTimeoutDefault (the control plane's own wait-and-cleanup
// budget for the whole pod lifecycle), so a hung page fails fast inside
// the pod itself rather than relying solely on the control plane's
// external timeout+delete to end it.
const navigateTimeout = 30 * time.Second

// viewportWidth/Height match a typical desktop preview size — this is a
// thumbnail for a status card, not a pixel-accurate device screenshot, so
// one fixed size is enough; no responsive/mobile variant is captured.
const (
	viewportWidth  = 1280
	viewportHeight = 800
)

// settleTimeout bounds how long capture waits, beyond ordinary page load
// (chromedp.Navigate already blocks for the load event), for web fonts to
// finish and the document to report itself complete. A page that never
// settles in time (a blocked font, a long-polling script) is not treated
// as a capture failure — an imperfect screenshot beats none — so this
// only trims how long the wait can run, not whether capture proceeds.
const settleTimeout = 5 * time.Second

// paintSettleDelay is a short buffer after the settle condition becomes
// true, so a CSS transition still animating at that instant (e.g. a
// fade-in keyed off the same load/fonts-ready event this waits on) has a
// moment to finish before the shot is taken. Replaces the old flat
// pre-settle sleep — see capture's own comment on why a fixed delay alone
// produced "in-between" shots: it had no relationship to when the page
// actually finished rendering. Found live 2026-09-02.
const paintSettleDelay = 500 * time.Millisecond

func main() {
	if err := run(); err != nil {
		log.Fatalf("kumbha-screenshot: %v", err)
	}
}

func run() error {
	targetURL := os.Getenv("TEEPIN_TARGET_URL")
	uploadURL := os.Getenv("TEEPIN_UPLOAD_URL")
	token := os.Getenv("TEEPIN_TOKEN")
	if targetURL == "" || uploadURL == "" || token == "" {
		return fmt.Errorf("TEEPIN_TARGET_URL, TEEPIN_UPLOAD_URL, and TEEPIN_TOKEN are all required")
	}

	png, err := capture(targetURL)
	if err != nil {
		return fmt.Errorf("capture failed: %w", err)
	}

	if err := upload(uploadURL, token, png); err != nil {
		return fmt.Errorf("upload failed: %w", err)
	}

	log.Printf("kumbha-screenshot: captured %d bytes from %s", len(png), targetURL)
	return nil
}

// capture launches headless Chromium (via chromedp — the well-established
// Go binding for the Chrome DevTools Protocol) and returns a
// full-viewport PNG of targetURL.
func capture(targetURL string) ([]byte, error) {
	// --no-sandbox: Chrome's own sandbox needs a setuid helper binary and
	// user-namespace privileges this pod's SecurityContext deliberately
	// does not grant (dropped capabilities, RuntimeDefault seccomp — the
	// same posture every Teepin workload runs under). That's acceptable
	// here for the same reason OpenHands runs unsandboxed inside the
	// Kumbha agent pod (Kumbha plan decision 1): the POD itself, not
	// Chrome's own sandbox, is the isolation boundary. Universal practice
	// for headless Chrome in a container — without it, Chrome fails to
	// launch at all under this SecurityContext.
	// --disable-dev-shm-usage: /dev/shm is frequently too small inside a
	// container's default tmpfs sizing, which crashes Chrome's renderer
	// process rather than just running slower — this makes it use /tmp
	// instead.
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
	)
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer allocCancel()

	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	ctx, timeoutCancel := context.WithTimeout(ctx, navigateTimeout)
	defer timeoutCancel()

	var png []byte
	err := chromedp.Run(ctx,
		chromedp.EmulateViewport(viewportWidth, viewportHeight),
		chromedp.Navigate(targetURL),
		// Poll a readiness condition rather than sleeping a fixed amount:
		// this runs against an arbitrary customer-built app whose markup
		// this process has no knowledge of, so there is no DOM element it
		// could reliably wait for — but "document complete and any web
		// fonts loaded" is a real, generic signal, unlike a flat sleep
		// that has no relationship to when the page actually finished
		// rendering. Previously a flat 2s sleep here produced visibly
		// "in-between" captures (mid font-swap or mid CSS-transition) on
		// pages that took longer than 2s to settle — found live
		// 2026-09-02.
		waitForSettle(),
		chromedp.Sleep(paintSettleDelay),
		chromedp.CaptureScreenshot(&png),
	)
	if err != nil {
		return nil, err
	}
	return png, nil
}

// waitForSettle polls until the document reports itself complete and any
// web fonts have finished loading, using chromedp.Poll's default
// requestAnimationFrame-driven polling — the tightest mode, suited to
// observing exactly this kind of styling/rendering change. A timeout
// (settleTimeout) deliberately does not fail the capture: the predicate
// never becoming true (a blocked font, a page that never reaches
// "complete") should still get a best-effort screenshot rather than none
// at all. Only a genuinely dead context (navigateTimeout already
// expired, or Navigate itself failed further up the chain) aborts the
// capture, and that propagates on its own at the next action.
func waitForSettle() chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		var ready bool
		_ = chromedp.Poll(
			`document.readyState === 'complete' && (!document.fonts || document.fonts.status === 'loaded')`,
			&ready,
			chromedp.WithPollingTimeout(settleTimeout),
		).Do(ctx)
		return nil
	})
}

// upload POSTs png to uploadURL with token as a bearer credential — the
// session-scoped upload token Gateway.CaptureScreenshot minted for this
// pod, authorising exactly this one write (see
// pkg/api.UploadKumbhaScreenshot).
func upload(uploadURL, token string, png []byte) error {
	req, err := http.NewRequest(http.MethodPost, uploadURL, bytes.NewReader(png))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "image/png")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("upload endpoint returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}
