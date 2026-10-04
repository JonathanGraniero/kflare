/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
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

// fakeTunnelConfigurationAPI is a test double for TunnelConfigurationAPI.
// Like cloudflare-go, it rejects an update without a tunnel ID.
type fakeTunnelConfigurationAPI struct {
	getResult cf.TunnelConfigurationResult
	getErr    error
	getIDs    []string

	updateResult cf.TunnelConfigurationResult
	updateErr    error
	updates      []cf.TunnelConfigurationParams

	// accountIDs records the resource container identifier of every call.
	accountIDs []string
	// calls records the method names in call order.
	calls []string
}

func (f *fakeTunnelConfigurationAPI) GetTunnelConfiguration(_ context.Context, rc *cf.ResourceContainer, tunnelID string) (cf.TunnelConfigurationResult, error) {
	f.accountIDs = append(f.accountIDs, rc.Identifier)
	f.calls = append(f.calls, "GetTunnelConfiguration")
	f.getIDs = append(f.getIDs, tunnelID)
	return f.getResult, f.getErr
}

func (f *fakeTunnelConfigurationAPI) UpdateTunnelConfiguration(_ context.Context, rc *cf.ResourceContainer, params cf.TunnelConfigurationParams) (cf.TunnelConfigurationResult, error) {
	f.accountIDs = append(f.accountIDs, rc.Identifier)
	f.calls = append(f.calls, "UpdateTunnelConfiguration")
	f.updates = append(f.updates, params)
	if params.TunnelID == "" {
		return cf.TunnelConfigurationResult{}, cf.ErrMissingTunnelID
	}
	return f.updateResult, f.updateErr
}

// reconcilerWithFakeTunnelConfigurationAPI returns a reconciler wired to a pre-built fake.
func reconcilerWithFakeTunnelConfigurationAPI(fake TunnelConfigurationAPI) *TunnelConfigurationReconciler {
	return &TunnelConfigurationReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
		NewTunnelConfigurationAPI: func(_ string) (TunnelConfigurationAPI, error) {
			return fake, nil
		},
	}
}

var _ = Describe("TunnelConfiguration Controller", func() {
	const (
		ns             = "default"
		configName     = "tc-test-config"
		otherConfig    = "tc-test-config-z"
		tunnelCRName   = "tc-test-tunnel"
		accountName    = "tc-test-account"
		apiSecretName  = "tc-test-api-token"
		fakeAcctID     = "acct-tc-123"
		fakeTunnelID   = "tun-tc-456"
		deletionPolicy = "kflare.dev/deletion-policy"
	)

	ctx := context.Background()
	configKey := types.NamespacedName{Name: configName, Namespace: ns}
	tunnelKey := types.NamespacedName{Name: tunnelCRName, Namespace: ns}

	notFoundErr := func() error {
		v := cf.NewNotFoundError(&cf.Error{StatusCode: 404, Type: cf.ErrorTypeNotFound})
		return &v
	}
	terminalErr := func() error {
		v := cf.NewAuthorizationError(&cf.Error{StatusCode: 403, Type: cf.ErrorTypeAuthorization})
		return &v
	}

	createAccount := func(ready bool) {
		acct := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID: fakeAcctID,
				TokenSecretRef: cloudflarev1alpha1.SecretReference{
					Name: apiSecretName, Namespace: ns, Key: "CF_API_TOKEN",
				},
			},
		}
		Expect(k8sClient.Create(ctx, acct)).To(Succeed())
		if ready {
			acct.Status.Conditions = []metav1.Condition{{
				Type:               cloudflarev1alpha1.ConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             "Validated",
				LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(ctx, acct)).To(Succeed())
		}
	}

	createAPISecret := func(data map[string][]byte) {
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: apiSecretName, Namespace: ns},
			Data:       data,
		})).To(Succeed())
	}

	// createTunnel creates the referenced Tunnel. With a tunnel ID it is also
	// marked Ready=True, as the Tunnel controller would after a sync.
	createTunnel := func(tunnelID string) {
		tunnel := &cloudflarev1alpha1.Tunnel{
			ObjectMeta: metav1.ObjectMeta{Name: tunnelCRName, Namespace: ns},
			Spec: cloudflarev1alpha1.TunnelSpec{
				Name:                 "tc-cf-tunnel",
				AccountRef:           corev1.LocalObjectReference{Name: accountName},
				CredentialsSecretRef: cloudflarev1alpha1.TunnelCredentialsSecretReference{Name: "tc-test-tunnel-token"},
			},
		}
		Expect(k8sClient.Create(ctx, tunnel)).To(Succeed())
		if tunnelID != "" {
			tunnel.Status.CloudflareMetadata.TunnelID = tunnelID
			tunnel.Status.Conditions = []metav1.Condition{{
				Type:               cloudflarev1alpha1.ConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             "Synced",
				LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(ctx, tunnel)).To(Succeed())
		}
	}

	setTunnelID := func(tunnelID string) {
		tunnel := &cloudflarev1alpha1.Tunnel{}
		Expect(k8sClient.Get(ctx, tunnelKey, tunnel)).To(Succeed())
		tunnel.Status.CloudflareMetadata.TunnelID = tunnelID
		Expect(k8sClient.Status().Update(ctx, tunnel)).To(Succeed())
	}

	// createConfig creates a TunnelConfiguration with two rules. withFinalizer
	// skips the finalizer-only first reconcile.
	createConfig := func(name string, annotations map[string]string, withFinalizer bool) {
		tc := &cloudflarev1alpha1.TunnelConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Annotations: annotations},
			Spec: cloudflarev1alpha1.TunnelConfigurationSpec{
				TunnelRef: corev1.LocalObjectReference{Name: tunnelCRName},
				Ingress: []cloudflarev1alpha1.TunnelIngressRule{
					{
						Hostname: "app.example.com",
						Service:  "http://app.default:80",
						OriginRequest: &cloudflarev1alpha1.TunnelOriginRequest{
							HTTPHostHeader: cf.StringPtr("app.internal"),
							NoTLSVerify:    cf.BoolPtr(true),
						},
					},
					{Hostname: "api.example.com", Path: "^/v1/", Service: "https://api.default:443"},
				},
			},
		}
		if withFinalizer {
			tc.Finalizers = []string{reconciler.Finalizer}
		}
		Expect(k8sClient.Create(ctx, tc)).To(Succeed())
	}

	// readyEnv creates a ready account, its API token, a ready Tunnel and the configuration.
	readyEnv := func(annotations map[string]string) {
		createAccount(true)
		createAPISecret(map[string][]byte{"CF_API_TOKEN": []byte("api-token")})
		createTunnel(fakeTunnelID)
		createConfig(configName, annotations, true)
	}

	// expectedRules is what createConfig's spec should produce, including the catch-all.
	expectedRules := func(defaultService string) []cf.UnvalidatedIngressRule {
		return []cf.UnvalidatedIngressRule{
			{
				Hostname: "app.example.com",
				Service:  "http://app.default:80",
				OriginRequest: &cf.OriginRequestConfig{
					HTTPHostHeader: cf.StringPtr("app.internal"),
					NoTLSVerify:    cf.BoolPtr(true),
				},
			},
			{Hostname: "api.example.com", Path: "^/v1/", Service: "https://api.default:443"},
			{Service: defaultService},
		}
	}

	getConfig := func() *cloudflarev1alpha1.TunnelConfiguration {
		tc := &cloudflarev1alpha1.TunnelConfiguration{}
		Expect(k8sClient.Get(ctx, configKey, tc)).To(Succeed())
		return tc
	}

	setStatusTunnelID := func(tunnelID string) {
		tc := getConfig()
		tc.Status.CloudflareMetadata.TunnelID = tunnelID
		Expect(k8sClient.Status().Update(ctx, tc)).To(Succeed())
	}

	expectReady := func(key types.NamespacedName, status metav1.ConditionStatus, reason string) {
		tc := &cloudflarev1alpha1.TunnelConfiguration{}
		Expect(k8sClient.Get(ctx, key, tc)).To(Succeed())
		var cond *metav1.Condition
		for i := range tc.Status.Conditions {
			if tc.Status.Conditions[i].Type == cloudflarev1alpha1.ConditionReady {
				cond = &tc.Status.Conditions[i]
			}
		}
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(status))
		Expect(cond.Reason).To(Equal(reason))
	}

	reconcileConfig := func(r *TunnelConfigurationReconciler, key types.NamespacedName) (reconcile.Result, error) {
		return r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	}

	startDeletion := func() {
		Expect(k8sClient.Delete(ctx, getConfig())).To(Succeed())
		Expect(getConfig().DeletionTimestamp).NotTo(BeNil())
	}

	expectConfigGone := func() {
		err := k8sClient.Get(ctx, configKey, &cloudflarev1alpha1.TunnelConfiguration{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected TunnelConfiguration to be deleted, got %v", err)
	}

	AfterEach(func() {
		for _, name := range []string{configName, otherConfig} {
			tc := &cloudflarev1alpha1.TunnelConfiguration{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, tc); err == nil {
				tc.Finalizers = nil
				_ = k8sClient.Update(ctx, tc)
				_ = k8sClient.Delete(ctx, tc)
			}
		}
		tunnel := &cloudflarev1alpha1.Tunnel{}
		if err := k8sClient.Get(ctx, tunnelKey, tunnel); err == nil {
			Expect(k8sClient.Delete(ctx, tunnel)).To(Succeed())
		}
		acct := &cloudflarev1alpha1.CloudflareAccount{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: accountName}, acct); err == nil {
			Expect(k8sClient.Delete(ctx, acct)).To(Succeed())
		}
		secret := &corev1.Secret{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: apiSecretName, Namespace: ns}, secret); err == nil {
			Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
		}
	})

	Describe("Reconcile", func() {
		It("returns no error when the TunnelConfiguration does not exist", func() {
			_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(&fakeTunnelConfigurationAPI{}), configKey)
			Expect(err).NotTo(HaveOccurred())
		})

		It("adds the finalizer on first reconcile and returns without calling CF", func() {
			createConfig(configName, nil, false)
			fake := &fakeTunnelConfigurationAPI{}
			_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(getConfig().Finalizers).To(ContainElement(reconciler.Finalizer))
			Expect(fake.calls).To(BeEmpty())
		})

		It("sets Ready=False TunnelNotFound when the Tunnel does not exist", func() {
			createConfig(configName, nil, true)
			fake := &fakeTunnelConfigurationAPI{}
			result, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))
			Expect(fake.calls).To(BeEmpty())
			expectReady(configKey, metav1.ConditionFalse, "TunnelNotFound")
		})

		It("sets Ready=False TunnelNotReady until the Tunnel has an ID", func() {
			createTunnel("")
			createConfig(configName, nil, true)
			fake := &fakeTunnelConfigurationAPI{}
			_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(BeEmpty())
			expectReady(configKey, metav1.ConditionFalse, "TunnelNotReady")
		})

		DescribeTable("sets Ready=False and retries later when credentials cannot be resolved",
			func(setup func(), reason string) {
				setup()
				createTunnel(fakeTunnelID)
				createConfig(configName, nil, true)

				fake := &fakeTunnelConfigurationAPI{}
				result, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
				Expect(fake.calls).To(BeEmpty())
				expectReady(configKey, metav1.ConditionFalse, reason)
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
			r := &TunnelConfigurationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				NewTunnelConfigurationAPI: func(_ string) (TunnelConfigurationAPI, error) {
					return nil, errors.New("bad token")
				},
			}
			_, err := reconcileConfig(r, configKey)
			Expect(err).NotTo(HaveOccurred())
			expectReady(configKey, metav1.ConditionFalse, "InvalidToken")
		})

		It("writes the rules plus the catch-all to a tunnel with no configuration", func() {
			readyEnv(nil)
			fake := &fakeTunnelConfigurationAPI{
				getResult:    cf.TunnelConfigurationResult{TunnelID: fakeTunnelID, Version: 0},
				updateResult: cf.TunnelConfigurationResult{TunnelID: fakeTunnelID, Version: 1},
			}
			result, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			Expect(fake.calls).To(Equal([]string{"GetTunnelConfiguration", "UpdateTunnelConfiguration"}))
			Expect(fake.accountIDs).To(HaveEach(fakeAcctID))
			Expect(fake.getIDs).To(Equal([]string{fakeTunnelID}))
			Expect(fake.updates).To(HaveLen(1))
			Expect(fake.updates[0].TunnelID).To(Equal(fakeTunnelID))
			Expect(fake.updates[0].Config).To(Equal(cf.TunnelConfiguration{Ingress: expectedRules("http_status:404")}))

			tc := getConfig()
			Expect(tc.Spec.DefaultService).To(Equal("http_status:404"), "API server should default defaultService")
			Expect(tc.Status.CloudflareMetadata.TunnelID).To(Equal(fakeTunnelID))
			Expect(tc.Status.CloudflareMetadata.Version).To(Equal(1))
			expectReady(configKey, metav1.ConditionTrue, "Synced")
		})

		It("uses spec.defaultService as the catch-all rule", func() {
			readyEnv(nil)
			tc := getConfig()
			tc.Spec.DefaultService = "http://fallback.default:80"
			Expect(k8sClient.Update(ctx, tc)).To(Succeed())

			fake := &fakeTunnelConfigurationAPI{}
			_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.updates).To(HaveLen(1))
			Expect(fake.updates[0].Config.Ingress).To(Equal(expectedRules("http://fallback.default:80")))
		})

		It("does not write when Cloudflare already has the desired configuration", func() {
			readyEnv(nil)
			fake := &fakeTunnelConfigurationAPI{getResult: cf.TunnelConfigurationResult{
				TunnelID: fakeTunnelID,
				Version:  7,
				// What Cloudflare returns: the defaults it fills in do not count as drift.
				Config: cf.TunnelConfiguration{
					Ingress:     expectedRules("http_status:404"),
					WarpRouting: &cf.WarpRoutingConfig{Enabled: false},
				},
			}}
			_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(Equal([]string{"GetTunnelConfiguration"}))
			Expect(getConfig().Status.CloudflareMetadata.Version).To(Equal(7))
			expectReady(configKey, metav1.ConditionTrue, "Synced")
		})

		DescribeTable("overwrites drift made outside kflare",
			func(mutate func(*cf.TunnelConfiguration)) {
				readyEnv(nil)
				actual := cf.TunnelConfiguration{Ingress: expectedRules("http_status:404")}
				mutate(&actual)
				fake := &fakeTunnelConfigurationAPI{
					getResult:    cf.TunnelConfigurationResult{TunnelID: fakeTunnelID, Version: 3, Config: actual},
					updateResult: cf.TunnelConfigurationResult{TunnelID: fakeTunnelID, Version: 4},
				}
				_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
				Expect(err).NotTo(HaveOccurred())
				Expect(fake.calls).To(Equal([]string{"GetTunnelConfiguration", "UpdateTunnelConfiguration"}))
				Expect(fake.updates[0].Config).To(Equal(cf.TunnelConfiguration{Ingress: expectedRules("http_status:404")}))
				Expect(getConfig().Status.CloudflareMetadata.Version).To(Equal(4))
			},
			Entry("changed service", func(c *cf.TunnelConfiguration) {
				c.Ingress[0].Service = "http://elsewhere:80"
			}),
			Entry("extra rule", func(c *cf.TunnelConfiguration) {
				c.Ingress = append([]cf.UnvalidatedIngressRule{{Hostname: "x.example.com", Service: "http://x:80"}}, c.Ingress...)
			}),
			Entry("origin setting the spec cannot express", func(c *cf.TunnelConfiguration) {
				c.Ingress[1].OriginRequest = &cf.OriginRequestConfig{ConnectTimeout: &cf.TunnelDuration{Duration: 30 * time.Second}}
			}),
			Entry("top-level origin settings", func(c *cf.TunnelConfiguration) {
				c.OriginRequest = cf.OriginRequestConfig{NoTLSVerify: cf.BoolPtr(true)}
			}),
			Entry("warp routing enabled", func(c *cf.TunnelConfiguration) {
				c.WarpRouting = &cf.WarpRoutingConfig{Enabled: true}
			}),
		)

		It("treats an empty originRequest object as absent", func() {
			readyEnv(nil)
			actual := expectedRules("http_status:404")
			actual[1].OriginRequest = &cf.OriginRequestConfig{}
			fake := &fakeTunnelConfigurationAPI{getResult: cf.TunnelConfigurationResult{
				Config: cf.TunnelConfiguration{Ingress: actual},
			}}
			_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(Equal([]string{"GetTunnelConfiguration"}))
		})

		It("writes to the new tunnel when the Tunnel was recreated with another ID", func() {
			readyEnv(nil)
			setStatusTunnelID("tun-old")
			fake := &fakeTunnelConfigurationAPI{}
			_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.getIDs).To(Equal([]string{fakeTunnelID}))
			Expect(fake.updates[0].TunnelID).To(Equal(fakeTunnelID))
			Expect(getConfig().Status.CloudflareMetadata.TunnelID).To(Equal(fakeTunnelID))
		})

		DescribeTable("classifies Cloudflare API errors",
			func(fake *fakeTunnelConfigurationAPI, reason string, expectErr bool) {
				readyEnv(nil)
				_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
				if expectErr {
					Expect(err).To(HaveOccurred())
				} else {
					Expect(err).NotTo(HaveOccurred())
				}
				expectReady(configKey, metav1.ConditionFalse, reason)
				Expect(getConfig().Status.CloudflareMetadata.TunnelID).To(BeEmpty())
			},
			Entry("terminal Get error", &fakeTunnelConfigurationAPI{getErr: terminalErr()}, "TerminalError", false),
			Entry("retryable Update error", &fakeTunnelConfigurationAPI{updateErr: errors.New("upstream timeout")}, "APIError", true),
		)

		It("lets only the oldest TunnelConfiguration for a Tunnel write its rules", func() {
			readyEnv(nil)
			createConfig(otherConfig, nil, true)
			otherKey := types.NamespacedName{Name: otherConfig, Namespace: ns}

			fake := &fakeTunnelConfigurationAPI{}
			result, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), otherKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(tunnelConflictRetryInterval))
			Expect(fake.calls).To(BeEmpty())
			expectReady(otherKey, metav1.ConditionFalse, "TunnelAlreadyConfigured")

			_, err = reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(ContainElement("UpdateTunnelConfiguration"))
			expectReady(configKey, metav1.ConditionTrue, "Synced")
		})
	})

	It("is not blocked by a configuration for a different Tunnel", func() {
		readyEnv(nil)
		Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.TunnelConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: otherConfig, Namespace: ns},
			Spec: cloudflarev1alpha1.TunnelConfigurationSpec{
				TunnelRef: corev1.LocalObjectReference{Name: "some-other-tunnel"},
				Ingress:   []cloudflarev1alpha1.TunnelIngressRule{{Hostname: "o.example.com", Service: "http://o:80"}},
			},
		})).To(Succeed())

		fake := &fakeTunnelConfigurationAPI{}
		_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.calls).To(ContainElement("UpdateTunnelConfiguration"))
		expectReady(configKey, metav1.ConditionTrue, "Synced")
	})

	Describe("Deletion", func() {
		It("resets the tunnel to the catch-all rule and removes the finalizer", func() {
			readyEnv(nil)
			setStatusTunnelID(fakeTunnelID)
			startDeletion()

			fake := &fakeTunnelConfigurationAPI{}
			_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(Equal([]string{"UpdateTunnelConfiguration"}))
			Expect(fake.accountIDs).To(HaveEach(fakeAcctID))
			Expect(fake.updates[0]).To(Equal(cf.TunnelConfigurationParams{
				TunnelID: fakeTunnelID,
				Config:   cf.TunnelConfiguration{Ingress: []cf.UnvalidatedIngressRule{{Service: "http_status:404"}}},
			}))
			expectConfigGone()
		})

		It("resets the tunnel it last wrote to, not the Tunnel's current ID", func() {
			readyEnv(nil)
			setStatusTunnelID("tun-written")
			setTunnelID("tun-current")
			startDeletion()

			fake := &fakeTunnelConfigurationAPI{}
			_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.updates[0].TunnelID).To(Equal("tun-written"))
		})

		DescribeTable("skips Cloudflare and removes the finalizer",
			func(annotations map[string]string, statusTunnelID string, deleteTunnel bool) {
				readyEnv(annotations)
				if statusTunnelID != "" {
					setStatusTunnelID(statusTunnelID)
				}
				if deleteTunnel {
					Expect(k8sClient.Delete(ctx, &cloudflarev1alpha1.Tunnel{
						ObjectMeta: metav1.ObjectMeta{Name: tunnelCRName, Namespace: ns},
					})).To(Succeed())
				}
				startDeletion()

				fake := &fakeTunnelConfigurationAPI{}
				_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
				Expect(err).NotTo(HaveOccurred())
				Expect(fake.calls).To(BeEmpty())
				expectConfigGone()
			},
			Entry("deletion-policy=retain", map[string]string{deletionPolicy: "retain"}, fakeTunnelID, false),
			Entry("nothing was ever written", nil, "", false),
			Entry("the Tunnel resource is gone", nil, fakeTunnelID, true),
		)

		It("treats a tunnel already gone from Cloudflare as reset", func() {
			readyEnv(nil)
			setStatusTunnelID(fakeTunnelID)
			startDeletion()

			fake := &fakeTunnelConfigurationAPI{updateErr: notFoundErr()}
			_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			expectConfigGone()
		})

		It("keeps the finalizer and returns the error when the reset fails", func() {
			readyEnv(nil)
			setStatusTunnelID(fakeTunnelID)
			startDeletion()

			fake := &fakeTunnelConfigurationAPI{updateErr: errors.New("cloudflare API unavailable")}
			_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).To(MatchError("cloudflare API unavailable"))
			Expect(getConfig().Finalizers).To(ContainElement(reconciler.Finalizer))
		})

		It("returns an error when the account cannot be resolved", func() {
			createTunnel(fakeTunnelID)
			createConfig(configName, nil, true)
			setStatusTunnelID(fakeTunnelID)
			startDeletion()

			_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(&fakeTunnelConfigurationAPI{}), configKey)
			Expect(err).To(MatchError(ContainSubstring("not found")))
			Expect(getConfig().Finalizers).To(ContainElement(reconciler.Finalizer))
		})

		It("returns an error when the CF client cannot be constructed", func() {
			readyEnv(nil)
			setStatusTunnelID(fakeTunnelID)
			startDeletion()

			r := &TunnelConfigurationReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				NewTunnelConfigurationAPI: func(_ string) (TunnelConfigurationAPI, error) {
					return nil, errors.New("bad token")
				},
			}
			_, err := reconcileConfig(r, configKey)
			Expect(err).To(MatchError("bad token"))
			Expect(getConfig().Finalizers).To(ContainElement(reconciler.Finalizer))
		})
	})

	Describe("Validation", func() {
		It("rejects changing spec.tunnelRef", func() {
			createConfig(configName, nil, false)
			tc := getConfig()
			tc.Spec.TunnelRef.Name = "another-tunnel"
			Expect(k8sClient.Update(ctx, tc)).To(MatchError(ContainSubstring("tunnelRef is immutable")))
		})

		It("rejects an empty ingress list", func() {
			createConfig(configName, nil, false)
			tc := getConfig()
			tc.Spec.Ingress = nil
			Expect(k8sClient.Update(ctx, tc)).To(MatchError(ContainSubstring("spec.ingress")))
		})

		It("rejects a rule without a hostname", func() {
			createConfig(configName, nil, false)
			tc := getConfig()
			tc.Spec.Ingress[0].Hostname = ""
			Expect(k8sClient.Update(ctx, tc)).To(MatchError(ContainSubstring("hostname")))
		})
	})
})
