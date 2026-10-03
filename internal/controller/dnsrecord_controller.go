/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

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

// DNSRecordAPI is the subset of the Cloudflare API used by this controller.
// Declaring a narrow interface keeps unit tests simple: tests inject a fake
// that implements only these methods. In production, *cfpkg.Client satisfies
// this interface because it embeds *cf.API.
type DNSRecordAPI interface {
	CreateDNSRecord(ctx context.Context, rc *cf.ResourceContainer, params cf.CreateDNSRecordParams) (cf.DNSRecord, error)
	GetDNSRecord(ctx context.Context, rc *cf.ResourceContainer, recordID string) (cf.DNSRecord, error)
	ListDNSRecords(ctx context.Context, rc *cf.ResourceContainer, params cf.ListDNSRecordsParams) ([]cf.DNSRecord, *cf.ResultInfo, error)
	UpdateDNSRecord(ctx context.Context, rc *cf.ResourceContainer, params cf.UpdateDNSRecordParams) (cf.DNSRecord, error)
	DeleteDNSRecord(ctx context.Context, rc *cf.ResourceContainer, recordID string) error
}

// DNSRecordReconciler reconciles a DNSRecord object.
type DNSRecordReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NewDNSRecordAPI constructs a DNSRecordAPI from a raw API token.
	// Defaults to defaultDNSRecordAPI; overridden in tests to inject a fake.
	NewDNSRecordAPI func(token string) (DNSRecordAPI, error)
}

//+kubebuilder:rbac:groups=cloudflare.cloudflare.k8s.io,resources=dnsrecords,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cloudflare.cloudflare.k8s.io,resources=dnsrecords/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cloudflare.cloudflare.k8s.io,resources=dnsrecords/finalizers,verbs=update
//+kubebuilder:rbac:groups=cloudflare.cloudflare.k8s.io,resources=zones,verbs=get;list;watch

func (r *DNSRecordReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	record := &cloudflarev1alpha1.DNSRecord{}
	if err := r.Get(ctx, req.NamespacedName, record); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion before anything else.
	if !record.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, record)
	}

	// Ensure the finalizer is present before doing any work.
	// On the very first reconcile the finalizer is absent — add it and return.
	// controller-runtime will re-enqueue the object once the update is visible.
	added, err := reconciler.EnsureFinalizer(ctx, r.Client, record)
	if err != nil {
		return ctrl.Result{}, err
	}
	if added {
		logger.Info("Added finalizer", "dnsrecord", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	// Fetch the Zone that this record belongs to (same namespace).
	zone := &cloudflarev1alpha1.Zone{}
	zoneKey := types.NamespacedName{Name: record.Spec.ZoneRef.Name, Namespace: req.Namespace}
	if err := r.Get(ctx, zoneKey, zone); err != nil {
		msg := fmt.Sprintf("Zone %q not found: %v", record.Spec.ZoneRef.Name, err)
		reconciler.SetCondition(&record.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "ZoneNotFound", msg, record.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, record)
	}

	// The zone must be ready (have a zone ID) before we can create records.
	if !isZoneReady(zone) {
		reconciler.SetCondition(&record.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "ZoneNotReady",
			fmt.Sprintf("Zone %q is not ready", record.Spec.ZoneRef.Name),
			record.Generation)
		if statusErr := r.Status().Update(ctx, record); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		// Return an error so controller-runtime requeues with backoff.
		return ctrl.Result{}, fmt.Errorf("zone %q is not ready", record.Spec.ZoneRef.Name)
	}

	zoneID := zone.Status.CloudflareMetadata.ZoneID

	// Fetch the CloudflareAccount that the zone belongs to.
	account := &cloudflarev1alpha1.CloudflareAccount{}
	if err := r.Get(ctx, types.NamespacedName{Name: zone.Spec.AccountRef.Name}, account); err != nil {
		reconciler.SetCondition(&record.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "AccountNotFound",
			fmt.Sprintf("CloudflareAccount %q not found: %v", zone.Spec.AccountRef.Name, err),
			record.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, record)
	}

	// The account must be ready before we can use its credentials.
	if !isAccountReady(account) {
		reconciler.SetCondition(&record.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "AccountNotReady",
			fmt.Sprintf("CloudflareAccount %q is not ready", zone.Spec.AccountRef.Name),
			record.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, record)
	}

	// Fetch the API token from the secret referenced by the account.
	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{
		Name:      account.Spec.TokenSecretRef.Name,
		Namespace: account.Spec.TokenSecretRef.Namespace,
	}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		reconciler.SetCondition(&record.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "SecretNotFound",
			fmt.Sprintf("Secret %s/%s not found: %v", secretKey.Namespace, secretKey.Name, err),
			record.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, record)
	}

	tokenKey := account.Spec.TokenSecretRef.Key
	if tokenKey == "" {
		tokenKey = "CF_API_TOKEN"
	}
	tokenBytes, ok := secret.Data[tokenKey]
	if !ok {
		reconciler.SetCondition(&record.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "TokenKeyMissing",
			fmt.Sprintf("Key %q not found in secret %s/%s", tokenKey, secretKey.Namespace, secretKey.Name),
			record.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, record)
	}

	// Build the Cloudflare client.
	newAPI := r.NewDNSRecordAPI
	if newAPI == nil {
		newAPI = defaultDNSRecordAPI
	}
	cfAPI, err := newAPI(string(tokenBytes))
	if err != nil {
		reconciler.SetCondition(&record.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "InvalidToken",
			fmt.Sprintf("Failed to create Cloudflare client: %v", err),
			record.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, record)
	}

	return r.syncDNSRecord(ctx, logger, record, zoneID, cfAPI)
}

// syncDNSRecord drives the desired→observed→delta→reconcile loop for a DNSRecord.
func (r *DNSRecordReconciler) syncDNSRecord(
	ctx context.Context,
	logger interface {
		Info(msg string, keysAndValues ...interface{})
	},
	record *cloudflarev1alpha1.DNSRecord,
	zoneID string,
	cfAPI DNSRecordAPI,
) (ctrl.Result, error) {
	rc := cf.ZoneIdentifier(zoneID)
	var cfRecord cf.DNSRecord

	// If we already have a record ID, try to fetch the current state.
	if record.Status.CloudflareMetadata.RecordID != "" {
		got, err := cfAPI.GetDNSRecord(ctx, rc, record.Status.CloudflareMetadata.RecordID)
		if err != nil {
			if !cfpkg.IsNotFound(err) {
				return r.handleDNSCFError(ctx, record, err)
			}
			// Record was deleted externally — fall through to find or recreate it.
			logger.Info("DNS record deleted externally, recreating",
				"recordID", record.Status.CloudflareMetadata.RecordID)
		} else {
			cfRecord = got
		}
	}

	// If we don't have a record yet (no ID, or externally deleted), find or create.
	if cfRecord.ID == "" {
		records, _, listErr := cfAPI.ListDNSRecords(ctx, rc, cf.ListDNSRecordsParams{
			Name: record.Spec.Name,
			Type: record.Spec.Type,
		})
		if listErr != nil {
			return r.handleDNSCFError(ctx, record, listErr)
		}
		if len(records) > 0 {
			// Adopt the pre-existing record.
			cfRecord = records[0]
			logger.Info("Adopted existing DNS record",
				"name", record.Spec.Name, "type", record.Spec.Type, "recordID", cfRecord.ID)
		} else {
			// Create a brand-new record.
			params, err := buildCreateParams(record)
			if err != nil {
				return ctrl.Result{}, err
			}
			created, createErr := cfAPI.CreateDNSRecord(ctx, rc, params)
			if createErr != nil {
				return r.handleDNSCFError(ctx, record, createErr)
			}
			cfRecord = created
			logger.Info("Created DNS record",
				"name", record.Spec.Name, "type", record.Spec.Type, "recordID", cfRecord.ID)
		}
	}

	// Drift detection: compare desired spec against observed CF state.
	if drifted, params := driftDetect(record, cfRecord); drifted {
		params.ID = cfRecord.ID
		updated, updateErr := cfAPI.UpdateDNSRecord(ctx, rc, params)
		if updateErr != nil {
			return r.handleDNSCFError(ctx, record, updateErr)
		}
		cfRecord = updated
		logger.Info("Updated DNS record", "recordID", cfRecord.ID)
	}

	// Sync status from Cloudflare.
	record.Status.CloudflareMetadata.RecordID = cfRecord.ID
	record.Status.CloudflareMetadata.ZoneID = cfRecord.ZoneID
	record.Status.CloudflareMetadata.Proxiable = cfRecord.Proxiable
	reconciler.SetCondition(&record.Status.Conditions, cloudflarev1alpha1.ConditionReady,
		metav1.ConditionTrue, "Synced",
		"DNS record is synced with Cloudflare",
		record.Generation)
	return ctrl.Result{}, r.Status().Update(ctx, record)
}

// buildCreateParams constructs a CreateDNSRecordParams from the DNSRecord spec.
func buildCreateParams(record *cloudflarev1alpha1.DNSRecord) (cf.CreateDNSRecordParams, error) {
	params := cf.CreateDNSRecordParams{
		Type:     record.Spec.Type,
		Name:     record.Spec.Name,
		Content:  record.Spec.Content,
		TTL:      record.Spec.TTL,
		Proxied:  record.Spec.Proxied,
		Priority: record.Spec.Priority,
		Comment:  record.Spec.Comment,
		Tags:     record.Spec.Tags,
	}
	if record.Spec.Data != nil {
		var data interface{}
		if err := json.Unmarshal(record.Spec.Data.Raw, &data); err != nil {
			return cf.CreateDNSRecordParams{}, fmt.Errorf("invalid data field: %w", err)
		}
		params.Data = data
	}
	return params, nil
}

// driftDetect compares the desired DNSRecord spec against the observed CF record.
// Returns (true, params) if any field has drifted, (false, zero) otherwise.
// Note: content drift is skipped for SRV records because Cloudflare auto-formats
// the content field from the data fields, making a direct comparison unreliable.
func driftDetect(record *cloudflarev1alpha1.DNSRecord, cfRecord cf.DNSRecord) (bool, cf.UpdateDNSRecordParams) {
	drifted := false
	params := cf.UpdateDNSRecordParams{
		Type:     cfRecord.Type,
		Name:     cfRecord.Name,
		Content:  cfRecord.Content,
		TTL:      cfRecord.TTL,
		Proxied:  cfRecord.Proxied,
		Priority: cfRecord.Priority,
		Tags:     cfRecord.Tags,
	}
	if cfRecord.Tags == nil {
		params.Tags = []string{}
	}

	// Content: skip for SRV (CF auto-formats it from the data block).
	if record.Spec.Type != "SRV" && record.Spec.Content != cfRecord.Content {
		params.Content = record.Spec.Content
		drifted = true
	}

	// TTL.
	if record.Spec.TTL != cfRecord.TTL {
		params.TTL = record.Spec.TTL
		drifted = true
	}

	// Proxied (*bool comparison).
	if !boolPtrEqual(record.Spec.Proxied, cfRecord.Proxied) {
		params.Proxied = record.Spec.Proxied
		drifted = true
	}

	// Priority (*uint16 comparison).
	if !uint16PtrEqual(record.Spec.Priority, cfRecord.Priority) {
		params.Priority = record.Spec.Priority
		drifted = true
	}

	// Comment (*string in UpdateDNSRecordParams).
	if record.Spec.Comment != cfRecord.Comment {
		params.Comment = &record.Spec.Comment
		drifted = true
	}

	// Tags (sort both before comparing).
	if !tagsEqual(record.Spec.Tags, cfRecord.Tags) {
		params.Tags = record.Spec.Tags
		drifted = true
	}

	// Data (JSON round-trip compare).
	if dataDrifted(record, cfRecord) {
		var data interface{}
		if record.Spec.Data != nil {
			// Unmarshal is safe here; it was validated in buildCreateParams.
			_ = json.Unmarshal(record.Spec.Data.Raw, &data)
		}
		params.Data = data
		drifted = true
	}

	if !drifted {
		return false, cf.UpdateDNSRecordParams{}
	}
	return true, params
}

// handleDNSCFError sets the appropriate condition based on whether the error is
// terminal (stop requeuing) or retryable (let controller-runtime back off).
func (r *DNSRecordReconciler) handleDNSCFError(ctx context.Context, record *cloudflarev1alpha1.DNSRecord, err error) (ctrl.Result, error) {
	if cfpkg.IsTerminalError(err) {
		reconciler.SetCondition(&record.Status.Conditions, cloudflarev1alpha1.ConditionReady,
			metav1.ConditionFalse, "TerminalError",
			fmt.Sprintf("Terminal Cloudflare API error: %v", err),
			record.Generation)
		return ctrl.Result{}, r.Status().Update(ctx, record)
	}
	reconciler.SetCondition(&record.Status.Conditions, cloudflarev1alpha1.ConditionReady,
		metav1.ConditionFalse, "APIError",
		fmt.Sprintf("Cloudflare API error: %v", err),
		record.Generation)
	if statusErr := r.Status().Update(ctx, record); statusErr != nil {
		return ctrl.Result{}, statusErr
	}
	return ctrl.Result{}, err
}

// reconcileDelete handles the deletion lifecycle: optionally removes the DNS
// record from Cloudflare (unless the retain policy is set), then removes
// the finalizer.
func (r *DNSRecordReconciler) reconcileDelete(ctx context.Context, record *cloudflarev1alpha1.DNSRecord) (ctrl.Result, error) {
	// Safety check: if the finalizer is already gone, there is nothing to do.
	if !controllerutil.ContainsFinalizer(record, reconciler.Finalizer) {
		return ctrl.Result{}, nil
	}

	recordID := record.Status.CloudflareMetadata.RecordID
	policy := record.Annotations["cloudflare.k8s.io/deletion-policy"]

	if recordID != "" && policy != "retain" {
		// Resolve Zone → Account → Secret to call the Cloudflare API.
		zone := &cloudflarev1alpha1.Zone{}
		zoneKey := types.NamespacedName{Name: record.Spec.ZoneRef.Name, Namespace: record.Namespace}
		if err := r.Get(ctx, zoneKey, zone); err != nil {
			return ctrl.Result{}, err
		}

		account := &cloudflarev1alpha1.CloudflareAccount{}
		if err := r.Get(ctx, types.NamespacedName{Name: zone.Spec.AccountRef.Name}, account); err != nil {
			return ctrl.Result{}, err
		}

		secret := &corev1.Secret{}
		secretKey := types.NamespacedName{
			Name:      account.Spec.TokenSecretRef.Name,
			Namespace: account.Spec.TokenSecretRef.Namespace,
		}
		if err := r.Get(ctx, secretKey, secret); err != nil {
			return ctrl.Result{}, err
		}

		tokenKey := account.Spec.TokenSecretRef.Key
		if tokenKey == "" {
			tokenKey = "CF_API_TOKEN"
		}
		tokenBytes, ok := secret.Data[tokenKey]
		if !ok {
			return ctrl.Result{}, fmt.Errorf("key %q not found in secret %s/%s",
				tokenKey, secretKey.Namespace, secretKey.Name)
		}

		newAPI := r.NewDNSRecordAPI
		if newAPI == nil {
			newAPI = defaultDNSRecordAPI
		}
		cfAPI, err := newAPI(string(tokenBytes))
		if err != nil {
			return ctrl.Result{}, err
		}

		zoneID := zone.Status.CloudflareMetadata.ZoneID
		rc := cf.ZoneIdentifier(zoneID)
		if err := cfAPI.DeleteDNSRecord(ctx, rc, recordID); err != nil && !cfpkg.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	// Remove the finalizer to allow Kubernetes to complete the deletion.
	_, err := reconciler.RemoveFinalizer(ctx, r.Client, record)
	return ctrl.Result{}, err
}

// SetupWithManager registers DNSRecordReconciler with the manager and adds a
// watch on Zone so that a zone becoming ready automatically re-triggers
// reconciliation of all DNS records that reference it.
func (r *DNSRecordReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudflarev1alpha1.DNSRecord{}).
		Watches(
			&cloudflarev1alpha1.Zone{},
			handler.EnqueueRequestsFromMapFunc(r.recordsForZone),
		).
		Complete(r)
}

// recordsForZone maps a Zone event to reconcile.Requests for all DNSRecords
// that reference it (within the same namespace).
func (r *DNSRecordReconciler) recordsForZone(ctx context.Context, obj client.Object) []reconcile.Request {
	recordList := &cloudflarev1alpha1.DNSRecordList{}
	if err := r.List(ctx, recordList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, rec := range recordList.Items {
		if rec.Spec.ZoneRef.Name == obj.GetName() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: rec.Name, Namespace: rec.Namespace},
			})
		}
	}
	return reqs
}

// isZoneReady returns true if the Zone has a Ready=True condition and a zone ID.
func isZoneReady(zone *cloudflarev1alpha1.Zone) bool {
	if zone.Status.CloudflareMetadata.ZoneID == "" {
		return false
	}
	for _, c := range zone.Status.Conditions {
		if c.Type == cloudflarev1alpha1.ConditionReady {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

// defaultDNSRecordAPI is the production factory: it delegates to cfpkg.New so
// that the returned *cfpkg.Client (which embeds *cf.API) satisfies DNSRecordAPI.
func defaultDNSRecordAPI(token string) (DNSRecordAPI, error) {
	return cfpkg.New(token)
}

// boolPtrEqual returns true if both pointers point to equal values, or are both nil.
func boolPtrEqual(a, b *bool) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// uint16PtrEqual returns true if both pointers point to equal values, or are both nil.
func uint16PtrEqual(a, b *uint16) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// tagsEqual returns true if both tag slices contain the same elements (order-independent).
func tagsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := make([]string, len(a))
	bc := make([]string, len(b))
	copy(ac, a)
	copy(bc, b)
	sort.Strings(ac)
	sort.Strings(bc)
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	return true
}

// dataDrifted returns true if the spec's data field differs from the CF record's data.
// The spec's raw JSON and the CF record's interface{} are both unmarshalled into
// interface{} and compared with reflect.DeepEqual, avoiding JSON key-ordering issues.
func dataDrifted(record *cloudflarev1alpha1.DNSRecord, cfRecord cf.DNSRecord) bool {
	// Neither side has data — no drift.
	if record.Spec.Data == nil && cfRecord.Data == nil {
		return false
	}
	// One side has data, the other doesn't — drifted.
	if record.Spec.Data == nil || cfRecord.Data == nil {
		return true
	}
	// Both have data — unmarshal spec JSON into interface{} and deep-compare.
	var specData interface{}
	if err := json.Unmarshal(record.Spec.Data.Raw, &specData); err != nil {
		return true
	}
	return !reflect.DeepEqual(specData, cfRecord.Data)
}
