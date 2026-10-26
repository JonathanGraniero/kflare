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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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

//+kubebuilder:rbac:groups=kflare.dev,resources=dnsrecords,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kflare.dev,resources=dnsrecords/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=kflare.dev,resources=dnsrecords/finalizers,verbs=update
//+kubebuilder:rbac:groups=kflare.dev,resources=zones,verbs=get;list;watch
//+kubebuilder:rbac:groups=kflare.dev,resources=tunnels,verbs=get;list;watch

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
		return ctrl.Result{}, updateNotReady(ctx, r.Client, record, "ZoneNotFound", msg)
	}

	// The zone must be ready (have a zone ID) before we can create records.
	if !isZoneReady(zone) {
		// The Zone watch re-triggers this reconcile once the zone is ready.
		return ctrl.Result{}, updateNotReady(ctx, r.Client, record, "ZoneNotReady",
			fmt.Sprintf("Zone %q is not ready", record.Spec.ZoneRef.Name))
	}

	// A record that points at a Tunnel takes its content from the tunnel's
	// ID, so it waits for the Tunnel. The Tunnel watch re-triggers this
	// reconcile when the Tunnel changes, including when it gets a new ID.
	spec := record.Spec
	if record.Spec.TunnelRef != nil {
		tunnelID, condErr := r.recordTunnelID(ctx, record, zone)
		if condErr != nil {
			return ctrl.Result{}, updateNotReady(ctx, r.Client, record, condErr.Reason, condErr.Message)
		}
		spec = tunnelRecordSpec(record.Spec, tunnelID)
	}

	_, token, credErr := resolveAccountToken(ctx, r.Client, zone.Spec.AccountRef.Name, true)
	if credErr != nil {
		return notReadyRetryAfter(ctx, r.Client, record, credErr)
	}

	cfAPI, err := r.newDNSRecordAPI(token)
	if err != nil {
		return invalidToken(ctx, r.Client, record, err)
	}

	return r.syncDNSRecord(ctx, record, &spec, zone.Status.CloudflareMetadata.ZoneID, cfAPI)
}

// recordTunnelID returns the ID of the tunnel record points at through its
// tunnelRef, or the condition that keeps the record from pointing at it.
func (r *DNSRecordReconciler) recordTunnelID(
	ctx context.Context,
	record *cloudflarev1alpha1.DNSRecord,
	zone *cloudflarev1alpha1.Zone,
) (string, *conditionError) {
	tunnel := &cloudflarev1alpha1.Tunnel{}
	key := types.NamespacedName{Name: record.Spec.TunnelRef.Name, Namespace: record.Namespace}
	if err := r.Get(ctx, key, tunnel); err != nil {
		return "", &conditionError{
			Reason:  "TunnelNotFound",
			Message: fmt.Sprintf("Tunnel %q not found: %v", key.Name, err),
		}
	}
	if tunnel.Spec.AccountRef.Name != zone.Spec.AccountRef.Name {
		return "", &conditionError{
			Reason: reasonAccountMismatch,
			Message: fmt.Sprintf("Tunnel %q uses CloudflareAccount %q but Zone %q uses %q; "+
				"a tunnel carries traffic only for DNS records in its own account",
				tunnel.Name, tunnel.Spec.AccountRef.Name, zone.Name, zone.Spec.AccountRef.Name),
		}
	}
	if !isTunnelReady(tunnel) {
		return "", &conditionError{
			Reason:  "TunnelNotReady",
			Message: fmt.Sprintf("Tunnel %q is not ready", key.Name),
		}
	}
	return tunnel.Status.CloudflareMetadata.TunnelID, nil
}

// tunnelRecordSpec returns spec as kflare writes it for a record that points
// at the tunnel tunnelID: a CNAME to the tunnel's hostname, proxied, since
// an unproxied one resolves to an address the internet cannot reach
// (verified live).
func tunnelRecordSpec(spec cloudflarev1alpha1.DNSRecordSpec, tunnelID string) cloudflarev1alpha1.DNSRecordSpec {
	spec.Content = tunnelID + cloudflarev1alpha1.TunnelHostnameSuffix
	spec.Proxied = new(true)
	return spec
}

// syncDNSRecord drives the desired→observed→delta→reconcile loop for a
// DNSRecord. spec is the record to write: record's own spec, with the content
// and proxying a tunnelRef implies.
func (r *DNSRecordReconciler) syncDNSRecord(
	ctx context.Context,
	record *cloudflarev1alpha1.DNSRecord,
	spec *cloudflarev1alpha1.DNSRecordSpec,
	zoneID string,
	cfAPI DNSRecordAPI,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	rc := cf.ZoneIdentifier(zoneID)
	var cfRecord cf.DNSRecord

	// If we already have a record ID, try to fetch the current state.
	if record.Status.CloudflareMetadata.RecordID != "" {
		got, err := cfAPI.GetDNSRecord(ctx, rc, record.Status.CloudflareMetadata.RecordID)
		if err != nil {
			if !cfpkg.IsNotFound(err) {
				return handleCloudflareError(ctx, r.Client, record, err)
			}
			// Record was deleted externally — fall through to find or recreate it.
			logger.Info("DNS record deleted externally, recreating",
				"recordID", record.Status.CloudflareMetadata.RecordID)
		} else {
			cfRecord = got
		}
	}

	// If we don't have a record yet (no ID, or externally deleted), find this
	// resource's own record among those with the same name and type (an
	// earlier reconcile did not record it in status), else one to adopt as
	// the adoption policy allows, else create one, unless the policy is adopt.
	adopting := false
	if cfRecord.ID == "" {
		records, _, listErr := cfAPI.ListDNSRecords(ctx, rc, cf.ListDNSRecordsParams{
			Name: record.Spec.Name,
			Type: record.Spec.Type,
		})
		if listErr != nil {
			return handleCloudflareError(ctx, r.Client, record, listErr)
		}
		claimed, err := claimedIDs(ctx, r.Client, &cloudflarev1alpha1.DNSRecordList{}, cloudflarev1alpha1.DNSRecordIDLabel, record)
		if err != nil {
			return ctrl.Result{}, err
		}
		// Only an adoption policy lets the only record of the name and type
		// be taken for this one. An unknown policy is reported below, by
		// mayAdopt or mayCreate.
		policy, _ := adoptionPolicy(record)
		key := dnsRecordKey(record)
		what := fmt.Sprintf("DNS record %s", key)
		if existing, ok := ownRecord(record, spec, records, claimed); ok {
			cfRecord = existing
			logger.Info("Found the DNS record this resource created",
				"name", record.Spec.Name, "type", record.Spec.Type, "recordID", cfRecord.ID)
		} else if existing, ok := adoptableRecord(spec, records, claimed, policy != ""); ok {
			if condErr := mayAdopt(record, what); condErr != nil {
				return notReadyRetryAfter(ctx, r.Client, record, condErr)
			}
			cfRecord, adopting = existing, true
			logger.Info("Adopted existing DNS record",
				"name", record.Spec.Name, "type", record.Spec.Type, "recordID", cfRecord.ID)
		} else {
			if condErr := mayCreate(record, what); condErr != nil {
				return notReadyRetryAfter(ctx, r.Client, record, condErr)
			}
			if err := markCreating(ctx, r.Client, record, key); err != nil {
				return ctrl.Result{}, err
			}
			// Create a brand-new record.
			params, err := buildCreateParams(spec)
			if err != nil {
				return ctrl.Result{}, err
			}
			created, createErr := cfAPI.CreateDNSRecord(ctx, rc, params)
			if createErr != nil {
				return handleCloudflareError(ctx, r.Client, record, createErr)
			}
			cfRecord = created
			logger.Info("Created DNS record",
				"name", record.Spec.Name, "type", record.Spec.Type, "recordID", cfRecord.ID)
		}
	}

	// Claim the record before anything else can fail, so no other DNSRecord
	// adopts it in the meantime.
	if err := claimID(ctx, r.Client, record, cloudflarev1alpha1.DNSRecordIDLabel, cfRecord.ID, adopting); err != nil {
		return ctrl.Result{}, err
	}

	// Drift detection: compare desired spec against observed CF state.
	if drifted, params := driftDetect(spec, cfRecord); drifted {
		params.ID = cfRecord.ID
		updated, updateErr := cfAPI.UpdateDNSRecord(ctx, rc, params)
		if updateErr != nil {
			return handleCloudflareError(ctx, r.Client, record, updateErr)
		}
		cfRecord = updated
		logger.Info("Updated DNS record", "recordID", cfRecord.ID)
	}

	// Sync status from Cloudflare.
	record.Status.CloudflareMetadata.RecordID = cfRecord.ID
	record.Status.CloudflareMetadata.ZoneID = zoneID
	record.Status.CloudflareMetadata.Proxiable = cfRecord.Proxiable
	return readyWithDriftCheck(ctx, r.Client, record, "Synced", "DNS record is synced with Cloudflare")
}

// dnsRecordKey is the key of record's creating annotation: its type and name,
// shared by the members of a record set, which ownRecord tells apart by
// content.
func dnsRecordKey(record *cloudflarev1alpha1.DNSRecord) string {
	return record.Spec.Type + " " + record.Spec.Name
}

// ownRecord returns the record among candidates (same name and type) that
// record created or adopted without recording it in status: the one its
// claim label names, or, while it was creating one, the one whose content (or
// data) matches spec, the record it writes. Records another DNSRecord manages
// are never its own.
func ownRecord(
	record *cloudflarev1alpha1.DNSRecord,
	spec *cloudflarev1alpha1.DNSRecordSpec,
	candidates []cf.DNSRecord,
	claimed map[string]string,
) (cf.DNSRecord, bool) {
	for _, c := range candidates {
		if _, taken := claimed[c.ID]; taken {
			continue
		}
		if ownsID(record, cloudflarev1alpha1.DNSRecordIDLabel, c.ID) ||
			(creating(record, dnsRecordKey(record)) && recordContentMatches(spec, c)) {
			return c, true
		}
	}
	return cf.DNSRecord{}, false
}

// adoptableRecord picks the existing Cloudflare record from candidates (same
// name and type) that is the one spec describes, or returns false when
// there is none and a new record joins them.
//
// Records another DNSRecord manages are never adopted. Among the rest, one
// whose content (or data) already matches is the one, so DNSRecords for the
// members of a set (several MX, TXT or A records) each find their own. When
// lone is true, a record that does not match is the one, to be adopted and
// corrected, when it is the only record with that name and type. Only an
// adoption policy makes it so: without one, an existing record whose content
// differs is left as it is, a separate member of the set.
func adoptableRecord(
	spec *cloudflarev1alpha1.DNSRecordSpec,
	candidates []cf.DNSRecord,
	claimed map[string]string,
	lone bool,
) (cf.DNSRecord, bool) {
	isClaimed := func(id string) bool { _, taken := claimed[id]; return taken }
	for _, c := range candidates {
		if !isClaimed(c.ID) && recordContentMatches(spec, c) {
			return c, true
		}
	}
	if lone && len(candidates) == 1 && !isClaimed(candidates[0].ID) {
		return candidates[0], true
	}
	return cf.DNSRecord{}, false
}

// recordContentMatches reports whether cfRecord holds the value spec
// describes: its data when data is set, otherwise its content.
func recordContentMatches(spec *cloudflarev1alpha1.DNSRecordSpec, cfRecord cf.DNSRecord) bool {
	if spec.Data != nil {
		return !dataDrifted(spec, cfRecord)
	}
	return spec.Content == cfRecord.Content
}

// automaticTTL is the TTL Cloudflare reports for "automatic". It is also what
// Cloudflare stores when a record is created without a TTL.
const automaticTTL = 1

// buildCreateParams constructs a CreateDNSRecordParams from the desired spec.
func buildCreateParams(spec *cloudflarev1alpha1.DNSRecordSpec) (cf.CreateDNSRecordParams, error) {
	params := cf.CreateDNSRecordParams{
		Type:     spec.Type,
		Name:     spec.Name,
		Content:  spec.Content,
		TTL:      desiredTTL(spec),
		Proxied:  spec.Proxied,
		Priority: spec.Priority,
		Comment:  spec.Comment,
		Tags:     spec.Tags,
	}
	if spec.Data != nil {
		var data any
		if err := json.Unmarshal(spec.Data.Raw, &data); err != nil {
			return cf.CreateDNSRecordParams{}, fmt.Errorf("invalid data field: %w", err)
		}
		params.Data = data
	}
	return params, nil
}

// desiredTTL returns spec.ttl, or automatic when it is unset.
func desiredTTL(spec *cloudflarev1alpha1.DNSRecordSpec) int {
	if spec.TTL == 0 {
		return automaticTTL
	}
	return spec.TTL
}

// driftDetect compares the desired spec against the observed CF record.
// Returns (true, params) if any field has drifted, (false, zero) otherwise.
//
// Unset spec fields are compared against Cloudflare's defaults, because
// Cloudflare always reports a value for them: an unset ttl means automatic (1)
// and an unset proxied means false. An unset priority is left to Cloudflare.
//
// Content is not compared when spec.data is set, or for SRV records, because
// Cloudflare derives the content from the data fields.
func driftDetect(spec *cloudflarev1alpha1.DNSRecordSpec, cfRecord cf.DNSRecord) (bool, cf.UpdateDNSRecordParams) {
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
	// Tags are always sent (the field has no omitempty), so never send null.
	if params.Tags == nil {
		params.Tags = []string{}
	}

	// Content: skipped when Cloudflare derives it from the data block.
	contentFromData := spec.Data != nil || spec.Type == "SRV"
	if !contentFromData && spec.Content != cfRecord.Content {
		params.Content = spec.Content
		drifted = true
	}

	// TTL.
	if ttl := desiredTTL(spec); ttl != cfRecord.TTL {
		params.TTL = ttl
		drifted = true
	}

	// Proxied. The update is a PATCH that omits a nil value, so the desired
	// value is always sent explicitly.
	proxied := spec.Proxied != nil && *spec.Proxied
	if proxied != (cfRecord.Proxied != nil && *cfRecord.Proxied) {
		params.Proxied = &proxied
		drifted = true
	}

	// Priority: only managed when the spec sets it.
	if p := spec.Priority; p != nil && (cfRecord.Priority == nil || *cfRecord.Priority != *p) {
		params.Priority = p
		drifted = true
	}

	// Comment (*string in UpdateDNSRecordParams).
	if spec.Comment != cfRecord.Comment {
		params.Comment = &spec.Comment
		drifted = true
	}

	// Tags (sort both before comparing).
	if !tagsEqual(spec.Tags, cfRecord.Tags) {
		params.Tags = append([]string{}, spec.Tags...)
		drifted = true
	}

	// Data (JSON round-trip compare).
	if dataDrifted(spec, cfRecord) {
		var data any
		if spec.Data != nil {
			// Unmarshal is safe here; it was validated in buildCreateParams.
			_ = json.Unmarshal(spec.Data.Raw, &data)
		}
		params.Data = data
		drifted = true
	}

	if !drifted {
		return false, cf.UpdateDNSRecordParams{}
	}
	return true, params
}

// reconcileDelete handles the deletion lifecycle: optionally removes the DNS
// record from Cloudflare (unless the retain policy is set), then removes
// the finalizer.
//
// The Zone waits for its records before it is deleted, so it is normally
// still there. If it is gone anyway (its finalizer was removed by hand),
// there are no credentials to delete with and the record is left in
// Cloudflare.
func (r *DNSRecordReconciler) reconcileDelete(ctx context.Context, record *cloudflarev1alpha1.DNSRecord) (ctrl.Result, error) {
	// Safety check: if the finalizer is already gone, there is nothing to do.
	if !controllerutil.ContainsFinalizer(record, reconciler.Finalizer) {
		return ctrl.Result{}, nil
	}

	recordID := record.Status.CloudflareMetadata.RecordID
	if recordID != "" && !reconciler.RetainOnDelete(record) {
		if err := r.deleteCloudflareRecord(ctx, record, recordID); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Remove the finalizer to allow Kubernetes to complete the deletion.
	_, err := reconciler.RemoveFinalizer(ctx, r.Client, record)
	return ctrl.Result{}, err
}

// deleteCloudflareRecord deletes recordID from the zone of record's Zone
// resource, resolving Zone → Account → Secret for credentials.
func (r *DNSRecordReconciler) deleteCloudflareRecord(
	ctx context.Context,
	record *cloudflarev1alpha1.DNSRecord,
	recordID string,
) error {
	zone := &cloudflarev1alpha1.Zone{}
	zoneKey := types.NamespacedName{Name: record.Spec.ZoneRef.Name, Namespace: record.Namespace}
	if err := r.Get(ctx, zoneKey, zone); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).Info("Zone is gone, skipping Cloudflare deletion",
				"zone", zoneKey.Name, "recordID", recordID)
			return nil
		}
		return err
	}

	_, token, credErr := resolveAccountToken(ctx, r.Client, zone.Spec.AccountRef.Name, false)
	if credErr != nil {
		return credErr
	}
	cfAPI, err := r.newDNSRecordAPI(token)
	if err != nil {
		return err
	}

	rc := cf.ZoneIdentifier(zone.Status.CloudflareMetadata.ZoneID)
	if err := cfAPI.DeleteDNSRecord(ctx, rc, recordID); err != nil && !cfpkg.IsNotFound(err) {
		return err
	}
	return nil
}

// SetupWithManager registers DNSRecordReconciler with the manager and adds a
// watch on Zone so that a zone becoming ready automatically re-triggers
// reconciliation of all DNS records that reference it. It watches Tunnels
// likewise, so a record follows its Tunnel when it becomes ready or is
// recreated with a new ID.
func (r *DNSRecordReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&cloudflarev1alpha1.DNSRecord{}).
		Watches(
			&cloudflarev1alpha1.Zone{},
			handler.EnqueueRequestsFromMapFunc(r.recordsForZone),
		).
		Watches(
			&cloudflarev1alpha1.Tunnel{},
			handler.EnqueueRequestsFromMapFunc(r.recordsForTunnel),
		).
		Complete(r)
}

// recordsForTunnel maps a Tunnel event to reconcile.Requests for the
// DNSRecords in its namespace whose tunnelRef names it.
func (r *DNSRecordReconciler) recordsForTunnel(ctx context.Context, obj client.Object) []reconcile.Request {
	recordList := &cloudflarev1alpha1.DNSRecordList{}
	if err := r.List(ctx, recordList, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, rec := range recordList.Items {
		if rec.Spec.TunnelRef != nil && rec.Spec.TunnelRef.Name == obj.GetName() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: rec.Name, Namespace: rec.Namespace},
			})
		}
	}
	return reqs
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
	return zone.Status.CloudflareMetadata.ZoneID != "" &&
		meta.IsStatusConditionTrue(zone.Status.Conditions, cloudflarev1alpha1.ConditionReady)
}

// newDNSRecordAPI builds a DNSRecordAPI with the injected factory, falling
// back to the production client.
func (r *DNSRecordReconciler) newDNSRecordAPI(token string) (DNSRecordAPI, error) {
	if r.NewDNSRecordAPI != nil {
		return r.NewDNSRecordAPI(token)
	}
	return defaultDNSRecordAPI(token)
}

// defaultDNSRecordAPI is the production factory: it delegates to cfpkg.New so
// that the returned *cfpkg.Client (which embeds *cf.API) satisfies DNSRecordAPI.
func defaultDNSRecordAPI(token string) (DNSRecordAPI, error) {
	return cfpkg.New(token)
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

// dataDrifted returns true if spec's data field differs from the CF record's data.
// The spec's raw JSON and the CF record's interface{} are both unmarshalled into
// interface{} and compared with reflect.DeepEqual, avoiding JSON key-ordering issues.
func dataDrifted(spec *cloudflarev1alpha1.DNSRecordSpec, cfRecord cf.DNSRecord) bool {
	// Neither side has data — no drift.
	if spec.Data == nil && cfRecord.Data == nil {
		return false
	}
	// One side has data, the other doesn't — drifted.
	if spec.Data == nil || cfRecord.Data == nil {
		return true
	}
	// Both have data — unmarshal spec JSON into interface{} and deep-compare.
	var specData any
	if err := json.Unmarshal(spec.Data.Raw, &specData); err != nil {
		return true
	}
	return !reflect.DeepEqual(specData, cfRecord.Data)
}
