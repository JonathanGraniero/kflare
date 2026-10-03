/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	cf "github.com/cloudflare/cloudflare-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// fakeWorkerScriptAPI is a test double for WorkerScriptAPI. Like
// cloudflare-go, every method rejects a resource container that is not
// account-level.
type fakeWorkerScriptAPI struct {
	uploadResp cf.WorkerScriptResponse
	uploadErr  error
	uploads    []cf.CreateWorkerParams

	workers []cf.WorkerMetaData
	listErr error

	deleteErr error
	deletes   []string

	// accountIDs records the resource container identifier of every call.
	accountIDs []string
	// calls records the method names in call order.
	calls []string
}

func (f *fakeWorkerScriptAPI) record(rc *cf.ResourceContainer, method string) error {
	f.accountIDs = append(f.accountIDs, rc.Identifier)
	f.calls = append(f.calls, method)
	if rc.Level != cf.AccountRouteLevel {
		return cf.ErrRequiredAccountLevelResourceContainer
	}
	return nil
}

func (f *fakeWorkerScriptAPI) UploadWorker(_ context.Context, rc *cf.ResourceContainer, params cf.CreateWorkerParams) (cf.WorkerScriptResponse, error) {
	if err := f.record(rc, "UploadWorker"); err != nil {
		return cf.WorkerScriptResponse{}, err
	}
	f.uploads = append(f.uploads, params)
	return f.uploadResp, f.uploadErr
}

func (f *fakeWorkerScriptAPI) ListWorkers(_ context.Context, rc *cf.ResourceContainer, _ cf.ListWorkersParams) (cf.WorkerListResponse, *cf.ResultInfo, error) {
	if err := f.record(rc, "ListWorkers"); err != nil {
		return cf.WorkerListResponse{}, &cf.ResultInfo{}, err
	}
	return cf.WorkerListResponse{WorkerList: f.workers}, &cf.ResultInfo{}, f.listErr
}

func (f *fakeWorkerScriptAPI) DeleteWorker(_ context.Context, rc *cf.ResourceContainer, params cf.DeleteWorkerParams) error {
	if err := f.record(rc, "DeleteWorker"); err != nil {
		return err
	}
	f.deletes = append(f.deletes, params.ScriptName)
	return f.deleteErr
}

// reconcilerWithFakeWorkerScriptAPI returns a WorkerScriptReconciler wired to a pre-built fake.
func reconcilerWithFakeWorkerScriptAPI(fake WorkerScriptAPI) *WorkerScriptReconciler {
	return &WorkerScriptReconciler{
		Client: k8sClient,
		Scheme: k8sClient.Scheme(),
		NewWorkerScriptAPI: func(_ string) (WorkerScriptAPI, error) {
			return fake, nil
		},
	}
}

var _ = Describe("WorkerScript Controller", func() {
	const (
		ns             = "default"
		wsName         = "ws-test-worker"
		scriptName     = "ws-test-script"
		accountName    = "ws-test-account"
		apiSecretName  = "ws-test-api-token"
		bindingSecret  = "ws-test-binding-secret"
		codeConfigMap  = "ws-test-code"
		fakeAcctID     = "acct-ws-123"
		inlineCode     = "export default { fetch() { return new Response('hi'); } };"
		deletionPolicy = "cloudflare.k8s.io/deletion-policy"
	)

	ctx := context.Background()
	wsKey := types.NamespacedName{Name: wsName, Namespace: ns}
	uploadedAt := time.Date(2026, 10, 3, 2, 13, 10, 984591000, time.UTC)
	uploadedAtStr := "2026-10-03T02:13:10.984591Z"

	notFoundErr := func() error {
		v := cf.NewNotFoundError(&cf.Error{StatusCode: 404, Type: cf.ErrorTypeNotFound})
		return &v
	}
	terminalErr := func() error {
		v := cf.NewAuthorizationError(&cf.Error{StatusCode: 403, Type: cf.ErrorTypeAuthorization})
		return &v
	}
	uploadResp := func(etag string, modified time.Time) cf.WorkerScriptResponse {
		return cf.WorkerScriptResponse{WorkerScript: cf.WorkerScript{
			WorkerMetaData: cf.WorkerMetaData{ID: scriptName, ETAG: etag, ModifiedOn: modified},
		}}
	}

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

	createSecret := func(name string, data map[string][]byte) {
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Data:       data,
		})).To(Succeed())
	}

	// baseSpec is an inline module Worker with one binding of each kind.
	baseSpec := func() cloudflarev1alpha1.WorkerScriptSpec {
		return cloudflarev1alpha1.WorkerScriptSpec{
			Name:               scriptName,
			AccountRef:         corev1.LocalObjectReference{Name: accountName},
			Script:             cf.StringPtr(inlineCode),
			CompatibilityDate:  "2024-09-23",
			CompatibilityFlags: []string{"nodejs_compat"},
			Bindings: []cloudflarev1alpha1.WorkerBinding{
				{Name: "GREETING", PlainText: cf.StringPtr("hello")},
				{Name: "API_KEY", SecretKeyRef: &cloudflarev1alpha1.WorkerKeyReference{Name: bindingSecret, Key: "api-key"}},
				{Name: "CACHE", KVNamespaceID: cf.StringPtr("kv-123")},
				{Name: "BUCKET", R2BucketName: cf.StringPtr("assets")},
			},
		}
	}

	createWorker := func(spec cloudflarev1alpha1.WorkerScriptSpec, annotations map[string]string, withFinalizer bool) {
		ws := &cloudflarev1alpha1.WorkerScript{
			ObjectMeta: metav1.ObjectMeta{Name: wsName, Namespace: ns, Annotations: annotations},
			Spec:       spec,
		}
		if withFinalizer {
			ws.Finalizers = []string{reconciler.Finalizer}
		}
		Expect(k8sClient.Create(ctx, ws)).To(Succeed())
	}

	// readyEnv creates a ready account, its API token, the binding Secret and the Worker.
	readyEnv := func(annotations map[string]string) {
		createAccount(true)
		createSecret(apiSecretName, map[string][]byte{"CF_API_TOKEN": []byte("api-token")})
		createSecret(bindingSecret, map[string][]byte{"api-key": []byte("s3cr3t-value")})
		createWorker(baseSpec(), annotations, true)
	}

	getWorker := func() *cloudflarev1alpha1.WorkerScript {
		ws := &cloudflarev1alpha1.WorkerScript{}
		Expect(k8sClient.Get(ctx, wsKey, ws)).To(Succeed())
		return ws
	}

	setUploadedStatus := func(modifiedOn string) {
		ws := getWorker()
		ws.Status.CloudflareMetadata.ModifiedOn = modifiedOn
		Expect(k8sClient.Status().Update(ctx, ws)).To(Succeed())
	}

	expectReady := func(status metav1.ConditionStatus, reason string) {
		ws := getWorker()
		var cond *metav1.Condition
		for i := range ws.Status.Conditions {
			if ws.Status.Conditions[i].Type == cloudflarev1alpha1.ConditionReady {
				cond = &ws.Status.Conditions[i]
			}
		}
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(status))
		Expect(cond.Reason).To(Equal(reason))
	}

	reconcileWorker := func(r *WorkerScriptReconciler) (reconcile.Result, error) {
		return r.Reconcile(ctx, reconcile.Request{NamespacedName: wsKey})
	}

	// syncedFake returns a fake whose upload reports uploadedAt and whose list
	// reports the same Worker, as Cloudflare does right after an upload.
	syncedFake := func() *fakeWorkerScriptAPI {
		return &fakeWorkerScriptAPI{
			uploadResp: uploadResp("etag-1", uploadedAt),
			workers:    []cf.WorkerMetaData{{ID: scriptName, ETAG: "etag-1", ModifiedOn: uploadedAt}},
		}
	}

	startDeletion := func() {
		Expect(k8sClient.Delete(ctx, getWorker())).To(Succeed())
		Expect(getWorker().DeletionTimestamp).NotTo(BeNil())
	}

	expectWorkerGone := func() {
		err := k8sClient.Get(ctx, wsKey, &cloudflarev1alpha1.WorkerScript{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "expected WorkerScript to be deleted, got %v", err)
	}

	AfterEach(func() {
		ws := &cloudflarev1alpha1.WorkerScript{}
		if err := k8sClient.Get(ctx, wsKey, ws); err == nil {
			ws.Finalizers = nil
			_ = k8sClient.Update(ctx, ws)
			_ = k8sClient.Delete(ctx, ws)
		}
		acct := &cloudflarev1alpha1.CloudflareAccount{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: accountName}, acct); err == nil {
			Expect(k8sClient.Delete(ctx, acct)).To(Succeed())
		}
		for _, name := range []string{apiSecretName, bindingSecret} {
			secret := &corev1.Secret{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, secret); err == nil {
				Expect(k8sClient.Delete(ctx, secret)).To(Succeed())
			}
		}
		cm := &corev1.ConfigMap{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: codeConfigMap, Namespace: ns}, cm); err == nil {
			Expect(k8sClient.Delete(ctx, cm)).To(Succeed())
		}
	})

	Describe("Reconcile", func() {
		It("returns no error when the WorkerScript does not exist", func() {
			_, err := reconcileWorker(reconcilerWithFakeWorkerScriptAPI(&fakeWorkerScriptAPI{}))
			Expect(err).NotTo(HaveOccurred())
		})

		It("adds the finalizer on first reconcile and returns without calling CF", func() {
			createWorker(baseSpec(), nil, false)
			fake := &fakeWorkerScriptAPI{}
			_, err := reconcileWorker(reconcilerWithFakeWorkerScriptAPI(fake))
			Expect(err).NotTo(HaveOccurred())
			Expect(getWorker().Finalizers).To(ContainElement(reconciler.Finalizer))
			Expect(fake.calls).To(BeEmpty())
		})

		DescribeTable("sets Ready=False and retries later when credentials cannot be resolved",
			func(setup func(), reason string) {
				setup()
				createWorker(baseSpec(), nil, true)
				fake := &fakeWorkerScriptAPI{}
				result, err := reconcileWorker(reconcilerWithFakeWorkerScriptAPI(fake))
				Expect(err).NotTo(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
				Expect(fake.calls).To(BeEmpty())
				expectReady(metav1.ConditionFalse, reason)
			},
			Entry("account missing", func() {}, "AccountNotFound"),
			Entry("account not ready", func() { createAccount(false) }, "AccountNotReady"),
			Entry("API token Secret missing", func() { createAccount(true) }, "SecretNotFound"),
			Entry("token key missing", func() {
				createAccount(true)
				createSecret(apiSecretName, map[string][]byte{"WRONG_KEY": []byte("x")})
			}, "TokenKeyMissing"),
		)

		It("sets Ready=False InvalidToken when the CF client cannot be constructed", func() {
			readyEnv(nil)
			r := &WorkerScriptReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				NewWorkerScriptAPI: func(_ string) (WorkerScriptAPI, error) {
					return nil, errors.New("bad token")
				},
			}
			_, err := reconcileWorker(r)
			Expect(err).NotTo(HaveOccurred())
			expectReady(metav1.ConditionFalse, "InvalidToken")
		})

		It("uploads the script with every binding and records the upload", func() {
			readyEnv(nil)
			fake := syncedFake()
			result, err := reconcileWorker(reconcilerWithFakeWorkerScriptAPI(fake))
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(reconcile.Result{}))

			// A new desired state uploads straight away without listing first.
			Expect(fake.calls).To(Equal([]string{"UploadWorker"}))
			Expect(fake.accountIDs).To(HaveEach(fakeAcctID))
			Expect(fake.uploads).To(HaveLen(1))
			Expect(fake.uploads[0]).To(Equal(cf.CreateWorkerParams{
				ScriptName:         scriptName,
				Script:             inlineCode,
				Module:             true,
				CompatibilityDate:  "2024-09-23",
				CompatibilityFlags: []string{"nodejs_compat"},
				Bindings: map[string]cf.WorkerBinding{
					"GREETING": cf.WorkerPlainTextBinding{Text: "hello"},
					"API_KEY":  cf.WorkerSecretTextBinding{Text: "s3cr3t-value"},
					"CACHE":    cf.WorkerKvNamespaceBinding{NamespaceID: "kv-123"},
					"BUCKET":   cf.WorkerR2BucketBinding{BucketName: "assets"},
				},
			}))

			ws := getWorker()
			Expect(ws.Spec.Format).To(Equal("module"), "API server should default format")
			Expect(ws.Status.CloudflareMetadata.Etag).To(Equal("etag-1"))
			Expect(ws.Status.CloudflareMetadata.ModifiedOn).To(Equal(uploadedAtStr))
			Expect(ws.Status.AppliedHash).To(HaveLen(64))
			expectReady(metav1.ConditionTrue, "Synced")

			statusJSON, marshalErr := json.Marshal(ws.Status)
			Expect(marshalErr).NotTo(HaveOccurred())
			Expect(string(statusJSON)).NotTo(ContainSubstring("s3cr3t-value"))
		})

		It("reads the script from a ConfigMap and uploads service-worker syntax", func() {
			createAccount(true)
			createSecret(apiSecretName, map[string][]byte{"CF_API_TOKEN": []byte("api-token")})
			Expect(k8sClient.Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: codeConfigMap, Namespace: ns},
				Data:       map[string]string{"worker.js": "addEventListener('fetch', () => {});"},
			})).To(Succeed())
			spec := baseSpec()
			spec.Script = nil
			spec.ScriptConfigMapRef = &cloudflarev1alpha1.WorkerKeyReference{Name: codeConfigMap, Key: "worker.js"}
			spec.Format = cloudflarev1alpha1.WorkerFormatServiceWorker
			spec.Bindings = nil
			createWorker(spec, nil, true)

			fake := syncedFake()
			_, err := reconcileWorker(reconcilerWithFakeWorkerScriptAPI(fake))
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.uploads).To(HaveLen(1))
			Expect(fake.uploads[0].Script).To(Equal("addEventListener('fetch', () => {});"))
			Expect(fake.uploads[0].Module).To(BeFalse())
			Expect(fake.uploads[0].Bindings).To(BeEmpty())
		})

		DescribeTable("reports a source it cannot resolve without calling CF",
			func(setup func() cloudflarev1alpha1.WorkerScriptSpec, reason string) {
				createAccount(true)
				createSecret(apiSecretName, map[string][]byte{"CF_API_TOKEN": []byte("api-token")})
				createWorker(setup(), nil, true)

				fake := &fakeWorkerScriptAPI{}
				result, err := reconcileWorker(reconcilerWithFakeWorkerScriptAPI(fake))
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(reconcile.Result{}))
				Expect(fake.calls).To(BeEmpty())
				expectReady(metav1.ConditionFalse, reason)
			},
			Entry("ConfigMap missing", func() cloudflarev1alpha1.WorkerScriptSpec {
				spec := baseSpec()
				spec.Script = nil
				spec.ScriptConfigMapRef = &cloudflarev1alpha1.WorkerKeyReference{Name: codeConfigMap, Key: "worker.js"}
				return spec
			}, "ScriptConfigMapNotFound"),
			Entry("ConfigMap key missing", func() cloudflarev1alpha1.WorkerScriptSpec {
				Expect(k8sClient.Create(ctx, &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{Name: codeConfigMap, Namespace: ns},
					Data:       map[string]string{"other.js": "x"},
				})).To(Succeed())
				spec := baseSpec()
				spec.Script = nil
				spec.ScriptConfigMapRef = &cloudflarev1alpha1.WorkerKeyReference{Name: codeConfigMap, Key: "worker.js"}
				return spec
			}, "ScriptKeyMissing"),
			Entry("binding Secret missing", baseSpec, "BindingSecretNotFound"),
			Entry("binding Secret key empty", func() cloudflarev1alpha1.WorkerScriptSpec {
				createSecret(bindingSecret, map[string][]byte{"api-key": {}})
				return baseSpec()
			}, "BindingSecretKeyMissing"),
		)

		It("does not upload again when Cloudflare still has the last upload", func() {
			readyEnv(nil)
			fake := syncedFake()
			r := reconcilerWithFakeWorkerScriptAPI(fake)
			_, err := reconcileWorker(r)
			Expect(err).NotTo(HaveOccurred())

			_, err = reconcileWorker(r)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(Equal([]string{"UploadWorker", "ListWorkers"}))
			expectReady(metav1.ConditionTrue, "Synced")
		})

		DescribeTable("uploads again when the Worker changed outside kflare",
			func(workers []cf.WorkerMetaData) {
				readyEnv(nil)
				fake := syncedFake()
				r := reconcilerWithFakeWorkerScriptAPI(fake)
				_, err := reconcileWorker(r)
				Expect(err).NotTo(HaveOccurred())

				fake.workers = workers
				fake.uploadResp = uploadResp("etag-1", uploadedAt.Add(time.Minute))
				_, err = reconcileWorker(r)
				Expect(err).NotTo(HaveOccurred())
				Expect(fake.calls).To(Equal([]string{"UploadWorker", "ListWorkers", "UploadWorker"}))
				Expect(getWorker().Status.CloudflareMetadata.ModifiedOn).To(Equal("2026-10-03T02:14:10.984591Z"))
			},
			Entry("modified (for example a binding edited in the dashboard)", []cf.WorkerMetaData{
				{ID: scriptName, ETAG: "etag-1", ModifiedOn: uploadedAt.Add(30 * time.Second)},
			}),
			Entry("deleted", []cf.WorkerMetaData{{ID: "some-other-worker", ModifiedOn: uploadedAt}}),
		)

		It("uploads again when the spec changes, without listing", func() {
			readyEnv(nil)
			fake := syncedFake()
			r := reconcilerWithFakeWorkerScriptAPI(fake)
			_, err := reconcileWorker(r)
			Expect(err).NotTo(HaveOccurred())
			firstHash := getWorker().Status.AppliedHash

			ws := getWorker()
			ws.Spec.Bindings[0].PlainText = cf.StringPtr("changed")
			Expect(k8sClient.Update(ctx, ws)).To(Succeed())

			_, err = reconcileWorker(r)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(Equal([]string{"UploadWorker", "UploadWorker"}))
			Expect(fake.uploads[1].Bindings["GREETING"]).To(Equal(cf.WorkerPlainTextBinding{Text: "changed"}))
			Expect(getWorker().Status.AppliedHash).NotTo(Equal(firstHash))
		})

		It("uploads again when a bound Secret changes", func() {
			readyEnv(nil)
			fake := syncedFake()
			r := reconcilerWithFakeWorkerScriptAPI(fake)
			_, err := reconcileWorker(r)
			Expect(err).NotTo(HaveOccurred())

			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: bindingSecret, Namespace: ns}, secret)).To(Succeed())
			secret.Data["api-key"] = []byte("rotated-value")
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())

			_, err = reconcileWorker(r)
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.calls).To(Equal([]string{"UploadWorker", "UploadWorker"}))
			Expect(fake.uploads[1].Bindings["API_KEY"]).To(Equal(cf.WorkerSecretTextBinding{Text: "rotated-value"}))
		})

		DescribeTable("classifies Cloudflare API errors",
			func(alreadyUploaded bool, mutate func(*fakeWorkerScriptAPI), reason string, expectErr bool) {
				readyEnv(nil)
				fake := syncedFake()
				r := reconcilerWithFakeWorkerScriptAPI(fake)
				if alreadyUploaded {
					_, err := reconcileWorker(r)
					Expect(err).NotTo(HaveOccurred())
				}
				mutate(fake)
				_, err := reconcileWorker(r)
				if expectErr {
					Expect(err).To(HaveOccurred())
				} else {
					Expect(err).NotTo(HaveOccurred())
				}
				expectReady(metav1.ConditionFalse, reason)
			},
			Entry("terminal upload error", false, func(f *fakeWorkerScriptAPI) { f.uploadErr = terminalErr() }, "TerminalError", false),
			Entry("retryable list error", true, func(f *fakeWorkerScriptAPI) { f.listErr = errors.New("upstream timeout") }, "APIError", true),
		)
	})

	Describe("Deletion", func() {
		It("deletes the Worker from Cloudflare and removes the finalizer", func() {
			readyEnv(nil)
			setUploadedStatus(uploadedAtStr)
			startDeletion()

			fake := &fakeWorkerScriptAPI{}
			_, err := reconcileWorker(reconcilerWithFakeWorkerScriptAPI(fake))
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.deletes).To(Equal([]string{scriptName}))
			Expect(fake.accountIDs).To(HaveEach(fakeAcctID))
			expectWorkerGone()
		})

		DescribeTable("skips Cloudflare and removes the finalizer",
			func(annotations map[string]string, uploaded bool) {
				readyEnv(annotations)
				if uploaded {
					setUploadedStatus(uploadedAtStr)
				}
				startDeletion()

				fake := &fakeWorkerScriptAPI{}
				_, err := reconcileWorker(reconcilerWithFakeWorkerScriptAPI(fake))
				Expect(err).NotTo(HaveOccurred())
				Expect(fake.calls).To(BeEmpty())
				expectWorkerGone()
			},
			Entry("deletion-policy=retain", map[string]string{deletionPolicy: "retain"}, true),
			Entry("never uploaded by kflare", nil, false),
		)

		It("treats a Worker already gone from Cloudflare as deleted", func() {
			readyEnv(nil)
			setUploadedStatus(uploadedAtStr)
			startDeletion()

			_, err := reconcileWorker(reconcilerWithFakeWorkerScriptAPI(&fakeWorkerScriptAPI{deleteErr: notFoundErr()}))
			Expect(err).NotTo(HaveOccurred())
			expectWorkerGone()
		})

		It("keeps the finalizer and returns the error when the delete fails", func() {
			readyEnv(nil)
			setUploadedStatus(uploadedAtStr)
			startDeletion()

			_, err := reconcileWorker(reconcilerWithFakeWorkerScriptAPI(&fakeWorkerScriptAPI{deleteErr: errors.New("cloudflare API unavailable")}))
			Expect(err).To(MatchError("cloudflare API unavailable"))
			Expect(getWorker().Finalizers).To(ContainElement(reconciler.Finalizer))
		})

		It("returns an error when the account cannot be resolved", func() {
			createWorker(baseSpec(), nil, true)
			setUploadedStatus(uploadedAtStr)
			startDeletion()

			_, err := reconcileWorker(reconcilerWithFakeWorkerScriptAPI(&fakeWorkerScriptAPI{}))
			Expect(err).To(MatchError(ContainSubstring("not found")))
			Expect(getWorker().Finalizers).To(ContainElement(reconciler.Finalizer))
		})

		It("returns an error when the CF client cannot be constructed", func() {
			readyEnv(nil)
			setUploadedStatus(uploadedAtStr)
			startDeletion()

			r := &WorkerScriptReconciler{
				Client: k8sClient,
				Scheme: k8sClient.Scheme(),
				NewWorkerScriptAPI: func(_ string) (WorkerScriptAPI, error) {
					return nil, errors.New("bad token")
				},
			}
			_, err := reconcileWorker(r)
			Expect(err).To(MatchError("bad token"))
			Expect(getWorker().Finalizers).To(ContainElement(reconciler.Finalizer))
		})
	})

	Describe("Validation", func() {
		create := func(mutate func(*cloudflarev1alpha1.WorkerScriptSpec)) error {
			spec := baseSpec()
			mutate(&spec)
			return k8sClient.Create(ctx, &cloudflarev1alpha1.WorkerScript{
				ObjectMeta: metav1.ObjectMeta{Name: wsName, Namespace: ns},
				Spec:       spec,
			})
		}

		DescribeTable("rejects invalid specs",
			func(mutate func(*cloudflarev1alpha1.WorkerScriptSpec), message string) {
				Expect(create(mutate)).To(MatchError(ContainSubstring(message)))
			},
			Entry("both script and scriptConfigMapRef", func(s *cloudflarev1alpha1.WorkerScriptSpec) {
				s.ScriptConfigMapRef = &cloudflarev1alpha1.WorkerKeyReference{Name: "cm", Key: "k"}
			}, "exactly one of script or scriptConfigMapRef"),
			Entry("neither script nor scriptConfigMapRef", func(s *cloudflarev1alpha1.WorkerScriptSpec) {
				s.Script = nil
			}, "exactly one of script or scriptConfigMapRef"),
			Entry("a binding with two sources", func(s *cloudflarev1alpha1.WorkerScriptSpec) {
				s.Bindings[0].KVNamespaceID = cf.StringPtr("kv")
			}, "exactly one of plainText, secretKeyRef, kvNamespaceID or r2BucketName"),
			Entry("a binding with no source", func(s *cloudflarev1alpha1.WorkerScriptSpec) {
				s.Bindings[0].PlainText = nil
			}, "exactly one of plainText, secretKeyRef, kvNamespaceID or r2BucketName"),
			Entry("duplicate binding names", func(s *cloudflarev1alpha1.WorkerScriptSpec) {
				s.Bindings[1].Name = s.Bindings[0].Name
			}, "Duplicate value"),
			Entry("an invalid script name", func(s *cloudflarev1alpha1.WorkerScriptSpec) {
				s.Name = "Not_Valid"
			}, "spec.name"),
		)

		It("rejects changing spec.name", func() {
			createWorker(baseSpec(), nil, false)
			ws := getWorker()
			ws.Spec.Name = "renamed"
			Expect(k8sClient.Update(ctx, ws)).To(MatchError(ContainSubstring("name is immutable")))
		})

		It("rejects changing spec.accountRef", func() {
			createWorker(baseSpec(), nil, false)
			ws := getWorker()
			ws.Spec.AccountRef.Name = "other-account"
			Expect(k8sClient.Update(ctx, ws)).To(MatchError(ContainSubstring("accountRef is immutable")))
		})
	})
})
