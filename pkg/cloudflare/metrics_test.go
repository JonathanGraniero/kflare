/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package cloudflare

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// requestsCounted reads kflare_cloudflare_api_requests_total.
func requestsCounted(controller, method, code string) float64 {
	return testutil.ToFloat64(apiRequests.WithLabelValues(controller, method, code))
}

// durationsObserved reads how many requests
// kflare_cloudflare_api_request_duration_seconds holds, from what the metrics
// endpoint serves.
func durationsObserved(t *testing.T, controller, method string) uint64 {
	t.Helper()
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != "kflare_cloudflare_api_request_duration_seconds" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["controller"] == controller && labels["method"] == method {
				return metric.GetHistogram().GetSampleCount()
			}
		}
	}
	return 0
}

// cloudflareAt returns a Client with a token of the test's own, on the
// httpClient every Client uses, and an address for v7's calls, with Cloudflare
// answering with answer.
func cloudflareAt(t *testing.T, answer http.HandlerFunc) (*Client, option.RequestOption) {
	t.Helper()
	server := httptest.NewServer(answer)
	t.Cleanup(server.Close)
	client, err := New(t.Name())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	client.BaseURL = server.URL
	return client, option.WithBaseURL(server.URL + "/")
}

func answerOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"result":{"id":"id","title":"title"},"success":true,"errors":[],"messages":[]}`)
}

func TestAPIMetrics_CountBothSDKsUnderTheCallersController(t *testing.T) {
	client, v7Address := cloudflareAt(t, answerOK)
	ctx := WithController(context.Background(), t.Name())

	if err := getAccount(ctx, client); err != nil {
		t.Fatalf("v0: %v", err)
	}
	if err := getNamespace(ctx, client, v7Address); err != nil {
		t.Fatalf("v7: %v", err)
	}

	if got := requestsCounted(t.Name(), http.MethodGet, "200"); got != 2 {
		t.Errorf("%v requests counted, want one per SDK", got)
	}
	if got := durationsObserved(t, t.Name(), http.MethodGet); got != 2 {
		t.Errorf("%d durations observed, want one per SDK", got)
	}
}

func TestAPIMetrics_CountEachAttempt(t *testing.T) {
	var sent atomic.Int64
	client, v7Address := cloudflareAt(t, func(w http.ResponseWriter, req *http.Request) {
		if sent.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		answerOK(w, req)
	})

	err := getNamespace(WithController(context.Background(), t.Name()), client, v7Address)
	if err != nil {
		t.Fatalf("v7 did not retry: %v", err)
	}

	for code, want := range map[string]float64{"502": 1, "200": 1} {
		if got := requestsCounted(t.Name(), http.MethodGet, code); got != want {
			t.Errorf("code %s: %v requests counted, want %v", code, got, want)
		}
	}
	if got := durationsObserved(t, t.Name(), http.MethodGet); got != 2 {
		t.Errorf("%d durations observed, want one per attempt", got)
	}
}

func TestAPIMetrics_LeaveOutACallKeptBack(t *testing.T) {
	client, v7Address := cloudflareAt(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	ctx := WithController(context.Background(), t.Name())

	if err := getNamespace(ctx, client, v7Address); !IsRateLimit(err) {
		t.Fatalf("err = %v, want Cloudflare's 429", err)
	}
	if err := getNamespace(ctx, client, v7Address); !IsRateLimit(err) {
		t.Fatalf("err = %v, want the call kept back", err)
	}

	if got := requestsCounted(t.Name(), http.MethodGet, "429"); got != 1 {
		t.Errorf("%v 429s counted, want the one Cloudflare sent", got)
	}
	if got := requestsCounted(t.Name(), http.MethodGet, "error"); got != 0 {
		t.Errorf("a call that was never sent counted as a failure, %v times", got)
	}
	if got := durationsObserved(t, t.Name(), http.MethodGet); got != 1 {
		t.Errorf("%d durations observed, want the one request that was sent", got)
	}
}

func TestAPIMetrics_CountACallWithoutAController(t *testing.T) {
	before := requestsCounted("unknown", http.MethodGet, "200")
	client, v7Address := cloudflareAt(t, answerOK)
	if err := getNamespace(context.Background(), client, v7Address); err != nil {
		t.Fatalf("v7: %v", err)
	}
	if got := requestsCounted("unknown", http.MethodGet, "200"); got != before+1 {
		t.Errorf("%v requests counted, want %v", got, before+1)
	}
}

// Dashboards and alerts go by these names and labels, so they are stated here
// rather than read from the variables.
func TestAPIMetrics_AreServedByTheirNames(t *testing.T) {
	client, v7Address := cloudflareAt(t, answerOK)
	if err := getNamespace(WithController(context.Background(), t.Name()), client, v7Address); err != nil {
		t.Fatalf("v7: %v", err)
	}

	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	want := map[string][]string{
		"kflare_cloudflare_api_requests_total":           {"code", "controller", "method"},
		"kflare_cloudflare_api_request_duration_seconds": {"controller", "method"},
	}
	for _, family := range families {
		labels, ok := want[family.GetName()]
		if !ok {
			continue
		}
		delete(want, family.GetName())
		for _, metric := range family.GetMetric() {
			var got []string
			for _, label := range metric.GetLabel() {
				if name := label.GetName(); name != "le" {
					got = append(got, name)
				}
			}
			if !slices.Equal(got, labels) {
				t.Errorf("%s has labels %v, want %v", family.GetName(), got, labels)
			}
		}
	}
	if len(want) != 0 {
		t.Errorf("not served: %v", want)
	}
}

func TestInstrumentedTransport(t *testing.T) {
	failure := errors.New("connection reset")
	for _, c := range []struct {
		name, method string
		status       int
		err          error
		wantCode     string
	}{
		{name: "a read", method: http.MethodGet, status: http.StatusOK, wantCode: "200"},
		{name: "a create", method: http.MethodPost, status: http.StatusCreated, wantCode: "201"},
		{name: "a refusal", method: http.MethodPut, status: http.StatusForbidden, wantCode: "403"},
		{name: "a failure on Cloudflare's side", method: http.MethodDelete, status: 500, wantCode: "500"},
		{name: "no response", method: http.MethodPatch, err: failure, wantCode: "error"},
	} {
		t.Run(c.name, func(t *testing.T) {
			transport := &instrumentedTransport{next: transportFunc(func(req *http.Request) (*http.Response, error) {
				if c.err != nil {
					return nil, c.err
				}
				res := okResponse()
				res.StatusCode, res.Request = c.status, req
				return res, nil
			})}
			req, err := http.NewRequestWithContext(WithController(context.Background(), t.Name()), c.method,
				invalidBaseURL, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			res, err := transport.RoundTrip(req)

			if !errors.Is(err, c.err) {
				t.Errorf("err = %v, want %v", err, c.err)
			}
			if c.err == nil && (res == nil || res.StatusCode != c.status) {
				t.Errorf("res = %v, want status %d passed on", res, c.status)
			}
			if got := requestsCounted(t.Name(), c.method, c.wantCode); got != 1 {
				t.Errorf("%v requests counted, want 1", got)
			}
			if got := durationsObserved(t, t.Name(), c.method); got != 1 {
				t.Errorf("%d durations observed, want 1", got)
			}
		})
	}
}

func TestInstrumentedTransport_LogsErrorResponsesWithTheirRayID(t *testing.T) {
	for _, c := range []struct {
		name      string
		status    int
		verbosity int
		want      []string
	}{
		{name: "a success", status: http.StatusOK},
		{name: "the last status short of an error", status: http.StatusBadRequest - 1},
		{
			name: "the first error status", status: http.StatusBadRequest,
			want: []string{`"cfRay"="ray-id-YYZ"`, `"status"=400`},
		},
		{
			name: "a refusal", status: http.StatusForbidden,
			want: []string{`"cfRay"="ray-id-YYZ"`, `"status"=403`, `"method"="GET"`},
		},
		{
			name: "a failure on Cloudflare's side", status: http.StatusBadGateway,
			want: []string{`"cfRay"="ray-id-YYZ"`, `"status"=502`},
		},
		{name: "a 404, which kflare expects", status: http.StatusNotFound},
		{
			name: "a 404, with more verbosity", status: http.StatusNotFound, verbosity: 1,
			want: []string{`"cfRay"="ray-id-YYZ"`, `"status"=404`},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			var lines []string
			logger := funcr.New(func(_, args string) { lines = append(lines, args) },
				funcr.Options{Verbosity: c.verbosity})
			transport := &instrumentedTransport{next: transportFunc(func(req *http.Request) (*http.Response, error) {
				res := okResponse()
				res.StatusCode, res.Request = c.status, req
				res.Header.Set("Cf-Ray", "ray-id-YYZ")
				return res, nil
			})}
			req, err := http.NewRequestWithContext(log.IntoContext(context.Background(), logger), http.MethodGet,
				invalidBaseURL, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if _, err := transport.RoundTrip(req); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(c.want) == 0 {
				if len(lines) != 0 {
					t.Fatalf("logged %q, want nothing", lines)
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("logged %q, want one line", lines)
			}
			for _, want := range c.want {
				if !strings.Contains(lines[0], want) {
					t.Errorf("logged %q, want %s in it", lines[0], want)
				}
			}
		})
	}
}
