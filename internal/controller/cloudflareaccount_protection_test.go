/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"

	cf "github.com/cloudflare/cloudflare-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

var _ = Describe("CloudflareAccount deletion protection", func() {
	const (
		acctName   = "protect-account"
		otherAcct  = "protect-other-account"
		secretName = "protect-token"
		newSecret  = "protect-token-rotated"
		ns         = "default"
		zoneName   = "protect-zone"
	)

	ctx := context.Background()
	acctKey := types.NamespacedName{Name: acctName}
	secretKey := types.NamespacedName{Name: secretName, Namespace: ns}
	newSecretKey := types.NamespacedName{Name: newSecret, Namespace: ns}
	zoneKey := types.NamespacedName{Name: zoneName, Namespace: ns}

	r := &CloudflareAccountReconciler{
		NewCFClient: func(string) (CloudflareAccountAPI, error) {
			return &fakeCFClient{account: cf.Account{Name: "Protected"}}, nil
		},
	}

	newAccount := func(name, secret string) *cloudflarev1alpha1.CloudflareAccount {
		return &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      "acct",
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: secret, Namespace: ns, Key: "CF_API_TOKEN"},
			},
		}
	}
	createSecret := func(name string) {
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Data:       map[string][]byte{"CF_API_TOKEN": []byte("token")},
		})).To(Succeed())
	}
	reconcileAccount := func(name string) error {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
		return err
	}
	getSecret := func(key types.NamespacedName) *corev1.Secret {
		s := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, key, s)).To(Succeed())
		return s
	}
	getAccount := func() *cloudflarev1alpha1.CloudflareAccount {
		a := &cloudflarev1alpha1.CloudflareAccount{}
		Expect(k8sClient.Get(ctx, acctKey, a)).To(Succeed())
		return a
	}

	BeforeEach(func() {
		r.Client = k8sClient
		createSecret(secretName)
		Expect(k8sClient.Create(ctx, newAccount(acctName, secretName))).To(Succeed())
		Expect(reconcileAccount(acctName)).To(Succeed())
	})

	AfterEach(func() {
		forceDelete(ctx, &cloudflarev1alpha1.Zone{}, zoneKey)
		forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, acctKey)
		forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: otherAcct})
		forceDelete(ctx, &corev1.Secret{}, secretKey)
		forceDelete(ctx, &corev1.Secret{}, newSecretKey)
	})

	It("holds a finalizer on the account and its token Secret", func() {
		Expect(getAccount().Finalizers).To(ContainElement(reconciler.Finalizer))
		Expect(getSecret(secretKey).Finalizers).To(ContainElement(tokenSecretFinalizer))
		Expect(getAccount().Status.ProtectedTokenSecret).To(Equal(&corev1.SecretReference{Name: secretName, Namespace: ns}))
	})

	It("waits for the resources that use the account before releasing it", func() {
		Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.Zone{
			ObjectMeta: metav1.ObjectMeta{Name: zoneName, Namespace: ns},
			Spec: cloudflarev1alpha1.ZoneSpec{Name: "protect.example.com",
				AccountRef: corev1.LocalObjectReference{Name: acctName}},
		})).To(Succeed())
		Expect(k8sClient.Delete(ctx, getAccount())).To(Succeed())

		Expect(reconcileAccount(acctName)).To(Succeed())
		account := getAccount()
		Expect(account.Finalizers).To(ContainElement(reconciler.Finalizer))
		ready := account.Status.Conditions[0]
		Expect(ready.Reason).To(Equal("InUse"))
		Expect(ready.Message).To(ContainSubstring("Zone default/" + zoneName))
		Expect(getSecret(secretKey).Finalizers).To(ContainElement(tokenSecretFinalizer))

		// The Zone's deletion triggers the account, which can now go.
		forceDelete(ctx, &cloudflarev1alpha1.Zone{}, zoneKey)
		Expect(reconcileAccount(acctName)).To(Succeed())
		err := k8sClient.Get(ctx, acctKey, &cloudflarev1alpha1.CloudflareAccount{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		Expect(getSecret(secretKey).Finalizers).NotTo(ContainElement(tokenSecretFinalizer))
	})

	It("keeps the token Secret protected while another account still uses it", func() {
		Expect(k8sClient.Create(ctx, newAccount(otherAcct, secretName))).To(Succeed())
		Expect(k8sClient.Delete(ctx, getAccount())).To(Succeed())
		Expect(reconcileAccount(acctName)).To(Succeed())

		err := k8sClient.Get(ctx, acctKey, &cloudflarev1alpha1.CloudflareAccount{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
		Expect(getSecret(secretKey).Finalizers).To(ContainElement(tokenSecretFinalizer))
	})

	It("releases the old token Secret when tokenSecretRef changes", func() {
		createSecret(newSecret)
		account := getAccount()
		account.Spec.TokenSecretRef.Name = newSecret
		Expect(k8sClient.Update(ctx, account)).To(Succeed())
		Expect(reconcileAccount(acctName)).To(Succeed())

		Expect(getSecret(secretKey).Finalizers).NotTo(ContainElement(tokenSecretFinalizer))
		Expect(getSecret(newSecretKey).Finalizers).To(ContainElement(tokenSecretFinalizer))
		Expect(getAccount().Status.ProtectedTokenSecret.Name).To(Equal(newSecret))
	})

	It("re-triggers a deleting account when a dependent goes away, and only then", func() {
		zone := &cloudflarev1alpha1.Zone{Spec: cloudflarev1alpha1.ZoneSpec{
			AccountRef: corev1.LocalObjectReference{Name: acctName}}}
		Expect(r.deletingAccountOf(ctx, zone)).To(BeEmpty())

		Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.Zone{
			ObjectMeta: metav1.ObjectMeta{Name: zoneName, Namespace: ns},
			Spec: cloudflarev1alpha1.ZoneSpec{Name: "protect.example.com",
				AccountRef: corev1.LocalObjectReference{Name: acctName}},
		})).To(Succeed())
		Expect(k8sClient.Delete(ctx, getAccount())).To(Succeed())
		ref := corev1.LocalObjectReference{Name: acctName}
		for _, dependent := range []client.Object{
			zone,
			&cloudflarev1alpha1.Tunnel{Spec: cloudflarev1alpha1.TunnelSpec{AccountRef: ref}},
			&cloudflarev1alpha1.WorkerScript{Spec: cloudflarev1alpha1.WorkerScriptSpec{AccountRef: ref}},
		} {
			Expect(r.deletingAccountOf(ctx, dependent)).To(ConsistOf(reconcile.Request{NamespacedName: acctKey}))
		}
		Expect(r.deletingAccountOf(ctx, &corev1.Secret{})).To(BeEmpty())
	})

	It("names every kind of dependent and shortens a long list", func() {
		ref := corev1.LocalObjectReference{Name: acctName}
		script := "export default {}"
		dependents := []client.Object{
			&cloudflarev1alpha1.Tunnel{ObjectMeta: metav1.ObjectMeta{Name: "protect-tunnel", Namespace: ns},
				Spec: cloudflarev1alpha1.TunnelSpec{Name: "t", AccountRef: ref,
					CredentialsSecretRef: cloudflarev1alpha1.TunnelCredentialsSecretReference{Name: "t-token"}}},
			&cloudflarev1alpha1.WorkerScript{ObjectMeta: metav1.ObjectMeta{Name: "protect-worker", Namespace: ns},
				Spec: cloudflarev1alpha1.WorkerScriptSpec{Name: "w", AccountRef: ref, Script: &script}},
		}
		for _, z := range []string{"protect-z1", "protect-z2", "protect-z3", "protect-z4"} {
			dependents = append(dependents, &cloudflarev1alpha1.Zone{ObjectMeta: metav1.ObjectMeta{Name: z, Namespace: ns},
				Spec: cloudflarev1alpha1.ZoneSpec{Name: z + ".example.com", AccountRef: ref}})
		}
		for _, d := range dependents {
			Expect(k8sClient.Create(ctx, d)).To(Succeed())
			DeferCleanup(forceDelete, ctx, d.DeepCopyObject().(client.Object), client.ObjectKeyFromObject(d))
		}
		Expect(k8sClient.Delete(ctx, getAccount())).To(Succeed())

		Expect(reconcileAccount(acctName)).To(Succeed())
		msg := getAccount().Status.Conditions[0].Message
		Expect(msg).To(HavePrefix("Deletion is waiting for 6 resource(s)"))
		Expect(msg).To(ContainSubstring("Tunnel default/protect-tunnel"))
		Expect(msg).To(ContainSubstring("WorkerScript default/protect-worker"))
		Expect(msg).To(HaveSuffix(", ..."))
	})

	It("does nothing for a deleting account without the finalizer", func() {
		account := getAccount()
		account.Finalizers = nil
		result, err := r.reconcileDelete(ctx, account)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.IsZero()).To(BeTrue())
	})

	It("ignores a token Secret that no longer exists when releasing it", func() {
		Expect(r.releaseTokenSecret(ctx, corev1.SecretReference{Name: "gone", Namespace: ns}, acctName)).To(Succeed())
	})

	It("releases both the recorded and the referenced Secret when they differ", func() {
		account := newAccount(acctName, newSecret)
		account.Status.ProtectedTokenSecret = &corev1.SecretReference{Name: secretName, Namespace: ns}
		Expect(protectedSecrets(account)).To(ConsistOf(
			corev1.SecretReference{Name: newSecret, Namespace: ns},
			corev1.SecretReference{Name: secretName, Namespace: ns},
		))
	})
})

var _ = Describe("deletesOnly", func() {
	It("passes delete events and nothing else", func() {
		Expect(deletesOnly.Delete(event.DeleteEvent{})).To(BeTrue())
		Expect(deletesOnly.Create(event.CreateEvent{})).To(BeFalse())
		Expect(deletesOnly.Update(event.UpdateEvent{})).To(BeFalse())
		Expect(deletesOnly.Generic(event.GenericEvent{})).To(BeFalse())
	})
})
