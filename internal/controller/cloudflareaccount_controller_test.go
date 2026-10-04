/*
Copyright 2026 Jonathan Graniero.

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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
)

// fakeCFClient is a test double for CloudflareAccountAPI.
type fakeCFClient struct {
	account cloudflare.Account
	err     error
}

func (f *fakeCFClient) Account(_ context.Context, _ string) (cloudflare.Account, cloudflare.ResultInfo, error) {
	return f.account, cloudflare.ResultInfo{}, f.err
}

// reconcilerWithFake returns a reconciler wired to a pre-built fake CF client.
func reconcilerWithFake(fake CloudflareAccountAPI) *CloudflareAccountReconciler {
	return &CloudflareAccountReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
		NewCFClient: func(_ string) (CloudflareAccountAPI, error) {
			return fake, nil
		},
	}
}

// reconcilerWithClientErr returns a reconciler whose client constructor always errors.
func reconcilerWithClientErr(err error) *CloudflareAccountReconciler {
	return &CloudflareAccountReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
		NewCFClient: func(_ string) (CloudflareAccountAPI, error) {
			return nil, err
		},
	}
}

var _ = Describe("CloudflareAccount Controller", func() {
	const (
		accountName = "test-account"
		secretName  = "cf-token-secret"
		secretNS    = "default"
		tokenKey    = "CF_API_TOKEN"
		fakeToken   = "fake-token-value"
		fakeAcctID  = "abc123"
	)

	ctx := context.Background()

	accountKey := types.NamespacedName{Name: accountName}
	secretKey := types.NamespacedName{Name: secretName, Namespace: secretNS}

	// createAccount creates a CloudflareAccount CR pointing at the test secret.
	createAccount := func() {
		acct := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID: fakeAcctID,
				TokenSecretRef: cloudflarev1alpha1.SecretReference{
					Name:      secretName,
					Namespace: secretNS,
					Key:       tokenKey,
				},
			},
		}
		Expect(k8sClient.Create(ctx, acct)).To(Succeed())
	}

	// createSecret creates the credentials secret with the given key/value.
	createSecret := func(key, value string) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: secretNS},
			Data:       map[string][]byte{key: []byte(value)},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	}

	// getCondition fetches the current CloudflareAccount and returns the named condition.
	getCondition := func(condType string) *metav1.Condition {
		acct := &cloudflarev1alpha1.CloudflareAccount{}
		Expect(k8sClient.Get(ctx, accountKey, acct)).To(Succeed())
		for i := range acct.Status.Conditions {
			if acct.Status.Conditions[i].Type == condType {
				return &acct.Status.Conditions[i]
			}
		}
		return nil
	}

	AfterEach(func() {
		// Clean up the CloudflareAccount CR.
		forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, accountKey)
		// Clean up the Secret.
		forceDelete(ctx, &corev1.Secret{}, secretKey)
	})

	Describe("Reconcile", func() {
		It("returns no error when the CloudflareAccount CR does not exist", func() {
			r := reconcilerWithFake(&fakeCFClient{})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: accountKey})
			Expect(err).NotTo(HaveOccurred())
		})

		It("sets Ready=False with reason SecretNotFound when the secret is missing", func() {
			createAccount()

			r := reconcilerWithFake(&fakeCFClient{})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: accountKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("SecretNotFound"))
		})

		It("sets Ready=False with reason TokenKeyMissing when the key is absent from the secret", func() {
			createAccount()
			createSecret("WRONG_KEY", fakeToken) // correct secret, wrong key name

			r := reconcilerWithFake(&fakeCFClient{})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: accountKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("TokenKeyMissing"))
		})

		It("sets Ready=False with reason InvalidToken when the CF client cannot be constructed", func() {
			createAccount()
			createSecret(tokenKey, fakeToken)

			r := reconcilerWithClientErr(errors.New("bad token"))
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: accountKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("InvalidToken"))
		})

		It("sets Ready=False TerminalError and does not requeue when Cloudflare rejects the token", func() {
			createAccount()
			createSecret(tokenKey, fakeToken)

			// cloudflare-go returns a pointer to this type for HTTP 403.
			forbidden := cloudflare.NewAuthenticationError(&cloudflare.Error{StatusCode: 403, Type: cloudflare.ErrorTypeAuthentication})
			r := reconcilerWithFake(&fakeCFClient{err: &forbidden})
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: accountKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())

			cond := getCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("TerminalError"))
		})

		It("sets Ready=False APIError and returns the error so a transient failure is retried", func() {
			createAccount()
			createSecret(tokenKey, fakeToken)

			r := reconcilerWithFake(&fakeCFClient{err: errors.New("connection reset by peer")})
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: accountKey})
			Expect(err).To(HaveOccurred())

			cond := getCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("APIError"))
		})

		It("sets Ready=True and populates accountName on successful validation", func() {
			createAccount()
			createSecret(tokenKey, fakeToken)

			fake := &fakeCFClient{account: cloudflare.Account{Name: "My CF Account"}}
			r := reconcilerWithFake(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: accountKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("Validated"))

			acct := &cloudflarev1alpha1.CloudflareAccount{}
			Expect(k8sClient.Get(ctx, accountKey, acct)).To(Succeed())
			Expect(acct.Status.AccountName).To(Equal("My CF Account"))
		})

		It("uses CF_API_TOKEN as the default key when TokenSecretRef.Key is empty", func() {
			// Create account with no explicit key set.
			acct := &cloudflarev1alpha1.CloudflareAccount{
				ObjectMeta: metav1.ObjectMeta{Name: accountName},
				Spec: cloudflarev1alpha1.CloudflareAccountSpec{
					AccountID: fakeAcctID,
					TokenSecretRef: cloudflarev1alpha1.SecretReference{
						Name:      secretName,
						Namespace: secretNS,
						// Key intentionally omitted — should default to CF_API_TOKEN
					},
				},
			}
			Expect(k8sClient.Create(ctx, acct)).To(Succeed())
			createSecret("CF_API_TOKEN", fakeToken)

			fake := &fakeCFClient{account: cloudflare.Account{Name: "Default Key Account"}}
			r := reconcilerWithFake(fake)
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: accountKey})
			Expect(err).NotTo(HaveOccurred())

			cond := getCondition(cloudflarev1alpha1.ConditionReady)
			Expect(cond).NotTo(BeNil())
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		})
	})
})
