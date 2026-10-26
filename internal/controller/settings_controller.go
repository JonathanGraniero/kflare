/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// A settings kind's resources each configure one product's settings in a
// zone, such as its managed transforms. The settings always exist, so a
// resource neither creates nor deletes them: it writes the values its spec
// sets, and resets them to Cloudflare's defaults when it is deleted. kflare-gen
// generates every settings kind, supplying its settingsKind, and they all
// reconcile the same way:
//
//   - One resource configures a zone's settings of a kind, of those whose Zones
//     point at the zone, in any namespace: the one that configures them
//     already, whose status records the zone, or else the oldest (by creation
//     time, then namespace and name). The others report AlreadyConfigured, and
//     check again every minute and when one of them is deleted.
//   - Settings that hold another value than a zone nobody has configured
//     (settingsKind.unconfigured) were configured outside kflare: a resource
//     takes them over only as its adoption policy allows (adoption.go), and
//     leaves them as they are when it is deleted unless its deletion policy
//     says delete. Otherwise it writes them, having recorded the zone in its
//     creating annotation, so that a reconcile that wrote them but failed to
//     record it does not take them for someone else's.
//   - Drift is a value the spec sets that Cloudflare holds otherwise. What the
//     spec leaves out is not compared (settingsShape.matches), so kflare never
//     fights Cloudflare over a value it does not manage.
//   - Deleting the resource that configures the settings resets them, unless
//     its deletion policy is retain or it adopted them.

// settingsConflictRetryInterval is how long a resource that does not
// configure its zone's settings waits before checking whether it can.
const settingsConflictRetryInterval = time.Minute

// settingsObject is a kflare resource that configures one product's settings
// in a zone. Every settings kind's API type implements it.
type settingsObject interface {
	conditionedObject
	GetSettingsSpec() *cloudflarev1alpha1.SettingsSpec
	GetSettingsStatus() *cloudflarev1alpha1.SettingsStatus
}

// settingsKind is what the shared settings reconciler needs to know about a
// kind. A is the narrow interface to the cloudflare-go v7 service it calls.
type settingsKind[A any] struct {
	// name is the kind's name, and noun what messages call its settings, as
	// in "ManagedTransform" and "managed transforms".
	name, noun string
	newObject  func() settingsObject
	newList    func() client.ObjectList

	// The kind's calls to Cloudflare, in the zone zoneID: read returns the
	// zone's settings as JSON, as Cloudflare returned them; write writes
	// those obj asks for; reset restores Cloudflare's defaults.
	read  func(ctx context.Context, api A, zoneID string) (string, error)
	write func(ctx context.Context, api A, zoneID string, obj settingsObject) error
	reset func(ctx context.Context, api A, zoneID string) error
	// desired returns the settings obj asks for, as the JSON write sends.
	desired func(obj settingsObject) ([]byte, error)

	// shape is what comparing the settings needs to know of them, and
	// unconfigured the values they hold in a zone nobody has configured: a
	// new zone's, or one reset.
	shape        settingsShape
	unconfigured []settingsValue
}

// settingsReconciler reconciles the resources of one settings kind. Each
// kind's exported reconciler delegates to one.
type settingsReconciler[A any] struct {
	client.Client
	kind   settingsKind[A]
	newAPI func(token string) (A, error)
}

func (r *settingsReconciler[A]) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	obj := r.kind.newObject()
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion before anything else.
	if !obj.GetDeletionTimestamp().IsZero() {
		return r.reconcileDelete(ctx, obj)
	}

	// Ensure the finalizer is present before doing any work. On the first
	// reconcile it is absent: add it and return; the update re-triggers this
	// reconcile.
	added, err := reconciler.EnsureFinalizer(ctx, r.Client, obj)
	if err != nil {
		return ctrl.Result{}, err
	}
	if added {
		logger.Info("Added finalizer", "resource", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	// The Zone watch re-triggers this reconcile when the Zone appears or
	// becomes ready.
	zone := &cloudflarev1alpha1.Zone{}
	zoneKey := types.NamespacedName{Name: obj.GetSettingsSpec().ZoneRef.Name, Namespace: obj.GetNamespace()}
	if err := r.Get(ctx, zoneKey, zone); err != nil {
		return ctrl.Result{}, updateNotReady(ctx, r.Client, obj, "ZoneNotFound",
			fmt.Sprintf("Zone %q not found: %v", zoneKey.Name, err))
	}
	if !isZoneReady(zone) {
		return ctrl.Result{}, updateNotReady(ctx, r.Client, obj, "ZoneNotReady",
			fmt.Sprintf("Zone %q is not ready", zoneKey.Name))
	}

	// Only one resource of the kind configures a zone's settings. Another
	// forgets the zone and any write it was about to make, so that it checks
	// the settings again before it writes them, once it is the one.
	zoneID := zone.Status.CloudflareMetadata.ZoneID
	owner, err := r.owner(ctx, obj, zoneID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if owner != obj {
		if err := clearCreating(ctx, r.Client, obj); err != nil {
			return ctrl.Result{}, err
		}
		obj.GetSettingsStatus().CloudflareMetadata = cloudflarev1alpha1.SettingsCloudflareMetadata{}
		return ctrl.Result{RequeueAfter: settingsConflictRetryInterval}, updateNotReady(ctx, r.Client, obj,
			"AlreadyConfigured", fmt.Sprintf("%s %s/%s configures the %s of zone %s", r.kind.name,
				owner.GetNamespace(), owner.GetName(), r.kind.noun, zoneID))
	}

	_, token, credErr := resolveAccountToken(ctx, r.Client, zone.Spec.AccountRef.Name, true)
	if credErr != nil {
		return notReadyRetryAfter(ctx, r.Client, obj, credErr)
	}
	api, err := r.newAPI(token)
	if err != nil {
		return invalidToken(ctx, r.Client, obj, err)
	}

	return r.sync(ctx, obj, zoneID, api)
}

// sync drives the desired→observed→delta→reconcile loop for one resource: it
// reads the zone's settings and writes those the spec sets only when they
// differ.
func (r *settingsReconciler[A]) sync(ctx context.Context, obj settingsObject, zoneID string, api A) (ctrl.Result,
	error) {
	logger := log.FromContext(ctx)
	k := r.kind

	current, err := k.read(ctx, api, zoneID)
	if err != nil {
		return handleCloudflareError(ctx, r.Client, obj, err)
	}

	// Settings this resource has not written yet (status names another zone,
	// or none) were configured outside kflare when they hold another value
	// than a zone nobody has configured: taking them over takes the adoption
	// policy, unless this resource wrote them and did not record that.
	// Otherwise the resource writes its own, unless the policy is adopt.
	recorded := obj.GetSettingsStatus().CloudflareMetadata.ZoneID == zoneID
	adopting := false
	if !recorded {
		configured, err := settingsConfigured([]byte(current), k.unconfigured)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("reading the %s of zone %s: %w", k.noun, zoneID, err)
		}
		what := fmt.Sprintf("A configuration of the %s of zone %s", k.noun, zoneID)
		switch {
		case !configured:
			if condErr := mayCreate(obj, what); condErr != nil {
				return notReadyRetryAfter(ctx, r.Client, obj, condErr)
			}
			if err := markCreating(ctx, r.Client, obj, zoneID); err != nil {
				return ctrl.Result{}, err
			}
		case !creating(obj, zoneID):
			if condErr := mayAdopt(obj, what); condErr != nil {
				return notReadyRetryAfter(ctx, r.Client, obj, condErr)
			}
			adopting = true
			logger.Info("Adopted the existing configuration of the "+k.noun, "zoneID", zoneID)
		}
	}

	// Record an adoption before writing the settings; the patch re-reads the
	// resource, so it comes before status is set. Once status records the
	// zone, the creating annotation can go.
	switch {
	case adopting:
		if err := markAdopted(ctx, r.Client, obj); err != nil {
			return ctrl.Result{}, err
		}
	case recorded:
		if err := clearCreating(ctx, r.Client, obj); err != nil {
			return ctrl.Result{}, err
		}
	}

	desired, err := k.desired(obj)
	if err != nil {
		return ctrl.Result{}, err
	}
	matches, err := k.shape.matches(desired, []byte(current))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("comparing the %s of zone %s: %w", k.noun, zoneID, err)
	}
	if !matches {
		if err := k.write(ctx, api, zoneID, obj); err != nil {
			return handleCloudflareError(ctx, r.Client, obj, err)
		}
		logger.Info("Wrote the "+k.noun, "zoneID", zoneID)
	}

	obj.GetSettingsStatus().CloudflareMetadata = cloudflarev1alpha1.SettingsCloudflareMetadata{ZoneID: zoneID}
	return readyWithDriftCheck(ctx, r.Client, obj, "Synced",
		fmt.Sprintf("The configuration of the %s is synced with Cloudflare", k.noun))
}

// owner returns the resource of the kind that configures the settings of
// zoneID, obj or another, of those whose Zones point at the zone, in any
// namespace, since Zones in different namespaces can point at the same
// Cloudflare zone: the one whose status records the zone, which keeps it even
// from an older one whose Zone becomes ready later, or else the oldest. When
// several record it, as two resources that synced at once may, the oldest of
// those. A resource being deleted keeps the settings until it is gone, so that
// it cannot reset what another one just wrote.
func (r *settingsReconciler[A]) owner(ctx context.Context, obj settingsObject, zoneID string) (settingsObject,
	error) {
	list := r.kind.newList()
	if err := r.List(ctx, list); err != nil {
		return nil, err
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return nil, err
	}
	owner := obj
	zoneIDs := map[types.NamespacedName]string{}
	for _, item := range items {
		other, ok := item.(settingsObject)
		if !ok {
			return nil, fmt.Errorf("unexpected %T in a %s list", item, r.kind.name)
		}
		// The list's copy of obj may be older than obj, which is the one to go
		// by.
		if client.ObjectKeyFromObject(other) == client.ObjectKeyFromObject(obj) ||
			!settingsPrecede(other, owner, zoneID) {
			continue
		}
		key := types.NamespacedName{Name: other.GetSettingsSpec().ZoneRef.Name, Namespace: other.GetNamespace()}
		id, seen := zoneIDs[key]
		if !seen {
			zone := &cloudflarev1alpha1.Zone{}
			if err := r.Get(ctx, key, zone); err != nil && !apierrors.IsNotFound(err) {
				return nil, err
			}
			id = zone.Status.CloudflareMetadata.ZoneID
			zoneIDs[key] = id
		}
		if id == zoneID {
			owner = other
		}
	}
	return owner, nil
}

// settingsPrecede reports whether a configures the settings of zoneID before
// b: a resource whose status records the zone comes before one whose status
// does not, then the older one.
func settingsPrecede(a, b settingsObject, zoneID string) bool {
	aRecords := a.GetSettingsStatus().CloudflareMetadata.ZoneID == zoneID
	bRecords := b.GetSettingsStatus().CloudflareMetadata.ZoneID == zoneID
	if aRecords != bRecords {
		return aRecords
	}
	return settingsOlder(a, b)
}

// settingsOlder orders resources by creation time, then by namespace and
// name.
func settingsOlder(a, b client.Object) bool {
	at, bt := a.GetCreationTimestamp(), b.GetCreationTimestamp()
	if !at.Equal(&bt) {
		return at.Before(&bt)
	}
	if a.GetNamespace() != b.GetNamespace() {
		return a.GetNamespace() < b.GetNamespace()
	}
	return a.GetName() < b.GetName()
}

// reconcileDelete handles the deletion lifecycle: unless the settings are
// retained (the retain policy, or settings this resource adopted), it resets
// them, then removes the finalizer.
func (r *settingsReconciler[A]) reconcileDelete(ctx context.Context, obj settingsObject) (ctrl.Result, error) {
	// Safety check: if the finalizer is already gone, there is nothing to do.
	if !controllerutil.ContainsFinalizer(obj, reconciler.Finalizer) {
		return ctrl.Result{}, nil
	}

	if !reconciler.RetainOnDelete(obj) {
		if err := r.resetSettings(ctx, obj); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Remove the finalizer to allow Kubernetes to complete the deletion.
	_, err := reconciler.RemoveFinalizer(ctx, r.Client, obj)
	return ctrl.Result{}, err
}

// resetSettings restores Cloudflare's defaults in the zone whose settings obj
// configures, when it still configures them: the zone of its last sync, or,
// without one, the zone its creating annotation names, whose settings a sync
// may have written before failing to record them.
//
// The Zone waits for its resources before it is deleted, so it is normally
// still there. If it is gone anyway (its finalizer was removed by hand), there
// are no credentials to reset with and the settings stay.
func (r *settingsReconciler[A]) resetSettings(ctx context.Context, obj settingsObject) error {
	logger := log.FromContext(ctx)
	zoneID := obj.GetSettingsStatus().CloudflareMetadata.ZoneID
	if zoneID == "" {
		zoneID = obj.GetAnnotations()[reconciler.CreatingAnnotation]
	}
	if zoneID == "" {
		return nil
	}

	zone := &cloudflarev1alpha1.Zone{}
	zoneKey := types.NamespacedName{Name: obj.GetSettingsSpec().ZoneRef.Name, Namespace: obj.GetNamespace()}
	if err := r.Get(ctx, zoneKey, zone); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("Zone is gone, leaving the "+r.kind.noun+" as they are", "zone", zoneKey.Name)
			return nil
		}
		return err
	}
	owner, err := r.owner(ctx, obj, zoneID)
	if err != nil {
		return err
	}
	if owner != obj {
		return nil
	}

	_, token, credErr := resolveAccountToken(ctx, r.Client, zone.Spec.AccountRef.Name, false)
	if credErr != nil {
		return credErr
	}
	api, err := r.newAPI(token)
	if err != nil {
		return err
	}
	if err := r.kind.reset(ctx, api, zoneID); err != nil && !cfpkg.IsNotFound(err) {
		return err
	}
	logger.Info("Reset the "+r.kind.noun, "zoneID", zoneID)
	return nil
}

// setup registers the kind's controller, with rec as its reconciler. It
// watches Zones, so a resource waiting for its Zone is reconciled once the
// Zone is ready, and the deletion of the kind's resources, so that another
// resource takes the deleted one's settings over at once.
func (r *settingsReconciler[A]) setup(mgr ctrl.Manager, rec reconcile.Reconciler) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(r.kind.newObject()).
		Watches(&cloudflarev1alpha1.Zone{}, handler.EnqueueRequestsFromMapFunc(r.resourcesForZone)).
		Watches(r.kind.newObject(), handler.EnqueueRequestsFromMapFunc(r.othersInZoneOf),
			builder.WithPredicates(deletesOnly)).
		Complete(rec)
}

// resourcesForZone maps a Zone event to the kind's resources that use it.
func (r *settingsReconciler[A]) resourcesForZone(ctx context.Context, zone client.Object) []reconcile.Request {
	return r.resourcesWhere(ctx, []client.ListOption{client.InNamespace(zone.GetNamespace())},
		func(obj settingsObject) bool { return obj.GetSettingsSpec().ZoneRef.Name == zone.GetName() })
}

// othersInZoneOf maps the deletion of a resource of the kind to the others
// that may configure its zone's settings now: those last synced to the same
// Cloudflare zone, and those not synced, which include every one that
// reported AlreadyConfigured.
func (r *settingsReconciler[A]) othersInZoneOf(ctx context.Context, deleted client.Object) []reconcile.Request {
	resource, ok := deleted.(settingsObject)
	if !ok {
		return nil
	}
	zoneID := resource.GetSettingsStatus().CloudflareMetadata.ZoneID
	return r.resourcesWhere(ctx, nil, func(obj settingsObject) bool {
		synced := obj.GetSettingsStatus().CloudflareMetadata.ZoneID
		return zoneID == "" || synced == "" || synced == zoneID
	})
}

// resourcesWhere returns a request for each of the kind's resources, listed
// with opts, that match accepts.
func (r *settingsReconciler[A]) resourcesWhere(ctx context.Context, opts []client.ListOption,
	match func(settingsObject) bool) []reconcile.Request {
	list := r.kind.newList()
	if err := r.List(ctx, list, opts...); err != nil {
		return nil
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, item := range items {
		if obj, ok := item.(settingsObject); ok && match(obj) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(obj)})
		}
	}
	return reqs
}
