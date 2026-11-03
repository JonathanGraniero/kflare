/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package cloudflare

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	// codeNoResponse is the code label of a request that got no response.
	codeNoResponse = "error"

	// controllerUnknown is the controller label of a request whose context
	// names none (WithController).
	controllerUnknown = "unknown"
)

var (
	apiRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kflare_cloudflare_api_requests_total",
		Help: "Requests sent to Cloudflare's API, each retry among them, by controller, HTTP method and response " +
			"status, or error for a request that got no response.",
	}, []string{"controller", "method", "code"})

	apiRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "kflare_cloudflare_api_request_duration_seconds",
		Help: "How long requests to Cloudflare's API took to get a response's headers, or to fail, by controller " +
			"and HTTP method. Time waiting for the rate limit does not count.",
		// From a quick read to the minute a response may take to start.
		Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60},
	}, []string{"controller", "method"})
)

func init() {
	metrics.Registry.MustRegister(apiRequests, apiRequestDuration)
}

// controllerKey is the context key of the controller making a request.
type controllerKey struct{}

// WithController returns ctx for a reconcile of the controller name, whose
// requests the API metrics count under that name. Both SDKs send a call's
// context along with its requests, so no caller has to pass anything else.
func WithController(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, controllerKey{}, name)
}

// controllerOf returns the controller WithController put in ctx.
func controllerOf(ctx context.Context) string {
	if name, ok := ctx.Value(controllerKey{}).(string); ok {
		return name
	}
	return controllerUnknown
}

// instrumentedTransport measures each request it sends. No label holds a
// path, so an ID in a URL cannot add to the number of series. A response with
// an error status is logged with its cf-ray ID, here because this is the one
// place that sees every attempt, for both SDKs, and those of a deletion, whose
// errors reach no condition.
type instrumentedTransport struct {
	next http.RoundTripper
}

func (t *instrumentedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	controller := controllerOf(req.Context())
	start := time.Now()
	res, err := t.next.RoundTrip(req)
	apiRequestDuration.WithLabelValues(controller, req.Method).Observe(time.Since(start).Seconds())
	if err != nil {
		apiRequests.WithLabelValues(controller, req.Method, codeNoResponse).Inc()
		return nil, err
	}
	apiRequests.WithLabelValues(controller, req.Method, strconv.Itoa(res.StatusCode)).Inc()
	logFailure(req, res)
	return res, nil
}

// logFailure logs a response with an error status. A 404 only with more
// verbosity: kflare expects one whenever it reads an object to learn that it
// is gone.
func logFailure(req *http.Request, res *http.Response) {
	if res.StatusCode < http.StatusBadRequest {
		return
	}
	logger := log.FromContext(req.Context())
	if res.StatusCode == http.StatusNotFound {
		logger = logger.V(1)
	}
	logger.Info("Cloudflare answered with an error", "method", req.Method, "status", res.StatusCode,
		"cfRay", res.Header.Get("Cf-Ray"))
}
