/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/cloudflare/cloudflare-go/v7/zero_trust"
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
	tunnelConfigurationObject = zero_trust.TunnelCloudflaredConfigurationGetResponse
	tunnelConfigurationListed = zero_trust.TunnelCloudflaredConfigurationGetResponse
)

// tunnelConfig is a tunnel's configuration as kflare writes it.
type tunnelConfig = zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfig

// tunnelIngressRule is one of a tunnelConfig's rules.
type tunnelIngressRule = zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress

// TunnelConfigurationAPI is the part of cloudflare-go v7's
// zero_trust.TunnelCloudflaredConfigurationService a TunnelConfiguration
// calls; tests inject a fake.
type TunnelConfigurationAPI interface {
	Get(ctx context.Context, tunnelID string, query zero_trust.TunnelCloudflaredConfigurationGetParams,
		opts ...option.RequestOption) (*zero_trust.TunnelCloudflaredConfigurationGetResponse, error)
	Update(ctx context.Context, tunnelID string, params zero_trust.TunnelCloudflaredConfigurationUpdateParams,
		opts ...option.RequestOption) (*zero_trust.TunnelCloudflaredConfigurationUpdateResponse, error)
}

// defaultTunnelConfigurationAPI is the production factory: cloudflare-go v7's
// zero_trust.TunnelCloudflaredConfigurationService, built from cfpkg.New's
// options for the token.
func defaultTunnelConfigurationAPI(token string) (TunnelConfigurationAPI, error) {
	client, err := cfpkg.New(token)
	if err != nil {
		return nil, err
	}
	return zero_trust.NewTunnelCloudflaredConfigurationService(client.V7Options()...), nil
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
	observe: func(obj *cloudflarev1alpha1.TunnelConfiguration, current *tunnelConfigurationObject) {
		obj.Status.CloudflareMetadata.TunnelID = current.TunnelID
		obj.Status.CloudflareMetadata.Version = int(current.Version)
	},

	id: func(current *tunnelConfigurationObject) string { return current.TunnelID },
	matches: func(obj *cloudflarev1alpha1.TunnelConfiguration, current *tunnelConfigurationObject,
		_ map[string]string) bool {
		return tunnelConfigurationMatches(desiredTunnelConfiguration(obj), current.Config)
	},

	get: func(ctx context.Context, api TunnelConfigurationAPI, _ *cloudflarev1alpha1.TunnelConfiguration,
		accountID, id string) (*tunnelConfigurationObject, error) {
		return readTunnelConfiguration(ctx, api, accountID, id)
	},
	create: func(ctx context.Context, api TunnelConfigurationAPI, accountID string,
		obj *cloudflarev1alpha1.TunnelConfiguration, refs map[string]string) (*tunnelConfigurationObject, error) {
		return writeTunnelConfiguration(ctx, api, accountID, refs[tunnelRefField], desiredTunnelConfiguration(obj))
	},
	update: func(ctx context.Context, api TunnelConfigurationAPI, obj *cloudflarev1alpha1.TunnelConfiguration,
		accountID, id string, _ map[string]string) (*tunnelConfigurationObject, error) {
		return writeTunnelConfiguration(ctx, api, accountID, id, desiredTunnelConfiguration(obj))
	},
	// Cloudflare does not allow an empty configuration, so deleting one leaves
	// the catch-all rule.
	delete: func(ctx context.Context, api TunnelConfigurationAPI, _ *cloudflarev1alpha1.TunnelConfiguration,
		accountID, id string) error {
		_, err := writeTunnelConfiguration(ctx, api, accountID, id, ingressConfig(nil, defaultTunnelService))
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
		// Another resource configures the same tunnel when its Tunnel is the
		// same one, or the one that replaces it.
		describes: func(ctx context.Context, c client.Client, tc, other *cloudflarev1alpha1.TunnelConfiguration) (bool, error) {
			return sameTunnel(ctx, c, tc.Namespace, tc.Spec.TunnelRef.Name, other.Spec.TunnelRef.Name)
		},
	},
}

// tunnelConfigurationAccount returns the CloudflareAccount of the Tunnel a
// TunnelConfiguration names. The Tunnel waits for its configurations before it
// is deleted, so it is normally still there. If it is gone anyway (its finalizer
// was removed by hand), there are no credentials to reset with and the rules
// are left. A Tunnel outside what the installation watches reports reason
// NotWatched, and is not gone.
func tunnelConfigurationAccount(ctx context.Context, c client.Client,
	tc *cloudflarev1alpha1.TunnelConfiguration) (string, bool, *conditionError) {
	tunnel := &cloudflarev1alpha1.Tunnel{}
	key := types.NamespacedName{Name: tc.Spec.TunnelRef.Name, Namespace: tc.Namespace}
	what := fmt.Sprintf("Tunnel %q", key.Name)
	err := c.Get(ctx, key, tunnel)
	if condErr := outsideReference(ctx, key, tunnel, what, err); condErr != nil {
		return "", false, condErr
	}
	if err != nil {
		return "", apierrors.IsNotFound(err), &conditionError{
			Reason: "TunnelNotFound", Message: fmt.Sprintf("%s not found: %v", what, err),
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
	refs map[string]string) (string, *tunnelConfigurationObject, error) {
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
	id string) (*tunnelConfigurationObject, error) {
	var params zero_trust.TunnelCloudflaredConfigurationGetParams
	params.AccountID.Value, params.AccountID.Present = accountID, true
	current, err := api.Get(ctx, id, params)
	if err != nil {
		return nil, err
	}
	current.TunnelID = id
	return current, nil
}

// writeTunnelConfiguration replaces the configuration of the tunnel id, and
// returns it as Cloudflare answered, which is what a read returns.
func writeTunnelConfiguration(ctx context.Context, api TunnelConfigurationAPI, accountID, id string,
	config tunnelConfig) (*tunnelConfigurationObject, error) {
	var params zero_trust.TunnelCloudflaredConfigurationUpdateParams
	params.AccountID.Value, params.AccountID.Present = accountID, true
	params.Config.Value, params.Config.Present = config, true
	updated, err := api.Update(ctx, id, params)
	if err != nil {
		return nil, err
	}
	written := &tunnelConfigurationObject{}
	if err := json.Unmarshal([]byte(updated.JSON.RawJSON()), written); err != nil {
		return nil, fmt.Errorf("reading the tunnel configuration Cloudflare answered with: %w", err)
	}
	written.TunnelID = id
	return written, nil
}

// desiredTunnelConfiguration builds the configuration to send to Cloudflare:
// the spec's rules followed by the catch-all rule.
func desiredTunnelConfiguration(tc *cloudflarev1alpha1.TunnelConfiguration) tunnelConfig {
	defaultService := tc.Spec.DefaultService
	if defaultService == "" {
		defaultService = defaultTunnelService
	}
	return ingressConfig(tc.Spec.Ingress, defaultService)
}

// ingressConfig is a configuration of rules, then a catch-all rule to
// defaultService.
func ingressConfig(rules []cloudflarev1alpha1.TunnelIngressRule, defaultService string) tunnelConfig {
	ingress := make([]tunnelIngressRule, 0, len(rules)+1)
	for _, rule := range rules {
		var out tunnelIngressRule
		out.Hostname.Value, out.Hostname.Present = rule.Hostname, true
		out.Service.Value, out.Service.Present = rule.Service, true
		if rule.Path != "" {
			out.Path.Value, out.Path.Present = rule.Path, true
		}
		if rule.OriginRequest != nil {
			out.OriginRequest.Value, out.OriginRequest.Present = originRequestConfig(rule.OriginRequest), true
		}
		ingress = append(ingress, out)
	}
	var catchAll tunnelIngressRule
	catchAll.Service.Value, catchAll.Service.Present = defaultService, true
	var config tunnelConfig
	config.Ingress.Value, config.Ingress.Present = append(ingress, catchAll), true
	return config
}

// originRequestConfig converts a rule's origin settings to the SDK's, with
// only the fields the spec sets.
func originRequestConfig(
	o *cloudflarev1alpha1.TunnelOriginRequest) zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngressOriginRequest {
	var out zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngressOriginRequest
	if o.HTTPHostHeader != nil {
		out.HTTPHostHeader.Value, out.HTTPHostHeader.Present = *o.HTTPHostHeader, true
	}
	if o.OriginServerName != nil {
		out.OriginServerName.Value, out.OriginServerName.Present = *o.OriginServerName, true
	}
	if o.NoTLSVerify != nil {
		out.NoTLSVerify.Value, out.NoTLSVerify.Present = *o.NoTLSVerify, true
	}
	if o.HTTP2Origin != nil {
		out.HTTP2Origin.Value, out.HTTP2Origin.Present = *o.HTTP2Origin, true
	}
	if o.DisableChunkedEncoding != nil {
		out.DisableChunkedEncoding.Value, out.DisableChunkedEncoding.Present = *o.DisableChunkedEncoding, true
	}
	if o.ConnectTimeout != nil {
		out.ConnectTimeout.Value, out.ConnectTimeout.Present = *o.ConnectTimeout, true
	}
	if o.TLSTimeout != nil {
		out.TLSTimeout.Value, out.TLSTimeout.Present = *o.TLSTimeout, true
	}
	if o.TCPKeepAlive != nil {
		out.TCPKeepAlive.Value, out.TCPKeepAlive.Present = *o.TCPKeepAlive, true
	}
	if o.KeepAliveTimeout != nil {
		out.KeepAliveTimeout.Value, out.KeepAliveTimeout.Present = *o.KeepAliveTimeout, true
	}
	return out
}

// tunnelConfigurationMatches reports whether actual, as Cloudflare stores it,
// already equals desired. kflare owns the whole configuration, so any rule,
// origin setting or top-level option that differs — including ones the spec
// cannot express, such as settings added in the dashboard — counts as drift
// and is overwritten. Both compare as JSON without their zero values:
// Cloudflare stores an empty originRequest and warp-routing disabled for a
// configuration that sets neither (verified live), and a setting at its zero
// value is one left unset.
func tunnelConfigurationMatches(desired tunnelConfig,
	actual zero_trust.TunnelCloudflaredConfigurationGetResponseConfig) bool {
	sent, err := desired.MarshalJSON()
	if err != nil {
		return false
	}
	var want, got any
	if json.Unmarshal(sent, &want) != nil || json.Unmarshal([]byte(actual.JSON.RawJSON()), &got) != nil {
		return false
	}
	return reflect.DeepEqual(withoutZeros(want), withoutZeros(got))
}

// withoutZeros drops the nulls, false, 0 and "", and the objects and arrays
// that are or end up empty, from the objects in a decoded JSON value, as
// withoutEmpty does with nulls alone. Array elements stay: their positions
// matter.
func withoutZeros(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if item = withoutZeros(item); !isEmptyJSON(item) && item != false && item != 0.0 && item != "" {
				out[key] = item
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = withoutZeros(item)
		}
		return out
	default:
		return v
	}
}

// tunnelConfigured reports whether config routes anything: it holds more
// than the catch-all rule a configuration must end with. A new tunnel has no
// rules, and one kflare reset only the catch-all rule.
func tunnelConfigured(config zero_trust.TunnelCloudflaredConfigurationGetResponseConfig) bool {
	return len(config.Ingress) > 1
}
