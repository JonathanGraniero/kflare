/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package cloudflare

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudflare/cloudflare-go/v7/kv"
	"github.com/cloudflare/cloudflare-go/v7/option"
)

// transportFunc stands in for the network.
type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// invalidBaseURL is an API base URL on a host that never resolves (RFC
// 6761), for tests whose requests must not leave the machine even when the
// code under test bypasses the transport they set up.
const invalidBaseURL = "https://api.kflare.invalid/client/v4"

// getAccount calls v0, and getNamespace v7, the way the controllers do.
func getAccount(ctx context.Context, client *Client) error {
	_, _, err := client.Account(ctx, "account-id")
	return err
}

func getNamespace(ctx context.Context, client *Client, opts ...option.RequestOption) error {
	var params kv.NamespaceGetParams
	params.AccountID.Value, params.AccountID.Present = "account-id", true
	_, err := kv.NewNamespaceService(client.V7Options()...).Get(ctx, "ns-id", params, opts...)
	return err
}

// timeoutLimit is how long a call that a test's timeout ends may take: the
// timeout, v7's two retries and their delays, and room for a slow machine.
const timeoutLimit = 5 * time.Second

// callContext ends a call after twice timeoutLimit, so that a timeout that
// does not fire fails its test in seconds rather than hanging it.
func callContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 2*timeoutLimit)
	t.Cleanup(cancel)
	return ctx
}

// get sends a GET request for url with client and ctx.
func get(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

// expectTimeout fails t unless err is a timeout that ended the call started
// at start within timeoutLimit.
func expectTimeout(t *testing.T, err error, start time.Time) {
	t.Helper()
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > timeoutLimit {
		t.Fatalf("the call took %s, want at most %s", elapsed, timeoutLimit)
	}
}

func TestNew_SendsBothSDKsThroughHTTPClient(t *testing.T) {
	original := httpClient.Transport
	t.Cleanup(func() { httpClient.Transport = original })
	var paths []string
	httpClient.Transport = transportFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"result":{"id":"id","name":"name","title":"title"},"success":true,"errors":[],"messages":[]}`)),
			Request: req,
		}, nil
	})

	client, err := New("test-token-value")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	client.BaseURL = invalidBaseURL
	if err := getAccount(context.Background(), client); err != nil {
		t.Fatalf("v0: %v", err)
	}
	if err := getNamespace(context.Background(), client, option.WithBaseURL(invalidBaseURL+"/")); err != nil {
		t.Fatalf("v7: %v", err)
	}
	want := []string{"/client/v4/accounts/account-id", "/client/v4/accounts/account-id/storage/kv/namespaces/ns-id"}
	if strings.Join(paths, " ") != strings.Join(want, " ") {
		t.Errorf("httpClient sent %q, want %q", paths, want)
	}
}

func TestHTTPClient_DefaultTimeouts(t *testing.T) {
	transport, ok := httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("httpClient's transport is a %T", httpClient.Transport)
	}
	if transport.HTTP2 == nil {
		t.Fatal("httpClient has no HTTP/2 health check")
	}
	for _, c := range []struct {
		name      string
		got, want time.Duration
	}{
		{"TLS handshake", transport.TLSHandshakeTimeout, 10 * time.Second},
		{"response headers", transport.ResponseHeaderTimeout, time.Minute},
		{"whole request", httpClient.Timeout, 2 * time.Minute},
		{"HTTP/2 ping", transport.HTTP2.SendPingTimeout, 30 * time.Second},
		{"HTTP/2 ping answer", transport.HTTP2.PingTimeout, 15 * time.Second},
	} {
		if c.got != c.want {
			t.Errorf("%s timeout = %s, want %s", c.name, c.got, c.want)
		}
	}
	if !transport.ForceAttemptHTTP2 {
		t.Error("httpClient does not try HTTP/2")
	}
}

// testTimeouts are short, for a test to see one fire. Each test lowers the
// timeout it checks; the others stay too long to fire first.
var testTimeouts = httpTimeouts{
	tlsHandshake:     time.Minute,
	responseHeader:   time.Minute,
	request:          time.Minute,
	http2Ping:        time.Minute,
	http2PingTimeout: time.Minute,
}

// silentServer accepts requests and never answers them. requests counts
// them.
func silentServer(t *testing.T) (srv *httptest.Server, requests *atomic.Int32) {
	requests = new(atomic.Int32)
	done := make(chan struct{})
	srv = httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}))
	t.Cleanup(func() {
		close(done)
		srv.Close()
	})
	return srv, requests
}

func TestNewClient_ServerThatNeverAnswers(t *testing.T) {
	timeouts := testTimeouts
	timeouts.responseHeader = 200 * time.Millisecond

	t.Run("v0", func(t *testing.T) {
		srv, requests := silentServer(t)
		client, err := newClient("test-token-value", newHTTPClient(timeouts))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		client.BaseURL = srv.URL + "/client/v4"
		start := time.Now()
		expectTimeout(t, getAccount(callContext(t), client), start)
		// v0 returns a timeout at once, without its retries.
		if got := requests.Load(); got != 1 {
			t.Errorf("v0 sent %d requests, want 1", got)
		}
	})

	t.Run("v7", func(t *testing.T) {
		srv, requests := silentServer(t)
		client, err := newClient("test-token-value", newHTTPClient(timeouts))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		start := time.Now()
		expectTimeout(t, getNamespace(callContext(t), client, option.WithBaseURL(srv.URL+"/client/v4/")), start)
		// v7 retries a request that timed out twice, each attempt bounded.
		if got := requests.Load(); got != 3 {
			t.Errorf("v7 sent %d requests, want 3", got)
		}
	})
}

func TestNewHTTPClient_TLSHandshakeTimeout(t *testing.T) {
	// A listener that accepts connections and never sends a byte.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})

	timeouts := testTimeouts
	timeouts.tlsHandshake = 200 * time.Millisecond
	start := time.Now()
	_, err = get(callContext(t), newHTTPClient(timeouts), "https://"+ln.Addr().String()+"/")
	expectTimeout(t, err, start)
	if !strings.Contains(err.Error(), "TLS handshake timeout") {
		t.Errorf("err = %v, want a TLS handshake timeout", err)
	}
}

func TestNewHTTPClient_RequestTimeout(t *testing.T) {
	// A server that sends its headers and the start of a body, then stalls.
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"result":`)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}))
	t.Cleanup(func() {
		close(done)
		srv.Close()
	})

	timeouts := testTimeouts
	timeouts.request = 300 * time.Millisecond
	start := time.Now()
	resp, err := get(callContext(t), newHTTPClient(timeouts), srv.URL)
	if err != nil {
		t.Fatalf("the headers did not arrive: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, err = io.ReadAll(resp.Body)
	expectTimeout(t, err, start)
}

// relay forwards TCP connections to a target. Once frozen, it passes nothing
// on in either direction, like a connection a network change left
// half-open.
type relay struct {
	ln     net.Listener
	frozen atomic.Bool

	mu    sync.Mutex
	conns []net.Conn
}

func startRelay(t *testing.T, target string) *relay {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &relay{ln: ln}
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", target)
			if err != nil {
				_ = in.Close()
				continue
			}
			r.mu.Lock()
			r.conns = append(r.conns, in, out)
			r.mu.Unlock()
			go r.pipe(out, in)
			go r.pipe(in, out)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, conn := range r.conns {
			_ = conn.Close()
		}
	})
	return r
}

// pipe copies src to dst, and drops what it reads while r is frozen.
func (r *relay) pipe(dst, src net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 && !r.frozen.Load() {
			if _, err := dst.Write(buf[:n]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func TestNewHTTPClient_ClosesASilentHTTP2Connection(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	r := startRelay(t, srv.Listener.Addr().String())

	timeouts := testTimeouts
	timeouts.http2Ping, timeouts.http2PingTimeout = time.Second, 500*time.Millisecond
	client := newHTTPClient(timeouts)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: roots}
	url := "https://" + r.ln.Addr().String() + "/"

	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("the first request used %s, want HTTP/2", resp.Proto)
	}

	// The next request goes over the same connection, which now carries
	// nothing either way. Every other timeout is a minute, and the relay
	// passes no new connection's handshake on either, so only the ping can
	// end the request soon.
	r.frozen.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err = client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a request over a silent connection succeeded")
	}
	if ctx.Err() != nil {
		t.Fatalf("the request waited for its context: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the request took %s to fail", elapsed)
	}
}

// proxyChildEnv marks the process TestNew_HonorsTheProxyEnvironment checks
// the proxy in. net/http reads the proxy variables once per process, so the
// test sets them in a process of its own, before its first request.
const proxyChildEnv = "KFLARE_TEST_PROXY_CHILD"

func TestNew_HonorsTheProxyEnvironment(t *testing.T) {
	if os.Getenv(proxyChildEnv) == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestNew_HonorsTheProxyEnvironment$", "-test.v")
		cmd.Env = append(os.Environ(), proxyChildEnv+"=1")
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "--- PASS: TestNew_HonorsTheProxyEnvironment") {
			t.Fatalf("the check failed in its own process: %v\n%s", err, out)
		}
		return
	}

	// A proxy that records where each tunnel is to go, and refuses it. The
	// SDKs retry a refusal, so the proxy records only what the test has not
	// yet read.
	connects := make(chan string, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			select {
			case connects <- r.Host:
			default:
			}
		}
		http.Error(w, "kflare test proxy", http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")

	client, err := New("test-token-value")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	client.BaseURL = invalidBaseURL
	for _, c := range []struct {
		sdk  string
		call func(context.Context) error
	}{
		{"v0", func(ctx context.Context) error { return getAccount(ctx, client) }},
		{"v7", func(ctx context.Context) error {
			return getNamespace(ctx, client, option.WithBaseURL(invalidBaseURL+"/"))
		}},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		errs := make(chan error, 1)
		go func() { errs <- c.call(ctx) }()
		select {
		case host := <-connects:
			if host != "api.kflare.invalid:443" {
				t.Errorf("%s: the proxy was asked for %q, want api.kflare.invalid:443", c.sdk, host)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("%s: no request reached the proxy", c.sdk)
		}
		cancel()
		if err := <-errs; err == nil {
			t.Errorf("%s: the call succeeded through a proxy that refuses every tunnel", c.sdk)
		}
		// Forget what a retry recorded before the cancel stopped the SDK.
		select {
		case <-connects:
		default:
		}
	}
}
