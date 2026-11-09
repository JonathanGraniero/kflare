/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/cloudflare/cloudflare-go"
	"github.com/cloudflare/cloudflare-go/v7/kv"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/cloudflare/cloudflare-go/v7/packages/pagination"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/yaml"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// lockedKVNamespaceAPI serializes a running manager's calls to a fake KV
// service, so that a spec can read the fake's store between them.
type lockedKVNamespaceAPI struct {
	mu  *sync.Mutex
	api KVNamespaceAPI
}

func (l lockedKVNamespaceAPI) New(ctx context.Context, params kv.NamespaceNewParams,
	opts ...option.RequestOption) (*kv.Namespace, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.api.New(ctx, params, opts...)
}

func (l lockedKVNamespaceAPI) Get(ctx context.Context, id string, params kv.NamespaceGetParams,
	opts ...option.RequestOption) (*kv.Namespace, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.api.Get(ctx, id, params, opts...)
}

func (l lockedKVNamespaceAPI) List(ctx context.Context, params kv.NamespaceListParams,
	opts ...option.RequestOption) (*pagination.V4PagePaginationArray[kv.Namespace], error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.api.List(ctx, params, opts...)
}

func (l lockedKVNamespaceAPI) Update(ctx context.Context, id string, params kv.NamespaceUpdateParams,
	opts ...option.RequestOption) (*kv.Namespace, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.api.Update(ctx, id, params, opts...)
}

func (l lockedKVNamespaceAPI) Delete(ctx context.Context, id string, params kv.NamespaceDeleteParams,
	opts ...option.RequestOption) (*kv.NamespaceDeleteResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.api.Delete(ctx, id, params, opts...)
}

// scopedKV is the KV namespaces of the Cloudflare accounts one installation's
// manager calls.
type scopedKV struct {
	mu    sync.Mutex
	store *fakeCRUDStore[kv.Namespace]
}

func newScopedKV() *scopedKV {
	return &scopedKV{store: newFakeCRUDStore[kv.Namespace](kvNamespaceListPageSize, "")}
}

func (s *scopedKV) api(string) (KVNamespaceAPI, error) {
	return lockedKVNamespaceAPI{mu: &s.mu, api: fakeKVNamespaceAPI{s.store}}, nil
}

// accountIDs returns the Cloudflare account of each call the manager made.
func (s *scopedKV) accountIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.store.parentIDs)
}

// objects returns how many KV namespaces the accounts hold.
func (s *scopedKV) objects() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.store.objects)
}

// setupInstallation registers the controllers of an installation the specs
// run: CloudflareAccount, whose fake validates every account as name;
// KVNamespace, on kvs; and DNSRecord, which calls no Cloudflare API here.
func setupInstallation(name string, kvs *scopedKV) func(mgr manager.Manager) error {
	return func(mgr manager.Manager) error {
		accounts := &CloudflareAccountReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
			NewCFClient: func(string) (CloudflareAccountAPI, error) {
				return &fakeCFClient{account: cloudflare.Account{Name: name}}, nil
			}}
		if err := accounts.SetupWithManager(mgr); err != nil {
			return err
		}
		kvNamespaces := &KVNamespaceReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
			NewKVNamespaceAPI: kvs.api}
		if err := kvNamespaces.SetupWithManager(mgr); err != nil {
			return err
		}
		return (&DNSRecordReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr)
	}
}

// chartRules reads the rules the chart grants a scoped installation from one
// of the files hack/chartrbac writes.
func chartRules(file string) []rbacv1.PolicyRule {
	GinkgoHelper()
	data, err := os.ReadFile(filepath.Join("..", "..", "helm", "kflare", "files", file))
	Expect(err).NotTo(HaveOccurred())
	var rules []rbacv1.PolicyRule
	Expect(yaml.UnmarshalStrict(data, &rules)).To(Succeed())
	Expect(rules).NotTo(BeEmpty())
	return rules
}

// installationUser returns the config of a new user that holds only what the
// chart grants an installation scoped to namespaces with rbac.namespaced: the
// cluster rules through a ClusterRole, and the namespaced rules through a
// Role in each of namespaces. It returns once the API server applies them.
func installationUser(ctx context.Context, name string, namespaces ...string) *rest.Config {
	GinkgoHelper()
	user, err := testEnv.AddUser(envtest.User{Name: name}, nil)
	Expect(err).NotTo(HaveOccurred())
	subjects := []rbacv1.Subject{{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: name}}
	objects := make([]client.Object, 0, 2+2*len(namespaces))
	objects = append(objects,
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}, Rules: chartRules("manager-cluster-rules.yaml")},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}, Subjects: subjects,
			RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name}})
	for _, namespace := range namespaces {
		objects = append(objects,
			&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Rules: chartRules("manager-namespaced-rules.yaml")},
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
		g.Expect(userClient.List(ctx, &cloudflarev1alpha1.CloudflareAccountList{})).To(Succeed())
	}, 10*time.Second, 100*time.Millisecond).Should(Succeed())
	By(name + " cannot read the Secrets of a namespace it does not watch")
	Expect(userClient.List(ctx, &corev1.SecretList{}, client.InNamespace("default"))).NotTo(Succeed())
	return user.Config()
}

var _ = Describe("Installations scoped to some namespaces or a label selector", func() {
	ctx := context.Background()
	const timeout, interval = 15 * time.Second, 100 * time.Millisecond

	// createNamespace creates name: envtest runs no namespace controller, so a
	// namespace is never deleted again, and each spec uses its own.
	createNamespace := func(name string) {
		GinkgoHelper()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})).To(Succeed())
	}

	// createAccount creates the CloudflareAccount name, with labels, and its
	// token Secret in secretNamespace.
	createAccount := func(name string, labels map[string]string, secretNamespace string) {
		GinkgoHelper()
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-token", Namespace: secretNamespace},
			Data:       map[string][]byte{"CF_API_TOKEN": []byte(name + "-token")},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		DeferCleanup(forceDelete, ctx, &corev1.Secret{}, client.ObjectKeyFromObject(secret))
		account := scopedAccount(name, labels, secretNamespace)
		Expect(k8sClient.Create(ctx, account)).To(Succeed())
		DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: name})
	}

	// createKV creates a KVNamespace at key, with labels, which uses account.
	createKV := func(key types.NamespacedName, labels map[string]string, account string) {
		GinkgoHelper()
		obj := &cloudflarev1alpha1.KVNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: labels},
			Spec: cloudflarev1alpha1.KVNamespaceSpec{AccountRef: corev1.LocalObjectReference{Name: account},
				Title: key.Namespace + "-" + key.Name},
		}
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.KVNamespace{}, key)
	}

	// expectReady waits for obj's Ready condition at key to have reason, and
	// returns it.
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

	// expectUntouched checks, for a while, that no manager reconciled obj at
	// key: it holds no finalizer and no status.
	expectUntouched := func(obj conditionedObject, key types.NamespacedName) {
		GinkgoHelper()
		Consistently(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, key, obj)).To(Succeed())
			g.Expect(obj.GetFinalizers()).To(BeEmpty())
			g.Expect(obj.GetConditions()).To(BeEmpty())
		}, 2*time.Second, interval).Should(Succeed())
	}

	// accountName returns the name the CloudflareAccount name's validation
	// recorded: the name of the installation whose fake validated it.
	accountName := func(name string) string {
		GinkgoHelper()
		account := &cloudflarev1alpha1.CloudflareAccount{}
		expectReady(account, types.NamespacedName{Name: name}, "Validated")
		return account.Status.AccountName
	}

	// protected reports whether the Secret at key holds the token protection
	// finalizer.
	protected := func(key types.NamespacedName) bool {
		GinkgoHelper()
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, key, secret)).To(Succeed())
		return controllerutil.ContainsFinalizer(secret, tokenSecretFinalizer)
	}

	It("manages the resources of the namespaces it watches, and the accounts whose token Secret is in one", func() {
		const watched, other = "scope-ns-watched", "scope-ns-other"
		createNamespace(watched)
		createNamespace(other)
		createAccount("scope-ns-account", nil, watched)
		createAccount("scope-ns-elsewhere", nil, other)
		inWatched := types.NamespacedName{Name: "kv", Namespace: watched}
		inOther := types.NamespacedName{Name: "kv", Namespace: other}
		usingElsewhere := types.NamespacedName{Name: "kv-elsewhere", Namespace: watched}
		createKV(inWatched, nil, "scope-ns-account")
		createKV(inOther, nil, "scope-ns-account")
		createKV(usingElsewhere, nil, "scope-ns-elsewhere")

		kvs := newScopedKV()
		startScopedManager(ctx, cfg, mustWatchScope(watched, "", k8sClient), setupInstallation("Watched", kvs))

		Expect(accountName("scope-ns-account")).To(Equal("Watched"))
		Expect(protected(types.NamespacedName{Name: "scope-ns-account-token", Namespace: watched})).To(BeTrue())
		expectReady(&cloudflarev1alpha1.KVNamespace{}, inWatched, "Synced")
		notWatched := expectReady(&cloudflarev1alpha1.KVNamespace{}, usingElsewhere, reasonNotWatched)
		Expect(notWatched.Message).To(Equal(`CloudflareAccount "scope-ns-elsewhere" is outside what this kflare ` +
			`installation watches: its token Secret is in namespace "scope-ns-other", which this installation does ` +
			`not watch (it watches scope-ns-watched)`))

		By("leaving the namespace it does not watch, and the account whose token is there, alone")
		expectUntouched(&cloudflarev1alpha1.KVNamespace{}, inOther)
		expectUntouched(&cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: "scope-ns-elsewhere"})
		Expect(protected(types.NamespacedName{Name: "scope-ns-elsewhere-token", Namespace: other})).To(BeFalse())
		Expect(kvs.objects()).To(Equal(1))
		Expect(kvs.accountIDs()).To(HaveEach("acct-scope-ns-account"))
	})

	It("manages the resources and accounts its selector matches, and says so of a reference outside it", func() {
		const namespace = "scope-selector"
		createNamespace(namespace)
		selected := map[string]string{instanceLabel: "a"}
		createAccount("scope-sel-account", selected, namespace)
		createAccount("scope-sel-other", map[string]string{instanceLabel: "b"}, namespace)
		labelledKV := types.NamespacedName{Name: "kv-a", Namespace: namespace}
		plainKV := types.NamespacedName{Name: "kv-plain", Namespace: namespace}
		createKV(labelledKV, selected, "scope-sel-account")
		createKV(plainKV, nil, "scope-sel-account")

		zone := &cloudflarev1alpha1.Zone{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: namespace},
			Spec: cloudflarev1alpha1.ZoneSpec{AccountRef: corev1.LocalObjectReference{Name: "scope-sel-account"},
				ZoneSpecExtras: cloudflarev1alpha1.ZoneSpecExtras{Name: "scope-selector.example.com"}},
		}
		Expect(k8sClient.Create(ctx, zone)).To(Succeed())
		DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.Zone{}, client.ObjectKeyFromObject(zone))
		record := &cloudflarev1alpha1.DNSRecord{
			ObjectMeta: metav1.ObjectMeta{Name: "www", Namespace: namespace, Labels: selected},
			Spec: cloudflarev1alpha1.DNSRecordSpec{ZoneRef: corev1.LocalObjectReference{Name: "web"},
				Name: "www.scope-selector.example.com", Type: "A", Content: "192.0.2.1"},
		}
		Expect(k8sClient.Create(ctx, record)).To(Succeed())
		DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.DNSRecord{}, client.ObjectKeyFromObject(record))

		kvs := newScopedKV()
		startScopedManager(ctx, cfg, mustWatchScope("", instanceLabel+"=a", k8sClient), setupInstallation("Selected", kvs))

		Expect(accountName("scope-sel-account")).To(Equal("Selected"))
		expectReady(&cloudflarev1alpha1.KVNamespace{}, labelledKV, "Synced")
		By("telling a Zone the selector leaves out, which the manager's cache does not hold, from a missing one")
		notWatched := expectReady(&cloudflarev1alpha1.DNSRecord{}, client.ObjectKeyFromObject(record), reasonNotWatched)
		Expect(notWatched.Message).To(Equal(`Zone "web" is outside what this kflare installation watches: its labels ` +
			`do not match this installation's selector kflare.dev/instance=a`))
		expectUntouched(&cloudflarev1alpha1.KVNamespace{}, plainKV)
		expectUntouched(&cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: "scope-sel-other"})

		By("labelling the Zone, which the cache then holds, and its record's reconcile then reads")
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(zone), zone)).To(Succeed())
		zone.Labels = selected
		Expect(k8sClient.Update(ctx, zone)).To(Succeed())
		expectReady(&cloudflarev1alpha1.DNSRecord{}, client.ObjectKeyFromObject(record), "ZoneNotReady")

		By("unlabelling a resource it manages, which then drops out of its cache, finalizer and all")
		kvA := &cloudflarev1alpha1.KVNamespace{}
		Expect(k8sClient.Get(ctx, labelledKV, kvA)).To(Succeed())
		Expect(kvA.Finalizers).To(ContainElement(reconciler.Finalizer))
		kvA.Labels = nil
		Expect(k8sClient.Update(ctx, kvA)).To(Succeed())
		Expect(k8sClient.Delete(ctx, kvA)).To(Succeed())
		Consistently(func(g Gomega) {
			g.Expect(k8sClient.Get(ctx, labelledKV, kvA)).To(Succeed(), "an installation it no longer selects deleted it")
			g.Expect(kvA.Finalizers).To(ContainElement(reconciler.Finalizer))
		}, 2*time.Second, interval).Should(Succeed())
		Expect(kvs.objects()).To(Equal(1), "the KV namespace of a resource it no longer selects was deleted")
	})

	It("lets two installations with disjoint selectors share a namespace, with only the chart's namespaced RBAC",
		func() {
			const homeA, homeB, shared = "scope-two-a", "scope-two-b", "scope-two-shared"
			for _, namespace := range []string{homeA, homeB, shared} {
				createNamespace(namespace)
			}
			a, b := map[string]string{instanceLabel: "a"}, map[string]string{instanceLabel: "b"}
			createAccount("scope-two-a", a, homeA)
			createAccount("scope-two-b", b, homeB)
			kvA := types.NamespacedName{Name: "kv-a", Namespace: shared}
			kvB := types.NamespacedName{Name: "kv-b", Namespace: shared}
			kvCross := types.NamespacedName{Name: "kv-cross", Namespace: shared}
			createKV(kvA, a, "scope-two-a")
			createKV(kvB, b, "scope-two-b")
			createKV(kvCross, a, "scope-two-b")

			userA := installationUser(ctx, "kflare-scope-two-a", homeA, shared)
			userB := installationUser(ctx, "kflare-scope-two-b", homeB, shared)
			readerA, err := client.New(userA, client.Options{Scheme: scheme.Scheme})
			Expect(err).NotTo(HaveOccurred())
			readerB, err := client.New(userB, client.Options{Scheme: scheme.Scheme})
			Expect(err).NotTo(HaveOccurred())
			kvsA, kvsB := newScopedKV(), newScopedKV()
			startScopedManager(ctx, userA, mustWatchScope(homeA+","+shared, instanceLabel+"=a", readerA),
				setupInstallation("Installation A", kvsA))
			startScopedManager(ctx, userB, mustWatchScope(homeB+","+shared, instanceLabel+"=b", readerB),
				setupInstallation("Installation B", kvsB))

			Expect(accountName("scope-two-a")).To(Equal("Installation A"))
			Expect(accountName("scope-two-b")).To(Equal("Installation B"))
			Expect(protected(types.NamespacedName{Name: "scope-two-a-token", Namespace: homeA})).To(BeTrue())
			Expect(protected(types.NamespacedName{Name: "scope-two-b-token", Namespace: homeB})).To(BeTrue())
			expectReady(&cloudflarev1alpha1.KVNamespace{}, kvA, "Synced")
			expectReady(&cloudflarev1alpha1.KVNamespace{}, kvB, "Synced")
			cross := expectReady(&cloudflarev1alpha1.KVNamespace{}, kvCross, reasonNotWatched)
			Expect(cross.Message).To(ContainSubstring(`CloudflareAccount "scope-two-b" is outside what this kflare ` +
				`installation watches: its labels do not match this installation's selector kflare.dev/instance=a`))

			By("each calling Cloudflare with its own accounts only, and validating its own accounts only")
			Consistently(func(g Gomega) {
				g.Expect(kvsA.accountIDs()).To(HaveEach("acct-scope-two-a"))
				g.Expect(kvsB.accountIDs()).To(HaveEach("acct-scope-two-b"))
				for name, validatedBy := range map[string]string{"scope-two-a": "Installation A",
					"scope-two-b": "Installation B"} {
					account := &cloudflarev1alpha1.CloudflareAccount{}
					g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, account)).To(Succeed())
					g.Expect(account.Status.AccountName).To(Equal(validatedBy))
				}
			}, 2*time.Second, interval).Should(Succeed())
			Expect(kvsA.objects()).To(Equal(1))
			Expect(kvsB.objects()).To(Equal(1))

			By("each deleting its own resource's KV namespace")
			for _, key := range []types.NamespacedName{kvA, kvB} {
				Expect(k8sClient.Delete(ctx, &cloudflarev1alpha1.KVNamespace{ObjectMeta: metav1.ObjectMeta{
					Name: key.Name, Namespace: key.Namespace}})).To(Succeed())
			}
			Eventually(func(g Gomega) {
				g.Expect(kvsA.objects()).To(BeZero())
				g.Expect(kvsB.objects()).To(BeZero())
			}, timeout, interval).Should(Succeed())
		})

	It("names, in AlreadyExists, the account through which another installation may manage an object of the "+
		"same Cloudflare account, with only the chart's namespaced RBAC", func() {
		const homeA, homeB = "scope-same-a", "scope-same-b"
		createNamespace(homeA)
		createNamespace(homeB)
		createAccount("scope-same-a", nil, homeA)
		createAccount("scope-same-b", nil, homeB)
		// Both accounts are for one Cloudflare account, which both
		// installations call.
		for _, name := range []string{"scope-same-a", "scope-same-b"} {
			account := &cloudflarev1alpha1.CloudflareAccount{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, account)).To(Succeed())
			account.Spec.AccountID = "acct-scope-same"
			Expect(k8sClient.Update(ctx, account)).To(Succeed())
		}
		kvs := newScopedKV()
		// createTitled creates a KVNamespace at key, titled "scope-same", which
		// uses account.
		createTitled := func(key types.NamespacedName, account string) {
			GinkgoHelper()
			Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.KVNamespace{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
				Spec: cloudflarev1alpha1.KVNamespaceSpec{AccountRef: corev1.LocalObjectReference{Name: account},
					Title: "scope-same"},
			})).To(Succeed())
			DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.KVNamespace{}, key)
		}
		start := func(home, user, name string) {
			GinkgoHelper()
			config := installationUser(ctx, user, home)
			reader, err := client.New(config, client.Options{Scheme: scheme.Scheme})
			Expect(err).NotTo(HaveOccurred())
			startScopedManager(ctx, config, mustWatchScope(home, "", reader), setupInstallation(name, kvs))
		}

		kvA := types.NamespacedName{Name: "kv", Namespace: homeA}
		start(homeA, "kflare-scope-same-a", "Installation A")
		createTitled(kvA, "scope-same-a")
		expectReady(&cloudflarev1alpha1.KVNamespace{}, kvA, "Synced")

		By("refusing a KV namespace of the same title in the other installation, naming the account")
		kvB := types.NamespacedName{Name: "kv", Namespace: homeB}
		start(homeB, "kflare-scope-same-b", "Installation B")
		createTitled(kvB, "scope-same-b")
		exists := expectReady(&cloudflarev1alpha1.KVNamespace{}, kvB, reasonAlreadyExists)
		Expect(exists.Message).To(Equal(`KV namespace "scope-same" already exists, and this kflare installation ` +
			`did not create it. Another installation may manage it through CloudflareAccount "scope-same-a", which ` +
			`uses the same Cloudflare account but is outside what this installation watches. Set the annotation ` +
			`kflare.dev/adoption-policy: adopt to manage it here only if none does: two installations would then ` +
			`manage it`))
		Expect(kvs.objects()).To(Equal(1))
	})
})
