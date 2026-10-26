/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/cloudflare/cloudflare-go/v7/rulesets"

	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
)

// Ruleset rules. WAF custom rules, rate limiting rules, managed ruleset
// deployments and the Rules products (transform, redirect, cache,
// configuration and origin rules) are all rules in a zone's entry point
// ruleset for one Rulesets API phase. A kflare resource of one of these kinds
// manages exactly one such rule, and this file holds what they share:
//
//   - A rule is kflare's through its ref (rulesetRuleRef), which is derived
//     from the resource's kind, namespace and name. kflare finds its rules by
//     ref and never touches a rule whose ref it did not derive.
//   - Rules are written one at a time through the rule endpoints. The entry
//     point is created when the zone has none, but never replaced as a whole:
//     that would replace every rule in it, including rules kflare does not
//     manage.
//   - kflare keeps the rules it manages in a zone and phase in priority
//     order, moving them with position-only updates. Rules created outside
//     kflare keep their place, and so do the rules of paused resources.
//   - An emptied entry point stays, except in the phases where an entry point
//     changes what Cloudflare runs even without rules
//     (emptiedEntrypointPhases).
//
// A rule is cloudflare-go v7's rulesets.RulesetNewParamsRule: the rule as
// Rulesets.New takes it, whatever its action, with the action's parameters
// as v7's typed params (rulesets.SkipRuleActionParametersParam, ...).

// rulesetEntrypointExistsCode is the Cloudflare error code (with HTTP 400)
// for creating a zone entry point ruleset in a phase that already has one.
const rulesetEntrypointExistsCode = 20217

// entrypointRulesetName names the entry point rulesets kflare creates.
// Cloudflare requires a name; the phase, not the name, identifies the entry
// point.
const entrypointRulesetName = "default"

// managedRulesetLatestVersion is the version Cloudflare records for an
// execute rule that names none: the managed ruleset's latest.
const managedRulesetLatestVersion = "latest"

// emptiedEntrypointPhases are the phases where a zone's entry point changes
// what Cloudflare runs even when it holds no rules, so kflare deletes the
// entry point once deleting a rule has emptied it. In
// http_request_firewall_managed, any entry point stops the managed rulesets
// Cloudflare deploys by default, such as the Free Managed Ruleset on Free
// zones, and deleting it brings them back (both verified live, 2026-10-14).
var emptiedEntrypointPhases = map[rulesets.Phase]bool{
	rulesets.PhaseHTTPRequestFirewallManaged: true,
}

// RulesetPhasesAPI is the part of cloudflare-go v7's rulesets.PhaseService
// that the rule kinds call: reading a phase's entry point.
type RulesetPhasesAPI interface {
	Get(ctx context.Context, rulesetPhase rulesets.Phase, query rulesets.PhaseGetParams,
		opts ...option.RequestOption) (*rulesets.PhaseGetResponse, error)
}

// RulesetsAPI is the part of cloudflare-go v7's rulesets.RulesetService that
// the rule kinds call: creating and deleting an entry point.
type RulesetsAPI interface {
	New(ctx context.Context, params rulesets.RulesetNewParams,
		opts ...option.RequestOption) (*rulesets.RulesetNewResponseEnvelopeResult, error)
	Delete(ctx context.Context, rulesetID string, params rulesets.RulesetDeleteParams,
		opts ...option.RequestOption) error
}

// RulesetRulesAPI is the part of cloudflare-go v7's rulesets.RuleService
// that the rule kinds call: writing one rule of a ruleset.
type RulesetRulesAPI interface {
	New(ctx context.Context, rulesetID string, params rulesets.RuleNewParams,
		opts ...option.RequestOption) (*rulesets.RuleNewResponseEnvelopeResult, error)
	Edit(ctx context.Context, rulesetID, ruleID string, params rulesets.RuleEditParams,
		opts ...option.RequestOption) (*rulesets.RuleEditResponseEnvelopeResult, error)
	Delete(ctx context.Context, rulesetID, ruleID string, params rulesets.RuleDeleteParams,
		opts ...option.RequestOption) (*rulesets.RuleDeleteResponseEnvelopeResult, error)
}

var (
	_ RulesetPhasesAPI = (*rulesets.PhaseService)(nil)
	_ RulesetsAPI      = (*rulesets.RulesetService)(nil)
	_ RulesetRulesAPI  = (*rulesets.RuleService)(nil)
)

// RulesetRuleAPI is the Rulesets API the rule kinds call. Tests inject
// fakes; in production these are v7's services (defaultRulesetRuleAPI).
// cloudflare-go v0.119, which the hand-written controllers use, cannot create
// or edit a single rule, and fails on the response of a rule delete.
type RulesetRuleAPI struct {
	Phases   RulesetPhasesAPI
	Rulesets RulesetsAPI
	Rules    RulesetRulesAPI
}

// rulesetRuleRef returns the ref of the rule a resource manages: "kflare-"
// and the first 32 hex characters of the SHA-256 of kind/namespace/name.
// Deriving it from the resource's identity rather than its UID lets a
// resource that is created again, for example from a backup, find its rule.
// Cloudflare requires refs to be unique within a ruleset and at most 128
// bytes long, and refuses to change an existing rule's ref.
func rulesetRuleRef(kind, namespace, name string) string {
	sum := sha256.Sum256([]byte(kind + "/" + namespace + "/" + name))
	return "kflare-" + hex.EncodeToString(sum[:16])
}

// rulesetRulePeer is a rule kflare manages in a zone and phase, with what
// decides its place among the others: lower priority first, then namespace,
// name and kind.
type rulesetRulePeer struct {
	ref       string
	priority  int32
	namespace string
	name      string
	kind      string
}

// sortedRulesetRulePeers returns peers in the order kflare keeps their rules
// in.
func sortedRulesetRulePeers(peers []rulesetRulePeer) []rulesetRulePeer {
	sorted := slices.Clone(peers)
	slices.SortFunc(sorted, func(a, b rulesetRulePeer) int {
		return cmp.Or(
			cmp.Compare(a.priority, b.priority),
			strings.Compare(a.namespace, b.namespace),
			strings.Compare(a.name, b.name),
			strings.Compare(a.kind, b.kind),
		)
	})
	return sorted
}

// rulesetRuleState is where a sync left the rule.
type rulesetRuleState struct {
	rulesetID string
	ruleID    string
	// version is the rule's version, which Cloudflare bumps on every update.
	version string
	// wrote is true when the sync created or updated the rule.
	wrote bool
	// moved is true when the sync reordered rules, or stopped reordering
	// because a rule disappeared. Another resource may have been reordering
	// the same rules at the same time, so the caller checks again shortly.
	moved bool
}

// syncRulesetRule makes the zone's entry point ruleset for phase hold rule,
// which rule.Ref identifies, and keeps the rules kflare manages there in
// order. peers lists those rules, this one included; without peers nothing
// is reordered and a new rule goes last. The rule is written whole, so it
// must set every field it wants: Cloudflare resets a field an update leaves
// out.
func syncRulesetRule(
	ctx context.Context,
	api RulesetRuleAPI,
	zoneID string,
	phase rulesets.Phase,
	rule rulesets.RulesetNewParamsRule,
	peers []rulesetRulePeer,
) (rulesetRuleState, error) {
	zone := zoneRulesets{api: api, zoneID: zoneID}
	ruleset, wrote, err := writeRulesetRule(ctx, zone, phase, rule, peers)
	if cfpkg.IsNotFound(err) {
		// The rule or its ruleset was deleted between the read and the
		// write. Read the entry point again, and try once more.
		ruleset, wrote, err = writeRulesetRule(ctx, zone, phase, rule, peers)
	}
	if err != nil {
		return rulesetRuleState{}, err
	}
	ruleset, moved, err := orderRulesetRules(ctx, zone, ruleset, peers)
	if err != nil {
		return rulesetRuleState{}, err
	}
	current, found := ruleset.rule(rule.Ref.Value)
	if !found {
		// Deleted by someone else right after kflare wrote it; the error
		// makes the next reconcile write it again.
		return rulesetRuleState{}, fmt.Errorf("rule %s disappeared from ruleset %s during the sync",
			rule.Ref.Value, ruleset.id)
	}
	return rulesetRuleState{
		rulesetID: ruleset.id, ruleID: current.id, version: current.version, wrote: wrote, moved: moved,
	}, nil
}

// writeRulesetRule creates or updates rule in the phase's entry point,
// creating the entry point when the zone has none, and writes nothing when
// the rule is already as desired. It returns the ruleset as Cloudflare last
// reported it, and whether it wrote.
func writeRulesetRule(
	ctx context.Context,
	zone zoneRulesets,
	phase rulesets.Phase,
	rule rulesets.RulesetNewParamsRule,
	peers []rulesetRulePeer,
) (entrypoint, bool, error) {
	ruleset, err := zone.getEntrypoint(ctx, phase)
	if cfpkg.IsNotFound(err) {
		created, createErr := zone.createEntrypoint(ctx, phase, rule)
		if createErr == nil {
			return created, true, nil
		}
		if !cfpkg.HasErrorCode(createErr, rulesetEntrypointExistsCode) {
			return entrypoint{}, false, createErr
		}
		// Another resource created the entry point first: add the rule to it.
		ruleset, err = zone.getEntrypoint(ctx, phase)
	}
	if err != nil {
		return entrypoint{}, false, err
	}

	current, found := ruleset.rule(rule.Ref.Value)
	switch {
	case !found:
		ruleset, err = zone.createRule(ctx, ruleset.id, rule, insertPosition(ruleset, rule.Ref.Value, peers))
	case !rulesetRuleMatches(rule, current.raw):
		ruleset, err = zone.updateRule(ctx, ruleset.id, current.id, rule)
	default:
		return ruleset, false, nil
	}
	if err != nil {
		return entrypoint{}, false, err
	}
	return ruleset, true, nil
}

// insertPosition is where a new rule goes so that the rules kflare manages
// stay in order: right after the closest peer that sorts before it, or else
// right before the closest that sorts after it. It is nil, which appends the
// rule, when no other peer's rule is in the ruleset or ref is not a peer.
func insertPosition(ruleset entrypoint, ref string, peers []rulesetRulePeer) rulePosition {
	sorted := sortedRulesetRulePeers(peers)
	self := slices.IndexFunc(sorted, func(p rulesetRulePeer) bool { return p.ref == ref })
	if self < 0 {
		return nil
	}
	for i := self - 1; i >= 0; i-- {
		if before, ok := ruleset.rule(sorted[i].ref); ok {
			return rulePosition{"after": before.id}
		}
	}
	for _, peer := range sorted[self+1:] {
		if after, ok := ruleset.rule(peer.ref); ok {
			return rulePosition{"before": after.id}
		}
	}
	return nil
}

// orderRulesetRules puts the peers' rules in ruleset into peer order. It
// walks them in that order and moves any rule that sits before the previous
// one to right after it, so the rules walked so far are always in order and
// one pass suffices. A move changes neither the rule's definition nor its
// version. Rules that are not kflare's keep their place. It returns the
// ruleset as Cloudflare last reported it, and whether it moved anything.
func orderRulesetRules(
	ctx context.Context,
	zone zoneRulesets,
	ruleset entrypoint,
	peers []rulesetRulePeer,
) (entrypoint, bool, error) {
	moved := false
	previousID := ""
	for _, peer := range sortedRulesetRulePeers(peers) {
		rule, found := ruleset.rule(peer.ref)
		if !found {
			continue
		}
		if previousID != "" && ruleset.index(rule.id) < ruleset.index(previousID) {
			updated, err := zone.moveRule(ctx, ruleset.id, rule.id, rulePosition{"after": previousID})
			if cfpkg.IsNotFound(err) {
				// One of the two rules was deleted meanwhile. Stop here; the
				// caller checks the order again shortly.
				return ruleset, true, nil
			}
			if err != nil {
				return ruleset, moved, err
			}
			ruleset, moved = updated, true
		}
		previousID = rule.id
	}
	return ruleset, moved, nil
}

// deleteRulesetRule removes the rule with ref from the zone's entry point
// ruleset for phase. A missing entry point or rule counts as removed. The
// entry point stays even when the rule was its last: Cloudflare keeps an
// emptied entry point, and kflare may not be its only user.
//
// In the phases of emptiedEntrypointPhases, an entry point left without rules
// is deleted, which restores what Cloudflare runs without one, unless
// peersRemain: another kflare resource is about to write its rule to it.
func deleteRulesetRule(
	ctx context.Context,
	api RulesetRuleAPI,
	zoneID string,
	phase rulesets.Phase,
	ref string,
	peersRemain bool,
) error {
	zone := zoneRulesets{api: api, zoneID: zoneID}
	ruleset, err := zone.getEntrypoint(ctx, phase)
	if cfpkg.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if rule, found := ruleset.rule(ref); found {
		updated, err := zone.deleteRule(ctx, ruleset.id, rule.id)
		switch {
		case cfpkg.IsNotFound(err):
			// The rule, or the whole entry point, went meanwhile. Read the
			// entry point again to see whether it is empty.
			ruleset, err = zone.getEntrypoint(ctx, phase)
			if cfpkg.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			ruleset = updated
		}
	}
	if !emptiedEntrypointPhases[phase] || peersRemain || len(ruleset.rules) > 0 {
		return nil
	}
	if err := zone.deleteEntrypoint(ctx, ruleset.id); err != nil && !cfpkg.IsNotFound(err) {
		return err
	}
	return nil
}

// entrypoint is what kflare reads of a zone's entry point ruleset: its ID
// and its rules, in order.
type entrypoint struct {
	id    string
	rules []entrypointRule
}

// entrypointRule is a rule of an entry point as Cloudflare returned it.
type entrypointRule struct {
	id      string
	ref     string
	version string
	// raw is the rule's JSON, which rulesetRuleMatches compares.
	raw string
}

// rule returns the rule with ref.
func (e entrypoint) rule(ref string) (entrypointRule, bool) {
	i := slices.IndexFunc(e.rules, func(rule entrypointRule) bool { return rule.ref == ref })
	if i < 0 {
		return entrypointRule{}, false
	}
	return e.rules[i], true
}

// index returns the position of the rule with id, or -1.
func (e entrypoint) index(id string) int {
	return slices.IndexFunc(e.rules, func(rule entrypointRule) bool { return rule.id == id })
}

// rulePosition places a rule right before or right after another rule of the
// same ruleset: {"before": id} or {"after": id}. v7's rule bodies take any
// value as a position (their typed positions are one per action), and a map
// holds only the key it is given: a struct would also send the other key,
// empty, which places a rule first or last.
type rulePosition map[string]string

// zoneRulesets makes the Rulesets calls of one zone, and reads the entry
// point back from each call's answer. v7 answers every call with a ruleset
// type of its own.
type zoneRulesets struct {
	api    RulesetRuleAPI
	zoneID string
}

// getEntrypoint reads the zone's entry point ruleset for phase. A zone without
// one answers 404.
func (z zoneRulesets) getEntrypoint(ctx context.Context, phase rulesets.Phase) (entrypoint, error) {
	var params rulesets.PhaseGetParams
	params.ZoneID.Value, params.ZoneID.Present = z.zoneID, true
	ruleset, err := z.api.Phases.Get(ctx, phase, params)
	if err != nil {
		return entrypoint{}, err
	}
	read := entrypoint{id: ruleset.ID}
	for _, rule := range ruleset.Rules {
		read.rules = append(read.rules, entrypointRule{rule.ID, rule.Ref, rule.Version, rule.JSON.RawJSON()})
	}
	return read, nil
}

// createEntrypoint creates the zone's entry point ruleset for phase, holding
// rule.
func (z zoneRulesets) createEntrypoint(
	ctx context.Context, phase rulesets.Phase, rule rulesets.RulesetNewParamsRule,
) (entrypoint, error) {
	var params rulesets.RulesetNewParams
	params.ZoneID.Value, params.ZoneID.Present = z.zoneID, true
	params.Kind.Value, params.Kind.Present = rulesets.KindZone, true
	params.Name.Value, params.Name.Present = entrypointRulesetName, true
	params.Phase.Value, params.Phase.Present = phase, true
	params.Rules.Value, params.Rules.Present = []rulesets.RulesetNewParamsRuleUnion{rule}, true
	ruleset, err := z.api.Rulesets.New(ctx, params)
	if err != nil {
		return entrypoint{}, err
	}
	read := entrypoint{id: ruleset.ID}
	for _, rule := range ruleset.Rules {
		read.rules = append(read.rules, entrypointRule{rule.ID, rule.Ref, rule.Version, rule.JSON.RawJSON()})
	}
	return read, nil
}

// createRule adds rule to the ruleset at position, or after its last rule
// when position is nil.
func (z zoneRulesets) createRule(
	ctx context.Context, rulesetID string, rule rulesets.RulesetNewParamsRule, position rulePosition,
) (entrypoint, error) {
	body := rulesets.RuleNewParamsBody{
		ID:                     rule.ID,
		ActionParameters:       rule.ActionParameters,
		Categories:             rule.Categories,
		Description:            rule.Description,
		Enabled:                rule.Enabled,
		ExposedCredentialCheck: rule.ExposedCredentialCheck,
		Expression:             rule.Expression,
		Logging:                rule.Logging,
		Ratelimit:              rule.Ratelimit,
		Ref:                    rule.Ref,
	}
	body.Action.Value, body.Action.Present = rulesets.RuleNewParamsBodyAction(rule.Action.Value), rule.Action.Present
	if position != nil {
		body.Position.Value, body.Position.Present = position, true
	}
	params := rulesets.RuleNewParams{Body: body}
	params.ZoneID.Value, params.ZoneID.Present = z.zoneID, true
	ruleset, err := z.api.Rules.New(ctx, rulesetID, params)
	if err != nil {
		return entrypoint{}, err
	}
	read := entrypoint{id: ruleset.ID}
	for _, rule := range ruleset.Rules {
		read.rules = append(read.rules, entrypointRule{rule.ID, rule.Ref, rule.Version, rule.JSON.RawJSON()})
	}
	return read, nil
}

// updateRule replaces the rule ruleID with rule. Cloudflare replaces the
// whole rule, so a field rule leaves unset is reset rather than kept; the
// rule keeps its ID and place.
func (z zoneRulesets) updateRule(
	ctx context.Context, rulesetID, ruleID string, rule rulesets.RulesetNewParamsRule,
) (entrypoint, error) {
	body := rulesets.RuleEditParamsBody{
		ID:                     rule.ID,
		ActionParameters:       rule.ActionParameters,
		Categories:             rule.Categories,
		Description:            rule.Description,
		Enabled:                rule.Enabled,
		ExposedCredentialCheck: rule.ExposedCredentialCheck,
		Expression:             rule.Expression,
		Logging:                rule.Logging,
		Ratelimit:              rule.Ratelimit,
		Ref:                    rule.Ref,
	}
	body.Action.Value, body.Action.Present = rulesets.RuleEditParamsBodyAction(rule.Action.Value), rule.Action.Present
	return z.editRule(ctx, rulesetID, ruleID, body)
}

// moveRule moves the rule ruleID to position. An edit that holds only a
// position changes neither the rule's definition nor its version (verified
// live).
func (z zoneRulesets) moveRule(
	ctx context.Context, rulesetID, ruleID string, position rulePosition,
) (entrypoint, error) {
	var body rulesets.RuleEditParamsBody
	body.Position.Value, body.Position.Present = position, true
	return z.editRule(ctx, rulesetID, ruleID, body)
}

func (z zoneRulesets) editRule(
	ctx context.Context, rulesetID, ruleID string, body rulesets.RuleEditParamsBody,
) (entrypoint, error) {
	params := rulesets.RuleEditParams{Body: body}
	params.ZoneID.Value, params.ZoneID.Present = z.zoneID, true
	ruleset, err := z.api.Rules.Edit(ctx, rulesetID, ruleID, params)
	if err != nil {
		return entrypoint{}, err
	}
	read := entrypoint{id: ruleset.ID}
	for _, rule := range ruleset.Rules {
		read.rules = append(read.rules, entrypointRule{rule.ID, rule.Ref, rule.Version, rule.JSON.RawJSON()})
	}
	return read, nil
}

// deleteRule removes the rule ruleID from the ruleset. Cloudflare answers with
// the ruleset (200), which cloudflare-go v0.119 fails to read and v7 reads.
func (z zoneRulesets) deleteRule(ctx context.Context, rulesetID, ruleID string) (entrypoint, error) {
	var params rulesets.RuleDeleteParams
	params.ZoneID.Value, params.ZoneID.Present = z.zoneID, true
	ruleset, err := z.api.Rules.Delete(ctx, rulesetID, ruleID, params)
	if err != nil {
		return entrypoint{}, err
	}
	read := entrypoint{id: ruleset.ID}
	for _, rule := range ruleset.Rules {
		read.rules = append(read.rules, entrypointRule{rule.ID, rule.Ref, rule.Version, rule.JSON.RawJSON()})
	}
	return read, nil
}

// deleteEntrypoint deletes the ruleset rulesetID, an entry point.
func (z zoneRulesets) deleteEntrypoint(ctx context.Context, rulesetID string) error {
	var params rulesets.RulesetDeleteParams
	params.ZoneID.Value, params.ZoneID.Present = z.zoneID, true
	return z.api.Rulesets.Delete(ctx, rulesetID, params)
}

// rulesetRuleMatches reports whether actual, a rule's JSON as Cloudflare
// returned it, already is desired. It compares the definitions only, not the
// fields Cloudflare sets (the ID, version and time of the last update), and
// counts what Cloudflare fills in by itself as set (verified live): a rule is
// enabled unless told otherwise, a skip rule logs its matches, and an execute
// rule runs the managed ruleset's latest version.
func rulesetRuleMatches(desired rulesets.RulesetNewParamsRule, actual string) bool {
	sent, err := desired.MarshalJSON()
	if err != nil {
		return false
	}
	want, err := comparableRule(sent)
	if err != nil {
		return false
	}
	got, err := comparableRule([]byte(actual))
	if err != nil {
		return false
	}
	return reflect.DeepEqual(want, got)
}

// comparableRule decodes a rule's JSON into plain values, without the fields
// Cloudflare sets and the nulls and empty objects and arrays, and with
// Cloudflare's defaults filled in, so that two definitions compare equal when
// Cloudflare treats them the same.
func comparableRule(raw []byte) (map[string]any, error) {
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	rule, _ := withoutEmpty(decoded).(map[string]any)
	delete(rule, "id")
	delete(rule, "version")
	delete(rule, "last_updated")
	if _, set := rule["enabled"]; !set {
		rule["enabled"] = true
	}
	switch rule["action"] {
	case string(rulesets.RulesetNewParamsRulesActionSkip):
		if _, set := rule["logging"]; !set {
			rule["logging"] = map[string]any{"enabled": true}
		}
	case string(rulesets.RulesetNewParamsRulesActionExecute):
		if params, ok := rule["action_parameters"].(map[string]any); ok {
			if _, set := params["version"]; !set {
				params["version"] = managedRulesetLatestVersion
			}
		}
	}
	return rule, nil
}

// withoutEmpty drops the nulls, and the objects and arrays that are or end
// up empty, from the objects in a decoded JSON value. Array elements stay:
// their positions matter.
func withoutEmpty(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if item = withoutEmpty(item); !isEmptyJSON(item) {
				out[key] = item
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = withoutEmpty(item)
		}
		return out
	default:
		return v
	}
}

// isEmptyJSON reports whether a decoded JSON value is null, {} or [].
func isEmptyJSON(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case map[string]any:
		return len(v) == 0
	case []any:
		return len(v) == 0
	default:
		return false
	}
}
