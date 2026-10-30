/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
)

// Deletion protection. A resource whose dependents need it for their own
// cleanup keeps its finalizer until they are gone, reporting Ready=False with
// reason InUse meanwhile:
//
//   - a CloudflareAccount waits for the Zones, Tunnels, WorkerScripts and
//     IPLists that use its credentials, and for the resources of every
//     account kind kflare-gen generates, such as KVNamespaces and R2Buckets;
//   - a Zone waits for its DNSRecords, WorkerRoutes, rules and settings (the
//     generated zone kinds, such as WAFCustomRules and ManagedTransforms),
//     which need its zone ID and account;
//   - a Tunnel waits for its TunnelConfigurations, which reset its ingress
//     rules through its account, and for the DNSRecords that point at it
//     through tunnelRef, whose hostnames would otherwise route to nothing;
//   - a KVNamespace or R2Bucket waits for the WorkerScripts bound to it:
//     Cloudflare deletes a namespace or bucket a Worker is bound to, but
//     every later upload of that Worker then fails.
//
// Without this, deleting everything at once (a namespace, or `kubectl delete
// -f` on a directory) lets a parent go first. A child then cannot reach
// Cloudflare and skips its cleanup, which ignores its own deletion policy:
// for example DNS records were left behind in a retained zone.

// dependentKind describes one kind of resource that depends on a parent: how
// to list it, which parents a given resource references, and an empty object
// of the kind, to watch it with.
type dependentKind struct {
	kind      string
	list      func() client.ObjectList
	parents   func(client.Object) ([]string, bool)
	newObject func() client.Object
}

// dependents builds a dependentKind for objects of type P (a *T) that
// reference the parents named by refs.
func dependents[T any, P interface {
	*T
	client.Object
}](kind string, list func() client.ObjectList, refs func(P) []string) dependentKind {
	return dependentKind{
		kind: kind,
		list: list,
		parents: func(obj client.Object) ([]string, bool) {
			typed, ok := obj.(P)
			if !ok {
				return nil, false
			}
			return refs(typed), true
		},
		newObject: func() client.Object { return P(new(T)) },
	}
}

var (
	// accountDependents use a CloudflareAccount's credentials directly.
	accountDependents = generatedAccountDependents

	// zoneDependents live in a Zone's namespace and reference it by name: the
	// generated kinds of a zone.
	zoneDependents = generatedZoneDependents
)

// configuredTunnels returns the Tunnel a TunnelConfiguration configures.
func configuredTunnels(tc *cloudflarev1alpha1.TunnelConfiguration) []string {
	return []string{tc.Spec.TunnelRef.Name}
}

// recordTunnels returns the Tunnel a DNSRecord points at through tunnelRef,
// if any.
func recordTunnels(d *cloudflarev1alpha1.DNSRecord) []string {
	if d.Spec.TunnelRef == nil {
		return nil
	}
	return []string{d.Spec.TunnelRef.Name}
}

// dependentsOf returns every resource of kinds in namespace ("" for all
// namespaces) that references parentName, as sorted "Kind namespace/name"
// strings.
func dependentsOf(
	ctx context.Context,
	c client.Reader,
	namespace, parentName string,
	kinds []dependentKind,
) ([]string, error) {
	var found []string
	for _, k := range kinds {
		list := k.list()
		if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
			return nil, err
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			obj, ok := item.(client.Object)
			if !ok {
				return nil, fmt.Errorf("unexpected list item %T", item)
			}
			if names, ok := k.parents(obj); ok && slices.Contains(names, parentName) {
				found = append(found, fmt.Sprintf("%s %s/%s", k.kind, obj.GetNamespace(), obj.GetName()))
			}
		}
	}
	sort.Strings(found)
	return found, nil
}

// maxListedDependents caps how many blocking resources the InUse message names.
const maxListedDependents = 5

// reportInUse sets Ready=False with reason InUse on obj, naming the
// dependents its deletion is waiting for.
func reportInUse(ctx context.Context, c client.StatusClient, obj conditionedObject, dependents []string) error {
	listed := dependents
	if len(listed) > maxListedDependents {
		listed = append(listed[:maxListedDependents:maxListedDependents], "...")
	}
	return updateNotReady(ctx, c, obj, "InUse",
		fmt.Sprintf("Deletion is waiting for %d resource(s) that depend on it: %s",
			len(dependents), strings.Join(listed, ", ")))
}

// deletesOnly passes only delete events, which are all a parent needs to
// hear about its dependents.
var deletesOnly = predicate.Funcs{
	CreateFunc:  func(event.CreateEvent) bool { return false },
	UpdateFunc:  func(event.UpdateEvent) bool { return false },
	DeleteFunc:  func(event.DeleteEvent) bool { return true },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// deletingParentOf maps the deletion of dependent to those of its parents
// that are themselves being deleted: a parent in normal use has nothing to do
// when a dependent goes away. newParent returns an empty object of the
// parent's kind; namespaced parents are looked up in the dependent's
// namespace.
func deletingParentOf(
	ctx context.Context,
	c client.Reader,
	dependent client.Object,
	newParent func() client.Object,
	namespaced bool,
	kinds []dependentKind,
) []reconcile.Request {
	var reqs []reconcile.Request
	for _, k := range kinds {
		names, ok := k.parents(dependent)
		if !ok {
			continue
		}
		for _, name := range names {
			key := types.NamespacedName{Name: name}
			if namespaced {
				key.Namespace = dependent.GetNamespace()
			}
			parent := newParent()
			if err := c.Get(ctx, key, parent); err != nil || parent.GetDeletionTimestamp().IsZero() {
				continue
			}
			reqs = append(reqs, reconcile.Request{NamespacedName: key})
		}
	}
	return reqs
}
