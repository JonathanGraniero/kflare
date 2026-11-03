/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package cloudflare

import (
	"net/http"
	"slices"
	"time"

	cf "github.com/cloudflare/cloudflare-go"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"golang.org/x/time/rate"
)

// Client is a thin wrapper around the cloudflare-go *API that serves as the
// single production implementation for all kflare controllers.
//
// Testability is achieved through narrow per-controller interfaces: each
// controller declares only the methods it uses, and tests inject a fake that
// satisfies those methods.  *Client satisfies every such interface because it
// embeds *cf.API, which provides all upstream Cloudflare SDK methods.
//
// It also carries what cloudflare-go v7, the SDK Cloudflare generates from
// its OpenAPI spec, needs to call the API with the same token: see V7Options.
//
// Error handling is intentionally left to the caller; use IsTerminalError,
// IsNotFound, and IsRateLimit from this package to classify errors returned
// by any method of either SDK.
type Client struct {
	*cf.API

	v7 []option.RequestOption
}

// New constructs a Client from a raw Cloudflare API token.
//
// The token is validated for basic format requirements by the cloudflare-go
// SDK.  An error is returned if the token string is empty or malformed; note
// that credential correctness (i.e. whether the token is accepted by the
// Cloudflare API) is only confirmed when the first API call is made.
//
// Both SDKs send their requests through httpClient, whose timeouts bound
// every request and whose transport keeps each token within its budget.
func New(token string) (*Client, error) {
	return newClient(token, httpClient)
}

// newClient is New, with the HTTP client both SDKs send their requests
// through. v0's own limiter is turned off: each Client has one, so it paces
// nothing but a reconcile's own calls, which the transport does already.
func newClient(token string, hc *http.Client) (*Client, error) {
	api, err := cf.NewWithAPIToken(token, cf.HTTPClient(hc), cf.UsingRateLimit(float64(rate.Inf)))
	if err != nil {
		return nil, err
	}
	return &Client{
		API: api,
		v7: []option.RequestOption{
			option.WithHTTPClient(hc),
			option.WithEnvironmentProduction(),
			option.WithAPIToken(token),
		},
	}, nil
}

// httpTimeouts bound the requests kflare sends to Cloudflare. A controller
// reconciles one resource at a time, so a request that never gets an answer,
// from a proxy that accepts a connection and goes silent or over a TCP
// connection a network change left half-open, would otherwise stall its
// kind until the manager restarts. Opening a connection is bounded already:
// http.DefaultTransport's dialer gives up after 30 seconds.
type httpTimeouts struct {
	// tlsHandshake bounds the TLS handshake.
	tlsHandshake time.Duration
	// responseHeader bounds the wait for a response's headers, from the end
	// of the request's body.
	responseHeader time.Duration
	// request bounds a whole request, from the dial to the last byte of the
	// response, so it also stops an upload or a response body that stalls.
	request time.Duration
	// http2Ping is how long an HTTP/2 connection may go without a frame
	// from Cloudflare before the transport pings it, and http2PingTimeout
	// how long it waits for the answer before it closes the connection.
	// Without the ping, a request that times out leaves a dead connection
	// in use, and every request after it times out the same way.
	http2Ping, http2PingTimeout time.Duration
}

// defaultHTTPTimeouts are httpClient's. Cloudflare usually answers in well
// under a second. The largest request kflare sends is a WorkerScript's
// upload: its script comes from the resource itself or from a ConfigMap, so
// a Kubernetes object's size limit keeps it under about 1.5 MiB, which takes
// seconds to send even over a slow link.
var defaultHTTPTimeouts = httpTimeouts{
	tlsHandshake:     10 * time.Second,
	responseHeader:   time.Minute,
	request:          2 * time.Minute,
	http2Ping:        30 * time.Second,
	http2PingTimeout: 15 * time.Second,
}

// httpClient is the HTTP client of every Client: one, so that both SDKs and
// every reconcile, each of which builds its own Client, reuse its connections
// and spend one budget per token. Its metrics sit inside the rate limit, so
// that they show what reached Cloudflare: not a call kept back unsent, nor
// the time one waits for its turn.
var httpClient = withRateLimit(withMetrics(newHTTPClient(defaultHTTPTimeouts)), limiter)

// newHTTPClient returns an HTTP client with timeouts t. Its transport is a
// copy of http.DefaultTransport, so it keeps the proxy that the standard
// variables (HTTPS_PROXY, NO_PROXY) set, connection reuse, HTTP/2 and the
// dialer's timeout.
func newHTTPClient(t httpTimeouts) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSHandshakeTimeout = t.tlsHandshake
	transport.ResponseHeaderTimeout = t.responseHeader
	transport.HTTP2 = &http.HTTP2Config{SendPingTimeout: t.http2Ping, PingTimeout: t.http2PingTimeout}
	return &http.Client{Transport: transport, Timeout: t.request}
}

// withMetrics makes hc's requests count in the API metrics.
func withMetrics(hc *http.Client) *http.Client {
	hc.Transport = &instrumentedTransport{next: hc.Transport}
	return hc
}

// withRateLimit makes hc's requests keep within limits.
func withRateLimit(hc *http.Client, limits *rateLimiter) *http.Client {
	hc.Transport = &rateLimitedTransport{next: hc.Transport, limits: limits}
	return hc
}

// V7Options returns the request options for the cloudflare-go v7 services a
// controller calls with this client's token, for example
//
//	namespaces := kv.NewNamespaceService(client.V7Options()...)
//
// kflare builds only the services it calls and never imports v7's root
// package, which golangci-lint's depguard refuses. Its NewClient builds a
// service for every Cloudflare product and applies six CLOUDFLARE_*
// environment variables to every request: other credentials, another base
// URL and extra headers, whereas kflare takes each account's token from its
// Secret. Importing the package at all pulls every v7 package into the
// manager, which grows by a third. A param field is set the way its F would
// set it:
//
//	params.Title.Value, params.Title.Present = title, true
//
// The services share httpClient with v0, whose timeouts bound each attempt.
// They keep v7's retries: two, after a 408, 409, 5xx or connection error,
// which includes a request that timed out, but not after a 429, which the
// transport marks not to retry (rateLimitedTransport). They set no per-attempt
// timeout of v7's own (option.WithRequestTimeout): httpClient's already
// bounds each attempt, and v7 gives up without a retry when its own runs
// out.
func (c *Client) V7Options() []option.RequestOption {
	return slices.Clone(c.v7)
}
