/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/cloudflare/cloudflare-go"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
)

// The token check interval and expiry warning these specs give the
// reconciler, as the manager's defaults do.
const (
	tokenCheckTestInterval = time.Hour
	tokenExpiryTestWarning = 14 * 24 * time.Hour
)

var _ = Describe("A CloudflareAccount's API token", func() {
	const (
		name     = "token-account"
		secretNS = "default"
		secret   = secretNS + "/" + name
	)
	ctx := context.Background()
	key := types.NamespacedName{Name: name}
	var fake *fakeCFClient
	var r *CloudflareAccountReconciler
	var recorder *events.FakeRecorder

	BeforeEach(func() {
		fake = &fakeCFClient{account: cloudflare.Account{Name: "Token Account"}}
		r = reconcilerWithFake(fake)
		r.TokenCheckInterval = tokenCheckTestInterval
		r.TokenExpiryWarning = tokenExpiryTestWarning
		recorder = events.NewFakeRecorder(20)
		SetEventRecorder(recorder)
		DeferCleanup(func() { SetEventRecorder(nil) })

		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: secretNS},
			Data:       map[string][]byte{"CF_API_TOKEN": []byte("token-value")},
		})).To(Succeed())
		DeferCleanup(forceDelete, ctx, &corev1.Secret{}, types.NamespacedName{Name: name, Namespace: secretNS})
		Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      "acct-token",
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: name, Namespace: secretNS, Key: "CF_API_TOKEN"},
			},
		})).To(Succeed())
		DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.CloudflareAccount{}, key)
	})

	reconcileIt := func() (reconcile.Result, error) {
		return r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
	}
	validate := func() reconcile.Result {
		GinkgoHelper()
		result, err := reconcileIt()
		Expect(err).NotTo(HaveOccurred())
		return result
	}
	stored := func() *cloudflarev1alpha1.CloudflareAccount {
		GinkgoHelper()
		account := &cloudflarev1alpha1.CloudflareAccount{}
		Expect(k8sClient.Get(ctx, key, account)).To(Succeed())
		return account
	}
	ready := func() *metav1.Condition {
		GinkgoHelper()
		return meta.FindStatusCondition(stored().Status.Conditions, cloudflarev1alpha1.ConditionReady)
	}
	// expiringIn sets the fake's token to expire d from now, to the second,
	// as the API server stores it.
	expiringIn := func(d time.Duration) time.Time {
		at := time.Now().Add(d).Truncate(time.Second)
		fake.verification = cfpkg.TokenVerification{Status: cfpkg.TokenActive, ExpiresOn: at}
		return at
	}
	// expiryWarnings drains the recorder and returns its TokenExpiresSoon
	// Events.
	expiryWarnings := func() []string {
		var warnings []string
		for {
			select {
			case event := <-recorder.Events:
				if strings.HasPrefix(event, "Warning "+reasonTokenExpiresSoon+" ") {
					warnings = append(warnings, event)
				}
			default:
				return warnings
			}
		}
	}

	Describe("its status", func() {
		It("records an active token's status and expiry", func() {
			at := expiringIn(90 * 24 * time.Hour)
			validate()
			Expect(fake.verifications).To(Equal(1))
			Expect(ready().Reason).To(Equal("Validated"))
			Expect(stored().Status.TokenStatus).To(Equal(cfpkg.TokenActive))
			Expect(stored().Status.TokenExpiresAt).NotTo(BeNil())
			Expect(stored().Status.TokenExpiresAt.Time).To(BeTemporally("==", at))
		})

		It("records no expiry for a token that never expires", func() {
			validate()
			Expect(stored().Status.TokenStatus).To(Equal(cfpkg.TokenActive))
			Expect(stored().Status.TokenExpiresAt).To(BeNil())
		})

		It("clears an expiry the token no longer has", func() {
			expiringIn(90 * 24 * time.Hour)
			validate()
			Expect(stored().Status.TokenExpiresAt).NotTo(BeNil())

			fake.verification = cfpkg.TokenVerification{Status: cfpkg.TokenActive}
			validate()
			Expect(stored().Status.TokenExpiresAt).To(BeNil())
		})

		It("forgets the token once Cloudflare refuses it, without reading the account", func() {
			expiringIn(90 * 24 * time.Hour)
			validate()

			fake.verifyErr = v7APIError(http.MethodGet, "accounts/acct-token/tokens/verify", http.StatusUnauthorized,
				1000, "Invalid API Token")
			result := validate()
			Expect(result.IsZero()).To(BeTrue(), "a refused token is checked again once something changes")
			Expect(ready().Reason).To(Equal("TerminalError"))
			Expect(ready().Message).To(ContainSubstring("Invalid API Token"))
			Expect(stored().Status.TokenStatus).To(BeEmpty())
			Expect(stored().Status.TokenExpiresAt).To(BeNil())
			Expect(fake.reads).To(Equal(1), "the account was read with a token Cloudflare refused")
		})

		It("keeps what it knows of the token through an error to retry", func() {
			at := expiringIn(90 * 24 * time.Hour)
			validate()

			fake.verifyErr = v7APIError(http.MethodGet, "accounts/acct-token/tokens/verify",
				http.StatusInternalServerError, 10000, "Internal error")
			_, err := reconcileIt()
			Expect(err).To(HaveOccurred(), "an error to retry is returned, for controller-runtime's back-off")
			Expect(ready().Reason).To(Equal("APIError"))
			Expect(stored().Status.TokenStatus).To(Equal(cfpkg.TokenActive))
			Expect(stored().Status.TokenExpiresAt.Time).To(BeTemporally("==", at))
		})

		It("forgets the token once its Secret no longer holds one", func() {
			expiringIn(90 * 24 * time.Hour)
			validate()

			forceDelete(ctx, &corev1.Secret{}, types.NamespacedName{Name: name, Namespace: secretNS})
			validate()
			Expect(ready().Reason).To(Equal("SecretNotFound"))
			Expect(stored().Status.TokenStatus).To(BeEmpty())
			Expect(stored().Status.TokenExpiresAt).To(BeNil())
		})

		It("forgets the token once no client can be built from it", func() {
			expiringIn(90 * 24 * time.Hour)
			validate()

			r.NewCFClient = reconcilerWithClientErr(errInvalidTestToken).NewCFClient
			validate()
			Expect(ready().Reason).To(Equal("InvalidToken"))
			Expect(stored().Status.TokenStatus).To(BeEmpty())
			Expect(stored().Status.TokenExpiresAt).To(BeNil())
		})

		It("goes by the account's read for a status it does not know", func() {
			fake.verification = cfpkg.TokenVerification{Status: "pending"}
			validate()
			Expect(fake.reads).To(Equal(1))
			Expect(ready().Reason).To(Equal("Validated"))
			Expect(stored().Status.TokenStatus).To(Equal("pending"))
		})
	})

	Describe("a token Cloudflare reports unusable", func() {
		It("turns Ready False with TokenExpired, naming the expiry, without reading the account", func() {
			at := time.Date(2026, 11, 9, 20, 7, 5, 0, time.UTC)
			fake.verification = cfpkg.TokenVerification{Status: cfpkg.TokenExpired, ExpiresOn: at}
			result := validate()
			Expect(fake.reads).To(BeZero())
			Expect(ready().Status).To(Equal(metav1.ConditionFalse))
			Expect(ready().Reason).To(Equal("TokenExpired"))
			Expect(ready().Message).To(Equal("The API token in Secret " + secret + " expired at " +
				"2026-11-09T20:07:05Z: put a new token in the Secret, or extend the token's expiry in Cloudflare"))
			Expect(stored().Status.TokenStatus).To(Equal(cfpkg.TokenExpired))
			Expect(stored().Status.TokenExpiresAt.Time).To(BeTemporally("==", at))
			Expect(result.RequeueAfter).To(Equal(tokenCheckTestInterval),
				"an expiry moved in Cloudflare changes nothing in the cluster")
			Expect(expiryWarnings()).To(BeEmpty())
		})

		It("says the token has expired when Cloudflare gives no expiry", func() {
			fake.verification = cfpkg.TokenVerification{Status: cfpkg.TokenExpired}
			validate()
			Expect(ready().Reason).To(Equal("TokenExpired"))
			Expect(ready().Message).To(HavePrefix("The API token in Secret " + secret + " has expired: "))
		})

		It("turns Ready False with TokenDisabled, without reading the account", func() {
			fake.verification = cfpkg.TokenVerification{Status: cfpkg.TokenDisabled}
			result := validate()
			Expect(fake.reads).To(BeZero())
			Expect(ready().Status).To(Equal(metav1.ConditionFalse))
			Expect(ready().Reason).To(Equal("TokenDisabled"))
			Expect(ready().Message).To(Equal("Cloudflare reports the API token in Secret " + secret + " as " +
				"disabled: enable it again in Cloudflare, or put a new token in the Secret"))
			Expect(stored().Status.TokenStatus).To(Equal(cfpkg.TokenDisabled))
			Expect(result.RequeueAfter).To(Equal(tokenCheckTestInterval),
				"a token enabled again in Cloudflare changes nothing in the cluster")
		})

		It("keeps the same message from one check to the next", func() {
			fake.verification = cfpkg.TokenVerification{Status: cfpkg.TokenExpired,
				ExpiresOn: time.Now().Add(-time.Hour).Truncate(time.Second)}
			validate()
			first := ready()
			validate()
			Expect(ready().Message).To(Equal(first.Message))
			Expect(ready().LastTransitionTime).To(Equal(first.LastTransitionTime))
		})
	})

	Describe("the next validation", func() {
		It("comes after the token check interval", func() {
			expiringIn(90 * 24 * time.Hour)
			Expect(validate().RequeueAfter).To(Equal(tokenCheckTestInterval))
		})

		It("comes just after the token expires, when that is sooner", func() {
			at := expiringIn(10 * time.Minute)
			result := validate()
			Expect(result.RequeueAfter).To(BeNumerically("~", time.Until(at)+30*time.Second, 2*time.Second))
		})

		It("comes again just after an expiry Cloudflare has not caught up with", func() {
			at := expiringIn(-10 * time.Second)
			result := validate()
			Expect(result.RequeueAfter).To(BeNumerically("~", time.Until(at)+30*time.Second, 2*time.Second))

			By("and after the interval once the expiry is further past")
			expiringIn(-time.Minute)
			Expect(validate().RequeueAfter).To(Equal(tokenCheckTestInterval))
		})

		It("comes just after the token expires without a token check interval", func() {
			r.TokenCheckInterval = 0
			at := expiringIn(10 * 24 * time.Hour)
			Expect(validate().RequeueAfter).To(BeNumerically("~", time.Until(at)+30*time.Second, 2*time.Second))
		})

		It("is not scheduled without an interval or an expiry", func() {
			r.TokenCheckInterval = 0
			Expect(validate()).To(BeZero())
		})

		It("comes at the drift check, when that is sooner", func() {
			r.TokenCheckInterval = 3 * driftCheckTestInterval
			turnOnDriftChecks()
			Expect(validate().RequeueAfter).To(BeNumerically("~", driftCheckTestInterval, driftCheckTestSpread))

			By("and at the token check, when that is sooner")
			r.TokenCheckInterval = driftCheckTestInterval / 2
			Expect(validate().RequeueAfter).To(Equal(driftCheckTestInterval / 2))
		})
	})

	Describe("the warning before the token expires", func() {
		It("is a Warning Event at each validation while the token expires within the warning", func() {
			at := expiringIn(3 * 24 * time.Hour)
			want := "Warning TokenExpiresSoon The API token in Secret " + secret + " expires at " +
				at.UTC().Format(time.RFC3339) + ": put a new token in the Secret before then"
			validate()
			Expect(expiryWarnings()).To(Equal([]string{want}))
			validate()
			Expect(expiryWarnings()).To(Equal([]string{want}),
				"the API server drops an Event after an hour, so one alone would be gone well before the token")
			Expect(ready().Status).To(Equal(metav1.ConditionTrue), "a token close to its expiry still works")
		})

		It("is not recorded for a token that expires later", func() {
			expiringIn(tokenExpiryTestWarning + time.Hour)
			validate()
			Expect(expiryWarnings()).To(BeEmpty())
		})

		It("is not recorded without a warning period", func() {
			r.TokenExpiryWarning = 0
			expiringIn(time.Hour)
			validate()
			Expect(expiryWarnings()).To(BeEmpty())
		})

		It("is not recorded for an expiry already past, which Cloudflare has not caught up with", func() {
			expiringIn(-time.Minute)
			validate()
			Expect(expiryWarnings()).To(BeEmpty())
		})

		It("is not recorded for a token that never expires", func() {
			validate()
			Expect(expiryWarnings()).To(BeEmpty())
		})

		It("is not recorded when the account cannot be read", func() {
			expiringIn(time.Hour)
			fake.err = cloudflareError(http.StatusForbidden, 9109)
			validate()
			Expect(ready().Reason).To(Equal("TerminalError"))
			Expect(expiryWarnings()).To(BeEmpty())
		})
	})

	Describe("kflare_account_token_expiry_timestamp_seconds", func() {
		// accountsOnly lists CloudflareAccounts alone, and every other kind as
		// empty: the specs' client keeps client-go's rate limit, which listing
		// every kind would run into.
		accountsOnly := listFunc(func(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*cloudflarev1alpha1.CloudflareAccountList); ok {
				return k8sClient.List(ctx, list, opts...)
			}
			return nil
		})
		series := func(scope WatchScope) map[string]float64 {
			GinkgoHelper()
			return tokenExpirySeries(testGatherer(newResourceCollector(accountsOnly, scheme.Scheme, closedChannel(),
				scope)))
		}

		It("holds each recorded expiry, by account", func() {
			at := expiringIn(30 * 24 * time.Hour)
			validate()
			Expect(series(WatchScope{})).To(HaveKeyWithValue(name, float64(at.Unix())))

			By("leaving out an account whose token never expires")
			fake.verification = cfpkg.TokenVerification{Status: cfpkg.TokenActive}
			validate()
			Expect(series(WatchScope{})).NotTo(HaveKey(name))
		})

		It("is served when another kind's cache is late", func() {
			at := expiringIn(30 * 24 * time.Hour)
			validate()
			late := listFunc(func(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*cloudflarev1alpha1.CloudflareAccountList); ok {
					return k8sClient.List(ctx, list, opts...)
				}
				<-ctx.Done()
				return ctx.Err()
			})
			gatherer := testGatherer(newResourceCollector(late, scheme.Scheme, closedChannel(), WatchScope{}))
			Expect(tokenExpirySeries(gatherer)).To(HaveKeyWithValue(name, float64(at.Unix())))
		})

		It("leaves out an account outside the installation's scope", func() {
			expiringIn(30 * 24 * time.Hour)
			validate()
			scope, err := NewWatchScope("elsewhere", "", k8sClient)
			Expect(err).NotTo(HaveOccurred())
			Expect(series(scope)).NotTo(HaveKey(name))
		})
	})
})

// errInvalidTestToken is the error of a client that cannot be built.
var errInvalidTestToken = errors.New("invalid token")

// tokenExpirySeries returns kflare_account_token_expiry_timestamp_seconds, by
// account.
func tokenExpirySeries(gatherer prometheus.Gatherer) map[string]float64 {
	GinkgoHelper()
	families, err := gatherer.Gather()
	Expect(err).NotTo(HaveOccurred(), "the scrape failed")
	series := map[string]float64{}
	for _, family := range families {
		if family.GetName() != "kflare_account_token_expiry_timestamp_seconds" {
			continue
		}
		for _, metric := range family.GetMetric() {
			Expect(metric.GetLabel()).To(HaveLen(1))
			series[metric.GetLabel()[0].GetValue()] = metric.GetGauge().GetValue()
		}
	}
	return series
}

// lockedTokenAPI is a CloudflareAccountAPI that a running manager calls while
// a spec changes what Cloudflare reports about the token.
type lockedTokenAPI struct {
	mu            sync.Mutex
	status        string
	verifications int
}

func (l *lockedTokenAPI) set(status string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.status = status
}

func (l *lockedTokenAPI) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.verifications
}

func (l *lockedTokenAPI) Account(context.Context, string) (cloudflare.Account, cloudflare.ResultInfo, error) {
	return cloudflare.Account{Name: "Scheduled"}, cloudflare.ResultInfo{}, nil
}

func (l *lockedTokenAPI) VerifyToken(context.Context, string) (cfpkg.TokenVerification, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.verifications++
	return cfpkg.TokenVerification{Status: l.status}, nil
}

var _ = Describe("A CloudflareAccount in a running manager", func() {
	const name = "token-schedule"
	ctx := context.Background()
	key := types.NamespacedName{Name: name}

	It("validates its token again on its schedule, with nothing changed in the cluster", func() {
		api := &lockedTokenAPI{status: cfpkg.TokenActive}
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Data:       map[string][]byte{"CF_API_TOKEN": []byte("token-value")},
		})).To(Succeed())
		DeferCleanup(forceDelete, ctx, &corev1.Secret{}, types.NamespacedName{Name: name, Namespace: "default"})
		Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      "acct-schedule",
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: name, Namespace: "default", Key: "CF_API_TOKEN"},
			},
		})).To(Succeed())
		DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.CloudflareAccount{}, key)
		startManager(ctx, func(mgr manager.Manager) error {
			return (&CloudflareAccountReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
				NewCFClient:        func(string) (CloudflareAccountAPI, error) { return api, nil },
				TokenCheckInterval: time.Second,
			}).SetupWithManager(mgr)
		})
		reason := func() string {
			account := &cloudflarev1alpha1.CloudflareAccount{}
			Expect(k8sClient.Get(ctx, key, account)).To(Succeed())
			if ready := meta.FindStatusCondition(account.Status.Conditions, cloudflarev1alpha1.ConditionReady); ready != nil {
				return ready.Reason
			}
			return ""
		}
		Eventually(reason, "10s", "50ms").Should(Equal("Validated"))
		settled := api.count()
		Eventually(api.count, "5s", "100ms").Should(BeNumerically(">=", settled+2),
			"no validation after the first, with a token check every second")

		By("showing a token disabled in Cloudflare")
		api.set(cfpkg.TokenDisabled)
		Eventually(reason, "5s", "50ms").Should(Equal("TokenDisabled"))

		By("and the same token once it is enabled again")
		api.set(cfpkg.TokenActive)
		Eventually(reason, "5s", "50ms").Should(Equal("Validated"))
	})
})
