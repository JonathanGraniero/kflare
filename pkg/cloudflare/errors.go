/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

// Package cloudflare provides a thin wrapper around the cloudflare-go SDKs,
// v0 and v7, and shared error classification utilities for kflare
// controllers.
package cloudflare

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	cf "github.com/cloudflare/cloudflare-go"
	"github.com/cloudflare/cloudflare-go/v7/kv"
)

// v7Error is the error cloudflare-go v7 returns for every response with an
// error status. v7 documents it as cloudflare.Error, and every v7 package
// re-exports it as Error; kv's is used here because kflare does not import
// v7's root package (see Client.V7Options).
type v7Error = kv.Error

// IsTerminalError returns true if err represents a condition that will not
// resolve by retrying — for example, an invalid token, insufficient
// permissions, or a malformed request. Controllers that receive a terminal
// error should set a Terminal condition and stop requeuing.
//
// Retryable errors (rate limits, 5xx service errors, transient network
// failures) return false, allowing the controller-runtime back-off queue to
// retry naturally.
//
// Note: cloudflare-go returns pointer types (*AuthenticationError, etc.) from
// its HTTP layer, so errors.As targets must use pointer types to match.
func IsTerminalError(err error) bool {
	if err == nil {
		return false
	}

	// cloudflare-go v7 returns one error type for every error status: any 4xx
	// but 429 is terminal, as with v0's typed errors below.
	if v7Err, ok := asV7Error(err); ok {
		return v7Err.StatusCode >= 400 && v7Err.StatusCode < 500 &&
			v7Err.StatusCode != http.StatusTooManyRequests
	}

	// HTTP 403 Forbidden — token lacks the required scope/permission.
	// cloudflare-go counterintuitively names this AuthenticationError.
	var authErr *cf.AuthenticationError
	if errors.As(err, &authErr) {
		return true
	}

	// HTTP 401 Unauthorized — bad or revoked token.
	// cloudflare-go counterintuitively names this AuthorizationError.
	var authzErr *cf.AuthorizationError
	if errors.As(err, &authzErr) {
		return true
	}

	// 404 Not Found — the resource does not exist on Cloudflare's side.
	// Controllers should treat this as terminal when fetching a known resource;
	// the create path may recover from it, so callers can check IsNotFound
	// separately when that distinction matters.
	var notFoundErr *cf.NotFoundError
	if errors.As(err, &notFoundErr) {
		return true
	}

	// 4xx RequestError — bad payload, unsupported operation, etc.
	// RatelimitError (429) and ServiceError (5xx) are retryable; all other
	// unknown errors (e.g. network timeouts) are also retryable by default.
	var reqErr *cf.RequestError
	return errors.As(err, &reqErr)
}

// IsNotFound returns true if err is a Cloudflare 404 Not Found response.
// This is useful in reconcilers that need to distinguish "resource missing"
// from other terminal errors (e.g. to create the resource instead of failing).
func IsNotFound(err error) bool {
	if v7Err, ok := asV7Error(err); ok {
		return v7Err.StatusCode == http.StatusNotFound
	}
	var notFoundErr *cf.NotFoundError
	return errors.As(err, &notFoundErr)
}

// IsRateLimit returns true if err is a Cloudflare 429 rate-limit response, or
// a call kflare did not send because of its token's rate limit. A rate limit
// says nothing about the resource being reconciled, so controllers wait for
// it rather than report a failure.
func IsRateLimit(err error) bool {
	var limited *rateLimitedError
	if errors.As(err, &limited) {
		return true
	}
	if v7Err, ok := asV7Error(err); ok {
		return v7Err.StatusCode == http.StatusTooManyRequests
	}
	var rlErr *cf.RatelimitError
	return errors.As(err, &rlErr)
}

// HasErrorCode returns true if err is a Cloudflare 4xx RequestError (any
// client error other than 401, 403, 404 and 429) whose response carries code
// among its error codes. Controllers use it to tell apart specific rejections
// that share an HTTP status, for example an R2 bucket that is not empty
// (409, code 10008).
func HasErrorCode(err error, code int) bool {
	if v7Err, ok := asV7Error(err); ok {
		if !isRequestErrorStatus(v7Err.StatusCode) {
			return false
		}
		for _, e := range v7Err.Errors {
			if e.Code == int64(code) {
				return true
			}
		}
		return false
	}
	var reqErr *cf.RequestError
	return errors.As(err, &reqErr) && reqErr.InternalErrorCodeIs(code)
}

// asV7Error returns the cloudflare-go v7 API error in err's chain, if there is
// one.
func asV7Error(err error) (*v7Error, bool) {
	var v7Err *v7Error
	ok := errors.As(err, &v7Err)
	return v7Err, ok
}

// ErrorMessage returns what a condition says about err. For an API error from
// cloudflare-go v7, that is Cloudflare's messages with their codes, as v0's
// errors read ("namespace not found (10013)"), rather than v7's request line
// and raw response body. Any other error reads as it is.
func ErrorMessage(err error) string {
	v7Err, ok := asV7Error(err)
	if !ok {
		return err.Error()
	}
	messages := make([]string, 0, len(v7Err.Errors))
	for _, e := range v7Err.Errors {
		message := strings.TrimSpace(e.Message)
		if e.Code != 0 {
			message = strings.TrimSpace(fmt.Sprintf("%s (%d)", message, e.Code))
		}
		if message != "" {
			messages = append(messages, message)
		}
	}
	if len(messages) == 0 {
		return fmt.Sprintf("HTTP %d %s", v7Err.StatusCode, http.StatusText(v7Err.StatusCode))
	}
	return strings.Join(messages, ", ")
}

// isRequestErrorStatus reports whether v0 returns a RequestError for status:
// a client error other than 401, 403, 404 and 429.
func isRequestErrorStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests:
		return false
	}
	return status >= 400 && status < 500
}
