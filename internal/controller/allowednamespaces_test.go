/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cloudflare/cloudflare-go/v7/kv"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// selectorAllowing returns an AllowedNamespaces of the namespaces matchLabels
// picks.
func selectorAllowing(matchLabels map[string]string) *cloudflarev1alpha1.AllowedNamespaces {
	return &cloudflarev1alpha1.AllowedNamespaces{
		From:     cloudflarev1alpha1.NamespacesFromSelector,
		Selector: &metav1.LabelSelector{MatchLabels: matchLabels},
	}
}

// allowOnlyNamespace makes the CloudflareAccount accountName allow the
// namespace name alone.
func allowOnlyNamespace(ctx context.Context, accountName, name string) {
	GinkgoHelper()
	account := &cloudflarev1alpha1.CloudflareAccount{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: accountName}, account)).To(Succeed())
	account.Spec.AllowedNamespaces = selectorAllowing(map[string]string{corev1.LabelMetadataName: name})
	Expect(k8sClient.Update(ctx, account)).To(Succeed())
}

// createAccountWithAllowedNamespaces creates a CloudflareAccount named name
// from allowed, as JSON: as a manifest would, it can leave out what the Go type
// would fill in.
func createAccountWithAllowedNamespaces(ctx context.Context, name, allowed string) error {
	GinkgoHelper()
	var field map[string]any
	Expect(json.Unmarshal([]byte(allowed), &field)).To(Succeed())
	return k8sClient.Create(ctx, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": cloudflarev1alpha1.GroupVersion.String(),
		"kind":       "CloudflareAccount",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"accountID":         "acct-allowed-crd",
			"tokenSecretRef":    map[string]any{"name": "token", "namespace": "default"},
			"allowedNamespaces": field,
		},
	}})
}

var _ = Describe("spec.allowedNamespaces", func() {
	const (
		accountName = "allowed-account"
		secretName  = "allowed-secret"
		secretNS    = "allowed-secret-ns"
		namespace   = "team-a"
	)
	ctx := context.Background()

	account := func(allowed *cloudflarev1alpha1.AllowedNamespaces) *cloudflarev1alpha1.CloudflareAccount {
		return &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:         "acct-allowed",
				TokenSecretRef:    cloudflarev1alpha1.SecretReference{Name: secretName, Namespace: secretNS, Key: "CF_API_TOKEN"},
				AllowedNamespaces: allowed,
			},
			Status: cloudflarev1alpha1.CloudflareAccountStatus{Conditions: []metav1.Condition{{
				Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "Validated",
			}}},
		}
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: secretNS},
		Data:       map[string][]byte{"CF_API_TOKEN": []byte("tok")},
	}
	teamA := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace, Labels: map[string]string{"team": "a"}}}

	DescribeTable("resolveAccountToken",
		func(allowed *cloudflarev1alpha1.AllowedNamespaces, ns *corev1.Namespace, wantAllowed bool, wantMessage string) {
			objs := []client.Object{account(allowed), secret}
			if ns != nil {
				objs = append(objs, ns)
			}
			c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
			got, token, err := resolveAccountToken(ctx, c, namespace, accountName, true)
			if wantAllowed {
				Expect(err).To(BeNil())
				Expect(got.Name).To(Equal(accountName))
				Expect(token).To(Equal("tok"))
				return
			}
			Expect(err).NotTo(BeNil())
			Expect(err.Reason).To(Equal(reasonAccountNotAllowed))
			Expect(err.Message).To(ContainSubstring(wantMessage))
			Expect(err.Message).To(ContainSubstring(fmt.Sprintf("%q", namespace)))
			Expect(got).To(BeNil())
			Expect(token).To(BeEmpty())
		},
		Entry("allows every namespace when unset", nil, nil, true, ""),
		Entry("allows every namespace with from: All, without looking at it",
			&cloudflarev1alpha1.AllowedNamespaces{From: cloudflarev1alpha1.NamespacesFromAll}, nil, true, ""),
		Entry("allows a namespace the selector matches", selectorAllowing(map[string]string{"team": "a"}), teamA, true, ""),
		Entry("allows every namespace with an empty selector",
			&cloudflarev1alpha1.AllowedNamespaces{
				From: cloudflarev1alpha1.NamespacesFromSelector, Selector: &metav1.LabelSelector{},
			}, teamA, true, ""),
		Entry("refuses a namespace the selector does not match",
			selectorAllowing(map[string]string{"team": "b"}), teamA, false, "does not match"),
		Entry("refuses a namespace it cannot read",
			selectorAllowing(map[string]string{"team": "a"}), nil, false, "could not be read"),
		Entry("refuses with a selector it cannot read",
			&cloudflarev1alpha1.AllowedNamespaces{
				From: cloudflarev1alpha1.NamespacesFromSelector,
				Selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key: "team", Operator: metav1.LabelSelectorOpIn,
				}}},
			}, teamA, false, "invalid"),
		Entry("refuses with from: Selector and no selector, which the CRD does not let through",
			&cloudflarev1alpha1.AllowedNamespaces{From: cloudflarev1alpha1.NamespacesFromSelector}, teamA, false,
			"does not match"),
		Entry("refuses a from it does not know, which the CRD does not let through",
			&cloudflarev1alpha1.AllowedNamespaces{From: "Everyone"}, teamA, false, "does not match"),
	)

	It("is checked before the account's readiness and its Secret, so it tells nothing of either", func() {
		notReady := account(selectorAllowing(map[string]string{"team": "b"}))
		notReady.Status.Conditions = nil
		c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(notReady, teamA).Build()
		for _, requireReady := range []bool{true, false} {
			_, _, err := resolveAccountToken(ctx, c, namespace, accountName, requireReady)
			Expect(err).NotTo(BeNil())
			Expect(err.Reason).To(Equal(reasonAccountNotAllowed))
		}
	})

	Describe("leaveCloudflareObject", func() {
		It("returns any other failure for a retry", func() {
			credErr := &conditionError{Reason: "SecretNotFound", Message: "gone"}
			Expect(leaveCloudflareObject(ctx, account(nil), credErr)).To(MatchError(credErr))
		})
	})

	DescribeTable("the CRD",
		func(allowed string, valid bool) {
			const name = "allowed-crd"
			err := createAccountWithAllowedNamespaces(ctx, name, allowed)
			if !valid {
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "err = %v", err)
				return
			}
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: name})
		},
		Entry("takes All", `{"from":"All"}`, true),
		Entry("defaults to All", `{}`, true),
		Entry("takes a selector with from: Selector", `{"from":"Selector","selector":{"matchLabels":{"team":"a"}}}`, true),
		Entry("takes an empty selector with from: Selector", `{"from":"Selector","selector":{}}`, true),
		Entry("refuses from: Selector without a selector", `{"from":"Selector"}`, false),
		Entry("refuses a selector with from: All, which would leave every namespace allowed unnoticed",
			`{"from":"All","selector":{"matchLabels":{"team":"a"}}}`, false),
		Entry("refuses a selector without from, for the same reason", `{"selector":{"matchLabels":{"team":"a"}}}`, false),
		Entry("refuses another from", `{"from":"Everyone"}`, false),
	)

	Describe("a CloudflareAccount with an invalid selector", func() {
		const name = "allowed-invalid"

		It("reports it, and checks nothing with Cloudflare", func() {
			built := 0
			r := &CloudflareAccountReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				NewCFClient: func(string) (CloudflareAccountAPI, error) {
					built++
					return &fakeCFClient{}, nil
				},
			}
			obj := &cloudflarev1alpha1.CloudflareAccount{
				ObjectMeta: metav1.ObjectMeta{Name: name, Finalizers: []string{reconciler.Finalizer}},
				Spec: cloudflarev1alpha1.CloudflareAccountSpec{
					AccountID:      "acct-invalid",
					TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: name, Namespace: "default", Key: "CF_API_TOKEN"},
					AllowedNamespaces: &cloudflarev1alpha1.AllowedNamespaces{
						From: cloudflarev1alpha1.NamespacesFromSelector,
						Selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
							Key: "team", Operator: metav1.LabelSelectorOpIn,
						}}},
					},
				},
			}
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: name})
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Data:       map[string][]byte{"CF_API_TOKEN": []byte("token")},
			})).To(Succeed())
			DeferCleanup(forceDelete, ctx, &corev1.Secret{}, types.NamespacedName{Name: name, Namespace: "default"})

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
			Expect(err).NotTo(HaveOccurred())
			Expect(built).To(BeZero())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, obj)).To(Succeed())
			cond := meta.FindStatusCondition(obj.Status.Conditions, cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("InvalidAllowedNamespaces"))
		})
	})
})

// This spec runs a controller as the manager does, for what no direct
// reconcile shows: the watches. Its timeouts are well under the minute a
// refused resource waits to retry, so only a watch passes it. Every kind that
// names its account watches the same way, so KVNamespace stands in for them.
var _ = Describe("A manager following spec.allowedNamespaces", func() {
	const (
		accountName = "allowed-manager-account"
		nsName      = "allowed-manager-ns"
		name        = "allowed-manager-kv"
		tokenNS     = "default"
	)
	ctx := context.Background()
	key := types.NamespacedName{Name: name, Namespace: nsName}

	expectReason := func(reason string) {
		GinkgoHelper()
		Eventually(func(g Gomega) {
			obj := &cloudflarev1alpha1.KVNamespace{}
			g.Expect(k8sClient.Get(ctx, key, obj)).To(Succeed())
			cond := meta.FindStatusCondition(obj.Status.Conditions, cloudflarev1alpha1.ConditionReady)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal(reason))
		}).WithTimeout(15 * time.Second).Should(Succeed())
	}
	setTeam := func(team string) {
		GinkgoHelper()
		namespace := &corev1.Namespace{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: nsName}, namespace)).To(Succeed())
		namespace.Labels["team"] = team
		Expect(k8sClient.Update(ctx, namespace)).To(Succeed())
	}
	setAllowedTeam := func(team string) {
		GinkgoHelper()
		account := &cloudflarev1alpha1.CloudflareAccount{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: accountName}, account)).To(Succeed())
		account.Spec.AllowedNamespaces = selectorAllowing(map[string]string{"team": team})
		Expect(k8sClient.Update(ctx, account)).To(Succeed())
	}

	It("follows a Namespace's labels and the account's selector, both ways", func() {
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: nsName, Labels: map[string]string{"team": "a"}},
		})).To(Succeed())
		account := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:         "acct-allowed-manager",
				TokenSecretRef:    cloudflarev1alpha1.SecretReference{Name: accountName, Namespace: tokenNS, Key: "CF_API_TOKEN"},
				AllowedNamespaces: selectorAllowing(map[string]string{"team": "a"}),
			},
		}
		Expect(k8sClient.Create(ctx, account)).To(Succeed())
		DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: accountName})
		account.Status.Conditions = []metav1.Condition{{
			Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
			Reason: "Validated", LastTransitionTime: metav1.Now(),
		}}
		Expect(k8sClient.Status().Update(ctx, account)).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: accountName, Namespace: tokenNS},
			Data:       map[string][]byte{"CF_API_TOKEN": []byte("token")},
		})).To(Succeed())
		DeferCleanup(forceDelete, ctx, &corev1.Secret{}, types.NamespacedName{Name: accountName, Namespace: tokenNS})
		Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.KVNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nsName, Finalizers: []string{reconciler.Finalizer}},
			Spec: cloudflarev1alpha1.KVNamespaceSpec{
				AccountRef: corev1.LocalObjectReference{Name: accountName},
				Title:      name,
			},
		})).To(Succeed())
		DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.KVNamespace{}, key)

		api := fakeKVNamespaceAPI{newFakeCRUDStore[kv.Namespace](kvNamespaceListPageSize, "")}
		mgr, err := manager.New(cfg, manager.Options{
			Controller: config.Controller{SkipNameValidation: new(true)},
			Metrics:    metricsserver.Options{BindAddress: "0"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect((&KVNamespaceReconciler{
			Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
			NewKVNamespaceAPI: func(string) (KVNamespaceAPI, error) { return api, nil },
		}).SetupWithManager(mgr)).To(Succeed())
		mgrCtx, stop := context.WithCancel(ctx)
		stopped := make(chan struct{})
		go func() {
			defer GinkgoRecover()
			defer close(stopped)
			Expect(mgr.Start(mgrCtx)).To(Succeed())
		}()
		// Stop it before the cleanup deletes the resources, which it would
		// otherwise reconcile.
		DeferCleanup(func() {
			stop()
			Eventually(stopped).Should(BeClosed())
		})
		expectReason("Synced")

		By("relabelling the namespace out of the selector, and back")
		setTeam("b")
		expectReason(reasonAccountNotAllowed)
		setTeam("a")
		expectReason("Synced")

		By("changing the account's selector, and back")
		setAllowedTeam("b")
		expectReason(reasonAccountNotAllowed)
		setAllowedTeam("a")
		expectReason("Synced")
	})
})
