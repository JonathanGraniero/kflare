# kflare architecture

This document explains how kflare's controllers behave and why: how they find and own Cloudflare resources, when they
notice drift, how they handle errors, and what happens on deletion. It describes the code as it is today, including
its known limitations. What may come next is in [ROADMAP.md](./ROADMAP.md).

## Resource model

```
Secret (API token)
  └─ CloudflareAccount (cluster-scoped)
       ├─ Zone (namespaced)
       │    ├─ DNSRecord (same namespace, via zoneRef; tunnelRef points it at a Tunnel)
       │    ├─ WorkerRoute (same namespace, via zoneRef; runs a WorkerScript)
       │    ├─ WAFCustomRule (same namespace, via zoneRef)
       │    ├─ RateLimitRule (same namespace, via zoneRef)
       │    ├─ ManagedRuleset (same namespace, via zoneRef)
       │    ├─ TransformRule (same namespace, via zoneRef)
       │    ├─ RedirectRule (same namespace, via zoneRef)
       │    ├─ CacheRule (same namespace, via zoneRef)
       │    ├─ ConfigurationRule (same namespace, via zoneRef)
       │    ├─ OriginRule (same namespace, via zoneRef)
       │    └─ ManagedTransform (same namespace, via zoneRef; one per Cloudflare zone)
       ├─ Tunnel (namespaced)
       │    ├─ TunnelConfiguration (same namespace, via tunnelRef)
       │    └─ DNSRecord (same namespace, via tunnelRef: a proxied CNAME to the tunnel)
       ├─ WorkerScript (namespaced; binds KVNamespaces via kvNamespaceRef, R2Buckets via r2BucketRef)
       ├─ KVNamespace (namespaced)
       ├─ R2Bucket (namespaced)
       ├─ D1Database (namespaced)
       └─ IPList (namespaced; rule expressions use it as $<name>)
```

Every namespaced resource resolves its credentials through this chain at reconcile time: a DNSRecord reads its Zone, the
Zone's CloudflareAccount, and the account's token Secret. Nothing caches a token between reconciles, so rotating the
Secret takes effect on the next reconcile.

Each controller declares a narrow interface over the Cloudflare SDK (for example `DNSRecordAPI`) holding only the calls it
makes. The production client (`pkg/cloudflare.Client`) embeds cloudflare-go v0 and satisfies all of them, except those of
the generated kinds, such as `KVNamespaceAPI`, and of the rule kinds (`RulesetRuleAPI`): each is part of a cloudflare-go
v7 service, built from the client's v7 options. Tests inject fakes.

## The reconcile loop

Every controller follows the same shape:

1. Handle deletion first if the resource is being deleted (see [Deletion](#deletion)).
2. Add the `kflare.dev/finalizer` finalizer and return; the update re-triggers the reconcile.
3. Resolve the parent resource and credentials. If the parent is missing or not ready, report `Ready=False` and wait
   for the parent's watch event.
4. Observe: fetch the Cloudflare resource by the ID stored in status (a rule: by its ref, see [Rules](#rules)). If there
   is no ID, or Cloudflare says it no longer exists, look for an existing resource with the same key: the resource's
   own, one to adopt if its adoption policy allows, or none, and then create one (see
   [Adoption and ownership](#adoption-and-ownership)).
5. Compare the spec with what Cloudflare reported, and write to Cloudflare only when they differ.
6. Record IDs and metadata in status and set `Ready=True`.

A DNSRecord with a `tunnelRef` has a second parent, its Tunnel. It waits until the Tunnel is ready with an ID
(`TunnelNotFound`, `TunnelNotReady`), and refuses a Tunnel that uses another CloudflareAccount than its Zone
(`AccountMismatch`): Cloudflare routes a tunnel's traffic only for DNS records in the tunnel's account. The record is
then a CNAME to `<tunnel ID>.cfargotunnel.com`, always proxied. Checked live on 2026-10-20: Cloudflare accepts an
unproxied CNAME to a tunnel, but it resolves to a private IPv6 address the internet cannot reach, so the CRD refuses
`proxied: false` with a `tunnelRef`. Cloudflare also accepts a CNAME to a tunnel ID that does not exist, so the check is
kflare's. And a proxied record's TTL is always automatic: Cloudflare stores 1 whatever it is sent, so the CRD refuses
another `ttl` on any proxied record, which would otherwise be written again on every reconcile.

The kinds kflare-gen generates run this loop through a shared reconciler: KVNamespace and D1Database through the shared
CRUD reconciler (`internal/controller/crudresource_controller.go`), TransformRule, RedirectRule, CacheRule,
ConfigurationRule and OriginRule through the shared rule controller (see [Rules](#rules)), and ManagedTransform through
the shared settings controller (see [Settings](#settings)). A generated kind only describes its Cloudflare calls, its
adoption key, what it compares and what depends on it. A CRUD kind's adoption finds the object in the account's list by
its key, then reads it by its ID: a list may show less than a read, as D1's does. kflare-gen writes that description
from the kind's entry in `generator/generator.yaml`, cloudflare-go v7 and Cloudflare's OpenAPI spec: its API types
(`api/v1alpha1/zz_generated.<kind>_types.go`), its controller (`internal/controller/zz_generated.<kind>.go`), and a
table of envtest specs that the shared specs of its shape run on (`zz_generated.<kind>_test.go`). The generated kinds
register together (`SetupGeneratedReconcilers`), and each counts as a user of its CloudflareAccount or its Zone.

Step 5 is deliberate: an idle reconcile makes read calls only. The live e2e suite checks this for DNSRecord,
WAFCustomRule, RateLimitRule, ManagedRuleset, TransformRule (each type), RedirectRule, CacheRule, ConfigurationRule,
OriginRule and IPList, and TunnelConfiguration's comparison ignores fields Cloudflare adds on its own so an unchanged
config does not bump its version. A zone's managed transforms carry no version to check this by, so ManagedTransform's
envtest specs check it instead.

## When drift is detected

kflare corrects drift whenever a resource is reconciled, but **a change made in the Cloudflare dashboard or API does not
by itself trigger a reconcile**. Reconciles are triggered by:

- a change to the resource itself, including a metadata-only change such as an annotation;
- a change to a watched dependency (for example a Zone becoming ready re-triggers its DNSRecords; a ConfigMap or Secret
  change re-triggers WorkerScripts that use it);
- a scheduled retry after an error (see below);
- the informer resync, which re-reconciles every resource. kflare does not set a sync period, so it uses
  controller-runtime's default of **10 hours**.

A successful reconcile does not schedule another one. In practice, a record edited in the dashboard can stay changed for
up to about 10 hours before kflare reverts it.

To force a reconcile, change the resource's metadata:

```sh
kubectl annotate dnsrecord my-record kflare.dev/reconcile-at="$(date +%s)" --overwrite
```

What each controller compares when it does reconcile:

| Resource | Drift detected on |
|---|---|
| `CloudflareAccount` | Token validity (re-validated against the account on every reconcile) |
| `Zone` | Zone type and `spec.plan`; recreated if deleted externally |
| `DNSRecord` | Content, TTL, proxied, priority (when set), comment, tags, structured data; recreated if deleted externally. With a `tunnelRef`, the content is the Tunnel's hostname, `<tunnel ID>.cfargotunnel.com`, and proxied is on: a Tunnel recreated with a new ID re-triggers the record, which updates its content in place |
| `Tunnel` | Existence (recreated if deleted externally); token Secret contents |
| `TunnelConfiguration` | The full ingress configuration |
| `WorkerScript` | Cloudflare's `modified_on` timestamp, because its etag does not change for binding-only edits; the IDs of bound KVNamespaces, so a recreated namespace re-uploads the Worker |
| `KVNamespace` | Title (renamed in place); recreated, empty, if deleted externally |
| `IPList` | Description, updated in place, and items, compared as a set in canonical form (Cloudflare stores 203.0.113.7/32 as 203.0.113.7, IPv6 in lowercase and compressed) and replaced as a whole. Cloudflare applies a replace in the background: the IPList reports `ReplacingItems` and checks every 2 seconds until it is done. Recreated if deleted externally |
| `D1Database` | Existence only: recreated, empty, if deleted externally. Its name, jurisdiction and location hint are fixed at creation |
| `R2Bucket` | Existence only: recreated, empty, if deleted externally. Its name cannot change and its location is fixed at creation. Workers bind a bucket by name, so they keep working with the recreated bucket without a re-upload (verified live) |
| `WorkerRoute` | Pattern and the Worker it runs; recreated if deleted externally, including when Cloudflare deletes it together with its Worker |
| `ManagedTransform` | Whether each managed transform its spec lists is on, by ID. The transforms it does not list are neither compared nor changed |
| `WAFCustomRule`, `RateLimitRule`, `ManagedRuleset`, `TransformRule`, `RedirectRule`, `CacheRule`, `ConfigurationRule`, `OriginRule` | The rule's whole definition: expression, action and its parameters, enabled, description, for a rate limiting rule how it counts (characteristics, period, requests per period, mitigation timeout, counting expression, origin requests only), for a managed ruleset deployment the ruleset and its overrides, and for a transform rule its URL rewrite or header changes. Also its place among kflare's rules in the zone and phase (see [Rules](#rules)). Recreated if deleted externally |

## Adoption and ownership

A resource creates its Cloudflare object. When an object with the resource's key already exists and kflare did not
create it, kflare leaves it untouched unless the resource asks to take it over, with the `kflare.dev/adoption-policy`
annotation:

| `kflare.dev/adoption-policy` | An existing object | No existing object |
|---|---|---|
| unset | left alone: `Ready=False`, reason `AlreadyExists`, checked again every minute | created |
| `adopt` | adopted | `Ready=False`, reason `NotFound`, checked again every minute; never created |
| `adopt-or-create` | adopted | created |

Any other value reports `InvalidAdoptionPolicy` and changes nothing. Adding or changing the annotation reconciles the
resource straight away.

```yaml
metadata:
  annotations:
    kflare.dev/adoption-policy: adopt
```

Adopting means kflare takes the object over: it overwrites it to match the spec and marks the resource with
`kflare.dev/adopted: "true"`. **An adopted object is kept when its resource is deleted**, unless the resource says
`kflare.dev/deletion-policy: delete` (see [Deletion](#deletion)), because deleting something kflare did not create is
the one step that cannot be undone. If an adopted object is
deleted outside kflare, `adopt-or-create` creates a new one, which is kflare's own from then on (the `adopted` mark
goes), and `adopt` reports `NotFound`.

A reconcile that created an object but failed before recording it must still know the object as its own, not report
`AlreadyExists`. So just before the create call, kflare records the object's key in the `kflare.dev/creating`
annotation, and an object found with that key is the resource's own. The annotation goes once the resource records the
object: with its claim label, in the same patch (see below), or, for kinds without one, on the next reconcile that
finds the object by the ID in status. An object a resource's claim label names is its own too.

| Resource | What counts as an existing object |
|---|---|
| `Zone` | A zone with `spec.name` in the referenced account. Zones in other accounts the token can reach are never adopted. |
| `DNSRecord` | Without a policy, a record with the same name, type and content (or data). A record of the same name and type with other content is left alone, and the DNSRecord creates its own next to it, as a member of a record set (several MX, TXT or A records). Where a name holds only one such record, Cloudflare refuses the second (for a CNAME: 400, code 81053, verified live), and the DNSRecord reports `TerminalError`: set a policy to take the existing record over. With a policy, a matching record is preferred; otherwise the only record with that name and type is adopted and corrected, and with several, `adopt` reports `NotFound` while `adopt-or-create` creates a new one. Records another DNSRecord manages are never adopted. |
| `Tunnel` | The first non-deleted tunnel named `spec.name` in the account. |
| `TunnelConfiguration` | The tunnel's configuration, when it routes anything (more than the catch-all rule) and this TunnelConfiguration has not written it. Adopting it replaces it whole. A new tunnel's empty configuration, or one kflare reset, counts as none. When several TunnelConfigurations point at one Tunnel, the oldest owns it and the others report `TunnelAlreadyConfigured`. |
| `WorkerScript` | A Worker with `spec.name` that this WorkerScript has not uploaded. Adopting it means the first upload replaces it. |
| `KVNamespace` | The namespace with the same title in the account (titles are unique per account), unless another KVNamespace manages it, in which case it reports `TitleConflict` and retries every minute. |
| `IPList` | The IP list named `spec.name` in the account (names are unique per account, across list kinds) unless another IPList manages it, or the name belongs to a list of another kind: either way it reports `NameConflict` and retries every minute. |
| `D1Database` | The database named `spec.name` in the account (names are unique per account) unless another D1Database manages it, in which case it reports `NameConflict` and retries every minute. An adopted database keeps its jurisdiction and location; `spec.jurisdiction` and `spec.primaryLocationHint` only apply when kflare creates the database. |
| `R2Bucket` | The bucket named `spec.name` in the account's default jurisdiction (names are unique per account) unless another R2Bucket for the same Cloudflare account manages it, in which case it reports `NameConflict` and retries every minute. An adopted bucket keeps its location; `spec.locationHint` only applies when kflare creates the bucket. |
| `WorkerRoute` | The route with the same pattern in the zone (patterns are unique per zone) unless another WorkerRoute manages it, in which case the WorkerRoute reports `PatternConflict` and retries every minute. |
| `ManagedTransform` | The zone's managed transforms, when any of them is on and this ManagedTransform did not turn it on: a zone with all of them off counts as none. Adopting them leaves the transforms the spec does not list as they are. When several ManagedTransforms point at one Cloudflare zone, across namespaces, the one that configures it keeps it, and otherwise the oldest takes it; the others report `AlreadyConfigured` and check again every minute. |
| `WAFCustomRule`, `RateLimitRule`, `ManagedRuleset`, `TransformRule`, `RedirectRule`, `CacheRule`, `ConfigurationRule`, `OriginRule` | Nothing, whatever the policy. A rule is kflare's only if it carries the ref kflare derives for the resource (see [Rules](#rules)); rules created outside kflare are never touched. A zone deploys a managed ruleset at most once, so while a rule kflare does not manage deploys it, the ManagedRuleset reports `RulesetConflict` and checks again every minute. |

A conflict with another resource of the kind comes first: it is reported whatever the policy.

The adoption and deletion policy annotations are modelled on those of AWS Controllers for Kubernetes.

### DNSRecord, WorkerRoute, KVNamespace, D1Database, IPList and R2Bucket ownership

DNSRecord stores the Cloudflare record ID it manages in the `kflare.dev/record-id` label, WorkerRoute its route ID in
`kflare.dev/route-id`, KVNamespace its namespace ID in `kflare.dev/kv-namespace-id`, D1Database its database ID in
`kflare.dev/d1-database-id` and IPList its list ID in `kflare.dev/ip-list-id`; each claims the object before doing
anything else with it (`internal/controller/ownership.go`).
Adoption skips any object another resource of the same kind already carries in its label, across all namespaces,
because two Zone resources in different namespaces can point at the same Cloudflare zone. Cloudflare record tags would be
a Cloudflare-side alternative, but they are only available on paid plans.

An R2 bucket has no ID, so R2Bucket claims the bucket name in `kflare.dev/r2-bucket-name`. Bucket names are only unique
within an account, so a claim blocks another R2Bucket only when both point at the same Cloudflare account, through the
same CloudflareAccount or two that share an account ID. An R2Bucket that never claimed its bucket, for example one
reporting `NameConflict`, leaves the bucket alone when it is deleted.

This prevents two DNSRecords from fighting over one record. Records created outside kflare are protected by the
adoption policy above: without one, a DNSRecord never takes over a record it did not create. Tools such as external-dns
keep a TXT ownership registry in the zone instead; kflare keeps its claims in the cluster.

### Rules

Each rule kind manages one rule per resource in its zone's entry point ruleset for a Rulesets API phase: `WAFCustomRule`
in `http_request_firewall_custom`, the zone's WAF custom rules, `RateLimitRule` in `http_ratelimit`, its rate limiting
rules, `ManagedRuleset` in `http_request_firewall_managed`, where an `execute` rule deploys a managed ruleset, and
`TransformRule` in the phase its immutable `spec.type` picks: `http_request_transform` for a URL rewrite,
`http_request_late_transform` for request headers, `http_response_headers_transform` for response headers,
`RedirectRule` in `http_request_dynamic_redirect`, the zone's single redirects, `CacheRule` in
`http_request_cache_settings`, `ConfigurationRule` in `http_config_settings`, and `OriginRule` in `http_request_origin`.
They share one controller (`internal/controller/rulesetrule.go`, `rulesetrule_controller.go`). kflare-gen generates
TransformRule, RedirectRule, CacheRule, ConfigurationRule and OriginRule from cloudflare-go v7's rule type for the
kind's action: its action parameters become the spec's fields, its unions a struct whose CEL rules say which
combinations Cloudflare takes. A zone has one entry point per phase, shared by every rule in that phase, kflare's or
not, so kflare:

- Marks each rule it manages with a ref, `kflare-` and the first 32 hex characters of the SHA-256 of
  `<kind>/<namespace>/<name>`, and finds its rule by that ref. The marker is on the Cloudflare side, and because it is
  derived from the resource's identity rather than its UID, a resource restored from a backup finds its rule again.
  A rule without such a ref is never adopted, changed or deleted.
- Writes one rule at a time, and creates the entry point only when the zone has none. It never replaces an entry point
  as a whole: that would replace the rules it does not manage. The calls are cloudflare-go v7's: v0.119 cannot create
  or edit a single rule, and fails on the answer to a rule delete.
- Compares the rule as Cloudflare returns it, as JSON, with the rule it would send: the fields Cloudflare sets (ID,
  version, last update) aside, and Cloudflare's defaults counted as set (a rule is enabled, a skip rule logs its
  matches, an execute rule runs the managed ruleset's latest version). A field set outside kflare is a difference too.
- Keeps its own rules in a zone and phase in order of `spec.priority`, lower first, with ties broken by namespace and
  name, across namespaces (two Zones in different namespaces can point at the same Cloudflare zone). Each phase is
  its own list: a WAF custom rule's priority does not order rate limiting rules. Rules created outside kflare keep
  their place. Reordering moves rules without changing them: Cloudflare keeps a moved rule's definition and version
  (verified live). A reconcile that moved rules checks the order again 10 seconds later, in case another resource was
  reordering the same rules at the same time.
- Leaves the entry point in place when its last rule is deleted, as Cloudflare does, except in the managed phase (see
  below).
- Writes a rule that uses a custom list (`$name`, managed by an `IPList`) once the list exists. Cloudflare refuses the
  rule until then (400, code 20127), a terminal error; every IPList change re-triggers the rules whose expression names
  the list, so a rule and its list can be applied together.

#### Managed rulesets

Cloudflare deploys its Free Managed Ruleset to Free zones by default, but only while the zone has **no entry point** in
`http_request_firewall_managed`. Any entry point there, even one holding only an unrelated exception, or none at all,
replaces that default with what its rules deploy: the ruleset stops running within seconds, and comes back within
seconds once the entry point is deleted (verified live on `kflare.dev`, with a request carrying a Log4j string). So:

- A ManagedRuleset deploys its ruleset explicitly, which keeps it running on a zone that had it by default. To turn it
  off, keep the ManagedRuleset and set `enabled: false`, or override the rules' `enabled` or `action`.
- When deleting a ManagedRuleset leaves the phase's entry point without rules, kflare deletes the entry point, which
  hands the zone back to Cloudflare's default. It does so even when the entry point was there, empty, before kflare,
  and had turned the default off: keeping every entry point kflare did not create would leave a zone without managed
  rules once its other rules were gone, and turning the default back on is the safer mistake. It keeps an emptied
  entry point while another kflare rule in the phase is about to be written to it, and every deletion re-triggers the
  zone's other ManagedRulesets, so one whose rule was written in the moment before the entry point went writes it
  again.
- A zone deploys a managed ruleset at most once (400, code 20014). A ManagedRuleset whose ruleset another rule already
  deploys reports `RulesetConflict` and checks again every minute, or as soon as another ManagedRuleset goes, so that a
  ManagedRuleset replaced by one with another name hands over within seconds.
- Cloudflare runs the ruleset's latest version and refuses any other (code 20055). It keeps overrides in the order
  sent, so kflare sorts categories and rules: reordering them in the spec writes nothing.

### Settings

A settings kind configures one product's settings in a zone, which always exist: `ManagedTransform`, the zone's
managed transforms. Its resource writes the settings its spec sets, and resets them when it is deleted; it never
creates or deletes anything. The settings kinds share one controller (`internal/controller/settings_controller.go`),
and kflare-gen generates them from cloudflare-go v7's service for the product: the write's body becomes the spec. So:

- One resource configures a zone's settings of a kind. Of the resources whose Zones point at the Cloudflare zone, in
  any namespace, the one whose status records the zone keeps it, and otherwise the oldest takes it (by creation time,
  then namespace and name). The others report `AlreadyConfigured`, and check again every minute and when one of them
  is deleted. A resource keeps the zone even from an older one whose Zone becomes ready later.
- Settings that hold other values than a zone nobody has configured were configured outside kflare, and a resource
  takes them over only as its adoption policy allows (see [Adoption and ownership](#adoption-and-ownership)). A zone
  nobody has configured has all of its managed transforms off.
- kflare compares what the spec sets with Cloudflare's copy, as JSON, and writes only when they differ. What the spec
  leaves out is not compared: a ManagedTransform lists transforms by ID, and those it does not list keep their state,
  whoever turned them on.
- Deleting the resource that configures the zone resets the settings with the product's reset: for managed transforms,
  all of them off, those the spec did not list included. A resource that adopted the settings, or whose deletion
  policy is `retain`, leaves them as they are.
- Cloudflare lists only the managed transforms a zone's plan has (six on a Free zone), and its documentation says it
  refuses to turn on one that conflicts with one that is on, such as `add_true_client_ip_headers` and
  `remove_visitor_ip_headers`. kflare reports such a refusal as `TerminalError`, with Cloudflare's message. A Free
  zone's six transforms have no conflicts, so this has not been seen live.

## Error handling

`pkg/cloudflare.IsTerminalError` sorts Cloudflare errors into two groups, and `handleCloudflareError` acts on them:

| Error | Classification | Behavior |
|---|---|---|
| 401, 403, other 4xx | Terminal | `Ready=False` with reason `TerminalError`; no retry. A spec or credential change re-triggers the reconcile. |
| 404 on a known resource ID | Handled first | The resource was deleted externally; kflare recreates it, or adopts another with its key, as the adoption policy allows. |
| 404 elsewhere | Terminal | As above. |
| 429, 5xx, network errors | Retryable | `Ready=False` with reason `APIError`; the error is returned so controller-runtime retries with exponential back-off (5 ms doubling up to about 16 minutes per resource). |

cloudflare-go names its 401 error `AuthorizationError` and its 403 error `AuthenticationError`, the reverse of what the
names suggest; `pkg/cloudflare/errors.go` documents the mapping. The generated SDK, cloudflare-go v7, returns one
error type, `*cloudflare.Error`, for every status, and `errors.go` classifies it by status in the same way. A condition
gives Cloudflare's messages and codes, such as `namespace not found (10013)`, for both SDKs (`cfpkg.ErrorMessage`); the
returned error, which controller-runtime logs, also names the request.

The Rulesets API reports what the zone's plan does not allow (a regular expression, the Log action, a custom block
response in a WAF custom rule, a rate limiting period other than 10 seconds, a paid managed ruleset, more rules than the
plan allows) as a 400, often without an error code. These are terminal like any other 4xx, and the condition carries
Cloudflare's message.

Failures caused by something a resource does not watch, such as a missing account or token Secret, retry every minute
with reason `AccountNotFound`, `AccountNotReady`, `SecretNotFound` or `TokenKeyMissing`. So does a managed ruleset that
another rule already deploys, with reason `RulesetConflict`, and a ManagedTransform whose zone another one configures,
with reason `AlreadyConfigured`. Terminal errors are also
retried by the 10-hour resync.

## Deletion

Each resource carries `kflare.dev/finalizer`. On deletion, kflare deletes the Cloudflare object the resource created,
keeps one it adopted (see [Adoption and ownership](#adoption-and-ownership)), and then removes the finalizer. The
`kflare.dev/deletion-policy` annotation overrides that: `retain` keeps the object either way, and `delete` deletes it
either way, an adopted one included.

```yaml
metadata:
  annotations:
    kflare.dev/deletion-policy: retain
```

Per-resource details:

- **Tunnel** removes the tunnel's connections first, which immediately disconnects any running `cloudflared`, then deletes
  the tunnel.
- **TunnelConfiguration** resets the tunnel to its catch-all rule rather than deleting anything. One that adopted the
  tunnel's configuration leaves it as it is, unless its deletion policy says `delete`.
- **WorkerScript** deletes the Worker only if kflare uploaded it, and keeps a Worker it adopted unless its deletion
  policy says `delete`.
- **WorkerScript** deletion also removes the Worker's routes: Cloudflare deletes them with the Worker. A WorkerRoute that
  still references it reports `WorkerScriptNotFound` and recreates its route once the Worker is uploaded again.
- **KVNamespace** deletion deletes the namespace **and all of its data**. Set the retain policy on namespaces holding data
  you need to keep.
- **D1Database** deletion deletes the database **and all of its data**, likewise.
- **WAFCustomRule**, **RateLimitRule**, **TransformRule**, **RedirectRule**, **CacheRule**, **ConfigurationRule** and
  **OriginRule** deletion deletes the resource's rule, found by its ref. The zone's other rules and its entry point
  stay.
- **ManagedRuleset** deletion deletes its `execute` rule, and with it the phase's entry point once no rule is left in
  it, which brings back Cloudflare's default deployment (see [Managed rulesets](#managed-rulesets)).
- **ManagedTransform** deletion turns all of the zone's managed transforms off, Cloudflare's reset, those its spec did
  not list included. One that adopted them leaves them as they are, unless its deletion policy says `delete`, and one
  that does not configure the zone (`AlreadyConfigured`) changes nothing.
- **IPList** deletion deletes the list. Cloudflare refuses while a rule uses it (400, code 10041): the IPList keeps its
  finalizer and reports `ListInUse`, checking again every minute, and straight away when a kflare rule that uses it is
  deleted. A rule created outside kflare holds it until that rule changes or the retain policy is set.
- **R2Bucket** deletion deletes only an empty bucket: kflare never deletes objects. Cloudflare refuses to delete a bucket
  that holds objects (409, code 10008), so the R2Bucket keeps its finalizer and reports `BucketNotEmpty`, checking again
  every minute until the bucket is emptied or the retain policy is set.

Deleting resources in any order, for example a namespace or `kubectl delete -f` on a directory, must not strand a
resource that still needs its parent for its own cleanup. Parents therefore keep their finalizer, reporting
`Ready=False` with reason `InUse` and the resources they wait for, until their dependents are gone
(`internal/controller/protection.go`):

- A `CloudflareAccount` waits for the Zones, Tunnels, WorkerScripts, R2Buckets and IPLists that reference it, and for
  the resources of every generated kind, KVNamespaces among them.
- A `Zone` waits for its DNSRecords, WorkerRoutes, WAFCustomRules, RateLimitRules, ManagedRulesets and the resources of
  every generated rule and settings kind, TransformRules, RedirectRules, CacheRules, ConfigurationRules, OriginRules
  and ManagedTransforms among them: a ManagedTransform needs its Zone to reset the zone's managed transforms.
- A `Tunnel` waits for its TunnelConfigurations, and for the DNSRecords that point at it through `tunnelRef`: deleting
  the tunnel first would leave their hostnames routing to nothing.
- A `KVNamespace` waits for the WorkerScripts bound to it through `kvNamespaceRef`. Cloudflare deletes a namespace a
  Worker is bound to, but every later upload of that Worker then fails. (Its entry in `generator/generator.yaml` lists
  the dependent kind and the hand-written function that finds the namespaces a WorkerScript binds.)
- An `R2Bucket` waits for the WorkerScripts bound to it through `r2BucketRef`, for the same reason: Cloudflare deletes a
  bucket a Worker is bound to, then refuses every upload of that Worker (code 10085, verified live).
- The account's token Secret carries `kflare.dev/token-protection` until no `CloudflareAccount` references it.

Each child therefore cleans up with its own deletion policy even when its parent is retained. The live e2e suite deletes
everything in one pass and checks Cloudflare afterwards.

A DNSRecord, WorkerRoute, rule, ManagedTransform or TunnelConfiguration whose parent is nevertheless gone (its
finalizer was removed by hand) cannot resolve credentials. It skips the Cloudflare call so its own deletion does not
hang, and leaves its Cloudflare object, or the zone's settings, as they are.

## Known limitations

- **Drift is not corrected promptly.** Out-of-band changes wait for the next trigger, at worst the 10-hour resync. A
  configurable periodic requeue on successful reconciles would bound this; it needs to stay within Cloudflare's API
  rate limits when many resources are managed.
- **Adoption goes by key only.** A resource adopts the object with its key (a name, a title, a pattern), and there is
  no way to name an object by its ID. Tunnel names are not unique: a Tunnel adopts the first one listed with its name.
- **No ownership marker on the Cloudflare side, except for rules.** kflare tracks what it owns only in Kubernetes, so if
  the cluster's resources are lost, it cannot tell its objects apart from anyone else's. Rules are the exception: their
  ref marks them (see [Rules](#rules)).
- **Rule order covers kflare's rules only.** A rule created outside kflare keeps its place, so it cannot be ordered
  relative to kflare's rules.
- **Pinned to the legacy SDK.** Most of kflare's controllers use cloudflare-go v0.119, the newest of the v0 line; the
  generated kinds and the rule kinds use the generated SDK, v7. Some limitations come from v0.119: Tunnel
  renames are impossible because `UpdateTunnel` omits the tunnel ID from the request path,
  `originRequest` timeouts are not exposed because `TunnelDuration` does not round-trip, and R2Bucket manages buckets
  in the default jurisdiction only: the SDK cannot send the `cf-r2-jurisdiction` header, and its Worker R2 binding has
  no jurisdiction field.
