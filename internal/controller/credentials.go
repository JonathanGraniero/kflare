/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
)

// defaultTokenKey is the Secret key read when TokenSecretRef.Key is empty.
const defaultTokenKey = "CF_API_TOKEN"

// credentialsError explains why an account's API token could not be resolved.
// Reason is a CamelCase condition reason suitable for a Ready=False condition.
type credentialsError struct {
	Reason  string
	Message string
}

func (e *credentialsError) Error() string { return e.Message }

// resolveAccountToken fetches the cluster-scoped CloudflareAccount named
// accountName and the API token from the Secret it references.
//
// When requireReady is true, an account without a Ready=True condition is
// rejected. Deletion paths pass false so that cleanup can proceed while the
// account is being re-validated.
//
// The error is a concrete *credentialsError so callers can read its Reason
// directly; it is nil on success.
func resolveAccountToken(
	ctx context.Context,
	c client.Reader,
	accountName string,
	requireReady bool,
) (*cloudflarev1alpha1.CloudflareAccount, string, *credentialsError) {
	account := &cloudflarev1alpha1.CloudflareAccount{}
	if err := c.Get(ctx, types.NamespacedName{Name: accountName}, account); err != nil {
		return nil, "", &credentialsError{
			Reason:  "AccountNotFound",
			Message: fmt.Sprintf("CloudflareAccount %q not found: %v", accountName, err),
		}
	}

	if requireReady && !isAccountReady(account) {
		return nil, "", &credentialsError{
			Reason:  "AccountNotReady",
			Message: fmt.Sprintf("CloudflareAccount %q is not ready", accountName),
		}
	}

	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Name:      account.Spec.TokenSecretRef.Name,
		Namespace: account.Spec.TokenSecretRef.Namespace,
	}
	if err := c.Get(ctx, secretKey, secret); err != nil {
		return nil, "", &credentialsError{
			Reason:  "SecretNotFound",
			Message: fmt.Sprintf("Secret %s/%s not found: %v", secretKey.Namespace, secretKey.Name, err),
		}
	}

	tokenKey := account.Spec.TokenSecretRef.Key
	if tokenKey == "" {
		tokenKey = defaultTokenKey
	}
	token, ok := secret.Data[tokenKey]
	if !ok {
		return nil, "", &credentialsError{
			Reason:  "TokenKeyMissing",
			Message: fmt.Sprintf("Key %q not found in secret %s/%s", tokenKey, secretKey.Namespace, secretKey.Name),
		}
	}

	return account, string(token), nil
}
