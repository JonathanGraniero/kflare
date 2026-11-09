/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package cloudflare

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The bodies Cloudflare answered the token verification endpoints with on
// 2026-11-09, IDs replaced.
const (
	verifiedWithExpiry = `{"result":{"id":"t1","status":"active","not_before":"2026-03-05T00:00:00Z",` +
		`"expires_on":"2027-03-07T23:59:59Z"},"success":true,"errors":[],"messages":[{"code":10000,` +
		`"message":"This API Token is valid and active","type":null}]}`
	verifiedWithoutExpiry = `{"result":{"id":"t2","status":"active"},"success":true,"errors":[],` +
		`"messages":[{"code":10000,"message":"This API Token is valid and active","type":null}]}`
	verifiedDisabled = `{"result":{"id":"t2","status":"disabled"},"success":true,"errors":[],` +
		`"messages":[{"code":10001,"message":"This API Token was disabled","type":null}]}`
	verifiedExpired = `{"result":{"id":"t1","status":"expired","expires_on":"2026-11-09T20:07:05Z"},` +
		`"success":true,"errors":[],"messages":[{"code":10001,"message":"This API Token was expired",` +
		`"type":null},{"code":10003,"message":"This API Token was expired at 2026-11-09 20:07:05+00",` +
		`"type":null}]}`
	invalidAPIToken = `{"success":false,"errors":[{"code":1000,"message":"Invalid API Token"}],` +
		`"messages":[],"result":null}`
	serverError = `{"success":false,"errors":[{"code":10000,"message":"Internal error"}],` +
		`"messages":[],"result":null}`
)

const (
	accountVerifyPath = "/client/v4/accounts/acct-1/tokens/verify"
	userVerifyPath    = "/client/v4/user/tokens/verify"
)

// verifyServer answers each verification path with its status and body, and
// records the paths it was asked, in order: every attempt, so v7's two
// retries after a 5xx show as three requests.
type verifyServer struct {
	answers map[string]answer
	asked   []string
}

type answer struct {
	status int
	body   string
}

func (s *verifyServer) client(t *testing.T) *Client {
	t.Helper()
	transport := transportFunc(func(req *http.Request) (*http.Response, error) {
		s.asked = append(s.asked, req.URL.Path)
		if got := req.Header.Get("Authorization"); got != "Bearer test-token-value" {
			t.Errorf("%s was sent Authorization %q, not the client's token", req.URL.Path, got)
		}
		a, ok := s.answers[req.URL.Path]
		if !ok {
			t.Errorf("unexpected request %s %s", req.Method, req.URL)
			a = answer{http.StatusNotFound, invalidAPIToken}
		}
		return &http.Response{
			StatusCode: a.status,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(a.body)),
			Request:    req,
		}, nil
	})
	// The rate limit's transport, as in production, which keeps v7 from
	// retrying a 429.
	hc := withRateLimit(&http.Client{Transport: transport}, newRateLimiter(DefaultRateLimit))
	client, err := newClient("test-token-value", hc)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestVerifyToken(t *testing.T) {
	expiry := time.Date(2027, 3, 7, 23, 59, 59, 0, time.UTC)
	expired := time.Date(2026, 11, 9, 20, 7, 5, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		answers map[string]answer
		want    TokenVerification
		asked   []string
	}{{
		name:    "an account-owned token with an expiry",
		answers: map[string]answer{accountVerifyPath: {http.StatusOK, verifiedWithExpiry}},
		want:    TokenVerification{Status: TokenActive, ExpiresOn: expiry},
		asked:   []string{accountVerifyPath},
	}, {
		name:    "an account-owned token that never expires",
		answers: map[string]answer{accountVerifyPath: {http.StatusOK, verifiedWithoutExpiry}},
		want:    TokenVerification{Status: TokenActive},
		asked:   []string{accountVerifyPath},
	}, {
		name:    "a disabled token, which Cloudflare still verifies",
		answers: map[string]answer{accountVerifyPath: {http.StatusOK, verifiedDisabled}},
		want:    TokenVerification{Status: TokenDisabled},
		asked:   []string{accountVerifyPath},
	}, {
		name:    "an expired token, which Cloudflare still verifies",
		answers: map[string]answer{accountVerifyPath: {http.StatusOK, verifiedExpired}},
		want:    TokenVerification{Status: TokenExpired, ExpiresOn: expired},
		asked:   []string{accountVerifyPath},
	}, {
		name: "a user token, which the account endpoint refuses",
		answers: map[string]answer{
			accountVerifyPath: {http.StatusUnauthorized, invalidAPIToken},
			userVerifyPath:    {http.StatusOK, verifiedWithExpiry},
		},
		want:  TokenVerification{Status: TokenActive, ExpiresOn: expiry},
		asked: []string{accountVerifyPath, userVerifyPath},
	}, {
		name: "a user token refused with 403 by the account endpoint",
		answers: map[string]answer{
			accountVerifyPath: {http.StatusForbidden, invalidAPIToken},
			userVerifyPath:    {http.StatusOK, verifiedDisabled},
		},
		want:  TokenVerification{Status: TokenDisabled},
		asked: []string{accountVerifyPath, userVerifyPath},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			server := &verifyServer{answers: tc.answers}
			got, err := server.client(t).VerifyToken(context.Background(), "acct-1")
			if err != nil {
				t.Fatalf("VerifyToken: %v", err)
			}
			if got.Status != tc.want.Status || !got.ExpiresOn.Equal(tc.want.ExpiresOn) {
				t.Errorf("VerifyToken = %+v, want %+v", got, tc.want)
			}
			if strings.Join(server.asked, " ") != strings.Join(tc.asked, " ") {
				t.Errorf("asked %v, want %v", server.asked, tc.asked)
			}
		})
	}
}

func TestVerifyToken_Errors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers map[string]answer
		// terminal and rateLimit say how the error must classify, and
		// message what ErrorMessage must hold.
		terminal, rateLimit bool
		message             string
		asked               []string
	}{{
		name: "a token both endpoints refuse: the account endpoint's error",
		answers: map[string]answer{
			accountVerifyPath: {http.StatusUnauthorized, invalidAPIToken},
			userVerifyPath: {http.StatusForbidden,
				`{"success":false,"errors":[{"code":9109,"message":"Invalid access token"}],"result":null}`},
		},
		terminal: true,
		message:  "Invalid API Token (1000)",
		asked:    []string{accountVerifyPath, userVerifyPath},
	}, {
		name:      "a rate limit on the account endpoint, returned without asking the user endpoint",
		answers:   map[string]answer{accountVerifyPath: {http.StatusTooManyRequests, invalidAPIToken}},
		rateLimit: true,
		asked:     []string{accountVerifyPath},
	}, {
		name: "a server error on the account endpoint, returned after v7's retries",
		answers: map[string]answer{
			accountVerifyPath: {http.StatusInternalServerError, serverError},
		},
		message: "Internal error",
		asked:   []string{accountVerifyPath, accountVerifyPath, accountVerifyPath},
	}, {
		name: "a server error on the user endpoint after a refusal: the error to retry",
		answers: map[string]answer{
			accountVerifyPath: {http.StatusUnauthorized, invalidAPIToken},
			userVerifyPath:    {http.StatusInternalServerError, serverError},
		},
		message: "Internal error",
		asked:   []string{accountVerifyPath, userVerifyPath, userVerifyPath, userVerifyPath},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			server := &verifyServer{answers: tc.answers}
			got, err := server.client(t).VerifyToken(context.Background(), "acct-1")
			if err == nil {
				t.Fatalf("VerifyToken = %+v, want an error", got)
			}
			if got != (TokenVerification{}) {
				t.Errorf("VerifyToken returned %+v with its error, want nothing", got)
			}
			if IsTerminalError(err) != tc.terminal || IsRateLimit(err) != tc.rateLimit {
				t.Errorf("error %v: terminal %t, rate limit %t; want %t, %t", err, IsTerminalError(err),
					IsRateLimit(err), tc.terminal, tc.rateLimit)
			}
			if !strings.Contains(ErrorMessage(err), tc.message) {
				t.Errorf("ErrorMessage = %q, want it to hold %q", ErrorMessage(err), tc.message)
			}
			if strings.Join(server.asked, " ") != strings.Join(tc.asked, " ") {
				t.Errorf("asked %v, want %v", server.asked, tc.asked)
			}
		})
	}
}
