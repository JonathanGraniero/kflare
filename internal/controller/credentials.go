/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
)

// defaultTokenKey is the Secret key read when TokenSecretRef.Key is empty.
const defaultTokenKey = "CF_API_TOKEN"

// reasonAccountNotAllowed reports a resource in a namespace its
// CloudflareAccount's spec.allowedNamespaces leaves out.
const reasonAccountNotAllowed = "AccountNotAllowed"

//+kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// resolveAccountToken returns the CloudflareAccount accountName and its API
// token, for a resource in namespace. It is the only way to a token, so the
// account's spec.allowedNamespaces and the installation's scope hold for every
// kind; a resource using a Zone in another namespace passes the Zone's
// namespace. requireReady refuses an account that is not Ready; deletions pass
// false, so cleanup goes on while the account is re-validated.
func resolveAccountToken(
	ctx context.Context,
	c client.Reader,
	namespace, accountName string,
	requireReady bool,
) (*cloudflarev1alpha1.CloudflareAccount, string, *conditionError) {
	account := &cloudflarev1alpha1.CloudflareAccount{}
	if err := c.Get(ctx, types.NamespacedName{Name: accountName}, account); err != nil {
		return nil, "", &conditionError{
			Reason:  "AccountNotFound",
			Message: fmt.Sprintf("CloudflareAccount %q not found: %v", accountName, err),
		}
	}

	if why := watchScopeFrom(ctx).accountOutside(account); why != "" {
		return nil, "", notWatched(fmt.Sprintf("CloudflareAccount %q", accountName), why)
	}

	if condErr := checkNamespaceAllowed(ctx, c, account, namespace); condErr != nil {
		return nil, "", condErr
	}

	if requireReady && !isReady(account) {
		return nil, "", &conditionError{
			Reason:  "AccountNotReady",
			Message: fmt.Sprintf("CloudflareAccount %q is not ready", accountName),
		}
	}

	token, credErr := accountToken(ctx, c, account)
	if credErr != nil {
		return nil, "", credErr
	}
	return account, token, nil
}

// checkNamespaceAllowed returns why account does not allow a resource in
// namespace, nil if it does. It fails closed: a selector it cannot read, or a
// namespace it cannot read, allows nothing.
func checkNamespaceAllowed(
	ctx context.Context,
	c client.Reader,
	account *cloudflarev1alpha1.CloudflareAccount,
	namespace string,
) *conditionError {
	if !restrictsNamespaces(account) {
		return nil
	}
	notAllowed := func(why string) *conditionError {
		return &conditionError{
			Reason:  reasonAccountNotAllowed,
			Message: fmt.Sprintf("Namespace %q may not use CloudflareAccount %q: %s", namespace, account.Name, why),
		}
	}
	selector, err := metav1.LabelSelectorAsSelector(account.Spec.AllowedNamespaces.Selector)
	if err != nil {
		return notAllowed(fmt.Sprintf("its spec.allowedNamespaces.selector is invalid: %v", err))
	}
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, types.NamespacedName{Name: namespace}, ns); err != nil {
		return notAllowed(fmt.Sprintf("the namespace could not be read: %v", err))
	}
	if !selector.Matches(labels.Set(ns.Labels)) {
		return notAllowed("its spec.allowedNamespaces.selector does not match the namespace's labels")
	}
	return nil
}

// restrictsNamespaces reports whether account leaves any namespace out. Only
// the value that says so outright opens it up to all of them.
func restrictsNamespaces(account *cloudflarev1alpha1.CloudflareAccount) bool {
	allowed := account.Spec.AllowedNamespaces
	return allowed != nil && allowed.From != cloudflarev1alpha1.NamespacesFromAll
}

// leaveCloudflareObject handles a deletion whose credentials could not be
// resolved. When the token is no longer the resource's to use (an account
// that no longer allows its namespace, an account or parent outside the
// installation's scope, a shared Zone that no longer allows it), the resource
// goes without a call, and a Warning Event says its Cloudflare object stays:
// holding the finalizer would leave its namespace stuck terminating. Anything
// else is returned for a retry.
func leaveCloudflareObject(ctx context.Context, obj conditionedObject, credErr *conditionError) error {
	if credErr.Reason != reasonAccountNotAllowed && credErr.Reason != reasonNotWatched &&
		credErr.Reason != reasonZoneReferenceNotAllowed {
		return credErr
	}
	log.FromContext(ctx).Info("Leaving the Cloudflare object of a resource being deleted", "reason", credErr.Message)
	recordWarning(obj, credErr.Reason, "Delete", credErr.Message+"; its Cloudflare object stays in Cloudflare")
	return nil
}

// accountToken reads the API token from the Secret account references.
func accountToken(
	ctx context.Context,
	c client.Reader,
	account *cloudflarev1alpha1.CloudflareAccount,
) (string, *conditionError) {
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Name:      account.Spec.TokenSecretRef.Name,
		Namespace: account.Spec.TokenSecretRef.Namespace,
	}
	if condErr := secretNamespaceNotAllowed(ctx, fmt.Sprintf("Token Secret %q", secretKey.Name),
		secretKey.Namespace); condErr != nil {
		return "", condErr
	}
	if err := c.Get(ctx, secretKey, secret); err != nil {
		return "", &conditionError{
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
		return "", &conditionError{
			Reason:  "TokenKeyMissing",
			Message: fmt.Sprintf("Key %q not found in secret %s/%s", tokenKey, secretKey.Namespace, secretKey.Name),
		}
	}

	return string(token), nil
}
