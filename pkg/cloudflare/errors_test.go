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

// newCFError is a test helper that constructs a cloudflare.Error with the
// given ErrorType and StatusCode using cloudflare-go's public constructors.
func newCFError(t ErrorType, statusCode int) *cf.Error {
	return &cf.Error{
		Type:       cf.ErrorType(t),
		StatusCode: statusCode,
	}
}

// ErrorType mirrors cf.ErrorType to keep test helper readable.
type ErrorType = string

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
			name:     "AuthenticationError (401) is terminal",
			err:      cf.NewAuthenticationError(&cf.Error{StatusCode: 401, Type: cf.ErrorTypeAuthentication}),
			terminal: true,
		},
		{
			name:     "AuthorizationError (403) is terminal",
			err:      cf.NewAuthorizationError(&cf.Error{StatusCode: 403, Type: cf.ErrorTypeAuthorization}),
			terminal: true,
		},
		{
			name:     "NotFoundError (404) is terminal",
			err:      cf.NewNotFoundError(&cf.Error{StatusCode: 404, Type: cf.ErrorTypeNotFound}),
			terminal: true,
		},
		{
			name:     "RequestError (400) is terminal",
			err:      cf.NewRequestError(&cf.Error{StatusCode: 400, Type: cf.ErrorTypeRequest}),
			terminal: true,
		},
		{
			name:     "RatelimitError (429) is NOT terminal (retryable)",
			err:      cf.NewRatelimitError(&cf.Error{StatusCode: 429, Type: cf.ErrorTypeRateLimit}),
			terminal: false,
		},
		{
			name:     "ServiceError (500) is NOT terminal (retryable)",
			err:      cf.NewServiceError(&cf.Error{StatusCode: 500, Type: cf.ErrorTypeService}),
			terminal: false,
		},
		{
			name:     "plain Go error is NOT terminal (retryable)",
			err:      errors.New("connection refused"),
			terminal: false,
		},
		{
			name:     "wrapped AuthenticationError is terminal",
			err:      errors.Join(errors.New("outer"), cf.NewAuthenticationError(&cf.Error{StatusCode: 401, Type: cf.ErrorTypeAuthentication})),
			terminal: true,
		},
		{
			name:     "wrapped RequestError is terminal",
			err:      errors.Join(errors.New("outer"), cf.NewRequestError(&cf.Error{StatusCode: 422, Type: cf.ErrorTypeRequest})),
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
		{"NotFoundError", cf.NewNotFoundError(&cf.Error{StatusCode: 404, Type: cf.ErrorTypeNotFound}), true},
		{"AuthenticationError", cf.NewAuthenticationError(&cf.Error{StatusCode: 401, Type: cf.ErrorTypeAuthentication}), false},
		{"RequestError", cf.NewRequestError(&cf.Error{StatusCode: 400, Type: cf.ErrorTypeRequest}), false},
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
		{"RatelimitError", cf.NewRatelimitError(&cf.Error{StatusCode: 429, Type: cf.ErrorTypeRateLimit}), true},
		{"AuthenticationError", cf.NewAuthenticationError(&cf.Error{StatusCode: 401, Type: cf.ErrorTypeAuthentication}), false},
		{"ServiceError", cf.NewServiceError(&cf.Error{StatusCode: 500, Type: cf.ErrorTypeService}), false},
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
