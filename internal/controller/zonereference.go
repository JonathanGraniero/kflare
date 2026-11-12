/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
)

// A Zone used from another namespace. A zone usually belongs to a platform
// team and its records to application teams, each in a namespace of its own.
// A DNSRecord or a WorkerRoute may therefore name a Zone in another namespace
// (zoneRef.namespace), and the Zone says which namespaces may, and for which
// hostnames (spec.allowedReferences), as a Gateway's listener says which
// namespaces' routes may attach and for which hostname. Rules and settings
// stay in the Zone's namespace: they apply to the whole zone.
//
// The Zone's account then checks the Zone's namespace, not the resource's
// (spec.allowedNamespaces): the account is used through the Zone, so the
// Zone's grant is all another namespace needs.
//
// The hostname is checked twice. What the spec asks for is checked on every
// reconcile, before any Cloudflare call. And an object the resource names by
// its ID, in its status or its claim label, is read and checked before it is
// changed or deleted: whoever may edit the resource may write that label, and
// without the check could point it at any object of the zone.

// reasonZoneReferenceNotAllowed reports a resource that names a Zone in
// another namespace whose spec.allowedReferences does not allow the
// resource's namespace, or its hostname.
const reasonZoneReferenceNotAllowed = "ZoneReferenceNotAllowed"

// checkZoneReference returns why zone does not allow a resource in namespace,
// which is not the Zone's own, to use it for hostname, nil if it does. It
// fails closed: an entry whose selector it cannot read, or a namespace it
// cannot read, allows nothing.
func checkZoneReference(ctx context.Context, c client.Reader, zone *cloudflarev1alpha1.Zone,
	namespace, hostname string) *conditionError {
	notAllowed := func(why string) *conditionError {
		return &conditionError{
			Reason: reasonZoneReferenceNotAllowed,
			Message: fmt.Sprintf("Namespace %q may not use Zone %s/%s for %q: %s", namespace, zone.Namespace, zone.Name,
				hostname, why),
		}
	}
	entries := zone.Spec.AllowedReferences
	if len(entries) == 0 {
		return notAllowed("its spec.allowedReferences is unset, so only resources in its own namespace use it")
	}
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, types.NamespacedName{Name: namespace}, ns); err != nil {
		return notAllowed(fmt.Sprintf("the namespace could not be read: %v", err))
	}
	namespaceAllowed := false
	for i := range entries {
		if !allowsNamespace(entries[i].Namespaces, ns) {
			continue
		}
		namespaceAllowed = true
		for _, pattern := range entries[i].Hostnames {
			if hostnameAllowed(pattern, hostname) {
				return nil
			}
		}
	}
	if !namespaceAllowed {
		return notAllowed("no entry of its spec.allowedReferences allows the namespace")
	}
	return notAllowed("no entry of its spec.allowedReferences that allows the namespace lists a hostname that " +
		"covers it")
}

// allowsNamespace reports whether allowed picks the namespace ns. Only the
// value that says so outright allows every namespace, and a selector that is
// unset or invalid allows none.
func allowsNamespace(allowed cloudflarev1alpha1.AllowedNamespaces, ns *corev1.Namespace) bool {
	if allowed.From == cloudflarev1alpha1.NamespacesFromAll {
		return true
	}
	if allowed.From != cloudflarev1alpha1.NamespacesFromSelector {
		return false
	}
	// No selector selects no namespace.
	selector, err := metav1.LabelSelectorAsSelector(allowed.Selector)
	return err == nil && selector.Matches(labels.Set(ns.Labels))
}

// hostnameAllowed reports whether pattern, an entry of a Zone's allowed
// hostnames, covers hostname. A pattern that starts with "*." covers every
// name below the rest, at any depth, and not the rest itself; any other
// covers itself alone. Names ignore case and a trailing dot.
//
// hostname may hold a wildcard of its own, as the record "*.a.example.com"
// and the route "*a.example.com/*" do. Every name it stands for ends with
// what follows its wildcard, so a pattern covers them all exactly when it
// covers hostname as written: "*.example.com" covers both of those, and
// "*.a.example.com" only the first.
func hostnameAllowed(pattern, hostname string) bool {
	pattern, hostname = normalizeHostname(pattern), normalizeHostname(hostname)
	if pattern == "" || hostname == "" {
		return false
	}
	if parent, wildcard := strings.CutPrefix(pattern, "*."); wildcard {
		return parent != "" && len(hostname) > len(parent)+1 && strings.HasSuffix(hostname, "."+parent)
	}
	return hostname == pattern
}

// normalizeHostname returns name as hostnames are compared: in lower case,
// without the dot that ends a fully-qualified name.
func normalizeHostname(name string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
}

// routePatternHostname returns the hostname of a Worker route's pattern, with
// its wildcard if it has one: what comes before the path, as in
// "*.example.com" for "*.example.com/api/*". Cloudflare stores a scheme as it
// is given, so one is dropped: what ends at the first "://", when no path
// starts before it. A "://" further on is part of the path, and what follows
// it is no hostname.
func routePatternHostname(pattern string) string {
	if scheme, rest, found := strings.Cut(pattern, "://"); found && !strings.Contains(scheme, "/") {
		pattern = rest
	}
	host, _, _ := strings.Cut(pattern, "/")
	return host
}

// sharedZoneDependents builds the dependentKind of a zone's kind whose
// resources may name a Zone in another namespace: ref returns a resource's
// zoneRef and the hostname it asks its Zone for.
func sharedZoneDependents[T any, P interface {
	*T
	client.Object
}](kind string, list func() client.ObjectList, ref func(P) (cloudflarev1alpha1.ZoneReference, string)) dependentKind {
	d := dependents(kind, list, func(obj P) []string {
		zoneRef, _ := ref(obj)
		return []string{zoneRef.Name}
	})
	d.zone = func(obj client.Object) (string, string, bool) {
		typed, ok := obj.(P)
		if !ok {
			return "", "", false
		}
		zoneRef, hostname := ref(typed)
		return zoneRef.Namespace, hostname, true
	}
	return d
}

// parentNamespace returns the namespace of the parent a dependent names: its
// own, unless its kind's resources may name a Zone in another namespace and it
// does.
func (k dependentKind) parentNamespace(dependent client.Object) string {
	if k.zone != nil {
		if namespace, _, ok := k.zone(dependent); ok && namespace != "" {
			return namespace
		}
	}
	return dependent.GetNamespace()
}
