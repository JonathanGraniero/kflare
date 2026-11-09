/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/cloudflare/cloudflare-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// instanceLabel is the label the specs' scopes select resources by.
const instanceLabel = "kflare.dev/instance"

// mustWatchScope returns the scope NewWatchScope parses from a spec's flags,
// which must be valid.
func mustWatchScope(namespaces, selector string, reader client.Reader) WatchScope {
	GinkgoHelper()
	scope, err := NewWatchScope(namespaces, selector, reader)
	Expect(err).NotTo(HaveOccurred())
	return scope
}

// scopedAccount returns a ready CloudflareAccount named name, with labels,
// whose token Secret is in secretNamespace.
func scopedAccount(name string, labels map[string]string, secretNamespace string) *cloudflarev1alpha1.CloudflareAccount {
	return &cloudflarev1alpha1.CloudflareAccount{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec: cloudflarev1alpha1.CloudflareAccountSpec{
			AccountID: "acct-" + name,
			TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: name + "-token", Namespace: secretNamespace,
				Key: "CF_API_TOKEN"},
		},
		Status: cloudflarev1alpha1.CloudflareAccountStatus{Conditions: []metav1.Condition{{
			Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Validated",
			Message: "Credentials are valid", LastTransitionTime: metav1.Now(),
		}}},
	}
}

var _ = Describe("WatchScope", func() {
	ctx := context.Background()

	Describe("NewWatchScope", func() {
		DescribeTable("parses the flags",
			func(namespaces, selector string, wantNamespaces []string, wantSelector string) {
				scope := mustWatchScope(namespaces, selector, nil)
				Expect(scope.namespaces).To(Equal(wantNamespaces))
				if wantSelector == "" {
					// Exactly nil: a parsed blank is an empty selector, which BeNil
					// takes for nil, and which would filter the cache by nothing.
					Expect(scope.selector == nil).To(BeTrue(), "a blank selector parsed: %#v", scope.selector)
					Expect(scope.String()).To(HaveSuffix(", every resource"))
				} else {
					Expect(scope.selector.String()).To(Equal(wantSelector))
				}
			},
			Entry("watches everything without either", "", "", nil, ""),
			Entry("sorts the namespaces, drops repeats and trims spaces", " team-b,team-a , team-b", "",
				[]string{"team-a", "team-b"}, ""),
			Entry("takes a selector in kubectl's syntax", "", "kflare.dev/instance in (b, a),!canary", nil,
				"!canary,kflare.dev/instance in (a,b)"),
			Entry("takes blanks for nothing", "  ", "  ", nil, ""),
		)

		DescribeTable("refuses",
			func(namespaces, selector, want string) {
				_, err := NewWatchScope(namespaces, selector, nil)
				Expect(err).To(MatchError(ContainSubstring(want)))
			},
			Entry("a namespace name that is not a DNS label", "team-a,Team-B", "", `namespace "Team-B"`),
			Entry("an empty name between commas", "team-a,,team-b", "", `namespace ""`),
			Entry("a selector it cannot parse", "", "kflare.dev/instance in (", `selector "kflare.dev/instance in ("`),
		)
	})

	It("says what it manages, for the manager's log", func() {
		Expect(WatchScope{}.String()).To(Equal("every namespace, every resource"))
		Expect(mustWatchScope("team-b,team-a", "kflare.dev/instance=a", nil).String()).To(Equal(
			"namespaces team-a, team-b, resources matching kflare.dev/instance=a"))
	})

	Describe("CacheOptions", func() {
		It("leaves the cache as it is for the zero scope", func() {
			opts := WatchScope{}.CacheOptions(scheme.Scheme)
			Expect(opts.DefaultNamespaces).To(BeNil())
			Expect(opts.ByObject).To(BeNil())
		})

		It("caches only the namespaces it watches", func() {
			opts := mustWatchScope("team-a,team-b", "", nil).CacheOptions(scheme.Scheme)
			Expect(opts.DefaultNamespaces).To(HaveLen(2))
			Expect(opts.DefaultNamespaces).To(HaveKey("team-a"))
			Expect(opts.DefaultNamespaces).To(HaveKey("team-b"))
			Expect(opts.ByObject).To(BeNil())
		})

		It("caches only the resources the selector matches, of every kind but CloudflareAccount", func() {
			opts := mustWatchScope("", "kflare.dev/instance=a", nil).CacheOptions(scheme.Scheme)
			Expect(opts.DefaultNamespaces).To(BeNil())
			kinds := make([]string, 0, len(opts.ByObject))
			for obj, byObject := range opts.ByObject {
				Expect(byObject.Label.String()).To(Equal("kflare.dev/instance=a"))
				Expect(byObject.Namespaces).To(BeNil(), "the cache's defaulting gives each kind the namespaces")
				kinds = append(kinds, reflect.TypeOf(obj).Elem().Name())
			}
			Expect(kinds).To(HaveLen(len(resourceLists(scheme.Scheme))-1), "every kind with a Ready condition")
			Expect(kinds).To(ContainElements("Zone", "DNSRecord", "Tunnel", "TunnelConfiguration", "WorkerScript",
				"KVNamespace", "WAFCustomRule", "ManagedTransform"))
			Expect(kinds).NotTo(ContainElement("CloudflareAccount"))
		})

		It("lists the kinds a selector applies to in the same order every time", func() {
			first := selectedKinds(scheme.Scheme)
			for range 5 {
				Expect(selectedKinds(scheme.Scheme)).To(Equal(first))
			}
		})
	})

	Describe("the CloudflareAccounts it manages", func() {
		labelled := map[string]string{instanceLabel: "a"}

		DescribeTable("by the account's labels and the namespace of its token Secret",
			func(namespaces, selector string, account *cloudflarev1alpha1.CloudflareAccount, want string) {
				Expect(mustWatchScope(namespaces, selector, nil).accountOutside(account)).To(Equal(want))
			},
			Entry("every account, in the zero scope", "", "", scopedAccount("x", nil, "anywhere"), ""),
			Entry("one whose labels match", "", instanceLabel+"=a", scopedAccount("x", labelled, "anywhere"), ""),
			Entry("not one whose labels do not match", "", instanceLabel+"=a", scopedAccount("x", nil, "anywhere"),
				"its labels do not match this installation's selector kflare.dev/instance=a"),
			Entry("one whose token Secret is in a namespace it watches", "team-a", "",
				scopedAccount("x", nil, "team-a"), ""),
			Entry("not one whose token Secret is elsewhere", "team-a", "", scopedAccount("x", nil, "kflare-system"),
				`its token Secret is in namespace "kflare-system", which this installation does not watch (it `+
					`watches team-a)`),
		)

		It("names five of the namespaces it watches at most", func() {
			scope := mustWatchScope("a,b,c,d,e,f,g", "", nil)
			Expect(scope.accountOutside(scopedAccount("x", nil, "h"))).To(HaveSuffix(
				"(it watches a, b, c, d, e and 2 more)"))
			scope = mustWatchScope("a,b,c,d,e", "", nil)
			Expect(scope.accountOutside(scopedAccount("x", nil, "h"))).To(HaveSuffix("(it watches a, b, c, d, e)"))
		})
	})

	Describe("references", func() {
		zoneKey := types.NamespacedName{Name: "web", Namespace: "team-a"}
		zone := func(labels map[string]string) *cloudflarev1alpha1.Zone {
			return &cloudflarev1alpha1.Zone{ObjectMeta: metav1.ObjectMeta{Name: zoneKey.Name,
				Namespace: zoneKey.Namespace, Labels: labels}}
		}
		// apiServer holds what the API server does; the manager's cache, which
		// the selector filters, is empty.
		apiServer := func(objs ...client.Object) client.Reader {
			return fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
		}
		emptyCache := apiServer()
		notFound := apierrors.NewNotFound(cloudflarev1alpha1.GroupVersion.WithResource("zones").GroupResource(), "web")
		in := func(scope WatchScope) context.Context { return WithWatchScope(ctx, scope) }

		It("are inside the zero scope, found or not", func() {
			Expect(outsideReference(ctx, zoneKey, zone(nil), `Zone "web"`, nil)).To(BeNil())
			Expect(outsideReference(ctx, zoneKey, zone(nil), `Zone "web"`, notFound)).To(BeNil())
		})

		It("are outside in a namespace it does not watch, which it reads nothing of", func() {
			scope := mustWatchScope("team-b", "", apiServer(zone(nil)))
			condErr := outsideReference(in(scope), zoneKey, &cloudflarev1alpha1.Zone{}, `Zone "web"`,
				fmt.Errorf("unable to get: %v because of unknown namespace for the cache", zoneKey))
			Expect(condErr).NotTo(BeNil())
			Expect(condErr.Reason).To(Equal(reasonNotWatched))
			Expect(condErr.Message).To(Equal(`Zone "web" is outside what this kflare installation watches: it is in ` +
				`namespace "team-a", which this installation does not watch (it watches team-b)`))
		})

		It("are outside when found with labels the selector does not match", func() {
			scope := mustWatchScope("", instanceLabel+"=a", emptyCache)
			condErr := outsideReference(in(scope), zoneKey, zone(map[string]string{instanceLabel: "b"}), `Zone "web"`, nil)
			Expect(condErr).NotTo(BeNil())
			Expect(condErr.Reason).To(Equal(reasonNotWatched))
			Expect(condErr.Message).To(Equal(`Zone "web" is outside what this kflare installation watches: its ` +
				`labels do not match this installation's selector kflare.dev/instance=a`))
			Expect(outsideReference(in(scope), zoneKey, zone(map[string]string{instanceLabel: "a"}), `Zone "web"`,
				nil)).To(BeNil())
		})

		It("are outside when the cache lacks one the API server holds, with labels the selector does not match", func() {
			scope := mustWatchScope("", instanceLabel+"=a", apiServer(zone(nil)))
			condErr := outsideReference(in(scope), zoneKey, &cloudflarev1alpha1.Zone{}, `Zone "web"`, notFound)
			Expect(condErr).NotTo(BeNil())
			Expect(condErr.Reason).To(Equal(reasonNotWatched))
		})

		It("are missing when the API server lacks them too, or holds one the selector matches", func() {
			scope := mustWatchScope("", instanceLabel+"=a", emptyCache)
			Expect(outsideReference(in(scope), zoneKey, &cloudflarev1alpha1.Zone{}, `Zone "web"`, notFound)).To(BeNil())
			By("holding one the selector matches, which the cache has yet to see")
			scope = mustWatchScope("", instanceLabel+"=a", apiServer(zone(map[string]string{instanceLabel: "a"})))
			Expect(outsideReference(in(scope), zoneKey, &cloudflarev1alpha1.Zone{}, `Zone "web"`, notFound)).To(BeNil())
			By("failing to read it, which reads nothing else")
			scope = mustWatchScope("", instanceLabel+"=a", apiServer(zone(nil)))
			Expect(outsideReference(in(scope), zoneKey, &cloudflarev1alpha1.Zone{}, `Zone "web"`,
				apierrors.NewServiceUnavailable("down"))).To(BeNil(), "only a NotFound is read again")
		})

		Describe("getReference", func() {
			It("reads the reference", func() {
				read := &cloudflarev1alpha1.Zone{}
				Expect(getReference(ctx, apiServer(zone(nil)), zoneKey, read, `Zone "web"`, "ZoneNotFound")).To(BeNil())
				Expect(read.Name).To(Equal("web"))
			})

			It("reports a missing reference with the reason it is given, and the error", func() {
				condErr := getReference(ctx, emptyCache, zoneKey, &cloudflarev1alpha1.Zone{}, `Zone "web"`, "ZoneNotFound")
				Expect(condErr).NotTo(BeNil())
				Expect(condErr.Reason).To(Equal("ZoneNotFound"))
				Expect(condErr.Message).To(HavePrefix(`Zone "web" not found: `))
				Expect(condErr.Message).To(ContainSubstring("not found"))
			})

			It("reports a reference outside the scope as NotWatched, found or not", func() {
				scope := mustWatchScope("", instanceLabel+"=a", apiServer(zone(nil)))
				for _, cache := range []client.Reader{emptyCache, apiServer(zone(nil))} {
					condErr := getReference(in(scope), cache, zoneKey, &cloudflarev1alpha1.Zone{}, `Zone "web"`,
						"ZoneNotFound")
					Expect(condErr).NotTo(BeNil())
					Expect(condErr.Reason).To(Equal(reasonNotWatched))
				}
			})
		})
	})

	It("travels in a context, without which everything is in scope", func() {
		Expect(watchScopeFrom(ctx)).To(Equal(WatchScope{}))
		scope := mustWatchScope("team-a", instanceLabel+"=a", nil)
		Expect(watchScopeFrom(WithWatchScope(ctx, scope))).To(Equal(scope))
	})
})

var _ = Describe("An installation's scope and its CloudflareAccounts", func() {
	ctx := context.Background()
	labelled := map[string]string{instanceLabel: "a"}

	Describe("resolveAccountToken", func() {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "scoped-token", Namespace: "team-a"},
			Data:       map[string][]byte{"CF_API_TOKEN": []byte("tok")},
		}
		build := func(objs ...client.Object) client.Client {
			return fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
		}

		It("gives the token of an account the installation manages", func() {
			c := build(scopedAccount("scoped", labelled, "team-a"), secret)
			scope := mustWatchScope("team-a", instanceLabel+"=a", nil)
			account, token, condErr := resolveAccountToken(WithWatchScope(ctx, scope), c, "team-a", "scoped", true)
			Expect(condErr).To(BeNil())
			Expect(account.Name).To(Equal("scoped"))
			Expect(token).To(Equal("tok"))
		})

		DescribeTable("refuses an account outside the scope with NotWatched, before its other checks",
			func(namespaces, selector string, account *cloudflarev1alpha1.CloudflareAccount, want string) {
				scope := mustWatchScope(namespaces, selector, nil)
				// Neither readiness, nor spec.allowedNamespaces, nor the Secret is
				// what the resource hears about.
				account.Status.Conditions = nil
				account.Spec.AllowedNamespaces = selectorAllowing(map[string]string{"team": "nobody"})
				c := build(account)
				for _, requireReady := range []bool{true, false} {
					got, token, condErr := resolveAccountToken(WithWatchScope(ctx, scope), c, "team-a", "scoped",
						requireReady)
					Expect(condErr).NotTo(BeNil())
					Expect(condErr.Reason).To(Equal(reasonNotWatched))
					Expect(condErr.Message).To(Equal(`CloudflareAccount "scoped" is outside what this kflare ` +
						"installation watches: " + want))
					Expect(got).To(BeNil())
					Expect(token).To(BeEmpty())
				}
			},
			Entry("by its labels", "", instanceLabel+"=a", scopedAccount("scoped", nil, "team-a"),
				"its labels do not match this installation's selector kflare.dev/instance=a"),
			Entry("by its token Secret's namespace", "team-a", "", scopedAccount("scoped", nil, "kflare-system"),
				`its token Secret is in namespace "kflare-system", which this installation does not watch (it `+
					`watches team-a)`),
		)
	})

	Describe("accountsElsewhere", func() {
		// account returns the CloudflareAccount name for the Cloudflare account
		// id, with labels and its token Secret in secretNamespace.
		account := func(name, id string, labels map[string]string,
			secretNamespace string) cloudflarev1alpha1.CloudflareAccount {
			account := scopedAccount(name, labels, secretNamespace)
			account.Spec.AccountID = id
			return *account
		}
		// listing returns a client whose list of CloudflareAccounts gives
		// accounts in their order, or err.
		listing := func(err error, accounts ...cloudflarev1alpha1.CloudflareAccount) client.Client {
			return interceptor.NewClient(fake.NewClientBuilder().WithScheme(scheme.Scheme).Build(), interceptor.Funcs{
				List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
					list.(*cloudflarev1alpha1.CloudflareAccountList).Items = accounts
					return err
				},
			})
		}

		It("names the other accounts for the Cloudflare account that the installation does not manage, sorted",
			func() {
				c := listing(nil,
					account("theirs-z", "acct-1", labelled, "team-b"),
					account("mine", "acct-1", labelled, "team-a"),
					account("unlabelled", "acct-1", nil, "team-a"),
					account("theirs-a", "acct-1", labelled, "kflare-system"),
					account("other-cloudflare-account", "acct-2", labelled, "team-b"),
				)
				scope := mustWatchScope("team-a", instanceLabel+"=a", nil)
				names, err := scope.accountsElsewhere(ctx, c, "acct-1")
				Expect(err).NotTo(HaveOccurred())
				Expect(names).To(Equal([]string{"theirs-a", "theirs-z", "unlabelled"}))
			})

		It("names none in the zero scope, which manages every account", func() {
			c := listing(nil, account("mine", "acct-1", nil, "team-a"), account("other", "acct-1", nil, "team-b"))
			names, err := WatchScope{}.accountsElsewhere(ctx, c, "acct-1")
			Expect(err).NotTo(HaveOccurred())
			Expect(names).To(BeEmpty())
		})

		It("returns the list error", func() {
			_, err := mustWatchScope("team-a", "", nil).accountsElsewhere(ctx, listing(errors.New("no list")),
				"acct-1")
			Expect(err).To(MatchError("no list"))
		})

		DescribeTable("lists names for a sentence that means any one of them",
			func(names []string, want string) {
				Expect(eitherOf(names)).To(Equal(want))
			},
			Entry("none", nil, ""),
			Entry("one", []string{"a"}, `"a"`),
			Entry("two", []string{"a", "b"}, `"a" or "b"`),
			Entry("five", []string{"a", "b", "c", "d", "e"}, `"a", "b", "c", "d" or "e"`),
			Entry("five at most, and how many more", []string{"a", "b", "c", "d", "e", "f", "g"},
				`"a", "b", "c", "d", "e" or 2 more`),
		)
	})

	Describe("leaveCloudflareObject", func() {
		It("lets a deletion through without a call, with a Warning Event, for an account outside the scope", func() {
			recorder := events.NewFakeRecorder(10)
			SetEventRecorder(recorder)
			DeferCleanup(func() { SetEventRecorder(nil) })
			condErr := notWatched(`CloudflareAccount "scoped"`, "its labels do not match")
			Expect(leaveCloudflareObject(ctx, scopedAccount("scoped", nil, "team-a"), condErr)).To(Succeed())
			Expect(recorder.Events).To(Receive(And(ContainSubstring("Warning "+reasonNotWatched),
				ContainSubstring(`CloudflareAccount "scoped" is outside`), ContainSubstring("stays in Cloudflare"))))
		})
	})

	Describe("the CloudflareAccount controller", func() {
		const secretNS = "default"
		var r *CloudflareAccountReconciler

		BeforeEach(func() {
			r = reconcilerWithFake(&fakeCFClient{account: cloudflare.Account{Name: "Scoped Account"}})
		})

		create := func(name string, labels map[string]string) *cloudflarev1alpha1.CloudflareAccount {
			account := scopedAccount(name, labels, secretNS)
			account.Status = cloudflarev1alpha1.CloudflareAccountStatus{}
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: account.Spec.TokenSecretRef.Name, Namespace: secretNS},
				Data:       map[string][]byte{"CF_API_TOKEN": []byte("tok")},
			})).To(Succeed())
			Expect(k8sClient.Create(ctx, account)).To(Succeed())
			DeferCleanup(forceDelete, ctx, &corev1.Secret{},
				types.NamespacedName{Name: account.Spec.TokenSecretRef.Name, Namespace: secretNS})
			DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: name})
			return account
		}
		reconcileIn := func(scope WatchScope, name string) {
			GinkgoHelper()
			_, err := r.Reconcile(WithWatchScope(ctx, scope), reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
			Expect(err).NotTo(HaveOccurred())
		}
		read := func(name string) (*cloudflarev1alpha1.CloudflareAccount, *corev1.Secret) {
			GinkgoHelper()
			account := &cloudflarev1alpha1.CloudflareAccount{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, account)).To(Succeed())
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-token", Namespace: secretNS}, secret)).To(Succeed())
			return account, secret
		}

		It("leaves an account outside the scope alone: no finalizer, status or protection of its Secret", func() {
			create("scoped-outside", nil)
			for _, scope := range []WatchScope{
				mustWatchScope("", instanceLabel+"=a", nil),
				mustWatchScope("team-a", "", nil),
			} {
				reconcileIn(scope, "scoped-outside")
				account, secret := read("scoped-outside")
				Expect(account.Finalizers).To(BeEmpty())
				Expect(account.Status.Conditions).To(BeEmpty())
				Expect(secret.Finalizers).To(BeEmpty())
			}
		})

		It("validates an account inside the scope", func() {
			create("scoped-inside", labelled)
			reconcileIn(mustWatchScope(secretNS, instanceLabel+"=a", nil), "scoped-inside")
			account, secret := read("scoped-inside")
			Expect(account.Finalizers).To(ContainElement(reconciler.Finalizer))
			Expect(account.Status.AccountName).To(Equal("Scoped Account"))
			Expect(isAccountReady(account)).To(BeTrue())
			Expect(secret.Finalizers).To(ContainElement(tokenSecretFinalizer))
		})

		It("leaves the finalizer of a token Secret it no longer watches, with a Warning Event", func() {
			recorder := events.NewFakeRecorder(10)
			SetEventRecorder(recorder)
			DeferCleanup(func() { SetEventRecorder(nil) })

			create("scoped-moved", nil)
			reconcileIn(WatchScope{}, "scoped-moved")
			_, secret := read("scoped-moved")
			Expect(secret.Finalizers).To(ContainElement(tokenSecretFinalizer))
			for len(recorder.Events) > 0 {
				<-recorder.Events
			}

			By("moving its token to a Secret in a namespace an installation watching only that one manages")
			const elsewhere = "scoped-moved-elsewhere"
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: elsewhere}})).To(
				Or(Succeed(), MatchError(ContainSubstring("already exists"))))
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "scoped-moved-token", Namespace: elsewhere},
				Data:       map[string][]byte{"CF_API_TOKEN": []byte("tok")},
			})).To(Succeed())
			DeferCleanup(forceDelete, ctx, &corev1.Secret{},
				types.NamespacedName{Name: "scoped-moved-token", Namespace: elsewhere})
			account, _ := read("scoped-moved")
			account.Spec.TokenSecretRef.Namespace = elsewhere
			Expect(k8sClient.Update(ctx, account)).To(Succeed())
			reconcileIn(mustWatchScope(elsewhere, "", nil), "scoped-moved")

			account, secret = read("scoped-moved")
			Expect(secret.Finalizers).To(ContainElement(tokenSecretFinalizer), "a Secret out of reach was released")
			Expect(*account.Status.ProtectedTokenSecret).To(Equal(corev1.SecretReference{Name: "scoped-moved-token",
				Namespace: elsewhere}))
			Expect(recorder.Events).To(Receive(And(ContainSubstring("Warning "+reasonNotWatched),
				ContainSubstring("Token Secret default/scoped-moved-token keeps its kflare.dev/token-protection "+
					"finalizer"))))
			moved := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "scoped-moved-token", Namespace: elsewhere},
				moved)).To(Succeed())
			Expect(controllerutil.ContainsFinalizer(moved, tokenSecretFinalizer)).To(BeTrue())
		})
	})

	Describe("kflare_resources", func() {
		It("counts the CloudflareAccounts the installation manages, and no other installation's", func() {
			for name, labels := range map[string]map[string]string{"scoped-metrics-a": labelled, "scoped-metrics-b": {
				instanceLabel: "b"}} {
				account := scopedAccount(name, labels, "default")
				status := account.Status
				Expect(k8sClient.Create(ctx, account)).To(Succeed())
				account.Status = status
				Expect(k8sClient.Status().Update(ctx, account)).To(Succeed())
				DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: name})
			}
			count := func(scope WatchScope) float64 {
				return resourceSeries(testGatherer(newResourceCollector(k8sClient, scheme.Scheme, closedChannel(), scope)),
					"CloudflareAccount")["True/Validated"]
			}
			everything := count(WatchScope{})
			Expect(everything).To(BeNumerically(">=", 2))
			Expect(count(mustWatchScope("", instanceLabel+"=a", nil))).To(Equal(1.0))
			Expect(count(mustWatchScope("", instanceLabel+" in (a,b)", nil))).To(Equal(2.0))
		})
	})
})
