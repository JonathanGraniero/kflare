/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/cloudflare/cloudflare-go/v7/packages/pagination"
	"github.com/cloudflare/cloudflare-go/v7/shared"
	"github.com/cloudflare/cloudflare-go/v7/zero_trust"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// fakeTunnelTokens is what an account's tunnels hold besides the objects a
// fakeCRUDStore keeps: their token, and calls to disconnect them.
type fakeTunnelTokens struct {
	token                string
	tokenErr, cleanupErr error
	tokenReads           atomic.Int64
	cleanups             []string
}

// fakeTunnelHooksAPI is a Tunnel's hooks' calls, on what its store's fake keeps
// across reconciles.
type fakeTunnelHooksAPI struct {
	*fakeTunnelTokens
}

func newFakeTunnelHooksAPI(store *fakeCRUDStore[shared.CloudflareTunnel]) fakeTunnelHooksAPI {
	if store.extra == nil {
		store.extra = &fakeTunnelTokens{token: "token-1"}
	}
	return fakeTunnelHooksAPI{store.extra.(*fakeTunnelTokens)}
}

func (f fakeTunnelHooksAPI) TunnelToken(context.Context, string, string) (string, error) {
	f.tokenReads.Add(1)
	return f.token, f.tokenErr
}

func (f fakeTunnelHooksAPI) CleanupConnections(_ context.Context, _, tunnelID string) error {
	f.cleanups = append(f.cleanups, tunnelID)
	return f.cleanupErr
}

// secretUpdateCounter counts the updates of Secrets.
type secretUpdateCounter struct {
	client.Client
	updates *int
}

func (c secretUpdateCounter) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if _, ok := obj.(*corev1.Secret); ok {
		*c.updates++
	}
	return c.Client.Update(ctx, obj, opts...)
}

// listRecorder records the params of the tunnel lists it serves.
type listRecorder struct {
	fakeTunnelAPI
	sent *zero_trust.TunnelCloudflaredListParams
}

func (l listRecorder) List(ctx context.Context, params zero_trust.TunnelCloudflaredListParams,
	opts ...option.RequestOption) (*pagination.V4PagePaginationArray[shared.CloudflareTunnel], error) {
	*l.sent = params
	return l.fakeTunnelAPI.List(ctx, params, opts...)
}

var _ = Describe("Tunnel hooks", func() {
	const (
		ns          = "default"
		accountName = "tunnel-hooks-account"
		secretName  = "tunnel-hooks-token"
		accountID   = "tunnel-hooks-account-id"
		tokenSecret = "tunnel-hooks-credentials"
	)
	ctx := context.Background()
	key := types.NamespacedName{Name: "edge", Namespace: ns}
	var store *fakeCRUDStore[shared.CloudflareTunnel]
	var tokens *fakeTunnelTokens
	var r *TunnelReconciler

	create := func(obj client.Object) {
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(forceDelete, ctx, obj.DeepCopyObject().(client.Object), client.ObjectKeyFromObject(obj))
	}
	createTunnel := func(annotations map[string]string) {
		create(&cloudflarev1alpha1.Tunnel{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: ns, Annotations: annotations,
				Finalizers: []string{reconciler.Finalizer}},
			Spec: cloudflarev1alpha1.TunnelSpec{
				AccountRef: corev1.LocalObjectReference{Name: accountName},
				Name:       "edge-tunnel",
				TunnelSpecExtras: cloudflarev1alpha1.TunnelSpecExtras{
					CredentialsSecretRef: cloudflarev1alpha1.TunnelCredentialsSecretReference{Name: tokenSecret},
				},
			},
		})
	}
	getTunnel := func() *cloudflarev1alpha1.Tunnel {
		tunnel := &cloudflarev1alpha1.Tunnel{}
		Expect(k8sClient.Get(ctx, key, tunnel)).To(Succeed())
		return tunnel
	}
	condition := func() metav1.Condition {
		cond := meta.FindStatusCondition(getTunnel().Status.Conditions, cloudflarev1alpha1.ConditionReady)
		Expect(cond).NotTo(BeNil(), "no Ready condition")
		return *cond
	}
	reconcileTunnel := func() (ctrl.Result, error) {
		return r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	}
	sync := func() {
		_, err := reconcileTunnel()
		Expect(err).NotTo(HaveOccurred())
		Expect(condition().Reason).To(Equal("Synced"))
	}
	getSecret := func(name string) *corev1.Secret {
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, secret)).To(Succeed())
		return secret
	}
	removeSecret := func(name string) {
		forceDelete(ctx, &corev1.Secret{}, types.NamespacedName{Name: name, Namespace: ns})
	}

	BeforeEach(func() {
		store = newFakeCRUDStore[shared.CloudflareTunnel](tunnelListPageSize, "")
		hooks := newFakeTunnelHooksAPI(store)
		tokens = hooks.fakeTunnelTokens
		r = &TunnelReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
			NewTunnelAPI: func(string) (TunnelAPI, error) { return fakeTunnelAPI{store, hooks}, nil }}
		create(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
			Data:       map[string][]byte{"CF_API_TOKEN": []byte("token")},
		})
		account := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      accountID,
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: secretName, Namespace: ns, Key: "CF_API_TOKEN"},
			},
		}
		create(account)
		account.Status.Conditions = []metav1.Condition{{
			Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
			Reason: "Validated", LastTransitionTime: metav1.Now(),
		}}
		Expect(k8sClient.Status().Update(ctx, account)).To(Succeed())
		DeferCleanup(removeSecret, tokenSecret)
	})

	It("creates a remotely-managed tunnel and writes its token to a Secret it owns", func() {
		createTunnel(nil)
		sync()
		sent := store.sent[0].(zero_trust.TunnelCloudflaredNewParams)
		Expect(string(sent.ConfigSrc.Value)).To(Equal("cloudflare"))
		Expect(sent.TunnelSecret.Present).To(BeFalse(), "cloudflared authenticates with the token")
		secret := getSecret(tokenSecret)
		Expect(secret.Data).To(HaveKeyWithValue(cloudflarev1alpha1.TunnelTokenKey, []byte("token-1")))
		Expect(metav1.IsControlledBy(secret, getTunnel())).To(BeTrue())
		Expect(getTunnel().Status.CredentialsSecretName).To(Equal(tokenSecret))
	})

	It("records the tunnel even when fetching its token fails", func() {
		createTunnel(nil)
		tokens.tokenErr = v7APIError(http.MethodGet, "token", http.StatusForbidden, 10000, "forbidden")
		_, err := reconcileTunnel()
		Expect(err).NotTo(HaveOccurred())
		Expect(condition().Reason).To(Equal("TerminalError"))
		Expect(getTunnel().Status.CloudflareMetadata.TunnelID).NotTo(BeEmpty())
	})

	It("follows a token rotated outside kflare, and leaves an unchanged Secret alone", func() {
		createTunnel(nil)
		sync()
		// The API server keeps a resourceVersion through an update that changes
		// nothing, so only a count of the updates tells.
		writes := 0
		r.Client = secretUpdateCounter{r.Client, &writes}
		sync()
		Expect(writes).To(BeZero())
		tokens.token = "token-2"
		sync()
		Expect(writes).To(Equal(1))
		Expect(getSecret(tokenSecret).Data).To(HaveKeyWithValue(cloudflarev1alpha1.TunnelTokenKey, []byte("token-2")))
	})

	It("restores the token when the owned Secret's data was removed", func() {
		createTunnel(nil)
		sync()
		secret := getSecret(tokenSecret)
		secret.Data = nil
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
		sync()
		Expect(getSecret(tokenSecret).Data).To(HaveKeyWithValue(cloudflarev1alpha1.TunnelTokenKey, []byte("token-1")))
	})

	It("refuses to overwrite a Secret it does not own", func() {
		create(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: tokenSecret, Namespace: ns},
			Data:       map[string][]byte{"mine": []byte("value")},
		})
		createTunnel(nil)
		result, err := reconcileTunnel()
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
		cond := condition()
		Expect(cond.Reason).To(Equal("CredentialsSecretConflict"))
		Expect(cond.Message).To(ContainSubstring(tokenSecret))
		Expect(getSecret(tokenSecret).Data).To(HaveKey("mine"))
		Expect(getTunnel().Status.CloudflareMetadata.TunnelID).NotTo(BeEmpty(), "status keeps the tunnel")
	})

	It("moves the token to a new Secret and deletes the old one", func() {
		createTunnel(nil)
		sync()
		tunnel := getTunnel()
		tunnel.Spec.CredentialsSecretRef.Name = tokenSecret + "-new"
		Expect(k8sClient.Update(ctx, tunnel)).To(Succeed())
		DeferCleanup(removeSecret, tokenSecret+"-new")
		sync()
		Expect(getSecret(tokenSecret + "-new").Data).To(HaveKey(cloudflarev1alpha1.TunnelTokenKey))
		old := &corev1.Secret{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: tokenSecret, Namespace: ns}, old)
		Expect(client.IgnoreNotFound(err)).To(Succeed())
		Expect(err == nil && old.DeletionTimestamp.IsZero()).To(BeFalse(), "the old copy of the token is gone")
	})

	It("leaves the previous Secret alone when the Tunnel does not own it", func() {
		create(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tokenSecret, Namespace: ns}})
		other := tokenSecret + "-other"
		createTunnel(nil)
		tunnel := getTunnel()
		tunnel.Spec.CredentialsSecretRef.Name = other
		Expect(k8sClient.Update(ctx, tunnel)).To(Succeed())
		DeferCleanup(removeSecret, other)
		tunnel = getTunnel()
		tunnel.Status.CredentialsSecretName = tokenSecret
		Expect(k8sClient.Status().Update(ctx, tunnel)).To(Succeed())
		sync()
		getSecret(tokenSecret)
		Expect(getTunnel().Status.CredentialsSecretName).To(Equal(other))
	})

	It("tolerates a previous Secret that no longer exists", func() {
		createTunnel(nil)
		tunnel := getTunnel()
		tunnel.Status.CredentialsSecretName = "long-gone"
		Expect(k8sClient.Status().Update(ctx, tunnel)).To(Succeed())
		sync()
		Expect(getTunnel().Status.CredentialsSecretName).To(Equal(tokenSecret))
	})

	It("takes a tunnel Cloudflare reports as deleted for a missing one, and creates a new one", func() {
		createTunnel(nil)
		sync()
		id := getTunnel().Status.CloudflareMetadata.TunnelID
		deleted := store.objects[id]
		deleted.DeletedAt = time.Now()
		store.objects[id] = deleted
		sync()
		Expect(getTunnel().Status.CloudflareMetadata.TunnelID).NotTo(Equal(id))
	})

	It("does not adopt a deleted tunnel with the same name", func() {
		store.objects["deleted"] = shared.CloudflareTunnel{ID: "deleted", Name: "edge-tunnel", DeletedAt: time.Now()}
		createTunnel(map[string]string{reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdoptOrCreate})
		sync()
		Expect(getTunnel().Status.CloudflareMetadata.TunnelID).NotTo(Equal("deleted"))
	})

	It("asks Cloudflare for the live tunnels of its name", func() {
		createTunnel(map[string]string{reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdoptOrCreate})
		var sent zero_trust.TunnelCloudflaredListParams
		r.NewTunnelAPI = func(string) (TunnelAPI, error) {
			return listRecorder{fakeTunnelAPI{store, fakeTunnelHooksAPI{tokens}}, &sent}, nil
		}
		sync()
		Expect(sent.Name.Value).To(Equal("edge-tunnel"))
		Expect(sent.IsDeleted.Present).To(BeTrue())
		Expect(sent.IsDeleted.Value).To(BeFalse())
	})

	It("restores a deleted token Secret, in a running manager", func() {
		createTunnel(nil)
		sync()
		reads := tokens.tokenReads.Load()
		api := r.NewTunnelAPI
		startManager(ctx, func(mgr manager.Manager) error {
			return (&TunnelReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), NewTunnelAPI: api}).SetupWithManager(mgr)
		})
		// The manager's first reconcile would restore a Secret deleted before it.
		Eventually(tokens.tokenReads.Load, 10*time.Second).Should(BeNumerically(">", reads))
		Consistently(tokens.tokenReads.Load, 500*time.Millisecond).Should(Equal(tokens.tokenReads.Load()))

		secret := getSecret(tokenSecret)
		Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
		Eventually(func(g Gomega) {
			restored := &corev1.Secret{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: tokenSecret, Namespace: ns}, restored)).To(Succeed())
			g.Expect(restored.UID).NotTo(Equal(secret.UID))
		}, 10*time.Second, 50*time.Millisecond).Should(Succeed())
	})

	It("disconnects the tunnel's connectors before deleting it", func() {
		createTunnel(nil)
		sync()
		id := getTunnel().Status.CloudflareMetadata.TunnelID
		Expect(k8sClient.Delete(ctx, getTunnel())).To(Succeed())
		_, err := reconcileTunnel()
		Expect(err).NotTo(HaveOccurred())
		Expect(tokens.cleanups).To(Equal([]string{id}))
		Expect(store.objects).To(BeEmpty())
	})

	It("keeps the finalizer when disconnecting the connectors fails", func() {
		createTunnel(nil)
		sync()
		Expect(k8sClient.Delete(ctx, getTunnel())).To(Succeed())
		tokens.cleanupErr = errors.New("unavailable")
		_, err := reconcileTunnel()
		Expect(err).To(MatchError("unavailable"))
		Expect(store.objects).To(HaveLen(1), "the tunnel is not deleted")
		Expect(getTunnel().Finalizers).To(ContainElement(reconciler.Finalizer))
	})

	It("deletes a tunnel whose connections are already gone", func() {
		createTunnel(nil)
		sync()
		Expect(k8sClient.Delete(ctx, getTunnel())).To(Succeed())
		tokens.cleanupErr = fakeCRUDNotFound(http.MethodDelete, accountID, "tunnel")
		_, err := reconcileTunnel()
		Expect(err).NotTo(HaveOccurred())
		Expect(store.objects).To(BeEmpty())
	})

	It("keeps the tunnel's connections with the retain policy", func() {
		createTunnel(map[string]string{reconciler.DeletionPolicyAnnotation: reconciler.DeletionPolicyRetain})
		sync()
		Expect(k8sClient.Delete(ctx, getTunnel())).To(Succeed())
		_, err := reconcileTunnel()
		Expect(err).NotTo(HaveOccurred())
		Expect(tokens.cleanups).To(BeEmpty())
		Expect(store.objects).To(HaveLen(1))
	})

	It("refuses a credentials Secret name that is not an object name", func() {
		tunnel := &cloudflarev1alpha1.Tunnel{
			ObjectMeta: metav1.ObjectMeta{Name: "invalid", Namespace: ns},
			Spec: cloudflarev1alpha1.TunnelSpec{
				AccountRef: corev1.LocalObjectReference{Name: accountName}, Name: "edge",
				TunnelSpecExtras: cloudflarev1alpha1.TunnelSpecExtras{
					CredentialsSecretRef: cloudflarev1alpha1.TunnelCredentialsSecretReference{Name: "Not_Valid"},
				},
			},
		}
		Expect(k8sClient.Create(ctx, tunnel)).To(MatchError(ContainSubstring("credentialsSecretRef.name")))
	})

	It("reads tokens and removes connections through the production client's services", func() {
		service := &tunnelService{TunnelCloudflaredService: zero_trust.NewTunnelCloudflaredService()}
		_, err := service.TunnelToken(ctx, "", "tunnel")
		Expect(err).To(HaveOccurred(), "a call without the account's ID is refused")
		Expect(service.CleanupConnections(ctx, "", "tunnel")).NotTo(Succeed())
	})
})
