# Code generator (`kflare-gen`)

Claude Code reads this file when it works on files in `generator/`. It is kflare-gen's plan of record and its
as-built notes, moved unchanged from the root CLAUDE.md on 2026-10-20. How to add a kind with kflare-gen: the root
CLAUDE.md's [Adding a kind](../CLAUDE.md#adding-a-kind). What each kind's entry rests on, verified live:
[docs/design-notes.md](../docs/design-notes.md).

**Depends on:** `feat/shared-client`, `feat/rulesets-foundation`; eight PRs, in order
**Status:** ✅ Complete: PRs 1 to 8 (`feat/cloudflare-go-v7`, `feat/crud-reconciler`, `feat/generator-types`,
`feat/generator-openapi`, `feat/generator-controllers`, `feat/d1-database-controller`, `feat/rulesets-v7`,
`feat/generator-rule-kinds`). Planned and decided 2026-10-13. Since then, a third shape: settings kinds
(`feat/generator-singletons`, notes below the table)

Most of a new kind is the same wiring every time. IPList (#26) changed 27 files (+2,596 lines). Besides its types and
its bulk-operation logic, it repeated `Get/SetConditions`, registration in `cmd/main.go`, the CRD kustomization, the
Helm `edit` role, the account watch and protection entry, the narrow API interface and its factory, and the lifecycle
specs every CRUD kind has (finalizer, account not ready, invalid token, create and claim, idle reconcile writes
nothing, update in place, adopt, conflict, recreate, retain, failed deletions retried). `kflare-gen` generates that
part from Cloudflare's own description of its API. What Cloudflare does not describe stays hand-written.

**Decided (2026-10-13):**
1. Generated kinds use cloudflare-go **v7** from the start, bringing
   [SDK decision](../docs/design-notes.md#sdk-decision) step 3 forward for them only. v0.119 is hand-written, with no
   machine-readable list of its resources and operations to generate from
2. Fields are **opt-out**: a generated kind has every field v7 exposes for it, minus those its entry ignores. The
   output is committed, so each field an SDK bump adds shows up in review
3. `ManagedRuleset` is built by hand first, as planned (✅); `feat/rulesets-v7` moved its conversion to v7 (✅)
4. The first new kind generated end to end is a free, account-level resource: a D1 database or a Turnstile widget,
   picked when that PR starts, after checking the Free-plan limits and the token permission. Picked 2026-10-15:
   `D1Database` (see [its section](../docs/design-notes.md#branch-featd1-database-controller))

**Sources (checked 2026-10-13):**
- cloudflare-go v7.12.0 has an `api.md` in every package that maps each method to its verb and route, for example
  `post /accounts/{account_id}/storage/kv/namespaces` → `client.KV.Namespaces.New`. Of its 939 services, 213 are
  CRUD-shaped (`New`, plus `Get` and `Delete` by ID: 157 account-level, 37 zone-level, 13 either, 6 other such as
  zones themselves) and 90 are settings-style singletons (`Get` plus `Update` or `Edit`, without `New` or `Delete`).
  Its Go types give the fields, required markers (`api:"required"`) and enums (typed string constants), but no
  lengths, patterns, ranges or defaults
- The OpenAPI spec ([cloudflare/api-schemas](https://github.com/cloudflare/api-schemas), BSD-3-Clause; `openapi.json`
  is OpenAPI 3.0.3, 27 MB, 2,339 paths) has those: a KV title of at most 512 characters, a list name of at most 50
  with a pattern. It also carries Stainless's hints, `x-sensitive`, `x-stainless-collection-type: set` and
  `x-stainless-terraform-configurability` (`computed`, `optional`, `computed_optional`), and per operation
  `x-api-token-group` (API permission names) and `x-cfPlanAvailability`
- Immutability can be read off the SDK. The fields kflare made immutable by hand are v7's create-only fields: R2's
  `name` and `locationHint` and the list's `name` are in the create params but not the update params. R2's
  `storageClass` is in `Edit`, so mutable, as Deferred says, and v7 sends the jurisdiction and storage-class headers
  that v0.119 cannot
- The OpenAPI spec runs ahead of the SDK (its KV create takes a `mode` that v7.12.0 lacks; v7.13.0 added it, and
  KVNamespace ignores it), and it is not always the live API: its list-name pattern allows uppercase, which Cloudflare
  refuses (10029, see IPList)

**Design.** It follows ACK's code-generator (AWS Controllers for Kubernetes): per-kind YAML plus the SDK's model, and
generated code on a shared runtime. Unlike ACK, hooks are Go functions rather than templates, and API defaults become
CRD defaults, so nothing is late-initialized.

- **Inputs:**
  - `generator/generator.yaml`, one entry per kind: kind, short name and noun, the v7 service, scope, doc, ID and
    claim label, the field resources adopt objects by, the kinds that depend on it, sample values, field adjustments
    (doc, print column, and a `validation` that replaces the spec's, with the reason), fixed values and ignored fields
    with their reasons
  - v7's Go types, loaded with `go/packages` at the version in `go.mod`, so the generated glue is type-checked
    against the SDK it calls
  - A subset of the OpenAPI spec under `generator/openapi/`: the operations and schemas the configured kinds use,
    written by `kflare-gen sync-spec` from a pinned api-schemas commit. `make codegen` reads only the subset, so it
    runs offline and constraint changes are reviewed. Operations match the SDK by verb and route from `api.md` (a
    `/{accounts_or_zones}/…` route matches both the `/accounts/…` and `/zones/…` paths), fields by JSON path. The
    SDK decides which fields exist; the OpenAPI spec only adds constraints and hints
- **Mapping defaults** (an entry can override any of them):
  - snake_case → lowerCamel JSON with Go initialisms (`statusCode`, `namespaceID`); field docs from the SDK comments
  - `account_id` / `zone_id` → an immutable `accountRef` / `zoneRef`, which also brings the credential chain, the
    parent watch and the protection entry
  - Create-only fields → `self == oldSelf`, with an envtest case each (convention 7). OpenAPI lengths, patterns,
    ranges and defaults → kubebuilder markers; typed string constants → `Enum`
  - API defaults become CRD defaults, so a resource's spec and Cloudflare's copy compare directly. A field Cloudflare
    fills in itself (`computed_optional` without a default) is compared only when the resource sets it: DNSRecord's
    `priority` rule, generalized
  - Sets → `+listType=set`, sorted in the conversion (as WAFCustomRule and RateLimitRule do by hand); `x-sensitive` →
    a `SecretKeySelector` (not built yet: kflare-gen refuses an `x-sensitive` field until then); the ID and computed
    fields → `status.cloudflareMetadata`
  - Unions → one struct with the fields of every variant and a CEL rule that a value matches a variant (built for rule
    kinds' action parameters; see the rule kinds' notes below). Maps of objects → a list keyed by the field the entry
    names (`listType=map`), and so is a list of objects whose items a field of their own tells apart (`listMapKey`)
  - A field that Cloudflare's read cannot return is refused unless the entry says how to detect its drift (a hash in
    status, as WorkerScript does) or ignores it
- **Outputs**, marked `// Code generated by kflare-gen. DO NOT EDIT.` and committed. `make codegen` runs before
  controller-gen, and CI's `git diff --exit-code` catches stale output:
  - `api/v1alpha1/zz_generated.<kind>_types.go`: spec, status, list, markers, print columns, `Get/SetConditions`
  - `internal/controller/zz_generated.<kind>.go`: the narrow v7 interface, spec → create and update params, response →
    status, the drift comparison, RBAC markers and the kind's descriptor
  - `internal/controller/zz_generated_kinds.go`: registration (`cmd/main.go` calls `SetupGeneratedReconcilers` once),
    the CRUD kinds' entries as users of their account, the rule kinds' with the shared rule controller, and the rule
    and settings kinds' as dependents of their Zone; the kinds' lines in the CRD kustomization and the Helm `edit`
    role, between `+kflare-gen:begin` and `+kflare-gen:end`; and a sample in `config/samples`
  - `internal/controller/zz_generated.<kind>_test.go`: an envtest table per kind, for the lifecycle specs above and the
    immutability and validation cases (`crudresource_specs_test.go`)
- **Shared CRUD reconciler** (hand-written, `crudresource_controller.go`, the counterpart of
  `rulesetrule_controller.go`): ARCHITECTURE.md's reconcile loop for any kind a `crudResourceKind` describes, with
  adoption by key and claim labels (`claimedIDs`/`claimID`), recreation after a 404, writes only on drift, the retain
  policy and waiting for dependents. Deletion blockers are data: `{code: 10008, reason: BucketNotEmpty}`,
  `{code: 10041, reason: ListInUse}` (not built yet: they come with the first generated kind that needs one). It
  carries the coverage target, which excludes generated files
- **Hooks:** optional Go functions in a hand-written `<kind>_hooks.go`, compiled and linted like any other code: adjust
  the desired params, normalize Cloudflare's copy before comparing, extra steps after a sync, extra watches. IPList's
  bulk operation and RateLimitRule's `cf.colo.id` are this kind of logic. Not built yet: no generated kind has needed
  one, since entry validations, CEL rules, ignores and tests covered every rule kind

```yaml
kinds:
  - kind: KVNamespace
    service: kv.NamespaceService      # <v7 package>.<service type>: its New, Get and Update (or Edit) give the fields
    scope: account                    # account_id → an immutable accountRef
    shortName: cfkv
    noun: KV namespace                # in docs and messages
    doc: |
      KVNamespace is the Schema for the kvnamespaces API. ...
    id: {field: id, status: namespaceID}
    claimLabel: kflare.dev/kv-namespace-id
    adopt: {by: title}                # a resource adopts the account's object with its title (TitleConflict if taken)
    dependents:                       # the kinds that use it: it waits for them before it is deleted
      - kind: WorkerScript
        refs: boundKVNamespaces       # a hand-written function: the KVNamespaces a WorkerScript binds
        reason: Cloudflare deletes a namespace a Worker is bound to, but every later upload of that Worker then fails.
    sample: {title: example-kv}       # the sample's spec, which the generated specs start from
    fields:
      title:                          # keys are the API's field names
        printColumn: Title
        validation: {minLength: 1, maxLength: 512, reason: The spec gives no minimum, and Cloudflare refuses ...}
    ignore:                           # every other field of the service is generated, so leaving one out takes a reason
      jurisdiction: Not sent by the controller, and restricting a namespace to a jurisdiction is not verified live yet.
      mode: Not sent by the controller, and KV Instant namespaces (cloudflare-go v7.13.0) are not verified live yet.
      supports_url_encoding: Not observed by the controller yet.
```

**PRs**, one at a time against main:

| # | Branch | Change | Done when |
|---|--------|--------|-----------|
| 1 | `feat/cloudflare-go-v7` | ✅ v7.12.0 next to v0.119: `pkg/cloudflare.Client` carries v7's request options (`V7Options`), from which controllers build the services they call, and `errors.go` also classifies `*cloudflare.Error` (see the [v7 notes](../docs/design-notes.md#sdk-decision)) | Unit tests pass. Nothing uses v7 yet |
| 2 | `feat/crud-reconciler` | ✅ The shared CRUD reconciler, with KVNamespace hand-ported onto it and onto v7, whose `Get` ends the list-and-match workaround (see the notes below the table) | KVNamespace's specs (fake adapted) and the live e2e pass |
| 3 | `feat/generator-types` | ✅ `generator/`: SDK loader, model, `generator.yaml`, types emitter, golden tests, `make codegen` (see the notes below the table) | KVNamespace's regenerated CRD matches the hand-written one, or differs only on purpose |
| 4 | `feat/generator-openapi` | ✅ `kflare-gen sync-spec`, the committed subset, constraints and hints (see the notes below the table) | Golden tests: the constraints generated for KVNamespace, R2Bucket and IPList match their CRDs, or the entry says why not |
| 5 | `feat/generator-controllers` | ✅ The controller emitter: descriptor, conversions, drift, RBAC, registration, protection, sample, envtest table (see the notes below the table) | KVNamespace is fully generated and its hand port deleted; its specs and the live e2e pass |
| 6 | `feat/d1-database-controller` | ✅ `D1Database`, the first new kind, generated end to end (decision 4; see [its section](../docs/design-notes.md#branch-featd1-database-controller)) | Only its entry, any hooks and its live e2e spec are hand-written |
| 7 | `feat/rulesets-v7` | ✅ The Rulesets foundation on v7: `Phases.Get`, `Rulesets.New` and `Rules.New/Edit/Delete` (with positions) replace the `Raw` calls in `pkg/cloudflare/rulesets.go`; the rule kinds' conversions move to v7 (see the notes below the table) | Rule specs and the live e2e pass, with the rule delete checked live again (v0.119's fails) |
| 8 | `feat/generator-rule-kinds` | ✅ A rule-kind emitter: the action parameters' types from v7's rule variants (`rulesets.RewriteRuleParam` and siblings) and the `definition` conversion, with `TransformRule` as its first kind (see the notes below the table) | `TransformRule`'s live e2e passes. The other four Rules kinds follow, one PR each |

- **The shared CRUD reconciler as built** (`feat/crud-reconciler`, 2026-10-14): `crudResourceKind[O, A, R]` (the API
  type, the narrow v7 interface, the Cloudflare object) holds the kind's name and noun, its constructors, accessors
  (`accountRef`, adoption `key`, `statusID`, `observe`; `id`, `objectKey`, `matches` on the object), the claim label and
  conflict reason, one function per call (`get`, `list` for one page, `create`, `update`, `delete`) and its
  `dependents`. `crudResourceReconciler` runs the loop, pages through the list for adoption (`findInPages`: from page
  1, until a page is empty or shorter than the `per_page` Cloudflare reports) and watches the account and the deletion
  of each dependent kind (`dependentKind.newObject`). A kind's exported reconciler only builds it, as the rule kinds
  do. Account-scoped kinds only, for now; deletion blockers (`BucketNotEmpty`, `ListInUse`) come with the first kind
  that needs one. Hand-written, the KVNamespace descriptor is about 70 lines: what PR 5 generates
- **kflare-gen's types as built** (`feat/generator-types`, 2026-10-14): `go run ./generator` (`make codegen`, which
  `make manifests` and `make generate` now run first) loads each entry's service with `go/packages` and writes
  `api/v1alpha1/zz_generated.<kind>_types.go`, removing those of kinds no longer listed. The spec is `accountRef` plus
  New's body params, in the SDK's order; status holds the ID plus the fields of Get's object that New does not take. A
  field missing from Update (or Edit) is immutable, and an optional one also gets the spec-level `has()` rule, the
  pattern R2Bucket's `locationHint` already used. Enums come from the SDK's typed constants, in its order, on spec
  fields only: a value Cloudflare adds must not make status updates fail. Docs come from the SDK unless the entry
  replaces them. Refused unless ignored, with the field named: numbers (v7 types them all `float64`; only the OpenAPI
  spec tells an integer), nested objects and arrays, unions, header and query params, update-only fields, and a scope
  without `account_id`. An entry naming a field the service lacks is refused too, so a typo or an SDK bump that removes
  a field shows up. KVNamespace's types are now generated: its CRD is unchanged but for two descriptions that say "KV
  namespace", the entry's noun. Golden tests (`generator/testdata`) cover every field of the KV namespace service and a
  custom list, and `TestCommittedTypesAreUpToDate` fails when the committed output is stale
- **The OpenAPI subset as built** (`feat/generator-openapi`, 2026-10-14): `generator/openapi/openapi.json` holds the
  operations of each configured kind's New, Get and Update (or Edit) calls, found by the route in the SDK's `api.md`
  (path parameters' names ignored), and every component they reach; the api-schemas license sits next to it. `make
  sync-spec` downloads `openapi.json` at `API_SCHEMAS_COMMIT` (`7350e9923848`, the last before v7.12.0's release, so
  that spec and SDK agree: the KV create's `mode` came later) and rewrites it; `make codegen` reads only the subset.
  From the spec come lengths, patterns, ranges (with exclusive bounds), defaults (strings always quoted), enums (the
  spec's, else the SDK's) and integers (`type: integer` → `int64`; a `number` stays refused: custom lists' `num_items`
  is one). `x-sensitive` fields are refused until they can go to a Secret. Status fields take no validation. An entry's
  `validation` replaces the spec's for a field, with the reason, and `fixed` keeps a field out of the spec with the
  value kflare always sends (`kind: ip` for an IP list). Response envelopes declare a generic `result` next to the
  specific one, so when `allOf` parts declare a property twice, the one with more properties wins.
  `TestConstraintsMatchTheHandWrittenCRDs` builds KVNamespace, R2Bucket and IPList (the latter two as test kinds) and
  compares every spec field's validation with their CRDs: they match, with three replacements the spec got wrong, each
  verified live: R2 names 3–63 characters, anchored (the spec: up to 64, unanchored), list names without capitals, and
  a KV title of at least one character. `TestCommittedSubsetReproduces` runs sync-spec on the subset itself and gets
  it back byte for byte
- **kflare-gen's controllers as built** (`feat/generator-controllers`, 2026-10-14): for each kind, kflare-gen writes
  the controller (`internal/controller/zz_generated.<kind>.go`: the narrow interface over the service's New, Get,
  List, Update or Edit, and Delete, the reconciler and its RBAC markers, and the `crudResourceKind`), an envtest table
  (`zz_generated.<kind>_test.go`: a fake of the service on `fakeCRUDStore`, the sample's spec, and edits of it) and
  a sample (`config/samples`). `zz_generated_kinds.go` registers the kinds (`SetupGeneratedReconcilers`, which
  `cmd/main.go` calls) and adds them to `accountDependents`, whose kinds the CloudflareAccount controller now watches
  in a loop. `make codegen` also keeps the kinds' lines in `config/crd/kustomization.yaml` and the Helm `edit` role,
  between `+kflare-gen:begin` and `+kflare-gen:end`, and removes the files it wrote for a kind no longer listed. New
  entry keys: `adopt.by` (a required string field Get reports; the conflict reason is its Go name and `Conflict`),
  `dependents` (a kind and a hand-written function returning the names a dependent uses, with the reason) and
  `sample` (values for every required field, and any string the specs must change; checked against the field's
  validation). The page size of the adoption list is the maximum of List's `per_page` in the OpenAPI spec, so the
  subset now holds List's operation too (KV: 1,000, as verified live). `crudresource_specs_test.go` runs the shared
  specs on each table: the finalizer, credentials, creation with every field, the claim, an idle reconcile that
  writes nothing, adoption from a later page and its conflict, recreation, each field updated in place, errors,
  deletion and retain, setup, the account watch, and the CRD (each immutable field; values below or above a length,
  outside an enum, out of range). A spec field is compared only when Update takes it; an optional one only when the
  resource sets it, so kflare never fights Cloudflare over a value it fills in, but it does not clear a value
  removed from the spec either: telling the two apart (`computed_optional`) waits for the first kind with optional
  fields. kflare-gen refuses what the shared CRUD reconciler cannot drive yet, saying why: New, Get and Update
  returning different types (custom lists), a List that is not a `V4PagePaginationArray` of Get's object (R2 buckets;
  D1 databases and Turnstile widgets list another type than Get returns, so PR 6 starts there), Get, List or Delete
  params it does not set, and Update params it does not send. `TestGoldenControllerCompiles` type-checks the golden
  controller and table of a second kind in `internal/controller`
- **The first generated kind as built** (`feat/d1-database-controller`, 2026-10-15): `D1Database` is its entry, its
  live e2e spec and generated code; it needs no hook. Two changes to the shared machinery came with it. A kind's list
  may hold another type than Get returns (D1's `DatabaseListResponse` against `D1`): `crudResourceKind` gained a
  fourth type parameter, `L`, with `listedID` and `listedKey` in place of `objectKey`, and adoption now finds the
  object in the list, then reads it by its ID (and creates one if it is gone by then). And an Update that takes
  nothing kflare sends is dropped: D1's takes only `read_replication`, which the entry ignores, so the kind has no
  Update and every field is immutable. Turnstile was not picked: its widgets need a list of domains, a secret that
  belongs in a Kubernetes Secret, and names Cloudflare does not keep unique
- **The Rulesets foundation on v7 as built** (`feat/rulesets-v7`, 2026-10-15): the rule kinds call cloudflare-go v7's
  rulesets services. `Phases.Get` reads an entry point; `Rulesets.New` creates one holding the rule, and
  `Rulesets.Delete` deletes the managed phase's emptied one; `Rules.New` adds a rule at a position, `Rules.Edit`
  replaces it or, holding only a position, moves it, and `Rules.Delete` deletes it. `RulesetRuleAPI` is now a struct of
  three narrow interfaces (`RulesetPhasesAPI`, `RulesetsAPI`, `RulesetRulesAPI`) that v7's services satisfy, and
  `pkg/cloudflare/rulesets.go` is gone. v7 has a rule type per action, and none shared for `challenge` and
  `js_challenge`, so a kind's conversion fills v7's generic rule, `rulesets.RulesetNewParamsRule`, with v7's typed
  params for its action (`SkipRuleActionParametersParam`, `ExecuteRuleActionParametersParam`,
  `BlockRuleRatelimitParam`, ...). `rulesetRuleDefinition` starts it with the fields every kind shares, which PR 8's
  generated conversions can use too, and the foundation copies it into the generic bodies of `Rules.New` and
  `Rules.Edit`; a spec fails if an SDK bump gives them a field the copy misses. A position is a map: v7 sends a
  struct's empty fields as well, and an empty `before` places a rule first. Drift compares the JSON v7 would send with
  Cloudflare's copy as v7 received it (`JSON.RawJSON()`), and the conversions send what v0 sent: optional fields only
  when set. The Rulesets fake keeps rules as JSON, reads every request from the JSON v7 sends and answers with v7's
  types decoded from JSON; the conversion tables pin each kind's JSON. 33 mutations of the sync, the comparison, the
  request bodies and the conversions were all caught. Live (run `ba4f94`, 15 of 15): idle reconciles wrote nothing,
  the all-at-once deletion deleted every rule through v7 (v0.119's delete fails on the same answer), kflare deleted the
  managed phase's emptied entry point, and the manager logged no error
- **The rule kinds as built** (`feat/generator-rule-kinds`, 2026-10-15): `generator.yaml` gained `ruleKinds`. An entry
  names v7's rule type for its action (`rule: rulesets.RewriteRuleParam`), its `phase` or its `types` (the values of an
  immutable `spec.type`, each picking a phase), one sample per type (a resource's spec), field adjustments by path
  (`headers[].value`), the field a map's entries are listed by (`key`), and CEL `rules`, each with its reason and an
  `invalid` spec it refuses. The rule type's other fields (logging, rate limit, exposed credential check) must be
  ignored with a reason. TransformRule went first because the dev token can already write its phases. kflare-gen walks
  the action parameters' v7 types together with the rule's OpenAPI schema (`rulesets_<Rule>`, which sync-spec now
  copies): an object becomes a struct (required from the schema or the SDK's `api:"required"`; `minProperties` and
  `maxProperties` a CEL count), an array a list (`listType=set`, sorted in the conversion, for unique scalars; their
  enums and patterns as `items:` markers), a map a list keyed by the entry's field (`listType=map`), and a union (an
  interface v7 declares, whose variants kflare-gen finds by type) one struct with every variant's fields, built as the
  variant that holds them all, with a CEL rule from the schema's `oneOf` (closed variants, as the live API treats them)
  or `anyOf`; enums merge across variants. An optional string that cannot be empty (an enum, a minimum length, or a
  pattern that refuses "") stays a plain string; other optional fields are pointers, so an empty query value differs
  from none. Per rule kind it writes the types (the spec embeds `RulesetRuleSpec`, status is `RulesetRuleStatus`), the
  controller (reconciler, RBAC, a type-to-phase map, the `rulesetRuleKind`, and one conversion function per struct, into
  the action's v7 params, which `rulesetRuleDefinition` wraps), the envtest table and a sample per type.
  `zz_generated_kinds.go` adds them to `rulesetRuleKinds` and to the Zone's dependents. In the foundation, a rule kind
  now lists its `phases`, and `phaseFor` picks a resource's from its spec; peers, deletion and the emptied-phase watches
  (`deletionsWatched`) go by the resource's phase. `rulesetrule_specs_test.go` runs shared specs on each table, from
  JSON specs decoded strictly into the API types: each sample's rule in its phase, as JSON kflare-gen renders itself (so
  the generated conversion is checked against a second rendering), and among its phase's peers; an idle reconcile; a
  change of every string leaf of every sample, plus the description and enabled, updated in place; deletion and retain
  in each sample's phase; the Zone waiting; zoneRef and type immutability; specs that set every field (to the samples'
  values, the OpenAPI schema's `example`, or values kflare-gen makes up; then again with each optional scalar at its
  zero value), converted without the API server, which would refuse them; and the CRD's refusals, sent as raw JSON so
  that an empty value or list reaches the API server: enums, empty keys, lengths, patterns, ranges, empty lists, a map
  or set listing an item twice, a breach of each schema rule (for a oneOf, each variant alone given each field outside
  it or left without each field it requires), and each entry rule's invalid spec. Golden tests add TestExecuteRule
  (nested objects, lists of objects, bools, integers, count rules) and TestSkipRule (sets of enums and patterns) from
  real rule types; `TestGoldenControllerCompiles` type-checks them in `internal/controller`, and
  `TestRuleKindRulesParse` parses every generated CEL rule with cel-go (since `feat/cache-rule-controller`, kflare-gen
  also evaluates the rules with it). A mutation check (of the foundation's phase handling, the generated conversion, the
  CRD and kflare-gen's edits) first left 12 of 26 mutations alive: deletion in another phase, the emptied-phase watches,
  a kind missing from the peers, fields no sample set, and the CRD's lengths, patterns, list bounds, map keys and
  variants. With the specs above, all 39 are caught. Live: run `7ed03a`, 16 of 16, and again with the final code, run
  `9c869b`, 16 of 16. A first run (`c951aa`) failed on the e2e's own header expression, `cf.colo.name`, which is no
  field (kflare reported Cloudflare's 20087 as `TerminalError`); the sample held it too. Both use `ip.src.country` now,
  and every sample's rule passed a dry run
- **Next:** the other Rules kinds, one entry and PR each: RedirectRule ✅ (`feat/redirect-rule-controller`), CacheRule ✅
  (`feat/cache-rule-controller`), ConfigurationRule ✅ (`feat/configuration-rule-controller`), and OriginRule ✅
  (`feat/origin-rule-controller`). The dev token can write all four phases since 2026-10-15
- **The settings kinds as built** (`feat/generator-singletons`, 2026-10-23): `generator.yaml` gained `settingsKinds`,
  for a zone's settings of one product, which always exist: the first of the Next wave's capabilities. An entry names a
  v7 service without `New` whose read (`Get`, or `List` where it returns the settings rather than a page), write
  (`Update` or `Edit`) and reset (`Delete`) each take only the zone's ID; kflare-gen refuses a service without one of
  them, or one that names its settings by an ID (zone settings, which need the reset to write defaults). The spec is
  `zoneRef` (`SettingsSpec`, shared and hand-written like `RulesetRuleSpec`) and the write's body params, walked by the
  rule kinds' param builder (objects, lists, maps and unions), its root's `zone_id` skipped; status is `SettingsStatus`,
  the zone's ID. The entry's `sample` is one spec, `tests` more, `fields` and `ignore` go by path as for rule kinds,
  plus `fields.<list>.listMapKey`: the field, required and a string or an integer, that tells a list's objects apart
  (`listType=map`). `unconfigured` gives, by path of API names (`managed_request_headers[].enabled`), the values the
  settings hold in a zone nobody has configured, with how that was verified; the sample must configure something. The
  runtime is `settings_controller.go`, with the JSON comparison in `settings.go`. Of the resources whose Zones point at
  the Cloudflare zone, in any namespace, the one whose status records the zone keeps configuring it, and otherwise the
  oldest does: TunnelConfiguration's plain "oldest" would hand a zone to an older resource whose Zone becomes ready
  later, which then finds the settings configured and reports `AlreadyExists`, leaving the zone to nobody. The others
  report `AlreadyConfigured`, and forget the zone and any write they were about to make, so that they check the settings
  again before they write them, once they configure them; settings that hold another value than `unconfigured` were
  configured outside kflare and are taken over only as the adoption policy allows, as TunnelConfiguration's are; drift
  is what the spec sets that Cloudflare holds otherwise, as JSON: objects field by field, keyed lists item by item
  (Cloudflare's other items not compared), sets in any order, other lists in order, nulls and empties as unset; deletion
  resets through the service's `Delete` unless retained or adopted, and only by the resource that configures the zone.
  Per kind kflare-gen writes the types, the controller (the narrow interface, whose reset may return a result or an
  error alone, the reconciler, RBAC, the `settingsKind` with its `settingsShape` and `unconfigured` values, and one
  conversion per struct, into the write's v7 params), an envtest table (a fake on `fakeSettingsStore`, the settings of
  an unconfigured zone, and the specs with the JSON each asks for) and a sample; `zz_generated_kinds.go` registers it
  and adds it to the Zone's dependents. `settings_specs_test.go` runs the shared specs on each table: the sample and
  each test written to an unconfigured zone, an idle reconcile, drift, each change (every scalar of each spec, enums
  included) written in place, adoption and the policies, one resource per zone and the hand-over, deletion and what
  keeps the settings, errors, the Zone waiting, the watches (in a manager), the full specs converted, and the CRD's
  refusals. No settings kind is committed yet, so a test-only kind, `SettingsFixture` (`settings_fixture_test.go`, with
  a CRD controller-gen wrote in `internal/controller/testdata/crd`), runs them in CI: TestURLNormalization by another
  name. Golden tests add TestURLNormalization (required enums, `Get`, `Update`), TestManagedTransform (keyed lists,
  `List`, `Edit`) and TestDNSSEC (optional bools, a `Delete` that returns a result); all three, generated into a copy of
  the tree for the check, passed the shared specs on envtest, 84 of 84. Mutations: 59 of the runtime and the comparison
  and 36 of kflare-gen's new code, all caught; the first runs left 12 and 8 alive, among them the ownership flaw above,
  the untested watches and a check that changed nothing, now gone. On the way: rule kinds' spec model and edits moved
  onto `paramKind` (`paramkind.go`, `paramedits.go`), which the two shapes share, and the conversions emitter is shared
  too; the committed and golden output did not change
- **Later:** the capabilities the next kinds need, in [Next wave](../CLAUDE.md#next-wave-proposed-2026-10-17):
  zone-scoped CRUD kinds, `SinglePage` lists, objects identified by name, services without `New`, secrets, and
  settings identified by an ID with a reset that writes defaults (zone settings). Also cross-kind references such as
  `kvNamespaceRef`, generated live e2e specs, and deploy.md
  permission rows from `x-api-token-group` (API names, while deploy.md uses the dashboard's labels; and the field is
  not reliable: for managed transforms it lists a permission that is refused, see Token permissions)
- **Stays hand-written:** `Zone`, `DNSRecord`, `Tunnel`, `TunnelConfiguration`, `WorkerScript` and `WorkerRoute`, which
  are mostly behavior verified live. Moving a hand-written kind needs a reason: R2Bucket would gain jurisdictions and
  storage class
- **Ruled out:** generating from v0.119 (no machine-readable model); OpenAPI alone with kflare's own wire types (it
  rebuilds the client v7 already generates from the same spec); Terraform provider schemas, Crossplane's upjet
  approach (it ties kflare to Terraform's resource model, and the spec already carries Stainless's Terraform hints)

**Risks:**
- The OpenAPI spec is not the live API (the list-name pattern above). Generated constraints are a starting point and
  live probes decide; an override in the entry records what was verified. When unsure, keep the looser rule: a
  too-strict CRD blocks valid configurations, while a loose one ends in a `TerminalError` with Cloudflare's message
- SDK major versions rename types. Entries and hooks name SDK types, so a major bump is its own regenerate-and-review
  PR
- Two SDKs share the binary for a while: two error families, classified alike since PR 1, and two retry setups, each
  kept at its SDK's defaults (see the v7 notes)
