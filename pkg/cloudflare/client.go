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

// Client calls Cloudflare with one API token, through cloudflare-go v0 (the
// embedded *cf.API) and v7 (V7Options). Controllers depend on narrow
// interfaces that *Client satisfies, so tests inject fakes. Classify its
// errors with IsTerminalError, IsNotFound and IsRateLimit.
type Client struct {
	*cf.API

	v7 []option.RequestOption
}

// New returns a Client for token. It refuses only an empty or malformed
// token: whether Cloudflare accepts it shows on the first call. Both SDKs
// send through httpClient, whose timeouts and rate limit apply to every
// request.
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

// httpTimeouts bound the requests kflare sends to Cloudflare, so that one
// that never gets an answer (a silent proxy, a half-open connection) cannot
// stall its controller until the manager restarts. Dialing is bounded
// already: http.DefaultTransport's dialer gives up after 30 seconds.
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
	// before the transport pings it, and http2PingTimeout how long it waits
	// for the answer before closing it: otherwise a dead connection stays in
	// use and every later request on it times out too.
	http2Ping, http2PingTimeout time.Duration
}

// defaultHTTPTimeouts are httpClient's. Cloudflare usually answers in well
// under a second; the largest request, a WorkerScript's upload, is bounded
// by a Kubernetes object's size (about 1.5 MiB).
var defaultHTTPTimeouts = httpTimeouts{
	tlsHandshake:     10 * time.Second,
	responseHeader:   time.Minute,
	request:          2 * time.Minute,
	http2Ping:        30 * time.Second,
	http2PingTimeout: 15 * time.Second,
}

// httpClient is shared by every Client, so that all of them reuse its
// connections and spend one budget per token. Its metrics sit inside the
// rate limit: they count what reached Cloudflare, not calls held back.
var httpClient = withRateLimit(withMetrics(newHTTPClient(defaultHTTPTimeouts)), limiter)

// newHTTPClient returns an HTTP client with timeouts t, on a copy of
// http.DefaultTransport: it keeps HTTPS_PROXY/NO_PROXY, connection reuse,
// HTTP/2 and the dialer's timeout.
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

// V7Options returns the request options of the cloudflare-go v7 services a
// controller calls with this client's token:
//
//	namespaces := kv.NewNamespaceService(client.V7Options()...)
//
// kflare never imports v7's root package (depguard refuses it): its NewClient
// reads CLOUDFLARE_* environment variables into every request, and the import
// grows the manager by a third. Set a param field as its F would:
//
//	params.Title.Value, params.Title.Present = title, true
//
// The services keep v7's two retries (408, 409, 5xx, connection errors; never
// a 429, which rateLimitedTransport marks) and no timeout of v7's own:
// httpClient bounds each attempt, and v7 would not retry after its own ran
// out.
func (c *Client) V7Options() []option.RequestOption {
	return slices.Clone(c.v7)
}
