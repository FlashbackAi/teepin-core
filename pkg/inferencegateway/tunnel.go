// Copyright 2026 TEEPIN Project
// Licensed under the Apache License, Version 2.0

package inferencegateway

import (
	"fmt"
	"net/url"
	"strconv"
)

// TunnelScheme marks an ObservedEndpoint that is reachable only through a
// provider's agent tunnel (a NAT'd home node), not by a direct HTTP dial.
const TunnelScheme = "tunnel"

// TunnelEndpoint builds "tunnel://<providerID>/<instanceID>:<port>".
func TunnelEndpoint(providerID, instanceID string, port int) string {
	return fmt.Sprintf("%s://%s/%s:%d", TunnelScheme, url.PathEscape(providerID), url.PathEscape(instanceID), port)
}

// ParseTunnelEndpoint is the inverse of TunnelEndpoint. ok is false for any
// endpoint that is not a tunnel:// address (a plain http(s) URL).
func ParseTunnelEndpoint(endpoint string) (providerID, instanceID string, port int, ok bool) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != TunnelScheme || u.Host == "" {
		return "", "", 0, false
	}
	path := u.Path
	if len(path) > 0 && path[0] == '/' {
		path = path[1:]
	}
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == ':' {
			p, err := strconv.Atoi(path[i+1:])
			if err != nil || i == 0 {
				return "", "", 0, false
			}
			return u.Host, path[:i], p, true
		}
	}
	return "", "", 0, false
}
