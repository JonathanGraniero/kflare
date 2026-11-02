/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cloudflare/cloudflare-go/v7/kv"
	"github.com/cloudflare/cloudflare-go/v7/managed_transforms"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/cloudflare/cloudflare-go/v7/rulesets"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/yaml"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
)

// served adds up the series of the metric name, among those with labels, that
// gatherer serves: 0 for a metric it does not serve.
func served(gatherer prometheus.Gatherer, name string, labels map[string]string) float64 {
	GinkgoHelper()
	families, err := gatherer.Gather()
	Expect(err).NotTo(HaveOccurred(), "the scrape failed")
	total := 0.0
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, series := range family.GetMetric() {
			have := map[string]string{}
			for _, label := range series.GetLabel() {
				have[label.GetName()] = label.GetValue()
			}
			matches := true
			for label, value := range labels {
				matches = matches && have[label] == value
			}
			if matches {
				total += series.GetCounter().GetValue() + series.GetGauge().GetValue()
			}
		}
	}
	return total
}

// resourceSeries returns the series of kflare_resources that gatherer serves
// for kind, as "<ready>/<reason>".
func resourceSeries(gatherer prometheus.Gatherer, kind string) map[string]float64 {
	GinkgoHelper()
	families, err := gatherer.Gather()
	Expect(err).NotTo(HaveOccurred(), "the scrape failed")
	series := map[string]float64{}
	for _, family := range families {
		if family.GetName() != "kflare_resources" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["kind"] == kind {
				series[labels["ready"]+"/"+labels["reason"]] = metric.GetGauge().GetValue()
			}
		}
	}
	return series
}

// corrections reads kflare_drift_corrections_total for kind.
func corrections(kind string) float64 {
	return testutil.ToFloat64(driftCorrections.WithLabelValues(kind))
}

// closedChannel stands for a manager that leads.
func closedChannel() <-chan struct{} {
	elected := make(chan struct{})
	close(elected)
	return elected
}

// crdKinds returns the kinds of the CRDs kflare installs.
func crdKinds() []string {
	GinkgoHelper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "config", "crd", "bases", "*.yaml"))
	Expect(err).NotTo(HaveOccurred())
	Expect(paths).NotTo(BeEmpty())
	kinds := make([]string, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		crd := &apiextensionsv1.CustomResourceDefinition{}
		Expect(yaml.Unmarshal(data, crd)).To(Succeed())
		kinds = append(kinds, crd.Spec.Names.Kind)
	}
	return kinds
}

// listFunc is a client.Reader that only lists.
type listFunc func(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error

func (f listFunc) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return f(ctx, list, opts...)
}

func (f listFunc) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return errors.New("the collector only lists")
}

var _ = Describe("kflare's metrics", func() {
	ctx := context.Background()

	Describe("a controller's calls to Cloudflare, over HTTP", func() {
		const (
			name    = "metrics-http"
			account = "metrics-http-account"
			zone    = "metrics-http-zone"
			ns      = "default"
		)
		var cloudflare *httptest.Server
		var calls atomic.Int64
		var v7Options func(token string) []option.RequestOption

		BeforeEach(func() {
			// Every answer carries a new ID, as Cloudflare's do.
			calls.Store(0)
			cloudflare = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cf-Ray", fmt.Sprintf("%016x-YYZ", calls.Add(1)))
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"result":null,"success":false,"errors":[{"code":9109,"message":"refused"}],` +
					`"messages":[]}`))
			}))
			DeferCleanup(cloudflare.Close)
			v7Options = func(token string) []option.RequestOption {
				client, err := cfpkg.New(token)
				Expect(err).NotTo(HaveOccurred())
				return append(client.V7Options(), option.WithBaseURL(cloudflare.URL+"/"))
			}

			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: account, Namespace: ns},
				Data:       map[string][]byte{"CF_API_TOKEN": fmt.Appendf(nil, "metrics-%d", heldTokens.Add(1))},
			})).To(Succeed())
			DeferCleanup(forceDelete, ctx, &corev1.Secret{}, types.NamespacedName{Name: account, Namespace: ns})
		})

		// createAccount creates the CloudflareAccount, which a controller
		// reports on or, set ready, the resources use.
		createAccount := func(ready bool) *cloudflarev1alpha1.CloudflareAccount {
			GinkgoHelper()
			obj := &cloudflarev1alpha1.CloudflareAccount{
				ObjectMeta: metav1.ObjectMeta{Name: account},
				Spec: cloudflarev1alpha1.CloudflareAccountSpec{
					AccountID:      "acct-metrics",
					TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: account, Namespace: ns, Key: "CF_API_TOKEN"},
				},
			}
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: account})
			if ready {
				Expect(updateReady(ctx, k8sClient, obj, "Validated", "valid")).To(Succeed())
			}
			return obj
		}
		// createZone creates a ready Zone of the account.
		createZone := func() {
			GinkgoHelper()
			createAccount(true)
			obj := &cloudflarev1alpha1.Zone{
				ObjectMeta: metav1.ObjectMeta{Name: zone, Namespace: ns},
				Spec: cloudflarev1alpha1.ZoneSpec{
					AccountRef:     corev1.LocalObjectReference{Name: account},
					ZoneSpecExtras: cloudflarev1alpha1.ZoneSpecExtras{Name: "metrics-example.com"},
				},
			}
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.Zone{}, types.NamespacedName{Name: zone, Namespace: ns})
			obj.Status.CloudflareMetadata.ZoneID = "metrics-zone-id"
			Expect(updateReady(ctx, k8sClient, obj, "Synced", "synced")).To(Succeed())
		}

		DescribeTable("count under the name controller-runtime gives the controller, and settle on one ray ID",
			func(controller string, resource func() conditionedObject, setup func(mgr manager.Manager) error) {
				obj := resource()
				requests := map[string]string{"controller": controller, "method": http.MethodGet, "code": "403"}
				reconciles := map[string]string{"controller": controller}
				requestsBefore := served(metrics.Registry, "kflare_cloudflare_api_requests_total", requests)
				reconcilesBefore := served(metrics.Registry, "controller_runtime_reconcile_total", reconciles)

				startManager(ctx, setup)

				condition := func() *metav1.Condition {
					current := obj.DeepCopyObject().(conditionedObject)
					Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), current)).To(Succeed())
					return meta.FindStatusCondition(current.GetConditions(), cloudflarev1alpha1.ConditionReady)
				}
				Eventually(func() string {
					if ready := condition(); ready != nil {
						return ready.Reason
					}
					return ""
				}, "10s", "20ms").Should(Equal("TerminalError"))
				reported := condition().Message
				Expect(reported).To(MatchRegexp(`refused \(9109\).* \(cf-ray [0-9a-f]{16}-YYZ\)$`))

				By("not reconciling again for every new ID")
				// A finalizer, a Secret's protection and the status each queue a
				// reconcile at the start. A new message per call has reached 2,862
				// calls in 5 seconds, or the token's burst of 30 with the rate limit.
				Consistently(calls.Load, "1500ms", "100ms").Should(BeNumerically("<=", 8))
				Expect(condition().Message).To(Equal(reported))

				By("counting every call under the controller's name")
				Eventually(func() float64 {
					return served(metrics.Registry, "kflare_cloudflare_api_requests_total", requests) - requestsBefore
				}).Should(Equal(float64(calls.Load())))
				Expect(served(metrics.Registry, "controller_runtime_reconcile_total", reconciles)).
					To(BeNumerically(">", reconcilesBefore))
			},
			Entry("of the shared CRUD reconciler, through v7", "kvnamespace",
				func() conditionedObject {
					createAccount(true)
					obj := &cloudflarev1alpha1.KVNamespace{
						ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
						Spec: cloudflarev1alpha1.KVNamespaceSpec{
							Title: name, AccountRef: corev1.LocalObjectReference{Name: account},
						},
					}
					Expect(k8sClient.Create(ctx, obj)).To(Succeed())
					DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.KVNamespace{}, client.ObjectKeyFromObject(obj))
					return obj
				},
				func(mgr manager.Manager) error {
					return (&KVNamespaceReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
						NewKVNamespaceAPI: func(token string) (KVNamespaceAPI, error) {
							return kv.NewNamespaceService(v7Options(token)...), nil
						}}).SetupWithManager(mgr)
				}),
			Entry("of the shared rule controller, through v7", "wafcustomrule",
				func() conditionedObject {
					createZone()
					obj := &cloudflarev1alpha1.WAFCustomRule{
						ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
						Spec: cloudflarev1alpha1.WAFCustomRuleSpec{
							RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
								ZoneRef: corev1.LocalObjectReference{Name: zone}, Expression: "true",
							},
							Action: "block",
						},
					}
					Expect(k8sClient.Create(ctx, obj)).To(Succeed())
					DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.WAFCustomRule{}, client.ObjectKeyFromObject(obj))
					return obj
				},
				func(mgr manager.Manager) error {
					return (&WAFCustomRuleReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
						NewRulesetRuleAPI: func(token string) (RulesetRuleAPI, error) {
							service := rulesets.NewRulesetService(v7Options(token)...)
							return RulesetRuleAPI{Phases: service.Phases, Rulesets: service, Rules: service.Rules}, nil
						}}).SetupWithManager(mgr)
				}),
			Entry("of the shared settings controller, through v7", "managedtransform",
				func() conditionedObject {
					createZone()
					obj := &cloudflarev1alpha1.ManagedTransform{
						ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
						Spec: cloudflarev1alpha1.ManagedTransformSpec{
							SettingsSpec: cloudflarev1alpha1.SettingsSpec{
								ZoneRef: corev1.LocalObjectReference{Name: zone},
							},
						},
					}
					Expect(k8sClient.Create(ctx, obj)).To(Succeed())
					DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.ManagedTransform{}, client.ObjectKeyFromObject(obj))
					return obj
				},
				func(mgr manager.Manager) error {
					return (&ManagedTransformReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
						NewManagedTransformAPI: func(token string) (ManagedTransformAPI, error) {
							return managed_transforms.NewManagedTransformService(v7Options(token)...), nil
						}}).SetupWithManager(mgr)
				}),
			Entry("of the CloudflareAccount controller, through v0", "cloudflareaccount",
				func() conditionedObject { return createAccount(false) },
				func(mgr manager.Manager) error {
					return (&CloudflareAccountReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
						NewCFClient: func(token string) (CloudflareAccountAPI, error) {
							client, err := cfpkg.New(token)
							if err != nil {
								return nil, err
							}
							client.BaseURL = cloudflare.URL
							return client, nil
						}}).SetupWithManager(mgr)
				}),
		)
	})

	Describe("kflare_resources", func() {
		const ns = "default"

		kvNamespace := func(name string) *cloudflarev1alpha1.KVNamespace {
			return &cloudflarev1alpha1.KVNamespace{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: cloudflarev1alpha1.KVNamespaceSpec{
					Title: name, AccountRef: corev1.LocalObjectReference{Name: "metrics-resources"},
				},
			}
		}

		It("follows resources as they are created, change state and are deleted, from the manager's cache", func() {
			var reader client.Reader
			startManager(ctx, func(mgr manager.Manager) error {
				reader = mgr.GetClient()
				return nil
			})
			registry := prometheus.NewPedanticRegistry()
			registry.MustRegister(newResourceCollector(reader, scheme.Scheme, closedChannel()))
			series := func() map[string]float64 { return resourceSeries(registry, "KVNamespace") }
			Eventually(series, "20s").Should(BeEmpty(), "another spec left KVNamespaces behind")

			By("counting a resource no reconcile has reached as Unknown")
			first, second := kvNamespace("metrics-first"), kvNamespace("metrics-second")
			for _, obj := range []*cloudflarev1alpha1.KVNamespace{first, second} {
				Expect(k8sClient.Create(ctx, obj)).To(Succeed())
				DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.KVNamespace{}, client.ObjectKeyFromObject(obj))
			}
			Eventually(series).Should(Equal(map[string]float64{"Unknown/": 2}))

			By("telling resources apart by their condition's status and reason")
			Expect(updateReady(ctx, k8sClient, first, "Synced", "synced")).To(Succeed())
			Expect(updateNotReady(ctx, k8sClient, second, "TerminalError", "refused")).To(Succeed())
			Eventually(series).Should(Equal(map[string]float64{"True/Synced": 1, "False/TerminalError": 1}))

			By("following a change")
			Expect(updateNotReady(ctx, k8sClient, first, "TerminalError", "refused")).To(Succeed())
			Eventually(series).Should(Equal(map[string]float64{"False/TerminalError": 2}))

			By("following a paused resource, whose status stays as it was")
			Expect(updatePaused(ctx, k8sClient, first)).To(Succeed())
			Eventually(series).Should(Equal(map[string]float64{"False/TerminalError": 1, "False/Paused": 1}))

			By("dropping what is deleted")
			Expect(k8sClient.Delete(ctx, first)).To(Succeed())
			Eventually(series).Should(Equal(map[string]float64{"False/TerminalError": 1}))
			Expect(k8sClient.Delete(ctx, second)).To(Succeed())
			Eventually(series).Should(BeEmpty())
		})

		It("leaves the cache's objects as they are", func() {
			obj := kvNamespace("metrics-cache")
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.KVNamespace{}, client.ObjectKeyFromObject(obj))

			var options []client.ListOption
			reader := listFunc(func(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
				options = opts
				return k8sClient.List(ctx, list)
			})
			Expect(resourceSeries(testGatherer(newResourceCollector(reader, scheme.Scheme, closedChannel())),
				"KVNamespace")).To(HaveKey("Unknown/"))
			Expect(options).To(ContainElement(client.UnsafeDisableDeepCopy),
				"a scrape would copy every resource to read one condition of each")
		})

		It("answers only on the leader, and starts no informer on a standby", func() {
			obj := kvNamespace("metrics-standby")
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.KVNamespace{}, client.ObjectKeyFromObject(obj))

			lists := 0
			reader := listFunc(func(ctx context.Context, list client.ObjectList, _ ...client.ListOption) error {
				lists++
				return k8sClient.List(ctx, list)
			})
			elected := make(chan struct{})
			collector := newResourceCollector(reader, scheme.Scheme, elected)

			Expect(resourceSeries(testGatherer(collector), "KVNamespace")).To(BeEmpty())
			Expect(lists).To(BeZero(), "a list on a standby starts an informer for every kind")

			close(elected)
			Expect(resourceSeries(testGatherer(collector), "KVNamespace")).To(Equal(map[string]float64{"Unknown/": 1}))
		})

		It("leaves out a kind it cannot count, rather than fail the scrape", func() {
			obj := kvNamespace("metrics-failing")
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.KVNamespace{}, client.ObjectKeyFromObject(obj))
			other := &cloudflarev1alpha1.D1Database{
				ObjectMeta: metav1.ObjectMeta{Name: "metrics-failing", Namespace: ns},
				Spec: cloudflarev1alpha1.D1DatabaseSpec{
					Name: "metrics-failing", AccountRef: corev1.LocalObjectReference{Name: "metrics-resources"},
				},
			}
			Expect(k8sClient.Create(ctx, other)).To(Succeed())
			DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.D1Database{}, client.ObjectKeyFromObject(other))

			reader := listFunc(func(ctx context.Context, list client.ObjectList, _ ...client.ListOption) error {
				if _, failing := list.(*cloudflarev1alpha1.D1DatabaseList); failing {
					return errors.New("the cache is late")
				}
				return k8sClient.List(ctx, list)
			})
			gatherer := testGatherer(newResourceCollector(reader, scheme.Scheme, closedChannel()))
			Expect(resourceSeries(gatherer, "D1Database")).To(BeEmpty())
			Expect(resourceSeries(gatherer, "KVNamespace")).To(HaveKey("Unknown/"))
		})

		It("gives up on a cache that does not answer, after collectTimeout", func() {
			var deadline time.Time
			reader := listFunc(func(ctx context.Context, _ client.ObjectList, _ ...client.ListOption) error {
				deadline, _ = ctx.Deadline()
				return ctx.Err()
			})
			start := time.Now()
			_, err := testGatherer(newResourceCollector(reader, scheme.Scheme, closedChannel())).Gather()
			Expect(err).NotTo(HaveOccurred())
			Expect(deadline).To(BeTemporally("~", start.Add(collectTimeout), time.Second))
		})

		It("covers every kind, and only kinds", func() {
			// As cmd/main.go builds it, without the kinds only the tests know.
			production := runtime.NewScheme()
			Expect(cloudflarev1alpha1.AddToScheme(production)).To(Succeed())
			Expect(slices.Collect(maps.Keys(resourceLists(production)))).To(ConsistOf(crdKinds()))
			for kind, list := range resourceLists(production) {
				Expect(fmt.Sprintf("%T", list)).To(Equal("*v1alpha1." + kind + "List"))
			}

			By("leaving out a kind without a Ready condition")
			version := cloudflarev1alpha1.GroupVersion
			production.AddKnownTypeWithName(version.WithKind("Unconditioned"), &corev1.Secret{})
			production.AddKnownTypeWithName(version.WithKind("UnconditionedList"), &corev1.SecretList{})
			Expect(resourceLists(production)).NotTo(HaveKey("Unconditioned"))
		})

		It("is served once with the others, and starts every kind's corrections at 0", func() {
			startManager(ctx, func(mgr manager.Manager) error {
				Expect(SetupMetrics(mgr)).To(Succeed())
				DeferCleanup(func() {
					metrics.Registry.Unregister(newResourceCollector(mgr.GetClient(), mgr.GetScheme(), mgr.Elected()))
				})
				var registered prometheus.AlreadyRegisteredError
				Expect(errors.As(SetupMetrics(mgr), &registered)).To(BeTrue(), "the second call must say so")
				return nil
			})
			obj := kvNamespace("metrics-served")
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.KVNamespace{}, client.ObjectKeyFromObject(obj))

			Eventually(func() map[string]float64 {
				return resourceSeries(metrics.Registry, "KVNamespace")
			}, "20s").Should(Equal(map[string]float64{"Unknown/": 1}))

			families, err := metrics.Registry.Gather()
			Expect(err).NotTo(HaveOccurred())
			var kinds []string
			for _, family := range families {
				if family.GetName() != "kflare_drift_corrections_total" {
					continue
				}
				for _, series := range family.GetMetric() {
					kinds = append(kinds, series.GetLabel()[0].GetValue())
				}
			}
			Expect(kinds).To(ContainElements(crdKinds()))
		})
	})

	Describe("recordDriftCorrection", func() {
		DescribeTable("counts a write only for a resource that was in sync",
			func(condition *metav1.Condition, deleting bool, want float64) {
				kind := fmt.Sprintf("spec-kind-%d", heldTokens.Add(1))
				obj := &cloudflarev1alpha1.KVNamespace{ObjectMeta: metav1.ObjectMeta{Generation: 2}}
				if deleting {
					obj.DeletionTimestamp = new(metav1.Now())
				}
				if condition != nil {
					condition.Type = cloudflarev1alpha1.ConditionReady
					obj.Status.Conditions = []metav1.Condition{*condition}
				}
				recordDriftCorrection(kind, obj)
				Expect(corrections(kind)).To(Equal(want))
			},
			Entry("Ready for its current spec: something changed in Cloudflare",
				&metav1.Condition{Status: metav1.ConditionTrue, ObservedGeneration: 2}, false, 1.0),
			Entry("a new resource", nil, false, 0.0),
			Entry("a changed spec", &metav1.Condition{Status: metav1.ConditionTrue, ObservedGeneration: 1}, false, 0.0),
			Entry("a retry after a failure", &metav1.Condition{Status: metav1.ConditionFalse, ObservedGeneration: 2},
				false, 0.0),
			Entry("a resource being deleted", &metav1.Condition{Status: metav1.ConditionTrue, ObservedGeneration: 2},
				true, 0.0),
		)
	})

	Describe("withControllerName", func() {
		It("names the kind's controller as the builder does", func() {
			var got context.Context
			rec := withControllerName("WAFCustomRule", reconcile.Func(func(ctx context.Context,
				_ reconcile.Request) (reconcile.Result, error) {
				got = ctx
				return reconcile.Result{RequeueAfter: time.Minute}, errors.New("failed")
			}))
			result, err := rec.Reconcile(ctx, reconcile.Request{})
			Expect(err).To(MatchError("failed"), "the reconciler's error is passed on")
			Expect(result.RequeueAfter).To(Equal(time.Minute), "the reconciler's result is passed on")
			Expect(got).To(Equal(cfpkg.WithController(ctx, strings.ToLower("WAFCustomRule"))))
		})
	})
})

// testGatherer serves what collector collects, as checked as a scrape can be.
func testGatherer(collector prometheus.Collector) prometheus.Gatherer {
	GinkgoHelper()
	registry := prometheus.NewPedanticRegistry()
	Expect(registry.Register(collector)).To(Succeed())
	return registry
}
