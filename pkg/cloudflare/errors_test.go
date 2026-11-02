/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package cloudflare_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cf "github.com/cloudflare/cloudflare-go"
	"github.com/cloudflare/cloudflare-go/v7/kv"
	"github.com/cloudflare/cloudflare-go/v7/option"

	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
)

// getV7Namespace makes one cloudflare-go v7 call with opts, without retries,
// and returns its error.
func getV7Namespace(opts ...option.RequestOption) error {
	var params kv.NamespaceGetParams
	params.AccountID.Value, params.AccountID.Present = "account-id", true
	_, err := kv.NewNamespaceService(append(opts, option.WithMaxRetries(0))...).
		Get(context.Background(), "ns-id", params)
	return err
}

// v7Error returns the error cloudflare-go v7 returns when Cloudflare answers
// with status and these error codes.
func v7Error(t *testing.T, status int, codes ...int) error {
	t.Helper()
	errs := make([]map[string]any, 0, len(codes))
	for _, code := range codes {
		errs = append(errs, map[string]any{"code": code, "message": "rejected"})
	}
	return v7ErrorBody(t, status, mustJSON(t, map[string]any{"result": nil, "success": false, "errors": errs,
		"messages": []any{}}))
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encoding %v: %v", v, err)
	}
	return string(b)
}

// v7ErrorBody returns the error cloudflare-go v7 returns when Cloudflare
// answers with status and body.
func v7ErrorBody(t *testing.T, status int, body string) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()

	err := getV7Namespace(option.WithBaseURL(server.URL))
	var apiErr *kv.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != status {
		t.Fatalf("v7 returned %v, want a *cloudflare.Error with status %d", err, status)
	}
	return err
}

// v7ConnectionError returns the error cloudflare-go v7 returns when nothing
// answers.
func v7ConnectionError(t *testing.T) error {
	t.Helper()
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()

	err := getV7Namespace(option.WithBaseURL(server.URL))
	var apiErr *kv.Error
	if err == nil || errors.As(err, &apiErr) {
		t.Fatalf("v7 returned %v, want a connection error", err)
	}
	return err
}

// ptrAuthN returns a *cf.AuthenticationError as an error, matching what the
// cloudflare-go HTTP layer returns for HTTP 403 responses.
func ptrAuthN(e *cf.Error) error {
	v := cf.NewAuthenticationError(e)
	return &v
}

// ptrAuthZ returns a *cf.AuthorizationError as an error, matching what the
// cloudflare-go HTTP layer returns for HTTP 401 responses.
func ptrAuthZ(e *cf.Error) error {
	v := cf.NewAuthorizationError(e)
	return &v
}

// ptrNotFound returns a *cf.NotFoundError as an error.
func ptrNotFound(e *cf.Error) error {
	v := cf.NewNotFoundError(e)
	return &v
}

// ptrRequest returns a *cf.RequestError as an error.
func ptrRequest(e *cf.Error) error {
	v := cf.NewRequestError(e)
	return &v
}

// ptrRateLimit returns a *cf.RatelimitError as an error.
func ptrRateLimit(e *cf.Error) error {
	v := cf.NewRatelimitError(e)
	return &v
}

// ptrService returns a *cf.ServiceError as an error.
func ptrService(e *cf.Error) error {
	v := cf.NewServiceError(e)
	return &v
}

func TestIsTerminalError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		terminal bool
	}{
		{
			name:     "nil error is not terminal",
			err:      nil,
			terminal: false,
		},
		{
			// cloudflare-go returns *AuthenticationError for HTTP 403 Forbidden
			// (insufficient token permissions).
			name:     "AuthenticationError (403) is terminal",
			err:      ptrAuthN(&cf.Error{StatusCode: 403, Type: cf.ErrorTypeAuthentication}),
			terminal: true,
		},
		{
			// cloudflare-go returns *AuthorizationError for HTTP 401 Unauthorized
			// (bad or revoked token).
			name:     "AuthorizationError (401) is terminal",
			err:      ptrAuthZ(&cf.Error{StatusCode: 401, Type: cf.ErrorTypeAuthorization}),
			terminal: true,
		},
		{
			name:     "NotFoundError (404) is terminal",
			err:      ptrNotFound(&cf.Error{StatusCode: 404, Type: cf.ErrorTypeNotFound}),
			terminal: true,
		},
		{
			name:     "RequestError (400) is terminal",
			err:      ptrRequest(&cf.Error{StatusCode: 400, Type: cf.ErrorTypeRequest}),
			terminal: true,
		},
		{
			name:     "RatelimitError (429) is NOT terminal (retryable)",
			err:      ptrRateLimit(&cf.Error{StatusCode: 429, Type: cf.ErrorTypeRateLimit}),
			terminal: false,
		},
		{
			name:     "ServiceError (500) is NOT terminal (retryable)",
			err:      ptrService(&cf.Error{StatusCode: 500, Type: cf.ErrorTypeService}),
			terminal: false,
		},
		{
			name:     "plain Go error is NOT terminal (retryable)",
			err:      errors.New("connection refused"),
			terminal: false,
		},
		{
			name:     "wrapped AuthenticationError is terminal",
			err:      errors.Join(errors.New("outer"), ptrAuthN(&cf.Error{StatusCode: 403, Type: cf.ErrorTypeAuthentication})),
			terminal: true,
		},
		{
			name:     "wrapped RequestError is terminal",
			err:      errors.Join(errors.New("outer"), ptrRequest(&cf.Error{StatusCode: 422, Type: cf.ErrorTypeRequest})),
			terminal: true,
		},
		// cloudflare-go v7 returns a *cloudflare.Error for every error status.
		{name: "v7 401 is terminal", err: v7Error(t, 401, 10000), terminal: true},
		{name: "v7 403 is terminal", err: v7Error(t, 403, 10000), terminal: true},
		{name: "v7 404 is terminal", err: v7Error(t, 404, 10013), terminal: true},
		{name: "v7 400 is terminal", err: v7Error(t, 400, 10014), terminal: true},
		{name: "v7 409 is terminal", err: v7Error(t, 409, 10008), terminal: true},
		{name: "v7 429 is NOT terminal (retryable)", err: v7Error(t, 429), terminal: false},
		{name: "v7 500 is NOT terminal (retryable)", err: v7Error(t, 500), terminal: false},
		{name: "v7 connection error is NOT terminal (retryable)", err: v7ConnectionError(t), terminal: false},
		{name: "wrapped v7 403 is terminal", err: fmt.Errorf("outer: %w", v7Error(t, 403)), terminal: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cfpkg.IsTerminalError(tc.err)
			if got != tc.terminal {
				t.Errorf("IsTerminalError(%v) = %v, want %v", tc.err, got, tc.terminal)
			}
		})
	}
}

func TestIsNotFound(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		found bool
	}{
		{"nil", nil, false},
		{"NotFoundError", ptrNotFound(&cf.Error{StatusCode: 404, Type: cf.ErrorTypeNotFound}), true},
		{"AuthenticationError", ptrAuthN(&cf.Error{StatusCode: 403, Type: cf.ErrorTypeAuthentication}), false},
		{"RequestError", ptrRequest(&cf.Error{StatusCode: 400, Type: cf.ErrorTypeRequest}), false},
		{"plain error", errors.New("not found"), false},
		{"v7 404", v7Error(t, 404, 10013), true},
		{"wrapped v7 404", fmt.Errorf("outer: %w", v7Error(t, 404, 10013)), true},
		{"v7 403", v7Error(t, 403, 10000), false},
		{"v7 400", v7Error(t, 400, 10014), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cfpkg.IsNotFound(tc.err)
			if got != tc.found {
				t.Errorf("IsNotFound(%v) = %v, want %v", tc.err, got, tc.found)
			}
		})
	}
}

func TestIsRateLimit(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		rateLimit bool
	}{
		{"nil", nil, false},
		{"RatelimitError", ptrRateLimit(&cf.Error{StatusCode: 429, Type: cf.ErrorTypeRateLimit}), true},
		{"AuthenticationError", ptrAuthN(&cf.Error{StatusCode: 403, Type: cf.ErrorTypeAuthentication}), false},
		{"ServiceError", ptrService(&cf.Error{StatusCode: 500, Type: cf.ErrorTypeService}), false},
		{"plain error", errors.New("too many requests"), false},
		{"v7 429", v7Error(t, 429), true},
		{"v7 403", v7Error(t, 403, 10000), false},
		{"v7 503", v7Error(t, 503), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cfpkg.IsRateLimit(tc.err)
			if got != tc.rateLimit {
				t.Errorf("IsRateLimit(%v) = %v, want %v", tc.err, got, tc.rateLimit)
			}
		})
	}
}

func TestHasErrorCode(t *testing.T) {
	// The shape cloudflare-go returns for deleting an R2 bucket that still
	// holds objects (verified live): HTTP 409 with code 10008.
	notEmpty := &cf.Error{StatusCode: 409, Type: cf.ErrorTypeRequest, ErrorCodes: []int{10008}}

	tests := []struct {
		name string
		err  error
		has  bool
	}{
		{"nil", nil, false},
		{"RequestError with the code", ptrRequest(notEmpty), true},
		{"wrapped RequestError with the code", errors.Join(errors.New("outer"), ptrRequest(notEmpty)), true},
		{"RequestError with another code", ptrRequest(&cf.Error{StatusCode: 400, ErrorCodes: []int{10005}}), false},
		{"RequestError without codes", ptrRequest(&cf.Error{StatusCode: 400}), false},
		{"NotFoundError with the code", ptrNotFound(&cf.Error{StatusCode: 404, ErrorCodes: []int{10008}}), false},
		{"plain error", errors.New("code 10008"), false},
		{"v7 409 with the code", v7Error(t, 409, 10008), true},
		{"wrapped v7 409 with the code", errors.Join(errors.New("outer"), v7Error(t, 409, 10008)), true},
		{"v7 400 with the code among others", v7Error(t, 400, 10005, 10008), true},
		{"v7 400 with another code", v7Error(t, 400, 10005), false},
		{"v7 400 without codes", v7Error(t, 400), false},
		{"v7 403 with the code", v7Error(t, 403, 10008), false},
		{"v7 404 with the code", v7Error(t, 404, 10008), false},
		{"v7 429 with the code", v7Error(t, 429, 10008), false},
		{"v7 500 with the code", v7Error(t, 500, 10008), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cfpkg.HasErrorCode(tc.err, 10008)
			if got != tc.has {
				t.Errorf("HasErrorCode(%v, 10008) = %v, want %v", tc.err, got, tc.has)
			}
		})
	}
}

func TestErrorMessage(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"v7 error with a code", v7Error(t, 404, 10013), "rejected (10013)"},
		{"v7 error with two codes", v7Error(t, 400, 10005, 10008), "rejected (10005), rejected (10008)"},
		{"v7 error without codes", v7Error(t, 503), "HTTP 503 Service Unavailable"},
		{"v7 error whose only entry is empty", v7ErrorBody(t, 502,
			`{"result":null,"success":false,"errors":[{"code":0,"message":""}],"messages":[]}`), "HTTP 502 Bad Gateway"},
		{"v7 error whose only message is blank", v7ErrorBody(t, 403,
			`{"result":null,"success":false,"errors":[{"code":0,"message":" \n"}],"messages":[]}`), "HTTP 403 Forbidden"},
		{"v7 error with a code and a blank message", v7ErrorBody(t, 400,
			`{"result":null,"success":false,"errors":[{"code":10014,"message":" "}],"messages":[]}`), "(10014)"},
		{"v7 error whose message is padded", v7ErrorBody(t, 404,
			`{"result":null,"success":false,"errors":[{"code":0,"message":" not found\n"}],"messages":[]}`), "not found"},
		{"wrapped v7 error", fmt.Errorf("outer: %w", v7Error(t, 404, 10013)), "rejected (10013)"},
		{"v0 error", ptrRequest(&cf.Error{StatusCode: 400, ErrorCodes: []int{10014},
			Errors: []cf.ResponseInfo{{Code: 10014, Message: "title already exists"}}}), "title already exists (10014)"},
		{"plain error", errors.New("connection refused"), "connection refused"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := cfpkg.ErrorMessage(tc.err); got != tc.want {
				t.Errorf("ErrorMessage(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// v0Answer returns the error cloudflare-go v0 returns when Cloudflare answers
// with status, without retries.
func v0Answer(t *testing.T, status int, header http.Header) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		maps.Copy(w.Header(), header)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"result":null,"success":false,"errors":[{"code":10000,"message":"no"}],"messages":[]}`)
	}))
	defer server.Close()
	api, err := cf.NewWithAPIToken("token", cf.BaseURL(server.URL), cf.UsingRetryPolicy(0, 0, 0))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, _, err = api.Account(context.Background(), "account-id")
	return err
}

// v7Answered is v7ErrorBody with the response's headers.
func v7Answered(t *testing.T, status int, header http.Header) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		maps.Copy(w.Header(), header)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"result":null,"success":false,"errors":[{"code":10000,"message":"no"}],"messages":[]}`)
	}))
	defer server.Close()
	return getV7Namespace(option.WithBaseURL(server.URL))
}

func TestRayID(t *testing.T) {
	const ray = "8f2b7c9d1a2e3f40-YYZ"
	withRay := http.Header{"Cf-Ray": {ray}}

	// Every status the SDKs tell apart.
	for _, status := range []int{400, 401, 403, 404, 409, 429, 500, 503} {
		t.Run(fmt.Sprintf("v0's %d", status), func(t *testing.T) {
			err := v0Answer(t, status, withRay)
			want := ray
			if status == http.StatusTooManyRequests || status >= http.StatusInternalServerError {
				// v0 reports these with a plain error once its retries run out.
				want = ""
			}
			if got := cfpkg.RayID(err); got != want {
				t.Errorf("RayID(%v) = %q, want %q", err, got, want)
			}
			if got := cfpkg.RayID(fmt.Errorf("reading the account: %w", err)); got != want {
				t.Errorf("wrapped: RayID = %q, want %q", got, want)
			}
			if got := cfpkg.RayID(v0Answer(t, status, nil)); got != "" {
				t.Errorf("no header: RayID = %q, want none", got)
			}
		})
		t.Run(fmt.Sprintf("v7's %d", status), func(t *testing.T) {
			err := v7Answered(t, status, withRay)
			if got := cfpkg.RayID(err); got != ray {
				t.Errorf("RayID(%v) = %q, want %q", err, got, ray)
			}
			if got := cfpkg.RayID(fmt.Errorf("reading the namespace: %w", err)); got != ray {
				t.Errorf("wrapped: RayID = %q, want %q", got, ray)
			}
			if got := cfpkg.RayID(v7Answered(t, status, nil)); got != "" {
				t.Errorf("no header: RayID = %q, want none", got)
			}
		})
	}

	t.Run("a v0 error that keeps the ID, whatever its type", func(t *testing.T) {
		for _, err := range []error{
			ptrAuthN(&cf.Error{RayID: ray}), ptrAuthZ(&cf.Error{RayID: ray}), ptrNotFound(&cf.Error{RayID: ray}),
			ptrRequest(&cf.Error{RayID: ray}), ptrRateLimit(&cf.Error{RayID: ray}), ptrService(&cf.Error{RayID: ray}),
		} {
			if got := cfpkg.RayID(err); got != ray {
				t.Errorf("RayID(%T) = %q, want %q", err, got, ray)
			}
		}
	})

	for name, err := range map[string]error{
		"no error":         nil,
		"another error":    errors.New("boom"),
		"no response":      v7ConnectionError(t),
		"a v7 error alone": &kv.Error{StatusCode: http.StatusInternalServerError},
	} {
		t.Run(name, func(t *testing.T) {
			if got := cfpkg.RayID(err); got != "" {
				t.Errorf("RayID(%v) = %q, want none", err, got)
			}
		})
	}
}

// v7Answer returns the error cloudflare-go v7 returns when a response has
// status and body, answered without a server so that a fuzzer can send many.
func v7Answer(status int, body []byte) error {
	answer := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    req,
		}, nil
	})
	return getV7Namespace(option.WithBaseURL("https://api.cloudflare.com/client/v4/"),
		option.WithHTTPClient(&http.Client{Transport: answer}))
}

// FuzzV7ErrorClassification classifies the error cloudflare-go v7 returns for
// a response with any status and body, as Cloudflare, or a proxy in front of
// it, may send. Whatever the response, the controllers' reading of the error
// must hold together:
//   - ErrorMessage is never blank, since the Ready condition shows it
//   - a rate limit is never terminal, so that it is retried
//   - a not-found, and an error HasErrorCode matches by any of its codes, are
//     always terminal, as the controllers that check for them expect
func FuzzV7ErrorClassification(f *testing.F) {
	envelope := func(errs string) []byte {
		return []byte(`{"result":null,"success":false,"errors":` + errs + `,"messages":[]}`)
	}
	f.Add(404, envelope(`[{"code":10013,"message":"namespace not found"}]`))
	f.Add(409, envelope(`[{"code":10008,"message":"The bucket you tried to delete is not empty"}]`))
	f.Add(400, envelope(`[{"code":10005,"message":"rejected"},{"code":10008,"message":"rejected"}]`))
	f.Add(403, envelope(`[{"code":10000,"message":"Authentication error"}]`))
	f.Add(429, envelope(`[{"code":971,"message":"Please wait and consider throttling your request speed"}]`))
	f.Add(429, envelope(`[{"code":10008,"message":"rejected"}]`))
	f.Add(502, envelope(`[{"code":0,"message":""}]`))
	f.Add(500, envelope(`[]`))
	f.Add(503, []byte("<html><body>Service Unavailable</body></html>"))
	f.Add(401, []byte{})
	f.Add(200, []byte(`{"result":{"id":"ns-id","title":"t"},"success":true,"errors":[],"messages":[]}`))

	f.Fuzz(func(t *testing.T, status int, body []byte) {
		if status < 100 || status > 999 {
			t.Skip("not an HTTP status")
		}
		err := v7Answer(status, body)
		if err == nil {
			return // a success v7 could read
		}
		if message := cfpkg.ErrorMessage(err); strings.TrimSpace(message) == "" {
			t.Errorf("ErrorMessage(%v) = %q: the Ready condition would say nothing", err, message)
		}
		terminal := cfpkg.IsTerminalError(err)
		if cfpkg.IsRateLimit(err) && terminal {
			t.Errorf("IsTerminalError(%v) = true for a rate limit, which must be retried", err)
		}
		if cfpkg.IsNotFound(err) && !terminal {
			t.Errorf("IsTerminalError(%v) = false for a not-found", err)
		}
		var apiErr *kv.Error
		if errors.As(err, &apiErr) {
			for _, e := range apiErr.Errors {
				if cfpkg.HasErrorCode(err, int(e.Code)) && !terminal {
					t.Errorf("IsTerminalError(%v) = false for an error HasErrorCode matches by %d", err, e.Code)
				}
			}
		}
	})
}
