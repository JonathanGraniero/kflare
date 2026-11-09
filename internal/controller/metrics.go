/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
)

// collectTimeout bounds what a scrape waits for the manager's cache: a new
// leader's caches may still be filling.
const collectTimeout = 5 * time.Second

var driftCorrections = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "kflare_drift_corrections_total",
	Help: "Writes to Cloudflare made because it differed from a spec that had not changed, by kind.",
}, []string{"kind"})

// resourcesDesc is kflare_resources.
var resourcesDesc = prometheus.NewDesc("kflare_resources",
	"Resources by kind, and by the status and reason of their Ready condition. Served by the leader only.",
	[]string{"kind", "ready", "reason"}, nil)

// tokenExpiryDesc is kflare_account_token_expiry_timestamp_seconds.
var tokenExpiryDesc = prometheus.NewDesc("kflare_account_token_expiry_timestamp_seconds",
	"When each CloudflareAccount's API token expires, as Cloudflare last reported it, in seconds since the Unix "+
		"epoch. An account whose token never expires has none. Served by the leader only.",
	[]string{"account"}, nil)

func init() {
	metrics.Registry.MustRegister(driftCorrections)
}

// recordDriftCorrection counts a write to Cloudflare for obj, of the named
// kind, if obj was in sync when its reconcile started: its spec has not
// changed since kflare last wrote it, so the difference arose in Cloudflare.
// A path that writes on a difference calls it once the write succeeds. One
// that also writes when something else than the spec changes, as a Worker does
// when its ConfigMap does, leaves those writes out (crudHooks.sourceChanged).
func recordDriftCorrection(kind string, obj conditionedObject) {
	if inSync(obj) {
		driftCorrections.WithLabelValues(kind).Inc()
	}
}

// withControllerName makes rec's calls to Cloudflare count under the
// controller of kind, by the name controller-runtime's own metrics give it:
// without Named, the builder takes the kind in lower case.
func withControllerName(kind string, rec reconcile.Reconciler) reconcile.Reconciler {
	name := strings.ToLower(kind)
	return reconcile.Func(func(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
		return rec.Reconcile(cfpkg.WithController(ctx, name), req)
	})
}

// resourceCollector serves kflare_resources and
// kflare_account_token_expiry_timestamp_seconds. It reads them when scraped,
// from the manager's cache, so nothing keeps them up to date between scrapes,
// and they hold across restarts and deletions.
//
// Only the leader answers. A standby has none of these informers, and a list
// would start them all, on a pod meant to sit idle; and a sum across pods
// would count each resource once per replica.
//
// It counts what the installation manages: the cache holds nothing else, but
// for the CloudflareAccounts of other installations, which scope leaves out.
type resourceCollector struct {
	reader client.Reader
	// lists holds an empty list of each kind with a Ready condition, by kind.
	lists map[string]client.ObjectList
	// elected closes once this manager leads.
	elected <-chan struct{}
	scope   WatchScope
}

func newResourceCollector(reader client.Reader, scheme *runtime.Scheme, elected <-chan struct{},
	scope WatchScope) *resourceCollector {
	return &resourceCollector{reader: reader, lists: resourceLists(scheme), elected: elected, scope: scope}
}

// resourceLists returns an empty list of each kind with a Ready condition in
// scheme, so that a new kind needs no entry here.
func resourceLists(scheme *runtime.Scheme) map[string]client.ObjectList {
	lists := map[string]client.ObjectList{}
	version := cloudflarev1alpha1.GroupVersion
	for kind := range scheme.KnownTypes(version) {
		object, _ := scheme.New(version.WithKind(kind))
		listObject, _ := scheme.New(version.WithKind(kind + "List"))
		_, conditioned := object.(conditionedObject)
		if list, ok := listObject.(client.ObjectList); ok && conditioned {
			lists[kind] = list
		}
	}
	return lists
}

func (c *resourceCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- resourcesDesc
	ch <- tokenExpiryDesc
}

// Collect leaves out a kind it cannot count. An invalid metric would fail the
// whole scrape, and with it every other metric, when one kind's cache is late.
func (c *resourceCollector) Collect(ch chan<- prometheus.Metric) {
	select {
	case <-c.elected:
	default:
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), collectTimeout)
	defer cancel()
	// The expiries first, so that a kind whose cache is late cannot use up
	// the time an alert on a token depends on.
	c.collectTokenExpiries(ctx, ch)
	for _, kind := range slices.Sorted(maps.Keys(c.lists)) {
		counts, err := c.count(ctx, kind)
		if err != nil {
			log.Log.WithName("metrics").Error(err, "Leaving a kind out of kflare_resources", "kind", kind)
			continue
		}
		for state, count := range counts {
			ch <- prometheus.MustNewConstMetric(resourcesDesc, prometheus.GaugeValue, float64(count), kind,
				state.status, state.reason)
		}
	}
}

// collectTokenExpiries sends the expiry of each token that the installation's
// CloudflareAccounts record (status.tokenExpiresAt). The label is the
// account's name: accounts are few, and an alert must say which token to
// replace.
func (c *resourceCollector) collectTokenExpiries(ctx context.Context, ch chan<- prometheus.Metric) {
	accounts := &cloudflarev1alpha1.CloudflareAccountList{}
	if err := c.reader.List(ctx, accounts, client.UnsafeDisableDeepCopy); err != nil {
		log.Log.WithName("metrics").Error(err, "Leaving out kflare_account_token_expiry_timestamp_seconds")
		return
	}
	for i := range accounts.Items {
		account := &accounts.Items[i]
		expiresAt := account.Status.TokenExpiresAt
		if expiresAt == nil || c.scope.accountOutside(account) != "" {
			continue
		}
		ch <- prometheus.MustNewConstMetric(tokenExpiryDesc, prometheus.GaugeValue,
			float64(expiresAt.Unix()), account.Name)
	}
}

// readyState is what kflare_resources tells a kind's resources apart by.
type readyState struct {
	status, reason string
}

// count returns how many resources of kind are in each state. A resource
// without a Ready condition, which no reconcile has reached yet, counts as
// Unknown.
//
// The list reads the cache's own objects: a scrape would otherwise copy every
// resource, every time, to read one condition of each, and nothing here keeps
// or changes them.
func (c *resourceCollector) count(ctx context.Context, kind string) (map[readyState]int, error) {
	list := c.lists[kind].DeepCopyObject().(client.ObjectList)
	if err := c.reader.List(ctx, list, client.UnsafeDisableDeepCopy); err != nil {
		return nil, err
	}
	counts := map[readyState]int{}
	err := meta.EachListItem(list, func(item runtime.Object) error {
		if account, ok := item.(*cloudflarev1alpha1.CloudflareAccount); ok && c.scope.accountOutside(account) != "" {
			return nil
		}
		state := readyState{status: string(metav1.ConditionUnknown)}
		conditions := item.(conditionedObject).GetConditions()
		if ready := meta.FindStatusCondition(conditions, cloudflarev1alpha1.ConditionReady); ready != nil {
			state = readyState{status: string(ready.Status), reason: ready.Reason}
		}
		counts[state]++
		return nil
	})
	return counts, err
}

// SetupMetrics serves kflare_resources from mgr's cache, counting what scope,
// the installation's, manages, and starts kflare_drift_corrections_total at 0
// for every kind, so that a rule on its increase sees the first correction.
// cmd/main.go calls it once, before the manager starts.
func SetupMetrics(mgr ctrl.Manager, scope WatchScope) error {
	collector := newResourceCollector(mgr.GetClient(), mgr.GetScheme(), mgr.Elected(), scope)
	for kind := range collector.lists {
		driftCorrections.WithLabelValues(kind)
	}
	return metrics.Registry.Register(collector)
}
