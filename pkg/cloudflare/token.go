/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package cloudflare

import (
	"context"
	"time"

	"github.com/cloudflare/cloudflare-go/v7/accounts"
	"github.com/cloudflare/cloudflare-go/v7/user"
)

// The statuses Cloudflare's token verification reports.
const (
	TokenActive   = "active"
	TokenDisabled = "disabled"
	TokenExpired  = "expired"
)

// TokenVerification is what Cloudflare reports about an API token.
type TokenVerification struct {
	// Status is active, disabled or expired (TokenActive, ...).
	Status string
	// ExpiresOn is when the token expires; zero for a token that never does.
	ExpiresOn time.Time
}

// VerifyToken asks Cloudflare about the client's own token: its status and
// when it expires. Verifying a token needs no permission but the token.
//
// Cloudflare has an endpoint for each kind of token, and neither verifies the
// other's: the user endpoint answers an account-owned token with 401, "Invalid
// API Token" (code 1000), checked live on 2026-11-09. So VerifyToken asks the
// endpoint of accountID's account-owned tokens first, and the user endpoint
// when that one refuses the token. When both refuse it, the error is the
// account endpoint's: the token is meant for that account. An error that is
// not a refusal, such as a rate limit or a 5xx, is returned at once, for the
// caller to retry.
//
// A disabled or expired token is still verified: Cloudflare answers with 200
// and the token's status, where reading the account with it gets "Invalid
// access token" (code 9109; 403 for the expired one), checked live on
// 2026-11-09 with an account-owned token of each. A token that never expires
// has no expires_on.
func (c *Client) VerifyToken(ctx context.Context, accountID string) (TokenVerification, error) {
	var params accounts.TokenVerifyParams
	params.AccountID.Value, params.AccountID.Present = accountID, true
	owned, err := accounts.NewTokenService(c.V7Options()...).Verify(ctx, params)
	if err == nil {
		return TokenVerification{Status: string(owned.Status), ExpiresOn: owned.ExpiresOn}, nil
	}
	if !IsTerminalError(err) {
		return TokenVerification{}, err
	}
	users, userErr := user.NewTokenService(c.V7Options()...).Verify(ctx)
	if userErr == nil {
		return TokenVerification{Status: string(users.Status), ExpiresOn: users.ExpiresOn}, nil
	}
	if !IsTerminalError(userErr) {
		return TokenVerification{}, userErr
	}
	return TokenVerification{}, err
}
