/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/cloudflare/cloudflare-go/v7/zero_trust"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// tunnelConfigJSON is a tunnel configuration as Cloudflare's JSON holds it:
// what the fake answers with, and what the specs expect kflare to send. An
// originRequest or warp-routing holds a decoded JSON object.
type tunnelConfigJSON struct {
	Ingress       []ingressRuleJSON `json:"ingress,omitempty"`
	OriginRequest any               `json:"originRequest,omitempty"`
	WarpRouting   any               `json:"warp-routing,omitempty"`
}

type ingressRuleJSON struct {
	Hostname      string `json:"hostname,omitempty"`
	Path          string `json:"path,omitempty"`
	Service       string `json:"service"`
	OriginRequest any    `json:"originRequest,omitempty"`
}

// storedTunnelConfig is what Cloudflare answers for a tunnel's configuration.
type storedTunnelConfig struct {
	TunnelID string           `json:"tunnel_id,omitempty"`
	Version  int64            `json:"version"`
	Config   tunnelConfigJSON `json:"config"`
}

// sentConfig is the configuration an update sent, as JSON.
func sentConfig(params zero_trust.TunnelCloudflaredConfigurationUpdateParams) tunnelConfigJSON {
	GinkgoHelper()
	raw, err := params.Config.Value.MarshalJSON()
	Expect(err).NotTo(HaveOccurred())
	var config tunnelConfigJSON
	Expect(json.Unmarshal(raw, &config)).To(Succeed())
	return config
}

// fakeTunnelConfigurationAPI is a test double for TunnelConfigurationAPI. Get
// answers with getResult; Update answers as Cloudflare does, with the
// configuration it was sent, under updateResult's ID and version. Like
// cloudflare-go, it rejects a call without a tunnel or an account ID.
type fakeTunnelConfigurationAPI struct {
	getResult storedTunnelConfig
	getErr    error
	getIDs    []string

	updateResult storedTunnelConfig
	updateErr    error
	updates      []zero_trust.TunnelCloudflaredConfigurationUpdateParams
	updateIDs    []string

	// accountIDs records the account ID of every call.
	accountIDs []string
	// calls records the method names in call order.
	calls []string
}

func (f *fakeTunnelConfigurationAPI) Get(_ context.Context, tunnelID string,
	query zero_trust.TunnelCloudflaredConfigurationGetParams,
	_ ...option.RequestOption) (*zero_trust.TunnelCloudflaredConfigurationGetResponse, error) {
	f.accountIDs = append(f.accountIDs, query.AccountID.Value)
	f.calls = append(f.calls, "Get")
	f.getIDs = append(f.getIDs, tunnelID)
	if tunnelID == "" || query.AccountID.Value == "" {
		return nil, errors.New("missing required parameter")
	}
	if f.getErr != nil {
		return nil, f.getErr
	}
	return decodeTunnelConfig[zero_trust.TunnelCloudflaredConfigurationGetResponse](f.getResult), nil
}

func (f *fakeTunnelConfigurationAPI) Update(_ context.Context, tunnelID string,
	params zero_trust.TunnelCloudflaredConfigurationUpdateParams,
	_ ...option.RequestOption) (*zero_trust.TunnelCloudflaredConfigurationUpdateResponse, error) {
	f.accountIDs = append(f.accountIDs, params.AccountID.Value)
	f.calls = append(f.calls, "Update")
	f.updates = append(f.updates, params)
	f.updateIDs = append(f.updateIDs, tunnelID)
	if tunnelID == "" || params.AccountID.Value == "" {
		return nil, errors.New("missing required parameter")
	}
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	answer := f.updateResult
	answer.Config = sentConfig(params)
	return decodeTunnelConfig[zero_trust.TunnelCloudflaredConfigurationUpdateResponse](answer), nil
}

// decodeTunnelConfig reads stored into v7's type R, as cloudflare-go reads
// Cloudflare's answer.
func decodeTunnelConfig[R any](stored storedTunnelConfig) *R {
	GinkgoHelper()
	raw, err := json.Marshal(stored)
	Expect(err).NotTo(HaveOccurred())
	out := new(R)
	Expect(json.Unmarshal(raw, out)).To(Succeed())
	return out
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

	configPath := "accounts/" + fakeAcctID + "/cfd_tunnel/" + fakeTunnelID + "/configurations"
	notFoundErr := func() error { return v7APIError("GET", configPath, 404, 1003, "Tunnel not found") }
	terminalErr := func() error { return v7APIError("PUT", configPath, 403, 10000, "Authentication error") }

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
				Name:             "tc-cf-tunnel",
				AccountRef:       corev1.LocalObjectReference{Name: accountName},
				TunnelSpecExtras: cloudflarev1alpha1.TunnelSpecExtras{CredentialsSecretRef: cloudflarev1alpha1.TunnelCredentialsSecretReference{Name: "tc-test-tunnel-token"}},
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
				TunnelConfigurationSpecExtras: cloudflarev1alpha1.TunnelConfigurationSpecExtras{
					TunnelRef: corev1.LocalObjectReference{Name: tunnelCRName},
					Ingress: []cloudflarev1alpha1.TunnelIngressRule{
						{
							Hostname: "app.example.com",
							Service:  "http://app.default:80",
							OriginRequest: &cloudflarev1alpha1.TunnelOriginRequest{
								HTTPHostHeader: new("app.internal"),
								NoTLSVerify:    new(true),
							},
						},
						{Hostname: "api.example.com", Path: "^/v1/", Service: "https://api.default:443"},
					},
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
	expectedRules := func(defaultService string) []ingressRuleJSON {
		return []ingressRuleJSON{
			{
				Hostname: "app.example.com",
				Service:  "http://app.default:80",
				OriginRequest: map[string]any{
					"httpHostHeader": "app.internal",
					"noTLSVerify":    true,
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

		It("sets Ready=False NotWatched, without calling CF, when its Tunnel is outside its installation's selector",
			func() {
				readyEnv(nil)
				fake := &fakeTunnelConfigurationAPI{}
				scope := mustWatchScope("", instanceLabel+"=a", k8sClient)
				_, err := reconcilerWithFakeTunnelConfigurationAPI(fake).Reconcile(WithWatchScope(ctx, scope),
					reconcile.Request{NamespacedName: configKey})
				Expect(err).NotTo(HaveOccurred())
				Expect(fake.calls).To(BeEmpty())
				expectReady(configKey, metav1.ConditionFalse, reasonNotWatched)
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
				getResult:    storedTunnelConfig{TunnelID: fakeTunnelID, Version: 0},
				updateResult: storedTunnelConfig{TunnelID: fakeTunnelID, Version: 1},
			}
			result, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			Expect(fake.calls).To(Equal([]string{"Get", "Update"}))
			Expect(fake.accountIDs).To(HaveEach(fakeAcctID))
			Expect(fake.getIDs).To(Equal([]string{fakeTunnelID}))
			Expect(fake.updates).To(HaveLen(1))
			Expect(fake.updateIDs).To(Equal([]string{fakeTunnelID}))
			Expect(sentConfig(fake.updates[0])).To(Equal(tunnelConfigJSON{Ingress: expectedRules("http_status:404")}))

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
			Expect(sentConfig(fake.updates[0]).Ingress).To(Equal(expectedRules("http://fallback.default:80")))
		})

		It("does not write when Cloudflare already has the desired configuration", func() {
			readyEnv(nil)
			setStatusTunnelID(fakeTunnelID)
			fake := &fakeTunnelConfigurationAPI{getResult: storedTunnelConfig{
				TunnelID: fakeTunnelID,
				Version:  7,
				// What Cloudflare returns: the defaults it fills in do not count as drift.
				Config: tunnelConfigJSON{
					Ingress:       expectedRules("http_status:404"),
					OriginRequest: map[string]any{},
					WarpRouting:   map[string]any{"enabled": false},
				},
			}}
			_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(Equal([]string{"Get"}))
			Expect(getConfig().Status.CloudflareMetadata.Version).To(Equal(7))
			expectReady(configKey, metav1.ConditionTrue, "Synced")
		})

		It("schedules a drift check after a sync, once the manager sets an interval", func() {
			readyEnv(nil)
			setStatusTunnelID(fakeTunnelID)
			fake := &fakeTunnelConfigurationAPI{getResult: storedTunnelConfig{
				TunnelID: fakeTunnelID,
				Version:  7,
				Config: tunnelConfigJSON{
					Ingress:     expectedRules("http_status:404"),
					WarpRouting: map[string]any{"enabled": false},
				},
			}}
			r := reconcilerWithFakeTunnelConfigurationAPI(fake)
			expectDriftCheck(func() (reconcile.Result, error) { return reconcileConfig(r, configKey) })
			Expect(fake.calls).To(Equal([]string{"Get", "Get"}))
		})

		DescribeTable("overwrites drift made outside kflare",
			func(mutate func(*tunnelConfigJSON)) {
				readyEnv(nil)
				setStatusTunnelID(fakeTunnelID)
				actual := tunnelConfigJSON{Ingress: expectedRules("http_status:404")}
				mutate(&actual)
				fake := &fakeTunnelConfigurationAPI{
					getResult:    storedTunnelConfig{TunnelID: fakeTunnelID, Version: 3, Config: actual},
					updateResult: storedTunnelConfig{TunnelID: fakeTunnelID, Version: 4},
				}
				_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
				Expect(err).NotTo(HaveOccurred())
				Expect(fake.calls).To(Equal([]string{"Get", "Update"}))
				Expect(sentConfig(fake.updates[0])).To(Equal(tunnelConfigJSON{Ingress: expectedRules("http_status:404")}))
				Expect(getConfig().Status.CloudflareMetadata.Version).To(Equal(4))
			},
			Entry("changed service", func(c *tunnelConfigJSON) {
				c.Ingress[0].Service = "http://elsewhere:80"
			}),
			Entry("extra rule", func(c *tunnelConfigJSON) {
				c.Ingress = append([]ingressRuleJSON{{Hostname: "x.example.com", Service: "http://x:80"}}, c.Ingress...)
			}),
			Entry("a rule's path", func(c *tunnelConfigJSON) {
				c.Ingress[0].Path = "^/admin/"
			}),
			Entry("a changed origin setting", func(c *tunnelConfigJSON) {
				c.Ingress[0].OriginRequest = map[string]any{"httpHostHeader": "elsewhere", "noTLSVerify": true}
			}),
			Entry("an origin setting the spec does not set", func(c *tunnelConfigJSON) {
				c.Ingress[1].OriginRequest = map[string]any{"connectTimeout": 30}
			}),
			Entry("an origin setting kflare does not know", func(c *tunnelConfigJSON) {
				c.Ingress[1].OriginRequest = map[string]any{"access": map[string]any{"required": true}}
			}),
			Entry("top-level origin settings", func(c *tunnelConfigJSON) {
				c.OriginRequest = map[string]any{"noTLSVerify": true}
			}),
			Entry("warp routing enabled", func(c *tunnelConfigJSON) {
				c.WarpRouting = map[string]any{"enabled": true}
			}),
		)

		It("treats an empty originRequest object, and settings at their zero value, as absent", func() {
			readyEnv(nil)
			setStatusTunnelID(fakeTunnelID)
			actual := expectedRules("http_status:404")
			actual[1].OriginRequest = map[string]any{}
			actual[2].OriginRequest = map[string]any{"connectTimeout": 0, "noTLSVerify": false, "caPool": ""}
			fake := &fakeTunnelConfigurationAPI{getResult: storedTunnelConfig{
				Config: tunnelConfigJSON{Ingress: actual},
			}}
			_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(Equal([]string{"Get"}))
		})

		// configuredOutside is a tunnel's configuration that routes a hostname,
		// as one written outside kflare would.
		configuredOutside := storedTunnelConfig{TunnelID: fakeTunnelID, Version: 5,
			Config: tunnelConfigJSON{Ingress: []ingressRuleJSON{
				{Hostname: "legacy.example.com", Service: "http://legacy:80"}, {Service: "http_status:404"},
			}}}
		adoptPolicy := map[string]string{reconciler.AdoptionPolicyAnnotation: reconciler.AdoptionPolicyAdopt}

		It("reports AlreadyExists for a tunnel configured outside kflare without an adoption policy, and leaves it",
			func() {
				readyEnv(nil)
				fake := &fakeTunnelConfigurationAPI{getResult: configuredOutside}
				result, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
				Expect(fake.calls).To(Equal([]string{"Get"}))
				expectReady(configKey, metav1.ConditionFalse, "AlreadyExists")
				Expect(getConfig().Status.CloudflareMetadata.TunnelID).To(BeEmpty())
			})

		It("replaces a tunnel's configuration with the adopt policy, and records the adoption", func() {
			readyEnv(adoptPolicy)
			fake := &fakeTunnelConfigurationAPI{getResult: configuredOutside,
				updateResult: storedTunnelConfig{TunnelID: fakeTunnelID, Version: 6}}
			_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(Equal([]string{"Get", "Update"}))
			Expect(sentConfig(fake.updates[0]).Ingress).To(Equal(expectedRules("http_status:404")))
			Expect(getConfig().Annotations).To(HaveKeyWithValue(reconciler.AdoptedAnnotation, "true"))
			Expect(getConfig().Status.CloudflareMetadata.TunnelID).To(Equal(fakeTunnelID))
		})

		It("reports NotFound with the adopt policy for a tunnel with nothing to adopt", func() {
			readyEnv(adoptPolicy)
			fake := &fakeTunnelConfigurationAPI{getResult: storedTunnelConfig{TunnelID: fakeTunnelID,
				Config: tunnelConfigJSON{Ingress: []ingressRuleJSON{{Service: "http_status:404"}}}}}
			_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(Equal([]string{"Get"}))
			expectReady(configKey, metav1.ConditionFalse, "NotFound")
		})

		It("marks the configuration it is about to write, and finds it on the next reconcile as its own", func() {
			readyEnv(nil)
			fake := &fakeTunnelConfigurationAPI{updateErr: errors.New("lost response")}
			_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).To(HaveOccurred())
			Expect(getConfig().Annotations).To(HaveKeyWithValue(reconciler.CreatingAnnotation, fakeTunnelID))

			By("finding the rules the lost write stored, without an adoption policy")
			fake = &fakeTunnelConfigurationAPI{getResult: storedTunnelConfig{TunnelID: fakeTunnelID,
				Version: 2, Config: tunnelConfigJSON{Ingress: expectedRules("http_status:404")}}}
			_, err = reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(Equal([]string{"Get"}))
			expectReady(configKey, metav1.ConditionTrue, "Synced")
			Expect(getConfig().Annotations).NotTo(HaveKey(reconciler.AdoptedAnnotation))

			By("dropping the mark once status records the tunnel")
			_, err = reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(getConfig().Annotations).NotTo(HaveKey(reconciler.CreatingAnnotation))
		})

		It("reports AlreadyExists when a recreated Tunnel's new tunnel is configured outside kflare", func() {
			readyEnv(nil)
			setStatusTunnelID("tun-old")
			fake := &fakeTunnelConfigurationAPI{getResult: configuredOutside}
			_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(Equal([]string{"Get"}), "what it wrote to the old tunnel is no claim")
			expectReady(configKey, metav1.ConditionFalse, "AlreadyExists")
		})

		It("writes to the new tunnel when the Tunnel was recreated with another ID", func() {
			readyEnv(nil)
			setStatusTunnelID("tun-old")
			fake := &fakeTunnelConfigurationAPI{}
			_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.getIDs).To(Equal([]string{fakeTunnelID}))
			Expect(fake.updateIDs).To(Equal([]string{fakeTunnelID}))
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
			Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
			Expect(fake.calls).To(BeEmpty())
			expectReady(otherKey, metav1.ConditionFalse, "TunnelAlreadyConfigured")

			_, err = reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(ContainElement("Update"))
			expectReady(configKey, metav1.ConditionTrue, "Synced")
		})
	})

	It("is not blocked by a configuration for a different Tunnel", func() {
		readyEnv(nil)
		Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.TunnelConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: otherConfig, Namespace: ns},
			Spec: cloudflarev1alpha1.TunnelConfigurationSpec{
				TunnelConfigurationSpecExtras: cloudflarev1alpha1.TunnelConfigurationSpecExtras{
					TunnelRef: corev1.LocalObjectReference{Name: "some-other-tunnel"},
					Ingress:   []cloudflarev1alpha1.TunnelIngressRule{{Hostname: "o.example.com", Service: "http://o:80"}},
				},
			},
		})).To(Succeed())

		fake := &fakeTunnelConfigurationAPI{}
		_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(fake.calls).To(ContainElement("Update"))
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
			Expect(fake.calls).To(Equal([]string{"Update"}))
			Expect(fake.accountIDs).To(HaveEach(fakeAcctID))
			Expect(fake.updateIDs).To(Equal([]string{fakeTunnelID}))
			Expect(sentConfig(fake.updates[0])).To(Equal(tunnelConfigJSON{Ingress: []ingressRuleJSON{{Service: "http_status:404"}}}))
			expectConfigGone()
		})

		It("hands a tunnel's rules to the configuration that replaces it, without resetting them", func() {
			readyEnv(nil)
			fake := &fakeTunnelConfigurationAPI{}
			r := reconcilerWithFakeTunnelConfigurationAPI(fake)
			_, err := reconcileConfig(r, configKey)
			Expect(err).NotTo(HaveOccurred())
			expectReady(configKey, metav1.ConditionTrue, "Synced")
			fake.getResult.Config = sentConfig(fake.updates[0])
			createConfig(otherConfig, nil, true)
			otherKey := types.NamespacedName{Name: otherConfig, Namespace: ns}
			_, err = reconcileConfig(r, otherKey)
			Expect(err).NotTo(HaveOccurred())
			expectReady(otherKey, metav1.ConditionFalse, "TunnelAlreadyConfigured")

			startDeletion()
			_, err = reconcileConfig(r, configKey)
			Expect(err).NotTo(HaveOccurred())
			expectConfigGone()
			Expect(fake.updates).To(HaveLen(1), "the rules are not reset")
			renamed := &cloudflarev1alpha1.TunnelConfiguration{}
			Expect(k8sClient.Get(ctx, otherKey, renamed)).To(Succeed())
			Expect(renamed.Labels).To(HaveKeyWithValue(cloudflarev1alpha1.TunnelConfigurationIDLabel, fakeTunnelID))
			Expect(renamed.Annotations).NotTo(HaveKey(reconciler.AdoptedAnnotation))

			By("finding the rules as its own, and writing nothing")
			_, err = reconcileConfig(r, otherKey)
			Expect(err).NotTo(HaveOccurred())
			expectReady(otherKey, metav1.ConditionTrue, "Synced")
			Expect(fake.updates).To(HaveLen(1))
		})

		It("hands a tunnel's rules to a configuration for the Tunnel that replaces its own", func() {
			readyEnv(nil)
			setStatusTunnelID(fakeTunnelID)
			// A renamed Tunnel names the same tunnel, and waits for it.
			renamedTunnel := &cloudflarev1alpha1.Tunnel{
				ObjectMeta: metav1.ObjectMeta{Name: tunnelCRName + "-renamed", Namespace: ns},
				Spec: cloudflarev1alpha1.TunnelSpec{
					Name:       "tc-cf-tunnel",
					AccountRef: corev1.LocalObjectReference{Name: accountName},
					TunnelSpecExtras: cloudflarev1alpha1.TunnelSpecExtras{
						CredentialsSecretRef: cloudflarev1alpha1.TunnelCredentialsSecretReference{Name: "tc-renamed-token"}},
				},
			}
			Expect(k8sClient.Create(ctx, renamedTunnel)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, renamedTunnel)).To(Succeed()) })
			Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.TunnelConfiguration{
				ObjectMeta: metav1.ObjectMeta{Name: otherConfig, Namespace: ns},
				Spec: cloudflarev1alpha1.TunnelConfigurationSpec{
					TunnelConfigurationSpecExtras: cloudflarev1alpha1.TunnelConfigurationSpecExtras{
						TunnelRef: corev1.LocalObjectReference{Name: renamedTunnel.Name},
						Ingress:   []cloudflarev1alpha1.TunnelIngressRule{{Hostname: "o.example.com", Service: "http://o:80"}},
					},
				},
			})).To(Succeed())
			startDeletion()
			fake := &fakeTunnelConfigurationAPI{}
			_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(BeEmpty(), "the rules are not reset")
			expectConfigGone()
			renamed := &cloudflarev1alpha1.TunnelConfiguration{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: otherConfig, Namespace: ns}, renamed)).To(Succeed())
			Expect(renamed.Labels).To(HaveKeyWithValue(cloudflarev1alpha1.TunnelConfigurationIDLabel, fakeTunnelID))
		})

		It("resets the rules when the other configuration is for another tunnel", func() {
			readyEnv(nil)
			setStatusTunnelID(fakeTunnelID)
			Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.TunnelConfiguration{
				ObjectMeta: metav1.ObjectMeta{Name: otherConfig, Namespace: ns},
				Spec: cloudflarev1alpha1.TunnelConfigurationSpec{
					TunnelConfigurationSpecExtras: cloudflarev1alpha1.TunnelConfigurationSpecExtras{
						TunnelRef: corev1.LocalObjectReference{Name: "some-other-tunnel"},
						Ingress:   []cloudflarev1alpha1.TunnelIngressRule{{Hostname: "o.example.com", Service: "http://o:80"}},
					},
				},
			})).To(Succeed())
			startDeletion()
			fake := &fakeTunnelConfigurationAPI{}
			_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(Equal([]string{"Update"}))
			expectConfigGone()
		})

		It("resets an adopted configuration with deletion-policy=delete", func() {
			readyEnv(map[string]string{reconciler.AdoptedAnnotation: "true", deletionPolicy: "delete"})
			setStatusTunnelID(fakeTunnelID)
			startDeletion()

			fake := &fakeTunnelConfigurationAPI{}
			_, err := reconcileConfig(reconcilerWithFakeTunnelConfigurationAPI(fake), configKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(Equal([]string{"Update"}))
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
			Expect(fake.updateIDs).To(Equal([]string{"tun-written"}))
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
			Entry("an adopted configuration", map[string]string{reconciler.AdoptedAnnotation: "true"}, fakeTunnelID,
				false),
			Entry("nothing was ever written", nil, "", false),
			Entry("the Tunnel resource is gone", nil, fakeTunnelID, true),
		)

		It("removes the finalizer of a configuration whose Tunnel left its installation's selector, and leaves the "+
			"tunnel's rules, with a Warning Event", func() {
			recorder := events.NewFakeRecorder(10)
			SetEventRecorder(recorder)
			DeferCleanup(func() { SetEventRecorder(nil) })
			readyEnv(nil)
			setStatusTunnelID(fakeTunnelID)
			startDeletion()

			fake := &fakeTunnelConfigurationAPI{}
			scope := mustWatchScope("", instanceLabel+"=a", k8sClient)
			_, err := reconcilerWithFakeTunnelConfigurationAPI(fake).Reconcile(WithWatchScope(ctx, scope),
				reconcile.Request{NamespacedName: configKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(BeEmpty())
			expectConfigGone()
			Expect(recorder.Events).To(Receive(And(ContainSubstring("Warning "+reasonNotWatched),
				ContainSubstring(`Tunnel "`+tunnelCRName+`" is outside`), ContainSubstring("stays in Cloudflare"))))
		})

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

		DescribeTable("rejects a negative timeout",
			func(field string, set func(*cloudflarev1alpha1.TunnelOriginRequest)) {
				createConfig(configName, nil, false)
				tc := getConfig()
				set(tc.Spec.Ingress[0].OriginRequest)
				Expect(k8sClient.Update(ctx, tc)).To(MatchError(ContainSubstring(
					"spec.ingress[0].originRequest." + field)))
			},
			Entry("connectTimeout", "connectTimeout",
				func(o *cloudflarev1alpha1.TunnelOriginRequest) { o.ConnectTimeout = new(int64(-1)) }),
			Entry("tlsTimeout", "tlsTimeout",
				func(o *cloudflarev1alpha1.TunnelOriginRequest) { o.TLSTimeout = new(int64(-1)) }),
			Entry("tcpKeepAlive", "tcpKeepAlive",
				func(o *cloudflarev1alpha1.TunnelOriginRequest) { o.TCPKeepAlive = new(int64(-1)) }),
			Entry("keepAliveTimeout", "keepAliveTimeout",
				func(o *cloudflarev1alpha1.TunnelOriginRequest) { o.KeepAliveTimeout = new(int64(-1)) }),
		)

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
