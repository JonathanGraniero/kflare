# kflare — Cloudflare Kubernetes Operator

## Project Overview

kflare is a production-grade Kubernetes operator for Cloudflare, enabling fully declarative, GitOps-driven management of Cloudflare resources via Kubernetes CRDs.

The goal is to fill a gap in the ecosystem: existing community projects (adyanth/cloudflare-operator, containeroo/cloudflare-operator, replicatedhq/kubeflare) are narrowly scoped and inconsistently maintained. kflare targets production-grade quality with the intent to pursue official or CNCF-adjacent backing.

**Key differentiator:** Code-generate controllers from the [Cloudflare OpenAPI spec](https://github.com/cloudflare/api-schemas), driving broad API coverage with consistent patterns.

---

## Code Quality Standards

This is a **public, production-grade repository**. Every line of code must meet production standards — no hacks, no test shortcuts, no stubs.

- **No shortcut implementations:** If a function needs real logic, write real logic. Do not write placeholder implementations that satisfy tests without being correct.
- **Tests must reflect real behaviour:** Tests exist to verify correctness, not to inflate coverage metrics. Fake/mock objects must faithfully represent the contracts they replace.
- **Readability over cleverness:** Code will be read by contributors unfamiliar with the codebase. Prefer explicit, well-documented patterns.
- **Consistent patterns across controllers:** Each new controller must follow the same structure as existing ones. Extract shared logic into `pkg/` before duplicating it.
- **Test coverage target:** Aim for ≥95% meaningful coverage on all `pkg/` and `internal/controller/` packages. `cmd/` and generated files are excluded.

---

## Architecture & Key Decisions

### API Group
```
kflare.dev
```
`apiVersion: kflare.dev/v1alpha1`. The project owns `kflare.dev`; finalizers, annotations and labels use
the same prefix (`kflare.dev/finalizer`, `kflare.dev/deletion-policy`, ...). The group was
`cloudflare.cloudflare.k8s.io` until October 2026, which sat in the Kubernetes-reserved `*.k8s.io` space.

### Language & Frameworks
- **Go** (1.26+; required by the Kubernetes 0.37 libraries)
- **Kubernetes 1.34+** is the support floor. Client libraries track the newest release (k8s.io/* v0.37,
  controller-runtime v0.25); envtest runs on 1.34.1 (`ENVTEST_K8S_VERSION`) and 1.37.0 in CI, and the e2e
  workflow on kind node images 1.34 and 1.37. No compatibility code for older clusters
- **controller-runtime** for reconciler scaffolding
- **kubebuilder** for CRD/RBAC/webhook generation
- **operator-sdk** for OLM compatibility (later phases)

### Code Generation Strategy
- Parse the Cloudflare OpenAPI 3.0 spec (`api-schemas` repo) to generate:
  - CRD type definitions (`types_*.go`)
  - Basic reconciler skeletons
  - Cloudflare API client wrappers
- Hand-write reconciler logic where OpenAPI spec is insufficient

### Authentication Model
- Per-namespace `Secret` containing `CF_API_TOKEN`
- Cluster-scoped `CloudflareAccount` resource that references the secret
- Support for external-secrets operator integration

### Reconciler Pattern
Every controller follows:
```
Desired State (CRD spec) → Observed State (Cloudflare API) → Delta → Reconcile
```
With:
- `status.conditions` using standard Kubernetes condition types
- `status.cloudflareMetadata` (ID, zone ID, timestamps)
- Finalizer-based deletion protection: `kflare.dev/finalizer`
- `AdoptedResource` CRD for importing pre-existing Cloudflare resources
- `FieldExport` CRD for piping resource fields into ConfigMaps/Secrets

### Repo Structure
```
.
├── api/v1alpha1/          # CRD type definitions + generated deepcopy
├── cmd/main.go            # Manager entrypoint; registers every controller
├── internal/controller/   # One reconciler per CRD (+ credentials.go), envtest suites alongside
├── pkg/
│   ├── cloudflare/        # Client wrapper and error classification
│   └── reconciler/        # Conditions, finalizer and deletion-policy helpers
├── config/
│   ├── crd/               # Generated CRDs (make manifests) + kustomization
│   ├── rbac/              # Generated role.yaml + kustomize RBAC
│   ├── default/           # Kustomize base used by make deploy
│   └── samples/           # Example CRs
├── test/e2e/              # Deployment smoke test against kind (make test-e2e)
├── local/                 # kind cluster setup for running the controller with make run
├── helm/kflare/           # Helm chart; templates/crds and files/manager-rules.yaml generated (make helm)
├── hack/                  # sync-helm-chart.sh and the license header
├── docs/deploy.md         # Deploy guide (Helm install, accounts, upgrade, uninstall, troubleshooting)
├── ARCHITECTURE.md        # How the controllers behave: drift, adoption, errors, deletion, known limitations
├── CLAUDE.md              # This file
└── Makefile
```
Planned, not yet present: `generator/` (Phase 3), the Docusaurus site under `docs/` (Phase 4).

---

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
    Zone, Tunnel, WorkerScript, KVNamespace or R2Bucket references it, and the token Secret
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
  - `make test` (runs manifests, generate, fmt, vet, unit and envtest suites)
  - `git diff --exit-code` afterwards (ensures generated and formatted files are committed)
  - `GOTOOLCHAIN=local` with Go 1.26; a second job runs the envtest suites on the newest Kubernetes (1.37.0)
  - Tool versions (controller-gen, setup-envtest, golangci-lint v2, kustomize, kind) are pinned in the Makefile;
    setup-envtest follows the controller-runtime version
- [x] `.github/workflows/e2e.yml` (manual trigger only) runs `make test-e2e`:
  - Deployment smoke test: builds the image, deploys `config/default` to kind, checks the manager pod is
    ready with no restarts
  - Live Cloudflare specs (`test/e2e/cloudflare_test.go`, skipped without credentials): account validation,
    Tunnel + token Secret, TunnelConfiguration, KVNamespace, R2Bucket, WorkerScript (bound to both). With
    `CF_E2E_ZONE` also Zone adoption (always `retain`), a DNSRecord (including a check that an idle reconcile does not
    write to Cloudflare) and a WorkerRoute plus exclusion route. Then everything is deleted in one pass and Cloudflare is
    checked. Everything is named `kflare-e2e-<run>` and cleaned up even on failure
  - `KFLARE_E2E_MANAGER=external` uses an already running manager (e.g. `make run`) instead of deploying one
    (first green live run: 2026-10-05, this way, on kind-kflare-dev)
  - Needs repo secrets `CF_API_TOKEN`, `CF_ACCOUNT_ID` and variable `CF_E2E_ZONE` (`kflare.dev`, the project's
    own zone, is the test zone)
- [x] CI also runs `make lint` and `make docker-build` (the Dockerfile copies source directories explicitly)

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
- cloudflare-go v0.89 `UpdateTunnel` omits the tunnel ID from the request path, so renames are impossible;
  `spec.name` and `spec.accountRef` are immutable via CEL (`self == oldSelf`)
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
- `originRequest` timeouts are not exposed: cloudflare-go v0.89 `TunnelDuration` marshals float seconds but only
  unmarshals integers
- Each hostname still needs a proxied CNAME to `<tunnelID>.cfargotunnel.com` (use a `DNSRecord`)
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
**Status:** ✅ Complete

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
| `feat/r2-bucket-controller` | `R2Bucket` (+ WorkerScript `r2BucketRef`) | shared-client | ✅ |
| `feat/cloudflare-go-v0.119` | none: SDK bump | — | Next, if the [SDK decision](#sdk-decision) is accepted |
| `feat/rulesets-foundation` | none: Rulesets client calls and the shared rule sync | zone-controller | Live probes first |
| `feat/waf-custom-rule-controller` | `WAFCustomRule` (was `FirewallRule`) | rulesets-foundation | Free: 5 rules, no regex, no Log action, no custom block response |
| `feat/rate-limit-rule-controller` | `RateLimitRule` (was `RateLimit`) | rulesets-foundation | Free: 1 rule, 10 s period, counts by IP |
| `feat/ip-list-controller` | `IPList` | shared-client | Free: 1 list, 10,000 items, IP lists only |
| `feat/zero-trust-controllers` | `AccessGroup`, `AccessPolicy`, `AccessApplication` | shared-client | Blocked: Zero Trust is not enabled on the account (a free plan exists) |
| `feat/managed-ruleset-controller` | `ManagedRuleset` (was `WAFPackage`) | rulesets-foundation | Free: only the Cloudflare Free Managed Ruleset |
| `feat/managed-transform-controller` | `ManagedTransform` | zone-controller | Free: all six transforms |
| `feat/transform-rule-controller` | `TransformRule` | rulesets-foundation | Free: 10 rules, no regex |
| `feat/redirect-rule-controller` | `RedirectRule` | rulesets-foundation | Free: 10 rules, no regex |
| `feat/cache-rule-controller` | `CacheRule` | rulesets-foundation | Free: 10 rules |
| `feat/configuration-rule-controller` | `ConfigurationRule` | rulesets-foundation | Free: 10 rules |
| `feat/origin-rule-controller` | `OriginRule` | rulesets-foundation | Free: 10 rules, destination port override only |
| `feat/load-balancer-monitor-controller` | `LoadBalancerMonitor` | shared-client | ⏸ Deferred: needs Load Balancing add-on |
| `feat/load-balancer-controller` | `LoadBalancer` (+ pools) | load-balancer-monitor-controller | ⏸ Deferred: needs Load Balancing add-on |

Rows run top to bottom. The order decided on 2026-10-06 (WAF, then rate limiting, then Zero Trust) is kept. `IPList`
comes before Zero Trust because it is free and can be tested today, while Zero Trust waits on a dashboard step. When a
row is blocked, skip ahead: `IPList`, `ManagedTransform` and the five Rules rows do not depend on each other.

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
authorization, plan and quota checks as a real write but stores nothing. The token's zone policy has only Read for
these products, yet the WAF and Transform phases were accepted: its account-level **Account WAF Write**, **Account
Rulesets Write** and **Transform Rules Write** evidently cover zone rulesets too. The names below are the API's
permission group names; deploy.md uses the dashboard's labels, so look those up when each branch adds its row there.

| Resource | Permission to document (least privilege) | Dev token today |
|---|---|---|
| `WAFCustomRule`, `RateLimitRule`, `ManagedRuleset` | Zone WAF Write | ✅ dry run accepted |
| `TransformRule` | Zone Transform Rules Write | ✅ dry run accepted |
| `RedirectRule` | Dynamic URL Redirects Write | ❌ 403, "request is not authorized" |
| `CacheRule` | Cache Settings Write | ❌ 403 |
| `ConfigurationRule` | Config Settings Write | ❌ 403 |
| `OriginRule` | Origin Write | ❌ 403 |
| `ManagedTransform` | Managed headers Write | ❌ Read only (not a Rulesets endpoint, so no dry run) |
| `IPList` | Account Rule Lists Write (account) | ✅ |
| `AccessGroup`, `AccessPolicy`, `AccessApplication` | Access: Organizations, Identity Providers, and Groups Write; Access: Apps and Policies Write (account) | ✅, but Access is not enabled |

- Adding a permission to the dev token needs the user's OK first, as with Workers Routes Write on 2026-10-05
- Before deploy.md recommends a zone-level permission, check that it is enough on its own. The dev token's
  account-level permissions already cover these phases, so it cannot show that; a second, narrower token can (ask
  before creating one)

#### SDK decision
**Status:** Proposed 2026-10-09; confirm before `feat/rulesets-foundation`

kflare pins cloudflare-go **v0.89.0**, and its gaps keep costing features. Checked 2026-10-09:

| | v0.89.0 (pinned) | v0.119.0 (newest v0, 2026-09-25) | v7.12.0 (generated SDK, 2026-10-01) |
|---|---|---|---|
| Create / edit / delete one rule in a ruleset | none | delete only | all three |
| Reusable Access policies (`/access/policies`) | no: app-scoped policies only | yes | yes |
| R2 jurisdictions (`cf-r2-jurisdiction`) | no | no | yes |
| Tunnel rename (`UpdateTunnel` sends the tunnel ID) | no | no | yes |
| Cost to adopt | — | one compile error: `DNSRecord.ZoneID` was removed (take the zone ID from the Zone), plus test fixtures | every controller's interface and fakes, and `errors.go` (different error types) |

Proposed:
1. **Bump to v0.119 now**, in its own PR (`feat/cloudflare-go-v0.119`). It is small, and brings `DeleteRulesetRule` and
   reusable Access policies. Run the unit and live e2e suites: 30 minor versions can change behaviour without
   breaking the build
2. **Fill the remaining gaps with `API.Raw`** wrappers in `pkg/cloudflare`. For rulesets only the create and edit calls
   are missing: v0's `cf.RulesetRule` and `cf.Ruleset` types already model rules, and `Raw` returns the same typed
   errors as every other call
3. **Move to the generated SDK later, controller by controller** (Phase 4, or sooner if the v0 line stops getting
   releases). Each controller already sits behind its own narrow interface, so `pkg/cloudflare.Client` can embed both
   SDKs during the move, with `errors.go` classifying both error types until it is done

#### Deferred: paid plan, add-on or account setup

Everything here was skipped, or built without live verification, because the dev account (Free zone `kflare.dev`, no
add-ons besides R2) cannot exercise it. Before starting any item below, re-check the limits: Cloudflare changes them.
Last checked 2026-10-07; the Free-plan limits and Zero Trust again on 2026-10-09.

**Not built yet — needs something we don't have:**

| Item | Needs | Cost (2026-10) | What was verified | When we come back to it |
|------|-------|----------------|-------------------|-------------------------|
| `LoadBalancerMonitor`, `LoadBalancer` (+ pools) | Load Balancing add-on (account) | $5/month incl. 2 origins, +$5/origin/month, +$0.50 per 500K DNS queries | Live, 2026-10-06: every monitor create fails, `interval is not in range [1, 1]`. The token already has Load Balancing: Monitors and Pools Write | Subscribe, build the monitor then the load balancer; cancel after if it was only for testing |
| `HealthCheck` (standalone, zone-level Health Checks) | Pro plan or higher on the zone; zone-level Health Checks Write on the token | Pro plan ~$20+/month per zone | Docs: Free plan gets 0 checks (Pro 10, Business 50). Token has Health Checks Read only | Not in the Phase 3 table yet. Add a branch once a Pro zone exists; keep it separate from `LoadBalancerMonitor` |
| R2Bucket `storageClass` (a bucket's default storage class, `Standard` or `InfrequentAccess`) | Nothing to subscribe to, but R2's free tier covers Standard storage only, so testing Infrequent Access is billed usage | Usage-based; see R2 pricing | Not probed, to stay free. Every bucket on the account reports `Standard`. Also needs `Raw` calls: cloudflare-go v0.89 cannot send the `cf-r2-storage-class` header | Decide whether a few cents of test usage are acceptable, then add it as an optional, mutable field (create header + PATCH) |

**Built, but not verified live because it costs money:**

| Item | Why | What exists | When we come back to it |
|------|-----|-------------|-------------------------|
| Zone `spec.plan` | Changing a plan bills the account | Implemented (subscription POST/PUT, `plan_pending`), unit-tested against response shapes read from the live Free zone | Exercise once on a disposable zone (upgrade, check status, downgrade); expect a prorated charge |

**Worked around because the clean option is paid:**

| Item | Paid option | Current workaround | When we come back to it |
|------|-------------|--------------------|-------------------------|
| Ownership marker on the Cloudflare side (DNSRecord, and in general) | DNS record tags (Pro plan and up) | `kflare.dev/record-id` / `route-id` / `kv-namespace-id` / `r2-bucket-name` labels in the cluster; ARCHITECTURE.md lists "no ownership marker on the Cloudflare side" as a known limitation | With a Pro zone, tag records kflare creates; or a TXT ownership registry like external-dns (works on Free) |

**Free, but needs account setup first:**

| Item | Setup | Verified |
|------|-------|----------|
| Zero Trust (`AccessApplication`, `AccessPolicy`, `AccessGroup`) | Enable Zero Trust in the dashboard (pick a team name and the Free plan, up to 50 users). Not confirmed whether it asks for a payment method | Live, 2026-10-07 and 2026-10-09: the API returns `Access is not enabled`. The token already has the Access: Apps/Policies/Groups Write permissions |

**Free-plan limits to design around** (build for them; the e2e suite must stay within them). From Cloudflare's docs,
checked 2026-10-09; ✔ marks what a dry run on `kflare.dev` confirmed the same day:

| Item | Free | Pro | Business |
|------|------|-----|----------|
| WAF custom rules | 5 rules ✔, no regex ✔, no Log action ✔, no custom block response ✔, 1 zone-level custom ruleset | 20 rules | 100 rules, regex |
| Rate limiting rules | 1 rule ✔, period 10 s only ✔, counts by IP, no `managed_challenge` ✔, no throttling (`mitigation_timeout: 0`) ✔ | 2 rules, periods up to 1 min, mitigation up to 1 h | 5 rules, periods up to 10 min, mitigation up to 1 day |
| WAF managed rulesets | Cloudflare Free Managed Ruleset only ✔ | + Cloudflare Managed Ruleset, OWASP Core Ruleset, Exposed Credentials Check | same as Pro |
| Custom lists | 1 list, 10,000 items, IP lists only | 10 lists, 10,000 items in total | same as Pro |
| Transform, redirect, cache, configuration and origin rules | 10 rules each; no regex in transform or redirect rules | 25 each | 50 each; regex in transform and redirect rules |
| Origin rule overrides | destination port only (Host header, SNI and DNS record need Enterprise) | same | same |

Rate limiting also requires `cf.colo.id` among the characteristics (code 20155) and a mitigation timeout of 0, equal
to the period or longer (code 20156). The docs limit Free expressions to path and verified-bot fields, but a dry run
accepted `http.host`; `http.user_agent` was refused.

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
  `status.cloudflareMetadata.namespaceID`
- [x] `internal/controller/kvnamespace_controller.go`: list → match status ID → adopt by title → create; title drift
  corrected by rename; claims its namespace in `kflare.dev/kv-namespace-id` (`TitleConflict` otherwise)
- [x] WorkerScript binding `kvNamespaceRef` (same namespace): waits for the KVNamespace to be ready, hashes the
  resolved ID (a recreated namespace re-uploads the Worker), watches KVNamespaces
- [x] Deletion: a KVNamespace waits for the WorkerScripts bound to it; it counts as a user of its account.
  Deleting it deletes the data unless `retain`
- [x] Unit/envtest tests; live e2e spec (namespace + Worker binding checked via `ListWorkerBindings`)

**KVNamespace design notes (verified live):**
- `KVNamespaceAPI` interface: `CreateWorkersKVNamespace/ListWorkersKVNamespaces/UpdateWorkersKVNamespace/
  DeleteWorkersKVNamespace` (account-level `rc`); cloudflare-go v0.89 has no single-namespace read, so the
  controller lists (the SDK pages) and matches the ID
- Titles are unique per account (duplicate → 400, code 10014); unknown namespace ID → 404 (10013); rename (PUT)
  keeps the ID
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
- Duplicate create → 409 (10004); missing bucket → 404 (10006) for GET and DELETE; invalid name → 400 (10005);
  unknown location hint → 400 (10044, lists `wnam, enam, weur, eeur, apac, oc, auto`). GET returns `location`,
  `storage_class` and `jurisdiction`; LIST returns them as null
- Deleting a bucket that holds objects → 409 (10008)
- Cloudflare deletes a bucket a Worker is bound to; every later upload of that Worker then fails with 10085, and a
  Worker bound to a missing bucket cannot be uploaded at all. Hence the deletion ordering and the readiness wait
- Bindings resolve the bucket by name: a Worker kept working, without a re-upload, after its bucket was deleted and
  created again under the same name (while it was gone the Worker threw error 1101). The binding hash is therefore the
  bucket name only, unlike KV where a recreated namespace has a new ID
- Needs the account-level **Workers R2 Storage: Edit** permission

**Not supported yet:**
- Jurisdictions (`eu`, `fedramp`): cloudflare-go v0.89 cannot send `cf-r2-jurisdiction`, and its Worker R2 binding has
  no `jurisdiction` field. Needs `Raw` calls and a custom binding, or a newer SDK
- Default storage class: see Deferred (Infrequent Access testing is billed)
- Bucket configuration that Terraform models as separate resources: CORS, lifecycle rules, custom domains and the
  r2.dev public URL, event notifications, bucket locks, Sippy

#### Branch: `feat/rulesets-foundation`
**Depends on:** `feat/zone-controller` and the [SDK decision](#sdk-decision)
**Merges into:** main, before any rule CRD branch starts (no stacked PRs)
**Status:** 🔲 Not started

Shared machinery for every resource that is one rule in a zone's phase entry point ruleset. It has no CRD of its own:
it is unit-tested with fakes, then proven live by `WAFCustomRule`.

- [ ] Live probes first, on `kflare.dev` with `kflare-probe-*` refs, deleting everything afterwards (the dev token can
  already write `http_request_firewall_custom`):
  - Does a rule keep its `id` across a PATCH? The docs describe the ID as identifying "a given version of a rule",
    which is why kflare finds its rules by `ref`
  - What Cloudflare adds or defaults on a rule it returns (`version`, `last_updated`, `logging`, action parameter
    defaults), for the drift comparison
  - Creating the entry point when one already exists (two resources racing to create it): status and error code
  - Two concurrent POSTs to `/rules`: do both rules land?
  - PATCH replaces the whole rule (per the docs): confirm that an omitted field is reset
  - Deleting the last rule: does the entry point stay, empty?
- [ ] `pkg/cloudflare/rulesets.go`: `CreateRulesetRule` and `UpdateRulesetRule` through `API.Raw` (POST
  `/zones/{zone}/rulesets/{ruleset}/rules`, PATCH `.../rules/{rule}`), with an optional `position`. Each returns the
  updated `cf.Ruleset`. `DeleteRulesetRule` comes with v0.119 (otherwise `Raw` as well). Unit tests against `httptest`
- [ ] `internal/controller/rulesetrule.go` syncs one rule in any phase:
  1. Resolve the Zone (its zone ID) and the account's token, as DNSRecord does
  2. Get the phase entry point. On 404 (code 10003), create it: POST `/zones/{zone}/rulesets` with `kind: zone`, the
     phase and this rule. **Never PUT the entry point**: a PUT replaces every rule in it, including rules kflare
     does not manage. If the create loses a race, get the entry point again
  3. Find the rule by its `ref`. Missing → POST it; different → PATCH the whole rule; same → no write
  4. Record the ruleset ID, rule ID and rule version in status
  - Deletion: DELETE the rule found by `ref` unless `retain`. Skip the call when the Zone is gone, as DNSRecord does.
    The entry point stays, even when empty
- [ ] Ownership on the Cloudflare side through `ref`: `kflare-` + the first 32 hex characters of SHA-256 over
  `<kind>/<namespace>/<name>`. Refs are unique within a ruleset (duplicate: code 20023) and at most 128 bytes (code
  20166). A ref derived from the name survives cluster loss and backup restores, which a UID would not. kflare never
  touches a rule whose ref it did not derive, and does not adopt rules implicitly: rules have no natural key. Rules
  will therefore not share the "no ownership marker on the Cloudflare side" limitation
- [ ] Ordering: optional `spec.priority` (lower runs first; ties broken by namespace/name). kflare orders only the
  rules it manages in a zone and phase, across every kind that writes to that phase. When the reconciled rule is out
  of place among them, kflare moves that rule alone (PATCH with `position.before` or `position.after` a neighbouring
  kflare rule). Rules created outside kflare keep their place. If this proves fragile, fall back to append-only and
  document it
- [ ] Drift: compare only the fields kflare sets (expression, action, action parameters, rate limit, enabled,
  description), after normalizing what Cloudflare adds. An idle reconcile writes nothing; the e2e checks that the
  ruleset version stays the same
- [ ] Rule kinds join `zoneDependents`, so a Zone waits for its rules
- [ ] ARCHITECTURE.md: drift and adoption rows, a rules ownership subsection, the known-limitations update

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
**Status:** 🔲 Not started

- [ ] `api/v1alpha1/wafcustomrule_types.go`: `spec.zoneRef` (immutable), `expression`, `action` (`block`,
  `managed_challenge`, `js_challenge`, `challenge`, `skip`, `log`), skip parameters (the rest of the current ruleset,
  `phases` or `products`) allowed only with `action: skip` (CEL), optional `response` for `block` (a custom body; paid),
  `enabled` (default true), `description`, `priority`. `status.cloudflareMetadata`: zone ID, ruleset ID, rule ID, ref,
  version
- [ ] Controller: the foundation in the `http_request_firewall_custom` phase; watches Zones
- [ ] Unit tests for the spec → rule conversion (every action's parameters); envtest for the CEL rules
- [ ] Live e2e: a `block` rule for `/kflare-e2e-<run>`, checked through the entry point; an idle reconcile leaves the
  ruleset version alone; deletion removes the rule. It uses 1 of Free's 5 rules
- [ ] Sample, deploy.md permission row, ARCHITECTURE.md rows


#### Branch: `feat/rate-limit-rule-controller`
**Depends on:** `feat/rulesets-foundation`
**Status:** 🔲 Not started

- [ ] `api/v1alpha1/ratelimitrule_types.go`: `spec.zoneRef` (immutable), `expression`, `action` (default `block`;
  others need a paid plan), `characteristics` (default `[ip.src]`; kflare always adds `cf.colo.id`, which Cloudflare
  requires), `period` (seconds, default 10), `requestsPerPeriod`, optional `mitigationTimeout` (kflare sends the
  period when unset; CEL: 0 or at least the period), optional `countingExpression` and `requestsToOrigin`, `enabled`,
  `description`, `priority`
- [ ] Controller: the foundation in the `http_ratelimit` phase
- [ ] Live e2e: one rule for `/kflare-e2e-<run>`. Free allows exactly one, so the suite must not run twice at once,
  and `kflare.dev` must hold no other rate limiting rule


#### Branch: `feat/ip-list-controller`
**Depends on:** `feat/shared-client`
**Status:** 🔲 Not started

An account-level custom IP list, for expressions such as `ip.src in $office_ips` in WAF custom rules.

- [ ] `api/v1alpha1/iplist_types.go`: `spec.accountRef` and `spec.name` (immutable; check Cloudflare's naming rules
  live), `description`, `items[]` (`ip`: an IPv4 or IPv6 address or CIDR range; optional `comment`).
  `status.cloudflareMetadata`: list ID, item count
- [ ] Controller: by status ID → by name → create (`kind: ip`). Description drift through `UpdateList`, item drift
  through `ReplaceListItems` (an asynchronous bulk operation; check how the SDK waits for it). Claims its list in a
  `kflare.dev/ip-list-id` label, like KVNamespace
- [ ] Deletion: check live whether Cloudflare refuses to delete a list that a rule references. If it does, report
  `ListInUse` and retry every minute, like `BucketNotEmpty`
- [ ] v0.89 already has the Lists calls (`CreateList`, `GetList`, `UpdateList`, `DeleteList`, `ReplaceListItems`)
- [ ] Live e2e: one list (Free allows one; the account has none as of 2026-10-09), optionally referenced by the WAF e2e
  rule


#### Branch: `feat/zero-trust-controllers`
**Depends on:** `feat/shared-client`
**Blocked on:** enabling Zero Trust on the account, a dashboard step (still `Access is not enabled` on 2026-10-09)
**Status:** 🔲 Not started

- [ ] `AccessGroup` (include, exclude and require rules), `AccessPolicy` (a reusable, account-level policy that
  references groups) and `AccessApplication` (self-hosted: domains, session duration, and policies by reference with a
  precedence). One PR per kind if the branch grows large
- [ ] Reusable policies need v0.119 or later; v0.89 creates only app-scoped policies (see the SDK decision)
- [ ] Deletion order: an application before its policies, a policy before its groups, using `protection.go`. Check
  live what Cloudflare refuses
- [ ] Live e2e once Zero Trust is enabled (the free plan covers up to 50 users)


#### Branch: `feat/managed-ruleset-controller`
**Depends on:** `feat/rulesets-foundation`
**Status:** 🔲 Not started

Deploys a Cloudflare managed ruleset to a zone, with overrides, through an `execute` rule in the
`http_request_firewall_managed` phase.

- [ ] **Verify first:** Cloudflare runs the Free Managed Ruleset on Free zones without any entry point. Does creating an
  entry point in this phase, for example for a single exception, stop that default deployment? If it does, kflare must
  keep an explicit `execute` rule in place, or a user who adds an exception silently loses the ruleset
- [ ] Types: `spec.zoneRef` (immutable), the managed ruleset ID (Free: `77454fe2d30c4220b5701f6fdfb893ba`), overrides
  (the whole ruleset's enabled state or action; per rule and per category), `enabled`, `priority`. Check with dry runs
  which overrides Free accepts
- [ ] Exceptions (`skip` rules in this phase; a dry run accepts them on Free): a separate small kind or part of this
  one, decided in the branch. They must run before the `execute` rule, which the foundation's ordering has to
  guarantee across kinds


#### Branch: `feat/managed-transform-controller`
**Depends on:** `feat/zone-controller`
**Status:** 🔲 Not started

- [ ] `api/v1alpha1/managedtransform_types.go`: `spec.zoneRef` (immutable), `requestHeaders[]` and
  `responseHeaders[]`: the transforms to enable, by Cloudflare's ID. `kflare.dev` offers
  `add_client_certificate_headers`, `add_visitor_location_headers`, `remove_visitor_ip_headers` and
  `add_waf_credential_check_status_header` for requests, `remove_x-powered-by_header` and `add_security_headers` for
  responses (2026-10-09). Every transform not listed is disabled
- [ ] Controller: owns the zone's whole configuration, like TunnelConfiguration. The oldest ManagedTransform per zone
  owns it; the others report `AlreadyConfigured`. `ListZoneManagedHeaders` → compare → `UpdateZoneManagedHeaders`;
  surfaces Cloudflare's `has_conflict` / `conflicts_with`. Deletion disables every transform unless `retain`
- [ ] Needs Managed headers Write on the dev token (ask the user)


#### Branches: Rules products (`TransformRule`, `RedirectRule`, `CacheRule`, `ConfigurationRule`, `OriginRule`)
**Depend on:** `feat/rulesets-foundation`; one branch and PR each
**Status:** 🔲 Not started

Each is the foundation plus typed action parameters. Together they replace Page Rules.

| CRD | Phase | Action | Free | Dev token |
|-----|-------|--------|------|-----------|
| `TransformRule` (`spec.type`, immutable: `urlRewrite`, `requestHeaders` or `responseHeaders`) | `http_request_transform`, `http_request_late_transform`, `http_response_headers_transform` | `rewrite` | 10 rules, no regex | ✅ |
| `RedirectRule` (single redirects) | `http_request_dynamic_redirect` | `redirect` | 10 rules, no regex | needs Dynamic URL Redirects Write |
| `CacheRule` | `http_request_cache_settings` | `set_cache_settings` | 10 rules | needs Cache Settings Write |
| `ConfigurationRule` | `http_config_settings` | `set_config` | 10 rules | needs Config Settings Write |
| `OriginRule` | `http_request_origin` | `route` | 10 rules, destination port only | needs Origin Write |

- Spec fields mirror Cloudflare's action parameters, typed. Start with what Free and Pro can use and add the rest when
  needed, rather than passing raw JSON through
- Check whether Free's 10 transform rules are counted per phase or across the three
- These five share one shape, which makes them the natural first target for the OpenAPI generator, if it is still
  wanted (see below)
- Not planned yet: Bulk Redirects (the account-level `http_request_redirect` phase plus redirect lists; Free: 15 rules,
  5 lists, 10,000 URL redirects)


#### Release and CI
**Status:** 🔲 Open, alongside Phase 3

- [ ] Run the live e2e suite in GitHub Actions. The repo has no Actions secrets or variables yet (2026-10-09). Use a
  dedicated CI token holding only what the suite needs, not the dev token, which also holds Account API Tokens Write
  and Registrar Domains Admin. Set the secrets `CF_API_TOKEN` and `CF_ACCOUNT_ID`, and the variable
  `CF_E2E_ZONE=kflare.dev`
- [ ] Tag `v0.1.0`: `Chart.yaml` is already at 0.1.0 and no tag exists. The release workflow publishes the image and
  chart. Fix first: `docs/deploy.md` says "the six kflare CRDs", but there are nine
- [ ] `kflare.dev` expires on 2027-10-04 with auto-renew off (registered 2026-10-04). The e2e zone disappears if it
  lapses: decide on renewal before September 2027
- [ ] Open question: a Terraform `infra/` root for `kflare.dev` (zone, renewal, CI token, GitHub secrets)

**Also in Phase 3:**
- [ ] OpenAPI-to-CRD generator skeleton (`generator/` package) — parses the [Cloudflare OpenAPI spec](https://github.com/cloudflare/api-schemas) to generate CRD type definitions and reconciler skeletons; intended to accelerate the long tail of resources beyond what is hand-written in Phase 2
  - Rescope before starting: the generated SDK (v7) already produces client code from the same spec, so the
    generator would only need CRD types and controller skeletons. The five Rules CRDs are the natural first target

---

### Phase 4 — Ecosystem & Graduation
**Status:** 🔲 Not started

- [ ] Full `FieldExport` implementation (export any CRD field → ConfigMap/Secret)
- [ ] `AdoptedResource` full implementation (import existing CF resources into management)
- [x] Helm chart (`helm/kflare`) with deploy guide (`docs/deploy.md`); `.github/workflows/release.yml` publishes
  the image and chart to `ghcr.io` on a `vX.Y.Z` tag matching `Chart.yaml`
  - CRDs are templates (upgraded by `helm upgrade`, `helm.sh/resource-policy: keep`), generated from
    `config/crd/bases` with the manager ClusterRole rules by `make helm`; CI fails if they drift
  - `view`/`edit` aggregated ClusterRoles; CloudflareAccount is never in `edit` (it can point at any Secret)
  - Verified by installing on kind with a live token: account Ready, Secret protection finalizer, upgrade,
    uninstall keeping CRDs
- [ ] Chart listed on ArtifactHub
- [ ] Kustomize component overlays for common patterns (tunnel + DNS combo, zero-trust stack)
- [ ] Docusaurus documentation site
  - Getting Started
  - CRD Reference (auto-generated from kubebuilder markers)
  - GitOps patterns (FluxCD + ArgoCD examples)
  - Migration guide from existing community operators
- [ ] OperatorHub / OLM bundle (via operator-sdk)
- [ ] Engage Cloudflare developer relations for official backing
- [ ] CNCF landscape submission


---

## Operator Conventions

1. **Conditions** — every resource has a single `Ready` condition. Reasons: `Synced`/`Validated` (True);
   `TerminalError` (Cloudflare 4xx, not requeued until something changes); `APIError` (retryable, the error
   is returned so controller-runtime backs off); credential reasons from `credentials.go`
   (`AccountNotFound`, `AccountNotReady`, `SecretNotFound`, `TokenKeyMissing`, requeued after a minute)
2. **Status fields** — every CRD status has `conditions []metav1.Condition` and a `cloudflareMetadata` block
3. **References** — foreign-key relationships use `*Ref` fields (e.g., `zoneRef`, `tunnelRef`) not raw IDs
4. **Adoption** — controllers adopt an existing Cloudflare resource with the same name before creating one
   (the planned `AdoptedResource` CRD, Phase 4, will make this explicit)
5. **FieldExport** (planned, Phase 4) — `FieldExport` CR pipes `.status.*` fields into ConfigMaps for cross-namespace consumption
6. **Deletion policy** — annotation `kflare.dev/deletion-policy: retain | delete` controls whether the
   Cloudflare resource is deleted on CR deletion; check it with `reconciler.RetainOnDelete`
7. **Immutability** — fields that identify the Cloudflare resource (names, refs) are immutable via CEL
   `self == oldSelf`, with an envtest case per field
8. **Status helpers** — set conditions and report errors through `internal/controller/status.go`
   (`updateReady`, `updateNotReady`, `handleCloudflareError`, `notReadyRetryAfter`); API types implement
   `GetConditions/SetConditions`
9. **Prefix** — every finalizer, annotation and label kflare owns starts with `kflare.dev/`
10. **ARCHITECTURE.md** — a new controller, or a change to drift detection, adoption, error handling or deletion,
    updates the matching section and tables in `ARCHITECTURE.md`

---

## Development Commands

```bash
# Generate CRDs and deepcopy functions
make generate manifests

# Install CRDs into current cluster context
make install

# Run controller locally (against current kubeconfig)
make run

# Run unit tests
make test

# Lint (also run in CI)
make lint

# Pinned kind (v0.30+ needed for 1.34 node images); local/setup.sh uses it
make kind

# e2e: deploys to the kind cluster in the current context ($KIND_CLUSTER, default "kind");
# runs the live Cloudflare specs when CF_API_TOKEN/CF_ACCOUNT_ID (and CF_E2E_ZONE) are set
make test-e2e

# Regenerate the Helm chart's CRDs/RBAC from config/, and lint + render it
make helm
make helm-lint

# Build and push controller image
make docker-build docker-push IMG=ghcr.io/your-org/kflare:latest
```

---

## Environment Variables (for local development)

```bash
export CF_API_TOKEN=<your-cloudflare-api-token>
export CF_ACCOUNT_ID=<your-cloudflare-account-id>
export CF_E2E_ZONE=kflare.dev               # zone for the DNS e2e specs; adopted with retain, never deleted
```

---

## Cloudflare API Reference

- **OpenAPI Spec:** https://github.com/cloudflare/api-schemas
- **Go SDK:** https://github.com/cloudflare/cloudflare-go
- **Developer Docs:** https://developers.cloudflare.com/api/

---

## Current Status

> **Phases 1 and 2 complete. Phase 3 in progress: `KVNamespace` and `R2Bucket` done.**
> Next: confirm the SDK decision, then `feat/rulesets-foundation` and `WAFCustomRule` (see Phase 3). Items needing a
> paid plan, add-on or account setup are tracked under "Deferred: paid plan, add-on or account setup" in Phase 3.
> Last updated: October 2026
