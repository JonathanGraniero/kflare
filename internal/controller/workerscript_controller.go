/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	cf "github.com/cloudflare/cloudflare-go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// WorkerScriptAPI is the subset of the Cloudflare API used by this controller.
// Declaring a narrow interface keeps unit tests simple: tests inject a fake
// that implements only these methods. In production, *cfpkg.Client satisfies
// this interface because it embeds *cf.API.
type WorkerScriptAPI interface {
	UploadWorker(ctx context.Context, rc *cf.ResourceContainer, params cf.CreateWorkerParams) (cf.WorkerScriptResponse, error)
	ListWorkers(ctx context.Context, rc *cf.ResourceContainer, params cf.ListWorkersParams) (cf.WorkerListResponse, *cf.ResultInfo, error)
	DeleteWorker(ctx context.Context, rc *cf.ResourceContainer, params cf.DeleteWorkerParams) error
}

// WorkerScriptReconciler reconciles a WorkerScript object.
type WorkerScriptReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NewWorkerScriptAPI constructs a WorkerScriptAPI from a raw API token.
	// Defaults to defaultWorkerScriptAPI; overridden in tests to inject a fake.
	NewWorkerScriptAPI func(token string) (WorkerScriptAPI, error)
}

//+kubebuilder:rbac:groups=cloudflare.cloudflare.k8s.io,resources=workerscripts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cloudflare.cloudflare.k8s.io,resources=workerscripts/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cloudflare.cloudflare.k8s.io,resources=workerscripts/finalizers,verbs=update
//+kubebuilder:rbac:groups=cloudflare.cloudflare.k8s.io,resources=cloudflareaccounts,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

func (r *WorkerScriptReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	ws := &cloudflarev1alpha1.WorkerScript{}
	if err := r.Get(ctx, req.NamespacedName, ws); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion before anything else.
	if !ws.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, ws)
	}

	// Ensure the finalizer is present before doing any work.
	// On the very first reconcile the finalizer is absent — add it and return.
	// controller-runtime will re-enqueue the object once the update is visible.
	added, err := reconciler.EnsureFinalizer(ctx, r.Client, ws)
	if err != nil {
		return ctrl.Result{}, err
	}
	if added {
		logger.Info("Added finalizer", "workerscript", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	account, token, credErr := resolveAccountToken(ctx, r.Client, ws.Spec.AccountRef.Name, true)
	if credErr != nil {
		reconciler.SetCondition(&ws.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, credErr.Reason, credErr.Message, ws.Generation)
		return ctrl.Result{RequeueAfter: credentialsRetryInterval}, r.Status().Update(ctx, ws)
	}

	// Resolve the script source and binding values. A missing ConfigMap or
	// Secret is reported and picked up again by the watches on both kinds.
	upload, hash, srcErr := r.desiredUpload(ctx, ws)
	if srcErr != nil {
		reconciler.SetCondition(&ws.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, srcErr.Reason, srcErr.Message, ws.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, ws)
	}

	cfAPI, err := r.newWorkerScriptAPI(token)
	if err != nil {
		reconciler.SetCondition(&ws.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "InvalidToken",
			fmt.Sprintf("Failed to create Cloudflare client: %v", err),
			ws.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, ws)
	}

	return r.syncWorker(ctx, ws, account, upload, hash, cfAPI)
}

// syncWorker drives the desired→observed→delta→reconcile loop. It uploads the
// Worker when the desired state changed since the last upload, when the
// Worker is missing from Cloudflare, or when Cloudflare reports a
// modification time other than the one from kflare's last upload.
//
// Cloudflare's etag only covers the code, so it cannot reveal a binding or
// compatibility change made outside kflare; the modification time can.
func (r *WorkerScriptReconciler) syncWorker(
	ctx context.Context,
	ws *cloudflarev1alpha1.WorkerScript,
	account *cloudflarev1alpha1.CloudflareAccount,
	upload cf.CreateWorkerParams,
	hash string,
	cfAPI WorkerScriptAPI,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	rc := cf.AccountIdentifier(account.Spec.AccountID)

	reason, observed := "", ""
	if hash != ws.Status.AppliedHash {
		reason = "desired state changed"
	} else {
		workers, _, err := cfAPI.ListWorkers(ctx, rc, cf.ListWorkersParams{})
		if err != nil {
			return r.handleCFError(ctx, ws, err)
		}
		reason, observed = workerDrift(workers.WorkerList, ws.Spec.Name, ws.Status.CloudflareMetadata.ModifiedOn)
	}

	if reason != "" {
		resp, err := cfAPI.UploadWorker(ctx, rc, upload)
		if err != nil {
			return r.handleCFError(ctx, ws, err)
		}
		logger.Info("Uploaded Worker", "script", ws.Spec.Name, "reason", reason,
			"observedModifiedOn", observed, "previousModifiedOn", ws.Status.CloudflareMetadata.ModifiedOn,
			"uploadedModifiedOn", formatModifiedOn(resp.ModifiedOn))
		ws.Status.CloudflareMetadata.Etag = resp.ETAG
		ws.Status.CloudflareMetadata.ModifiedOn = formatModifiedOn(resp.ModifiedOn)
		ws.Status.AppliedHash = hash
	}

	reconciler.SetCondition(&ws.Status.Conditions, cloudflarev1alpha1.ConditionReady,
		metav1.ConditionTrue, "Synced",
		"Worker is synced with Cloudflare",
		ws.Generation)
	return ctrl.Result{}, r.Status().Update(ctx, ws)
}

// workerDrift returns why the Worker named scriptName must be uploaded again,
// or "" when Cloudflare still has kflare's last upload, together with the
// modification time Cloudflare reported (empty when the Worker is missing).
func workerDrift(workers []cf.WorkerMetaData, scriptName, appliedModifiedOn string) (reason, observed string) {
	for _, w := range workers {
		if w.ID != scriptName {
			continue
		}
		observed = formatModifiedOn(w.ModifiedOn)
		if observed != appliedModifiedOn {
			return "modified outside kflare", observed
		}
		return "", observed
	}
	return "missing from Cloudflare", ""
}

// formatModifiedOn renders a modification time at full precision, so a
// stored value compares equal to the same instant reported later.
func formatModifiedOn(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// sourceError explains why the Worker's script or a binding value could not
// be resolved. Reason is a CamelCase condition reason.
type sourceError struct {
	Reason  string
	Message string
}

// desiredUpload resolves the script source and binding values into the
// upload request, together with a hash identifying that desired state.
//
// Secret values go into the request but not into the hash: a Secret
// contributes its UID and resourceVersion instead, so the hash (stored in
// status) reveals nothing about the value yet changes whenever it does.
func (r *WorkerScriptReconciler) desiredUpload(
	ctx context.Context,
	ws *cloudflarev1alpha1.WorkerScript,
) (cf.CreateWorkerParams, string, *sourceError) {
	script, srcErr := r.scriptSource(ctx, ws)
	if srcErr != nil {
		return cf.CreateWorkerParams{}, "", srcErr
	}

	format := ws.Spec.Format
	if format == "" {
		format = cloudflarev1alpha1.WorkerFormatModule
	}

	// Each field is written %q-quoted on its own line, so the encoding is
	// unambiguous and writing to the hash cannot fail.
	h := sha256.New()
	fmt.Fprintf(h, "script=%q\nformat=%q\ncompatibilityDate=%q\ncompatibilityFlags=%q\n",
		script, format, ws.Spec.CompatibilityDate, ws.Spec.CompatibilityFlags)

	bindings := make(map[string]cf.WorkerBinding, len(ws.Spec.Bindings))
	for _, b := range ws.Spec.Bindings {
		var kind, hashed string
		switch {
		case b.PlainText != nil:
			bindings[b.Name] = cf.WorkerPlainTextBinding{Text: *b.PlainText}
			kind, hashed = "plainText", *b.PlainText
		case b.KVNamespaceID != nil:
			bindings[b.Name] = cf.WorkerKvNamespaceBinding{NamespaceID: *b.KVNamespaceID}
			kind, hashed = "kvNamespace", *b.KVNamespaceID
		case b.R2BucketName != nil:
			bindings[b.Name] = cf.WorkerR2BucketBinding{BucketName: *b.R2BucketName}
			kind, hashed = "r2Bucket", *b.R2BucketName
		case b.SecretKeyRef != nil:
			value, version, srcErr := r.bindingSecret(ctx, ws.Namespace, b)
			if srcErr != nil {
				return cf.CreateWorkerParams{}, "", srcErr
			}
			bindings[b.Name] = cf.WorkerSecretTextBinding{Text: value}
			kind, hashed = "secretText", version
		}
		fmt.Fprintf(h, "binding=%q type=%q value=%q\n", b.Name, kind, hashed)
	}

	return cf.CreateWorkerParams{
		ScriptName:         ws.Spec.Name,
		Script:             script,
		Module:             format == cloudflarev1alpha1.WorkerFormatModule,
		CompatibilityDate:  ws.Spec.CompatibilityDate,
		CompatibilityFlags: ws.Spec.CompatibilityFlags,
		Bindings:           bindings,
	}, hex.EncodeToString(h.Sum(nil)), nil
}

// scriptSource returns the Worker code from spec.script or the referenced ConfigMap.
func (r *WorkerScriptReconciler) scriptSource(ctx context.Context, ws *cloudflarev1alpha1.WorkerScript) (string, *sourceError) {
	if ws.Spec.Script != nil {
		return *ws.Spec.Script, nil
	}
	ref := ws.Spec.ScriptConfigMapRef
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ws.Namespace}, cm); err != nil {
		return "", &sourceError{
			Reason:  "ScriptConfigMapNotFound",
			Message: fmt.Sprintf("ConfigMap %s/%s not found: %v", ws.Namespace, ref.Name, err),
		}
	}
	script, ok := cm.Data[ref.Key]
	if !ok || script == "" {
		return "", &sourceError{
			Reason:  "ScriptKeyMissing",
			Message: fmt.Sprintf("Key %q not found or empty in ConfigMap %s/%s", ref.Key, ws.Namespace, ref.Name),
		}
	}
	return script, nil
}

// bindingSecret returns the value of a secret binding and a version string
// (UID and resourceVersion) that changes whenever the Secret does.
func (r *WorkerScriptReconciler) bindingSecret(
	ctx context.Context,
	namespace string,
	b cloudflarev1alpha1.WorkerBinding,
) (string, string, *sourceError) {
	ref := b.SecretKeyRef
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: namespace}, secret); err != nil {
		return "", "", &sourceError{
			Reason:  "BindingSecretNotFound",
			Message: fmt.Sprintf("Secret %s/%s for binding %q not found: %v", namespace, ref.Name, b.Name, err),
		}
	}
	value, ok := secret.Data[ref.Key]
	if !ok || len(value) == 0 {
		return "", "", &sourceError{
			Reason:  "BindingSecretKeyMissing",
			Message: fmt.Sprintf("Key %q not found or empty in Secret %s/%s for binding %q", ref.Key, namespace, ref.Name, b.Name),
		}
	}
	return string(value), fmt.Sprintf("%s/%s/%s", secret.UID, secret.ResourceVersion, ref.Key), nil
}

// handleCFError sets the appropriate condition based on whether the Cloudflare
// error is terminal (stop requeuing) or retryable (let controller-runtime
// back off and retry).
func (r *WorkerScriptReconciler) handleCFError(ctx context.Context, ws *cloudflarev1alpha1.WorkerScript, err error) (ctrl.Result, error) {
	if cfpkg.IsTerminalError(err) {
		reconciler.SetCondition(&ws.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "TerminalError",
			fmt.Sprintf("Terminal Cloudflare API error: %v", err),
			ws.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, ws)
	}
	reconciler.SetCondition(&ws.Status.Conditions, cloudflarev1alpha1.ConditionReady,
		metav1.ConditionFalse, "APIError",
		fmt.Sprintf("Cloudflare API error: %v", err),
		ws.Generation)
	if statusErr := r.Status().Update(ctx, ws); statusErr != nil {
		return ctrl.Result{}, statusErr
	}
	return ctrl.Result{}, err
}

// reconcileDelete handles the deletion lifecycle: unless the retain policy is
// set, it deletes the Worker from Cloudflare, then removes the finalizer. A
// Worker kflare never uploaded is left alone.
func (r *WorkerScriptReconciler) reconcileDelete(ctx context.Context, ws *cloudflarev1alpha1.WorkerScript) (ctrl.Result, error) {
	// Safety check: if the finalizer is already gone, there is nothing to do.
	if !controllerutil.ContainsFinalizer(ws, reconciler.Finalizer) {
		return ctrl.Result{}, nil
	}

	uploaded := ws.Status.CloudflareMetadata.ModifiedOn != ""
	if uploaded && !reconciler.RetainOnDelete(ws) {
		account, token, credErr := resolveAccountToken(ctx, r.Client, ws.Spec.AccountRef.Name, false)
		if credErr != nil {
			return ctrl.Result{}, credErr
		}
		cfAPI, err := r.newWorkerScriptAPI(token)
		if err != nil {
			return ctrl.Result{}, err
		}
		err = cfAPI.DeleteWorker(ctx, cf.AccountIdentifier(account.Spec.AccountID), cf.DeleteWorkerParams{ScriptName: ws.Spec.Name})
		if err != nil && !cfpkg.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	// Remove the finalizer to allow Kubernetes to complete the deletion.
	_, err := reconciler.RemoveFinalizer(ctx, r.Client, ws)
	return ctrl.Result{}, err
}

// SetupWithManager registers WorkerScriptReconciler with the manager. Besides
// WorkerScripts it watches the ConfigMaps holding script code, the Secrets
// behind secret bindings, and CloudflareAccounts, so a change to any of them
// re-triggers the Workers that use it.
func (r *WorkerScriptReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudflarev1alpha1.WorkerScript{}).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.workersForConfigMap)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.workersForSecret)).
		Watches(&cloudflarev1alpha1.CloudflareAccount{}, handler.EnqueueRequestsFromMapFunc(r.workersForAccount)).
		Complete(r)
}

// workersMatching returns a reconcile.Request for every WorkerScript in
// namespace (all namespaces when empty) for which match returns true.
func (r *WorkerScriptReconciler) workersMatching(
	ctx context.Context,
	namespace string,
	match func(*cloudflarev1alpha1.WorkerScript) bool,
) []reconcile.Request {
	list := &cloudflarev1alpha1.WorkerScriptList{}
	if err := r.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if match(&list.Items[i]) {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: list.Items[i].Name, Namespace: list.Items[i].Namespace},
			})
		}
	}
	return reqs
}

// workersForConfigMap maps a ConfigMap event to the WorkerScripts reading their code from it.
func (r *WorkerScriptReconciler) workersForConfigMap(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.workersMatching(ctx, obj.GetNamespace(), func(ws *cloudflarev1alpha1.WorkerScript) bool {
		return ws.Spec.ScriptConfigMapRef != nil && ws.Spec.ScriptConfigMapRef.Name == obj.GetName()
	})
}

// workersForSecret maps a Secret event to the WorkerScripts with a secret binding read from it.
func (r *WorkerScriptReconciler) workersForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.workersMatching(ctx, obj.GetNamespace(), func(ws *cloudflarev1alpha1.WorkerScript) bool {
		for _, b := range ws.Spec.Bindings {
			if b.SecretKeyRef != nil && b.SecretKeyRef.Name == obj.GetName() {
				return true
			}
		}
		return false
	})
}

// workersForAccount maps a CloudflareAccount event to the WorkerScripts that reference it.
func (r *WorkerScriptReconciler) workersForAccount(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.workersMatching(ctx, "", func(ws *cloudflarev1alpha1.WorkerScript) bool {
		return ws.Spec.AccountRef.Name == obj.GetName()
	})
}

// newWorkerScriptAPI builds a WorkerScriptAPI with the injected factory,
// falling back to the production client.
func (r *WorkerScriptReconciler) newWorkerScriptAPI(token string) (WorkerScriptAPI, error) {
	if r.NewWorkerScriptAPI != nil {
		return r.NewWorkerScriptAPI(token)
	}
	return defaultWorkerScriptAPI(token)
}

// defaultWorkerScriptAPI is the production factory: it delegates to cfpkg.New
// so that the returned *cfpkg.Client (which embeds *cf.API) satisfies
// WorkerScriptAPI.
func defaultWorkerScriptAPI(token string) (WorkerScriptAPI, error) {
	return cfpkg.New(token)
}
