/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

// Package cloudflare provides a thin wrapper around the cloudflare-go SDK
// and shared error classification utilities for kflare controllers.
package cloudflare

import (
	"errors"

	cf "github.com/cloudflare/cloudflare-go"
)

// IsTerminalError returns true if err represents a condition that will not
// resolve by retrying — for example, an invalid token, insufficient
// permissions, or a malformed request. Controllers that receive a terminal
// error should set a Terminal condition and stop requeuing.
//
// Retryable errors (rate limits, 5xx service errors, transient network
// failures) return false, allowing the controller-runtime back-off queue to
// retry naturally.
func IsTerminalError(err error) bool {
	if err == nil {
		return false
	}

	// 401 Unauthorized — bad or revoked token.
	var authErr cf.AuthenticationError
	if errors.As(err, &authErr) {
		return true
	}

	// 403 Forbidden — token exists but lacks the required scope.
	var authzErr cf.AuthorizationError
	if errors.As(err, &authzErr) {
		return true
	}

	// 404 Not Found — the resource does not exist on Cloudflare's side.
	// Controllers should treat this as terminal when fetching a known resource;
	// the create path may recover from it, so callers can check IsNotFound
	// separately when that distinction matters.
	var notFoundErr cf.NotFoundError
	if errors.As(err, &notFoundErr) {
		return true
	}

	// 4xx RequestError — bad payload, unsupported operation, etc.
	var reqErr cf.RequestError
	if errors.As(err, &reqErr) {
		return true
	}

	// RatelimitError (429) and ServiceError (5xx) are retryable; all other
	// unknown errors (e.g. network timeouts) are also retryable by default.
	return false
}

// IsNotFound returns true if err is a Cloudflare 404 Not Found response.
// This is useful in reconcilers that need to distinguish "resource missing"
// from other terminal errors (e.g. to create the resource instead of failing).
func IsNotFound(err error) bool {
	var notFoundErr cf.NotFoundError
	return errors.As(err, &notFoundErr)
}

// IsRateLimit returns true if err is a Cloudflare 429 rate-limit response.
// Controllers can inspect this to apply custom back-off logic, though
// controller-runtime's default exponential back-off is usually sufficient.
func IsRateLimit(err error) bool {
	var rlErr cf.RatelimitError
	return errors.As(err, &rlErr)
}
