/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
)

// resolveAccountToken is exercised against an in-memory client rather than
// envtest: the API server defaults an empty TokenSecretRef.Key, so the
// empty-key fallback is only reachable this way.
var _ = Describe("resolveAccountToken", func() {
	const (
		accountName = "creds-account"
		secretName  = "creds-secret"
		secretNS    = "creds-ns"
	)
	ctx := context.Background()

	account := func(key string, ready bool) *cloudflarev1alpha1.CloudflareAccount {
		acct := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID: "acct-creds",
				TokenSecretRef: cloudflarev1alpha1.SecretReference{
					Name: secretName, Namespace: secretNS, Key: key,
				},
			},
		}
		if ready {
			acct.Status.Conditions = []metav1.Condition{{
				Type:   cloudflarev1alpha1.ConditionReady,
				Status: metav1.ConditionTrue,
				Reason: "Validated",
			}}
		}
		return acct
	}
	secret := func(data map[string][]byte) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: secretNS},
			Data:       data,
		}
	}
	newReader := func(objs ...client.Object) client.Reader {
		return fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(objs...).Build()
	}

	It("returns the account and the token stored under the configured key", func() {
		c := newReader(account("MY_KEY", true), secret(map[string][]byte{"MY_KEY": []byte("tok")}))
		acct, token, err := resolveAccountToken(ctx, c, accountName, true)
		Expect(err).To(BeNil())
		Expect(acct.Spec.AccountID).To(Equal("acct-creds"))
		Expect(token).To(Equal("tok"))
	})

	It("reads CF_API_TOKEN when the key is empty", func() {
		c := newReader(account("", true), secret(map[string][]byte{"CF_API_TOKEN": []byte("default-tok")}))
		_, token, err := resolveAccountToken(ctx, c, accountName, true)
		Expect(err).To(BeNil())
		Expect(token).To(Equal("default-tok"))
	})

	It("reports AccountNotFound when the account does not exist", func() {
		_, _, err := resolveAccountToken(ctx, newReader(), accountName, true)
		Expect(err).NotTo(BeNil())
		Expect(err.Reason).To(Equal("AccountNotFound"))
		Expect(err.Error()).To(ContainSubstring(accountName))
	})

	It("reports AccountNotReady only when readiness is required", func() {
		c := newReader(account("CF_API_TOKEN", false), secret(map[string][]byte{"CF_API_TOKEN": []byte("tok")}))

		_, _, err := resolveAccountToken(ctx, c, accountName, true)
		Expect(err).NotTo(BeNil())
		Expect(err.Reason).To(Equal("AccountNotReady"))

		_, token, err := resolveAccountToken(ctx, c, accountName, false)
		Expect(err).To(BeNil())
		Expect(token).To(Equal("tok"))
	})

	It("reports SecretNotFound when the referenced Secret does not exist", func() {
		_, _, err := resolveAccountToken(ctx, newReader(account("CF_API_TOKEN", true)), accountName, true)
		Expect(err).NotTo(BeNil())
		Expect(err.Reason).To(Equal("SecretNotFound"))
		Expect(err.Message).To(ContainSubstring(secretNS + "/" + secretName))
	})

	It("reports TokenKeyMissing when the Secret lacks the key", func() {
		c := newReader(account("CF_API_TOKEN", true), secret(map[string][]byte{"OTHER": []byte("x")}))
		_, _, err := resolveAccountToken(ctx, c, accountName, true)
		Expect(err).NotTo(BeNil())
		Expect(err.Reason).To(Equal("TokenKeyMissing"))
		Expect(err.Message).To(ContainSubstring(`"CF_API_TOKEN"`))
	})
})
