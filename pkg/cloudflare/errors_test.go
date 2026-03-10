/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package cloudflare_test

import (
	"errors"
	"testing"

	cf "github.com/cloudflare/cloudflare-go"

	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
)

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
