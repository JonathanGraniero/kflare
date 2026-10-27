/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"time"

	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/cloudflare/cloudflare-go/v7/rulesets"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
)

const (
	rulesTestZone  = "zone-rules-1"
	rulesTestPhase = rulesets.PhaseHTTPRequestFirewallCustom
)

// fakeRulesetAPI is an in-memory Rulesets API for one zone, behind the three
// interfaces of RulesetRuleAPI (api). Like Cloudflare, it keeps each rule as
// the JSON it was sent: it reads every request from the JSON cloudflare-go v7
// sends, and answers with v7's types decoded from JSON, as the SDK does. It
// follows the behavior verified against the live API on 2026-10-12:
//
//   - A phase without an entry point answers 404 (code 10003). Creating a
//     second zone entry point in a phase is a 400 (code 20217).
//   - A new rule goes last unless placed before or after another rule. A rule
//     without a ref gets its ID as ref. Refs are unique within a ruleset and
//     cannot change (400, code 20023). The plan caps the rules in a phase
//     (400, code 50001).
//   - A rule comes back as sent, plus its ID, version and update time.
//     Cloudflare enables a rule unless told otherwise, and makes a skip rule
//     log its matches.
//   - An update replaces the whole rule but keeps its ID, ref and place, and
//     bumps its version. A move, an edit that holds only a position, changes
//     neither its definition nor its version.
//   - Every write bumps the ruleset's version and returns the whole ruleset.
//   - An unknown ruleset is a 404 (code 10001), an unknown rule a 404 (code
//     10002). Deleting the last rule leaves the entry point, without rules.
//
// And on 2026-10-14, for execute rules: an execute rule runs the managed
// ruleset's latest version unless told otherwise, a zone executes a managed
// ruleset at most once (400, code 20014), and deleting a ruleset removes it.
type fakeRulesetAPI struct {
	entrypoints map[rulesets.Phase]*fakeRuleset
	maxRules    int
	nextID      int
	clock       time.Time

	// before runs at the start of every call with the call's name, so that a
	// test can change the ruleset the way a concurrent writer would. The
	// names are Phases.Get, Rulesets.New, Rulesets.Delete, Rules.New,
	// Rules.Edit, Rules.Delete, and Rules.Edit position for a move.
	before func(call string)
	// errs makes a call fail with the error instead of running.
	errs map[string]error

	writes int // successful creates, updates and deletes
	moves  int // successful moves
	// lastBody is the body of the last rule create or update, as v7 sent it.
	lastBody map[string]any
}

// fakeRuleset is an entry point as the fake keeps it.
type fakeRuleset struct {
	id      string
	name    string
	kind    string
	phase   rulesets.Phase
	version int
	rules   []fakeRule
}

// fakeRule is a rule as Cloudflare keeps it: the JSON it was sent, decoded,
// with its ID, version and update time, and Cloudflare's defaults.
type fakeRule map[string]any

func (r fakeRule) id() string      { return r.text("id") }
func (r fakeRule) ref() string     { return r.text("ref") }
func (r fakeRule) version() string { return r.text("version") }

func (r fakeRule) text(key string) string {
	s, _ := r[key].(string)
	return s
}

// jsonOf returns the JSON of the rule's field key, for MatchJSON.
func (r fakeRule) jsonOf(key string) string {
	raw, err := json.Marshal(r[key])
	Expect(err).NotTo(HaveOccurred())
	return string(raw)
}

// clone returns a copy of the rule that shares nothing with it.
func (r fakeRule) clone() fakeRule {
	raw, err := json.Marshal(r)
	Expect(err).NotTo(HaveOccurred())
	var out fakeRule
	Expect(json.Unmarshal(raw, &out)).To(Succeed())
	return out
}

func newFakeRulesetAPI() *fakeRulesetAPI {
	return &fakeRulesetAPI{
		entrypoints: map[rulesets.Phase]*fakeRuleset{},
		maxRules:    5,
		clock:       time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC),
		errs:        map[string]error{},
	}
}

// api returns the fake as the services of a RulesetRuleAPI.
func (f *fakeRulesetAPI) api() RulesetRuleAPI {
	return RulesetRuleAPI{Phases: fakeRulesetPhases{f}, Rulesets: fakeRulesetsService{f}, Rules: fakeRulesetRules{f}}
}

// start checks that a call is for the fake's zone, runs before, and returns
// the error errs holds for the call.
func (f *fakeRulesetAPI) start(call, accountID, zoneID string) error {
	if accountID != "" || zoneID != rulesTestZone {
		return fmt.Errorf("fake: %s called for account %q and zone %q, want zone %s", call, accountID, zoneID, rulesTestZone)
	}
	if f.before != nil {
		f.before(call)
	}
	return f.errs[call]
}

func (f *fakeRulesetAPI) newID() string {
	f.nextID++
	return fmt.Sprintf("id%02d", f.nextID)
}

// stored returns rule as Cloudflare stores it under id, with version.
func (f *fakeRulesetAPI) stored(rule fakeRule, id string, version int) fakeRule {
	f.clock = f.clock.Add(time.Minute)
	out := rule.clone()
	out["id"], out["version"], out["last_updated"] = id, strconv.Itoa(version), f.clock.Format(time.RFC3339)
	if out.ref() == "" {
		out["ref"] = id
	}
	out.fillDefaults()
	return out
}

// fillDefaults fills in what Cloudflare stores for a rule that leaves it out:
// a rule is enabled, a skip rule logs its matches, and an execute rule runs
// the managed ruleset's latest version.
func (r fakeRule) fillDefaults() {
	if _, set := r["enabled"]; !set {
		r["enabled"] = true
	}
	switch r["action"] {
	case "skip":
		if _, set := r["logging"]; !set {
			r["logging"] = map[string]any{"enabled": true}
		}
	case "execute":
		if params, ok := r["action_parameters"].(map[string]any); ok {
			if _, set := params["version"]; !set {
				params["version"] = managedRulesetLatestVersion
			}
		}
	}
}

// executes returns the managed ruleset rule executes, or "" if it is not an
// execute rule.
func executes(rule fakeRule) string {
	if rule["action"] != "execute" {
		return ""
	}
	params, _ := rule["action_parameters"].(map[string]any)
	id, _ := params["id"].(string)
	return id
}

// executesTwice reports whether rule executes a managed ruleset that a rule
// in rules other than the one with exceptID already executes.
func executesTwice(rules []fakeRule, rule fakeRule, exceptID string) bool {
	ruleset := executes(rule)
	return ruleset != "" && slices.ContainsFunc(rules, func(other fakeRule) bool {
		return other.id() != exceptID && executes(other) == ruleset
	})
}

func (f *fakeRulesetAPI) ruleset(id string) (*fakeRuleset, error) {
	for _, rs := range f.entrypoints {
		if rs.id == id {
			return rs, nil
		}
	}
	return nil, rulesetsError(http.StatusNotFound, 10001)
}

// rule returns the rule in rs with ref.
func (rs *fakeRuleset) rule(ref string) (fakeRule, bool) {
	i := slices.IndexFunc(rs.rules, func(rule fakeRule) bool { return rule.ref() == ref })
	if i < 0 {
		return nil, false
	}
	return rs.rules[i], true
}

// index returns the position of the rule in rs with id, or -1.
func (rs *fakeRuleset) index(id string) int {
	return slices.IndexFunc(rs.rules, func(rule fakeRule) bool { return rule.id() == id })
}

// sent decodes the JSON v7 sends for a request's params.
func sent(params json.Marshaler) fakeRule {
	raw, err := params.MarshalJSON()
	Expect(err).NotTo(HaveOccurred())
	var body fakeRule
	Expect(json.Unmarshal(raw, &body)).To(Succeed())
	return body
}

// answer returns rs as v7 reads Cloudflare's answer: decoded from JSON into
// T. Cloudflare returns "rules": null for a ruleset without rules.
func answer[T any](rs *fakeRuleset) *T {
	var rules []fakeRule
	if len(rs.rules) > 0 {
		rules = rs.rules
	}
	raw, err := json.Marshal(map[string]any{
		"id": rs.id, "name": rs.name, "kind": rs.kind, "phase": rs.phase, "version": strconv.Itoa(rs.version),
		"last_updated": time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC), "rules": rules,
	})
	Expect(err).NotTo(HaveOccurred())
	var out T
	Expect(json.Unmarshal(raw, &out)).To(Succeed())
	return &out
}

// insertionIndex is where a rule placed at position goes in rules: the end
// without a position, or next to the one rule the position names.
func insertionIndex(rules []fakeRule, position map[string]any) (int, error) {
	before, _ := position["before"].(string)
	after, _ := position["after"].(string)
	index := func(id string) int {
		return slices.IndexFunc(rules, func(rule fakeRule) bool { return rule.id() == id })
	}
	switch {
	case position == nil:
		return len(rules), nil
	case len(position) == 1 && before != "":
		if i := index(before); i >= 0 {
			return i, nil
		}
	case len(position) == 1 && after != "":
		if i := index(after); i >= 0 {
			return i + 1, nil
		}
	}
	return 0, fmt.Errorf("fake: position %v does not name exactly one rule of the ruleset", position)
}

// fakeRulesetPhases is the fake as v7's rulesets.PhaseService.
type fakeRulesetPhases struct{ *fakeRulesetAPI }

func (f fakeRulesetPhases) Get(
	_ context.Context, phase rulesets.Phase, params rulesets.PhaseGetParams, _ ...option.RequestOption,
) (*rulesets.PhaseGetResponse, error) {
	if err := f.start("Phases.Get", params.AccountID.Value, params.ZoneID.Value); err != nil {
		return nil, err
	}
	rs, ok := f.entrypoints[phase]
	if !ok {
		return nil, rulesetsError(http.StatusNotFound, 10003)
	}
	return answer[rulesets.PhaseGetResponse](rs), nil
}

// fakeRulesetsService is the fake as v7's rulesets.RulesetService.
type fakeRulesetsService struct{ *fakeRulesetAPI }

func (f fakeRulesetsService) New(
	_ context.Context, params rulesets.RulesetNewParams, _ ...option.RequestOption,
) (*rulesets.RulesetNewResponseEnvelopeResult, error) {
	if err := f.start("Rulesets.New", params.AccountID.Value, params.ZoneID.Value); err != nil {
		return nil, err
	}
	body := sent(params)
	phase := rulesets.Phase(body.text("phase"))
	if body["kind"] != "zone" || body["name"] == "" || phase == "" {
		return nil, fmt.Errorf("fake: want a named zone entry point for a phase, got %v", body)
	}
	if _, ok := f.entrypoints[phase]; ok {
		return nil, rulesetsError(http.StatusBadRequest, rulesetEntrypointExistsCode)
	}
	rules, _ := body["rules"].([]any)
	if len(rules) > f.maxRules {
		return nil, rulesetsError(http.StatusBadRequest, 50001)
	}
	rs := &fakeRuleset{id: f.newID(), name: body.text("name"), kind: body.text("kind"), phase: phase}
	for _, item := range rules {
		rule := fakeRule(item.(map[string]any))
		if _, taken := rs.rule(rule.ref()); taken {
			return nil, rulesetsError(http.StatusBadRequest, 20023)
		}
		if executesTwice(rs.rules, rule, "") {
			return nil, rulesetsError(http.StatusBadRequest, executeConflictCode)
		}
		rs.rules = append(rs.rules, f.stored(rule, f.newID(), 1))
	}
	f.entrypoints[phase] = rs
	f.writes++
	rs.version++
	return answer[rulesets.RulesetNewResponseEnvelopeResult](rs), nil
}

func (f fakeRulesetsService) Delete(
	_ context.Context, rulesetID string, params rulesets.RulesetDeleteParams, _ ...option.RequestOption,
) error {
	if err := f.start("Rulesets.Delete", params.AccountID.Value, params.ZoneID.Value); err != nil {
		return err
	}
	for phase, rs := range f.entrypoints {
		if rs.id == rulesetID {
			delete(f.entrypoints, phase)
			f.writes++
			return nil
		}
	}
	return rulesetsError(http.StatusNotFound, 10001)
}

// fakeRulesetRules is the fake as v7's rulesets.RuleService.
type fakeRulesetRules struct{ *fakeRulesetAPI }

func (f fakeRulesetRules) New(
	_ context.Context, rulesetID string, params rulesets.RuleNewParams, _ ...option.RequestOption,
) (*rulesets.RuleNewResponseEnvelopeResult, error) {
	if err := f.start("Rules.New", params.AccountID.Value, params.ZoneID.Value); err != nil {
		return nil, err
	}
	rs, err := f.ruleset(rulesetID)
	if err != nil {
		return nil, err
	}
	rule := sent(params)
	f.lastBody = rule.clone()
	position, _ := rule["position"].(map[string]any)
	delete(rule, "position")
	if _, taken := rs.rule(rule.ref()); taken && rule.ref() != "" {
		return nil, rulesetsError(http.StatusBadRequest, 20023)
	}
	if len(rs.rules) >= f.maxRules {
		return nil, rulesetsError(http.StatusBadRequest, 50001)
	}
	if executesTwice(rs.rules, rule, "") {
		return nil, rulesetsError(http.StatusBadRequest, executeConflictCode)
	}
	index, err := insertionIndex(rs.rules, position)
	if err != nil {
		return nil, err
	}
	rs.rules = slices.Insert(rs.rules, index, f.stored(rule, f.newID(), 1))
	f.writes++
	rs.version++
	return answer[rulesets.RuleNewResponseEnvelopeResult](rs), nil
}

// Edit updates a rule, or moves it when the edit holds only a position.
func (f fakeRulesetRules) Edit(
	_ context.Context, rulesetID, ruleID string, params rulesets.RuleEditParams, _ ...option.RequestOption,
) (*rulesets.RuleEditResponseEnvelopeResult, error) {
	body := sent(params)
	position, move := body["position"].(map[string]any)
	call := "Rules.Edit"
	if move {
		call = "Rules.Edit position"
	}
	if err := f.start(call, params.AccountID.Value, params.ZoneID.Value); err != nil {
		return nil, err
	}
	if move && len(body) > 1 {
		return nil, fmt.Errorf("fake: kflare moves a rule with its position alone, got %v", body)
	}
	rs, err := f.ruleset(rulesetID)
	if err != nil {
		return nil, err
	}
	i := rs.index(ruleID)
	if i < 0 {
		return nil, rulesetsError(http.StatusNotFound, 10002)
	}

	if move {
		rule := rs.rules[i]
		rest := slices.Delete(slices.Clone(rs.rules), i, i+1)
		index, err := insertionIndex(rest, position)
		if err != nil {
			return nil, err
		}
		rs.rules = slices.Insert(rest, index, rule)
		f.moves++
	} else {
		f.lastBody = body.clone()
		old := rs.rules[i]
		if ref := body.ref(); ref != "" && ref != old.ref() {
			return nil, rulesetsError(http.StatusBadRequest, 20023)
		}
		if executesTwice(rs.rules, body, ruleID) {
			return nil, rulesetsError(http.StatusBadRequest, executeConflictCode)
		}
		body["ref"] = old.ref()
		version, err := strconv.Atoi(old.version())
		Expect(err).NotTo(HaveOccurred())
		rs.rules[i] = f.stored(body, old.id(), version+1)
		f.writes++
	}
	rs.version++
	return answer[rulesets.RuleEditResponseEnvelopeResult](rs), nil
}

func (f fakeRulesetRules) Delete(
	_ context.Context, rulesetID, ruleID string, params rulesets.RuleDeleteParams, _ ...option.RequestOption,
) (*rulesets.RuleDeleteResponseEnvelopeResult, error) {
	if err := f.start("Rules.Delete", params.AccountID.Value, params.ZoneID.Value); err != nil {
		return nil, err
	}
	rs, err := f.ruleset(rulesetID)
	if err != nil {
		return nil, err
	}
	i := rs.index(ruleID)
	if i < 0 {
		return nil, rulesetsError(http.StatusNotFound, 10002)
	}
	rs.rules = slices.Delete(rs.rules, i, i+1)
	f.writes++
	rs.version++
	return answer[rulesets.RuleDeleteResponseEnvelopeResult](rs), nil
}

// entrypoint returns the fake's entry point for the test phase.
func (f *fakeRulesetAPI) entrypoint() *fakeRuleset {
	return f.entrypointIn(rulesTestPhase)
}

// entrypointIn returns the fake's entry point for phase.
func (f *fakeRulesetAPI) entrypointIn(phase rulesets.Phase) *fakeRuleset {
	rs, ok := f.entrypoints[phase]
	Expect(ok).To(BeTrue(), "the zone has no entry point in the %s phase", phase)
	return rs
}

// seed gives the zone an entry point in the test phase holding rules, as if
// they had been created through the API.
func (f *fakeRulesetAPI) seed(rules ...rulesets.RulesetNewParamsRule) {
	f.seedIn(rulesTestPhase, rules...)
}

// seedIn gives the zone an entry point for phase holding rules, as if
// written outside kflare.
func (f *fakeRulesetAPI) seedIn(phase rulesets.Phase, rules ...rulesets.RulesetNewParamsRule) {
	rs := &fakeRuleset{id: f.newID(), name: "default", kind: "zone", phase: phase, version: 1}
	for _, rule := range rules {
		rs.rules = append(rs.rules, f.stored(sent(rule), f.newID(), 1))
	}
	f.entrypoints[phase] = rs
}

// rulesetsError is the error cloudflare-go v7 returns when the Rulesets API
// answers with an HTTP status and a Cloudflare error code.
func rulesetsError(status, code int) error {
	return v7APIError(http.MethodPost, "zones/"+rulesTestZone+"/rulesets", status, code, "refused by the fake")
}

// forbidden is the error cloudflare-go v7 returns for a 403: a token without
// the permission the call needs. Cloudflare sends no error code with it.
func forbidden() error {
	return rulesetsError(http.StatusForbidden, 0)
}

// managedPhase is the phase whose emptied entry point kflare deletes.
const managedPhase = rulesets.PhaseHTTPRequestFirewallManaged

// executeConflictCode is the error code Cloudflare refuses a second execute
// rule for a managed ruleset the phase already runs with, as the fake does.
const executeConflictCode = 20014

// freeManagedRulesetID is the Cloudflare Free Managed Ruleset.
const freeManagedRulesetID = "77454fe2d30c4220b5701f6fdfb893ba"

// blockRule is a rule as a kflare controller would ask for it: every field it
// wants set, Cloudflare's defaults left out.
func blockRule(ref, path string) rulesets.RulesetNewParamsRule {
	var rule rulesets.RulesetNewParamsRule
	rule.Ref.Value, rule.Ref.Present = ref, true
	rule.Action.Value, rule.Action.Present = rulesets.RulesetNewParamsRulesActionBlock, true
	rule.Expression.Value, rule.Expression.Present = fmt.Sprintf("http.request.uri.path eq %q", path), true
	rule.Description.Value, rule.Description.Present = "block "+path, true
	return rule
}

// executeRule deploys the managed ruleset rulesetID to every request.
func executeRule(ref, rulesetID string) rulesets.RulesetNewParamsRule {
	var params rulesets.ExecuteRuleActionParametersParam
	params.ID.Value, params.ID.Present = rulesetID, true
	var rule rulesets.RulesetNewParamsRule
	rule.Ref.Value, rule.Ref.Present = ref, true
	rule.Action.Value, rule.Action.Present = rulesets.RulesetNewParamsRulesActionExecute, true
	rule.ActionParameters.Value, rule.ActionParameters.Present = params, true
	rule.Expression.Value, rule.Expression.Present = "true", true
	return rule
}

// skipRule skips the remaining rules for /health, and leaves its logging to
// Cloudflare's default.
func skipRule() rulesets.RulesetNewParamsRule {
	var params rulesets.SkipRuleActionParametersParam
	params.Ruleset.Value, params.Ruleset.Present = rulesets.SkipRuleActionParametersRulesetCurrent, true
	var rule rulesets.RulesetNewParamsRule
	rule.Ref.Value, rule.Ref.Present = "kflare-skip", true
	rule.Action.Value, rule.Action.Present = rulesets.RulesetNewParamsRulesActionSkip, true
	rule.ActionParameters.Value, rule.ActionParameters.Present = params, true
	rule.Expression.Value, rule.Expression.Present = `http.request.uri.path eq "/health"`, true
	return rule
}

func enabled(rule rulesets.RulesetNewParamsRule, on bool) rulesets.RulesetNewParamsRule {
	rule.Enabled.Value, rule.Enabled.Present = on, true
	return rule
}

func withParameters(rule rulesets.RulesetNewParamsRule, parameters any) rulesets.RulesetNewParamsRule {
	rule.ActionParameters.Value, rule.ActionParameters.Present = parameters, true
	return rule
}

func refs(rules []fakeRule) []string {
	out := make([]string, len(rules))
	for i, rule := range rules {
		out[i] = rule.ref()
	}
	return out
}

func peer(ref string, priority int32, name string) rulesetRulePeer {
	return rulesetRulePeer{ref: ref, priority: priority, namespace: "default", name: name, kind: "WAFCustomRule"}
}

// fieldNames lists the fields of the struct v holds.
func fieldNames(v any) []string {
	t := reflect.TypeOf(v)
	names := make([]string, 0, t.NumField())
	for field := range t.Fields() {
		names = append(names, field.Name)
	}
	return slices.Sorted(slices.Values(names))
}

var _ = Describe("Ruleset rules", func() {
	var (
		ctx context.Context
		api *fakeRulesetAPI
	)

	BeforeEach(func() {
		ctx = context.Background()
		api = newFakeRulesetAPI()
	})

	sync := func(rule rulesets.RulesetNewParamsRule, peers ...rulesetRulePeer) (rulesetRuleState, error) {
		return syncRulesetRule(ctx, api.api(), rulesTestZone, rulesTestPhase, rule, peers)
	}

	Describe("refs", func() {
		It("derives a stable ref of at most 128 bytes from the kind, namespace and name", func() {
			ref := rulesetRuleRef("WAFCustomRule", "web", "block-admin")
			Expect(ref).To(MatchRegexp(`^kflare-[0-9a-f]{32}$`))
			Expect(len(ref)).To(BeNumerically("<=", 128))
			Expect(rulesetRuleRef("WAFCustomRule", "web", "block-admin")).To(Equal(ref))
		})

		It("gives every resource its own ref", func() {
			all := []string{
				rulesetRuleRef("WAFCustomRule", "web", "a"),
				rulesetRuleRef("WAFCustomRule", "api", "a"),
				rulesetRuleRef("WAFCustomRule", "web", "b"),
				rulesetRuleRef("RateLimitRule", "web", "a"),
			}
			Expect(slices.Compact(slices.Sorted(slices.Values(all)))).To(HaveLen(len(all)))
		})
	})

	Describe("syncing a rule", func() {
		It("creates the zone's entry point, holding the rule, when there is none", func() {
			state, err := sync(blockRule("kflare-a", "/a"))
			Expect(err).NotTo(HaveOccurred())

			rs := api.entrypoint()
			Expect(rs.name).To(Equal("default"))
			Expect(rs.kind).To(Equal("zone"))
			Expect(refs(rs.rules)).To(Equal([]string{"kflare-a"}))
			Expect(state).To(Equal(rulesetRuleState{
				rulesetID: rs.id, ruleID: rs.rules[0].id(), version: "1", wrote: true,
			}))
		})

		It("adds the rule after the entry point's other rules and leaves them alone", func() {
			api.seed(blockRule("someone-else", "/x"))
			untouched := api.entrypoint().rules[0].clone()

			state, err := sync(blockRule("kflare-a", "/a"))
			Expect(err).NotTo(HaveOccurred())

			rs := api.entrypoint()
			Expect(refs(rs.rules)).To(Equal([]string{"someone-else", "kflare-a"}))
			Expect(rs.rules[0]).To(Equal(untouched))
			Expect(state).To(Equal(rulesetRuleState{rulesetID: rs.id, ruleID: rs.rules[1].id(), version: "1", wrote: true}))
		})

		It("sends every field of the rule, whether it creates or updates it", func() {
			var check rulesets.BlockRuleExposedCredentialCheckParam
			check.UsernameExpression.Value, check.UsernameExpression.Present = `http.request.body.form["user"][0]`, true
			check.PasswordExpression.Value, check.PasswordExpression.Present = `http.request.body.form["pass"][0]`, true
			rule := enabled(blockRule("kflare-a", "/a"), false)
			rule.ID.Value, rule.ID.Present = "rule-id", true
			rule.Categories.Value, rule.Categories.Present = []string{"login"}, true
			rule.ExposedCredentialCheck.Value, rule.ExposedCredentialCheck.Present = check, true
			rule.Logging.Value.Enabled.Value, rule.Logging.Value.Enabled.Present = false, true
			rule.Logging.Present = true
			rule.Ratelimit.Value, rule.Ratelimit.Present = map[string]any{"period": 10}, true
			rule.ActionParameters.Value, rule.ActionParameters.Present = blockResponseParameters(403, "text/plain", "no"), true
			want, err := rule.MarshalJSON()
			Expect(err).NotTo(HaveOccurred())

			api.seed(blockRule("kflare-b", "/b"))
			_, err = sync(rule, peer("kflare-a", 1, "a"), peer("kflare-b", 2, "b"))
			Expect(err).NotTo(HaveOccurred())
			Expect(api.lastBody).To(HaveKeyWithValue("position", map[string]any{"before": api.entrypoint().rules[1].id()}))
			delete(api.lastBody, "position")
			Expect(json.Marshal(api.lastBody)).To(MatchJSON(want))

			api.entrypoint().rules[0]["description"] = "changed outside kflare"
			_, err = sync(rule, peer("kflare-a", 1, "a"), peer("kflare-b", 2, "b"))
			Expect(err).NotTo(HaveOccurred())
			Expect(json.Marshal(api.lastBody)).To(MatchJSON(want))

			By("covering every field v7 has for a rule")
			Expect(fieldNames(rulesets.RuleNewParamsBody{})).To(Equal(
				slices.Sorted(slices.Values(append(fieldNames(rulesets.RulesetNewParamsRule{}), "Position")))))
			Expect(fieldNames(rulesets.RuleEditParamsBody{})).To(Equal(fieldNames(rulesets.RuleNewParamsBody{})))
		})

		It("writes nothing when the rule is already as desired, counting Cloudflare's defaults as set", func() {
			first, err := sync(skipRule())
			Expect(err).NotTo(HaveOccurred())
			Expect(api.entrypoint().rules[0].jsonOf("logging")).To(MatchJSON(`{"enabled":true}`),
				"Cloudflare logs skip rules by default")
			writes, version := api.writes, api.entrypoint().version

			second, err := sync(skipRule())
			Expect(err).NotTo(HaveOccurred())
			Expect(second).To(Equal(rulesetRuleState{rulesetID: first.rulesetID, ruleID: first.ruleID, version: "1"}))
			Expect(api.writes).To(Equal(writes))
			Expect(api.entrypoint().version).To(Equal(version))
		})

		It("counts the latest version, which Cloudflare records for an execute rule, as set", func() {
			deploy := executeRule("kflare-managed", freeManagedRulesetID)
			_, err := syncRulesetRule(ctx, api.api(), rulesTestZone, managedPhase, deploy, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(api.entrypointIn(managedPhase).rules[0].jsonOf("action_parameters")).To(
				MatchJSON(`{"id":"` + freeManagedRulesetID + `","version":"latest"}`))
			writes := api.writes

			state, err := syncRulesetRule(ctx, api.api(), rulesTestZone, managedPhase, deploy, nil)
			Expect(err).NotTo(HaveOccurred())
			Expect(state.wrote).To(BeFalse())
			Expect(api.writes).To(Equal(writes))
		})

		It("rewrites a rule changed outside kflare, keeping its ID and place", func() {
			api.seed(blockRule("someone-else", "/x"), blockRule("kflare-a", "/a"), blockRule("someone-else-2", "/y"))
			rs := api.entrypoint()
			ruleID := rs.rules[1].id()
			rs.rules[1]["expression"] = `http.request.uri.path eq "/changed"`
			rs.rules[1]["enabled"] = false

			state, err := sync(blockRule("kflare-a", "/a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(state.wrote).To(BeTrue())
			Expect(state.ruleID).To(Equal(ruleID))
			Expect(state.version).To(Equal("2"))

			rule := api.entrypoint().rules[1]
			Expect(rule.id()).To(Equal(ruleID))
			Expect(rule).To(HaveKeyWithValue("expression", `http.request.uri.path eq "/a"`))
			Expect(rule).To(HaveKeyWithValue("enabled", true))
		})

		It("adds its rule when another resource created the entry point first", func() {
			api.before = func(call string) {
				if call == "Rulesets.New" && api.entrypoints[rulesTestPhase] == nil {
					api.seed(blockRule("kflare-other", "/other"))
				}
			}

			state, err := sync(blockRule("kflare-a", "/a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(state.wrote).To(BeTrue())
			Expect(refs(api.entrypoint().rules)).To(Equal([]string{"kflare-other", "kflare-a"}))
		})

		It("tries once more when its rule disappears between the read and the write", func() {
			api.seed(blockRule("kflare-a", "/old"))
			api.before = func(call string) {
				if call == "Rules.Edit" {
					api.entrypoint().rules = nil
				}
			}

			state, err := sync(blockRule("kflare-a", "/a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(state.wrote).To(BeTrue())
			Expect(api.entrypoint().rules).To(HaveLen(1))
			Expect(api.entrypoint().rules[0]).To(HaveKeyWithValue("expression", `http.request.uri.path eq "/a"`))
		})

		It("reports Cloudflare's rejections", func() {
			api.maxRules = 1
			api.seed(blockRule("someone-else", "/x"))
			_, err := sync(blockRule("kflare-a", "/a"))
			Expect(cfpkg.HasErrorCode(err, 50001)).To(BeTrue(), "over the plan's rule quota")

			api.errs["Phases.Get"] = forbidden()
			_, err = sync(blockRule("kflare-a", "/a"))
			Expect(cfpkg.IsTerminalError(err)).To(BeTrue())
		})

		It("reports a failure to create the entry point", func() {
			api.errs["Rulesets.New"] = rulesetsError(http.StatusBadRequest, 20166)
			_, err := sync(blockRule("kflare-a", "/a"))
			Expect(cfpkg.HasErrorCode(err, 20166)).To(BeTrue())
		})

		It("reports a failure to read the entry point another resource created first", func() {
			api.before = func(call string) {
				if call == "Rulesets.New" {
					api.seed(blockRule("kflare-other", "/other"))
					api.errs["Phases.Get"] = forbidden()
				}
			}
			_, err := sync(blockRule("kflare-a", "/a"))
			Expect(cfpkg.IsTerminalError(err)).To(BeTrue())
		})

		It("reports a failed create", func() {
			api.seed(blockRule("someone-else", "/x"))
			api.errs["Rules.New"] = rulesetsError(http.StatusBadRequest, 20127)
			_, err := sync(blockRule("kflare-a", "/a"))
			Expect(cfpkg.HasErrorCode(err, 20127)).To(BeTrue())
		})

		It("reports a failed update", func() {
			api.seed(blockRule("kflare-a", "/old"))
			api.errs["Rules.Edit"] = rulesetsError(http.StatusBadRequest, 20023)
			_, err := sync(blockRule("kflare-a", "/a"))
			Expect(cfpkg.HasErrorCode(err, 20023)).To(BeTrue())
		})

		It("reports a rule that disappears after it was written", func() {
			api.seed(blockRule("kflare-b", "/b"), blockRule("kflare-a", "/a"), blockRule("kflare-c", "/c"))
			api.before = func(call string) {
				if call == "Rules.Edit position" {
					// Someone deletes kflare's rule while it reorders the others.
					rs := api.entrypoint()
					rs.rules = slices.DeleteFunc(rs.rules, func(r fakeRule) bool { return r.ref() == "kflare-c" })
				}
			}

			_, err := sync(blockRule("kflare-c", "/c"),
				peer("kflare-a", 1, "a"), peer("kflare-b", 2, "b"), peer("kflare-c", 3, "c"))
			Expect(err).To(MatchError(ContainSubstring("disappeared")))
			Expect(api.moves).To(Equal(1))
		})
	})

	Describe("ordering", func() {
		It("places a new rule right after the closest peer that sorts before it", func() {
			api.seed(blockRule("kflare-a", "/a"), blockRule("someone-else", "/x"), blockRule("kflare-c", "/c"))

			state, err := sync(blockRule("kflare-b", "/b"),
				peer("kflare-a", 1, "a"), peer("kflare-b", 2, "b"), peer("kflare-c", 3, "c"))
			Expect(err).NotTo(HaveOccurred())
			Expect(state.moved).To(BeFalse())
			Expect(refs(api.entrypoint().rules)).To(Equal([]string{"kflare-a", "kflare-b", "someone-else", "kflare-c"}))
		})

		It("places a new rule right before the closest peer that sorts after it when none sorts before", func() {
			api.seed(blockRule("someone-else", "/x"), blockRule("kflare-b", "/b"), blockRule("kflare-c", "/c"))

			_, err := sync(blockRule("kflare-a", "/a"),
				peer("kflare-a", 1, "a"), peer("kflare-b", 2, "b"), peer("kflare-c", 3, "c"))
			Expect(err).NotTo(HaveOccurred())
			Expect(refs(api.entrypoint().rules)).To(Equal([]string{"someone-else", "kflare-a", "kflare-b", "kflare-c"}))
		})

		It("appends a rule when none of its peers' rules exists yet", func() {
			api.seed(blockRule("someone-else", "/x"))

			_, err := sync(blockRule("kflare-b", "/b"), peer("kflare-a", 1, "a"), peer("kflare-b", 2, "b"))
			Expect(err).NotTo(HaveOccurred())
			Expect(refs(api.entrypoint().rules)).To(Equal([]string{"someone-else", "kflare-b"}))
		})

		It("moves peers into priority order without changing them, and leaves other rules in place", func() {
			api.seed(
				blockRule("someone-else", "/x"),
				blockRule("kflare-c", "/c"),
				blockRule("someone-else-2", "/y"),
				blockRule("kflare-a", "/a"),
				blockRule("kflare-b", "/b"),
			)
			before := map[string]fakeRule{}
			for _, rule := range api.entrypoint().rules {
				before[rule.ref()] = rule.clone()
			}

			state, err := sync(blockRule("kflare-a", "/a"),
				peer("kflare-c", 3, "c"), peer("kflare-a", 1, "a"), peer("kflare-b", 2, "b"))
			Expect(err).NotTo(HaveOccurred())
			Expect(state.wrote).To(BeFalse())
			Expect(state.moved).To(BeTrue())
			Expect(api.moves).To(Equal(1))
			Expect(refs(api.entrypoint().rules)).To(Equal(
				[]string{"someone-else", "someone-else-2", "kflare-a", "kflare-b", "kflare-c"}))
			for _, rule := range api.entrypoint().rules {
				Expect(rule).To(Equal(before[rule.ref()]), "a move must not change rule %s", rule.ref())
			}

			By("leaving rules that are already in order alone")
			again, err := sync(blockRule("kflare-a", "/a"),
				peer("kflare-c", 3, "c"), peer("kflare-a", 1, "a"), peer("kflare-b", 2, "b"))
			Expect(err).NotTo(HaveOccurred())
			Expect(again.moved).To(BeFalse())
			Expect(api.moves).To(Equal(1))
		})

		It("breaks priority ties by namespace, name and kind", func() {
			api.seed(
				blockRule("kflare-4", "/4"), blockRule("kflare-3", "/3"),
				blockRule("kflare-2", "/2"), blockRule("kflare-1", "/1"),
			)
			peers := []rulesetRulePeer{
				{ref: "kflare-4", priority: 5, namespace: "b", name: "a", kind: "WAFCustomRule"},
				{ref: "kflare-3", priority: 5, namespace: "a", name: "b", kind: "WAFCustomRule"},
				{ref: "kflare-2", priority: 5, namespace: "a", name: "a", kind: "WAFCustomRule"},
				{ref: "kflare-1", priority: 5, namespace: "a", name: "a", kind: "RateLimitRule"},
			}

			_, err := sync(blockRule("kflare-1", "/1"), peers...)
			Expect(err).NotTo(HaveOccurred())
			Expect(refs(api.entrypoint().rules)).To(Equal([]string{"kflare-1", "kflare-2", "kflare-3", "kflare-4"}))
		})

		It("stops, and says so, when a rule disappears while it reorders", func() {
			api.seed(blockRule("kflare-b", "/b"), blockRule("kflare-a", "/a"))
			api.before = func(call string) {
				if call == "Rules.Edit position" {
					rs := api.entrypoint()
					rs.rules = slices.DeleteFunc(rs.rules, func(r fakeRule) bool { return r.ref() == "kflare-b" })
				}
			}

			state, err := sync(blockRule("kflare-a", "/a"), peer("kflare-a", 1, "a"), peer("kflare-b", 2, "b"))
			Expect(err).NotTo(HaveOccurred())
			Expect(state.moved).To(BeTrue(), "the caller must check the order again")
			Expect(refs(api.entrypoint().rules)).To(Equal([]string{"kflare-a"}))
		})

		It("reports a failed move", func() {
			api.seed(blockRule("kflare-b", "/b"), blockRule("kflare-a", "/a"))
			api.errs["Rules.Edit position"] = rulesetsError(http.StatusBadRequest, 0)

			_, err := sync(blockRule("kflare-a", "/a"), peer("kflare-a", 1, "a"), peer("kflare-b", 2, "b"))
			Expect(cfpkg.IsTerminalError(err)).To(BeTrue())
		})
	})

	Describe("deleting a rule", func() {
		del := func(ref string) error {
			return deleteRulesetRule(ctx, api.api(), rulesTestZone, rulesTestPhase, ref, false)
		}
		delManaged := func(ref string, peersRemain bool) error {
			return deleteRulesetRule(ctx, api.api(), rulesTestZone, managedPhase, ref, peersRemain)
		}
		hasEntrypoint := func(phase rulesets.Phase) bool {
			_, ok := api.entrypoints[phase]
			return ok
		}

		It("deletes the rule with its ref and leaves the others", func() {
			api.seed(blockRule("someone-else", "/x"), blockRule("kflare-a", "/a"))
			Expect(del("kflare-a")).To(Succeed())
			Expect(refs(api.entrypoint().rules)).To(Equal([]string{"someone-else"}))
		})

		It("leaves the entry point in place when it deletes the last rule", func() {
			api.seed(blockRule("kflare-a", "/a"))
			Expect(del("kflare-a")).To(Succeed())
			Expect(api.entrypoint().rules).To(BeEmpty())
		})

		It("counts a missing entry point, rule or concurrently deleted rule as deleted", func() {
			Expect(del("kflare-a")).To(Succeed())

			api.seed(blockRule("someone-else", "/x"))
			Expect(del("kflare-a")).To(Succeed())
			Expect(api.writes).To(BeZero())

			api.seed(blockRule("kflare-a", "/a"))
			api.before = func(call string) {
				if call == "Rules.Delete" {
					api.entrypoint().rules = nil
				}
			}
			Expect(del("kflare-a")).To(Succeed())
		})

		It("reports other errors", func() {
			api.errs["Phases.Get"] = forbidden()
			Expect(cfpkg.IsTerminalError(del("kflare-a"))).To(BeTrue())

			api.errs["Phases.Get"] = nil
			api.seed(blockRule("kflare-a", "/a"))
			api.errs["Rules.Delete"] = rulesetsError(http.StatusBadRequest, 0)
			Expect(cfpkg.IsTerminalError(del("kflare-a"))).To(BeTrue())
		})

		Describe("in a phase whose emptied entry point kflare deletes", func() {
			It("deletes the entry point it empties, which brings back Cloudflare's default deployment", func() {
				api.seedIn(managedPhase, executeRule("kflare-a", freeManagedRulesetID))
				Expect(delManaged("kflare-a", false)).To(Succeed())
				Expect(hasEntrypoint(managedPhase)).To(BeFalse())
			})

			It("deletes an entry point left empty even when the rule had gone already", func() {
				api.seedIn(managedPhase)
				Expect(delManaged("kflare-a", false)).To(Succeed())
				Expect(hasEntrypoint(managedPhase)).To(BeFalse())
			})

			It("keeps the emptied entry point while other kflare resources are about to write to it", func() {
				api.seedIn(managedPhase, executeRule("kflare-a", freeManagedRulesetID))
				Expect(delManaged("kflare-a", true)).To(Succeed())
				Expect(api.entrypointIn(managedPhase).rules).To(BeEmpty())
			})

			It("keeps the entry point while it holds other rules", func() {
				api.seedIn(managedPhase, executeRule("someone-else", freeManagedRulesetID),
					executeRule("kflare-a", "efb7b8c949ac4650a09736fc376e9aee"))
				Expect(delManaged("kflare-a", false)).To(Succeed())
				Expect(refs(api.entrypointIn(managedPhase).rules)).To(Equal([]string{"someone-else"}))
			})

			It("reads the entry point again when the rule went meanwhile", func() {
				api.seedIn(managedPhase, executeRule("kflare-a", freeManagedRulesetID))
				api.before = func(call string) {
					if call == "Rules.Delete" {
						api.entrypointIn(managedPhase).rules = nil
					}
				}
				Expect(delManaged("kflare-a", false)).To(Succeed())
				Expect(hasEntrypoint(managedPhase)).To(BeFalse())
			})

			It("counts an entry point deleted meanwhile as deleted", func() {
				api.seedIn(managedPhase, executeRule("kflare-a", freeManagedRulesetID))
				api.before = func(call string) {
					if call == "Rules.Delete" {
						delete(api.entrypoints, managedPhase)
					}
				}
				Expect(delManaged("kflare-a", false)).To(Succeed())
				Expect(hasEntrypoint(managedPhase)).To(BeFalse())

				api.before = nil
				api.seedIn(managedPhase)
				api.errs["Rulesets.Delete"] = rulesetsError(http.StatusNotFound, 10001)
				Expect(delManaged("kflare-a", false)).To(Succeed())
			})

			It("reports errors reading the entry point again or deleting it", func() {
				api.seedIn(managedPhase, executeRule("kflare-a", freeManagedRulesetID))
				api.before = func(call string) {
					if call == "Rules.Delete" {
						api.entrypointIn(managedPhase).rules = nil
						api.errs["Phases.Get"] = forbidden()
					}
				}
				Expect(cfpkg.IsTerminalError(delManaged("kflare-a", false))).To(BeTrue())

				api.before, api.errs["Phases.Get"] = nil, nil
				api.errs["Rulesets.Delete"] = forbidden()
				Expect(cfpkg.IsTerminalError(delManaged("kflare-a", false))).To(BeTrue())
				Expect(hasEntrypoint(managedPhase)).To(BeTrue())
			})

			It("leaves the entry points of other phases in place", func() {
				api.seed(blockRule("kflare-a", "/a"))
				Expect(del("kflare-a")).To(Succeed())
				Expect(hasEntrypoint(rulesTestPhase)).To(BeTrue())
			})
		})
	})

	// stored is a rule as Cloudflare returns it: Cloudflare's fields, then
	// those of the definition.
	const stored = `"id":"id01","version":"4","last_updated":"2026-10-12T19:00:00Z",`
	const blockA = `"ref":"kflare-a","action":"block","expression":"http.request.uri.path eq \"/a\"",` +
		`"description":"block /a"`
	const skipHealth = `"ref":"kflare-skip","action":"skip","action_parameters":{"ruleset":"current"},` +
		`"expression":"http.request.uri.path eq \"/health\""`

	DescribeTable("comparing a desired rule with Cloudflare's",
		func(desired rulesets.RulesetNewParamsRule, actual string, same bool) {
			Expect(rulesetRuleMatches(desired, actual)).To(Equal(same))
		},
		Entry("ignores the fields Cloudflare sets",
			blockRule("kflare-a", "/a"), `{`+stored+blockA+`,"enabled":true}`, true),
		Entry("counts an unset enabled as enabled",
			blockRule("kflare-a", "/a"), `{`+blockA+`,"enabled":true}`, true),
		Entry("tells a disabled rule apart",
			enabled(blockRule("kflare-a", "/a"), true), `{`+blockA+`,"enabled":false}`, false),
		Entry("counts an unset skip logging as logged",
			skipRule(), `{`+skipHealth+`,"enabled":true,"logging":{"enabled":true}}`, true),
		Entry("tells a skip rule that does not log apart",
			skipRule(), `{`+skipHealth+`,"logging":{"enabled":false}}`, false),
		Entry("counts the latest version of the managed ruleset an execute rule runs as set",
			executeRule("kflare-a", freeManagedRulesetID),
			`{"ref":"kflare-a","action":"execute","expression":"true","enabled":true,`+
				`"action_parameters":{"id":"`+freeManagedRulesetID+`","version":"latest"}}`, true),
		Entry("tells a different expression apart",
			blockRule("kflare-a", "/a"),
			`{"ref":"kflare-a","action":"block","expression":"http.request.uri.path eq \"/b\"","description":"block /a"}`,
			false),
		Entry("tells a different description apart",
			blockRule("kflare-a", "/a"),
			`{"ref":"kflare-a","action":"block","expression":"http.request.uri.path eq \"/a\""}`, false),
		Entry("tells a field set outside kflare apart",
			blockRule("kflare-a", "/a"), `{`+blockA+`,"ratelimit":{"period":10}}`, false),
		Entry("counts empty action parameters as none",
			withParameters(blockRule("kflare-a", "/a"), rulesets.BlockRuleActionParametersParam{}),
			`{`+blockA+`}`, true),
		Entry("counts empty lists as none",
			withParameters(skipRule(), map[string]any{"ruleset": "current", "phases": []string{}}),
			`{`+skipHealth+`}`, true),
		Entry("tells different action parameters apart",
			withParameters(skipRule(), map[string]any{"phases": []string{"http_ratelimit"}}),
			`{`+skipHealth+`}`, false),
		Entry("does not match what is not a rule",
			blockRule("kflare-a", "/a"), `[]`, false),
	)
})
