/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"time"

	"github.com/cloudflare/cloudflare-go"
	"github.com/cloudflare/cloudflare-go/v7/shared"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
)

// mustSecretScope returns the scope of an installation that watches every
// namespace and may use Secrets only in namespaces, separated by commas.
func mustSecretScope(namespaces string) WatchScope {
	GinkgoHelper()
	scope, err := WatchScope{}.WithSecretNamespaces(namespaces)
	Expect(err).NotTo(HaveOccurred())
	return scope
}

// secretScopedUser returns the config of a new user that holds only what the
// chart grants an installation by default: every rule but those on Secrets
// through a ClusterRole, and the Secret rules through a Role in each of
// namespaces. It returns once the API server applies them.
func secretScopedUser(ctx context.Context, name string, namespaces ...string) *rest.Config {
	GinkgoHelper()
	user, err := testEnv.AddUser(envtest.User{Name: name}, nil)
	Expect(err).NotTo(HaveOccurred())
	subjects := []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: name}}
	objects := make([]client.Object, 0, 2+2*len(namespaces))
	objects = append(objects,
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Rules: chartRules("manager-rules.yaml")},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}, Subjects: subjects,
			RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name}})
	for _, namespace := range namespaces {
		objects = append(objects,
			&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Rules: chartRules("manager-secret-rules.yaml")},
			&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}, Subjects: subjects,
				RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name}})
	}
	for _, obj := range objects {
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(forceDelete, ctx, obj.DeepCopyObject().(client.Object), client.ObjectKeyFromObject(obj))
	}

	userClient, err := client.New(user.Config(), client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Eventually(func(g Gomega) {
		for _, namespace := range namespaces {
			g.Expect(userClient.List(ctx, &corev1.SecretList{}, client.InNamespace(namespace))).To(Succeed())
		}
		g.Expect(userClient.List(ctx, &cloudflarev1alpha1.TunnelList{})).To(Succeed())
	}, 10*time.Second, 100*time.Millisecond).Should(Succeed())
	By(name + " can read the Secrets of no other namespace, nor of all of them at once")
	for _, namespace := range []string{"default", "kube-system", metav1.NamespaceAll} {
		err := userClient.List(ctx, &corev1.SecretList{}, client.InNamespace(namespace))
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "listing Secrets in %q: %v", namespace, err)
	}
	return user.Config()
}

var _ = Describe("An installation that may use Secrets in some namespaces", func() {
	ctx := context.Background()
	const timeout, interval = 15 * time.Second, 100 * time.Millisecond

	Describe("WithSecretNamespaces", func() {
		It("parses the flag, sorted and without repeats, and leaves the rest of the scope", func() {
			scope, err := mustWatchScope("team-a", instanceLabel+"=a", nil).WithSecretNamespaces(" kflare, team-a,kflare ")
			Expect(err).NotTo(HaveOccurred())
			Expect(scope.secretNamespaces).To(Equal([]string{"kflare", "team-a"}))
			Expect(scope.namespaces).To(Equal([]string{"team-a"}))
			Expect(scope.String()).To(Equal("namespaces team-a, resources matching " + instanceLabel +
				"=a, Secrets in namespaces kflare, team-a"))
		})

		It("uses Secrets wherever the installation watches when the flag is empty", func() {
			for _, flag := range []string{"", "  "} {
				scope := mustSecretScope(flag)
				Expect(scope.secretNamespaces).To(BeNil())
				Expect(scope.String()).To(Equal("every namespace, every resource"))
				Expect(scope.CacheOptions(scheme.Scheme).ByObject).To(BeEmpty())
				Expect(secretNamespaceNotAllowed(WithWatchScope(ctx, scope), "Secret", "anywhere")).To(BeNil())
			}
			Expect(secretNamespaceNotAllowed(ctx, "Secret", "anywhere")).To(BeNil(), "a context without a scope")
		})

		It("refuses a name that is not a namespace's", func() {
			_, err := WatchScope{}.WithSecretNamespaces("kflare,Team_A")
			Expect(err).To(MatchError(ContainSubstring(`namespace "Team_A"`)))
		})

		It("caches the Secrets of those namespaces only, whatever else the scope selects", func() {
			opts := mustSecretScope("kflare,team-a").CacheOptions(scheme.Scheme)
			Expect(opts.DefaultNamespaces).To(BeNil())
			Expect(opts.ByObject).To(HaveLen(1))
			for obj, byObject := range opts.ByObject {
				Expect(obj).To(BeAssignableToTypeOf(&corev1.Secret{}))
				Expect(byObject.Namespaces).To(And(HaveLen(2), HaveKey("kflare"), HaveKey("team-a")))
			}

			scope, err := mustWatchScope("team-a", instanceLabel+"=a", nil).WithSecretNamespaces("kflare")
			Expect(err).NotTo(HaveOccurred())
			opts = scope.CacheOptions(scheme.Scheme)
			Expect(opts.DefaultNamespaces).To(HaveKey("team-a"))
			secrets := 0
			for obj, byObject := range opts.ByObject {
				if _, ok := obj.(*corev1.Secret); ok {
					secrets++
					Expect(byObject.Namespaces).To(And(HaveLen(1), HaveKey("kflare")))
					Expect(byObject.Label).To(BeNil())
				} else {
					Expect(byObject.Label.String()).To(Equal(instanceLabel + "=a"))
				}
			}
			Expect(secrets).To(Equal(1))
		})

		It("says where it may use Secrets, and of many namespaces only the first", func() {
			scoped := WithWatchScope(ctx, mustSecretScope("kflare,team-a"))
			Expect(secretNamespaceNotAllowed(scoped, "Secret", "kflare")).To(BeNil())
			condErr := secretNamespaceNotAllowed(scoped, `Token Secret "tok"`, "team-b")
			Expect(condErr.Reason).To(Equal("SecretNamespaceNotAllowed"))
			Expect(condErr.Message).To(Equal(`Token Secret "tok" is in namespace "team-b", where this kflare ` +
				`installation may not use Secrets (it may in kflare, team-a): add the namespace to the chart's ` +
				`secretNamespaces`))

			many := WithWatchScope(ctx, mustSecretScope("a,b,c,d,e,f,g"))
			Expect(secretNamespaceNotAllowed(many, "Secret", "z").Message).To(ContainSubstring(
				"(it may in a, b, c, d, e and 2 more)"))
		})
	})

	It("refuses a WorkerScript's secret binding elsewhere without reading the Secret", func() {
		binding := cloudflarev1alpha1.WorkerBinding{Name: "API_KEY",
			SecretKeyRef: &cloudflarev1alpha1.WorkerKeyReference{Name: "worker-secret", Key: "api-key"}}
		scoped := WithWatchScope(ctx, mustSecretScope("kflare"))
		// A client that fails every read: the check comes before the lookup.
		_, _, condErr := bindingSecret(scoped, failingReads{k8sClient}, "team-b", binding)
		Expect(condErr.Reason).To(Equal(reasonSecretNamespaceNotAllowed))
		Expect(condErr.Message).To(HavePrefix(`Secret "worker-secret" for binding "API_KEY" is in namespace "team-b"`))

		_, _, condErr = bindingSecret(scoped, k8sClient, "kflare", binding)
		Expect(condErr.Reason).To(Equal("BindingSecretNotFound"))
	})

	Describe("a CloudflareAccount's token Secret", func() {
		const allowed, elsewhere = "secretns-account", "secretns-account-elsewhere"
		var r *CloudflareAccountReconciler

		BeforeEach(func() {
			for _, namespace := range []string{allowed, elsewhere} {
				Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(
					Or(Succeed(), MatchError(ContainSubstring("already exists"))))
			}
			r = &CloudflareAccountReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
				NewCFClient: func(string) (CloudflareAccountAPI, error) {
					return &fakeCFClient{account: cloudflare.Account{Name: "Secret Account"}}, nil
				}}
		})

		It("keeps the finalizer of one it may no longer use, with a Warning Event", func() {
			recorder := events.NewFakeRecorder(10)
			SetEventRecorder(recorder)
			DeferCleanup(func() { SetEventRecorder(nil) })

			const name = "secretns-moved"
			secrets := map[string]types.NamespacedName{}
			for _, namespace := range []string{allowed, elsewhere} {
				key := types.NamespacedName{Name: name + "-token", Namespace: namespace}
				Expect(k8sClient.Create(ctx, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
					Data:       map[string][]byte{"CF_API_TOKEN": []byte("tok")},
				})).To(Succeed())
				DeferCleanup(forceDelete, ctx, &corev1.Secret{}, key)
				secrets[namespace] = key
			}
			account := scopedAccount(name, nil, elsewhere)
			account.Status = cloudflarev1alpha1.CloudflareAccountStatus{}
			Expect(k8sClient.Create(ctx, account)).To(Succeed())
			DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: name})
			reconcileIn := func(scope WatchScope) {
				GinkgoHelper()
				_, err := r.Reconcile(WithWatchScope(ctx, scope), reconcile.Request{
					NamespacedName: types.NamespacedName{Name: name}})
				Expect(err).NotTo(HaveOccurred())
			}
			protected := func(namespace string) bool {
				GinkgoHelper()
				secret := &corev1.Secret{}
				Expect(k8sClient.Get(ctx, secrets[namespace], secret)).To(Succeed())
				return controllerutil.ContainsFinalizer(secret, tokenSecretFinalizer)
			}

			By("protecting the Secret while every namespace's Secrets are in reach")
			reconcileIn(WatchScope{})
			Expect(protected(elsewhere)).To(BeTrue())
			for len(recorder.Events) > 0 {
				<-recorder.Events
			}

			By("reporting it, untouched, once the installation may use Secrets only in another namespace")
			reconcileIn(mustSecretScope(allowed))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, account)).To(Succeed())
			ready := meta.FindStatusCondition(account.Status.Conditions, cloudflarev1alpha1.ConditionReady)
			Expect(ready.Reason).To(Equal(reasonSecretNamespaceNotAllowed))
			Expect(ready.Message).To(HavePrefix(`Token Secret "secretns-moved-token" is in namespace ` +
				`"secretns-account-elsewhere", where this kflare installation may not use Secrets`))
			Expect(protected(elsewhere)).To(BeTrue())

			for len(recorder.Events) > 0 {
				<-recorder.Events
			}

			By("moving the account to a Secret in reach: the old one keeps its finalizer")
			account.Spec.TokenSecretRef.Namespace = allowed
			Expect(k8sClient.Update(ctx, account)).To(Succeed())
			reconcileIn(mustSecretScope(allowed))
			Expect(protected(allowed)).To(BeTrue())
			Expect(protected(elsewhere)).To(BeTrue(), "a Secret out of reach was released")
			Expect(recorder.Events).To(Receive(And(ContainSubstring("Warning "+reasonSecretNamespaceNotAllowed),
				ContainSubstring("Token Secret secretns-account-elsewhere/secretns-moved-token keeps its "+
					"kflare.dev/token-protection finalizer"))))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, account)).To(Succeed())
			Expect(isAccountReady(account)).To(BeTrue())
		})
	})

	It("syncs its cache and reconciles holding the chart's default rules, and reports a Secret elsewhere", func() {
		const release, listed, unlisted = "secretns-release", "secretns-listed", "secretns-unlisted"
		for _, namespace := range []string{release, listed, unlisted} {
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		}
		createAccount := func(name, secretNamespace string) {
			GinkgoHelper()
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-token", Namespace: secretNamespace},
				Data:       map[string][]byte{"CF_API_TOKEN": []byte(name + "-token")},
			}
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			DeferCleanup(forceDelete, ctx, &corev1.Secret{}, client.ObjectKeyFromObject(secret))
			account := scopedAccount(name, nil, secretNamespace)
			account.Status = cloudflarev1alpha1.CloudflareAccountStatus{}
			Expect(k8sClient.Create(ctx, account)).To(Succeed())
			DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: name})
		}
		createTunnel := func(namespace string) types.NamespacedName {
			GinkgoHelper()
			tunnel := &cloudflarev1alpha1.Tunnel{
				ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: namespace},
				Spec: cloudflarev1alpha1.TunnelSpec{
					AccountRef: corev1.LocalObjectReference{Name: "secretns-main"},
					Name:       namespace + "-edge",
					TunnelSpecExtras: cloudflarev1alpha1.TunnelSpecExtras{
						CredentialsSecretRef: cloudflarev1alpha1.TunnelCredentialsSecretReference{Name: "edge-token"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, tunnel)).To(Succeed())
			key := client.ObjectKeyFromObject(tunnel)
			DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.Tunnel{}, key)
			DeferCleanup(forceDelete, ctx, &corev1.Secret{}, types.NamespacedName{Name: "edge-token", Namespace: namespace})
			return key
		}
		expectReady := func(obj conditionedObject, key types.NamespacedName, reason string) metav1.Condition {
			GinkgoHelper()
			var ready metav1.Condition
			Eventually(func(g Gomega) {
				g.Expect(k8sClient.Get(ctx, key, obj)).To(Succeed())
				cond := meta.FindStatusCondition(obj.GetConditions(), cloudflarev1alpha1.ConditionReady)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Reason).To(Equal(reason), cond.Message)
				ready = *cond
			}, timeout, interval).Should(Succeed())
			return ready
		}

		createAccount("secretns-main", release)
		createAccount("secretns-elsewhere", unlisted)
		inListed, inUnlisted := createTunnel(listed), createTunnel(unlisted)

		store := newFakeCRUDStore[shared.CloudflareTunnel](tunnelListPageSize, "")
		hooks := newFakeTunnelHooksAPI(store)
		// startScopedManager waits for the cache to sync, which a cluster-wide
		// Secret informer never would as this user.
		startScopedManager(ctx, secretScopedUser(ctx, "secretns-manager", release, listed),
			mustSecretScope(release+","+listed), func(mgr manager.Manager) error {
				accounts := &CloudflareAccountReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
					NewCFClient: func(string) (CloudflareAccountAPI, error) {
						return &fakeCFClient{account: cloudflare.Account{Name: "Secret Scoped"}}, nil
					}}
				if err := accounts.SetupWithManager(mgr); err != nil {
					return err
				}
				return (&TunnelReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
					NewTunnelAPI: func(string) (TunnelAPI, error) { return fakeTunnelAPI{store, hooks}, nil },
				}).SetupWithManager(mgr)
			})

		By("validating the account whose token Secret is in the release namespace, and protecting the Secret")
		expectReady(&cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: "secretns-main"}, "Validated")
		token := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "secretns-main-token", Namespace: release}, token)).To(
			Succeed())
		Expect(token.Finalizers).To(ContainElement(tokenSecretFinalizer))

		By("reporting the account whose token Secret is elsewhere, its Secret untouched")
		ready := expectReady(&cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: "secretns-elsewhere"},
			reasonSecretNamespaceNotAllowed)
		Expect(ready.Message).To(Equal(`Token Secret "secretns-elsewhere-token" is in namespace "secretns-unlisted", ` +
			`where this kflare installation may not use Secrets (it may in secretns-listed, secretns-release): add ` +
			`the namespace to the chart's secretNamespaces`))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "secretns-elsewhere-token", Namespace: unlisted},
			token)).To(Succeed())
		Expect(token.Finalizers).To(BeEmpty())

		By("writing the token Secret of the Tunnel in a listed namespace")
		expectReady(&cloudflarev1alpha1.Tunnel{}, inListed, "Synced")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "edge-token", Namespace: listed}, token)).To(Succeed())
		Expect(token.Data).To(HaveKeyWithValue(cloudflarev1alpha1.TunnelTokenKey, []byte("token-1")))

		By("reporting the Tunnel in an unlisted namespace, with no tunnel created and nothing written")
		tunnel := &cloudflarev1alpha1.Tunnel{}
		ready = expectReady(tunnel, inUnlisted, reasonSecretNamespaceNotAllowed)
		Expect(ready.Message).To(HavePrefix(`Its credentials Secret "edge-token" is in namespace "secretns-unlisted", ` +
			`where this kflare installation may not use Secrets`))
		Expect(tunnel.Status.CloudflareMetadata.TunnelID).To(BeEmpty())
		Consistently(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "edge-token", Namespace: unlisted}, token)
			return apierrors.IsNotFound(err)
		}, 2*time.Second, interval).Should(BeTrue())
	})
})

// failingReads is a client whose reads fail the spec.
type failingReads struct {
	client.Client
}

func (failingReads) Get(_ context.Context, key client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	Fail("read " + key.String())
	return nil
}
