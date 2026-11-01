/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"time"

	cf "github.com/cloudflare/cloudflare-go"
	"github.com/cloudflare/cloudflare-go/v7/kv"
	"github.com/cloudflare/cloudflare-go/v7/option"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// freeToken is the token the specs' Secrets hold, which no spec rate limits.
const freeToken = "token"

// heldTokens numbers the tokens newHeldToken makes.
var heldTokens atomic.Int64

// callCloudflare makes one call with token, through the transport every
// Client uses, to a Cloudflare answering with answer.
func callCloudflare(token string, answer http.HandlerFunc) error {
	GinkgoHelper()
	srv := httptest.NewServer(answer)
	defer srv.Close()
	client, err := cfpkg.New(token)
	Expect(err).NotTo(HaveOccurred())
	var params kv.NamespaceGetParams
	params.AccountID.Value, params.AccountID.Present = "account-id", true
	opts := append(client.V7Options(), option.WithBaseURL(srv.URL+"/"))
	_, err = kv.NewNamespaceService(opts...).Get(context.Background(), "ns-id", params)
	return err
}

// newHeldToken returns a token on which Cloudflare has answered a call with a
// 429 asking to wait an hour, as a token past its rate limit has: nothing
// short of the real path sets a hold.
func newHeldToken() string {
	GinkgoHelper()
	token := fmt.Sprintf("held-token-%d", heldTokens.Add(1))
	err := callCloudflare(token, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"result":null,"success":false,"errors":[{"code":10000,"message":"limited"}]}`))
	})
	Expect(cfpkg.IsRateLimit(err)).To(BeTrue(), "the 429 was not a rate limit: %v", err)
	return token
}

// setToken puts token in the Secret at key.
func setToken(ctx context.Context, key types.NamespacedName, token string) {
	GinkgoHelper()
	secret := &corev1.Secret{}
	Expect(k8sClient.Get(ctx, key, secret)).To(Succeed())
	secret.Data = map[string][]byte{"CF_API_TOKEN": []byte(token)}
	Expect(k8sClient.Update(ctx, secret)).To(Succeed())
}

// holdToken gives the Secret at key a token of its own that Cloudflare's rate
// limit holds for the rest of the spec.
func holdToken(ctx context.Context, key types.NamespacedName) {
	GinkgoHelper()
	setToken(ctx, key, newHeldToken())
}

// releaseToken gives the Secret at key a token no rate limit holds, which is
// how a spec ends a hold without waiting for it.
func releaseToken(ctx context.Context, key types.NamespacedName) {
	GinkgoHelper()
	setToken(ctx, key, freeToken)
}

// countingStatusClient counts the status writes a helper starts.
type countingStatusClient struct {
	client.StatusClient
	writes int
}

func (c *countingStatusClient) Status() client.SubResourceWriter {
	c.writes++
	return c.StatusClient.Status()
}

// These specs cover what every controller does about a rate limit, with a
// CloudflareAccount standing in for the kinds, as in updatePaused's specs. The
// shared reconcilers' spec tables cover where each one stops for it.
var _ = Describe("Waiting for a rate limit", func() {
	const name = "rate-limit-status"
	ctx := context.Background()
	var recorder *events.FakeRecorder
	var account *cloudflarev1alpha1.CloudflareAccount

	BeforeEach(func() {
		recorder = events.NewFakeRecorder(10)
		SetEventRecorder(recorder)
		DeferCleanup(func() { SetEventRecorder(nil) })
		account = &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: name, Finalizers: []string{reconciler.Finalizer}},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      "acct-" + name,
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: name, Namespace: "default", Key: "CF_API_TOKEN"},
			},
		}
		Expect(k8sClient.Create(ctx, account)).To(Succeed())
		DeferCleanup(func() {
			forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: name})
		})
	})

	stored := func() *cloudflarev1alpha1.CloudflareAccount {
		obj := &cloudflarev1alpha1.CloudflareAccount{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name}, obj)).To(Succeed())
		return obj
	}
	ready := func() *metav1.Condition {
		return meta.FindStatusCondition(stored().Status.Conditions, cloudflarev1alpha1.ConditionReady)
	}

	Describe("rateLimited", func() {
		It("comes back after the wait, and reports a resource that was not Ready once, with one Event", func() {
			result, err := rateLimited(ctx, k8sClient, account, time.Minute)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))
			Expect(ready().Status).To(Equal(metav1.ConditionFalse))
			Expect(ready().Reason).To(Equal(reasonRateLimited))
			Expect(ready().Message).To(Equal(rateLimitedMessage))
			Expect(recorder.Events).To(Receive(ContainSubstring("Warning " + reasonRateLimited)))

			By("not writing the status again while the resource waits")
			// The API server drops a write that changes nothing, so
			// resourceVersion would not show one: a backlog's would still load it.
			counted := &countingStatusClient{StatusClient: k8sClient}
			result, err = rateLimited(ctx, counted, stored(), 2*time.Minute)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(2 * time.Minute))
			Expect(counted.writes).To(BeZero())
			Expect(recorder.Events).NotTo(Receive())
		})

		It("leaves a Ready resource as it is, with no Event", func() {
			Expect(updateReady(ctx, k8sClient, account, "Validated", "valid")).To(Succeed())
			Expect(recorder.Events).To(Receive())

			counted := &countingStatusClient{StatusClient: k8sClient}
			result, err := rateLimited(ctx, counted, stored(), time.Minute)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))
			Expect(counted.writes).To(BeZero())
			Expect(ready().Reason).To(Equal("Validated"))
			Expect(recorder.Events).NotTo(Receive())
		})

		It("reports a resource that failed for another reason", func() {
			Expect(updateNotReady(ctx, k8sClient, account, "APIError", "failed")).To(Succeed())
			_, err := rateLimited(ctx, k8sClient, stored(), time.Minute)
			Expect(err).NotTo(HaveOccurred())
			Expect(ready().Reason).To(Equal(reasonRateLimited))
		})

		It("returns the error of a status write that failed", func() {
			account.ResourceVersion = "1"
			_, err := rateLimited(ctx, k8sClient, account, time.Minute)
			Expect(apierrors.IsConflict(err)).To(BeTrue(), "err = %v", err)
		})
	})

	DescribeTable("handleCloudflareError with a rate limit waits it out, rather than report a failure",
		func(failure func() error) {
			Expect(updateReady(ctx, k8sClient, account, "Validated", "valid")).To(Succeed())
			Expect(recorder.Events).To(Receive())

			result, err := handleCloudflareError(ctx, k8sClient, stored(), failure())
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(rateLimitRetryInterval))
			Expect(ready().Status).To(Equal(metav1.ConditionTrue))
			Expect(ready().Reason).To(Equal("Validated"))
			Expect(recorder.Events).NotTo(Receive())
		},
		Entry("v0's 429", func() error {
			limited := cf.NewRatelimitError(&cf.Error{StatusCode: 429, Type: cf.ErrorTypeRateLimit})
			return fmt.Errorf("read: %w", &limited)
		}),
		Entry("v7's 429", func() error { return &kv.Error{StatusCode: http.StatusTooManyRequests} }),
		Entry("a call the token's hold kept back", func() error {
			client, err := cfpkg.New(newHeldToken())
			Expect(err).NotTo(HaveOccurred())
			_, _, err = client.Account(ctx, "account-id")
			Expect(err).To(HaveOccurred())
			return err
		}),
	)

	It("handleCloudflareError still reports a failure that is not a rate limit", func() {
		result, err := handleCloudflareError(ctx, k8sClient, account, errors.New("connection reset"))
		Expect(err).To(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero())
		Expect(ready().Reason).To(Equal("APIError"))
	})

	Describe("a CloudflareAccount whose token is held", func() {
		var built int
		var r *CloudflareAccountReconciler
		secret := types.NamespacedName{Name: name, Namespace: "default"}
		reconcileIt := func() reconcile.Result {
			GinkgoHelper()
			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
			Expect(err).NotTo(HaveOccurred())
			return result
		}

		BeforeEach(func() {
			built = 0
			r = &CloudflareAccountReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				NewCFClient: func(string) (CloudflareAccountAPI, error) {
					built++
					return &fakeCFClient{account: cf.Account{ID: account.Spec.AccountID, Name: "Name"}}, nil
				},
			}
			Expect(k8sClient.Create(ctx, &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Data:       map[string][]byte{"CF_API_TOKEN": []byte(newHeldToken())},
			})).To(Succeed())
			DeferCleanup(forceDelete, ctx, &corev1.Secret{}, secret)
		})

		It("waits for it before it first checks the token", func() {
			Expect(reconcileIt().RequeueAfter).To(BeNumerically(">", 0))
			Expect(built).To(BeZero(), "a reconcile held by a rate limit called Cloudflare")
			Expect(ready().Status).To(Equal(metav1.ConditionFalse))
			Expect(ready().Reason).To(Equal(reasonRateLimited))

			By("checking it once the limit lifts")
			releaseToken(ctx, secret)
			reconcileIt()
			Expect(built).To(Equal(1))
			Expect(ready().Reason).To(Equal("Validated"))
		})

		It("stays Ready, so the resources that use it are not held up by more than the limit itself", func() {
			Expect(updateReady(ctx, k8sClient, account, "Validated", "valid")).To(Succeed())
			Expect(reconcileIt().RequeueAfter).To(BeNumerically(">", 0))
			Expect(built).To(BeZero(), "a reconcile held by a rate limit called Cloudflare")
			Expect(ready().Status).To(Equal(metav1.ConditionTrue))
		})
	})
})

var _ = Describe("Admitting a reconcile", func() {
	ctx := context.Background()

	DescribeTable("inSync",
		func(generation int64, deleting bool, condition *metav1.Condition, want bool) {
			obj := &cloudflarev1alpha1.KVNamespace{ObjectMeta: metav1.ObjectMeta{Generation: generation}}
			if deleting {
				obj.DeletionTimestamp = new(metav1.Now())
			}
			if condition != nil {
				condition.Type = cloudflarev1alpha1.ConditionReady
				obj.Status.Conditions = []metav1.Condition{*condition}
			}
			Expect(inSync(obj)).To(Equal(want))
		},
		Entry("Ready at the current generation", int64(2), false,
			&metav1.Condition{Status: metav1.ConditionTrue, ObservedGeneration: 2}, true),
		Entry("never reconciled", int64(1), false, nil, false),
		Entry("Ready at an older generation", int64(3), false,
			&metav1.Condition{Status: metav1.ConditionTrue, ObservedGeneration: 2}, false),
		Entry("not Ready", int64(2), false,
			&metav1.Condition{Status: metav1.ConditionFalse, ObservedGeneration: 2}, false),
		Entry("paused before its first reconcile", int64(2), false,
			&metav1.Condition{Status: metav1.ConditionUnknown, ObservedGeneration: 2}, false),
		Entry("being deleted", int64(2), true,
			&metav1.Condition{Status: metav1.ConditionTrue, ObservedGeneration: 2}, false),
	)

	Describe("spec.rateLimit", func() {
		answerOK := func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"result":{"id":"id","title":"title"},"success":true,"errors":[],"messages":[]}`))
		}
		account := func(rateLimit *cloudflarev1alpha1.RateLimit) *cloudflarev1alpha1.CloudflareAccount {
			return &cloudflarev1alpha1.CloudflareAccount{
				ObjectMeta: metav1.ObjectMeta{Name: "rate-limit-spec"},
				Spec:       cloudflarev1alpha1.CloudflareAccountSpec{RateLimit: rateLimit},
			}
		}

		It("keeps a resource in sync from waiting behind one with a change, on separate turns", func() {
			// One request every ten seconds, with a burst of 1.
			limited := account(&cloudflarev1alpha1.RateLimit{RequestsPerFiveMinutes: 30})
			token := fmt.Sprintf("spec-rate-limit-%d", heldTokens.Add(1))
			Expect(admit(limited, token, limited)).To(BeZero())
			Expect(callCloudflare(token, answerOK)).To(Succeed())

			changed := &cloudflarev1alpha1.KVNamespace{ObjectMeta: metav1.ObjectMeta{Generation: 2}}
			inSyncNow := changed.DeepCopy()
			inSyncNow.Generation = 1
			inSyncNow.Status.Conditions = []metav1.Condition{{
				Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 1,
			}}

			Expect(admit(limited, token, changed)).To(BeNumerically("~", 10*time.Second, time.Second))
			Expect(admit(limited, token, changed)).To(BeNumerically("~", 20*time.Second, time.Second))
			Expect(admit(limited, token, inSyncNow)).To(BeNumerically("~", 10*time.Second, time.Second),
				"a resource in sync took a turn among the changes")
		})

		It("sets how fast the account's token is spent", func() {
			// 30 requests per five minutes have a burst of 1, so one call
			// spends the budget.
			limited := account(&cloudflarev1alpha1.RateLimit{RequestsPerFiveMinutes: 30})
			token := fmt.Sprintf("spec-rate-limit-%d", heldTokens.Add(1))
			Expect(admit(limited, token, limited)).To(BeZero())
			Expect(callCloudflare(token, answerOK)).To(Succeed())
			Expect(admit(limited, token, limited)).To(BeNumerically(">", 0))
		})

		It("leaves a token to the manager's rate limit without one", func() {
			unset := account(nil)
			token := fmt.Sprintf("spec-rate-limit-%d", heldTokens.Add(1))
			Expect(admit(unset, token, unset)).To(BeZero())
			Expect(callCloudflare(token, answerOK)).To(Succeed())
			Expect(admit(unset, token, unset)).To(BeZero())
		})

		DescribeTable("the CRD",
			func(requests int32, valid bool) {
				obj := account(&cloudflarev1alpha1.RateLimit{RequestsPerFiveMinutes: requests})
				obj.Name = fmt.Sprintf("rate-limit-crd-%d", requests)
				obj.Spec.AccountID = "acct"
				obj.Spec.TokenSecretRef = cloudflarev1alpha1.SecretReference{Name: "token", Namespace: "default"}
				err := k8sClient.Create(ctx, obj)
				if !valid {
					Expect(apierrors.IsInvalid(err)).To(BeTrue(), "err = %v", err)
					return
				}
				Expect(err).NotTo(HaveOccurred())
				DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.CloudflareAccount{},
					types.NamespacedName{Name: obj.Name})
			},
			Entry("takes one request", int32(1), true),
			Entry("takes a raised limit", int32(60000), true),
			Entry("refuses none", int32(0), false),
			Entry("refuses a negative rate", int32(-1), false),
		)
	})

	Describe("retryCleanup", func() {
		It("comes back after a deferred cleanup's wait", func() {
			result, err := retryCleanup(fmt.Errorf("cleanup: %w", &cleanupDeferred{wait: time.Minute}))
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Minute))
		})

		It("comes back soon from a cleanup a rate limit stopped partway, with no error", func() {
			result, err := retryCleanup(&kv.Error{StatusCode: http.StatusTooManyRequests})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(rateLimitRetryInterval))
		})

		It("returns any other error", func() {
			failure := errors.New("connection reset")
			result, err := retryCleanup(failure)
			Expect(err).To(MatchError(failure))
			Expect(result.IsZero()).To(BeTrue())
		})

		It("says what a deferred cleanup waits for", func() {
			Expect((&cleanupDeferred{wait: time.Minute}).Error()).To(ContainSubstring("rate limit"))
		})
	})
})

// recordingKVNamespaceAPIs builds a KVNamespaceAPI per call, and records the
// tokens they were built with, which is how far a reconcile got.
type recordingKVNamespaceAPIs struct {
	api *fakeKVNamespaceAPI

	mu     sync.Mutex
	tokens map[string]int
}

func (r *recordingKVNamespaceAPIs) build(token string) (KVNamespaceAPI, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tokens[token]++
	return r.api, nil
}

func (r *recordingKVNamespaceAPIs) uses(token string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tokens[token]
}

// This spec runs a controller as the manager does, one reconcile at a time and
// from a real queue, for the case the rate limit's design turns on: one
// manager serving several Cloudflare accounts. Every kind stops for a rate
// limit the same way, so KVNamespace stands in for all of them.
var _ = Describe("A rate limit on one of a manager's accounts", func() {
	const (
		ns         = "default"
		held, free = "rate-limit-held", "rate-limit-free"
		backlog    = 25
	)
	ctx := context.Background()

	ready := func(key types.NamespacedName) *metav1.Condition {
		obj := &cloudflarev1alpha1.KVNamespace{}
		Expect(k8sClient.Get(ctx, key, obj)).To(Succeed())
		return meta.FindStatusCondition(obj.Status.Conditions, cloudflarev1alpha1.ConditionReady)
	}

	createAccount := func(name, token string) {
		account := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      "acct-" + name,
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: name, Namespace: ns, Key: "CF_API_TOKEN"},
			},
		}
		Expect(k8sClient.Create(ctx, account)).To(Succeed())
		DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: name})
		account.Status.Conditions = []metav1.Condition{{
			Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
			Reason: "Validated", LastTransitionTime: metav1.Now(),
		}}
		Expect(k8sClient.Status().Update(ctx, account)).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Data:       map[string][]byte{"CF_API_TOKEN": []byte(token)},
		})).To(Succeed())
		DeferCleanup(forceDelete, ctx, &corev1.Secret{}, types.NamespacedName{Name: name, Namespace: ns})
	}

	createNamespace := func(name, account string) types.NamespacedName {
		key := types.NamespacedName{Name: name, Namespace: ns}
		Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.KVNamespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: ns, Finalizers: []string{reconciler.Finalizer},
			},
			Spec: cloudflarev1alpha1.KVNamespaceSpec{
				AccountRef: corev1.LocalObjectReference{Name: account},
				Title:      name,
			},
		})).To(Succeed())
		DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.KVNamespace{}, key)
		return key
	}

	It("leaves the other account's resources to reconcile while its own wait", func() {
		heldToken := newHeldToken()
		createAccount(held, heldToken)
		createAccount(free, freeToken)

		By("queueing a backlog on the held account ahead of one resource of the other")
		backlogKeys := make([]types.NamespacedName, 0, backlog)
		for i := range backlog {
			backlogKeys = append(backlogKeys, createNamespace(fmt.Sprintf("%s-%d", held, i), held))
		}
		freeKey := createNamespace(free+"-namespace", free)

		apis := &recordingKVNamespaceAPIs{
			api:    &fakeKVNamespaceAPI{newFakeCRUDStore[kv.Namespace](kvNamespaceListPageSize, "")},
			tokens: map[string]int{},
		}
		mgr, err := manager.New(cfg, manager.Options{
			Controller: config.Controller{SkipNameValidation: new(true)},
			Metrics:    metricsserver.Options{BindAddress: "0"},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect((&KVNamespaceReconciler{
			Client: mgr.GetClient(), Scheme: mgr.GetScheme(), NewKVNamespaceAPI: apis.build,
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

		Eventually(func(g Gomega) {
			cond := ready(freeKey)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal("Synced"))
		}).WithTimeout(10*time.Second).Should(Succeed(), "the held account's backlog held up the other account")

		Eventually(func(g Gomega) {
			for _, key := range backlogKeys {
				cond := ready(key)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Reason).To(Equal(reasonRateLimited))
			}
		}).WithTimeout(10 * time.Second).Should(Succeed())
		Expect(apis.uses(heldToken)).To(BeZero(), "a reconcile held by a rate limit called Cloudflare")
		Expect(apis.uses(freeToken)).To(BeNumerically(">", 0))
	})
})
