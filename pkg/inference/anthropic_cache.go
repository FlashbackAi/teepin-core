// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package inference

import "github.com/anthropics/anthropic-sdk-go"

// Prompt caching for Anthropic.
//
// A build agent calls the model dozens of times with a request that grows by a
// turn or two each time: the same tool definitions and system prompt (about 16k
// tokens before the conversation starts), then a long history that only gets
// appended to. Without caching, every call pays full price and full latency for
// all of it. Anthropic lets a request mark up to four "breakpoints"; everything
// up to a breakpoint is cached and a later request with the same prefix reads it
// back at a fraction of the cost and time.
//
// Done here, in the provider, rather than asked of the agent: it works for every
// caller of this model, does not depend on an SDK recognising our route name as a
// caching-capable model, and cannot be left off by a harness that does not know
// about it. Breakpoints, in order of how long their prefix stays stable:
//
//  1. the last tool definition (the tools never change within a build),
//  2. the last system block (the instructions rarely change),
//  3. the last block of the final message (so the next turn reads this whole
//     request back and only pays for what it adds),
//  4. the last block of the message three back (a second chance when a turn adds
//     enough blocks to fall outside the lookback window of the third).
//
// A prefix below the model's minimum cacheable size is simply not cached by the
// API; marking it is harmless.

// markCacheable sets an ephemeral cache breakpoint on a content block. Blocks of
// kinds that cannot carry one (thinking, documents, anything unknown) are left
// alone and report false.
func markCacheable(b *anthropic.ContentBlockParamUnion) bool {
	cc := anthropic.NewCacheControlEphemeralParam()
	switch {
	case b.OfText != nil:
		b.OfText.CacheControl = cc
	case b.OfToolResult != nil:
		b.OfToolResult.CacheControl = cc
	case b.OfToolUse != nil:
		b.OfToolUse.CacheControl = cc
	case b.OfImage != nil:
		b.OfImage.CacheControl = cc
	default:
		return false
	}
	return true
}

// markLastCacheable puts a breakpoint on the last block of a message that can
// hold one, and reports whether it placed one.
func markLastCacheable(m *anthropic.MessageParam) bool {
	for i := len(m.Content) - 1; i >= 0; i-- {
		if markCacheable(&m.Content[i]) {
			return true
		}
	}
	return false
}

// applyCacheBreakpoints marks the request's stable prefixes for caching. It
// never uses more than the four breakpoints the API allows, and changes only
// cache markers, never what the model is asked.
func applyCacheBreakpoints(system []anthropic.TextBlockParam, tools []anthropic.ToolUnionParam, messages []anthropic.MessageParam) {
	if n := len(tools); n > 0 && tools[n-1].OfTool != nil {
		tools[n-1].OfTool.CacheControl = anthropic.NewCacheControlEphemeralParam()
	}
	if n := len(system); n > 0 {
		system[n-1].CacheControl = anthropic.NewCacheControlEphemeralParam()
	}
	if n := len(messages); n > 0 {
		markLastCacheable(&messages[n-1])
		if n >= 4 {
			markLastCacheable(&messages[n-3])
		}
	}
}

// Anthropic's prompt-cache prices relative to its ordinary input price: reading
// a cached prefix costs a tenth, writing one costs a quarter more (the default
// five-minute lifetime). Used only to work out what a call cost Teepin.
const (
	cacheReadCostFactor  = 0.10
	cacheWriteCostFactor = 1.25
)

// VendorInputCost is what the input side of a call cost Teepin, in dollars, at a
// vendor's ordinary price of perMillion per million input tokens: freshly
// processed tokens at full price, cache reads and writes at their own factors.
// For a backend that reports no caching it is simply tokens times the price.
// Customers are never charged on this; it feeds cost_basis, the margin record.
func VendorInputCost(u Usage, perMillion float64) float64 {
	fresh := u.InputTokens - u.CachedInputTokens - u.CacheWriteTokens
	if fresh < 0 {
		fresh = 0 // a backend that over-reports cached tokens must not make the cost negative
	}
	weighted := float64(fresh) + float64(u.CachedInputTokens)*cacheReadCostFactor + float64(u.CacheWriteTokens)*cacheWriteCostFactor
	return weighted / 1e6 * perMillion
}
