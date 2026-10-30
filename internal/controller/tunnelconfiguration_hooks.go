/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"
	"reflect"

	cf "github.com/cloudflare/cloudflare-go"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
)

// defaultTunnelService is the catch-all service used when spec.defaultService
// is empty, and the only rule left after deletion. Cloudflare rejects a
// configuration without a final catch-all rule.
const defaultTunnelService = "http_status:404"

// tunnelConfigurationObject and tunnelConfigurationListed are what a
// TunnelConfiguration's calls return. A configuration is never listed.
type (
	tunnelConfigurationObject = cf.TunnelConfigurationResult
	tunnelConfigurationListed = cf.TunnelConfigurationResult
)

// TunnelConfigurationAPI is the subset of the Cloudflare API used by a
// TunnelConfiguration. In production, *cfpkg.Client satisfies this interface
// because it embeds *cf.API.
type TunnelConfigurationAPI interface {
	GetTunnelConfiguration(ctx context.Context, rc *cf.ResourceContainer, tunnelID string) (cf.TunnelConfigurationResult, error)
	UpdateTunnelConfiguration(ctx context.Context, rc *cf.ResourceContainer, params cf.TunnelConfigurationParams) (cf.TunnelConfigurationResult, error)
}

// defaultTunnelConfigurationAPI is the production factory: it delegates to
// cfpkg.New so that the returned *cfpkg.Client (which embeds *cf.API)
// satisfies TunnelConfigurationAPI.
func defaultTunnelConfigurationAPI(token string) (TunnelConfigurationAPI, error) {
	return cfpkg.New(token)
}

// tunnelConfigurationKind describes TunnelConfigurations to the shared CRUD
// reconciler. A tunnel's configuration always exists, and has no ID of its own:
// the tunnel's names it, and deleting one resets it to the catch-all rule. So
// "creating" one writes a tunnel's first rules, and a configuration that holds
// more than the catch-all rule is an existing one, which a resource takes over
// only as its adoption policy allows. A resource claims its tunnel, and the
// others that name it report TunnelAlreadyConfigured.
var tunnelConfigurationKind = crudResourceKind[*cloudflarev1alpha1.TunnelConfiguration, TunnelConfigurationAPI,
	tunnelConfigurationObject, tunnelConfigurationListed]{
	name:      "TunnelConfiguration",
	noun:      "tunnel configuration",
	newObject: func() *cloudflarev1alpha1.TunnelConfiguration { return &cloudflarev1alpha1.TunnelConfiguration{} },
	newList:   func() client.ObjectList { return &cloudflarev1alpha1.TunnelConfigurationList{} },

	// The Tunnel a resource names. Its hooks' key is the tunnel's ID.
	key:            func(obj *cloudflarev1alpha1.TunnelConfiguration) string { return obj.Spec.TunnelRef.Name },
	claimLabel:     cloudflarev1alpha1.TunnelConfigurationIDLabel,
	conflictReason: "TunnelAlreadyConfigured",
	statusID: func(obj *cloudflarev1alpha1.TunnelConfiguration) string {
		return obj.Status.CloudflareMetadata.TunnelID
	},
	observe: func(obj *cloudflarev1alpha1.TunnelConfiguration, current *cf.TunnelConfigurationResult) {
		obj.Status.CloudflareMetadata.TunnelID = current.TunnelID
		obj.Status.CloudflareMetadata.Version = current.Version
	},

	id: func(current *cf.TunnelConfigurationResult) string { return current.TunnelID },
	matches: func(obj *cloudflarev1alpha1.TunnelConfiguration, current *cf.TunnelConfigurationResult,
		_ map[string]string) bool {
		return tunnelConfigurationMatches(desiredTunnelConfiguration(obj), current.Config)
	},

	get: func(ctx context.Context, api TunnelConfigurationAPI, _ *cloudflarev1alpha1.TunnelConfiguration,
		accountID, id string) (*cf.TunnelConfigurationResult, error) {
		return readTunnelConfiguration(ctx, api, accountID, id)
	},
	create: func(ctx context.Context, api TunnelConfigurationAPI, accountID string,
		obj *cloudflarev1alpha1.TunnelConfiguration, refs map[string]string) (*cf.TunnelConfigurationResult, error) {
		return writeTunnelConfiguration(ctx, api, accountID, refs[tunnelRefField], desiredTunnelConfiguration(obj))
	},
	update: func(ctx context.Context, api TunnelConfigurationAPI, obj *cloudflarev1alpha1.TunnelConfiguration,
		accountID, id string, _ map[string]string) (*cf.TunnelConfigurationResult, error) {
		return writeTunnelConfiguration(ctx, api, accountID, id, desiredTunnelConfiguration(obj))
	},
	// Cloudflare does not allow an empty configuration, so deleting one leaves
	// the catch-all rule.
	delete: func(ctx context.Context, api TunnelConfigurationAPI, _ *cloudflarev1alpha1.TunnelConfiguration,
		accountID, id string) error {
		_, err := writeTunnelConfiguration(ctx, api, accountID, id, cf.TunnelConfiguration{
			Ingress: []cf.UnvalidatedIngressRule{{Service: defaultTunnelService}},
		})
		return err
	},

	hooks: crudHooks[*cloudflarev1alpha1.TunnelConfiguration, TunnelConfigurationAPI, tunnelConfigurationObject]{
		account: tunnelConfigurationAccount,
		key: func(_ *cloudflarev1alpha1.TunnelConfiguration, refs map[string]string) string {
			return refs[tunnelRefField]
		},
		references: []crudReference[*cloudflarev1alpha1.TunnelConfiguration]{
			reference(tunnelRefField, "Tunnel",
				func(obj *cloudflarev1alpha1.TunnelConfiguration) string { return obj.Spec.TunnelRef.Name },
				func(ref *cloudflarev1alpha1.Tunnel) string { return ref.Spec.AccountRef.Name },
				func(ref *cloudflarev1alpha1.Tunnel) string { return ref.Status.CloudflareMetadata.TunnelID }),
		},
		resolve: resolveTunnelOwner,
		find:    findTunnelConfiguration,
	},
}

// tunnelConfigurationAccount returns the CloudflareAccount of the Tunnel a
// TunnelConfiguration names. The Tunnel waits for its configurations before it
// is deleted, so it is normally still there. If it is gone anyway (its finalizer
// was removed by hand), there are no credentials to reset with and the rules
// are left.
func tunnelConfigurationAccount(ctx context.Context, c client.Client,
	tc *cloudflarev1alpha1.TunnelConfiguration) (string, bool, *conditionError) {
	tunnel := &cloudflarev1alpha1.Tunnel{}
	key := types.NamespacedName{Name: tc.Spec.TunnelRef.Name, Namespace: tc.Namespace}
	if err := c.Get(ctx, key, tunnel); err != nil {
		return "", apierrors.IsNotFound(err), &conditionError{
			Reason: "TunnelNotFound", Message: fmt.Sprintf("Tunnel %q not found: %v", key.Name, err),
		}
	}
	return tunnel.Spec.AccountRef.Name, false, nil
}

// resolveTunnelOwner keeps a resource from writing a tunnel's rules unless it
// owns them, and forgets the tunnel status records when the Tunnel now has
// another one: a recreated Tunnel's old tunnel stays readable, and the new one
// has no rules yet.
func resolveTunnelOwner(ctx context.Context, c client.Client, tc *cloudflarev1alpha1.TunnelConfiguration,
	refs map[string]string) *conditionError {
	owner, err := configurationOwner(ctx, c, tc)
	if err != nil {
		return &conditionError{Reason: "APIError", Message: err.Error(), retry: true}
	}
	if owner != tc.Name {
		return &conditionError{Reason: "TunnelAlreadyConfigured", retry: true,
			Message: fmt.Sprintf("Tunnel %q is already configured by TunnelConfiguration %q", tc.Spec.TunnelRef.Name, owner)}
	}
	if tc.Status.CloudflareMetadata.TunnelID != refs[tunnelRefField] {
		tc.Status.CloudflareMetadata.TunnelID = ""
	}
	return nil
}

// configurationOwner returns the name of the TunnelConfiguration that owns
// the ingress rules of tc's Tunnel: the oldest one referencing it, with the
// name breaking ties. Configurations still being deleted keep ownership until
// they are gone, so they cannot clear rules another configuration just wrote.
func configurationOwner(ctx context.Context, c client.Client, tc *cloudflarev1alpha1.TunnelConfiguration) (string, error) {
	list := &cloudflarev1alpha1.TunnelConfigurationList{}
	if err := c.List(ctx, list, client.InNamespace(tc.Namespace)); err != nil {
		return "", err
	}
	owner := tc
	for i := range list.Items {
		other := &list.Items[i]
		if other.Spec.TunnelRef.Name != tc.Spec.TunnelRef.Name {
			continue
		}
		if olderThan(other, owner) {
			owner = other
		}
	}
	return owner.Name, nil
}

// olderThan orders TunnelConfigurations by creation time, then by name.
func olderThan(a, b *cloudflarev1alpha1.TunnelConfiguration) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
}

// findTunnelConfiguration returns the ID of the resource's tunnel when it holds
// a configuration: more than the catch-all rule, which is all a new tunnel, or
// one kflare reset, holds.
func findTunnelConfiguration(ctx context.Context, _ client.Client, api TunnelConfigurationAPI,
	_ *cloudflarev1alpha1.TunnelConfiguration, accountID string,
	refs map[string]string) (string, *cf.TunnelConfigurationResult, error) {
	current, err := readTunnelConfiguration(ctx, api, accountID, refs[tunnelRefField])
	if err != nil {
		return "", nil, err
	}
	if !tunnelConfigured(current.Config) {
		return "", nil, nil
	}
	return current.TunnelID, current, nil
}

// readTunnelConfiguration reads the configuration of the tunnel id.
func readTunnelConfiguration(ctx context.Context, api TunnelConfigurationAPI, accountID,
	id string) (*cf.TunnelConfigurationResult, error) {
	current, err := api.GetTunnelConfiguration(ctx, cf.AccountIdentifier(accountID), id)
	if err != nil {
		return nil, err
	}
	current.TunnelID = id
	return &current, nil
}

// writeTunnelConfiguration replaces the configuration of the tunnel id.
func writeTunnelConfiguration(ctx context.Context, api TunnelConfigurationAPI, accountID, id string,
	config cf.TunnelConfiguration) (*cf.TunnelConfigurationResult, error) {
	updated, err := api.UpdateTunnelConfiguration(ctx, cf.AccountIdentifier(accountID), cf.TunnelConfigurationParams{
		TunnelID: id, Config: config,
	})
	if err != nil {
		return nil, err
	}
	// What was just written is what the resource asks for, however Cloudflare
	// encodes its answer.
	updated.TunnelID, updated.Config = id, config
	return &updated, nil
}

// desiredTunnelConfiguration builds the configuration to send to Cloudflare:
// the spec's rules followed by the catch-all rule.
func desiredTunnelConfiguration(tc *cloudflarev1alpha1.TunnelConfiguration) cf.TunnelConfiguration {
	rules := make([]cf.UnvalidatedIngressRule, 0, len(tc.Spec.Ingress)+1)
	for _, rule := range tc.Spec.Ingress {
		rules = append(rules, cf.UnvalidatedIngressRule{
			Hostname:      rule.Hostname,
			Path:          rule.Path,
			Service:       rule.Service,
			OriginRequest: originRequestConfig(rule.OriginRequest),
		})
	}
	defaultService := tc.Spec.DefaultService
	if defaultService == "" {
		defaultService = defaultTunnelService
	}
	rules = append(rules, cf.UnvalidatedIngressRule{Service: defaultService})
	return cf.TunnelConfiguration{Ingress: rules}
}

// originRequestConfig converts the spec's origin settings to the SDK type.
func originRequestConfig(o *cloudflarev1alpha1.TunnelOriginRequest) *cf.OriginRequestConfig {
	if o == nil {
		return nil
	}
	return &cf.OriginRequestConfig{
		HTTPHostHeader:         o.HTTPHostHeader,
		OriginServerName:       o.OriginServerName,
		NoTLSVerify:            o.NoTLSVerify,
		Http2Origin:            o.HTTP2Origin,
		DisableChunkedEncoding: o.DisableChunkedEncoding,
	}
}

// tunnelConfigurationMatches reports whether actual already equals desired.
// kflare owns the whole configuration, so any rule, origin setting or
// top-level option that differs — including ones the spec cannot express,
// such as settings added in the dashboard — counts as drift and is
// overwritten.
func tunnelConfigurationMatches(desired, actual cf.TunnelConfiguration) bool {
	if !reflect.DeepEqual(normalizeIngress(desired.Ingress), normalizeIngress(actual.Ingress)) {
		return false
	}
	if !reflect.DeepEqual(actual.OriginRequest, cf.OriginRequestConfig{}) {
		return false
	}
	// Cloudflare reports warp-routing as disabled when it was never set.
	return actual.WarpRouting == nil || !actual.WarpRouting.Enabled
}

// tunnelConfigured reports whether config routes anything: it holds more
// than the catch-all rule a configuration must end with. A new tunnel has no
// rules, and one kflare reset only the catch-all rule.
func tunnelConfigured(config cf.TunnelConfiguration) bool {
	return len(config.Ingress) > 1
}

// normalizeIngress treats an empty originRequest object the same as an
// absent one, so equivalent rules compare equal however the API encoded them.
func normalizeIngress(rules []cf.UnvalidatedIngressRule) []cf.UnvalidatedIngressRule {
	out := make([]cf.UnvalidatedIngressRule, len(rules))
	for i, rule := range rules {
		if rule.OriginRequest != nil && reflect.DeepEqual(*rule.OriginRequest, cf.OriginRequestConfig{}) {
			rule.OriginRequest = nil
		}
		out[i] = rule
	}
	return out
}
