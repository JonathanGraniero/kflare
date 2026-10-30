/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/cloudflare/cloudflare-go/v7/packages/pagination"
	"github.com/cloudflare/cloudflare-go/v7/shared"
	"github.com/cloudflare/cloudflare-go/v7/zero_trust"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
)

//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

// TunnelHooksAPI is what a Tunnel's hooks call: a tunnel's token, and the
// connections of the cloudflared connectors running it.
type TunnelHooksAPI interface {
	TunnelToken(ctx context.Context, accountID, tunnelID string) (string, error)
	CleanupConnections(ctx context.Context, accountID, tunnelID string) error
}

// tunnelService is cloudflare-go v7's tunnel service with the calls a Tunnel's
// hooks make, through its services of tokens and connections.
type tunnelService struct {
	*zero_trust.TunnelCloudflaredService
}

func (s *tunnelService) TunnelToken(ctx context.Context, accountID, tunnelID string) (string, error) {
	params := zero_trust.TunnelCloudflaredTokenGetParams{}
	params.AccountID.Value, params.AccountID.Present = accountID, true
	token, err := s.Token.Get(ctx, tunnelID, params)
	if err != nil {
		return "", err
	}
	return *token, nil
}

func (s *tunnelService) CleanupConnections(ctx context.Context, accountID, tunnelID string) error {
	params := zero_trust.TunnelCloudflaredConnectionDeleteParams{}
	params.AccountID.Value, params.AccountID.Present = accountID, true
	_, err := s.Connections.Delete(ctx, tunnelID, params)
	return err
}

// tunnelHooks are what a Tunnel does beyond the shared loop: Cloudflare keeps a
// deleted tunnel readable, and kflare keeps the tunnel's token in a Secret and
// disconnects its connectors before it deletes it.
var tunnelHooks = crudHooks[*cloudflarev1alpha1.Tunnel, TunnelAPI, shared.CloudflareTunnel]{
	gone:         tunnelDeleted,
	find:         findTunnel,
	synced:       syncTunnelToken,
	beforeDelete: disconnectTunnel,
	watch:        watchTokenSecrets,
}

// tunnelDeleted reports whether Cloudflare keeps t only as a record: it answers
// for a deleted tunnel, with deleted_at set.
func tunnelDeleted(t *shared.CloudflareTunnel) bool {
	return !t.DeletedAt.IsZero()
}

// findTunnel returns the ID of the account's tunnel named as the resource's
// spec says, among those not deleted: a deleted tunnel keeps its name, and a
// new one may share it.
func findTunnel(ctx context.Context, _ client.Client, api TunnelAPI, tunnel *cloudflarev1alpha1.Tunnel, accountID string,
	_ map[string]string) (string, error) {
	found, err := findInPages(func(page int64) (*pagination.V4PagePaginationArray[shared.CloudflareTunnel], error) {
		params := zero_trust.TunnelCloudflaredListParams{}
		params.AccountID.Value, params.AccountID.Present = accountID, true
		params.Name.Value, params.Name.Present = tunnel.Spec.Name, true
		params.IsDeleted.Value, params.IsDeleted.Present = false, true
		params.Page.Value, params.Page.Present = float64(page), true
		params.PerPage.Value, params.PerPage.Present = tunnelListPageSize, true
		return api.List(ctx, params)
	}, func(t *shared.CloudflareTunnel) bool { return t.Name == tunnel.Spec.Name && !tunnelDeleted(t) })
	if err != nil || found == nil {
		return "", err
	}
	return found.ID, nil
}

// syncTunnelToken writes the tunnel's token to the credentials Secret. It
// fetches the token every reconcile, so the Secret follows a secret rotated
// outside kflare.
func syncTunnelToken(ctx context.Context, c client.Client, api TunnelAPI, tunnel *cloudflarev1alpha1.Tunnel,
	accountID string, current *shared.CloudflareTunnel) (ctrl.Result, bool, error) {
	token, err := api.TunnelToken(ctx, accountID, current.ID)
	if err != nil {
		result, err := handleCloudflareError(ctx, c, tunnel, err)
		return result, true, err
	}
	if err := ensureCredentialsSecret(ctx, c, tunnel, token); err != nil {
		var conflict *secretConflictError
		if !errors.As(err, &conflict) {
			return ctrl.Result{}, true, err
		}
		result, err := notReadyRetryAfter(ctx, c, tunnel,
			&conditionError{Reason: "CredentialsSecretConflict", Message: conflict.Error()})
		return result, true, err
	}
	if err := deleteStaleCredentialsSecret(ctx, c, tunnel); err != nil {
		return ctrl.Result{}, true, err
	}
	tunnel.Status.CredentialsSecretName = tunnel.Spec.CredentialsSecretRef.Name
	return ctrl.Result{}, false, nil
}

// secretConflictError reports that the credentials Secret exists but belongs
// to something other than this Tunnel.
type secretConflictError struct {
	namespace, name string
}

func (e *secretConflictError) Error() string {
	return fmt.Sprintf("Secret %s/%s already exists and is not owned by this Tunnel; "+
		"delete it or choose another credentialsSecretRef", e.namespace, e.name)
}

// ensureCredentialsSecret creates or updates the Secret named by
// spec.credentialsSecretRef so that it holds token under TUNNEL_TOKEN and is
// controlled by tunnel. A Secret that exists but is not controlled by tunnel
// is left untouched and reported as a *secretConflictError.
func ensureCredentialsSecret(ctx context.Context, c client.Client, tunnel *cloudflarev1alpha1.Tunnel, token string) error {
	key := types.NamespacedName{Name: tunnel.Spec.CredentialsSecretRef.Name, Namespace: tunnel.Namespace}

	secret := &corev1.Secret{}
	err := c.Get(ctx, key, secret)
	if apierrors.IsNotFound(err) {
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{cloudflarev1alpha1.TunnelTokenKey: []byte(token)},
		}
		if err := controllerutil.SetControllerReference(tunnel, secret, c.Scheme()); err != nil {
			return err
		}
		return c.Create(ctx, secret)
	}
	if err != nil {
		return err
	}

	if !metav1.IsControlledBy(secret, tunnel) {
		return &secretConflictError{namespace: key.Namespace, name: key.Name}
	}
	if bytes.Equal(secret.Data[cloudflarev1alpha1.TunnelTokenKey], []byte(token)) {
		return nil
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[cloudflarev1alpha1.TunnelTokenKey] = []byte(token)
	return c.Update(ctx, secret)
}

// deleteStaleCredentialsSecret removes the Secret recorded in
// status.credentialsSecretName when spec.credentialsSecretRef now names a
// different one, so an old copy of the token does not linger. A Secret this
// Tunnel does not control is never deleted.
func deleteStaleCredentialsSecret(ctx context.Context, c client.Client, tunnel *cloudflarev1alpha1.Tunnel) error {
	previous := tunnel.Status.CredentialsSecretName
	if previous == "" || previous == tunnel.Spec.CredentialsSecretRef.Name {
		return nil
	}

	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: previous, Namespace: tunnel.Namespace}, secret); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(secret, tunnel) {
		return nil
	}
	return client.IgnoreNotFound(c.Delete(ctx, secret))
}

// disconnectTunnel removes the tunnel's connections, which disconnects any
// cloudflared still running with its token, so deletion does not wait for
// connectors to stop.
func disconnectTunnel(ctx context.Context, api TunnelAPI, _ *cloudflarev1alpha1.Tunnel, accountID, id string) error {
	return api.CleanupConnections(ctx, accountID, id)
}

// watchTokenSecrets watches the Secrets a Tunnel owns, so a deleted or edited
// token Secret is restored.
func watchTokenSecrets(b *builder.Builder, _ client.Client) *builder.Builder {
	return b.Owns(&corev1.Secret{})
}

// isTunnelReady returns true if the Tunnel has a Ready=True condition and a tunnel ID.
func isTunnelReady(tunnel *cloudflarev1alpha1.Tunnel) bool {
	return tunnel.Status.CloudflareMetadata.TunnelID != "" &&
		meta.IsStatusConditionTrue(tunnel.Status.Conditions, cloudflarev1alpha1.ConditionReady)
}
