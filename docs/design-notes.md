# Design notes

What was decided, built and verified live for each phase and kind, moved unchanged from CLAUDE.md on 2026-10-20 so
that it is read when needed rather than loaded into every session. Read a kind's section before changing it. The
plans still in progress, the conventions and the recipe for a new kind are in [CLAUDE.md](../CLAUDE.md);
kflare-gen's design is in [generator/CLAUDE.md](../generator/CLAUDE.md).

## Phased Implementation Plan

### Phase 1 — Foundation & Scaffolding
**Status:** ✅ Complete

**Goals:**
- [x] Initialize repo with kubebuilder scaffolding (`kubebuilder init --domain cloudflare.k8s.io`)
- [x] Set up API group with versioning strategy starting at `v1alpha1`
- [x] Implement `CloudflareAccount` cluster-scoped credential controller
  - Reads token from a referenced Secret
  - Calls `cf.Account()` to validate credentials on reconcile
  - Sets `Ready` condition and `accountName` in status
  - Tested live against real Cloudflare account ✅
  - Watches its token Secret; deletion protection keeps the account (finalizer, `Ready=False InUse`) while a
    Zone, Tunnel, WorkerScript, KVNamespace, R2Bucket, IPList or D1Database references it, and the token Secret
    (`kflare.dev/token-protection`, recorded in `status.protectedTokenSecret`) while an account references it — see
    `cloudflareaccount_protection.go`
- [x] Makefile targets: `generate`, `manifests`, `build`, `run`, `test` (kubebuilder-generated)
- [x] Local dev environment: `local/setup.sh`, `local/teardown.sh`, kind cluster config
- [x] `pkg/cloudflare/client.go` — shared CF client wrapper (completed in `feat/shared-client`)
- [x] `pkg/reconciler/base.go` — shared reconciler helpers (completed in `feat/shared-client`)

**Actual file layout (differs from original plan):**
- `api/v1alpha1/` (not `apis/`) — kubebuilder convention
- `internal/controller/` (not `pkg/reconciler/`) — kubebuilder convention
- API group is `kflare.dev` (originally `cloudflare.cloudflare.k8s.io` from group=`cloudflare` + domain=`cloudflare.k8s.io`)

---

### Phase 2 — Core Resource Controllers
**Status:** ✅ Complete

Each feature below is developed on its own branch and merged independently once tested.
Branch naming convention: `feat/<name>` (e.g. `feat/shared-client`, `feat/zone-controller`)

---

#### Branch: `feat/shared-client`
**Depends on:** main (Phase 1)
**Merges into:** main (before any controller branch starts)
**Status:** ✅ Complete

Shared infrastructure that all Phase 2 controllers will use.

- [x] `pkg/cloudflare/client.go` — thin wrapper around cloudflare-go SDK
  - `New(token string) (*Client, error)` constructor
  - `*Client` embeds `*cloudflare.API` — satisfies any narrow per-controller interface automatically
  - Production implementation; tests inject fakes via narrow interfaces
- [x] `pkg/cloudflare/errors.go` — error classification
  - `IsTerminalError(err)` — true for 401/403/404/4xx (stop requeuing)
  - `IsNotFound(err)` — true for 404 specifically
  - `IsRateLimit(err)` — true for 429 (apply back-off)
  - `HasErrorCode(err, code)` — a 4xx carrying a specific Cloudflare error code (added with R2Bucket)
- [x] `pkg/reconciler/base.go` — shared reconciler helpers
  - `SetCondition()` — wraps `meta.SetStatusCondition`, always sets `ObservedGeneration`
  - `EnsureFinalizer()` / `RemoveFinalizer()` — idempotent finalizer lifecycle
  - `Finalizer` constant — `"kflare.dev/finalizer"`
- [x] Unit tests — 100% coverage on both packages; controller updated to use shared helpers

**Design note:** Each controller declares its own narrow interface (e.g. `CloudflareAccountAPI`) for testability. `*cfpkg.Client` satisfies all such interfaces because it embeds `*cloudflare.API`.

**Test locally:** `make test` — no cluster needed

---

#### Branch: `feat/ci`
**Depends on:** main (can be done anytime)
**Merges into:** main
**Status:** ✅ Complete

- [x] `.github/workflows/ci.yml`
  - `go mod tidy` check, `go build ./...`
  - `make test` (runs manifests, generate, fmt, fix, vet, unit and envtest suites; `fix`, `go fix ./...`, since #38)
  - `git diff --exit-code` afterwards (ensures generated and formatted files are committed)
  - `GOTOOLCHAIN=local` with Go 1.26; a second job runs the envtest suites on the newest Kubernetes (1.37.0)
  - Tool versions (controller-gen, setup-envtest, golangci-lint v2, kustomize, kind) are pinned in the Makefile;
    setup-envtest follows the controller-runtime version
- [x] `.github/workflows/e2e.yml` (manual trigger only) runs `make test-e2e`:
  - Deployment smoke test: builds the image, deploys `config/default` to kind, checks the manager pod is
    ready with no restarts
  - Live Cloudflare specs (`test/e2e/cloudflare_test.go`, skipped without credentials): account validation, Tunnel +
    token Secret, TunnelConfiguration, KVNamespace (renamed in place), R2Bucket, a D1Database (created in the EU
    jurisdiction, deleted with the retain policy, then adopted by name), WorkerScript (bound to KV and R2), and an
    IPList (items replaced in place; an idle reconcile leaves the list alone). With `CF_E2E_ZONE` also Zone adoption
    (always `retain`), a DNSRecord (including a check that an idle reconcile does not write to Cloudflare), a
    WorkerRoute plus exclusion route, two WAFCustomRules (a skip rule placed before a block rule by priority; an idle
    reconcile leaves the ruleset version alone), a RateLimitRule (checked field by field, then updated in place; an idle
    reconcile leaves it alone), a ManagedRuleset deploying the Free Managed Ruleset (overrides checked, a rule override
    added in place; an idle reconcile leaves it alone), three TransformRules, one of each type (each phase's rule
    checked field by field, a response header changed in place; an idle reconcile leaves each phase alone), a
    RedirectRule (checked field by field, then switched in place from an expression target and 302 to a static URL and
    308; an idle reconcile leaves it alone), a CacheRule (its settings checked as the JSON Cloudflare stores, its TTLs
    changed in place; an idle reconcile leaves it alone), a ConfigurationRule (likewise, with settings changed and its
    WebMCP packs dropped in place), an OriginRule (likewise, moved to another destination port in place), and a
    WAFCustomRule that uses the IP list (refused while the list does not exist, written once it does). Then everything
    is deleted in one pass and Cloudflare is checked, including that the managed phase's emptied entry point is gone.
    Everything is named `kflare-e2e-<run>` (the list `kflare_e2e_<run>`) and cleaned up even on failure
  - `KFLARE_E2E_MANAGER=external` uses an already running manager (e.g. `make run`) instead of deploying one
    (first green live run: 2026-10-05, this way, on kind-kflare-dev)
  - Needs repo secrets `CF_API_TOKEN`, `CF_ACCOUNT_ID` and variable `CF_E2E_ZONE` (`kflare.dev`, the project's
    own zone, is the test zone)
- [x] CI also runs `make lint` and `make docker-build` (the Dockerfile copies source directories explicitly)
- [x] Security (`feat/security-scanning`, 2026-10-17): CodeQL through GitHub's default setup (Go and Actions, the
  `security-extended` suite; on pull requests, pushes and weekly), `dependency-review.yml` on pull requests,
  `.github/dependabot.yml` (Go modules, with the `k8s.io/*` and `sigs.k8s.io/*` modules in one group; Actions; the
  Dockerfile's images; a 7-day cooldown), read-only `permissions` on every workflow, third-party actions pinned to
  commit SHAs with the version in a comment (Dependabot updates both), and `SECURITY.md` (private
  vulnerability reporting). In the repo settings: Dependabot alerts and security updates, secret scanning and push
  protection. CodeQL's `go/weak-sensitive-data-hashing` on WorkerScript's `appliedHash` is a false positive: it is a
  change-detection hash, and a secret binding contributes the Secret's resourceVersion, never the value

**Test locally:** push branch and verify Actions run green

---

#### Branch: `feat/zone-controller`
**Depends on:** `feat/shared-client`
**Merges into:** main
**Status:** ✅ Complete

`Zone` is the root resource — every other resource references it via `zoneRef`.

- [x] `api/v1alpha1/zone_types.go`
  - `spec`: `name` (domain), `accountRef`, `plan` (free/pro/business), `type` (full/partial)
  - `status.conditions`, `status.cloudflareMetadata` (zone ID, name servers, status)
- [x] `internal/controller/zone_controller.go`
  - Create / read / update / delete lifecycle
  - Drift detection on zone type
  - Finalizer: `kflare.dev/finalizer`
  - Deletion policy annotation: `retain | delete`
  - Watch on CloudflareAccount → re-triggers zones when account heals
- [x] Unit tests (mocked CF client, 96.2% coverage)
- [x] `config/samples/cloudflare_v1alpha1_zone.yaml`
- [x] Bug fix: `pkg/cloudflare/errors.go` — `errors.As` targets must use pointer types

**Zone-specific design notes:**
- `ZoneAPI` interface: `CreateZone/ZoneDetails/ListZones/DeleteZone/EditZone` (all on `*cf.API`)
- `*cfpkg.Client` satisfies `ZoneAPI` because it embeds `*cf.API`
- List→adopt pre-existing zones before creating new ones; only a zone owned by the referenced account is adopted
  (the token may reach several accounts that hold the same domain)
- `spec.name` and `spec.accountRef` are immutable via CEL
- `spec.plan` (optional) is applied through the zone subscription API: POST (`ZoneSetPlan`) for a free zone,
  which has no subscription, PUT (`ZoneUpdatePlan`) for a paid one. A change already in `plan_pending` counts
  as applied. Setting it bills the account; never exercised live for that reason
- Get→NotFound path recreates externally-deleted zones
- cloudflare-go returns pointer error types (`*AuthenticationError` etc.) from its HTTP layer;
  `errors.As` targets must be pointer types too — see `pkg/cloudflare/errors.go` comments

**Test locally:**
```sh
kubectl apply -f config/samples/cloudflare_v1alpha1_zone.yaml
kubectl get zone example-zone -o yaml   # check Ready condition + cloudflareMetadata.zoneID
```

---

#### Branch: `feat/dns-record-controller`
**Depends on:** `feat/zone-controller`
**Merges into:** main
**Status:** ✅ Complete

- [x] `api/v1alpha1/dnsrecord_types.go`
  - `spec`: `zoneRef`, `name`, `type` (A/AAAA/CNAME/MX/TXT/SRV/CAA/NS/PTR/…), `content`, `ttl`, `proxied`, `priority`, `comment`, `tags`, `data`
  - `status.conditions`, `status.cloudflareMetadata` (record ID, zone ID, proxiable)
- [x] `internal/controller/dnsrecord_controller.go`
  - Create / update / delete lifecycle
  - Drift detection on all fields (content, TTL, proxied, priority, comment, tags, data)
  - Content drift skipped for SRV records (CF auto-formats it)
  - List→adopt pre-existing records before creating
  - Watches parent `Zone` — requeues records when zone becomes ready
  - Finalizer + deletion policy annotation
- [x] Unit tests (96.2% coverage, 121 specs total across controller suite)
- [x] `config/samples/cloudflare_v1alpha1_dnsrecord_a.yaml`
- [x] `config/samples/cloudflare_v1alpha1_dnsrecord_mx.yaml`
- [x] `config/samples/cloudflare_v1alpha1_dnsrecord_srv.yaml`

**DNSRecord-specific design notes:**
- `DNSRecordAPI` interface: `CreateDNSRecord/GetDNSRecord/ListDNSRecords/UpdateDNSRecord/DeleteDNSRecord`
- `rc` (ResourceContainer) is `cloudflare.ZoneIdentifier(zoneID)` — zone-scoped, not account-scoped
- `spec.data` uses `*apiextensionsv1.JSON` for structured SRV/LOC/CAA data; unmarshalled to `interface{}` for SDK
- `UpdateDNSRecordParams.Comment` is `*string` (unlike Create which uses `string`)
- Resolves chain: DNSRecord → Zone (namespaced) → CloudflareAccount (cluster-scoped) → Secret
- Cloudflare always reports `ttl` and `proxied`, so unset spec values are compared against its defaults
  (ttl 1 = automatic, proxied false) and sent explicitly when they drift — the update is a PATCH that omits
  zero values. An unset `priority` is left to Cloudflare
- Content is not compared when `spec.data` is set (or for SRV): Cloudflare derives it from data
- `spec.name`, `spec.type` and `spec.zoneRef` are immutable via CEL; `ttl` must be 1 or 30–86400
- Deletion skips the Cloudflare call when the Zone resource is already gone (e.g. namespace deletion)
- Ownership: the `kflare.dev/record-id` label holds the Cloudflare record a DNSRecord manages. Adoption skips
  records another DNSRecord carries in that label, prefers a record whose content/data matches, and only adopts
  a non-matching record when it is the only one with that name and type. Record tags would be the
  Cloudflare-side alternative, but they are paid-plan only

**Test locally:**
```sh
kubectl apply -f config/samples/cloudflare_v1alpha1_dnsrecord_a.yaml
kubectl get dnsrecord -o yaml   # check Ready + cloudflareMetadata.recordID
```

---

#### Branch: `feat/tunnel-controller`
**Depends on:** `feat/shared-client`
**Merges into:** main (independent of zone controller)
**Status:** ✅ Complete

- [x] `api/v1alpha1/tunnel_types.go`
  - `spec`: `name` (immutable), `accountRef` (immutable), `credentialsSecretRef.name` (Secret in the Tunnel's namespace)
  - `status.conditions`, `status.cloudflareMetadata` (tunnel ID, health), `status.credentialsSecretName`
- [x] `internal/controller/tunnel_controller.go`
  - Get by status ID → List→adopt by name → Create (remotely managed, `config_src=cloudflare`)
  - Recreates tunnels deleted outside kflare (404, or `deleted_at` set)
  - Writes the tunnel token to an owned Secret under `TUNNEL_TOKEN`; refuses to touch a Secret it does not own
  - Deletes the old Secret when `credentialsSecretRef` changes
  - Finalizer: removes connections (disconnecting any running cloudflared) and deletes the tunnel (unless `retain`)
  - Watches owned Secrets and CloudflareAccounts
- [x] `internal/controller/credentials.go` — shared account → Secret → token resolution
- [x] Unit tests (95.8% package coverage)
- [x] `config/samples/cloudflare_v1alpha1_tunnel.yaml`

**Tunnel-specific design notes:**
- `TunnelAPI` interface: `CreateTunnel/GetTunnel/ListTunnels/GetTunnelToken/CleanupTunnelConnections/DeleteTunnel`
- `rc` is `cloudflare.AccountIdentifier(accountID)` — account-scoped
- cloudflare-go v0's `UpdateTunnel` (still in v0.119) omits the tunnel ID from the request path, so renames are
  impossible; `spec.name` and `spec.accountRef` are immutable via CEL (`self == oldSelf`)
- `CreateTunnel` requires a secret client-side; the controller sends 32 random bytes (base64) and never stores them
- Deletion calls `CleanupTunnelConnections` first, which drops even active connectors, then `DeleteTunnel`;
  a running cloudflared is disconnected immediately (verified live)
- `GetTunnel` on a deleted tunnel returns success with `deleted_at` set, not 404 (verified live)
- Credential and Secret-conflict failures requeue after 1 minute (the API token Secret is not watched); Zone
  and DNSRecord use `resolveAccountToken` and the same retry
- Needs an API token with the account-level **Cloudflare Tunnel: Edit** permission

**Test locally:**
```sh
kubectl apply -f config/samples/cloudflare_v1alpha1_tunnel.yaml
kubectl get tunnel example-tunnel -o yaml        # check Ready + cloudflareMetadata.tunnelID
kubectl get secret example-tunnel-token -o yaml  # TUNNEL_TOKEN for cloudflared
```

---

#### Branch: `feat/tunnel-configuration-controller`
**Depends on:** `feat/tunnel-controller`
**Merges into:** main
**Status:** ✅ Complete

- [x] `api/v1alpha1/tunnelconfiguration_types.go`
  - `spec`: `tunnelRef` (immutable, same namespace), `ingress[]` (hostname, path, service, originRequest subset), `defaultService` (default `http_status:404`)
  - `status.conditions`, `status.cloudflareMetadata` (tunnel ID last written, config version)
- [x] `internal/controller/tunnelconfiguration_controller.go`
  - Get config → compare → Put only on drift; kflare owns the whole configuration
  - Appends `defaultService` as the catch-all rule Cloudflare requires
  - Oldest TunnelConfiguration per Tunnel owns it; others report `TunnelAlreadyConfigured` and retry every minute
  - Finalizer: resets the tunnel to the catch-all rule (unless `retain`); skipped when the Tunnel resource is gone
  - Watches Tunnels → requeues configurations when a Tunnel becomes ready or is recreated
- [x] Unit tests (95.9% package coverage)
- [x] `config/samples/cloudflare_v1alpha1_tunnelconfiguration.yaml`

**TunnelConfiguration-specific design notes:**
- `TunnelConfigurationAPI` interface: `GetTunnelConfiguration/UpdateTunnelConfiguration` (account-scoped `rc`)
- Cloudflare rejects a config whose last rule has a hostname or path, and rejects an empty rule list (verified live)
- Cloudflare returns the config exactly as written plus `warp-routing: {enabled: false}` and an empty top-level
  `originRequest`; both count as "unchanged" (verified live — an idle reconcile does not bump the version)
- `originRequest` timeouts are not exposed: cloudflare-go v0's `TunnelDuration` (still in v0.119) marshals float
  seconds but only unmarshals integers
- Each hostname still needs a proxied CNAME to `<tunnelID>.cfargotunnel.com`: a `DNSRecord` with `tunnelRef` (since
  Launch readiness L3)
- Drift is only corrected when something triggers a reconcile (spec change, Tunnel change, resync period)

**Test locally:**
```sh
kubectl apply -f config/samples/cloudflare_v1alpha1_tunnelconfiguration.yaml
kubectl get tunnelconfiguration -o yaml   # check Ready + cloudflareMetadata.version
```

---

#### Branch: `feat/worker-script-controller`
**Depends on:** `feat/shared-client`
**Merges into:** main (independent of zone/tunnel)
**Status:** ✅ Complete

- [x] `api/v1alpha1/workerscript_types.go`
  - `spec`: `name` (immutable), `accountRef` (immutable), exactly one of `script` / `scriptConfigMapRef {name, key}`,
    `format` (`module` default, or `serviceWorker`), `compatibilityDate`, `compatibilityFlags`,
    `bindings[]` (exactly one of `plainText`, `secretKeyRef {name, key}`, `kvNamespaceRef`, `kvNamespaceID`,
    `r2BucketRef`, `r2BucketName`; the refs were added with KVNamespace and R2Bucket)
  - `status.conditions`, `status.cloudflareMetadata` (etag, modifiedOn at full precision), `status.appliedHash`
- [x] `internal/controller/workerscript_controller.go`
  - Uploads when the desired-state hash changes, when the Worker is missing, or when Cloudflare's `modified_on`
    differs from kflare's last upload
  - Watches ConfigMaps (script source), Secrets (secret bindings), KVNamespaces (`kvNamespaceRef`), R2Buckets
    (`r2BucketRef`) and CloudflareAccounts
  - Finalizer: deletes the Worker (unless `retain`); a Worker kflare never uploaded is left alone
- [x] Unit tests (96.4% package coverage)
- [x] `config/samples/cloudflare_v1alpha1_workerscript.yaml`

**WorkerScript-specific design notes:**
- `WorkerScriptAPI` interface: `UploadWorker/ListWorkers/DeleteWorker` (account-level `rc` required)
- Cloudflare's etag hashes the code only: it stays the same for an identical re-upload *and* for a binding-only
  change, so it cannot detect drift (verified live). `modified_on` changes on every upload and matches between the
  upload response and `ListWorkers`, so it is the drift signal
- `modified_on` has microsecond precision; it is stored as an RFC 3339 string because `metav1.Time` truncates
  to seconds and would never match
- The hash covers script, format, compatibility settings and bindings; secret bindings contribute the Secret's
  UID/resourceVersion, never the value
- Any change made outside kflare triggers one re-upload, including toggling the workers.dev subdomain (verified
  live); the re-upload keeps the subdomain enabled
- Routes are not part of WorkerScript: they are zone-level resources with their own IDs, managed by `WorkerRoute`

**Test locally:**
```sh
kubectl create secret generic example-worker-secrets --from-literal=api-key=...
kubectl apply -f config/samples/cloudflare_v1alpha1_workerscript.yaml
kubectl get workerscript example-worker -o yaml   # check Ready + cloudflareMetadata.modifiedOn
```

---

#### Branch: `feat/worker-route-controller`
**Depends on:** `feat/zone-controller`, `feat/worker-script-controller`
**Merges into:** main
**Status:** ✅ Complete; generated since `feat/generate-worker-route` (below)

- [x] `api/v1alpha1/workerroute_types.go`
  - `spec`: `zoneRef` (immutable, same namespace), `pattern`, optional `workerScriptRef` (same namespace);
    without a WorkerScript the route excludes its pattern from a broader route
  - `status.conditions`, `status.cloudflareMetadata` (route ID, zone ID, script routed to)
- [x] `internal/controller/workerroute_controller.go`
  - Get by status ID → adopt the route with the same pattern → create; pattern and Worker drift corrected with an
    in-place PUT
  - Waits for the Zone and an uploaded WorkerScript (Cloudflare refuses a route to a missing Worker); reports
    `PatternOutsideZone` and `AccountMismatch` without calling Cloudflare
  - Claims its route in the `kflare.dev/route-id` label; `PatternConflict` instead of taking over another
    WorkerRoute's route (`ownership.go`, shared with DNSRecord)
  - Watches Zones and WorkerScripts; finalizer deletes the route (unless `retain`)
- [x] Zone and Tunnel deletion protection (`protection.go`): a Zone waits for its DNSRecords and WorkerRoutes, a
  Tunnel for its TunnelConfigurations, as accounts already did. Found live: with a retained Zone, deleting
  everything at once left routes behind
- [x] Unit/envtest tests; live e2e spec (route + exclusion) and an all-at-once deletion spec
- [x] `config/samples/cloudflare_v1alpha1_workerroute.yaml`

**WorkerRoute-specific design notes (all verified live on `kflare.dev`):**
- `WorkerRouteAPI` interface: `CreateWorkerRoute/GetWorkerRoute/ListWorkerRoutes/UpdateWorkerRoute/DeleteWorkerRoute`;
  `rc` is `cloudflare.ZoneIdentifier(zoneID)`
- Patterns are unique per zone (duplicate → 409, code 10020); a route outside the zone → 400 (10022); a route to a
  missing Worker → 400 (10019); an unknown route ID → 404
- PUT replaces the route: omitting `script` (the SDK's `omitempty` for "") clears it, so exclusion works through
  `UpdateWorkerRoute`
- **Deleting a Worker deletes its routes.** The WorkerRoute sees a 404 by ID and recreates the route once the
  WorkerScript is uploaded again (the WorkerScript watch triggers it)
- Needs the zone-level **Workers Routes: Edit** permission (added to the dev token on 2026-10-05)

**Generated** (`feat/generate-worker-route`, 2026-10-29; [Generate every kind](../CLAUDE.md#generate-every-kind), PRs 9
and 10 in one, as the user asked for the next pair). Its entry in `generator/generator.yaml` replaces the hand-written
types and controller, on cloudflare-go v7's `workers.RouteService`. It needed kflare-gen's CRUD kinds of a zone,
references to resources of another kind (`workerScriptRef`), lists that are never paged (v7's `SinglePage`) and calls
that each return a type of their own, converted to Get's ([generator/CLAUDE.md](../generator/CLAUDE.md)). The CRD is the
hand-written one but for descriptions and the pattern's `minLength: 1`, which its pattern already implied (a mutation
check found it untestable), so v0.3 manifests apply unchanged. Probed on `kflare.dev` that day with a
`kflare-probe-route` Worker, deleted afterwards with its routes:

- Duplicate pattern 409 (10020), a pattern outside the zone, with a port or empty 400 (10022, "Route pattern must
  include zone name: kflare.dev"), a missing Worker 400 (10019), all as on 2026-10-05. An unknown route ID is a 404 for
  GET, PUT and DELETE (now 10007 or 10009), which the shared CRUD reconciler reads as gone
- PUT replaces the route: without `script` it clears it (`"script": null` back), and `"script": ""` is refused
  (10019), so an unset `workerScriptRef` sends no script at all. A new pattern moves the route in place, same ID
- The list has no `result_info` and ignores `page` and `per_page`: one page holds every route of the zone, which v7
  types as a `SinglePage`. Routes also carry `request_limit_fail_open`, which v7.13.0 does not know
- A pattern's host is stored as given (`KFLARE-PROBE-ROUTE.kflare.dev` kept its capitals), so nothing drifts. A
  scheme is stored too (`https://...`); the entry's validation keeps refusing one, as the hand-written CRD did
- What changed: kflare no longer checks the pattern's host against the Zone before calling Cloudflare, so a pattern
  outside the zone is Cloudflare's `TerminalError` with the 10022 message rather than `PatternOutsideZone`. A claim on a
  route counts from a WorkerRoute whose Zone is for the same Cloudflare zone, through any Zone, in any namespace

**Test locally:**
```sh
kubectl apply -f config/samples/cloudflare_v1alpha1_workerroute.yaml
kubectl get workerroutes.kflare.dev   # check Ready + Route ID
```

---

### Phase 3 — Expanded API Surface
**Status:** 🚧 In progress

Same branch-per-feature pattern as Phase 2. Items that can be built **and live-tested on a Free zone** come first;
load balancing waits until the account has the Load Balancing add-on (decided 2026-10-06). Anything skipped or
only partly built because of a paid plan, add-on or account setup is tracked in
[Deferred: paid plan, add-on or account setup](#deferred-paid-plan-add-on-or-account-setup) below.

Most of what remains is a rule in one of a zone's **Rulesets API** phases: WAF custom rules, rate limiting rules,
managed ruleset deployments, and the Rules products that replace Page Rules. `feat/rulesets-foundation` builds the
shared machinery once; each rule CRD on top of it is mostly types plus a conversion from spec to rule. Decided
2026-10-09: **one Kubernetes resource per rule**, not one per zone and phase, so that each app can own its own rules.

| Branch | CRD(s) | Depends on | Status / Free-plan scope |
|--------|--------|------------|--------------------------|
| `feat/kv-namespace-controller` | `KVNamespace` (+ WorkerScript `kvNamespaceRef`) | shared-client | ✅ |
| `feat/r2-bucket-controller` | `R2Bucket` (+ WorkerScript `r2BucketRef`) | shared-client | ✅ Generated since `feat/generate-r2-bucket`, with jurisdictions and storage classes |
| `feat/cloudflare-go-v0.119` | none: SDK bump (see [SDK decision](#sdk-decision)) | — | ✅ |
| `feat/rulesets-foundation` | none: Rulesets client calls and the shared rule sync | zone-controller | ✅ |
| `feat/waf-custom-rule-controller` | `WAFCustomRule` (was `FirewallRule`) | rulesets-foundation | ✅ Generated since `feat/generate-waf-custom-rule`. Free: 5 rules, no regex, no Log action, no custom block response |
| `feat/rate-limit-rule-controller` | `RateLimitRule` (was `RateLimit`) | rulesets-foundation | ✅ Generated since `feat/generate-rate-limit-rule`. Free: 1 rule, 10 s period and timeout, counts by IP, block only |
| `feat/ip-list-controller` | `IPList` | shared-client | ✅ Free: 1 IP list, 10,000 items |
| `feat/zero-trust-controllers` | `AccessGroup`, `AccessPolicy`, `AccessApplication` | shared-client | Blocked: Zero Trust is not enabled on the account (a free plan exists) |
| `feat/managed-ruleset-controller` | `ManagedRuleset` (was `WAFPackage`) | rulesets-foundation | ✅ By hand (decided 2026-10-13), generated since `feat/generate-managed-ruleset`. Free: only the Cloudflare Free Managed Ruleset, with overrides |
| Code generator: `feat/cloudflare-go-v7` … `feat/generator-rule-kinds` (8 PRs) | none: `kflare-gen` and the shared CRUD reconciler; KVNamespace moves onto them; the first new generated kind and the first generated rule kind (see [Code generator](../generator/CLAUDE.md#code-generator-kflare-gen)) | — | ✅ 8 of 8 (`feat/cloudflare-go-v7` … `feat/generator-rule-kinds`); decided 2026-10-13 |
| `feat/d1-database-controller` | `D1Database` | code generator (PR 6) | ✅ Generated end to end. Free: 10 databases, 500 MB each, 5 GB in total |
| `feat/managed-transform-controller` | `ManagedTransform` | code generator (settings kinds) | ✅ Generated, the first settings kind. Free: all six transforms, none conflicting |
| `feat/generator-rule-kinds` | `TransformRule` | code generator (rule kinds) | ✅ Generated, the first rule kind (the code generator's PR 8). Free: 10 rules across its three phases, no regex |
| `feat/redirect-rule-controller` | `RedirectRule` | code generator (rule kinds) | ✅ Generated. Free: 10 rules ✔, no regex ✔ |
| `feat/cache-rule-controller` | `CacheRule` | code generator (rule kinds) | ✅ Generated. Free: 10 rules ✔; of a custom cache key, only leaving out the query string ✔ |
| `feat/configuration-rule-controller` | `ConfigurationRule` | code generator (rule kinds) | ✅ Generated. Free: 10 rules ✔; some values need a higher plan ✔ |
| `feat/origin-rule-controller` | `OriginRule` | code generator (rule kinds) | ✅ Generated. Free: 10 rules (per the docs), destination port override only ✔ |
| `feat/managed-ruleset-exception-controller` | `ManagedRulesetException` | managed-ruleset-controller | Not scheduled (decided 2026-10-14, see `ManagedRuleset`). Free: skip rules accepted |
| `feat/load-balancer-monitor-controller` | `LoadBalancerMonitor` | shared-client | ⏸ Deferred: needs Load Balancing add-on |
| `feat/load-balancer-controller` | `LoadBalancer` (+ pools) | load-balancer-monitor-controller | ⏸ Deferred: needs Load Balancing add-on |

Rows run top to bottom. The order decided on 2026-10-06 (WAF, then rate limiting, then Zero Trust) is kept. `IPList`
comes before Zero Trust because it is free and can be tested today, while Zero Trust waits on a dashboard step. When a
row is blocked, skip ahead: `ManagedTransform` depends on no other row. The five Rules rows are generated (decided
2026-10-13) and done: TransformRule, RedirectRule, CacheRule, ConfigurationRule and OriginRule. What may come after
them is in [Next wave](../CLAUDE.md#next-wave-proposed-2026-10-17).

`PageRule` is dropped (decided 2026-10-09): Page Rules is deprecated, and its API refuses account-owned tokens (see
"Plan corrections" below). The Transform, Redirect, Cache, Configuration and Origin rows cover its settings.

**Notes:**
- `LoadBalancerMonitor` (renamed from `HealthCheck`, 2026-10-06) is Cloudflare's account-level load balancer monitor,
  matching the API and Terraform (`cloudflare_load_balancer_monitor`). Without the Load Balancing add-on
  (~$5/month) Cloudflare rejects every monitor: the interval range collapses to [1, 1], which cannot satisfy
  interval > (retries+1) × timeout (verified live). The name `HealthCheck` stays free for Cloudflare's separate
  zone-level Health Checks product, which needs a Pro plan
- Zero Trust resources are account-level (`/accounts/{id}/access/...`) and take an `accountRef`. One account-owned
  token can hold them alongside zone permissions; the dev token already has the Access write permissions
- R2, KV and IP lists are account-level — no `zoneRef`

#### Token permissions for Phase 3

Checked 2026-10-09 against the dev token. The Rulesets phases were checked with `dry_run=true`, which runs the same
authorization, plan and quota checks as a real write but stores nothing. The token's zone policy had only Read for these
products (it gained Write on `kflare.dev` for the redirect, cache, configuration and origin phases on 2026-10-15, see
below), yet the WAF and Transform phases were accepted: its account-level **Account WAF Write**, **Account Rulesets
Write** and **Transform Rules Write** evidently cover zone rulesets too. The names below are the API's permission group
names; deploy.md uses the dashboard's labels, so look those up when each branch adds its row there.

| Resource | Permission to document (least privilege) | Dev token today |
|---|---|---|
| `WAFCustomRule`, `RateLimitRule`, `ManagedRuleset` | Zone WAF Write | ✅ dry run accepted; enough on its own (2026-10-13 and 2026-10-14, see below) |
| `TransformRule` | Zone Transform Rules Write | ✅ written, updated and deleted by the e2e (2026-10-15, runs `7ed03a` and `9c869b`); enough on its own (2026-10-15, see below) |
| `RedirectRule` | Dynamic URL Redirects Write | ✅ added on `kflare.dev` 2026-10-15 (see below); written, updated and deleted by the e2e (run `5f9daf`); enough on its own (2026-10-17, see below) |
| `CacheRule` | Cache Settings Write | ✅ added on `kflare.dev` 2026-10-15; written, updated and deleted by the e2e (run `fd8de2`); enough on its own (2026-10-17, see below) |
| `ConfigurationRule` | Config Settings Write | ✅ added on `kflare.dev` 2026-10-15; written, updated and deleted by the e2e (run `e9994a`); enough on its own (2026-10-19, see below) |
| `OriginRule` | Origin Write | ✅ added on `kflare.dev` 2026-10-15; dry runs and ten probe rules written and deleted (2026-10-18); written, updated and deleted by the e2e (run `2748f8`); enough on its own (2026-10-19, see below) |
| `ManagedTransform` | Managed headers Write | ✅ added on `kflare.dev` to the dev and CI tokens on 2026-10-23, with the user's OK (not a Rulesets endpoint, so no dry run; an empty PATCH was refused on 2026-10-17 with Config Settings Write, which the OpenAPI spec lists for it); written, changed and reset by the e2e (run `3f5c86`, 23 of 27 with the dev token). Checked on its own before v0.2.0: the CI token, holding exactly deploy.md's permissions with Zone Read (read back 2026-10-26: 7 account, 10 zone; not Account Rulesets Read, which Cloudflare's documentation also lists for the PATCH), passed the whole live suite in GitHub Actions (run 40254222769, e2e run `5a099c`, 27 of 27) |
| `IPList` | Account Rule Lists Write (account) | ✅ create, replace and delete verified 2026-10-13; enough on its own |
| `D1Database` | D1 Write (account) | ✅ create, read, list and delete verified 2026-10-15; enough on its own (2026-10-15, see below) |
| `AccessGroup`, `AccessPolicy`, `AccessApplication` | Access: Organizations, Identity Providers, and Groups Write; Access: Apps and Policies Write (account) | ✅, but Access is not enabled |

- Adding a permission to the dev token needs the user's OK first, as with Workers Routes Write on 2026-10-05
- **deploy.md's permissions are enough on their own** (checked 2026-10-13, with the user's OK). An account-owned token
  holding only Account Settings Read, Cloudflare Tunnel Write, Workers Scripts Write, Workers KV Storage Write,
  Workers R2 Storage Write and Account Rule Lists Write on the account, and Zone Read, DNS Write, Workers Routes Write
  and Zone WAF Write on `kflare.dev`, ran the whole live e2e suite (13 of 13, run `714bb3`), with no refusal in the
  manager's log. It was refused for transform rules, account rulesets and API tokens. So Zone WAF Write alone covers
  `WAFCustomRule` and `RateLimitRule`, and Zone Read is enough to adopt a zone. The token expired within 3 hours and
  was deleted after the run
- Repeated on 2026-10-14 for `ManagedRuleset`, with the user's OK: a token with the same permissions ran the suite, 14
  of 14 (run `fef249`), with no refusal, and kflare deleted the managed phase's emptied entry point with it. So Zone
  WAF Write covers `ManagedRuleset` too. That token was deleted after the run as well
- Repeated on 2026-10-15 for `D1Database`, with the user's OK: a token holding deploy.md's permissions, D1 Write among
  them, and Zone Read ran the suite against main's manager, 15 of 15 (run `5a755b`), with no refusal in the manager's
  log. It was refused for API tokens, transform rules and account rulesets. So D1 Write covers `D1Database`. That
  token was deleted after the run as well
- Repeated on 2026-10-15 for `TransformRule`, with the user's OK: deploy.md's permissions, Zone Transform Rules Write
  among them, and Zone Read ran the suite against main's manager, 16 of 16 (run `1aaa68`), with no refusal in the
  manager's log. It was refused for API tokens, account rulesets and the redirect phase. So Zone Transform Rules Write
  covers `TransformRule`. That token was deleted after the run as well
- Repeated on 2026-10-17 for `RedirectRule` and `CacheRule`, with the user's OK: deploy.md's permissions, Dynamic URL
  Redirects Write and Cache Settings Write among them, and Zone Read ran the suite against main's manager, 18 of 18
  (run `2660d4`), with no refusal in the manager's log. It was refused for API tokens, account rulesets and the
  configuration phase. So Dynamic URL Redirects Write covers `RedirectRule`, and Cache Settings Write `CacheRule`. That
  token was deleted after the run as well
- Repeated on 2026-10-19 for `ConfigurationRule` and `OriginRule` (Launch readiness L1), with the user's OK: deploy.md's
  permissions, Config Settings Write and Origin Write among them, and Zone Read ran the suite against main's manager
  (run `e79693`). Every resource spec passed, 19 of 19, with no refusal in the manager's log, and the token deleted
  every rule, list, database, bucket, namespace, Worker, route, record and tunnel the run made. The last spec, the
  all-at-once deletion, timed out on a kflare bug the run found, not on a permission: the Zone controller did not watch
  the generated rule kinds, so a Zone whose last dependent was a TransformRule was never re-triggered (fixed in
  `fix/zone-watches-generated-rule-kinds`; run `f1f17f`, with the dev token, 20 of 20). So Config Settings Write covers
  `ConfigurationRule`, and Origin Write `OriginRule`. That token was deleted after the run as well
- On 2026-10-15, with the user's OK for the four remaining Rules kinds, the dev token gained Dynamic URL Redirects
  Write, Cache Settings Write, Config Settings Write and Origin Write, in a new policy scoped to `kflare.dev` alone (its
  other policies are unchanged). A dry run in each of the four phases was accepted
- The dev token's account-level permissions cover more than deploy.md lists, so it cannot show that a permission is
  enough. Check a new one with a short-lived token holding only deploy.md's permissions, as above (ask first)

#### SDK decision
**Status:** Accepted 2026-10-09. Step 1 is done: kflare uses cloudflare-go **v0.119.0**. Step 3 is brought forward
for generated kinds (2026-10-13): **v7** sits next to v0.119 since `feat/cloudflare-go-v7` (v7.12.0, v7.13.0 since
2026-10-23), and the rule kinds use it since `feat/rulesets-v7`

kflare pinned cloudflare-go v0.89.0 until October 2026, and its gaps kept costing features. Checked 2026-10-09:

| | v0.89.0 (pinned until 2026-10) | v0.119.0 (newest v0, 2026-09-25; now used) | v7.12.0 (generated SDK, 2026-10-01) |
|---|---|---|---|
| Create / edit / delete one rule in a ruleset | none | delete only, and it fails on the live API (see the foundation) | all three |
| Reusable Access policies (`/access/policies`) | no: app-scoped policies only | yes | yes |
| R2 jurisdictions (`cf-r2-jurisdiction`) | no | no | yes |
| Tunnel rename (`UpdateTunnel` sends the tunnel ID) | no | no | yes |
| Cost to adopt | — | one compile error: `DNSRecord.ZoneID` was removed (take the zone ID from the Zone), plus test fixtures | every controller's interface and fakes, and `errors.go` (different error types) |

Decided:
1. ✅ **Bump to v0.119**, in its own PR (`feat/cloudflare-go-v0.119`). It brings reusable Access policies. Its
   `DeleteRulesetRule` turned out to fail on the live API, so kflare makes that call itself too. See the bump notes
   below
2. **Fill the remaining gaps with `API.Raw`** wrappers in `pkg/cloudflare`. ✅ For rulesets (`feat/rulesets-foundation`):
   rule create, edit, move and delete. v0's `cf.RulesetRule` and `cf.Ruleset` types already model rules, and `Raw`
   returns the same typed errors as every other call. v7's rulesets services replaced them in `feat/rulesets-v7`. R2
   jurisdictions came with R2Bucket's move onto v7 (`feat/generate-r2-bucket`)
3. **Move to the generated SDK later, controller by controller** (Phase 4, or sooner if the v0 line stops getting
   releases). Each controller already sits behind its own narrow interface, so `pkg/cloudflare.Client` can embed both
   SDKs during the move, with `errors.go` classifying both error types until it is done. Brought forward on 2026-10-13
   for the kinds the [code generator](../generator/CLAUDE.md#code-generator-kflare-gen) produces: they use v7 from
   the start, so
   `feat/cloudflare-go-v7` added v7 next to v0.119 (✅, see the v7 notes below). A hand-written controller moves when
   v7 buys it something (R2Bucket: jurisdictions and storage class), and the rule kinds moved together in
   `feat/rulesets-v7` (✅)

**v7 notes (`feat/cloudflare-go-v7`, v7.12.0, 2026-10-14):**
- `pkg/cloudflare.Client` carries v7's request options (`V7Options()`): the token, Cloudflare's production base URL and
  `http.DefaultClient`. A controller builds only the services it calls, as in
  `kv.NewNamespaceService(client.V7Options()...)`
- **kflare never imports v7's root package** (`github.com/cloudflare/cloudflare-go/v7`); golangci-lint's depguard
  refuses it. Its `NewClient` applies six `CLOUDFLARE_*` environment variables to every request (another token, an API
  key and email, a user service key, a base URL and extra headers), builds a service for every product, and gives each
  client its own transport, which with a client per reconcile would mean a connection pool per reconcile. And
  importing the package at all, which imports 134 of v7's other 135 packages, costs a third of the binary. Measured
  on the manager: 50.7 MB on main, 67.9 MB when `errors.go` named the error type through the root package, 51.1 MB
  through `kv`. So `errors.go` names it through `kv` (every v7 package re-exports it as `Error`), and
  a param field is set as the root package's `F` would set it, which code generated from the SDK does at no cost:
  `params.Title.Value, params.Title.Present = title, true`
- Errors: v7 returns one type, `*cloudflare.Error`, for every error status, with `StatusCode` and `Errors[].Code`.
  `errors.go` classifies it by status as v0's typed errors: any 4xx but 429 is terminal, 404 `IsNotFound`, 429
  `IsRateLimit`, and `HasErrorCode` reads the codes of a 4xx other than 401, 403, 404 and 429. A connection error is
  not a `*cloudflare.Error`, so it stays retryable. The tests get v7's errors from the SDK itself, through `httptest`
- Retries differ, and kflare keeps each SDK's defaults: v7 retries twice (0.5 s doubling, at most 8 s, or the
  `Retry-After` it is given) after a 408, 409, 429, 5xx or connection error; v0 retries a 429, 5xx or connection error
  up to three times (1 s doubling, at most 30 s). The difference ends when the last controller leaves v0
- v7's `Error()` reads `GET "<URL>": 404 Not Found <response body>`, too long for a condition. Since
  `feat/crud-reconciler`, `handleCloudflareError` writes `cfpkg.ErrorMessage(err)`, Cloudflare's messages and codes as
  v0 shows them (`namespace not found (10013)`, or `HTTP 503 Service Unavailable` without any); the returned error keeps
  the request line for the log. `Error()` also dereferences the error's `Request` and `Response`, so fakes build v7
  errors with `v7APIError` (`helpers_test.go`), which fills both in

**Bump notes (v0.89 → v0.119, 2026-10-12):**
- `DNSRecord` lost `ZoneID`, `ZoneName` and `Locked`. A DNSRecord's `status.cloudflareMetadata.zoneID` now comes from
  its Zone, as the field's documentation already said
- `DNSRecord` gained `settings` (`flatten_cname`), so every create and update sends `"settings":{}`. Verified live on
  `kflare.dev`: a create, then a content change through `UpdateDNSRecord`, changed only the content; TTL, comment,
  `proxied`, `settings` and `tags` stayed as they were
- The error types now `Unwrap()` to `*cf.Error`. `pkg/cloudflare/errors.go` classifies them as before: its `errors.As`
  targets are the typed errors
- The response envelope also accepts bare strings in `errors` and `messages`
- The request path, its retries and its rate limiting are unchanged. `go-retryablehttp` and `go-cleanhttp` left the
  module graph: in v0.89 only the experimental client and the STS code, which kflare does not use, imported them
- Unit and envtest suites (Kubernetes 1.34.1 and 1.37.0), lint, and the live e2e (10 of 10, run `92c397`) passed

#### Deferred: paid plan, add-on or account setup

Everything here was skipped, or built without live verification, because the dev account (Free zone `kflare.dev`, no
add-ons besides R2) cannot exercise it. Before starting any item below, re-check the limits: Cloudflare changes them.
Last checked 2026-10-07; the Free-plan limits and Zero Trust again on 2026-10-09, and Zero Trust on 2026-10-17.

**Not built yet — needs something we don't have:**

| Item | Needs | Cost (2026-10) | What was verified | When we come back to it |
|------|-------|----------------|-------------------|-------------------------|
| `LoadBalancerMonitor`, `LoadBalancer` (+ pools) | Load Balancing add-on (account) | $5/month incl. 2 origins, +$5/origin/month, +$0.50 per 500K DNS queries | Live, 2026-10-06: every monitor create fails, `interval is not in range [1, 1]`. The token already has Load Balancing: Monitors and Pools Write | Subscribe, build the monitor then the load balancer; cancel after if it was only for testing |
| `HealthCheck` (standalone, zone-level Health Checks) | Pro plan or higher on the zone; zone-level Health Checks Write on the token | Pro plan ~$20+/month per zone | Docs: Free plan gets 0 checks (Pro 10, Business 50). Token has Health Checks Read only | Not in the Phase 3 table yet. Add a branch once a Pro zone exists; keep it separate from `LoadBalancerMonitor` |

**Built, but not verified live because it costs money:**

| Item | Why | What exists | When we come back to it |
|------|-----|-------------|-------------------------|
| Zone `spec.plan` | Changing a plan bills the account | Implemented (subscription POST/PUT, `plan_pending`), unit-tested against response shapes read from the live Free zone | Exercise once on a disposable zone (upgrade, check status, downgrade); expect a prorated charge |
| WAFCustomRule `response` (was `blockResponse`) | Custom block responses need a Pro plan; a Free zone refuses them ("not entitled to use a custom response", dry run 2026-10-09) | Implemented to Cloudflare's documented limits (status 400–499, four content types, 2 KB body), checked by the generated specs. Dry runs on 2026-10-13 and 2026-10-27 confirmed the limits: Cloudflare validates a response before it checks the plan (codes 20048, 20050, 20051, 20052). The same response on a `RateLimitRule`, which Free allows, is verified live | On a Pro zone: create a block rule with a response, request the path, check the status and body |

**Worked around because the clean option is paid:**

| Item | Paid option | Current workaround | When we come back to it |
|------|-------------|--------------------|-------------------------|
| Ownership marker on the Cloudflare side (DNSRecord, and in general) | DNS record tags (Pro plan and up) | `kflare.dev/record-id` / `route-id` / `kv-namespace-id` / `d1-database-id` / `r2-bucket-name` / `ip-list-id` labels in the cluster; ARCHITECTURE.md lists "no ownership marker on the Cloudflare side" as a known limitation | With a Pro zone, tag records kflare creates; or a TXT ownership registry like external-dns (works on Free) |

**Free, but needs account setup first:**

| Item | Setup | Verified |
|------|-------|----------|
| Zero Trust (`AccessApplication`, `AccessPolicy`, `AccessGroup`) | Enable Zero Trust in the dashboard (pick a team name and the Free plan, up to 50 users). Not confirmed whether it asks for a payment method | Live, 2026-10-07, 2026-10-09 and 2026-10-17: the API returns `Access is not enabled`. The token already has the Access: Apps/Policies/Groups Write permissions |

**Free-plan limits to design around** (build for them; the e2e suite must stay within them). From Cloudflare's docs,
checked 2026-10-09; ✔ marks what a dry run on `kflare.dev` confirmed the same day:

| Item | Free | Pro | Business |
|------|------|-----|----------|
| WAF custom rules | 5 rules ✔, no regex ✔, no Log action ✔, no custom block response ✔, 1 zone-level custom ruleset | 20 rules | 100 rules, regex |
| Rate limiting rules | 1 rule ✔, period and mitigation timeout 10 s only ✔, counts by IP ✔, block only ✔, no counting expression ✔, no throttling (`mitigation_timeout: 0`) ✔, custom block response ✔ | 2 rules, periods up to 1 min, mitigation up to 1 h | 5 rules, periods up to 10 min, mitigation up to 1 day |
| WAF managed rulesets | Cloudflare Free Managed Ruleset only ✔, with overrides for the whole ruleset, a category or a rule ✔; no payload logging ✔ | + Cloudflare Managed Ruleset, OWASP Core Ruleset, Exposed Credentials Check | same as Pro |
| Custom lists | 1 IP list ✔, 10,000 items in the account ✔; the quota is per kind (a hostname list was accepted alongside) | 10 lists, 10,000 items in total | same as Pro |
| Transform, redirect, cache, configuration and origin rules | 10 rules each, transform rules counted across their three phases ✔, single redirects ✔, cache rules ✔, configuration rules ✔; no regex in transform ✔ or redirect rules ✔ | 25 each | 50 each; regex in transform and redirect rules |
| Origin rule overrides | destination port only ✔ (Host header, SNI and DNS record need Enterprise; refused on Free ✔) | same | same |

Rate limiting also requires `cf.colo.id` among the characteristics (code 20155) and a mitigation timeout of 0, equal
to the period or longer (code 20156). The docs limit Free expressions to path and verified-bot fields, but dry runs
accepted `http.host` and `http.request.method`; `http.user_agent`, `ip.src`, `http.request.uri.query` and the `matches`
operator were refused.

Controllers must not hard-code these limits: report Cloudflare's own rejection (`TerminalError`) instead, so higher plans
work unchanged.

**Plan corrections (resolved 2026-10-09):**

- `FirewallRule` → `WAFCustomRule`. The Firewall Rules and Filters APIs now return **410 Gone** ("This API has been
  deprecated. Please use the Rulesets API instead"). Custom rules live in the `http_request_firewall_custom` phase
- `RateLimit` → `RateLimitRule`. The previous rate limiting API (`/zones/{id}/rate_limits`) returns 410 too. Rate
  limiting rules live in the `http_ratelimit` phase
- `WAFPackage` → `ManagedRuleset`. The WAF packages API still answers, but lists 0 packages for `kflare.dev`. Managed
  rulesets are deployed by an `execute` rule in the `http_request_firewall_managed` phase. This corrects an earlier
  note: `kflare.dev` has **no** entry point in that phase. The Cloudflare Managed Free Ruleset
  (`77454fe2d30c4220b5701f6fdfb893ba`) only shows up as a managed ruleset; the docs say Cloudflare deploys it by
  default on Free zones
- `PageRule` → dropped. Page Rules is deprecated, and `GET /zones/{id}/pagerules` with the dev token returns code
  1011, "Page Rules endpoint does not support account owned tokens": the kind of token deploy.md tells users to create.
  Cloudflare's [migration guide](https://developers.cloudflare.com/rules/reference/page-rules-migration/) maps its
  settings to Redirect, Cache, Configuration, Origin and Transform Rules

#### Branch: `feat/kv-namespace-controller`
**Status:** ✅ Complete

- [x] `api/v1alpha1/kvnamespace_types.go`: `spec.accountRef` (immutable), `spec.title` (renamed in place);
  `status.cloudflareMetadata.namespaceID`. Generated since `feat/generator-types`
  (`zz_generated.kvnamespace_types.go`, from its `generator/generator.yaml` entry)
- [x] `internal/controller/kvnamespace_controller.go`: get by status ID → adopt by title → create; title drift
  corrected by rename; claims its namespace in `kflare.dev/kv-namespace-id` (`TitleConflict` otherwise). Since
  `feat/crud-reconciler` it is a `crudResourceKind` on cloudflare-go v7 (see the code generator's PR 2), and since
  `feat/generator-controllers` it is generated (`zz_generated.kvnamespace.go`)
- [x] WorkerScript binding `kvNamespaceRef` (same namespace): waits for the KVNamespace to be ready, hashes the
  resolved ID (a recreated namespace re-uploads the Worker), watches KVNamespaces
- [x] Deletion: a KVNamespace waits for the WorkerScripts bound to it; it counts as a user of its account.
  Deleting it deletes the data unless `retain`
- [x] Unit/envtest tests; live e2e spec (namespace + Worker binding checked via `ListWorkerBindings`). Since
  `feat/generator-controllers` the specs are generated, but for the WorkerScript ordering (`kvnamespace_test.go`)

**KVNamespace design notes (verified live):**
- `KVNamespaceAPI` interface: cloudflare-go v7's `kv.NamespaceService` (`New/Get/List/Update/Delete`). Until
  `feat/crud-reconciler` it was v0's, which has no single-namespace read, so the controller listed every namespace and
  matched the ID
- Titles are unique per account (duplicate → 400, code 10014) and required (an empty one → 400, 10019, "title is
  required"; verified 2026-10-14); unknown namespace ID → 404 (10013) for GET, PUT and DELETE; rename (PUT) keeps the ID
  and returns the renamed namespace. The list takes `per_page` up to 1,000 (more: 400, code 10008) and returns an empty
  page past the end; v7's page type keeps only `page` and `per_page` of the result info. Verified through v7 on
  2026-10-14
- Cloudflare deletes a namespace a Worker is bound to and leaves the binding dangling; every later upload of that
  Worker fails with 10041 "KV namespace not found". Hence the deletion ordering
- Needs the account-level **Workers KV Storage: Edit** permission

#### Branch: `feat/r2-bucket-controller`
**Status:** ✅ Complete

- [x] `api/v1alpha1/r2bucket_types.go`: `spec.accountRef` and `spec.name` (immutable), optional `spec.locationHint`
  (immutable, used only when kflare creates the bucket); `status.cloudflareMetadata.location`, `creationDate`
- [x] `internal/controller/r2bucket_controller.go`: get by name → adopt or create; claims the name in
  `kflare.dev/r2-bucket-name` (`NameConflict` when another R2Bucket for the same Cloudflare account holds it, even
  through a different CloudflareAccount); recreated, empty, if deleted outside kflare
- [x] WorkerScript binding `r2BucketRef` (same namespace): waits for the R2Bucket to be ready, binds by bucket name,
  watches R2Buckets
- [x] Deletion: waits for the WorkerScripts bound to it; counts as a user of its account. kflare never deletes
  objects: a bucket that holds any reports `BucketNotEmpty` and is checked again every minute. A bucket the R2Bucket
  never claimed is left alone
- [x] `pkg/cloudflare.HasErrorCode` tells 4xx rejections apart by Cloudflare error code
- [x] Unit/envtest tests; live e2e spec (bucket + Worker binding checked via `ListWorkerBindings`)

**R2Bucket design notes (verified live, 2026-10-07):**
- `R2BucketAPI` interface: `GetR2Bucket/CreateR2Bucket/DeleteR2Bucket` (account `rc`). A bucket has no ID; its name
  identifies it in the account
- Duplicate create → 409 (10004); missing bucket → 404 (10006) for GET and DELETE; invalid name → 400 (10005). Names are
  3 to 63 characters: 63 was taken, while 64, a trailing hyphen, a capital, an underscore and 2 characters were refused
  (2026-10-14, through v7). The OpenAPI spec says up to 64, with a pattern not anchored at the end; unknown location
  hint → 400 (10044, lists `wnam, enam, weur, eeur, apac, oc, auto`). GET returns `location`, `storage_class` and
  `jurisdiction`; LIST returns them as null
- Deleting a bucket that holds objects → 409 (10008)
- Cloudflare deletes a bucket a Worker is bound to; every later upload of that Worker then fails with 10085, and a
  Worker bound to a missing bucket cannot be uploaded at all. Hence the deletion ordering and the readiness wait
- Bindings resolve the bucket by name: a Worker kept working, without a re-upload, after its bucket was deleted and
  created again under the same name (while it was gone the Worker threw error 1101). The binding hash is therefore the
  bucket name only, unlike KV where a recreated namespace has a new ID
- Needs the account-level **Workers R2 Storage: Edit** permission

**Not supported yet:**
- A WorkerScript binding of a bucket outside the default jurisdiction: WorkerScript uploads through cloudflare-go
  v0.119, whose Worker R2 binding has no `jurisdiction` field, so it refuses one (`R2BucketJurisdictionUnsupported`)
  until it moves onto v7 (Generate every kind, PR 20)
- Bucket configuration that Terraform models as separate resources: CORS, lifecycle rules, custom domains and the
  r2.dev public URL, event notifications, bucket locks, Sippy

**Generated** (`feat/generate-r2-bucket`, 2026-10-28; [Generate every kind](../CLAUDE.md#generate-every-kind), PRs 5
and 6 in one, at the user's request). Its entry in `generator/generator.yaml` replaces the hand-written types and
controller, on cloudflare-go v7's `r2.BucketService`, and needed kflare-gen's CRUD kinds named by their key, header
params and deletion blockers ([generator/CLAUDE.md](../generator/CLAUDE.md)). The spec keeps `name` and `locationHint`,
so v0.3 manifests apply unchanged, and gains `jurisdiction` and `storageClass`, both defaulted to Cloudflare's own
defaults; status adds `bucketName` and reports `creationDate` as Cloudflare's string (CHANGELOG.md lists the changes).
The user OK'd mapping the storage class and probing Infrequent Access live on 2026-10-28, accepting a few cents of
usage (the buckets stay empty). Probes on the dev account that day, every `kflare-probe-*` bucket deleted afterwards:

- `cf-r2-jurisdiction: eu` and `us` create buckets in those jurisdictions on this account; `fedramp` is refused (403,
  10003 "Access Denied") and `fedramp-high` too ("invalid jurisdiction", plain text). The enum keeps all five, the
  looser rule: a FedRAMP account can use them. A GET or DELETE without the header reaches the default jurisdiction
  only, and `default` is the same as no header
- The same name exists in two jurisdictions at once (`kflare-probe-r2-j` in `default` and `eu`): a jurisdiction holds
  buckets of its own, so the address of a bucket is its jurisdiction and its name, and kflare's claims compare both.
  Deleting one left the other. A second bucket of a name in one jurisdiction is refused (409, 10004)
- A location hint does not move a bucket out of its jurisdiction: `us` with `locationHint: weur` stored `ENAM`
- Storage class: a create with `storageClass: InfrequentAccess` stored it, and `PATCH` with `cf-r2-storage-class`
  changed it both ways, in either jurisdiction, keeping the bucket (same `creation_date`). A PATCH without the header or
  with another class is refused (400, 10062); a create with another class too (10040, "not well formed")
- GET reports `location` in capitals (`EEUR`, `ENAM`), where v7's enum has them in lower case: status takes it as it is
- The list pages by cursor, not by page number, and kflare does not need it: a bucket's name is its ID, so a resource
  reads its bucket by name, as the hand-written controller did

#### Branch: `feat/rulesets-foundation`
**Depends on:** `feat/zone-controller`, `feat/cloudflare-go-v0.119`
**Merges into:** main, before any rule CRD branch starts (no stacked PRs)
**Status:** ✅ Complete

Shared machinery for every resource that is one rule in a zone's phase entry point ruleset. It has no CRD of its own:
it is unit-tested against a fake that follows the live API, was run live on `kflare.dev` (2026-10-12: entry point
creation, idle sync, placement, reordering, drift, deletion), and `WAFCustomRule` adds it to the e2e suite.

- [x] Live probes on `kflare.dev` (2026-10-12, `kflare-probe-*` refs; everything deleted afterwards, the entry point
  too). Results below
- [x] `pkg/cloudflare/rulesets.go`: `CreateRulesetRule`, `UpdateRulesetRule`, `MoveRulesetRule` (position only) and
  `DeleteRulesetRule` on `*Client`, through `API.Raw`; each returns the updated `cf.Ruleset`. The delete replaces
  cloudflare-go's, which fails on the live API (it expects an empty 204; Cloudflare answers 200 with the ruleset).
  Unit tests against `httptest`. Since `feat/rulesets-v7`, cloudflare-go v7's rulesets services make these calls and
  the file is gone (see the code generator's notes)
- [x] `internal/controller/rulesetrule.go` (`RulesetRuleAPI`):
  - `syncRulesetRule(zoneID, phase, rule, peers)`: gets the phase's entry point. On 404 it creates it holding the
    rule (`kind: zone`, name `default`); on 400 code 20217 (another resource created it first) it reads it again.
    It finds the rule by ref: missing → POST, placed among its peers; different → PATCH the whole rule; same → no
    write. A write that gets a 404 (the rule or ruleset was deleted meanwhile) starts over once. Then it orders the
    peers. It returns the ruleset ID, rule ID and rule version, and whether it wrote or moved anything
  - `deleteRulesetRule(zoneID, phase, ref)`: a missing entry point or rule counts as deleted. The entry point stays,
    even when emptied (except in the managed phase, since `feat/managed-ruleset-controller`)
  - It never PUTs an entry point: that replaces every rule in it, including rules kflare does not manage
- [x] Ownership on the Cloudflare side through `ref` (`rulesetRuleRef`): `kflare-` + the first 32 hex characters of
  SHA-256 over `<kind>/<namespace>/<name>`. Refs are unique within a ruleset (duplicate: code 20023), at most 128
  bytes (code 20166) and cannot change once set. A ref derived from the name survives cluster loss and backup
  restores, which a UID would not. kflare never touches a rule whose ref it did not derive, and does not adopt rules
  implicitly: rules have no natural key
- [x] Ordering (`rulesetRulePeer`: ref, priority, namespace, name, kind). The caller passes every kflare rule in the
  zone and phase, across kinds. A new rule goes right after the closest peer that sorts before it, else right before
  the closest that sorts after it. Then one pass moves any peer whose rule sits before its predecessor's to right
  after it, with a position-only PATCH, which changes neither definition nor version. This moves other resources'
  rules too, but only kflare's; rules created outside kflare keep their place. Changed from "move only the reconciled
  rule": that cannot always restore the order, because a misplaced peer may never reconcile. `moved` tells the caller
  to check again shortly, since two syncs reordering at once from different cluster views can leave rules out of order
- [x] Drift (`rulesetRuleMatches`): compares the definitions as JSON values, without the fields Cloudflare sets (ID,
  version, last update) and without nulls and empty objects, and counts Cloudflare's defaults as set (`enabled:
  true`; `logging: {enabled: true}` on skip rules). Callers set every field they want: an update resets the rest
- Moved to `feat/waf-custom-rule-controller`, which has the first CRD: listing the peers, the requeue after a move,
  rule kinds in `zoneDependents`, the e2e check that an idle reconcile leaves the ruleset version alone, and
  ARCHITECTURE.md

**Rulesets API behavior (verified live on `kflare.dev`, 2026-10-12):**
- POST `/zones/{zone}/rulesets` (`kind: zone`, the phase) creates the entry point, the ruleset and its rule each at
  version 1. A second zone entry point in the phase → 400, code 20217
- A rule comes back exactly as sent plus `id`, `version` and `last_updated`, with two defaults: `enabled: true` when
  omitted, and `logging: {enabled: true}` on a skip rule
- POST `/rules` appends unless `position` says otherwise; two concurrent POSTs both land. Every write bumps the
  ruleset's version and returns the whole ruleset
- PATCH replaces the whole rule: an omitted `description` is reset, an omitted `enabled` becomes true, an omitted
  `ref` is kept. The rule keeps its `id`; its version goes up. A different ref → 400, codes 20023 and 20142
- A PATCH with only `position` moves the rule without changing its definition or version
- DELETE `/rules/{id}` → 200 with the updated ruleset. Deleting the last rule leaves the entry point, with
  `rules: null`; DELETE `/rulesets/{id}` removes it (204)
- Unknown rule → 404, code 10002 (PATCH and DELETE); unknown ruleset → 404, code 10001; over the plan's quota → 400,
  code 50001 (also for a single-rule POST)
- cloudflare-go v0.119's `DeleteRulesetRule` deletes the rule, then returns an untyped error: it treats the response
  body as a failure. v7's `Rules.Delete` reads it (live, 2026-10-15)

**Rulesets facts (dry runs on `kflare.dev`, 2026-10-09; nothing written):**
- `kflare.dev` has no zone entry point in any phase: `GET .../rulesets/phases/{phase}/entrypoint` returns 404, code
  10003. Its zone rulesets are only Cloudflare's managed ones (DDoS L7, Managed Free, Normalization)
- Rejections are 400 unless noted. Over the plan's rule quota: code 50001 ("exceeded the maximum number of rules in
  the phase http_request_firewall_custom: 6 out of 5"). Plan entitlement (regex operator, Log action, custom block
  response, paid managed ruleset, rate limiting period or action): `code: null`, with a message starting "not
  entitled". A missing permission for the phase: 403, `code: null`, "request is not authorized"
- `HasErrorCode` cannot tell `code: null` errors apart. They are all `TerminalError`, and the condition shows
  Cloudflare's message, which is what the user needs
- The rule-level calls take an optional `position` (`before` or `after` a rule ID, where `""` means first or last, or a
  1-based `index`; by default the rule is appended) and return the whole ruleset. Every Rulesets write accepts
  `dry_run=true`


#### Branch: `feat/waf-custom-rule-controller`
**Depends on:** `feat/rulesets-foundation`
**Status:** ✅ Complete; generated since `feat/generate-waf-custom-rule` (below)

- [x] Shared API types for every rule kind (`api/v1alpha1/rulesetrule_types.go`): `RulesetRuleSpec`, embedded inline
  (`zoneRef` immutable, `expression` 1–4096 characters, `enabled` default true, `description` up to 4096 characters,
  `priority`), and `RulesetRuleStatus` with `cloudflareMetadata` (zone ID, ruleset ID, rule ID, ref, version). Each
  kind implements `GetRulesetRuleSpec` and `GetRulesetRuleStatus`
- [x] `api/v1alpha1/wafcustomrule_types.go`: the shared fields plus `action` (`block`, `challenge`, `js_challenge`,
  `managed_challenge`, `skip`, `log`); `skip` (`remainingRules`, `phases`, `products`, `log` default true), required
  with `action: skip`, allowed only with it, and required to skip something (CEL); `blockResponse` (`statusCode`
  400–499, default 403; `contentType` text/html, text/plain, application/json or text/xml; `content` up to 2 KB),
  allowed only with `block`. Short name `cfwaf`
- [x] Shared rule controller (`internal/controller/rulesetrule_controller.go`), the reconcile loop of every rule kind:
  Zone readiness, credentials, peers, `syncRulesetRule`, status, a requeue after 10 s when it moved rules, and deletion
  by ref (in the zone of the last sync, else the Zone's; skipped when the Zone is gone). A kind supplies only a
  `rulesetRuleKind` (name, phase, constructors, spec → rule conversion), listed in `rulesetRuleKinds`. Peers are the
  resources of every kind in the same phase and Cloudflare zone, in any namespace, that are not being deleted
- [x] `WAFCustomRuleReconciler`: phase `http_request_firewall_custom`. It sorts the skipped phases and products, which
  Cloudflare keeps in the order sent (verified live), so reordering them in the spec writes nothing; it sets `logging`
  only on skip rules
- [x] A Zone waits for its WAFCustomRules before it is deleted
- [x] Unit and envtest tests against the Rulesets fake, CEL validation cases, and a live e2e spec: a block rule and a
  lower-priority skip rule, placed before it; an idle reconcile leaves the ruleset version alone; deletion removes both
- [x] Sample, deploy.md permission row, Helm `edit` role, ARCHITECTURE.md (new "Rules" section)

**WAFCustomRule design notes (dry runs and probes on `kflare.dev`, 2026-10-12):**
- Free accepts `block`, `challenge`, `js_challenge`, `managed_challenge` and `skip`; `log` is refused ("not entitled")
- Skip options Free accepts, alone or together: `ruleset: current`; `phases` `http_ratelimit`,
  `http_request_firewall_managed` and `http_request_sbfm`; `products` `zoneLockdown`, `uaBlock`, `bic`, `hot`,
  `securityLevel`, `rateLimit` and `waf`; `logging: {enabled: false}`
- Rejections (all 400): a skip without action parameters (20115); a phase that cannot be skipped (20120); an unknown
  product (20119); `logging` on any other action (20018); an expression that does not parse or is over 4096 characters
  (20127); a description over 4096 characters (20208)
- `blockResponse` follows Cloudflare's documentation (Pro plans and up); it is not verified live, see Deferred
- deploy.md documents Zone · Zone WAF · Edit, which is enough on its own (verified 2026-10-13, see Token permissions)

**Generated** (`feat/generate-waf-custom-rule`, 2026-10-27; [Generate every kind](../CLAUDE.md#generate-every-kind),
PR 3). Its entry in `generator/generator.yaml` replaces the hand-written types and controller. A skip rule's
parameters sit at the spec's root as the API names them (`ruleset: current`, `phases`, `products`), its logging is the
rule's own `logging`, and the block response is `response`; nothing defaults any more, so the response's status code
is set in the spec (CHANGELOG.md lists the changes). The entry keeps what the notes above verified: the phases a skip
can skip and the four content types, as enums, the 2,048-byte body, `logging` only on skip rules, and a skip that
skips something (kflare-gen's rule for an action whose parameters must set one). Dry runs on `kflare.dev` on
2026-10-27 (`?dry_run=true`, which stores nothing) checked again the refusals the entry encodes, and settled the rest:

- Skip's `phase: current` is accepted, alone, with `products` and with `logging`, and is mapped. Cloudflare refuses it
  with `phases` (20247) and with `ruleset` (20116), neither of which the API description says: two CEL rules
- Refused in this phase, and ignored with these as reasons: skip's `rulesets` (20117) and `rules` (20118), which
  belong to the managed phase, `ratelimit` (20131) and `exposed_credential_check` (20189, "not allowed in phase
  http_request_firewall_custom with kind zone")
- The block response: a status outside 400–499 or none (20048), another content type (20050), an empty body (20051)
  and one over 2,048 bytes (20052) are refused before the plan is checked; a valid one is "not entitled" on Free
- Also refused: a skip without parameters or with an empty `phases` (20115), its own phase or one listed twice
  (20120), `logging` on `block` or `managed_challenge` (20018), and the `log` action on Free ("not entitled")

Cloudflare logs a skip rule's matches when `logging` is left out, as its docs say, and kflare counts that as unset,
so a skip rule that leaves it out is idle. The live e2e spec updates its skip rule in place to `phase: current` with
`logging` left out, and checks the rule Cloudflare stores and that a reconcile then writes nothing.

#### Branch: `feat/rate-limit-rule-controller`
**Depends on:** `feat/rulesets-foundation`
**Status:** ✅ Complete; generated since `feat/generate-rate-limit-rule` (below)

- [x] `api/v1alpha1/ratelimitrule_types.go`: the shared fields plus `action` (default `block`; `challenge`,
  `js_challenge`, `managed_challenge`, `log`), `characteristics` (a set, default `[ip.src]`; kflare adds `cf.colo.id`),
  `period` (seconds, default 10), `requestsPerPeriod` (1 to 1,000,000), `mitigationTimeout` (unset: the period; CEL: 0
  or at least the period), `countingExpression` (up to 4096 characters), `requestsToOrigin`, and `blockResponse`
  (status 400–499, default 429; the four content types; body up to 30,720 characters), allowed only with `block`
  (CEL). Short name `cfratelimit`. `blockResponse` was not in the plan: it was added because Free accepts it here
- [x] `RateLimitRuleReconciler`: the shared rule controller in the `http_ratelimit` phase. Its conversion puts
  `cf.colo.id` first and sorts the other characteristics, and always sends a mitigation timeout
- [x] A Zone waits for its RateLimitRules before it is deleted
- [x] Unit and envtest tests against the Rulesets fake (the written rule, idle syncs with every option set, in-place
  updates, rules kept apart from the zone's WAF custom rules, deletion, defaults, CEL validation), a conversion table,
  and a live e2e spec: one rule for `/kflare-e2e-<run>`, checked field by field (custom JSON response included),
  updated in place, then left alone by an idle reconcile. Free allows exactly one rate limiting rule, so the suite
  must not run twice at once, and `kflare.dev` must hold no other
- [x] Sample, deploy.md (permission row, kinds table, uninstall), Helm `edit` role, ARCHITECTURE.md

**RateLimitRule design notes (dry runs and a stored probe on `kflare.dev`, 2026-10-13):**
- Free accepts only `block`; the four other actions are refused as "not entitled". `skip` and `execute` are not rate
  limiting actions at all (20133), and a rule without `ratelimit` is refused in this phase (20132)
- Cloudflare reads a missing `mitigation_timeout` as 0, which throttles: on Free, "not entitled to use throttle
  behaviour". So kflare always sends one; cloudflare-go leaves out an explicit 0, which therefore still means 0
- Validation codes: characteristics without `cf.colo.id`, with only `cf.colo.id`, with a duplicate or with an unknown
  field (20155); a mitigation timeout that is not 0 and below the period (20156); a period outside the API's list,
  which is 10, 15, 20, 30, 40, 45, 60, 90, 120, 180, 240, 300, 480, 600, 900, 1200, 1800, 2400, 3600 and 65535
  seconds (20157; Cloudflare's docs list fewer); requests per period outside 1 to 1,000,000 (20158). kflare's CRD
  checks the requests range and the timeout rule, and leaves the period to Cloudflare
- Free accepts a custom block response here, unlike in WAF custom rules: status 400–499 (20048), content type one of
  four (20050), body required (20051) and at most 30,720 bytes (20052; WAF custom rules allow 2,048). It also accepts
  `requests_to_origin` and an empty counting expression; a non-empty one needs a higher plan
- Stored as sent: characteristics keep the order sent; `requests_to_origin: false` and an empty counting expression
  come back absent, as cloudflare-go sends them
- deploy.md documents Zone · Zone WAF · Edit, which is enough on its own (verified 2026-10-13, see Token permissions)

**Generated** (`feat/generate-rate-limit-rule`, 2026-10-27; [Generate every kind](../CLAUDE.md#generate-every-kind),
PR 4, taken before PR 3). Its entry in `generator/generator.yaml` replaces the hand-written types and controller. The
rate limit is the rule's own `ratelimit` object, and the block response is `response`, as the API names them; nothing
defaults any more, so `action`, the characteristics, the period, the mitigation timeout and the response's status code
are set in the spec (CHANGELOG.md lists the changes). The entry keeps what the notes above verified, as CEL rules on
`ratelimit` and validations: `requestsPerPeriod` required and at most 1,000,000 (20158), the mitigation timeout
required and 0 or at least the period (20156), `cf.colo.id` and another field among the characteristics (20155),
which kflare no longer adds, the response's four content types (20050) and its 30,720-byte body (20052). New, verified
on `kflare.dev` on 2026-10-27:

- Dry runs (`?dry_run=true`, which returns no rule): `requests_to_origin: false` and `cf.colo.id` listed second are
  accepted; `mitigation_timeout: 0` is refused on Free ("not entitled to use throttle behaviour"). A rule without
  `requests_per_period` is refused as 0 (20158). Score counting is not entitled on Free ("not entitled to use score
  fields in ratelimiting"); it replaces `requests_per_period` (20159, 20131) and needs `score_response_header_name`
  (20161). `logging` is refused (20018), and so is `exposed_credential_check` (20189, "not allowed in phase
  http_ratelimit with kind zone"). `challenge`, `js_challenge` and a counting expression are not entitled on Free.
  The score fields, `logging` and `exposed_credential_check` are ignored with these as reasons
- A stored probe, a `kflare-probe-requests-to-origin-false` rule added with `requests_to_origin: false` (the rules
  endpoint, as kflare writes), read back and deleted: Cloudflare stores false as absent, both in the write's answer and
  in a separate read. The generated conversion sent every optional field a spec sets, so the rule would have differed
  from the stored one on every reconcile. kflare-gen's `omitZero` (a rule kind's optional boolean or integer, with
  its reason) sends the field only when it is not zero; `requests_to_origin` and `mitigation_timeout` take it, as the
  hand-written conversion did. The CRD keeps the pointer, so a spec can still say 0 or false


#### Branch: `feat/ip-list-controller`
**Depends on:** `feat/shared-client`
**Status:** ✅ Complete

An account-level custom IP list, for expressions such as `ip.src in $office_ips` in rules.

- [x] `api/v1alpha1/iplist_types.go`: `spec.accountRef` and `spec.name` (immutable; `^[a-z0-9_]{1,50}$`, Cloudflare's
  rule), `description` (up to 500 characters), `items[]` (a map keyed by `ip`: an address or CIDR range, checked
  against a character set; optional `comment`, up to 500 characters). `status.cloudflareMetadata`: list ID, item
  count, and the bulk operation kflare is waiting for. Short name `cfiplist`
- [x] Controller: lists the account's lists → matches the status ID → adopts the IP list with the same name
  (`NameConflict` when another IPList claims it or it is a list of another kind) → creates one (`kind: ip`); claims it
  in `kflare.dev/ip-list-id`. Description drift through `UpdateList`. Items are compared as a set in canonical form,
  and replaced with `ReplaceListItemsAsync`: the IPList records the operation in status, reports `ReplacingItems` and
  checks again every 2 seconds, and a failed operation is retried with back-off. Not the SDK's `ReplaceListItems`,
  which polls for up to 8.5 minutes and would block a worker
- [x] Deletion: `ListInUse` while a rule uses the list (Cloudflare refuses, 10041), checked again every minute and
  re-triggered by the deletion of a rule that uses it. A list whose status was never written is found through its
  claim. An IPList counts as a user of its account
- [x] The rule kinds watch IPLists: a rule whose expression uses a list (`$name`) is reconciled once the list exists,
  since Cloudflare refuses it until then (20127, a terminal error). Added during the branch: without it, a list and a
  rule applied together could leave the rule failed until the next resync
- [x] Unit and envtest tests against a fake that follows the live API, and a live e2e spec: a WAF custom rule that uses
  the list is refused before the list exists and written after; items replaced in place; an idle reconcile leaves the
  list alone; the all-at-once deletion removes both
- [x] Sample (a list and a rule that uses it), deploy.md, Helm `edit` role, ARCHITECTURE.md

**IPList design notes (verified live on the dev account, 2026-10-13; the probe lists were deleted):**
- Names: lowercase letters, digits and underscores, 1–50 characters (else 400, code 10029), unique in the account across
  list kinds (10021). Only the description can change (a new name: 10076). Descriptions hold at most 500 characters
  (10018); an empty one comes back as null
- Free: one IP list (a second: 10019, "maximum number of lists"), but a hostname list was accepted alongside it, so the
  quota is per kind; 10,000 items in the account (10020)
- Replacing items checks them all before it starts and refuses the whole request (400): an invalid address, including a
  zone or leading zeros (10037); host bits set (10036); an IPv4 range outside /8–/32 (10034) or an IPv6 one outside
  /12–/128 (10035); an IPv4-mapped IPv6 address (10080); one address twice, in any form (10028); a comment over 500
  characters (10022). Overlapping ranges are fine, and an empty array empties the list
- The replace runs as a bulk operation (`pending`, `running`, `completed`, `failed`): one finished within a second,
  10,000 items within a few. Operations run in order, so the later of two replaces wins. An unknown operation is a
  404 (10001)
- Cloudflare stores a /32 or /128 as the address and IPv6 in lowercase, compressed; comments verbatim (whitespace,
  newlines, Unicode), an empty one as null; items in no particular order. Items come 25 to a page by default and 500
  at most (a larger page: a misleading 10027, "invalid or expired cursor")
- An unknown list is a 404 (10001), but DELETE is idempotent (200 for an unknown ID). Deleting a list a ruleset rule
  uses: 400, 10041, "This list is referenced by at least one ruleset rule". The list's `num_referencing_filters` stays 0
  meanwhile, so it cannot tell kflare whether a rule uses the list
- A rule that uses a list the account does not have: 400, 20127, "the list ... is unknown"
- deploy.md documents Account · Account Rule Lists · Edit (the API's Account Rule Lists Write), which is enough on its
  own (verified 2026-10-13, see Token permissions)


#### Branch: `feat/zero-trust-controllers`
**Depends on:** `feat/shared-client`
**Blocked on:** enabling Zero Trust on the account, a dashboard step (still `Access is not enabled` on 2026-10-09)
**Status:** 🔲 Not started

- [ ] `AccessGroup` (include, exclude and require rules), `AccessPolicy` (a reusable, account-level policy that
  references groups) and `AccessApplication` (self-hosted: domains, session duration, and policies by reference with a
  precedence). One PR per kind if the branch grows large
- [ ] Reusable policies (`/access/policies`) are in the SDK since v0.119; v0.89 could only create app-scoped policies
- [ ] Deletion order: an application before its policies, a policy before its groups, using `protection.go`. Check
  live what Cloudflare refuses
- [ ] Live e2e once Zero Trust is enabled (the free plan covers up to 50 users)


#### Branch: `feat/managed-ruleset-controller`
**Depends on:** `feat/rulesets-foundation`
**Status:** ✅ Complete; generated since `feat/generate-managed-ruleset` (below)

Deploys a Cloudflare managed ruleset to a zone, with overrides, through an `execute` rule in the
`http_request_firewall_managed` phase. Built by hand on the v0.119 foundation (decided 2026-10-13), not generated;
`feat/rulesets-v7` moved its conversion to v7 with the other rule kinds.

- [x] **Verified first** (live on `kflare.dev`, 2026-10-14; see the notes below): any entry point in this phase, even an
  empty one, stops Cloudflare's default deployment of the Free Managed Ruleset, and deleting the entry point brings it
  back. So kflare deletes the phase's entry point once deleting a rule leaves it empty
- [x] `api/v1alpha1/managedruleset_types.go`: the shared rule fields plus `rulesetID` (32 hex characters, immutable)
  and `overrides`: `enabled` and `action` for the whole ruleset, and `categories[]` and `rules[]` (maps keyed by
  category and rule ID, each needing `enabled` or `action`), overriding at least something (CEL). Actions: `block`,
  `challenge`, `js_challenge`, `managed_challenge`, `log`. Short name `cfmanaged`
- [x] `ManagedRulesetReconciler`: the shared rule controller in the managed phase. Its conversion sorts the category and
  rule overrides, which Cloudflare keeps in the order sent
- [x] Foundation changes:
  - `comparableRule` counts the `version: latest` Cloudflare records on an execute rule as set
  - `deleteRulesetRule` deletes an entry point it leaves empty in the phases of `emptiedEntrypointPhases` (only this
    one), unless another kflare resource in the phase and zone is about to write to it (`peersRemain`).
    `RulesetRuleAPI` gained `DeleteRuleset` (cloudflare-go's, which handles the 204)
  - A kind can list Cloudflare error codes that mean a conflict (`rulesetRuleKind.conflicts`): the resource reports
    the reason and checks again every minute. ManagedRuleset lists 20014 as `RulesetConflict`
  - In this phase the rule controller also watches the deletion of the phase's kinds and re-triggers the kind's
    resources last synced to the same zone or not synced yet (`rulesInZoneOfDeleted`). A rule written in the moment
    before the entry point went is written again, and a conflict clears as soon as the other resource is gone
- [x] A Zone waits for its ManagedRulesets before it is deleted
- [x] Unit and envtest tests against the Rulesets fake (which now also defaults execute rules to the latest version,
  refuses a second execute of the same ruleset with 20014 and deletes rulesets), a conversion table, CEL validation,
  and a live e2e spec (run `21c184`, 14 of 14): the Free Managed Ruleset with category overrides checked field by
  field, a rule override added in place, an idle reconcile that writes nothing, and the all-at-once deletion leaving no
  entry point in the phase
- [x] Sample, deploy.md (permission row, kinds table, uninstall, `RulesetConflict`), Helm `edit` role, ARCHITECTURE.md
  (new "Managed rulesets" section)
- **Exceptions** (`skip` rules in this phase) are not in this branch. Decided 2026-10-14: a separate kind,
  `ManagedRulesetException`, not scheduled yet. An exception alone in the phase would switch off Cloudflare's default
  deployment, so it must reference a ManagedRuleset (`managedRulesetRef`) and wait for it to be ready. It must also run
  before the `execute` rule it skips, which needs a rank per kind in the foundation's ordering. Meanwhile a rule or
  category override with `enabled: false` turns a misfiring rule off for every request
- **An entry point that was empty before kflare** is deleted like any other once kflare's last rule leaves it
  (decided 2026-10-14). That entry point had turned the default off, so deleting the last ManagedRuleset turns it back
  on. Telling the two apart would need a mark on the entry points kflare creates, and then an entry point someone else
  created, and later emptied, would stay and leave the zone without managed rules. Turning the default back on is the
  safer mistake. To keep it off, keep a ManagedRuleset with `enabled: false`

**ManagedRuleset design notes (verified live on `kflare.dev`, 2026-10-14):**
- The default deployment was checked with a probe Worker behind a proxied `kflare-probe-*` hostname. A request carrying
  a Log4j string in a header or the query got Cloudflare's 403 block page while the phase had no entry point. It got
  through (200) about 10 seconds after an entry point appeared holding only an unrelated skip rule, and still got
  through once that rule was deleted and the entry point was empty. It was blocked again once an explicit `execute`
  rule deployed the Free Managed Ruleset, and blocked again by default about 5 seconds after the emptied entry point
  was deleted (DELETE `/rulesets/{id}`, 204). The probe objects were deleted and the zone left as found
- The Free Managed Ruleset (`77454fe2d30c4220b5701f6fdfb893ba`, version 74 on 2026-10-14) holds 31 rules, all `block`,
  tagged with categories such as `wordpress`, `log4j` and CVE IDs
- An execute rule comes back as sent plus `action_parameters.version: "latest"`. An explicit `"latest"` is accepted;
  any other version is refused ("version pinning cannot be used", 20055). Overrides come back exactly as sent, in the
  order sent
- Dry runs on Free: overrides for the whole ruleset (`enabled`; `action` `block`, `challenge`, `js_challenge`,
  `managed_challenge` or `log`), per category and per rule are accepted. Refused: `skip` as an override action
  (20067); an override without `enabled` or `action` (20063); an unknown category (20235) or rule (20233); a category
  or rule listed twice (20236, 20234); `sensitivity_level` on the whole ruleset (20069). `score_threshold` on a rule is
  accepted but only means something in the OWASP Core Ruleset, so kflare does not expose it yet
- Also refused: payload logging (`matched_data`) and the paid rulesets ("not entitled"), an unknown ruleset (20226),
  `logging` on an execute rule (20018), the `block` action in this phase (20016), and a second execute rule for a
  ruleset the zone already deploys (20014, "more than one rule is trying to execute the same managed ruleset")
- Exceptions: Free accepts skip rules here with `rulesets`, `rules`, `ruleset: current` and `phases`
- deploy.md documents Zone · Zone WAF · Edit, which is enough on its own (verified 2026-10-14, see Token permissions)

**Generated** (`feat/generate-managed-ruleset`, 2026-10-27; [Generate every kind](../CLAUDE.md#generate-every-kind),
PR 2). Its entry in `generator/generator.yaml` replaces the hand-written types and controller, and keeps what the
notes above verified: the five override actions (`validation.enum`; `skip` refused, 20067), the category and rule
overrides keyed by category and ID (`listMapKey`; 20236, 20234), each override setting `enabled` or `action` (the
API description's own rules; 20063), conflict 20014 as `RulesetConflict`, and the Ruleset column (`printColumn`, which
rule kinds now take). Ignored, with these notes' codes as reasons: `matched_data` (not entitled), `sensitivity_level`
at every level (20069; DDoS phases only), `score_threshold` (only the OWASP ruleset scores), `logging` (20018), and
`ratelimit` and `exposed_credential_check` (never sent, not verified in this phase). What changes, listed in
CHANGELOG.md: `rulesetID` is `id`, which may change in place (the rule is updated to run the other ruleset); the
overrides go in the spec's order, which Cloudflare keeps, rather than sorted; and an empty list of overrides is
refused. The envtest specs that only ManagedRuleset had (the entry point its deletion empties, kept while another
resource is about to write to it, and the resources re-triggered) are now shared specs, run for every kind in a phase
whose emptied entry point kflare deletes. Verified live with the CI token (run 40543255707, e2e run `b64702`, 27 of
27): the deployment written, its overrides changed in place, an idle reconcile, and the managed phase left with no
entry point.


#### Branch: `feat/managed-transform-controller`
**Depends on:** `feat/zone-controller`, the code generator's settings kinds (`feat/generator-singletons`, #92)
**Status:** ✅ Generated, and verified live on 2026-10-23 (run `3f5c86`)

The first settings kind, generated end to end: its entry in `generator/generator.yaml`, the generated output and a live
e2e spec. It replaces the plan written before kflare-gen had settings kinds, which listed the transforms to enable by
ID and disabled every other one.

**ManagedTransform design notes (verified live on `kflare.dev`, 2026-10-22 and 2026-10-23):**
- Its spec is v7's Edit params, `managedRequestHeaders` and `managedResponseHeaders`: lists of `{id, enabled}` keyed
  by `id` (`listMapKey`), so the API server refuses a transform listed twice. Drift compares the transforms a spec
  lists, and those it does not list keep their state, whoever turned them on. The old plan's "every transform not
  listed is disabled" is a spec that lists them all. Flagged to the user with #92 (2026-10-23), who went ahead
- Free: six transforms, `add_client_certificate_headers`, `add_visitor_location_headers`, `remove_visitor_ip_headers`
  and `add_waf_credential_check_status_header` for requests, `remove_x-powered-by_header` and `add_security_headers`
  for responses, all off on `kflare.dev`, with no conflicts (`conflicts_with` empty). Cloudflare lists only the
  transforms a zone's plan has, and its documentation says it refuses to turn on one that conflicts with one that is on
  (`add_true_client_ip_headers` and `remove_visitor_ip_headers`); a Free zone cannot show that refusal
- Unconfigured: every transform off, as `kflare.dev`, which nobody has configured, holds them; Cloudflare's reset
  ("Disables all Managed Transforms") turns them all off, those a spec does not list included. Deleting the
  ManagedTransform that configures the zone resets it, as the resource took over the zone's managed transforms
- One ManagedTransform per Cloudflare zone, with the settings kinds' sticky ownership (see ARCHITECTURE.md's
  Settings). The plan's `has_conflict` and `conflicts_with` are not surfaced in status: a conflicting PATCH is refused
  with Cloudflare's message, which the `TerminalError` condition carries
- The permission: Managed headers Write, the dashboard's Zone · Managed Headers · Edit. Not a Rulesets endpoint, so no
  dry run. Config Settings Write, which the OpenAPI spec lists for the PATCH, was refused for an empty PATCH on
  2026-10-17, with the dev token's account-level Transform Rules Write too
- Live e2e spec: it checks that the zone's managed transforms are all off, turns `add_visitor_location_headers` and
  `remove_x-powered-by_header` on, then in place turns the first off and `add_security_headers` on, no longer listing
  `remove_x-powered-by_header`, which must stay on: the PATCH's merge by ID that the fake (`fakeSettingsStore`)
  assumes. The all-at-once deletion spec deletes it and checks that every transform is off again; the suite's cleanup
  turns them off if the run turned them on and stopped before that. A zone's managed transforms carry no version, so
  the e2e cannot see an idle write: the shared envtest specs check that an idle reconcile writes nothing. Passed on
  2026-10-23 (run `3f5c86`, 23 of 27 with the dev token; the 4 skipped are the chart's smoke specs, which a manager
  outside the cluster skips). The transform no longer listed stayed on, so Cloudflare's PATCH merges by ID, as the
  fake assumes, and the reset turned all six off. The manager logged two writes of the managed transforms (the create
  and the change), one reset, and no refusal
- kflare-gen needed no new mapping, but the kind's mutation check found two gaps in the generated specs, which three
  mutations survived: a keyed list compared without its key, and an `unconfigured` value left out. kflare-gen now
  closes them for every settings kind (see [generator/CLAUDE.md](../generator/CLAUDE.md)): the fake writes keyed lists
  as the table says Cloudflare does, the unconfigured zone holds an item no spec lists in each keyed list, a shared
  spec checks that the kind keys its lists alike, and another checks each unconfigured value. The kind also turned up
  a spec that predated settings kinds: `SetupGeneratedReconcilers`' expected the rule kinds and the account's
  dependents; it now expects every account and Zone dependent
- Mutations: 45 of the generated kind's calls (read, write and reset, each in its zone), its desired settings,
  keyed lists and unconfigured values, the conversion of each list and field, the registration (the controller, the
  Zone's dependents), the API type's accessors, JSON names and scheme registration, and the CRD's refusals (empty or
  missing IDs, missing `enabled`, an ID listed twice), all caught; 24 of kflare-gen's new code, all caught
- Not verified live: what Cloudflare does with an ID the zone does not have. deploy.md's permissions were checked on
  their own before v0.2.0 (see Token permissions)


#### Branches: Rules products (`TransformRule`, `RedirectRule`, `CacheRule`, `ConfigurationRule`, `OriginRule`)
**Depend on:** the code generator's rule kinds (`feat/generator-rule-kinds`); one branch and PR each
**Status:** ✅ `TransformRule` ✅, generated in `feat/generator-rule-kinds` (the code generator's PR 8); `RedirectRule` ✅
(`feat/redirect-rule-controller`); `CacheRule` ✅ (`feat/cache-rule-controller`); `ConfigurationRule` ✅
(`feat/configuration-rule-controller`); `OriginRule` ✅ (`feat/origin-rule-controller`)

Each is the foundation plus typed action parameters. Together they replace Page Rules. They are generated (decided
2026-10-13): each is a `generator.yaml` entry plus hooks for what Cloudflare's schema leaves out (see
[Code generator](../generator/CLAUDE.md#code-generator-kflare-gen)).

| CRD | Phase | Action | Free | Dev token |
|-----|-------|--------|------|-----------|
| `TransformRule` (`spec.type`, immutable: `urlRewrite`, `requestHeaders` or `responseHeaders`) ✅ | `http_request_transform`, `http_request_late_transform`, `http_response_headers_transform` | `rewrite` | 10 rules across the three phases ✔, no regex ✔ | ✅ |
| `RedirectRule` (single redirects) ✅ | `http_request_dynamic_redirect` | `redirect` | 10 rules ✔, no regex ✔ | ✅ since 2026-10-15 |
| `CacheRule` ✅ | `http_request_cache_settings` | `set_cache_settings` | 10 rules ✔ | ✅ since 2026-10-15 |
| `ConfigurationRule` ✅ | `http_config_settings` | `set_config` | 10 rules ✔ | ✅ since 2026-10-15 |
| `OriginRule` 🚧 | `http_request_origin` | `route` | 10 rules (per the docs, see below), destination port only ✔ | ✅ since 2026-10-15 |

- Spec fields are v7's typed action parameters (`rulesets.RewriteRuleParam`, `RedirectRuleParam`,
  `SetCacheSettingsRuleParam`, `SetConfigRuleParam`, `RouteRuleParam`), never raw JSON. Fields are opt-out (decided
  2026-10-13), so a large surface such as cache settings starts with an `ignore` list for what has not been checked,
  which shrinks as fields are verified
- Free's 10 transform rules are counted across the three phases (verified 2026-10-15, see below)
- Not planned yet: Bulk Redirects (the account-level `http_request_redirect` phase plus redirect lists; Free: 15 rules,
  5 lists, 10,000 URL redirects)

**TransformRule design notes (verified live on `kflare.dev`, 2026-10-15, with dry runs and `kflare-probe-*` rules,
each deleted afterwards):**
- Its spec: `type` (immutable) picks the phase; `uri` (a `path` and/or a `query`, each a static `value` or an
  `expression`) with `urlRewrite`; `headers` (a list keyed by `name`: `operation` `set`, `add` or `remove`, with a
  `value` or an `expression`) with the header types. kflare-gen builds it from v7's `rulesets.RewriteRuleParam` and the
  rule's OpenAPI schema: a header's five variants become one struct whose CEL rule matches the live API, the path and
  the query each take exactly one of value and expression, and the URI at least one of them. The entry adds what the
  schema leaves out, each refusal checked live: `uri` only with `urlRewrite` (URI rewrites in the header phases:
  20088), `headers` only with the header types (header rewrites in `http_request_transform`: 20086), `add` only on
  response headers (20087), and a path value starting with a slash (20089)
- Refused (400): a header with both a value and an expression, or with neither for `set` and `add`, or with either
  for `remove` (20087); an empty header value (20087); an operation other than set, add and remove (20087); a header
  name with a space (20087); `set` on `host` (20087); a path or query with both a value and an expression, or neither
  (20089); a URI with neither path nor query (20089); no action parameters, or an empty headers map (20084); `origin`
  in the URI ("cannot set 'origin' in this kind"); `regex_replace` on Free ("not entitled", a Business plan). Accepted:
  upper-case header names, removing `cf-connecting-ip`, an empty query value (it removes the query string)
- Limits, left out of the CRD since a plan may raise them: 30 header rewrites in a rule (31: 20087, "rule exceeds the
  maximum number of header rewrites"), a static header value or an expression of up to 4,096 characters
- Free allows 10 transform rules across the three phases: with one rule in each header phase, the URL rewrite phase
  took 8 ("11 out of 10", 50001)
- Stored as sent: header names keep their case, an empty query value stays `{"value": ""}`, and nothing is added to the
  URI. So the raw-JSON comparison holds: the e2e's idle reconciles write nothing in any of the three phases
- `logging` (20018), `ratelimit` (20131) and `exposed_credential_check` (20189) are refused in the transform phases;
  the entry ignores them
- deploy.md documents Zone · Transform Rules · Edit (the API's Zone Transform Rules Write), which is enough on its own
  (verified 2026-10-15, see Token permissions)

**RedirectRule design notes (verified live on `kflare.dev`, 2026-10-15, with dry runs, `kflare-probe-*` rules and a
proxied `kflare-probe-*` hostname, each deleted afterwards):**
- Its spec: `fromValue`, required, with `targetURL` (a static `value` or an `expression`, exactly one), `statusCode`
  (301, 302, 303, 307 or 308) and `preserveQueryString`. kflare-gen builds it from v7's `rulesets.RedirectRuleParam`:
  the schema's action parameters take exactly one of `from_value` and `from_list`, and with `from_list` ignored,
  `from_value` is required. `statusCode` is an integer enum, the first kflare-gen maps
- Refused (400): a target with both a value and an expression, or neither, or an empty expression (20083, "target_url
  should be either value or expression"); a status code outside the five (20083); no action parameters (20078);
  `from_list`, which Bulk Redirects use in the account-level `http_request_redirect` phase (20080); `regex_replace` in
  the target and `matches` in the rule's expression on Free ("not entitled", a Business plan). Accepted: any static
  value, even a relative URL, one without a scheme, one with a space or an empty one (the CRD keeps the schema's
  minimum length of 1); `wildcard_replace` in the target and the `wildcard` operator in the expression on Free
- Traffic through the edge: a rule without a status code redirects with 301 and drops the query string;
  `preserveQueryString` keeps it; an expression target is computed per request. A new rule took a few seconds to
  reach every edge server, so a first request may miss it
- Free allows 10 single redirects (11: 50001)
- Stored as sent: an omitted status code stays omitted, `preserve_query_string: false` stays false, and only `enabled`
  defaults (to true, which the comparison already counts). So the e2e's idle reconcile writes nothing (run `5f9daf`,
  17 of 17)
- `logging` (20018), `ratelimit` (20131) and `exposed_credential_check` (20189) are refused in the phase; the entry
  ignores them, with `from_list`
- deploy.md documents Zone · Single Redirect · Edit (the API's Dynamic URL Redirects Write), which is enough on its own
  (verified 2026-10-17, see Token permissions)
- kflare-gen gained, for it: integer enums (`+kubebuilder:validation:Enum=301;302;...`, an invalid case one past the
  highest value), and an object left with one field after the entry's ignores (no count rule; a minimum makes the field
  required). For every rule kind, the generated specs gained a refusal of each required field left out (`Required
  value`), a change for each object that takes exactly one of its fields that sets each other field in its place
  (so `targetURL.value`, which no sample sets, is written and refused like `targetURL.expression`), refusals drawn from
  the changes as well as the samples, and full specs that give an enum a value no sample uses. A mutation check (27
  mutations of the generated conversion, the CRD and the new generator paths) caught all but a conversion that always
  sent 301, the sample's status code; the last change closed it

**CacheRule design notes (verified live on `kflare.dev`, 2026-10-16, with dry runs and `kflare-probe-*` rules, each
deleted afterwards):**
- Its spec: what v7's `rulesets.SetCacheSettingsRuleParam` holds, but for what Free zones cannot use and what the phase
  refuses: `cache`; `browserTTL` and `edgeTTL` (a `mode`, and a `default` with `override_origin`); `edgeTTL`'s
  `statusCodeTTL` (a `value` for a `statusCode` or a `statusCodeRange`); `cacheKey` (`cacheByDeviceType`,
  `cacheDeceptionArmor`, `ignoreQueryStringsOrder`, and `customKey.queryString.exclude.all`); `cacheReserve`;
  `originCacheControl`; `originErrorPagePassthru`; `respectStrongEtags`; `serveStale`; `additionalCacheablePorts`;
  `originRangeRequests`; and `vary` (a `default`, and `headers`, a list keyed by lower-case `name`)
- Refused (400), each a CEL rule or a pattern in the CRD: a TTL `default` without `override_origin`, or none with it
  (20107 edge, 20108 browser); a status code range whose `from` is not below its `to` (20110); a Cache Reserve
  `minimumFileSize` without `eligible` (20197); vary `headers` without a `default`, a header name with a capital,
  `languages` on another header than `accept-language` and `mediaTypes` on another than `accept` (20289). From the
  schema: status codes outside 100–999, a code and a range together or neither, an empty range (20110), no action
  parameters (20106). Left to Cloudflare: overlapping ranges and a code listed twice (20110, "status_code ranges should
  not overlap")
- Free: of a custom cache key only `query_string.exclude.all: true` ("not entitled to use the custom cache key
  override" for its cookie, header, host, user, include and exclude list parts, and for an empty one); no extended
  `read_timeout` and no `shared_dictionary` ("not entitled"). Refused in the phase on any plan: `strip_etags`,
  `strip_last_modified` and `strip_set_cookie` (20250). The entry ignores them all, with their reasons. Accepted and
  stored on Free though Cloudflare documents them for Enterprise or an add-on: `origin_cache_control`,
  `additional_cacheable_ports` and `cache_reserve`; their docs say so
- Free allows 10 cache rules (11: 50001)
- Stored as sent, a rule with every accepted setting among the probes: lists keep their order (kflare sorts the ports,
  a set), an empty `serve_stale` stays `{}`, and only `enabled` defaults. The e2e's idle reconcile writes nothing (run
  `fd8de2`)
- `logging` (20018), `ratelimit` (20131) and `exposed_credential_check` (20189) are refused in the phase; the entry
  ignores them
- deploy.md documents Zone · Cache Rules · Edit (the label Cloudflare's documentation gives the API's Cache Settings
  Write), which is enough on its own (verified 2026-10-17, see Token permissions)
- kflare-gen gained, for it: CEL rules on an object of the spec (`fields.<path>.rules`, on the object at the path or on
  an item of a list or map, `<path>[]`); a map key's validation (`keyValidation`); `tests`, specs the generated specs
  write and edit besides the samples, for fields a sample should not set, which are not published; boolean enums
  (`exclude.all` takes only `true`); and CEL evaluation in kflare-gen itself, with cel-go: samples and tests must hold
  every rule, a change or swap that breaks one is left out (as flipping `cacheReserve.eligible` would), and every
  invalid spec must break the rule it is for. Count rules are now sums (`(has(self.a) ? 1 : 0) + ...`): the API server
  refused the CRD when they filtered a list, on the items of `statusCodeTTL`, a list without `maxItems`, as over its
  CEL cost budget. A mutation check (40 mutations of the generated conversion, phase and action, the CRD's rules and
  markers, and the new generator paths) caught all but two: one did not compile as written and was caught once
  rewritten, and disabling the check that a generated breach breaks its rule survived until a unit test gave a rule
  that no breach can break


**ConfigurationRule design notes (verified live on `kflare.dev`, 2026-10-17, with dry runs and `kflare-probe-*` rules,
each deleted afterwards):**
- Its spec: the settings v7's `rulesets.SetConfigRuleParam` holds, flat: switches (`automaticHTTPSRewrites`, `bic`,
  `contentConverter`, `emailObfuscation`, `fonts`, `hotlinkProtection`, `opportunisticEncryption`,
  `redirectsForAITraining`, `rocketLoader`, `serverSideExcludes`, `webmcpEnabled`), switches that take only `true`
  (`disableRUM`, `disableZaraz`: false is refused, 20114, and a boolean enum in the CRD), levels (`polish`,
  `requestBodyBuffering`, `responseBodyBuffering`, `securityLevel`, `ssl`) and `webmcpPacks`, a set of up to 100 pack
  names. No setting depends on another, so the entry gives no CEL rules
- Free refuses some values, which the CRD allows, for plans that take them ("not entitled"): `contentConverter`,
  `redirectsForAITraining` and `mirage` turned on, `polish` other than `off`, `securityLevel: off` and
  `ssl: origin_pull`. The field docs say so. Refused on any plan: `sxg` either way (20259, deprecated) and
  `disable_apps` (20251, deprecated; false: 20114). Free cannot set `disable_pay_per_crawl` at all. The entry ignores
  those three, `mirage` (deprecated) and `autominify` (Cloudflare retired Auto Minify in August 2024, though the API
  still accepts it)
- The OpenAPI spec says non-empty `webmcp_packs` need a WebMCP entitlement, yet Free stored two packs as sent. An empty
  list turns every pack off and an unset one leaves them alone; kflare sends the field only when it lists a pack
- Free allows 10 configuration rules (11: 50001); no action parameters: 20113
- Stored as sent, a rule with every setting Free takes among the probes: values as sent, the packs in the order sent
  (kflare sorts them, a set), and only `enabled` defaulted. The e2e's idle reconcile writes nothing (run `e9994a`)
- `logging` (20018), `ratelimit` (20131) and `exposed_credential_check` (20189) are refused in the phase; the entry
  ignores them
- deploy.md documents Zone · Config Rules · Edit (the label Cloudflare's documentation gives the API's Config Settings
  Write), which is enough on its own (verified 2026-10-19, see Token permissions)
- It needed nothing new from kflare-gen: the boolean enums and tests CacheRule brought were enough. kflare-gen gained a
  refusal one item over each list's or map's maximum (`Too many`), for `webmcpPacks` and, untested before, CacheRule's
  vary headers, media types and languages. A mutation check (21 mutations of the conversion, the CRD and the new case)
  caught all but a conversion that does not sort the packs, as the test listed them sorted; it lists them unsorted now

**OriginRule design notes (verified live on `kflare.dev`, 2026-10-18, with dry runs and ten `kflare-probe-*` rules,
deleted afterwards with their entry point):**
- Its spec: `origin.port`, the destination port (1 to 65535), required, as is `origin`. v7's `rulesets.RouteRuleParam`
  also holds `host_header`, `origin.host` (a DNS record to resolve) and `sni`, which Free zones cannot use. With those
  ignored, `origin` keeps one field, so kflare-gen makes it and the port required, as with RedirectRule's `fromValue`.
  The entry gives no CEL rules
- Free: the destination port only. `host_header`, `origin.host` and `sni` are refused ("not entitled to use the
  HostHeader override", "the Origin Host override", "SNI override"), as Cloudflare's documentation says: Enterprise
  plans only. The entry ignores them, with that reason
- Refused (400): port 0 (20093) and 65536 ("invalid JSON"), both outside the CRD's range; an empty `origin` (20092,
  "either host or port should not be empty"); no action parameters, or empty ones (20090); an empty host header (20091)
  or SNI value (20094). Accepted: ports 1, 8443 and 65535
- Stored as sent: the ten probe rules, one disabled and with a description, came back as sent, with only `enabled`
  defaulted (to true)
- Quota, not settled: Cloudflare's documentation gives Free 10 origin rules, yet with ten probe rules stored (one
  disabled), a dry run of an 11th single-rule POST was accepted. Writing an 11th rule for real was not done (the
  session's permission check stopped it; the probe rules were deleted). Hypotheses, most likely first: a dry run of a
  single-rule POST does not count the quota; a disabled rule does not count; the Free quota is above 10
- `logging` (20018), `ratelimit` (20131) and `exposed_credential_check` (20189) are refused in the phase; the entry
  ignores them
- deploy.md documents Zone · Origin Rules · Edit (the label Cloudflare's origin rules API documentation gives the API's
  Origin Write), which is enough on its own (verified 2026-10-19, see Token permissions)
- Live e2e spec: one rule on port 8443, checked as the JSON Cloudflare stores, moved to port 8080 in place, then left
  alone by an idle reconcile. Passed on 2026-10-19 (run `2748f8`, 20 of 20, and run `e79693` with deploy.md's
  permissions)
- It needed nothing new from kflare-gen's mapping. Its docs did need "an" before a name that starts with a vowel ("an
  OriginRule", and for the test IP list kind "an IP list"): kflare-gen's `article` now picks it wherever a generated
  doc names a kind or noun, tested by generating the test and committed rule kinds, and a CRUD controller, under names
  that start with a vowel. A mutation check (`hack/mutate.sh`, 31 mutations of the generated conversion, phase, action
  and kind name, the registration, the CRD's bounds and required fields, and kflare-gen's articles) first left three
  alive: a wrong kind name (the rule kind tables were named after it, so a focus on the kind ran nothing, and their
  expected refs came from it), the kind missing from `SetupGeneratedReconcilers` (its spec only checked that setup
  succeeded), and the ID label's article. The tables are now named after the API type, which the kind's name must
  match; the setup spec checks that every generated kind's controller is registered, by name; and the vowel test
  covers a CRUD kind's types. All 31 are caught

#### Branch: `feat/d1-database-controller`
**Depends on:** the code generator's PR 5 (`feat/generator-controllers`)
**Status:** ✅ Complete

A D1 database in an account: the code generator's PR 6, its first new kind generated end to end. Picked over a
Turnstile widget on 2026-10-15: D1 is free (10 databases on Workers Free, 500 MB each, 5 GB in the account), the dev
token already holds D1 Write, and it fits the shared CRUD reconciler with one change (see the generator's notes).

- [x] Its entry in `generator/generator.yaml`: adopted by `name`; docs for `name`, the location hint and `version`;
  `read_replication`, `created_at`, `file_size` and `num_tables` ignored, with reasons. kflare-gen writes the rest:
  `spec.accountRef`, `name` (the spec's pattern), `jurisdiction` (`eu`, `fedramp` or `us`) and `primaryLocationHint`
  (`wnam`, `enam`, `weur`, `eeur`, `apac` or `oc`), all immutable; `status.cloudflareMetadata.databaseID` and
  `version`; the claim label `kflare.dev/d1-database-id` (`NameConflict` otherwise); the controller, its envtest
  table and the sample. Short name `cfd1`
- [x] Live e2e spec: a D1Database in the EU jurisdiction, checked through v7's D1 service; deleted with the retain
  policy, which keeps the database; a new D1Database with the same name adopts it, found through the list; the
  all-at-once deletion deletes it
- [x] deploy.md (permission row, kinds table, uninstall, `NameConflict`), README, ARCHITECTURE.md

**D1Database design notes (verified live on the dev account, 2026-10-15, with `kflare-probe-*` databases, each deleted
afterwards):**
- Names are unique in the account (a duplicate → 400, code 7502), so adoption by name is sound. The spec's pattern,
  `^[a-zA-Z0-9][a-zA-Z0-9_-]*$`, is what Cloudflare enforces (a dot, a leading hyphen or an empty name → 400, 7400);
  capitals, underscores and 100 characters are accepted, and the spec gives no maximum, so the CRD gives none
- An unknown or malformed database ID → 404, code 7404, for GET and DELETE. DELETE answers 200 with no result
- The list takes `per_page` up to 10,000 (10,001 → 400, 7400), the spec's maximum, and returns an empty page past the
  end. It shows name, ID, creation time, version, jurisdiction, table count and size, not the read replication
- A new database reports `version: production`, `read_replication: {mode: disabled}`, 8,192 bytes and 0 tables.
  `jurisdiction: eu` was accepted and reported by GET and the list; a location hint (`weur`) was accepted, and
  Cloudflare reports only where the database runs (`running_in_region`, which v7's type lacks). Values outside either
  enum → 400, 7400 (the error lists the location hints in capitals; lowercase ones are accepted)
- No API renames a database or moves it: Update and Edit take only `read_replication`
- The e2e spec and its cleanup touch only databases named `kflare-e2e-<run>`, so the test account's other databases
  are safe
- Needs the account-level **D1: Edit** permission (the API's D1 Write), which is enough on its own (verified
  2026-10-15, see Token permissions)


#### Branch: `feat/generate-remaining-kinds`

IPList, Tunnel, DNSRecord, Zone, TunnelConfiguration and WorkerScript move onto kflare-gen and the shared CRUD
reconciler in one pull request, at the user's request on 2026-10-30. What was decided, and what was checked live:

- **A remotely-managed tunnel takes no secret.** Created with `config_src: cloudflare` and no `tunnel_secret`,
  `GET .../cfd_tunnel/<id>/token` still returned a token of 240 characters (probe tunnel, 2026-10-30, deleted). The
  hand-written Tunnel sent 32 random bytes that nothing read; the entry ignores the field
- **Tunnels rename in place.** `PATCH .../cfd_tunnel/<id>` with a `name` renamed the probe tunnel, and a list by the new
  name found it. v0.119's update left the ID out of the path, so `spec.name` was immutable until now
- **A deleted tunnel stays readable.** `GET` after `DELETE` succeeded, with `deleted_at` set: Tunnel's `gone` hook
- **Hooked kinds, Claude's call.** Zone, TunnelConfiguration and WorkerScript keep their fields and v0.119 calls, on the
  shared loop. v7's `ZoneNewParams` has no `jump_start` and its `Zone` no `plan_pending`, and the dev token cannot
  create a zone, so a move of Zone's create could not be checked. Moving a Worker's upload means a multipart request
  whose bindings v7 types as a union of about 30 variants. Each of them is a later change of its own, with a live run
- **Two Zones may name one zone.** The first live run failed on it: Zone had gained claims with the shared loop, and the
  example's Zone reported `NameConflict` against the suite's. ARCHITECTURE.md supports that setup, so a Zone's label
  records its zone without blocking another (`sharedObjects`)
- **What stays as it was.** Zone's plan, a set's records, a Tunnel's token Secret and the oldest TunnelConfiguration
  owning a tunnel's rules kept their rules, in hooks. CRDs differ only in descriptions, Tunnel's `name` no longer being
  immutable, and WorkerScript's `status.cloudflareMetadata.scriptName`

#### Launch readiness, done items

The finished items of CLAUDE.md's [Launch readiness](../CLAUDE.md#launch-readiness).

##### L2. Adoption on opt-in, ACK style (`feat/adoption-policy`)
**Status:** ✅ Built 2026-10-19 in `feat/adoption-policy`, as designed below with adopted objects kept on deletion (the
user's choice); see "As built" at the end of this section.

Until then a `Zone` applied for an existing domain adopted the zone, and deleting the `Zone` deleted it from Cloudflare
with every record in it. The same held for an adopted DNS record, tunnel, Worker route, IP list, R2 bucket, KV
namespace or D1 database (the last two with their data). `WorkerScript` replaced a Worker with the same name on its
first upload and deleted it with the resource. Changing this before v0.1.0 changes no released behavior.

Proposed design (the user preferred it on 2026-10-19 to retaining implicitly adopted objects): ACK's adoption
annotations under kflare's prefix. kflare's `deletion-policy` annotation already has ACK's shape; ARCHITECTURE.md
credits ACK for both. Checked 2026-10-19: ACK documents `services.k8s.aws/adoption-policy` (`adopt`,
`adopt-or-create`), `services.k8s.aws/adoption-fields`, an `adopted` annotation its controller sets, and deletes
adopted resources unless `retain`.
- `kflare.dev/adoption-policy`, set by the user:
  - unset: create only. An existing object with the resource's key (a zone, a name, a title, a pattern) that kflare
    did not create is left untouched, and the resource reports `Ready=False`, reason `AlreadyExists`, naming it
  - `adopt`: adopt the existing object; never create one (`NotFound` while there is none)
  - `adopt-or-create`: today's behavior
- `kflare.dev/adopted: "true"`, set by kflare when it adopts, in the same patch as the claim label where the kind has
  one (`ownership.go`); Zone and Tunnel, which have none, patch it before writing status
- Adopted objects are kept on deletion unless `kflare.dev/deletion-policy: delete` is set: a deliberate difference
  from ACK, since deleting something kflare did not make is the one step that cannot be undone. `delete` becomes a
  value kflare checks (`DeletionPolicyDelete`), as convention 6 documents. Alternative: delete them by default, as ACK
  does (the user decides)
- Recovery: a reconcile that created an object but lost its claim or status write must still find it as its own, not
  report `AlreadyExists`. kflare records its intent before the create call (for example a `kflare.dev/creating`
  annotation holding the key) and treats an object with that key as created by the resource
- Per kind: `WorkerScript` checks, with the `ListWorkers` call it already makes, whether a Worker with its name exists
  before its first upload. `TunnelConfiguration` counts a tunnel whose ingress holds more than the catch-all rule as
  existing. `DNSRecord` counts an identical record as existing, while a record of the same name and type with other
  content stays a separate member of a record set. Rule kinds are unchanged: they never adopt
- `kflare.dev/adoption-fields` (ACK's adoption by identifier) is left out: every adopting kind has a natural key.
  Revisit for kinds whose names are not unique (Tunnel, Turnstile widgets). The planned `AdoptedResource` CRD (Phase 4)
  may no longer be needed; decide then

- [x] Unit and envtest specs for every adopting kind and each policy: unset with an existing object reports
  `AlreadyExists` and writes nothing; `adopt` without one reports `NotFound`; an adopted object is annotated and kept
  on deletion, and deleted with `delete`; a created object is deleted as before; the recovery case is not refused
- [x] Live e2e: the Zone and D1Database adoption specs set the policy; an existing object without it is refused and
  left as it was; an adopted object outlives its resource. Run `9781eb`, 20 of 20 (2026-10-19). The Zone keeps its
  retain policy, so a bug cannot delete the test zone; the D1Database shows the adopted object outliving its resource
- [x] A mutation run with every mutation caught (`hack/mutate.sh`): 79 mutations of `RetainOnDelete`, `adoption.go`,
  `ownership.go` and every adopting kind's policy checks, marks and recovery. The first run left five alive, each
  closed by a spec (a recreated tunnel's configuration, a WorkerScript once its mark is gone, IPList's and
  WorkerRoute's recovery by claim) or by removing what it showed redundant (DNSRecord's early policy check)
- [x] ARCHITECTURE.md (Adoption and ownership, Deletion, Known limitations), deploy.md, README's "Import existing
  resources" line, the samples (the Zone sample adopts), conventions 4 and 6
- Done when: all of the above pass, and kflare touches no existing object unless a resource asks it to

**As built** (2026-10-19):
- `pkg/reconciler`: `AdoptionPolicyAnnotation` (`AdoptionPolicyAdopt`, `AdoptionPolicyAdoptOrCreate`),
  `AdoptedAnnotation`, `CreatingAnnotation` and `DeletionPolicyDelete`. `RetainOnDelete` keeps an adopted object unless
  the policy is `delete`, and a created one only with `retain` (any other value deletes it, as before)
- `internal/controller/adoption.go`: `adoptionPolicy` (an unknown value is `InvalidAdoptionPolicy`, never read as
  another), `mayAdopt` and `mayCreate` (the conditions, retried after a minute), `creating`, `markCreating` (sets the
  key and drops `adopted`: kflare creates this object), `markAdopted` and `clearCreating` for the kinds without a claim
  label, and `patchMetadata`, which patches only on a change. `claimID` (`ownership.go`) now also sets `adopted` and
  drops `creating` in the claim's patch; `ownsID` reads a resource's own claim
- The claim-label kinds (the shared CRUD reconciler, so KVNamespace and D1Database, plus DNSRecord, IPList, R2Bucket and
  WorkerRoute) count an existing object as their own when their claim or their `creating` key names it. Zone, Tunnel,
  TunnelConfiguration (key: the tunnel ID) and WorkerScript (key: its name) patch `adopted` before writing status, and
  drop `creating` on the reconcile that next finds the object through status. kflare-gen's CRUD kind docs say so
- WorkerScript now lists the account's Workers on every reconcile, its first included; its fake keeps the list as
  uploads change it. A Worker deleted outside kflare is uploaded again, unless the policy is `adopt`
- DNSRecord: `ownRecord` (claim, or `creating` = type and name plus matching content: members of a set share the key)
  and `adoptableRecord(lone)`, which takes the only record of the name and type only with a policy. Verified live
  2026-10-19: Cloudflare refuses a second CNAME for a name (400, 81053), so without a policy a DNSRecord whose name
  holds another CNAME reports `TerminalError`
- The rule kinds are unchanged

##### L3. A DNSRecord points at a Tunnel by reference (`feat/dnsrecord-tunnel-ref`)
**Status:** ✅ Merged 2026-10-21 (#60). Moved from CLAUDE.md verbatim.

The closest alternative, adyanth/cloudflare-operator, exposes a Service through a tunnel with one resource. kflare
should not need an ID copied from status to do the same: manifests must be writable before the first apply.
- [x] `spec.tunnelRef` on DNSRecord (a Tunnel in the same namespace): the record is a proxied CNAME to
  `<tunnelID>.cfargotunnel.com`. CEL: exactly one of `content`, `data` and `tunnelRef`; `tunnelRef` only with type
  `CNAME`. Check live what Cloudflare does with an unproxied one before deciding whether `proxied: false` is refused
- [x] Waits for the Tunnel to be ready, and watches Tunnels, so a recreated tunnel (a new ID) corrects the record
- [x] A Tunnel waits for the DNSRecords that reference it before it is deleted (`protection.go`), as it waits for its
  TunnelConfigurations; otherwise their hostnames would point at nothing
- [x] `examples/expose-a-service/`: a Zone adopting the reader's existing domain (`kflare.dev/adoption-policy: adopt`,
  L2), a Tunnel, a cloudflared Deployment reading `TUNNEL_TOKEN` from the Tunnel's Secret (image pinned), a
  TunnelConfiguration and a DNSRecord with `tunnelRef`. kflare does not run cloudflared; the example does
- [x] Unit and envtest specs, CEL cases, a live e2e spec (the record follows the Tunnel, including after the tunnel is
  recreated), a mutation run, ARCHITECTURE.md, deploy.md and the samples
- Done when: the example works on kind with a real account, and no sample asks for a copied ID. The live e2e suite now
  runs the example (decided 2026-10-20: in GitHub Actions, see below), so this holds once a live CI run passes

**As built** (2026-10-20, `feat/dnsrecord-tunnel-ref`):
- Checked live first on `kflare.dev` (`kflare-probe-*` objects, deleted afterwards): Cloudflare accepts an unproxied
  CNAME to `<id>.cfargotunnel.com`, but it resolves to that name, which has only a private IPv6 address
  (`fd10:aec2:5dae::`), and HTTPS cannot connect; so the CRD refuses `proxied: false` with a `tunnelRef`. A proxied
  CNAME to a tunnel ID that does not exist is accepted too (requests get 530), so the Tunnel check is kflare's. A
  proxied record's TTL is always 1, whatever a create or update sends: a proxied DNSRecord with another `ttl` used to
  write on every reconcile, so the CRD now refuses that on any proxied record. Cloudflare's docs: `cfargotunnel.com`
  only proxies traffic for DNS records in the tunnel's own account
- CEL on the spec: exactly one of `content`, `data` and `tunnelRef` (`data` became `type: object`, without which CEL
  cannot see it; `content` has a minimum length of 1); `tunnelRef` only on a CNAME; no `proxied: false` with it; `ttl`
  1 on any proxied record. A DNSRecord resolves its Tunnel after its Zone: `TunnelNotFound`, `TunnelNotReady`, and
  `AccountMismatch` when the Tunnel's CloudflareAccount is not the Zone's (by name, as WorkerRoute and WorkerScript
  compare). `tunnelRecordSpec` gives the spec kflare writes; ownership, adoption, create and drift take it in place of
  the resource's spec. The DNSRecord controller watches Tunnels (`recordsForTunnel`); the Tunnel controller watches the
  deletion of every kind in `tunnelDependents` in a loop, DNSRecords among them (`recordTunnels`)
- Specs: the waits, create, idle, a new tunnel ID followed, proxying restored, `AlreadyExists` and recovery by the
  tunnel's hostname, the CEL cases, and two specs that run a controller in a manager: a Tunnel's new ID reaches its
  record through the watch, and a Tunnel being deleted is released by its record's deletion. A mutation check
  (`hack/mutate.sh`, 24 mutations of the controller, the watches, the protection entry and the CRD's rules) caught all
- Live e2e: a DNSRecord with `tunnelRef` follows its tunnel after the tunnel is deleted outside kflare and recreated
  with a new ID (the same record, updated in place, and the TunnelConfiguration follows too); and the example, applied
  with the run's names, answers `https://kflare-e2e-<run>-hello.kflare.dev` through `cloudflared`, then deleting its
  namespace deletes the record and the tunnel and keeps the zone. The HTTP check resolves through the zone's
  authoritative name server: a resolver that asks before Cloudflare serves a new name caches the miss for 30 minutes
  (the zone's SOA minimum)
- The live e2e suite moved to GitHub Actions in this branch, at the user's request (2026-10-20). `e2e.yml` runs the
  deployment smoke test on pull requests, pushes to main and by hand (Kubernetes 1.34 and 1.37). The live suite is a
  separate job, on 1.37, that runs only when the owner, a member or a collaborator leaves a review comment starting
  with `/live-e2e` (`gh pr review <number> --comment -b /live-e2e`), or by hand with the `live` input (decided by the
  user the same day). A plain `/live-e2e` comment was the first design: CodeQL flagged it (critical,
  `actions/untrusted-checkout-toctou`), because a comment event runs privileged, from main's copy of the workflow, and
  carries no commit, so the head looked up afterwards could have moved since the comment. A review runs like a
  `pull_request` event instead: from the pull request's own copy of the workflow, on the merge commit as of the review,
  and without secrets for a fork (the job then fails, naming the missing secrets, instead of skipping every spec). It
  sits in a `concurrency` group that never runs two at once or cancels one halfway

## Rate limits

Why the rate limit works as it does (ARCHITECTURE.md's [Rate limits](../ARCHITECTURE.md#rate-limits)), from 11 read-only
calls with the dev token on 2026-10-31, on the Free plan, and measurements of both SDKs against a fake server. A real
429 was never provoked: it would have blocked the account's real Workers and dashboard for five minutes, so what a 429
carries, `Retry-After` included, comes from Cloudflare's docs.

- Public and unauthenticated calls carry no `Ratelimit` header. Authenticated ones do, but not all: `GET /zones`,
  `/accounts/{id}`, `/zones/{id}/dns_records`, `/zones/{id}/rulesets` and `/accounts/{id}/workers/scripts` did, and
  `/accounts/{id}/storage/kv/namespaces` did not
- Limits have names. `GET /zones` sent `"list_zones";r=1200;t=1` with `"list_zones";q=1201;w=300`; the others
  `"default";r=1199;t=1` with `"default";q=1200;w=300`. So one response may not show the limit that matters next
- `r` is not a count down a fixed window. Five `GET /zones` in a row, three back to back and two after pauses, all read
  1200, and `default` read 1199, 1199, 1197, 1199 over four calls. `t` read 1 throughout, near a full quota, so what it
  means low down is not known. This is why a hold lasts `t` seconds and the next response decides, whichever `t` is
- Whether `list_zones` draws on `default` as well is not known. A budget paces all of a token's calls as one, which is
  safe either way

Measured against a server answering every call with 429 and `Retry-After: 3` (cloudflare-go v0.119, v7.13):

| SDK | Requests | Took |
|---|---|---|
| v7 | 3 | 6 s: it sleeps for `Retry-After`, with no upper bound |
| v7, with `x-should-retry: false` on the response | 1 | none |
| v0 | 4 | 7 s: 1, 2 and 4 seconds, whatever `Retry-After` says |
| v7, the transport refusing with an error | 3 | 1.3 s; the error's type survives |
| v0, the transport refusing with an error | 4 | 7 s; the error's type survives |

Decided against:

- **Holding requests in the transport until a block ends**, as #73 proposed. A block outlasts the 2-minute request
  timeout, and the waiting request keeps its kind's only worker from every other account
- **A cache of Clients per token**, also from #73. It would keep what a Client carries between reconciles, and since
  #72 that is nothing: one HTTP client is shared already, and the transport reads the token from each request
- **A budget per Cloudflare account.** One user token can reach many accounts, as in an Organization, and Cloudflare
  counts it once
- **Learning a raised limit from `Ratelimit-Policy`.** The Free plan cannot show a raised quota, and a limit named for
  one operation says nothing certain about the rest, so that would raise the pace on a guess. `spec.rateLimit` says it
  outright
- **Relying on workqueue priorities to put changes first.** Items that wait out a rate limit leave the queue at once, so
  a change draws on the budget level with a restart's backlog. In a 5,000-item backlog it would wait about as long as
  the backlog does

## Allowed namespaces

Decisions behind `spec.allowedNamespaces` (#77), for one kflare serving several Cloudflare accounts in a shared or
management cluster:

- **Unset means every namespace.** Denying by default would be safer, and would break every install on upgrade, so the
  deploy guide recommends a selector instead
- **A selector without `from: Selector` is refused**, by a CEL rule on the type. `from` defaults to `All`, so a manifest
  with only a selector would otherwise look restricted and be open to every namespace
- **The check is in `resolveAccountToken`**, the one way to a token, not in each controller. It comes before the
  account's readiness and its Secret, so a refused namespace learns nothing of either
- **It fails closed.** A selector kflare cannot read, or a namespace it cannot read, allows nothing. Only an explicit
  `All`, or no field, opens an account up, so an unknown `from` from a stale client allows nothing either
- **Deleting from a namespace that lost access leaves the object**, as the issue's safe side. Keeping the finalizer
  until the namespace is allowed again would leave the namespace stuck terminating
- **No ValidatingAdmissionPolicy.** It cannot look up the account a resource names, so the controller's check would stay
  the one that enforces, and it would add a second thing to install and explain
- **Namespaces are watched only by kinds that name an account.** A zone's kind follows its Zone, and a relabelling that
  no account's selector concerns reconciles nothing, so it costs no Cloudflare calls

