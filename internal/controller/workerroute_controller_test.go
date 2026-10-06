/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"
	"fmt"

	cf "github.com/cloudflare/cloudflare-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// fakeWorkerRouteAPI is an in-memory Cloudflare Workers Routes API for one
// zone. It follows the behavior verified against the live API: patterns are
// unique (409 on a duplicate), an unknown route ID is a 404, and an update
// replaces the route, so an empty script clears it.
type fakeWorkerRouteAPI struct {
	routes map[string]cf.WorkerRoute
	nextID int

	// Errors to return instead of performing the call.
	createErr, getErr, listErr, updateErr, deleteErr error

	// zoneIDs records the zone identifier of every call; writes counts the
	// create, update and delete calls.
	zoneIDs []string
	writes  int
	updates []cf.UpdateWorkerRouteParams
}

func newFakeWorkerRouteAPI(existing ...cf.WorkerRoute) *fakeWorkerRouteAPI {
	f := &fakeWorkerRouteAPI{routes: map[string]cf.WorkerRoute{}}
	for _, rt := range existing {
		f.routes[rt.ID] = rt
	}
	return f
}

func routeNotFound() error {
	v := cf.NewNotFoundError(&cf.Error{StatusCode: 404, Type: cf.ErrorTypeNotFound})
	return &v
}

func routeRequestError(status int) error {
	v := cf.NewRequestError(&cf.Error{StatusCode: status, Type: cf.ErrorTypeRequest})
	return &v
}

func (f *fakeWorkerRouteAPI) CreateWorkerRoute(_ context.Context, rc *cf.ResourceContainer, params cf.CreateWorkerRouteParams) (cf.WorkerRouteResponse, error) {
	f.zoneIDs = append(f.zoneIDs, rc.Identifier)
	f.writes++
	if f.createErr != nil {
		return cf.WorkerRouteResponse{}, f.createErr
	}
	for _, rt := range f.routes {
		if rt.Pattern == params.Pattern {
			return cf.WorkerRouteResponse{}, routeRequestError(409)
		}
	}
	f.nextID++
	rt := cf.WorkerRoute{ID: fmt.Sprintf("route-%d", f.nextID), Pattern: params.Pattern, ScriptName: params.Script}
	f.routes[rt.ID] = rt
	return cf.WorkerRouteResponse{WorkerRoute: rt}, nil
}

func (f *fakeWorkerRouteAPI) GetWorkerRoute(_ context.Context, rc *cf.ResourceContainer, routeID string) (cf.WorkerRouteResponse, error) {
	f.zoneIDs = append(f.zoneIDs, rc.Identifier)
	if f.getErr != nil {
		return cf.WorkerRouteResponse{}, f.getErr
	}
	rt, ok := f.routes[routeID]
	if !ok {
		return cf.WorkerRouteResponse{}, routeNotFound()
	}
	return cf.WorkerRouteResponse{WorkerRoute: rt}, nil
}

func (f *fakeWorkerRouteAPI) ListWorkerRoutes(_ context.Context, rc *cf.ResourceContainer, _ cf.ListWorkerRoutesParams) (cf.WorkerRoutesResponse, error) {
	f.zoneIDs = append(f.zoneIDs, rc.Identifier)
	if f.listErr != nil {
		return cf.WorkerRoutesResponse{}, f.listErr
	}
	resp := cf.WorkerRoutesResponse{}
	for _, rt := range f.routes {
		resp.Routes = append(resp.Routes, rt)
	}
	return resp, nil
}

func (f *fakeWorkerRouteAPI) UpdateWorkerRoute(_ context.Context, rc *cf.ResourceContainer, params cf.UpdateWorkerRouteParams) (cf.WorkerRouteResponse, error) {
	f.zoneIDs = append(f.zoneIDs, rc.Identifier)
	f.writes++
	f.updates = append(f.updates, params)
	if f.updateErr != nil {
		return cf.WorkerRouteResponse{}, f.updateErr
	}
	if _, ok := f.routes[params.ID]; !ok {
		return cf.WorkerRouteResponse{}, routeNotFound()
	}
	rt := cf.WorkerRoute{ID: params.ID, Pattern: params.Pattern, ScriptName: params.Script}
	f.routes[rt.ID] = rt
	return cf.WorkerRouteResponse{WorkerRoute: rt}, nil
}

func (f *fakeWorkerRouteAPI) DeleteWorkerRoute(_ context.Context, rc *cf.ResourceContainer, routeID string) (cf.WorkerRouteResponse, error) {
	f.zoneIDs = append(f.zoneIDs, rc.Identifier)
	f.writes++
	if f.deleteErr != nil {
		return cf.WorkerRouteResponse{}, f.deleteErr
	}
	rt, ok := f.routes[routeID]
	if !ok {
		return cf.WorkerRouteResponse{}, routeNotFound()
	}
	delete(f.routes, routeID)
	return cf.WorkerRouteResponse{WorkerRoute: rt}, nil
}

var _ = Describe("WorkerRoute Controller", func() {
	const (
		ns          = "default"
		routeName   = "wr-test-route"
		otherRoute  = "wr-test-route-other"
		zoneCRName  = "wr-test-zone"
		zoneDomain  = "wr-example.com"
		workerCR    = "wr-test-worker"
		workerName  = "wr-cf-worker"
		accountName = "wr-test-account"
		otherAcct   = "wr-other-account"
		secretName  = "wr-test-api-token"
		fakeZoneID  = "zone-wr-123"
		pattern     = "wr-example.com/api/*"
	)

	ctx := context.Background()
	routeKey := types.NamespacedName{Name: routeName, Namespace: ns}

	var fake *fakeWorkerRouteAPI
	var r *WorkerRouteReconciler

	createAccount := func(ready bool) {
		acct := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      "acct-wr",
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: secretName, Namespace: ns, Key: "CF_API_TOKEN"},
			},
		}
		Expect(k8sClient.Create(ctx, acct)).To(Succeed())
		if ready {
			acct.Status.Conditions = []metav1.Condition{{
				Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
				Reason: "Validated", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(ctx, acct)).To(Succeed())
		}
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
			Data:       map[string][]byte{"CF_API_TOKEN": []byte("token")},
		})).To(Succeed())
	}

	// createZone creates the Zone; ready marks it synced with fakeZoneID.
	createZone := func(ready bool) {
		zone := &cloudflarev1alpha1.Zone{
			ObjectMeta: metav1.ObjectMeta{Name: zoneCRName, Namespace: ns},
			Spec: cloudflarev1alpha1.ZoneSpec{
				Name: zoneDomain, AccountRef: corev1.LocalObjectReference{Name: accountName},
			},
		}
		Expect(k8sClient.Create(ctx, zone)).To(Succeed())
		if ready {
			zone.Status.CloudflareMetadata.ZoneID = fakeZoneID
			zone.Status.Conditions = []metav1.Condition{{
				Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
				Reason: "Synced", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(ctx, zone)).To(Succeed())
		}
	}

	// createWorker creates the WorkerScript in account; uploaded marks it
	// ready, as the WorkerScript controller would after an upload.
	createWorker := func(account string, uploaded bool) {
		script := "export default {}"
		ws := &cloudflarev1alpha1.WorkerScript{
			ObjectMeta: metav1.ObjectMeta{Name: workerCR, Namespace: ns},
			Spec: cloudflarev1alpha1.WorkerScriptSpec{
				Name: workerName, AccountRef: corev1.LocalObjectReference{Name: account}, Script: &script,
			},
		}
		Expect(k8sClient.Create(ctx, ws)).To(Succeed())
		if uploaded {
			ws.Status.CloudflareMetadata.ModifiedOn = "2026-10-05T00:00:00.123456Z"
			ws.Status.Conditions = []metav1.Condition{{
				Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
				Reason: "Synced", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(ctx, ws)).To(Succeed())
		}
	}

	// createRoute creates a WorkerRoute that already carries the finalizer.
	// With worker false it is an exclusion route.
	createRoute := func(name, routePattern string, worker bool, annotations map[string]string) {
		route := &cloudflarev1alpha1.WorkerRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: ns, Annotations: annotations,
				Finalizers: []string{reconciler.Finalizer},
			},
			Spec: cloudflarev1alpha1.WorkerRouteSpec{
				ZoneRef: corev1.LocalObjectReference{Name: zoneCRName},
				Pattern: routePattern,
			},
		}
		if worker {
			route.Spec.WorkerScriptRef = &corev1.LocalObjectReference{Name: workerCR}
		}
		Expect(k8sClient.Create(ctx, route)).To(Succeed())
	}

	getRoute := func() *cloudflarev1alpha1.WorkerRoute {
		route := &cloudflarev1alpha1.WorkerRoute{}
		Expect(k8sClient.Get(ctx, routeKey, route)).To(Succeed())
		return route
	}

	readyCondition := func() metav1.Condition {
		for _, c := range getRoute().Status.Conditions {
			if c.Type == cloudflarev1alpha1.ConditionReady {
				return c
			}
		}
		Fail("no Ready condition")
		return metav1.Condition{}
	}

	reconcileRoute := func() (ctrl.Result, error) {
		return r.Reconcile(ctx, reconcile.Request{NamespacedName: routeKey})
	}

	// readyEnv creates everything a route to the Worker needs.
	readyEnv := func() {
		createAccount(true)
		createZone(true)
		createWorker(accountName, true)
	}

	BeforeEach(func() {
		fake = newFakeWorkerRouteAPI()
		r = &WorkerRouteReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			NewWorkerRouteAPI: func(string) (WorkerRouteAPI, error) {
				return fake, nil
			},
		}
	})

	AfterEach(func() {
		forceDelete(ctx, &cloudflarev1alpha1.WorkerRoute{}, routeKey)
		forceDelete(ctx, &cloudflarev1alpha1.WorkerRoute{}, types.NamespacedName{Name: otherRoute, Namespace: ns})
		forceDelete(ctx, &cloudflarev1alpha1.WorkerScript{}, types.NamespacedName{Name: workerCR, Namespace: ns})
		forceDelete(ctx, &cloudflarev1alpha1.Zone{}, types.NamespacedName{Name: zoneCRName, Namespace: ns})
		forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: accountName})
		forceDelete(ctx, &corev1.Secret{}, types.NamespacedName{Name: secretName, Namespace: ns})
	})

	Describe("Reconcile", func() {
		It("returns no error when the WorkerRoute does not exist", func() {
			_, err := reconcileRoute()
			Expect(err).NotTo(HaveOccurred())
		})

		It("adds the finalizer on the first reconcile without calling Cloudflare", func() {
			Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.WorkerRoute{
				ObjectMeta: metav1.ObjectMeta{Name: routeName, Namespace: ns},
				Spec: cloudflarev1alpha1.WorkerRouteSpec{
					ZoneRef: corev1.LocalObjectReference{Name: zoneCRName}, Pattern: pattern,
				},
			})).To(Succeed())
			_, err := reconcileRoute()
			Expect(err).NotTo(HaveOccurred())
			Expect(getRoute().Finalizers).To(ContainElement(reconciler.Finalizer))
			Expect(fake.zoneIDs).To(BeEmpty())
		})

		DescribeTable("waits without calling Cloudflare",
			func(setup func(), reason string) {
				setup()
				result, err := reconcileRoute()
				Expect(err).NotTo(HaveOccurred())
				Expect(result.IsZero()).To(BeTrue(), "a watch re-triggers this reconcile")
				Expect(readyCondition().Status).To(Equal(metav1.ConditionFalse))
				Expect(readyCondition().Reason).To(Equal(reason))
				Expect(fake.zoneIDs).To(BeEmpty())
			},
			Entry("when the Zone is missing", func() {
				createRoute(routeName, pattern, true, nil)
			}, "ZoneNotFound"),
			Entry("when the Zone is not ready", func() {
				createAccount(true)
				createZone(false)
				createRoute(routeName, pattern, true, nil)
			}, "ZoneNotReady"),
			Entry("when the pattern is outside the zone", func() {
				readyEnv()
				createRoute(routeName, "wr-other.com/*", true, nil)
			}, "PatternOutsideZone"),
			Entry("when the WorkerScript is missing", func() {
				createAccount(true)
				createZone(true)
				createRoute(routeName, pattern, true, nil)
			}, "WorkerScriptNotFound"),
			Entry("when the WorkerScript has not been uploaded", func() {
				createAccount(true)
				createZone(true)
				createWorker(accountName, false)
				createRoute(routeName, pattern, true, nil)
			}, "WorkerScriptNotReady"),
			Entry("when the WorkerScript belongs to another account", func() {
				createAccount(true)
				createZone(true)
				createWorker(otherAcct, true)
				createRoute(routeName, pattern, true, nil)
			}, "AccountMismatch"),
		)

		It("retries after a minute when the account is not ready", func() {
			createAccount(false)
			createZone(true)
			createWorker(accountName, true)
			createRoute(routeName, pattern, true, nil)
			result, err := reconcileRoute()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
			Expect(readyCondition().Reason).To(Equal("AccountNotReady"))
		})

		It("reports InvalidToken when no Cloudflare client can be built", func() {
			readyEnv()
			createRoute(routeName, pattern, true, nil)
			r.NewWorkerRouteAPI = func(string) (WorkerRouteAPI, error) { return nil, errors.New("bad token") }
			_, err := reconcileRoute()
			Expect(err).NotTo(HaveOccurred())
			Expect(readyCondition().Reason).To(Equal("InvalidToken"))
		})

		It("creates a route to the Worker, claims it and reports it in status", func() {
			readyEnv()
			createRoute(routeName, pattern, true, nil)
			_, err := reconcileRoute()
			Expect(err).NotTo(HaveOccurred())

			Expect(fake.routes).To(HaveLen(1))
			created := fake.routes["route-1"]
			Expect(created.Pattern).To(Equal(pattern))
			Expect(created.ScriptName).To(Equal(workerName))
			Expect(fake.zoneIDs).To(HaveEach(fakeZoneID))

			route := getRoute()
			Expect(route.Status.CloudflareMetadata).To(Equal(cloudflarev1alpha1.WorkerRouteCloudflareMetadata{
				RouteID: "route-1", ZoneID: fakeZoneID, Script: workerName,
			}))
			Expect(route.Labels).To(HaveKeyWithValue(cloudflarev1alpha1.WorkerRouteIDLabel, "route-1"))
			Expect(readyCondition().Status).To(Equal(metav1.ConditionTrue))
		})

		It("creates an exclusion route without a Worker", func() {
			createAccount(true)
			createZone(true)
			createRoute(routeName, pattern, false, nil)
			_, err := reconcileRoute()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.routes["route-1"].ScriptName).To(BeEmpty())
			Expect(getRoute().Status.CloudflareMetadata.Script).To(BeEmpty())
			Expect(readyCondition().Status).To(Equal(metav1.ConditionTrue))
		})

		It("writes nothing to Cloudflare once the route is in sync", func() {
			readyEnv()
			createRoute(routeName, pattern, true, nil)
			_, err := reconcileRoute()
			Expect(err).NotTo(HaveOccurred())
			writes := fake.writes

			_, err = reconcileRoute()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.writes).To(Equal(writes))
		})

		It("adopts the existing route with the pattern and points it at the Worker", func() {
			readyEnv()
			fake = newFakeWorkerRouteAPI(cf.WorkerRoute{ID: "existing", Pattern: pattern, ScriptName: "someone-elses-worker"})
			createRoute(routeName, pattern, true, nil)
			_, err := reconcileRoute()
			Expect(err).NotTo(HaveOccurred())

			Expect(fake.routes).To(HaveLen(1))
			Expect(fake.routes["existing"].ScriptName).To(Equal(workerName))
			Expect(getRoute().Status.CloudflareMetadata.RouteID).To(Equal("existing"))
		})

		It("reports PatternConflict instead of taking over a route another WorkerRoute manages", func() {
			readyEnv()
			fake = newFakeWorkerRouteAPI(cf.WorkerRoute{ID: "taken", Pattern: pattern, ScriptName: workerName})
			createRoute(otherRoute, pattern, true, nil)
			other := &cloudflarev1alpha1.WorkerRoute{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: otherRoute, Namespace: ns}, other)).To(Succeed())
			other.Labels = map[string]string{cloudflarev1alpha1.WorkerRouteIDLabel: "taken"}
			Expect(k8sClient.Update(ctx, other)).To(Succeed())

			createRoute(routeName, pattern, true, nil)
			result, err := reconcileRoute()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
			Expect(readyCondition().Reason).To(Equal("PatternConflict"))
			Expect(readyCondition().Message).To(ContainSubstring(ns + "/" + otherRoute))
			Expect(fake.writes).To(BeZero())
		})

		It("recreates the route when Cloudflare deleted it with its Worker", func() {
			readyEnv()
			createRoute(routeName, pattern, true, nil)
			_, err := reconcileRoute()
			Expect(err).NotTo(HaveOccurred())
			delete(fake.routes, "route-1")

			_, err = reconcileRoute()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.routes).To(HaveKey("route-2"))
			route := getRoute()
			Expect(route.Status.CloudflareMetadata.RouteID).To(Equal("route-2"))
			Expect(route.Labels).To(HaveKeyWithValue(cloudflarev1alpha1.WorkerRouteIDLabel, "route-2"))
		})

		It("moves the route to a new pattern in place", func() {
			readyEnv()
			createRoute(routeName, pattern, true, nil)
			_, err := reconcileRoute()
			Expect(err).NotTo(HaveOccurred())

			route := getRoute()
			route.Spec.Pattern = "*.wr-example.com/*"
			Expect(k8sClient.Update(ctx, route)).To(Succeed())
			_, err = reconcileRoute()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.routes).To(HaveLen(1))
			Expect(fake.routes["route-1"].Pattern).To(Equal("*.wr-example.com/*"))
		})

		It("stops running the Worker when workerScriptRef is removed", func() {
			readyEnv()
			createRoute(routeName, pattern, true, nil)
			_, err := reconcileRoute()
			Expect(err).NotTo(HaveOccurred())

			route := getRoute()
			route.Spec.WorkerScriptRef = nil
			Expect(k8sClient.Update(ctx, route)).To(Succeed())
			_, err = reconcileRoute()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.routes["route-1"].ScriptName).To(BeEmpty())
			Expect(getRoute().Status.CloudflareMetadata.Script).To(BeEmpty())
		})

		It("sets TerminalError without requeueing when Cloudflare rejects the route", func() {
			readyEnv()
			createRoute(routeName, pattern, true, nil)
			fake.createErr = routeRequestError(400)
			result, err := reconcileRoute()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(readyCondition().Reason).To(Equal("TerminalError"))
		})

		It("returns retryable errors for back-off", func() {
			readyEnv()
			createRoute(routeName, pattern, true, nil)
			fake.listErr = errors.New("connection reset")
			_, err := reconcileRoute()
			Expect(err).To(MatchError("connection reset"))
			Expect(readyCondition().Reason).To(Equal("APIError"))
		})

		It("reports errors from reading the known route and from updating it", func() {
			readyEnv()
			createRoute(routeName, pattern, true, nil)
			_, err := reconcileRoute()
			Expect(err).NotTo(HaveOccurred())

			fake.getErr = errors.New("timeout")
			_, err = reconcileRoute()
			Expect(err).To(MatchError("timeout"))

			fake.getErr = nil
			fake.routes["route-1"] = cf.WorkerRoute{ID: "route-1", Pattern: pattern, ScriptName: "changed-outside"}
			fake.updateErr = errors.New("unavailable")
			_, err = reconcileRoute()
			Expect(err).To(MatchError("unavailable"))
			Expect(readyCondition().Reason).To(Equal("APIError"))
		})
	})

	Describe("deletion", func() {
		// syncedRoute creates a route that the controller has synced.
		syncedRoute := func(annotations map[string]string) {
			readyEnv()
			createRoute(routeName, pattern, true, annotations)
			_, err := reconcileRoute()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.routes).To(HaveLen(1))
			Expect(k8sClient.Delete(ctx, getRoute())).To(Succeed())
		}
		expectGone := func() {
			err := k8sClient.Get(ctx, routeKey, &cloudflarev1alpha1.WorkerRoute{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}

		It("deletes the route from Cloudflare and removes the finalizer", func() {
			syncedRoute(nil)
			_, err := reconcileRoute()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.routes).To(BeEmpty())
			expectGone()
		})

		It("keeps the route in Cloudflare with the retain policy", func() {
			syncedRoute(map[string]string{reconciler.DeletionPolicyAnnotation: reconciler.DeletionPolicyRetain})
			_, err := reconcileRoute()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.routes).To(HaveLen(1))
			expectGone()
		})

		It("finishes when the route is already gone from Cloudflare", func() {
			syncedRoute(nil)
			delete(fake.routes, "route-1")
			_, err := reconcileRoute()
			Expect(err).NotTo(HaveOccurred())
			expectGone()
		})

		It("skips Cloudflare when the Zone resource is already gone", func() {
			syncedRoute(nil)
			forceDelete(ctx, &cloudflarev1alpha1.Zone{}, types.NamespacedName{Name: zoneCRName, Namespace: ns})
			writes := fake.writes
			_, err := reconcileRoute()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.writes).To(Equal(writes))
			expectGone()
		})

		It("keeps the finalizer when Cloudflare fails, so deletion is retried", func() {
			syncedRoute(nil)
			fake.deleteErr = errors.New("unavailable")
			_, err := reconcileRoute()
			Expect(err).To(MatchError("unavailable"))
			Expect(getRoute().Finalizers).To(ContainElement(reconciler.Finalizer))
		})

		It("keeps the finalizer when the credentials cannot be resolved", func() {
			syncedRoute(nil)
			forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: accountName})
			_, err := reconcileRoute()
			Expect(err).To(HaveOccurred())
			Expect(getRoute().Finalizers).To(ContainElement(reconciler.Finalizer))
		})

		It("does nothing for a route without the finalizer", func() {
			result, err := r.reconcileDelete(ctx, &cloudflarev1alpha1.WorkerRoute{})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
		})
	})

	Describe("Validation", func() {
		BeforeEach(func() { createRoute(routeName, pattern, true, nil) })

		It("rejects changing spec.zoneRef", func() {
			route := getRoute()
			route.Spec.ZoneRef.Name = "other-zone"
			Expect(k8sClient.Update(ctx, route)).To(MatchError(ContainSubstring("zoneRef is immutable")))
		})

		DescribeTable("checks the pattern format",
			func(p string, valid bool) {
				route := getRoute()
				route.Spec.Pattern = p
				err := k8sClient.Update(ctx, route)
				if valid {
					Expect(err).NotTo(HaveOccurred())
				} else {
					Expect(err).To(MatchError(ContainSubstring("spec.pattern")))
				}
			},
			Entry("host and path wildcard", "wr-example.com/*", true),
			Entry("subdomain wildcard", "*.wr-example.com/*", true),
			Entry("wildcard including the apex", "*wr-example.com/*", true),
			Entry("host only", "api.wr-example.com", true),
			Entry("a scheme", "https://wr-example.com/*", false),
			Entry("a port", "wr-example.com:8080/*", false),
			Entry("whitespace", "wr-example.com/a b", false),
		)
	})
})
