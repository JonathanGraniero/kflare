/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
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

// resolveAccountToken fetches the cluster-scoped CloudflareAccount named
// accountName and the API token from the Secret it references, for a resource
// in namespace, which the account must allow. This is the only way a resource
// reaches a token, so what spec.allowedNamespaces says holds for every kind:
// those that reach an account through a parent use one in their own namespace.
//
// When requireReady is true, an account without a Ready=True condition is
// rejected. Deletion paths pass false so that cleanup can proceed while the
// account is being re-validated.
//
// The error is a concrete *conditionError so callers can read its Reason
// directly; it is nil on success.
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

	if condErr := checkNamespaceAllowed(ctx, c, account, namespace); condErr != nil {
		return nil, "", condErr
	}

	if requireReady && !isAccountReady(account) {
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
// resolved. A namespace the account no longer allows gets no call and keeps
// nothing waiting: the resource goes, and a Warning Event says that its
// Cloudflare object stays. Holding the finalizer until someone allows the
// namespace again would leave the namespace stuck terminating, and calling
// Cloudflare would use a token the namespace has lost. Anything else is
// returned for a retry.
func leaveCloudflareObject(ctx context.Context, obj conditionedObject, credErr *conditionError) error {
	if credErr.Reason != reasonAccountNotAllowed {
		return credErr
	}
	log.FromContext(ctx).Info("Leaving the Cloudflare object of a resource being deleted", "reason", credErr.Message)
	recordDeletionWarning(obj, credErr.Reason, credErr.Message+"; its Cloudflare object stays in Cloudflare")
	return nil
}

// accountToken reads the API token from the Secret referenced by account.
//
// The error is a concrete *conditionError so callers can read its Reason
// directly; it is nil on success.
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

// isAccountReady returns true if the CloudflareAccount has a Ready=True condition.
func isAccountReady(account *cloudflarev1alpha1.CloudflareAccount) bool {
	return meta.IsStatusConditionTrue(account.Status.Conditions, cloudflarev1alpha1.ConditionReady)
}
