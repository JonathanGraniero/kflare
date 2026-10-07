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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// fakeKVNamespaceAPI is an in-memory Workers KV namespaces API for one
// account. It follows the behavior verified against the live API: titles are
// unique in an account (a duplicate is a 400), a rename keeps the ID, and an
// unknown namespace ID is a 404. Like cloudflare-go, it rejects a resource
// container that is not account-level.
type fakeKVNamespaceAPI struct {
	namespaces map[string]cf.WorkersKVNamespace
	nextID     int

	createErr, listErr, updateErr, deleteErr error

	accountIDs []string
	writes     int
}

func newFakeKVNamespaceAPI(existing ...cf.WorkersKVNamespace) *fakeKVNamespaceAPI {
	f := &fakeKVNamespaceAPI{namespaces: map[string]cf.WorkersKVNamespace{}}
	for _, ns := range existing {
		f.namespaces[ns.ID] = ns
	}
	return f
}

func kvNotFound() error {
	v := cf.NewNotFoundError(&cf.Error{StatusCode: 404, Type: cf.ErrorTypeNotFound})
	return &v
}

func kvRequestError() error {
	v := cf.NewRequestError(&cf.Error{StatusCode: 400, Type: cf.ErrorTypeRequest})
	return &v
}

func (f *fakeKVNamespaceAPI) record(rc *cf.ResourceContainer) error {
	f.accountIDs = append(f.accountIDs, rc.Identifier)
	if rc.Level != cf.AccountRouteLevel {
		return cf.ErrRequiredAccountLevelResourceContainer
	}
	return nil
}

func (f *fakeKVNamespaceAPI) titleTaken(title string) bool {
	for _, ns := range f.namespaces {
		if ns.Title == title {
			return true
		}
	}
	return false
}

func (f *fakeKVNamespaceAPI) CreateWorkersKVNamespace(_ context.Context, rc *cf.ResourceContainer, params cf.CreateWorkersKVNamespaceParams) (cf.WorkersKVNamespaceResponse, error) {
	if err := f.record(rc); err != nil {
		return cf.WorkersKVNamespaceResponse{}, err
	}
	f.writes++
	if f.createErr != nil {
		return cf.WorkersKVNamespaceResponse{}, f.createErr
	}
	if f.titleTaken(params.Title) {
		return cf.WorkersKVNamespaceResponse{}, kvRequestError()
	}
	f.nextID++
	ns := cf.WorkersKVNamespace{ID: fmt.Sprintf("kv-%d", f.nextID), Title: params.Title}
	f.namespaces[ns.ID] = ns
	return cf.WorkersKVNamespaceResponse{Result: ns}, nil
}

func (f *fakeKVNamespaceAPI) ListWorkersKVNamespaces(_ context.Context, rc *cf.ResourceContainer, _ cf.ListWorkersKVNamespacesParams) ([]cf.WorkersKVNamespace, *cf.ResultInfo, error) {
	if err := f.record(rc); err != nil {
		return nil, &cf.ResultInfo{}, err
	}
	if f.listErr != nil {
		return nil, &cf.ResultInfo{}, f.listErr
	}
	var out []cf.WorkersKVNamespace
	for _, ns := range f.namespaces {
		out = append(out, ns)
	}
	return out, &cf.ResultInfo{}, nil
}

func (f *fakeKVNamespaceAPI) UpdateWorkersKVNamespace(_ context.Context, rc *cf.ResourceContainer, params cf.UpdateWorkersKVNamespaceParams) (cf.Response, error) {
	if err := f.record(rc); err != nil {
		return cf.Response{}, err
	}
	f.writes++
	if f.updateErr != nil {
		return cf.Response{}, f.updateErr
	}
	ns, ok := f.namespaces[params.NamespaceID]
	if !ok {
		return cf.Response{}, kvNotFound()
	}
	if ns.Title != params.Title && f.titleTaken(params.Title) {
		return cf.Response{}, kvRequestError()
	}
	ns.Title = params.Title
	f.namespaces[ns.ID] = ns
	return cf.Response{Success: true}, nil
}

func (f *fakeKVNamespaceAPI) DeleteWorkersKVNamespace(_ context.Context, rc *cf.ResourceContainer, namespaceID string) (cf.Response, error) {
	if err := f.record(rc); err != nil {
		return cf.Response{}, err
	}
	f.writes++
	if f.deleteErr != nil {
		return cf.Response{}, f.deleteErr
	}
	if _, ok := f.namespaces[namespaceID]; !ok {
		return cf.Response{}, kvNotFound()
	}
	delete(f.namespaces, namespaceID)
	return cf.Response{Success: true}, nil
}

var _ = Describe("KVNamespace Controller", func() {
	const (
		ns          = "default"
		kvName      = "kv-test-namespace"
		otherKV     = "kv-test-namespace-other"
		accountName = "kv-test-account"
		secretName  = "kv-test-api-token"
		fakeAcctID  = "acct-kv-123"
		title       = "kv-test-title"
	)

	ctx := context.Background()
	kvKey := types.NamespacedName{Name: kvName, Namespace: ns}

	var fake *fakeKVNamespaceAPI
	var r *KVNamespaceReconciler

	createAccount := func(ready bool) {
		acct := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      fakeAcctID,
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

	// createKV creates a KVNamespace that already carries the finalizer.
	createKV := func(name, kvTitle string, annotations map[string]string) {
		Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.KVNamespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: ns, Annotations: annotations,
				Finalizers: []string{reconciler.Finalizer},
			},
			Spec: cloudflarev1alpha1.KVNamespaceSpec{
				AccountRef: corev1.LocalObjectReference{Name: accountName}, Title: kvTitle,
			},
		})).To(Succeed())
	}

	getKV := func() *cloudflarev1alpha1.KVNamespace {
		kv := &cloudflarev1alpha1.KVNamespace{}
		Expect(k8sClient.Get(ctx, kvKey, kv)).To(Succeed())
		return kv
	}

	readyCondition := func() metav1.Condition {
		cond := meta.FindStatusCondition(getKV().Status.Conditions, cloudflarev1alpha1.ConditionReady)
		Expect(cond).NotTo(BeNil())
		return *cond
	}

	reconcileKV := func() (ctrl.Result, error) {
		return r.Reconcile(ctx, reconcile.Request{NamespacedName: kvKey})
	}

	BeforeEach(func() {
		fake = newFakeKVNamespaceAPI()
		r = &KVNamespaceReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			NewKVNamespaceAPI: func(string) (KVNamespaceAPI, error) {
				return fake, nil
			},
		}
	})

	AfterEach(func() {
		forceDelete(ctx, &cloudflarev1alpha1.KVNamespace{}, kvKey)
		forceDelete(ctx, &cloudflarev1alpha1.KVNamespace{}, types.NamespacedName{Name: otherKV, Namespace: ns})
		forceDelete(ctx, &cloudflarev1alpha1.WorkerScript{}, types.NamespacedName{Name: "kv-test-worker", Namespace: ns})
		forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: accountName})
		forceDelete(ctx, &corev1.Secret{}, types.NamespacedName{Name: secretName, Namespace: ns})
	})

	Describe("Reconcile", func() {
		It("returns no error when the KVNamespace does not exist", func() {
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
		})

		It("adds the finalizer on the first reconcile without calling Cloudflare", func() {
			Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.KVNamespace{
				ObjectMeta: metav1.ObjectMeta{Name: kvName, Namespace: ns},
				Spec: cloudflarev1alpha1.KVNamespaceSpec{
					AccountRef: corev1.LocalObjectReference{Name: accountName}, Title: title,
				},
			})).To(Succeed())
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(getKV().Finalizers).To(ContainElement(reconciler.Finalizer))
			Expect(fake.accountIDs).To(BeEmpty())
		})

		It("retries after a minute when the account is not ready", func() {
			createAccount(false)
			createKV(kvName, title, nil)
			result, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
			Expect(readyCondition().Reason).To(Equal("AccountNotReady"))
		})

		It("reports InvalidToken when no Cloudflare client can be built", func() {
			createAccount(true)
			createKV(kvName, title, nil)
			r.NewKVNamespaceAPI = func(string) (KVNamespaceAPI, error) { return nil, errors.New("bad token") }
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(readyCondition().Reason).To(Equal("InvalidToken"))
		})

		It("creates the namespace, claims it and reports its ID", func() {
			createAccount(true)
			createKV(kvName, title, nil)
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())

			Expect(fake.namespaces).To(HaveKeyWithValue("kv-1", cf.WorkersKVNamespace{ID: "kv-1", Title: title}))
			Expect(fake.accountIDs).To(HaveEach(fakeAcctID))
			kv := getKV()
			Expect(kv.Status.CloudflareMetadata.NamespaceID).To(Equal("kv-1"))
			Expect(kv.Labels).To(HaveKeyWithValue(cloudflarev1alpha1.KVNamespaceIDLabel, "kv-1"))
			Expect(readyCondition().Status).To(Equal(metav1.ConditionTrue))
		})

		It("writes nothing to Cloudflare once the namespace is in sync", func() {
			createAccount(true)
			createKV(kvName, title, nil)
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			writes := fake.writes

			_, err = reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.writes).To(Equal(writes))
		})

		It("adopts the existing namespace with its title", func() {
			createAccount(true)
			fake = newFakeKVNamespaceAPI(cf.WorkersKVNamespace{ID: "existing", Title: title})
			createKV(kvName, title, nil)
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.namespaces).To(HaveLen(1))
			Expect(getKV().Status.CloudflareMetadata.NamespaceID).To(Equal("existing"))
		})

		It("reports TitleConflict instead of taking over a namespace another KVNamespace manages", func() {
			createAccount(true)
			fake = newFakeKVNamespaceAPI(cf.WorkersKVNamespace{ID: "taken", Title: title})
			createKV(otherKV, title, nil)
			other := &cloudflarev1alpha1.KVNamespace{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: otherKV, Namespace: ns}, other)).To(Succeed())
			other.Labels = map[string]string{cloudflarev1alpha1.KVNamespaceIDLabel: "taken"}
			Expect(k8sClient.Update(ctx, other)).To(Succeed())

			createKV(kvName, title, nil)
			result, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
			Expect(readyCondition().Reason).To(Equal("TitleConflict"))
			Expect(readyCondition().Message).To(ContainSubstring(ns + "/" + otherKV))
			Expect(fake.writes).To(BeZero())
		})

		It("recreates the namespace when it was deleted outside kflare", func() {
			createAccount(true)
			createKV(kvName, title, nil)
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			delete(fake.namespaces, "kv-1")

			_, err = reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			kv := getKV()
			Expect(kv.Status.CloudflareMetadata.NamespaceID).To(Equal("kv-2"))
			Expect(kv.Labels).To(HaveKeyWithValue(cloudflarev1alpha1.KVNamespaceIDLabel, "kv-2"))
		})

		It("renames the namespace in place when spec.title changes", func() {
			createAccount(true)
			createKV(kvName, title, nil)
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())

			kv := getKV()
			kv.Spec.Title = "kv-test-renamed"
			Expect(k8sClient.Update(ctx, kv)).To(Succeed())
			_, err = reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.namespaces).To(HaveKeyWithValue("kv-1", cf.WorkersKVNamespace{ID: "kv-1", Title: "kv-test-renamed"}))
			Expect(getKV().Status.CloudflareMetadata.NamespaceID).To(Equal("kv-1"))
		})

		It("sets TerminalError when the new title is taken by another namespace", func() {
			createAccount(true)
			createKV(kvName, title, nil)
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			fake.namespaces["other"] = cf.WorkersKVNamespace{ID: "other", Title: "kv-taken-title"}

			kv := getKV()
			kv.Spec.Title = "kv-taken-title"
			Expect(k8sClient.Update(ctx, kv)).To(Succeed())
			result, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(readyCondition().Reason).To(Equal("TerminalError"))
		})

		It("returns retryable errors for back-off", func() {
			createAccount(true)
			createKV(kvName, title, nil)
			fake.listErr = errors.New("connection reset")
			_, err := reconcileKV()
			Expect(err).To(MatchError("connection reset"))
			Expect(readyCondition().Reason).To(Equal("APIError"))

			fake.listErr = nil
			fake.createErr = errors.New("unavailable")
			_, err = reconcileKV()
			Expect(err).To(MatchError("unavailable"))
		})
	})

	Describe("deletion", func() {
		syncedKV := func(annotations map[string]string) {
			createAccount(true)
			createKV(kvName, title, annotations)
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.namespaces).To(HaveLen(1))
		}
		startDeletion := func() {
			Expect(k8sClient.Delete(ctx, getKV())).To(Succeed())
		}
		expectGone := func() {
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, kvKey, &cloudflarev1alpha1.KVNamespace{}))).To(BeTrue())
		}

		It("deletes the namespace from Cloudflare and removes the finalizer", func() {
			syncedKV(nil)
			startDeletion()
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.namespaces).To(BeEmpty())
			expectGone()
		})

		It("keeps the namespace and its data with the retain policy", func() {
			syncedKV(map[string]string{reconciler.DeletionPolicyAnnotation: reconciler.DeletionPolicyRetain})
			startDeletion()
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.namespaces).To(HaveLen(1))
			expectGone()
		})

		It("finishes when the namespace is already gone from Cloudflare", func() {
			syncedKV(nil)
			delete(fake.namespaces, "kv-1")
			startDeletion()
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			expectGone()
		})

		It("waits for the WorkerScripts bound to it, then deletes", func() {
			syncedKV(nil)
			script := "export default {}"
			Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.WorkerScript{
				ObjectMeta: metav1.ObjectMeta{Name: "kv-test-worker", Namespace: ns, Finalizers: []string{reconciler.Finalizer}},
				Spec: cloudflarev1alpha1.WorkerScriptSpec{
					Name: "kv-test-script", AccountRef: corev1.LocalObjectReference{Name: accountName}, Script: &script,
					Bindings: []cloudflarev1alpha1.WorkerBinding{
						{Name: "KV", KVNamespaceRef: &corev1.LocalObjectReference{Name: kvName}},
					},
				},
			})).To(Succeed())
			startDeletion()

			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(readyCondition().Reason).To(Equal("InUse"))
			Expect(readyCondition().Message).To(ContainSubstring("WorkerScript default/kv-test-worker"))
			Expect(fake.namespaces).To(HaveLen(1))

			forceDelete(ctx, &cloudflarev1alpha1.WorkerScript{}, types.NamespacedName{Name: "kv-test-worker", Namespace: ns})
			_, err = reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.namespaces).To(BeEmpty())
			expectGone()
		})

		It("keeps the finalizer when Cloudflare fails, so deletion is retried", func() {
			syncedKV(nil)
			fake.deleteErr = errors.New("unavailable")
			startDeletion()
			_, err := reconcileKV()
			Expect(err).To(MatchError("unavailable"))
			Expect(getKV().Finalizers).To(ContainElement(reconciler.Finalizer))
		})

		It("keeps the finalizer when the credentials cannot be resolved", func() {
			syncedKV(nil)
			forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: accountName})
			startDeletion()
			_, err := reconcileKV()
			Expect(err).To(HaveOccurred())
			Expect(getKV().Finalizers).To(ContainElement(reconciler.Finalizer))
		})

		It("returns the error when no Cloudflare client can be built", func() {
			syncedKV(nil)
			r.NewKVNamespaceAPI = func(string) (KVNamespaceAPI, error) { return nil, errors.New("bad token") }
			startDeletion()
			_, err := reconcileKV()
			Expect(err).To(MatchError("bad token"))
		})

		It("does nothing for a namespace without the finalizer", func() {
			result, err := r.reconcileDelete(ctx, &cloudflarev1alpha1.KVNamespace{})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
		})
	})

	Describe("Validation", func() {
		BeforeEach(func() { createKV(kvName, title, nil) })

		It("rejects changing spec.accountRef", func() {
			kv := getKV()
			kv.Spec.AccountRef.Name = "other-account"
			Expect(k8sClient.Update(ctx, kv)).To(MatchError(ContainSubstring("accountRef is immutable")))
		})

		It("rejects an empty title", func() {
			kv := getKV()
			kv.Spec.Title = ""
			Expect(k8sClient.Update(ctx, kv)).To(MatchError(ContainSubstring("spec.title")))
		})
	})
})
