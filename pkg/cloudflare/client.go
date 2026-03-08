/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package cloudflare

import (
	cf "github.com/cloudflare/cloudflare-go"
)

// Client is a thin wrapper around the cloudflare-go *API that serves as the
// single production implementation for all kflare controllers.
//
// Testability is achieved through narrow per-controller interfaces: each
// controller declares only the methods it uses, and tests inject a fake that
// satisfies those methods.  *Client satisfies every such interface because it
// embeds *cf.API, which provides all upstream Cloudflare SDK methods.
//
// Error handling is intentionally left to the caller; use IsTerminalError,
// IsNotFound, and IsRateLimit from this package to classify errors returned
// by any method.
type Client struct {
	*cf.API
}

// New constructs a Client from a raw Cloudflare API token.
//
// The token is validated for basic format requirements by the cloudflare-go
// SDK.  An error is returned if the token string is empty or malformed; note
// that credential correctness (i.e. whether the token is accepted by the
// Cloudflare API) is only confirmed when the first API call is made.
func New(token string) (*Client, error) {
	api, err := cf.NewWithAPIToken(token)
	if err != nil {
		return nil, err
	}
	return &Client{API: api}, nil
}
