/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"encoding/base64"
	"errors"
	"time"

	cf "github.com/cloudflare/cloudflare-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// fakeTunnelAPI is a test double for TunnelAPI. Like cloudflare-go, it
// rejects a create request without a name or secret before "sending" it.
type fakeTunnelAPI struct {
	getTunnel cf.Tunnel
	getErr    error

	listTunnels []cf.Tunnel
	listErr     error
	listParams  *cf.TunnelListParams

	createTunnel cf.Tunnel
	createErr    error
	createParams *cf.TunnelCreateParams

	token    string
	tokenErr error

	cleanupErr error
	deleteErr  error

	// accountIDs records the resource container identifier of every call.
	accountIDs []string
	// calls records the method names in call order.
	calls []string
}

func (f *fakeTunnelAPI) record(rc *cf.ResourceContainer, method string) {
	f.accountIDs = append(f.accountIDs, rc.Identifier)
	f.calls = append(f.calls, method)
}

func (f *fakeTunnelAPI) CreateTunnel(_ context.Context, rc *cf.ResourceContainer, params cf.TunnelCreateParams) (cf.Tunnel, error) {
	f.record(rc, "CreateTunnel")
	f.createParams = &params
	if params.Name == "" {
		return cf.Tunnel{}, errors.New("missing tunnel name")
	}
	if params.Secret == "" {
		return cf.Tunnel{}, errors.New("missing tunnel secret")
	}
	return f.createTunnel, f.createErr
}

func (f *fakeTunnelAPI) GetTunnel(_ context.Context, rc *cf.ResourceContainer, _ string) (cf.Tunnel, error) {
	f.record(rc, "GetTunnel")
	return f.getTunnel, f.getErr
}

func (f *fakeTunnelAPI) ListTunnels(_ context.Context, rc *cf.ResourceContainer, params cf.TunnelListParams) ([]cf.Tunnel, *cf.ResultInfo, error) {
	f.record(rc, "ListTunnels")
	f.listParams = &params
	return f.listTunnels, &cf.ResultInfo{}, f.listErr
}

func (f *fakeTunnelAPI) GetTunnelToken(_ context.Context, rc *cf.ResourceContainer, _ string) (string, error) {
	f.record(rc, "GetTunnelToken")
	return f.token, f.tokenErr
}

func (f *fakeTunnelAPI) CleanupTunnelConnections(_ context.Context, rc *cf.ResourceContainer, _ string) error {
	f.record(rc, "CleanupTunnelConnections")
	return f.cleanupErr
}

func (f *fakeTunnelAPI) DeleteTunnel(_ context.Context, rc *cf.ResourceContainer, _ string) error {
	f.record(rc, "DeleteTunnel")
	return f.deleteErr
}

// reconcilerWithFakeTunnelAPI returns a TunnelReconciler wired to a pre-built fake.
func reconcilerWithFakeTunnelAPI(fake TunnelAPI) *TunnelReconciler {
	return &TunnelReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
		NewTunnelAPI: func(_ string) (TunnelAPI, error) {
			return fake, nil
		},
	}
}

var _ = Describe("Tunnel Controller", func() {
	const (
		tunnelCRName   = "test-tunnel"
		tunnelNS       = "default"
		cfTunnelName   = "test-tunnel-cf"
		accountName    = "tunnel-test-account"
		apiSecretName  = "tunnel-test-api-token"
		credsName      = "test-tunnel-token"
		altCredsName   = "test-tunnel-token-v2"
		fakeAcctID     = "acct-tun-123"
		fakeTunnelID   = "tun-456"
		fakeTunnelTok  = "tunnel-token-abc"
		deletionPolicy = "kflare.dev/deletion-policy"
	)

	ctx := context.Background()
	tunnelKey := types.NamespacedName{Name: tunnelCRName, Namespace: tunnelNS}
	credsKey := types.NamespacedName{Name: credsName, Namespace: tunnelNS}
	altCredsKey := types.NamespacedName{Name: altCredsName, Namespace: tunnelNS}

	notFoundErr := func() error {
		v := cf.NewNotFoundError(&cf.Error{StatusCode: 404, Type: cf.ErrorTypeNotFound})
		return &v
	}
	terminalErr := func() error {
		v := cf.NewAuthorizationError(&cf.Error{StatusCode: 403, Type: cf.ErrorTypeAuthorization})
		return &v
	}

	// createAccount creates the CloudflareAccount and optionally marks it Ready=True.
	createAccount := func(ready bool) {
		acct := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID: fakeAcctID,
				TokenSecretRef: cloudflarev1alpha1.SecretReference{
					Name: apiSecretName, Namespace: tunnelNS, Key: "CF_API_TOKEN",
				},
			},
		}
		Expect(k8sClient.Create(ctx, acct)).To(Succeed())
		if ready {
			acct.Status.Conditions = []metav1.Condition{{
				Type:               cloudflarev1alpha1.ConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             "Validated",
				Message:            "Credentials valid",
				LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(ctx, acct)).To(Succeed())
		}
	}

	// createAPISecret creates the Secret holding the Cloudflare API token.
	createAPISecret := func(data map[string][]byte) {
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: apiSecretName, Namespace: tunnelNS},
			Data:       data,
		})).To(Succeed())
	}

	// createTunnel creates the Tunnel CR. withFinalizer skips the
	// finalizer-only first reconcile that most specs do not care about.
	createTunnel := func(annotations map[string]string, withFinalizer bool) {
		tunnel := &cloudflarev1alpha1.Tunnel{
			ObjectMeta: metav1.ObjectMeta{
				Name:        tunnelCRName,
				Namespace:   tunnelNS,
				Annotations: annotations,
			},
			Spec: cloudflarev1alpha1.TunnelSpec{
				Name:                 cfTunnelName,
				AccountRef:           corev1.LocalObjectReference{Name: accountName},
				CredentialsSecretRef: cloudflarev1alpha1.TunnelCredentialsSecretReference{Name: credsName},
			},
		}
		if withFinalizer {
			tunnel.Finalizers = []string{reconciler.Finalizer}
		}
		Expect(k8sClient.Create(ctx, tunnel)).To(Succeed())
	}

	// readyEnv creates a ready account, its API token Secret and a Tunnel with the finalizer.
	readyEnv := func(annotations map[string]string) {
		createAccount(true)
		createAPISecret(map[string][]byte{"CF_API_TOKEN": []byte("api-token")})
		createTunnel(annotations, true)
	}

	getTunnel := func() *cloudflarev1alpha1.Tunnel {
		tunnel := &cloudflarev1alpha1.Tunnel{}
		Expect(k8sClient.Get(ctx, tunnelKey, tunnel)).To(Succeed())
		return tunnel
	}

	setTunnelStatus := func(tunnelID, credentialsSecretName string) {
		tunnel := getTunnel()
		tunnel.Status.CloudflareMetadata.TunnelID = tunnelID
		tunnel.Status.CredentialsSecretName = credentialsSecretName
		Expect(k8sClient.Status().Update(ctx, tunnel)).To(Succeed())
	}

	readyCondition := func() *metav1.Condition {
		tunnel := getTunnel()
		for i := range tunnel.Status.Conditions {
			if tunnel.Status.Conditions[i].Type == cloudflarev1alpha1.ConditionReady {
				return &tunnel.Status.Conditions[i]
			}
		}
		return nil
	}

	expectReady := func(status metav1.ConditionStatus, reason string) {
		cond := readyCondition()
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(status))
		Expect(cond.Reason).To(Equal(reason))
	}

	reconcileTunnel := func(r *TunnelReconciler) (reconcile.Result, error) {
		return r.Reconcile(ctx, reconcile.Request{NamespacedName: tunnelKey})
	}

	// startDeletion marks the Tunnel for deletion; its finalizer keeps it visible.
	startDeletion := func() {
		Expect(k8sClient.Delete(ctx, getTunnel())).To(Succeed())
		Expect(getTunnel().DeletionTimestamp).NotTo(BeNil())
	}

	// expectTunnelGone asserts the finalizer was removed and the object deleted.
	expectTunnelGone := func() {
		err := k8sClient.Get(ctx, tunnelKey, &cloudflarev1alpha1.Tunnel{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected Tunnel to be deleted, got %v", err)
	}

	AfterEach(func() {
		tunnel := &cloudflarev1alpha1.Tunnel{}
		if err := k8sClient.Get(ctx, tunnelKey, tunnel); err == nil {
			tunnel.Finalizers = nil
			_ = k8sClient.Update(ctx, tunnel)
			_ = k8sClient.Delete(ctx, tunnel)
		}
		acct := &cloudflarev1alpha1.CloudflareAccount{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: accountName}, acct); err == nil {
			Expect(k8sClient.Delete(ctx, acct)).To(Succeed())
		}
		// envtest runs no garbage collector, so owned Secrets are removed by hand.
		for _, name := range []string{apiSecretName, credsName, altCredsName} {
			secret := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: tunnelNS}, secret); err == nil {
				Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
			}
		}
	})

	Describe("Reconcile", func() {
		It("returns no error when the Tunnel CR does not exist", func() {
			_, err := reconcileTunnel(reconcilerWithFakeTunnelAPI(&fakeTunnelAPI{}))
			Expect(err).NotTo(HaveOccurred())
		})

		It("adds the finalizer on first reconcile and returns without calling CF", func() {
			createAccount(true)
			createAPISecret(map[string][]byte{"CF_API_TOKEN": []byte("api-token")})
			createTunnel(nil, false)

			fake := &fakeTunnelAPI{}
			_, err := reconcileTunnel(reconcilerWithFakeTunnelAPI(fake))
			Expect(err).NotTo(HaveOccurred())
			Expect(getTunnel().Finalizers).To(ContainElement(reconciler.Finalizer))
			Expect(fake.calls).To(BeEmpty())
		})

		DescribeTable("sets Ready=False and retries later when credentials cannot be resolved",
			func(setup func(), reason string) {
				setup()
				createTunnel(nil, true)

				fake := &fakeTunnelAPI{}
				result, err := reconcileTunnel(reconcilerWithFakeTunnelAPI(fake))
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
				Expect(fake.calls).To(BeEmpty())
				expectReady(metav1.ConditionFalse, reason)
			},
			Entry("account missing", func() {}, "AccountNotFound"),
			Entry("account not ready", func() { createAccount(false) }, "AccountNotReady"),
			Entry("API token Secret missing", func() { createAccount(true) }, "SecretNotFound"),
			Entry("token key missing", func() {
				createAccount(true)
				createAPISecret(map[string][]byte{"WRONG_KEY": []byte("x")})
			}, "TokenKeyMissing"),
		)

		It("sets Ready=False InvalidToken when the CF client cannot be constructed", func() {
			readyEnv(nil)
			r := &TunnelReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				NewTunnelAPI: func(_ string) (TunnelAPI, error) {
					return nil, errors.New("bad token")
				},
			}
			_, err := reconcileTunnel(r)
			Expect(err).NotTo(HaveOccurred())
			expectReady(metav1.ConditionFalse, "InvalidToken")
		})

		It("creates a remotely-managed tunnel, writes the token Secret and sets Ready=True", func() {
			readyEnv(nil)
			fake := &fakeTunnelAPI{
				createTunnel: cf.Tunnel{ID: fakeTunnelID, Name: cfTunnelName, Status: "inactive"},
				token:        fakeTunnelTok,
			}
			result, err := reconcileTunnel(reconcilerWithFakeTunnelAPI(fake))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			Expect(fake.calls).To(Equal([]string{"ListTunnels", "CreateTunnel", "GetTunnelToken"}))
			Expect(fake.accountIDs).To(HaveEach(fakeAcctID))
			Expect(fake.listParams.Name).To(Equal(cfTunnelName))
			Expect(fake.listParams.IsDeleted).To(Equal(cf.BoolPtr(false)))
			Expect(fake.createParams.Name).To(Equal(cfTunnelName))
			Expect(fake.createParams.ConfigSrc).To(Equal("cloudflare"))
			secretBytes, decodeErr := base64.StdEncoding.DecodeString(fake.createParams.Secret)
			Expect(decodeErr).NotTo(HaveOccurred())
			Expect(secretBytes).To(HaveLen(32))

			tunnel := getTunnel()
			Expect(tunnel.Status.CloudflareMetadata.TunnelID).To(Equal(fakeTunnelID))
			Expect(tunnel.Status.CloudflareMetadata.Status).To(Equal("inactive"))
			Expect(tunnel.Status.CredentialsSecretName).To(Equal(credsName))
			expectReady(metav1.ConditionTrue, "Synced")

			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, credsKey, secret)).To(Succeed())
			Expect(secret.Type).To(Equal(corev1.SecretTypeOpaque))
			Expect(secret.Data).To(HaveKeyWithValue(cloudflarev1alpha1.TunnelTokenKey, []byte(fakeTunnelTok)))
			Expect(metav1.IsControlledBy(secret, tunnel)).To(BeTrue())
		})

		It("adopts an existing tunnel with the same name instead of creating one", func() {
			readyEnv(nil)
			fake := &fakeTunnelAPI{
				listTunnels: []cf.Tunnel{{ID: "tun-existing", Name: cfTunnelName, Status: "healthy"}},
				token:       fakeTunnelTok,
			}
			_, err := reconcileTunnel(reconcilerWithFakeTunnelAPI(fake))
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(Equal([]string{"ListTunnels", "GetTunnelToken"}))
			Expect(getTunnel().Status.CloudflareMetadata.TunnelID).To(Equal("tun-existing"))
			expectReady(metav1.ConditionTrue, "Synced")
		})

		It("uses the tunnel recorded in status without listing or creating", func() {
			readyEnv(nil)
			setTunnelStatus(fakeTunnelID, credsName)
			fake := &fakeTunnelAPI{
				getTunnel: cf.Tunnel{ID: fakeTunnelID, Name: cfTunnelName, Status: "healthy"},
				token:     fakeTunnelTok,
			}
			_, err := reconcileTunnel(reconcilerWithFakeTunnelAPI(fake))
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(Equal([]string{"GetTunnel", "GetTunnelToken"}))
			Expect(getTunnel().Status.CloudflareMetadata.Status).To(Equal("healthy"))
		})

		DescribeTable("recreates a tunnel that was deleted outside kflare",
			func(fake *fakeTunnelAPI) {
				readyEnv(nil)
				setTunnelStatus(fakeTunnelID, credsName)
				fake.createTunnel = cf.Tunnel{ID: "tun-recreated", Name: cfTunnelName}
				fake.token = fakeTunnelTok

				_, err := reconcileTunnel(reconcilerWithFakeTunnelAPI(fake))
				Expect(err).NotTo(HaveOccurred())
				Expect(fake.calls).To(Equal([]string{"GetTunnel", "ListTunnels", "CreateTunnel", "GetTunnelToken"}))
				Expect(getTunnel().Status.CloudflareMetadata.TunnelID).To(Equal("tun-recreated"))
				expectReady(metav1.ConditionTrue, "Synced")
			},
			Entry("Get returns 404", &fakeTunnelAPI{getErr: notFoundErr()}),
			Entry("Get returns the tunnel with deleted_at set", &fakeTunnelAPI{
				getTunnel: cf.Tunnel{ID: fakeTunnelID, DeletedAt: func() *time.Time { t := time.Now(); return &t }()},
			}),
		)

		DescribeTable("classifies Cloudflare API errors",
			func(fake *fakeTunnelAPI, withStatusID bool, reason string, expectErr bool) {
				readyEnv(nil)
				if withStatusID {
					setTunnelStatus(fakeTunnelID, credsName)
				}
				_, err := reconcileTunnel(reconcilerWithFakeTunnelAPI(fake))
				if expectErr {
					Expect(err).To(HaveOccurred())
				} else {
					Expect(err).NotTo(HaveOccurred())
				}
				expectReady(metav1.ConditionFalse, reason)
			},
			Entry("terminal Get error", &fakeTunnelAPI{getErr: terminalErr()}, true, "TerminalError", false),
			Entry("retryable List error", &fakeTunnelAPI{listErr: errors.New("connection reset")}, false, "APIError", true),
			Entry("terminal Create error", &fakeTunnelAPI{createErr: terminalErr()}, false, "TerminalError", false),
		)

		It("records the new tunnel ID even when fetching its token fails", func() {
			readyEnv(nil)
			fake := &fakeTunnelAPI{
				createTunnel: cf.Tunnel{ID: fakeTunnelID, Name: cfTunnelName},
				tokenErr:     errors.New("upstream timeout"),
			}
			_, err := reconcileTunnel(reconcilerWithFakeTunnelAPI(fake))
			Expect(err).To(HaveOccurred())
			expectReady(metav1.ConditionFalse, "APIError")
			Expect(getTunnel().Status.CloudflareMetadata.TunnelID).To(Equal(fakeTunnelID))

			err = k8sClient.Get(ctx, credsKey, &corev1.Secret{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("updates the owned Secret when the tunnel token changes", func() {
			readyEnv(nil)
			setTunnelStatus(fakeTunnelID, credsName)
			fake := &fakeTunnelAPI{
				getTunnel: cf.Tunnel{ID: fakeTunnelID, Name: cfTunnelName},
				token:     "old-token",
			}
			r := reconcilerWithFakeTunnelAPI(fake)
			_, err := reconcileTunnel(r)
			Expect(err).NotTo(HaveOccurred())

			fake.token = "rotated-token"
			_, err = reconcileTunnel(r)
			Expect(err).NotTo(HaveOccurred())

			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, credsKey, secret)).To(Succeed())
			Expect(secret.Data).To(HaveKeyWithValue(cloudflarev1alpha1.TunnelTokenKey, []byte("rotated-token")))
		})

		It("does not rewrite the Secret when the token is unchanged", func() {
			readyEnv(nil)
			setTunnelStatus(fakeTunnelID, credsName)
			fake := &fakeTunnelAPI{
				getTunnel: cf.Tunnel{ID: fakeTunnelID, Name: cfTunnelName},
				token:     fakeTunnelTok,
			}
			r := reconcilerWithFakeTunnelAPI(fake)
			_, err := reconcileTunnel(r)
			Expect(err).NotTo(HaveOccurred())
			before := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, credsKey, before)).To(Succeed())

			_, err = reconcileTunnel(r)
			Expect(err).NotTo(HaveOccurred())
			after := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, credsKey, after)).To(Succeed())
			Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))
		})

		It("restores the token when the owned Secret's data was removed", func() {
			readyEnv(nil)
			setTunnelStatus(fakeTunnelID, credsName)
			fake := &fakeTunnelAPI{
				getTunnel: cf.Tunnel{ID: fakeTunnelID, Name: cfTunnelName},
				token:     fakeTunnelTok,
			}
			r := reconcilerWithFakeTunnelAPI(fake)
			_, err := reconcileTunnel(r)
			Expect(err).NotTo(HaveOccurred())

			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, credsKey, secret)).To(Succeed())
			secret.Data = nil
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())

			_, err = reconcileTunnel(r)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, credsKey, secret)).To(Succeed())
			Expect(secret.Data).To(HaveKeyWithValue(cloudflarev1alpha1.TunnelTokenKey, []byte(fakeTunnelTok)))
		})

		It("tolerates a previous Secret that no longer exists", func() {
			readyEnv(nil)
			setTunnelStatus(fakeTunnelID, altCredsName)
			fake := &fakeTunnelAPI{
				getTunnel: cf.Tunnel{ID: fakeTunnelID, Name: cfTunnelName},
				token:     fakeTunnelTok,
			}
			_, err := reconcileTunnel(reconcilerWithFakeTunnelAPI(fake))
			Expect(err).NotTo(HaveOccurred())
			Expect(getTunnel().Status.CredentialsSecretName).To(Equal(credsName))
			expectReady(metav1.ConditionTrue, "Synced")
		})

		It("refuses to overwrite a Secret it does not own", func() {
			readyEnv(nil)
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: credsName, Namespace: tunnelNS},
				Data:       map[string][]byte{"unrelated": []byte("keep-me")},
			})).To(Succeed())

			fake := &fakeTunnelAPI{
				createTunnel: cf.Tunnel{ID: fakeTunnelID, Name: cfTunnelName},
				token:        fakeTunnelTok,
			}
			result, err := reconcileTunnel(reconcilerWithFakeTunnelAPI(fake))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))

			cond := readyCondition()
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("CredentialsSecretConflict"))
			Expect(cond.Message).To(ContainSubstring(tunnelNS + "/" + credsName))
			Expect(getTunnel().Status.CloudflareMetadata.TunnelID).To(Equal(fakeTunnelID))

			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, credsKey, secret)).To(Succeed())
			Expect(secret.Data).To(Equal(map[string][]byte{"unrelated": []byte("keep-me")}))
			Expect(secret.OwnerReferences).To(BeEmpty())
		})

		It("moves the token to the new Secret and deletes the old one when credentialsSecretRef changes", func() {
			readyEnv(nil)
			fake := &fakeTunnelAPI{
				createTunnel: cf.Tunnel{ID: fakeTunnelID, Name: cfTunnelName},
				getTunnel:    cf.Tunnel{ID: fakeTunnelID, Name: cfTunnelName},
				token:        fakeTunnelTok,
			}
			r := reconcilerWithFakeTunnelAPI(fake)
			_, err := reconcileTunnel(r)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, credsKey, &corev1.Secret{})).To(Succeed())

			tunnel := getTunnel()
			tunnel.Spec.CredentialsSecretRef.Name = altCredsName
			Expect(k8sClient.Update(ctx, tunnel)).To(Succeed())

			_, err = reconcileTunnel(r)
			Expect(err).NotTo(HaveOccurred())

			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, altCredsKey, secret)).To(Succeed())
			Expect(secret.Data).To(HaveKeyWithValue(cloudflarev1alpha1.TunnelTokenKey, []byte(fakeTunnelTok)))
			err = k8sClient.Get(ctx, credsKey, &corev1.Secret{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "old Secret should be deleted, got %v", err)
			Expect(getTunnel().Status.CredentialsSecretName).To(Equal(altCredsName))
		})

		It("leaves the previous Secret alone when the Tunnel does not own it", func() {
			readyEnv(nil)
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: altCredsName, Namespace: tunnelNS},
				Data:       map[string][]byte{"unrelated": []byte("keep-me")},
			})).To(Succeed())
			setTunnelStatus(fakeTunnelID, altCredsName)

			fake := &fakeTunnelAPI{
				getTunnel: cf.Tunnel{ID: fakeTunnelID, Name: cfTunnelName},
				token:     fakeTunnelTok,
			}
			_, err := reconcileTunnel(reconcilerWithFakeTunnelAPI(fake))
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, altCredsKey, &corev1.Secret{})).To(Succeed())
			Expect(getTunnel().Status.CredentialsSecretName).To(Equal(credsName))
		})
	})

	Describe("Deletion", func() {
		It("cleans up connections, deletes the tunnel and removes the finalizer", func() {
			readyEnv(map[string]string{deletionPolicy: "delete"})
			setTunnelStatus(fakeTunnelID, credsName)
			startDeletion()

			fake := &fakeTunnelAPI{}
			_, err := reconcileTunnel(reconcilerWithFakeTunnelAPI(fake))
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(Equal([]string{"CleanupTunnelConnections", "DeleteTunnel"}))
			Expect(fake.accountIDs).To(HaveEach(fakeAcctID))
			expectTunnelGone()
		})

		It("deletes from Cloudflare by default when no policy annotation is set", func() {
			readyEnv(nil)
			setTunnelStatus(fakeTunnelID, credsName)
			startDeletion()

			fake := &fakeTunnelAPI{}
			_, err := reconcileTunnel(reconcilerWithFakeTunnelAPI(fake))
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(ContainElement("DeleteTunnel"))
			expectTunnelGone()
		})

		It("skips Cloudflare when deletion-policy=retain", func() {
			readyEnv(map[string]string{deletionPolicy: "retain"})
			setTunnelStatus(fakeTunnelID, credsName)
			startDeletion()

			fake := &fakeTunnelAPI{}
			_, err := reconcileTunnel(reconcilerWithFakeTunnelAPI(fake))
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(BeEmpty())
			expectTunnelGone()
		})

		It("skips Cloudflare when no tunnel was ever created", func() {
			readyEnv(nil)
			startDeletion()

			fake := &fakeTunnelAPI{}
			_, err := reconcileTunnel(reconcilerWithFakeTunnelAPI(fake))
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(BeEmpty())
			expectTunnelGone()
		})

		It("treats a tunnel already gone from Cloudflare as deleted", func() {
			readyEnv(nil)
			setTunnelStatus(fakeTunnelID, credsName)
			startDeletion()

			fake := &fakeTunnelAPI{cleanupErr: notFoundErr(), deleteErr: notFoundErr()}
			_, err := reconcileTunnel(reconcilerWithFakeTunnelAPI(fake))
			Expect(err).NotTo(HaveOccurred())
			expectTunnelGone()
		})

		It("keeps the finalizer and returns the error when the Cloudflare delete fails", func() {
			readyEnv(nil)
			setTunnelStatus(fakeTunnelID, credsName)
			startDeletion()

			fake := &fakeTunnelAPI{deleteErr: errors.New("cloudflare API unavailable")}
			_, err := reconcileTunnel(reconcilerWithFakeTunnelAPI(fake))
			Expect(err).To(MatchError("cloudflare API unavailable"))
			Expect(getTunnel().Finalizers).To(ContainElement(reconciler.Finalizer))
		})

		It("keeps the finalizer when cleaning up connections fails", func() {
			readyEnv(nil)
			setTunnelStatus(fakeTunnelID, credsName)
			startDeletion()

			fake := &fakeTunnelAPI{cleanupErr: errors.New("upstream timeout")}
			_, err := reconcileTunnel(reconcilerWithFakeTunnelAPI(fake))
			Expect(err).To(HaveOccurred())
			Expect(fake.calls).To(Equal([]string{"CleanupTunnelConnections"}))
			Expect(getTunnel().Finalizers).To(ContainElement(reconciler.Finalizer))
		})

		It("proceeds when the account is not ready", func() {
			createAccount(false)
			createAPISecret(map[string][]byte{"CF_API_TOKEN": []byte("api-token")})
			createTunnel(nil, true)
			setTunnelStatus(fakeTunnelID, credsName)
			startDeletion()

			fake := &fakeTunnelAPI{}
			_, err := reconcileTunnel(reconcilerWithFakeTunnelAPI(fake))
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(ContainElement("DeleteTunnel"))
			expectTunnelGone()
		})

		It("returns an error when the account cannot be resolved", func() {
			createTunnel(nil, true)
			setTunnelStatus(fakeTunnelID, credsName)
			startDeletion()

			_, err := reconcileTunnel(reconcilerWithFakeTunnelAPI(&fakeTunnelAPI{}))
			Expect(err).To(MatchError(ContainSubstring("not found")))
			Expect(getTunnel().Finalizers).To(ContainElement(reconciler.Finalizer))
		})

		It("returns an error when the CF client cannot be constructed", func() {
			readyEnv(nil)
			setTunnelStatus(fakeTunnelID, credsName)
			startDeletion()

			r := &TunnelReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				NewTunnelAPI: func(_ string) (TunnelAPI, error) {
					return nil, errors.New("bad token")
				},
			}
			_, err := reconcileTunnel(r)
			Expect(err).To(MatchError("bad token"))
			Expect(getTunnel().Finalizers).To(ContainElement(reconciler.Finalizer))
		})
	})

	Describe("Validation", func() {
		BeforeEach(func() { createTunnel(nil, false) })

		It("rejects changing spec.name", func() {
			tunnel := getTunnel()
			tunnel.Spec.Name = "renamed"
			Expect(k8sClient.Update(ctx, tunnel)).To(MatchError(ContainSubstring("name is immutable")))
		})

		It("rejects changing spec.accountRef", func() {
			tunnel := getTunnel()
			tunnel.Spec.AccountRef.Name = "other-account"
			Expect(k8sClient.Update(ctx, tunnel)).To(MatchError(ContainSubstring("accountRef is immutable")))
		})

		It("allows changing spec.credentialsSecretRef", func() {
			tunnel := getTunnel()
			tunnel.Spec.CredentialsSecretRef.Name = altCredsName
			Expect(k8sClient.Update(ctx, tunnel)).To(Succeed())
		})

		It("rejects a credentials Secret name that is not a valid object name", func() {
			tunnel := getTunnel()
			tunnel.Spec.CredentialsSecretRef.Name = "Not_Valid"
			Expect(k8sClient.Update(ctx, tunnel)).To(MatchError(ContainSubstring("credentialsSecretRef.name")))
		})
	})
})
