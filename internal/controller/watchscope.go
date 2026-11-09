/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
)

// reasonNotWatched reports a resource whose CloudflareAccount, or a resource
// it references, is outside what its kflare installation watches: another
// installation's, or nobody's.
const reasonNotWatched = "NotWatched"

// maxListed caps how many names, of namespaces or CloudflareAccounts, a
// message lists.
const maxListed = 5

// WatchScope is what one kflare installation manages, so that several can
// share a cluster: the resources in the namespaces it watches whose labels
// match its selector, and the CloudflareAccounts whose labels match it and
// whose token Secret is in a namespace it watches. An installation reads only
// the tokens of those accounts, and two installations whose scopes do not
// overlap never manage the same account. The zero WatchScope manages
// everything, as an installation without --watch-namespaces or
// --watch-selector does.
//
// The manager's cache holds only what the scope selects (CacheOptions), so the
// controllers never see another installation's resources, nor their
// finalizers. CloudflareAccounts are the exception: every installation caches
// all of them, so that releasing a token Secret sees every account that uses
// it, and a resource naming another installation's account can say so.
//
// The scope reaches every reconcile and watch handler of a manager through
// their context, which derives from the manager's base context: cmd/main.go
// sets one that carries it (WithWatchScope). A context without a scope, as a
// test that calls Reconcile directly has, manages everything.
type WatchScope struct {
	// namespaces is sorted; nil watches every namespace.
	namespaces []string
	// selector is nil for an installation that selects every resource.
	selector labels.Selector
	// reader reads from the API server, past the manager's cache, to tell a
	// reference the selector leaves out from one to a missing resource.
	reader client.Reader
}

// NewWatchScope returns the scope of an installation that watches the
// namespaces listed in namespaces, separated by commas (every namespace when
// it is empty), and manages the resources and CloudflareAccounts whose labels
// match selector, in the syntax of kubectl's --selector (all of them when it
// is empty). reader reads from the API server, past the manager's cache.
func NewWatchScope(namespaces, selector string, reader client.Reader) (WatchScope, error) {
	scope := WatchScope{reader: reader}
	if strings.TrimSpace(namespaces) != "" {
		for name := range strings.SplitSeq(namespaces, ",") {
			name = strings.TrimSpace(name)
			if problems := validation.IsDNS1123Label(name); len(problems) > 0 {
				return WatchScope{}, fmt.Errorf("namespace %q: %s", name, strings.Join(problems, "; "))
			}
			if !slices.Contains(scope.namespaces, name) {
				scope.namespaces = append(scope.namespaces, name)
			}
		}
		slices.Sort(scope.namespaces)
	}
	if strings.TrimSpace(selector) != "" {
		parsed, err := labels.Parse(selector)
		if err != nil {
			return WatchScope{}, fmt.Errorf("selector %q: %w", selector, err)
		}
		scope.selector = parsed
	}
	return scope, nil
}

// String describes the scope, for the manager's log.
func (s WatchScope) String() string {
	namespaces, selector := "every namespace", "every resource"
	if s.namespaces != nil {
		namespaces = "namespaces " + strings.Join(s.namespaces, ", ")
	}
	if s.selector != nil {
		selector = "resources matching " + s.selector.String()
	}
	return namespaces + ", " + selector
}

// CacheOptions returns the manager's cache options for the scope: only the
// namespaces it watches, and of kflare's kinds, but for CloudflareAccount,
// only the resources its selector matches. Secrets, ConfigMaps and Namespaces
// are cached whatever their labels: resources name them.
func (s WatchScope) CacheOptions(scheme *runtime.Scheme) cache.Options {
	var opts cache.Options
	if s.namespaces != nil {
		opts.DefaultNamespaces = make(map[string]cache.Config, len(s.namespaces))
		for _, namespace := range s.namespaces {
			opts.DefaultNamespaces[namespace] = cache.Config{}
		}
	}
	if s.selector != nil {
		opts.ByObject = map[client.Object]cache.ByObject{}
		for _, obj := range selectedKinds(scheme) {
			opts.ByObject[obj] = cache.ByObject{Label: s.selector}
		}
	}
	return opts
}

// selectedKinds returns an empty object of each kind in scheme that a
// selector picks resources of: every kflare kind with a Ready condition but
// CloudflareAccount, which every installation caches whole.
func selectedKinds(scheme *runtime.Scheme) []client.Object {
	var kinds []client.Object
	for kind := range resourceLists(scheme) {
		object, err := scheme.New(cloudflarev1alpha1.GroupVersion.WithKind(kind))
		if err != nil {
			continue
		}
		if _, account := object.(*cloudflarev1alpha1.CloudflareAccount); !account {
			kinds = append(kinds, object.(client.Object))
		}
	}
	slices.SortFunc(kinds, func(a, b client.Object) int {
		return strings.Compare(reflect.TypeOf(a).String(), reflect.TypeOf(b).String())
	})
	return kinds
}

// watches reports whether the scope watches namespace.
func (s WatchScope) watches(namespace string) bool {
	return s.namespaces == nil || slices.Contains(s.namespaces, namespace)
}

// selects reports whether obj's labels match the scope's selector.
func (s WatchScope) selects(obj client.Object) bool {
	return s.selector == nil || s.selector.Matches(labels.Set(obj.GetLabels()))
}

// accountOutside returns why the installation does not manage account, or ""
// when it does.
func (s WatchScope) accountOutside(account *cloudflarev1alpha1.CloudflareAccount) string {
	if !s.selects(account) {
		return s.labelsOutside()
	}
	if namespace := account.Spec.TokenSecretRef.Namespace; !s.watches(namespace) {
		return fmt.Sprintf("its token Secret is in namespace %q, which %s", namespace, s.namespacesOutside())
	}
	return ""
}

// labelsOutside says why an object whose labels the selector does not match
// is outside the scope.
func (s WatchScope) labelsOutside() string {
	return fmt.Sprintf("its labels do not match this installation's selector %s", s.selector)
}

// namespacesOutside completes a sentence about a namespace outside the scope,
// naming the namespaces it watches.
func (s WatchScope) namespacesOutside() string {
	listed := s.namespaces
	more := ""
	if len(listed) > maxListed {
		listed, more = listed[:maxListed], fmt.Sprintf(" and %d more", len(s.namespaces)-maxListed)
	}
	return fmt.Sprintf("this installation does not watch (it watches %s%s)", strings.Join(listed, ", "), more)
}

// accountsElsewhere returns the names of the CloudflareAccounts for the
// Cloudflare account accountID that this installation does not manage,
// sorted. Another installation may manage objects of that Cloudflare account
// through one of them, which this one cannot tell: what keeps two resources
// from managing one object, a claim or a settings owner, counts the resources
// of one installation only. Every installation caches every account.
func (s WatchScope) accountsElsewhere(ctx context.Context, c client.Reader, accountID string) ([]string, error) {
	accounts := &cloudflarev1alpha1.CloudflareAccountList{}
	if err := c.List(ctx, accounts); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(accounts.Items))
	for i := range accounts.Items {
		if account := &accounts.Items[i]; account.Spec.AccountID == accountID && s.accountOutside(account) != "" {
			names = append(names, account.Name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// eitherOf lists names, quoted, for a sentence that means any one of them:
// "a", "a" or "b", "a", "b" or "c". It lists maxListed of them at most, and
// then how many more there are.
func eitherOf(names []string) string {
	listed := names[:min(len(names), maxListed)]
	quoted := make([]string, len(listed))
	for i, name := range listed {
		quoted[i] = strconv.Quote(name)
	}
	if more := len(names) - len(listed); more > 0 {
		return fmt.Sprintf("%s or %d more", strings.Join(quoted, ", "), more)
	}
	last := len(quoted) - 1
	if last < 1 {
		return strings.Join(quoted, "")
	}
	return strings.Join(quoted[:last], ", ") + " or " + quoted[last]
}

// notWatched returns the condition of a resource that needs what, such as
// CloudflareAccount "main", which why says is outside its installation's
// scope.
func notWatched(what, why string) *conditionError {
	return &conditionError{
		Reason:  reasonNotWatched,
		Message: fmt.Sprintf("%s is outside what this kflare installation watches: %s", what, why),
	}
}

// outsideReference returns why a resource cannot use the object key it
// references, what (such as Zone "web"), which the manager's cache read into
// obj, or failed to with err: the object is outside the installation's scope.
// It returns nil for an object inside it, and for one that is missing: err is
// NotFound, and the API server has no object the selector leaves out either.
func outsideReference(ctx context.Context, key types.NamespacedName, obj client.Object, what string,
	err error) *conditionError {
	scope := watchScopeFrom(ctx)
	if !scope.watches(key.Namespace) {
		return notWatched(what, fmt.Sprintf("it is in namespace %q, which %s", key.Namespace, scope.namespacesOutside()))
	}
	switch {
	case err == nil:
		if !scope.selects(obj) {
			return notWatched(what, scope.labelsOutside())
		}
	case apierrors.IsNotFound(err) && scope.selector != nil && scope.reader != nil:
		// The cache holds only what the selector picks, so an object it leaves
		// out is there all the same.
		uncached := obj.DeepCopyObject().(client.Object)
		if scope.reader.Get(ctx, key, uncached) == nil && !scope.selects(uncached) {
			return notWatched(what, scope.labelsOutside())
		}
	}
	return nil
}

// getReference reads into obj the object key that a resource references,
// what (such as Zone "web"), and returns why the resource cannot use it: it is
// outside the installation's scope (reason NotWatched), or the manager's cache
// cannot read it (notFoundReason, such as ZoneNotFound).
func getReference(ctx context.Context, c client.Reader, key types.NamespacedName, obj client.Object,
	what, notFoundReason string) *conditionError {
	err := c.Get(ctx, key, obj)
	if condErr := outsideReference(ctx, key, obj, what, err); condErr != nil {
		return condErr
	}
	if err != nil {
		return &conditionError{Reason: notFoundReason, Message: fmt.Sprintf("%s not found: %v", what, err)}
	}
	return nil
}

// watchScopeKey is the context key of the installation's WatchScope.
type watchScopeKey struct{}

// WithWatchScope returns a copy of ctx that carries scope. cmd/main.go gives
// the manager such a base context, so that every reconcile and watch handler
// of its controllers manages what scope says.
func WithWatchScope(ctx context.Context, scope WatchScope) context.Context {
	return context.WithValue(ctx, watchScopeKey{}, scope)
}

// watchScopeFrom returns the WatchScope ctx carries, or the zero one, which
// manages everything.
func watchScopeFrom(ctx context.Context) WatchScope {
	scope, _ := ctx.Value(watchScopeKey{}).(WatchScope)
	return scope
}
