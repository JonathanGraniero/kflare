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

// TokenStatus is an API token's status, as Cloudflare's token verification
// reports it.
type TokenStatus string

// The statuses Cloudflare's token verification reports.
const (
	TokenActive   TokenStatus = "active"
	TokenDisabled TokenStatus = "disabled"
	TokenExpired  TokenStatus = "expired"
)

// TokenVerification is what Cloudflare reports about an API token.
type TokenVerification struct {
	// Status is active, disabled or expired.
	Status TokenStatus
	// ExpiresOn is when the token expires; zero for a token that never does.
	ExpiresOn time.Time
}

// VerifyToken asks Cloudflare for the client's own token's status and expiry;
// it needs no permission. Each kind of token has its own endpoint, and
// neither verifies the other's (the user endpoint answers an account-owned
// token with 401, code 1000, checked live 2026-11-09): it asks accountID's
// endpoint first, then the user one. When both refuse, the error is the
// account endpoint's; any other error returns at once. A disabled or expired
// token still verifies (200 with its status), where reading the account with
// it fails with code 9109. A token that never expires has no expiry.
func (c *Client) VerifyToken(ctx context.Context, accountID string) (TokenVerification, error) {
	var params accounts.TokenVerifyParams
	params.AccountID.Value, params.AccountID.Present = accountID, true
	owned, err := accounts.NewTokenService(c.V7Options()...).Verify(ctx, params)
	if err == nil {
		return TokenVerification{Status: TokenStatus(owned.Status), ExpiresOn: owned.ExpiresOn}, nil
	}
	if !IsTerminalError(err) {
		return TokenVerification{}, err
	}
	users, userErr := user.NewTokenService(c.V7Options()...).Verify(ctx)
	if userErr == nil {
		return TokenVerification{Status: TokenStatus(users.Status), ExpiresOn: users.ExpiresOn}, nil
	}
	if !IsTerminalError(userErr) {
		return TokenVerification{}, userErr
	}
	return TokenVerification{}, err
}

// Account reads the account accountID, which the token must be allowed to
// read (Account Settings Read).
func (c *Client) Account(ctx context.Context, accountID string) (*accounts.Account, error) {
	var params accounts.AccountGetParams
	params.AccountID.Value, params.AccountID.Present = accountID, true
	return accounts.NewAccountService(c.V7Options()...).Get(ctx, params)
}
