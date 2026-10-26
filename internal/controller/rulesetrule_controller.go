/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/cloudflare/cloudflare-go/v7/rulesets"
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

// Every rule kind (WAFCustomRule, RateLimitRule, ManagedRuleset, and the kinds
// kflare-gen generates) reconciles the same way: wait for the Zone, resolve
// the account's token, write the one rule the resource manages with
// syncRulesetRule (rulesetrule.go), and delete it with the resource. The
// kind's own controller supplies only what differs: its phase and how its
// spec becomes a Cloudflare rule.

// rulesetRuleRecheckInterval is how soon a resource that reordered rules
// checks the order again: another resource may have been reordering the same
// rules at the same time, from an older view of the cluster.
const rulesetRuleRecheckInterval = 10 * time.Second

// rulesetRuleObject is a kflare resource that manages one rule in a zone's
// Rulesets phase. Every rule kind's API type implements it.
type rulesetRuleObject interface {
	conditionedObject
	GetRulesetRuleSpec() *cloudflarev1alpha1.RulesetRuleSpec
	GetRulesetRuleStatus() *cloudflarev1alpha1.RulesetRuleStatus
}

// rulesetRuleKind is what the shared reconciler needs to know about a rule
// kind.
type rulesetRuleKind struct {
	// name is the kind's name. It is part of the ref of every rule of the kind.
	name string
	// phases are the Rulesets phases the kind's rules are in. A kind with
	// several picks each resource's from its spec with phaseFor, from a field
	// the CRD keeps immutable.
	phases    []rulesets.Phase
	phaseFor  func(rulesetRuleObject) rulesets.Phase
	newObject func() rulesetRuleObject
	newList   func() client.ObjectList
	// definition returns the rule a resource of the kind asks for, without
	// its ref. It sets every field the rule should have: an update replaces
	// the whole rule. rulesetRuleDefinition starts it.
	definition func(rulesetRuleObject) rulesets.RulesetNewParamsRule
	// conflicts maps the Cloudflare error codes that mean another rule holds
	// something the kind's rule needs to the condition reason to report. A
	// resource that gets one checks again every minute instead of failing:
	// the other rule may be about to go.
	conflicts map[int]string
}

// phase returns the phase obj's rule is in: the kind's only phase, or the one
// its spec picks.
func (k rulesetRuleKind) phase(obj rulesetRuleObject) rulesets.Phase {
	if k.phaseFor == nil {
		return k.phases[0]
	}
	return k.phaseFor(obj)
}

// conflict returns the condition reason for err when it carries one of the
// kind's conflict codes.
func (k rulesetRuleKind) conflict(err error) (string, bool) {
	for _, code := range slices.Sorted(maps.Keys(k.conflicts)) {
		if cfpkg.HasErrorCode(err, code) {
			return k.conflicts[code], true
		}
	}
	return "", false
}

// rulesetRuleKinds lists every rule kind: the hand-written ones and those
// kflare-gen generates. The rules of all the kinds that share a phase are
// kept in one order (see syncRulesetRule).
var rulesetRuleKinds = append([]rulesetRuleKind{wafCustomRuleKind, rateLimitRuleKind, managedRulesetKind},
	generatedRulesetRuleKinds...)

// rulesetRuleReconciler reconciles the resources of one rule kind. Each kind's
// exported reconciler delegates to one.
type rulesetRuleReconciler struct {
	client.Client
	kind   rulesetRuleKind
	newAPI func(token string) (RulesetRuleAPI, error)
}

func (r *rulesetRuleReconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	obj := r.kind.newObject()
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// A paused resource is left as it is, deletion included, until its
	// kflare.dev/paused annotation is removed.
	if reconciler.Paused(obj) {
		return ctrl.Result{}, updatePaused(ctx, r.Client, obj)
	}

	// Then handle deletion, before anything else.
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
		logger.Info("Added finalizer", "rule", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	// The Zone watch re-triggers this reconcile when the Zone appears or
	// becomes ready.
	zone := &cloudflarev1alpha1.Zone{}
	zoneKey := types.NamespacedName{Name: obj.GetRulesetRuleSpec().ZoneRef.Name, Namespace: obj.GetNamespace()}
	if err := r.Get(ctx, zoneKey, zone); err != nil {
		return ctrl.Result{}, updateNotReady(ctx, r.Client, obj, "ZoneNotFound",
			fmt.Sprintf("Zone %q not found: %v", zoneKey.Name, err))
	}
	if !isZoneReady(zone) {
		return ctrl.Result{}, updateNotReady(ctx, r.Client, obj, "ZoneNotReady",
			fmt.Sprintf("Zone %q is not ready", zoneKey.Name))
	}

	_, token, credErr := resolveAccountToken(ctx, r.Client, zone.Spec.AccountRef.Name, true)
	if credErr != nil {
		return notReadyRetryAfter(ctx, r.Client, obj, credErr)
	}
	cfAPI, err := r.newAPI(token)
	if err != nil {
		return invalidToken(ctx, r.Client, obj, err)
	}

	zoneID, phase := zone.Status.CloudflareMetadata.ZoneID, r.kind.phase(obj)
	peers, err := r.peers(ctx, zoneID, phase)
	if err != nil {
		return ctrl.Result{}, err
	}
	ref := rulesetRuleRef(r.kind.name, obj.GetNamespace(), obj.GetName())
	rule := r.kind.definition(obj)
	rule.Ref.Value, rule.Ref.Present = ref, true
	state, err := syncRulesetRule(ctx, cfAPI, zoneID, phase, rule, peers)
	if reason, ok := r.kind.conflict(err); ok {
		return notReadyRetryAfter(ctx, r.Client, obj, &conditionError{
			Reason:  reason,
			Message: "Cloudflare refused the rule: " + cfpkg.ErrorMessage(err),
		})
	}
	if err != nil {
		return handleCloudflareError(ctx, r.Client, obj, err)
	}
	if state.wrote {
		logger.Info("Wrote rule", "ruleID", state.ruleID, "version", state.version)
	}
	if state.moved {
		logger.Info("Reordered rules", "zoneID", zoneID, "phase", phase)
	}

	obj.GetRulesetRuleStatus().CloudflareMetadata = cloudflarev1alpha1.RulesetRuleCloudflareMetadata{
		ZoneID:    zoneID,
		RulesetID: state.rulesetID,
		RuleID:    state.ruleID,
		Ref:       ref,
		Version:   state.version,
	}
	result, err := readyWithDriftCheck(ctx, r.Client, obj, "Synced", "Rule is synced with Cloudflare")
	if err != nil || !state.moved {
		return result, err
	}
	// Rules that moved get their order checked again soon, whatever the drift
	// check interval.
	return ctrl.Result{RequeueAfter: rulesetRuleRecheckInterval}, nil
}

// peers lists the rules kflare manages in zoneID and phase: those of every
// kind writing to the phase, in every namespace, since Zones in different
// namespaces can point at the same Cloudflare zone. Resources being deleted
// are left out: their rules are about to go. So are paused resources, whose
// rules kflare leaves alone: they keep their place, as rules created outside
// kflare do, rather than being moved by another rule's sync.
func (r *rulesetRuleReconciler) peers(ctx context.Context, zoneID string, phase rulesets.Phase) ([]rulesetRulePeer,
	error) {
	var peers []rulesetRulePeer
	zoneIDs := map[types.NamespacedName]string{}
	for _, kind := range rulesetRuleKinds {
		if !slices.Contains(kind.phases, phase) {
			continue
		}
		list := kind.newList()
		if err := r.List(ctx, list); err != nil {
			return nil, err
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			obj, ok := item.(rulesetRuleObject)
			if !ok {
				return nil, fmt.Errorf("unexpected %T in a %s list", item, kind.name)
			}
			if !obj.GetDeletionTimestamp().IsZero() || reconciler.Paused(obj) || kind.phase(obj) != phase {
				continue
			}
			spec := obj.GetRulesetRuleSpec()
			key := types.NamespacedName{Name: spec.ZoneRef.Name, Namespace: obj.GetNamespace()}
			id, seen := zoneIDs[key]
			if !seen {
				zone := &cloudflarev1alpha1.Zone{}
				if err := r.Get(ctx, key, zone); err != nil && !apierrors.IsNotFound(err) {
					return nil, err
				}
				id = zone.Status.CloudflareMetadata.ZoneID
				zoneIDs[key] = id
			}
			if id != zoneID {
				continue
			}
			peers = append(peers, rulesetRulePeer{
				ref:       rulesetRuleRef(kind.name, obj.GetNamespace(), obj.GetName()),
				priority:  spec.Priority,
				namespace: obj.GetNamespace(),
				name:      obj.GetName(),
				kind:      kind.name,
			})
		}
	}
	return peers, nil
}

// reconcileDelete handles the deletion lifecycle: unless the retain policy is
// set, it deletes the rule from Cloudflare, then removes the finalizer.
func (r *rulesetRuleReconciler) reconcileDelete(ctx context.Context, obj rulesetRuleObject) (ctrl.Result, error) {
	// Safety check: if the finalizer is already gone, there is nothing to do.
	if !controllerutil.ContainsFinalizer(obj, reconciler.Finalizer) {
		return ctrl.Result{}, nil
	}

	if !reconciler.RetainOnDelete(obj) {
		if err := r.deleteCloudflareRule(ctx, obj); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Remove the finalizer to allow Kubernetes to complete the deletion.
	_, err := reconciler.RemoveFinalizer(ctx, r.Client, obj)
	return ctrl.Result{}, err
}

// deleteCloudflareRule deletes the resource's rule from Cloudflare, resolving
// Zone → Account → Secret for credentials.
//
// The Zone waits for its rules before it is deleted, so it is normally still
// there. If it is gone anyway (its finalizer was removed by hand), there are
// no credentials to delete with and the rule stays in Cloudflare.
func (r *rulesetRuleReconciler) deleteCloudflareRule(ctx context.Context, obj rulesetRuleObject) error {
	zone := &cloudflarev1alpha1.Zone{}
	zoneKey := types.NamespacedName{Name: obj.GetRulesetRuleSpec().ZoneRef.Name, Namespace: obj.GetNamespace()}
	if err := r.Get(ctx, zoneKey, zone); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).Info("Zone is gone, skipping Cloudflare deletion", "zone", zoneKey.Name)
			return nil
		}
		return err
	}

	// The rule is in the zone of the last sync. Without one, a sync may still
	// have written the rule before failing to record it, so look in the
	// Zone's zone; a Zone that never had one never reached Cloudflare.
	zoneID := obj.GetRulesetRuleStatus().CloudflareMetadata.ZoneID
	if zoneID == "" {
		zoneID = zone.Status.CloudflareMetadata.ZoneID
	}
	if zoneID == "" {
		return nil
	}

	_, token, credErr := resolveAccountToken(ctx, r.Client, zone.Spec.AccountRef.Name, false)
	if credErr != nil {
		return credErr
	}
	cfAPI, err := r.newAPI(token)
	if err != nil {
		return err
	}
	// In a phase whose emptied entry point kflare deletes, it stays while
	// another resource is about to write its rule there.
	phase, peersRemain := r.kind.phase(obj), false
	if emptiedEntrypointPhases[phase] {
		peers, err := r.peers(ctx, zoneID, phase)
		if err != nil {
			return err
		}
		peersRemain = len(peers) > 0
	}
	return deleteRulesetRule(ctx, cfAPI, zoneID, phase,
		rulesetRuleRef(r.kind.name, obj.GetNamespace(), obj.GetName()), peersRemain)
}

// setup registers the kind's controller, with rec as its reconciler. It
// watches Zones, so a rule waiting for its Zone is reconciled as soon as the
// Zone is ready, and IPLists, so a rule that uses a list is written once the
// list exists. In a phase whose emptied entry point kflare deletes, it also
// watches the deletion of every rule kind in the phase (see
// rulesInZoneOfDeleted).
func (r *rulesetRuleReconciler) setup(mgr ctrl.Manager, rec reconcile.Reconciler) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(r.kind.newObject()).
		Watches(&cloudflarev1alpha1.Zone{}, handler.EnqueueRequestsFromMapFunc(r.rulesForZone)).
		Watches(&cloudflarev1alpha1.IPList{}, handler.EnqueueRequestsFromMapFunc(r.rulesUsingList))
	for _, kind := range r.kind.deletionsWatched() {
		b = b.Watches(kind.newObject(), handler.EnqueueRequestsFromMapFunc(r.rulesInZoneOfDeleted),
			builder.WithPredicates(deletesOnly))
	}
	return b.Complete(rec)
}

// deletionsWatched returns the rule kinds whose deletions the kind's
// controller watches: in each of its phases whose emptied entry point kflare
// deletes, every kind writing to the phase, each once.
func (k rulesetRuleKind) deletionsWatched() []rulesetRuleKind {
	var kinds []rulesetRuleKind
	for _, phase := range k.phases {
		if !emptiedEntrypointPhases[phase] {
			continue
		}
		for _, kind := range rulesetRuleKinds {
			watched := slices.ContainsFunc(kinds, func(w rulesetRuleKind) bool { return w.name == kind.name })
			if slices.Contains(kind.phases, phase) && !watched {
				kinds = append(kinds, kind)
			}
		}
	}
	return kinds
}

// rulesInZoneOfDeleted maps the deletion of a rule resource in a phase whose
// emptied entry point kflare deletes to the kind's resources that may have
// lost their rule with it: those last synced to the same Cloudflare zone, and
// those not synced yet. A resource that deletes the last rule there deletes
// the entry point too, and with it any rule another resource wrote in the
// moment between; the other resource's reconcile writes it again. It also
// lets a resource refused as a conflict with the deleted one write its rule
// at once rather than at its next retry.
func (r *rulesetRuleReconciler) rulesInZoneOfDeleted(ctx context.Context, deleted client.Object) []reconcile.Request {
	rule, ok := deleted.(rulesetRuleObject)
	if !ok {
		return nil
	}
	zoneID := rule.GetRulesetRuleStatus().CloudflareMetadata.ZoneID
	list := r.kind.newList()
	if err := r.List(ctx, list); err != nil {
		return nil
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, item := range items {
		obj, ok := item.(rulesetRuleObject)
		if !ok {
			continue
		}
		synced := obj.GetRulesetRuleStatus().CloudflareMetadata.ZoneID
		if zoneID == "" || synced == "" || synced == zoneID {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()},
			})
		}
	}
	return reqs
}

// The rule kinds' controllers watch IPLists.
//+kubebuilder:rbac:groups=kflare.dev,resources=iplists,verbs=get;list;watch

// rulesUsingList maps an IPList event to the kind's resources, in any
// namespace, whose expression uses the list, once the list exists in
// Cloudflare. Cloudflare refuses a rule that uses a list the account does not
// have (code 20127, a terminal error), so a rule applied together with its
// list may fail first; this writes it once the list is there.
func (r *rulesetRuleReconciler) rulesUsingList(ctx context.Context, obj client.Object) []reconcile.Request {
	ipList, ok := obj.(*cloudflarev1alpha1.IPList)
	if !ok || ipList.Status.CloudflareMetadata.ListID == "" {
		return nil
	}
	list := r.kind.newList()
	if err := r.List(ctx, list); err != nil {
		return nil
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, item := range items {
		rule, ok := item.(rulesetRuleObject)
		if ok && slices.Contains(listsInExpression(rule.GetRulesetRuleSpec().Expression), ipList.Spec.Name) {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: rule.GetName(), Namespace: rule.GetNamespace()},
			})
		}
	}
	return reqs
}

// rulesForZone maps a Zone event to the kind's resources in that zone.
func (r *rulesetRuleReconciler) rulesForZone(ctx context.Context, zone client.Object) []reconcile.Request {
	list := r.kind.newList()
	if err := r.List(ctx, list, client.InNamespace(zone.GetNamespace())); err != nil {
		return nil
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, item := range items {
		obj, ok := item.(rulesetRuleObject)
		if ok && obj.GetRulesetRuleSpec().ZoneRef.Name == zone.GetName() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()},
			})
		}
	}
	return reqs
}

// defaultRulesetRuleAPI is the production factory: cloudflare-go v7's
// rulesets services, with the token of a cfpkg.Client.
func defaultRulesetRuleAPI(token string) (RulesetRuleAPI, error) {
	client, err := cfpkg.New(token)
	if err != nil {
		return RulesetRuleAPI{}, err
	}
	service := rulesets.NewRulesetService(client.V7Options()...)
	return RulesetRuleAPI{Phases: service.Phases, Rulesets: service, Rules: service.Rules}, nil
}

// rulesetRuleDefinition starts the rule a resource asks for: action, and the
// fields every rule kind's spec holds. It always says whether the rule is
// enabled, since an update that leaves it out enables the rule, and leaves
// out an empty description, which Cloudflare stores as none. The kind adds
// its action's parameters.
func rulesetRuleDefinition(
	action rulesets.RulesetNewParamsRulesAction, spec cloudflarev1alpha1.RulesetRuleSpec,
) rulesets.RulesetNewParamsRule {
	var rule rulesets.RulesetNewParamsRule
	rule.Action.Value, rule.Action.Present = action, true
	rule.Expression.Value, rule.Expression.Present = spec.Expression, true
	if spec.Description != "" {
		rule.Description.Value, rule.Description.Present = spec.Description, true
	}
	rule.Enabled.Value, rule.Enabled.Present = spec.Enabled == nil || *spec.Enabled, true
	return rule
}

// blockResponseParameters are the action parameters of a block rule that
// answers with its own response. WAF custom rules and rate limiting rules
// both take one.
func blockResponseParameters(statusCode int32, contentType, content string) rulesets.BlockRuleActionParametersParam {
	var response rulesets.BlockRuleActionParametersResponseParam
	response.StatusCode.Value, response.StatusCode.Present = int64(statusCode), true
	response.ContentType.Value, response.ContentType.Present = contentType, true
	response.Content.Value, response.Content.Present = content, true
	var params rulesets.BlockRuleActionParametersParam
	params.Response.Value, params.Response.Present = response, true
	return params
}
