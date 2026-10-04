/*
Copyright 2026 kflare contributors.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cloudflare/cloudflare-go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
)

var _ = Describe("defaultCFClient", func() {
	It("returns an error for an empty token", func() {
		_, err := defaultCFClient("")
		Expect(err).To(HaveOccurred())
	})

	It("returns a client for a non-empty token", func() {
		client, err := defaultCFClient("any-token-value")
		Expect(err).NotTo(HaveOccurred())
		Expect(client).NotTo(BeNil())
	})
})

var _ = Describe("SetupWithManager", func() {
	It("registers without error", func() {
		mgr, err := manager.New(cfg, manager.Options{})
		Expect(err).NotTo(HaveOccurred())

		r := &CloudflareAccountReconciler{
			Client: mgr.GetClient(),
			Scheme: mgr.GetScheme(),
		}
		Expect(r.SetupWithManager(mgr)).To(Succeed())
	})
})

var _ = Describe("CloudflareAccount Controller (nil factory fallback)", func() {
	const (
		accountName = "nil-factory-account"
		secretName  = "nil-factory-secret"
		secretNS    = "default"
		tokenKey    = "CF_API_TOKEN"
	)

	ctx := context.Background()
	accountKey := types.NamespacedName{Name: accountName}
	secretKey := types.NamespacedName{Name: secretName, Namespace: secretNS}

	AfterEach(func() {
		forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, accountKey)
		forceDelete(ctx, &corev1.Secret{}, secretKey)
	})

	It("falls back to defaultCFClient when NewCFClient is nil and sets InvalidToken for empty token", func() {
		// Create account pointing at a secret with an empty token value.
		// defaultCFClient("") returns an error, so we expect InvalidToken.
		acct := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID: "any",
				TokenSecretRef: cloudflarev1alpha1.SecretReference{
					Name: secretName, Namespace: secretNS, Key: tokenKey,
				},
			},
		}
		Expect(k8sClient.Create(ctx, acct)).To(Succeed())

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: secretNS},
			Data:       map[string][]byte{tokenKey: []byte("")}, // empty token
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())

		// Reconciler with no NewCFClient set — should use defaultCFClient.
		r := &CloudflareAccountReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: accountKey})
		Expect(err).NotTo(HaveOccurred())

		updated := &cloudflarev1alpha1.CloudflareAccount{}
		Expect(k8sClient.Get(ctx, accountKey, updated)).To(Succeed())

		var readyCond *metav1.Condition
		for i := range updated.Status.Conditions {
			if updated.Status.Conditions[i].Type == cloudflarev1alpha1.ConditionReady {
				readyCond = &updated.Status.Conditions[i]
			}
		}
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		Expect(readyCond.Reason).To(Equal("InvalidToken"))
	})

	It("uses a non-nil NewCFClient and surfaces APIError when the API fails", func() {
		acct := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID: "any",
				TokenSecretRef: cloudflarev1alpha1.SecretReference{
					Name: secretName, Namespace: secretNS, Key: tokenKey,
				},
			},
		}
		Expect(k8sClient.Create(ctx, acct)).To(Succeed())

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: secretNS},
			Data:       map[string][]byte{tokenKey: []byte("token")},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())

		r := &CloudflareAccountReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			NewCFClient: func(_ string) (CloudflareAccountAPI, error) {
				return &fakeCFClient{
					account: cloudflare.Account{},
					err:     errors.New("rate limited"),
				}, nil
			},
		}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: accountKey})
		// Retryable: the error goes back to controller-runtime for back-off.
		Expect(err).To(HaveOccurred())

		updated := &cloudflarev1alpha1.CloudflareAccount{}
		Expect(k8sClient.Get(ctx, accountKey, updated)).To(Succeed())

		var readyCond *metav1.Condition
		for i := range updated.Status.Conditions {
			if updated.Status.Conditions[i].Type == cloudflarev1alpha1.ConditionReady {
				readyCond = &updated.Status.Conditions[i]
			}
		}
		Expect(readyCond).NotTo(BeNil())
		Expect(readyCond.Reason).To(Equal("APIError"))
	})
})

var _ = Describe("CloudflareAccountReconciler accountsForSecret", func() {
	ctx := context.Background()
	r := &CloudflareAccountReconciler{}

	newAccount := func(name, secretName, secretNS string) *cloudflarev1alpha1.CloudflareAccount {
		return &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      "any",
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: secretName, Namespace: secretNS},
			},
		}
	}

	BeforeEach(func() {
		r.Client = k8sClient
		Expect(k8sClient.Create(ctx, newAccount("afs-match", "afs-token", "default"))).To(Succeed())
		Expect(k8sClient.Create(ctx, newAccount("afs-other-ns", "afs-token", "kube-system"))).To(Succeed())
		Expect(k8sClient.Create(ctx, newAccount("afs-other-name", "afs-other", "default"))).To(Succeed())
	})

	AfterEach(func() {
		for _, name := range []string{"afs-match", "afs-other-ns", "afs-other-name"} {
			Expect(k8sClient.Delete(ctx, newAccount(name, "", ""))).To(Succeed())
		}
	})

	It("returns only the accounts whose token Secret matches by name and namespace", func() {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "afs-token", Namespace: "default"}}
		reqs := r.accountsForSecret(ctx, secret)
		Expect(reqs).To(HaveLen(1))
		Expect(reqs[0].NamespacedName).To(Equal(types.NamespacedName{Name: "afs-match"}))
	})

	It("returns nil when the account list fails", func() {
		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "afs-token", Namespace: "default"}}
		Expect(r.accountsForSecret(cancelCtx, secret)).To(BeNil())
	})
})
