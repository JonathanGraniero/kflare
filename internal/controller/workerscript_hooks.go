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
	"slices"
	"time"

	cf "github.com/cloudflare/cloudflare-go"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
)

//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
//+kubebuilder:rbac:groups=kflare.dev,resources=kvnamespaces;r2buckets,verbs=get;list;watch

// The values a WorkerScript's resolve hook gives its calls, besides each
// binding's, under workerBindingValue.
const (
	workerScriptValue = "script"
	workerHashValue   = "hash"
	workerFormatValue = "format"
)

// workerBindingValue is the key of the value a binding of this name gives.
func workerBindingValue(name string) string { return "binding/" + name }

// workerScriptObject is a Worker, as Cloudflare lists it. An upload also
// carries what status records of it: hash, with uploaded set. workerScriptListed
// is the same: a Worker is listed, and read, through the account's list.
type workerScriptObject struct {
	cf.WorkerMetaData
	uploaded bool
	hash     string
}

type workerScriptListed = workerScriptObject

// WorkerScriptAPI is the subset of the Cloudflare API used by a WorkerScript.
// In production, *cfpkg.Client satisfies this interface because it embeds
// *cf.API.
type WorkerScriptAPI interface {
	UploadWorker(ctx context.Context, rc *cf.ResourceContainer, params cf.CreateWorkerParams) (cf.WorkerScriptResponse, error)
	ListWorkers(ctx context.Context, rc *cf.ResourceContainer, params cf.ListWorkersParams) (cf.WorkerListResponse, *cf.ResultInfo, error)
	DeleteWorker(ctx context.Context, rc *cf.ResourceContainer, params cf.DeleteWorkerParams) error
}

// defaultWorkerScriptAPI is the production factory: it delegates to cfpkg.New
// so that the returned *cfpkg.Client (which embeds *cf.API) satisfies
// WorkerScriptAPI.
func defaultWorkerScriptAPI(token string) (WorkerScriptAPI, error) {
	return cfpkg.New(token)
}

// workerScriptKind describes WorkerScripts to the shared CRUD reconciler. An
// upload creates a Worker and replaces one with the same name, so a Worker this
// resource has not uploaded yet is an existing one, which it replaces only as
// its adoption policy allows. It uploads the Worker when the desired state
// changed since the last upload, when the Worker is missing from Cloudflare, or
// when Cloudflare reports a modification time other than the one from kflare's
// last upload. Cloudflare's etag only covers the code, so it cannot reveal a
// binding or compatibility change made outside kflare; the modification time
// can.
var workerScriptKind = crudResourceKind[*cloudflarev1alpha1.WorkerScript, WorkerScriptAPI, workerScriptObject,
	workerScriptListed]{
	name:      "WorkerScript",
	noun:      "Worker",
	newObject: func() *cloudflarev1alpha1.WorkerScript { return &cloudflarev1alpha1.WorkerScript{} },
	newList:   func() client.ObjectList { return &cloudflarev1alpha1.WorkerScriptList{} },

	accountRef:     func(obj *cloudflarev1alpha1.WorkerScript) string { return obj.Spec.AccountRef.Name },
	key:            func(obj *cloudflarev1alpha1.WorkerScript) string { return obj.Spec.Name },
	claimLabel:     cloudflarev1alpha1.WorkerScriptIDLabel,
	conflictReason: reasonNameConflict,
	// The modification time records an upload: a WorkerScript of an earlier
	// release has no scriptName in status, and owns its Worker all the same.
	statusID: func(obj *cloudflarev1alpha1.WorkerScript) string {
		if obj.Status.CloudflareMetadata.ModifiedOn == "" {
			return ""
		}
		return obj.Spec.Name
	},
	observe: func(obj *cloudflarev1alpha1.WorkerScript, current *workerScriptObject) {
		if !current.uploaded {
			return
		}
		obj.Status.CloudflareMetadata.ScriptName = current.ID
		obj.Status.CloudflareMetadata.Etag = current.ETAG
		obj.Status.CloudflareMetadata.ModifiedOn = formatModifiedOn(current.ModifiedOn)
		obj.Status.AppliedHash = current.hash
	},

	id: func(current *workerScriptObject) string { return current.ID },
	matches: func(obj *cloudflarev1alpha1.WorkerScript, current *workerScriptObject, refs map[string]string) bool {
		return current.uploaded || refs[workerHashValue] == obj.Status.AppliedHash &&
			formatModifiedOn(current.ModifiedOn) == obj.Status.CloudflareMetadata.ModifiedOn
	},

	get: func(ctx context.Context, api WorkerScriptAPI, _ *cloudflarev1alpha1.WorkerScript, accountID,
		id string) (*workerScriptObject, error) {
		found, err := listedWorker(ctx, api, accountID, id)
		if err != nil || found != nil {
			return found, err
		}
		return &workerScriptObject{}, nil
	},
	create: func(ctx context.Context, api WorkerScriptAPI, accountID string, obj *cloudflarev1alpha1.WorkerScript,
		refs map[string]string) (*workerScriptObject, error) {
		return uploadWorker(ctx, api, accountID, obj, refs)
	},
	update: func(ctx context.Context, api WorkerScriptAPI, obj *cloudflarev1alpha1.WorkerScript, accountID, _ string,
		refs map[string]string) (*workerScriptObject, error) {
		return uploadWorker(ctx, api, accountID, obj, refs)
	},
	delete: func(ctx context.Context, api WorkerScriptAPI, obj *cloudflarev1alpha1.WorkerScript, accountID, _ string) error {
		return api.DeleteWorker(ctx, cf.AccountIdentifier(accountID), cf.DeleteWorkerParams{ScriptName: obj.Spec.Name})
	},

	hooks: crudHooks[*cloudflarev1alpha1.WorkerScript, WorkerScriptAPI, workerScriptObject]{
		resolve: resolveWorkerUpload,
		gone:    func(current *workerScriptObject) bool { return current.ID == "" },
		sourceChanged: func(obj *cloudflarev1alpha1.WorkerScript, refs map[string]string) bool {
			return refs[workerHashValue] != obj.Status.AppliedHash
		},
		find: func(ctx context.Context, _ client.Client, api WorkerScriptAPI, obj *cloudflarev1alpha1.WorkerScript,
			accountID string, _ map[string]string) (string, *workerScriptObject, error) {
			found, err := listedWorker(ctx, api, accountID, obj.Spec.Name)
			if err != nil || found == nil {
				return "", nil, err
			}
			return found.ID, found, nil
		},
		watch: watchWorkerSources,
	},
}

// listedWorker returns the account's Worker named name, or nil when it has
// none: Cloudflare's read of a script returns only its code.
func listedWorker(ctx context.Context, api WorkerScriptAPI, accountID, name string) (*workerScriptObject, error) {
	workers, _, err := api.ListWorkers(ctx, cf.AccountIdentifier(accountID), cf.ListWorkersParams{})
	if err != nil {
		return nil, err
	}
	for _, worker := range workers.WorkerList {
		if worker.ID == name {
			return &workerScriptObject{WorkerMetaData: worker}, nil
		}
	}
	return nil, nil
}

// uploadWorker uploads the Worker refs describes.
func uploadWorker(ctx context.Context, api WorkerScriptAPI, accountID string, ws *cloudflarev1alpha1.WorkerScript,
	refs map[string]string) (*workerScriptObject, error) {
	resp, err := api.UploadWorker(ctx, cf.AccountIdentifier(accountID), workerUpload(ws, refs))
	if err != nil {
		return nil, err
	}
	return &workerScriptObject{uploaded: true, hash: refs[workerHashValue],
		WorkerMetaData: cf.WorkerMetaData{ID: ws.Spec.Name, ETAG: resp.ETAG, ModifiedOn: resp.ModifiedOn}}, nil
}

// formatModifiedOn renders a modification time at full precision, so a
// stored value compares equal to the same instant reported later.
func formatModifiedOn(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// resolveWorkerUpload resolves the script source and binding values into refs,
// together with a hash identifying that desired state. A missing ConfigMap or
// Secret is reported and picked up again by the watches on both kinds.
//
// Secret values go into refs, which lives for this reconcile, but not into the
// hash: a Secret contributes its UID, resourceVersion and the key's name
// instead, so the hash (stored in status) reveals nothing about the value yet
// changes whenever it does.
func resolveWorkerUpload(ctx context.Context, c client.Client, ws *cloudflarev1alpha1.WorkerScript,
	refs map[string]string) *conditionError {
	script, srcErr := scriptSource(ctx, c, ws)
	if srcErr != nil {
		return srcErr
	}

	format := ws.Spec.Format
	if format == "" {
		format = cloudflarev1alpha1.WorkerFormatModule
	}
	refs[workerScriptValue], refs[workerFormatValue] = script, format

	// Each field is written %q-quoted on its own line, so the encoding is
	// unambiguous and writing to the hash cannot fail.
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "script=%q\nformat=%q\ncompatibilityDate=%q\ncompatibilityFlags=%q\n",
		script, format, ws.Spec.CompatibilityDate, ws.Spec.CompatibilityFlags)

	for _, b := range ws.Spec.Bindings {
		var kind, value, hashed string
		switch {
		case b.PlainText != nil:
			kind, value, hashed = "plainText", *b.PlainText, *b.PlainText
		case b.KVNamespaceRef != nil:
			namespaceID, srcErr := bindingKVNamespace(ctx, c, ws, b)
			if srcErr != nil {
				return srcErr
			}
			kind, value, hashed = "kvNamespace", namespaceID, namespaceID
		case b.KVNamespaceID != nil:
			kind, value, hashed = "kvNamespace", *b.KVNamespaceID, *b.KVNamespaceID
		case b.R2BucketRef != nil:
			bucketName, srcErr := bindingR2Bucket(ctx, c, ws, b)
			if srcErr != nil {
				return srcErr
			}
			kind, value, hashed = "r2Bucket", bucketName, bucketName
		case b.R2BucketName != nil:
			kind, value, hashed = "r2Bucket", *b.R2BucketName, *b.R2BucketName
		case b.SecretKeyRef != nil:
			secret, version, srcErr := bindingSecret(ctx, c, ws.Namespace, b)
			if srcErr != nil {
				return srcErr
			}
			kind, value, hashed = "secretText", secret, version
		}
		refs[workerBindingValue(b.Name)] = value
		_, _ = fmt.Fprintf(h, "binding=%q type=%q value=%q\n", b.Name, kind, hashed)
	}
	refs[workerHashValue] = hex.EncodeToString(h.Sum(nil))
	return nil
}

// workerUpload builds the upload request from what resolveWorkerUpload gave.
func workerUpload(ws *cloudflarev1alpha1.WorkerScript, refs map[string]string) cf.CreateWorkerParams {
	bindings := make(map[string]cf.WorkerBinding, len(ws.Spec.Bindings))
	for _, b := range ws.Spec.Bindings {
		value := refs[workerBindingValue(b.Name)]
		switch {
		case b.PlainText != nil:
			bindings[b.Name] = cf.WorkerPlainTextBinding{Text: value}
		case b.KVNamespaceRef != nil, b.KVNamespaceID != nil:
			bindings[b.Name] = cf.WorkerKvNamespaceBinding{NamespaceID: value}
		case b.R2BucketRef != nil, b.R2BucketName != nil:
			bindings[b.Name] = cf.WorkerR2BucketBinding{BucketName: value}
		case b.SecretKeyRef != nil:
			bindings[b.Name] = cf.WorkerSecretTextBinding{Text: value}
		}
	}
	return cf.CreateWorkerParams{
		ScriptName:         ws.Spec.Name,
		Script:             refs[workerScriptValue],
		Module:             refs[workerFormatValue] == cloudflarev1alpha1.WorkerFormatModule,
		CompatibilityDate:  ws.Spec.CompatibilityDate,
		CompatibilityFlags: ws.Spec.CompatibilityFlags,
		Bindings:           bindings,
	}
}

// scriptSource returns the Worker code from spec.script or the referenced ConfigMap.
func scriptSource(ctx context.Context, c client.Client, ws *cloudflarev1alpha1.WorkerScript) (string, *conditionError) {
	if ws.Spec.Script != nil {
		return *ws.Spec.Script, nil
	}
	ref := ws.Spec.ScriptConfigMapRef
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ws.Namespace}, cm); err != nil {
		return "", &conditionError{
			Reason:  "ScriptConfigMapNotFound",
			Message: fmt.Sprintf("ConfigMap %s/%s not found: %v", ws.Namespace, ref.Name, err),
		}
	}
	script, ok := cm.Data[ref.Key]
	if !ok || script == "" {
		return "", &conditionError{
			Reason:  "ScriptKeyMissing",
			Message: fmt.Sprintf("Key %q not found or empty in ConfigMap %s/%s", ref.Key, ws.Namespace, ref.Name),
		}
	}
	return script, nil
}

// bindingKVNamespace returns the Cloudflare namespace ID of the KVNamespace a
// kvNamespaceRef binding names. It must be ready and use the Worker's account:
// a Worker can only bind namespaces in its own account.
func bindingKVNamespace(ctx context.Context, c client.Client, ws *cloudflarev1alpha1.WorkerScript,
	b cloudflarev1alpha1.WorkerBinding) (string, *conditionError) {
	ref := b.KVNamespaceRef
	kv := &cloudflarev1alpha1.KVNamespace{}
	if err := c.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ws.Namespace}, kv); err != nil {
		return "", &conditionError{
			Reason:  "KVNamespaceNotFound",
			Message: fmt.Sprintf("KVNamespace %q for binding %q not found: %v", ref.Name, b.Name, err),
		}
	}
	if kv.Spec.AccountRef.Name != ws.Spec.AccountRef.Name {
		return "", &conditionError{
			Reason: reasonAccountMismatch,
			Message: fmt.Sprintf("KVNamespace %q for binding %q uses CloudflareAccount %q but this Worker uses %q",
				kv.Name, b.Name, kv.Spec.AccountRef.Name, ws.Spec.AccountRef.Name),
		}
	}
	if !isKVNamespaceReady(kv) {
		return "", &conditionError{
			Reason:  "KVNamespaceNotReady",
			Message: fmt.Sprintf("KVNamespace %q for binding %q is not ready", kv.Name, b.Name),
		}
	}
	return kv.Status.CloudflareMetadata.NamespaceID, nil
}

// isKVNamespaceReady returns true if the KVNamespace has a Ready=True
// condition and a namespace ID.
func isKVNamespaceReady(kv *cloudflarev1alpha1.KVNamespace) bool {
	return kv.Status.CloudflareMetadata.NamespaceID != "" &&
		meta.IsStatusConditionTrue(kv.Status.Conditions, cloudflarev1alpha1.ConditionReady)
}

// boundKVNamespaces returns the names of the KVNamespaces ws binds through
// kvNamespaceRef.
func boundKVNamespaces(ws *cloudflarev1alpha1.WorkerScript) []string {
	var names []string
	for _, b := range ws.Spec.Bindings {
		if b.KVNamespaceRef != nil {
			names = append(names, b.KVNamespaceRef.Name)
		}
	}
	return names
}

// r2DefaultJurisdiction is the jurisdiction of the R2 buckets a Worker binds
// by name alone.
const r2DefaultJurisdiction = "default"

// bindingR2Bucket returns the Cloudflare bucket name of the R2Bucket an
// r2BucketRef binding names. It must be ready, because Cloudflare refuses to
// upload a Worker bound to a missing bucket, use the Worker's account, and be
// in the default jurisdiction: cloudflare-go v0.119's R2 binding names a
// bucket without one, so it would bind the default jurisdiction's bucket of
// that name.
//
// The binding resolves the bucket by name: a Worker keeps working when its
// bucket is deleted and created again under the same name (verified live),
// so unlike a KV namespace a recreated bucket needs no re-upload.
func bindingR2Bucket(ctx context.Context, c client.Client, ws *cloudflarev1alpha1.WorkerScript,
	b cloudflarev1alpha1.WorkerBinding) (string, *conditionError) {
	ref := b.R2BucketRef
	bucket := &cloudflarev1alpha1.R2Bucket{}
	if err := c.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ws.Namespace}, bucket); err != nil {
		return "", &conditionError{
			Reason:  "R2BucketNotFound",
			Message: fmt.Sprintf("R2Bucket %q for binding %q not found: %v", ref.Name, b.Name, err),
		}
	}
	if bucket.Spec.AccountRef.Name != ws.Spec.AccountRef.Name {
		return "", &conditionError{
			Reason: reasonAccountMismatch,
			Message: fmt.Sprintf("R2Bucket %q for binding %q uses CloudflareAccount %q but this Worker uses %q",
				bucket.Name, b.Name, bucket.Spec.AccountRef.Name, ws.Spec.AccountRef.Name),
		}
	}
	if j := bucket.Spec.Jurisdiction; j != "" && j != r2DefaultJurisdiction {
		return "", &conditionError{
			Reason: "R2BucketJurisdictionUnsupported",
			Message: fmt.Sprintf("R2Bucket %q for binding %q is in the %s jurisdiction, and kflare binds only "+
				"buckets in the default one yet", bucket.Name, b.Name, j),
		}
	}
	if !meta.IsStatusConditionTrue(bucket.Status.Conditions, cloudflarev1alpha1.ConditionReady) {
		return "", &conditionError{
			Reason:  "R2BucketNotReady",
			Message: fmt.Sprintf("R2Bucket %q for binding %q is not ready", bucket.Name, b.Name),
		}
	}
	return bucket.Spec.Name, nil
}

// boundR2Buckets returns the names of the R2Buckets ws binds through
// r2BucketRef.
func boundR2Buckets(ws *cloudflarev1alpha1.WorkerScript) []string {
	var names []string
	for _, b := range ws.Spec.Bindings {
		if b.R2BucketRef != nil {
			names = append(names, b.R2BucketRef.Name)
		}
	}
	return names
}

// bindingSecret returns the value of a secret binding and a version string
// (UID, resourceVersion and key) that changes whenever the value can.
func bindingSecret(ctx context.Context, c client.Client, namespace string,
	b cloudflarev1alpha1.WorkerBinding) (string, string, *conditionError) {
	ref := b.SecretKeyRef
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: namespace}, secret); err != nil {
		return "", "", &conditionError{
			Reason:  "BindingSecretNotFound",
			Message: fmt.Sprintf("Secret %s/%s for binding %q not found: %v", namespace, ref.Name, b.Name, err),
		}
	}
	value, ok := secret.Data[ref.Key]
	if !ok || len(value) == 0 {
		return "", "", &conditionError{
			Reason:  "BindingSecretKeyMissing",
			Message: fmt.Sprintf("Key %q not found or empty in Secret %s/%s for binding %q", ref.Key, namespace, ref.Name, b.Name),
		}
	}
	return string(value), fmt.Sprintf("%s/%s/%s", secret.UID, secret.ResourceVersion, ref.Key), nil
}

// watchWorkerSources watches the ConfigMaps holding script code, the Secrets
// behind secret bindings, and the KVNamespaces and R2Buckets behind namespace
// and bucket bindings, so a change to any of them re-triggers the Workers that
// use it.
func watchWorkerSources(b *builder.Builder, c client.Client) *builder.Builder {
	r := &WorkerScriptReconciler{Client: c}
	return b.
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.workersForConfigMap)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.workersForSecret)).
		Watches(&cloudflarev1alpha1.KVNamespace{}, handler.EnqueueRequestsFromMapFunc(r.workersForKVNamespace)).
		Watches(&cloudflarev1alpha1.R2Bucket{}, handler.EnqueueRequestsFromMapFunc(r.workersForR2Bucket))
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

// workersForKVNamespace maps a KVNamespace event to the WorkerScripts that bind it.
func (r *WorkerScriptReconciler) workersForKVNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.workersMatching(ctx, obj.GetNamespace(), func(ws *cloudflarev1alpha1.WorkerScript) bool {
		return slices.Contains(boundKVNamespaces(ws), obj.GetName())
	})
}

// workersForR2Bucket maps an R2Bucket event to the WorkerScripts that bind it.
func (r *WorkerScriptReconciler) workersForR2Bucket(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.workersMatching(ctx, obj.GetNamespace(), func(ws *cloudflarev1alpha1.WorkerScript) bool {
		return slices.Contains(boundR2Buckets(ws), obj.GetName())
	})
}
