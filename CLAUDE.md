# kflare — Cloudflare Kubernetes Operator

## Project Overview

kflare is a production-grade Kubernetes operator for Cloudflare, enabling fully declarative, GitOps-driven management of Cloudflare resources via Kubernetes CRDs.

The goal is to fill a gap in the ecosystem: existing community projects (adyanth/cloudflare-operator, containeroo/cloudflare-operator, replicatedhq/kubeflare) are narrowly scoped and inconsistently maintained. kflare targets production-grade quality with the intent to pursue official or CNCF-adjacent backing.

**Key differentiator:** Code-generate CRDs and controllers from Cloudflare's generated Go SDK and its [OpenAPI spec](https://github.com/cloudflare/api-schemas), driving broad API coverage with consistent patterns.

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
Decided 2026-10-13; the plan is [Code generator (`kflare-gen`)](#code-generator-kflare-gen) in Phase 3.
- `kflare-gen` reads a per-kind `generator.yaml`, the Go types of cloudflare-go v7 (the SDK Cloudflare generates from
  its OpenAPI spec) and a pinned subset of that spec (`api-schemas` repo). It generates CRD types and each kind's
  controller glue for a shared reconciler. It generates no client code: v7 is the client
- What the API description cannot express, such as behavior verified live, stays hand-written as Go hooks
- Controllers written before the generator stay hand-written unless moving them buys something

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
├── generator/             # kflare-gen: generator.yaml, SDK and OpenAPI readers, emitters (make codegen)
├── hack/                  # sync-helm-chart.sh and the license header
├── docs/deploy.md         # Deploy guide (Helm install, accounts, upgrade, uninstall, troubleshooting)
├── ARCHITECTURE.md        # How the controllers behave: drift, adoption, errors, deletion, known limitations
├── CLAUDE.md              # This file
└── Makefile
```
Planned, not yet present: the Docusaurus site under `docs/` (Phase 4).

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
  - `make test` (runs manifests, generate, fmt, vet, unit and envtest suites)
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
    changed in place; an idle reconcile leaves it alone), and a WAFCustomRule that uses the IP list (refused while the
    list does not exist, written once it does). Then everything is deleted in one pass and Cloudflare is checked,
    including that the managed phase's emptied entry point is gone. Everything is named `kflare-e2e-<run>` (the list
    `kflare_e2e_<run>`) and cleaned up even on failure
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
| `feat/cloudflare-go-v0.119` | none: SDK bump (see [SDK decision](#sdk-decision)) | — | ✅ |
| `feat/rulesets-foundation` | none: Rulesets client calls and the shared rule sync | zone-controller | ✅ |
| `feat/waf-custom-rule-controller` | `WAFCustomRule` (was `FirewallRule`) | rulesets-foundation | ✅ Free: 5 rules, no regex, no Log action, no custom block response |
| `feat/rate-limit-rule-controller` | `RateLimitRule` (was `RateLimit`) | rulesets-foundation | ✅ Free: 1 rule, 10 s period and timeout, counts by IP, block only |
| `feat/ip-list-controller` | `IPList` | shared-client | ✅ Free: 1 IP list, 10,000 items |
| `feat/zero-trust-controllers` | `AccessGroup`, `AccessPolicy`, `AccessApplication` | shared-client | Blocked: Zero Trust is not enabled on the account (a free plan exists) |
| `feat/managed-ruleset-controller` | `ManagedRuleset` (was `WAFPackage`) | rulesets-foundation | ✅ By hand (decided 2026-10-13). Free: only the Cloudflare Free Managed Ruleset, with overrides |
| Code generator: `feat/cloudflare-go-v7` … `feat/generator-rule-kinds` (8 PRs) | none: `kflare-gen` and the shared CRUD reconciler; KVNamespace moves onto them; the first new generated kind and the first generated rule kind (see [Code generator](#code-generator-kflare-gen)) | — | ✅ 8 of 8 (`feat/cloudflare-go-v7` … `feat/generator-rule-kinds`); decided 2026-10-13 |
| `feat/d1-database-controller` | `D1Database` | code generator (PR 6) | ✅ Generated end to end. Free: 10 databases, 500 MB each, 5 GB in total |
| `feat/managed-transform-controller` | `ManagedTransform` | zone-controller | Free: all six transforms |
| `feat/generator-rule-kinds` | `TransformRule` | code generator (rule kinds) | ✅ Generated, the first rule kind (the code generator's PR 8). Free: 10 rules across its three phases, no regex |
| `feat/redirect-rule-controller` | `RedirectRule` | code generator (rule kinds) | ✅ Generated. Free: 10 rules ✔, no regex ✔ |
| `feat/cache-rule-controller` | `CacheRule` | code generator (rule kinds) | ✅ Generated. Free: 10 rules ✔; of a custom cache key, only leaving out the query string ✔ |
| `feat/configuration-rule-controller` | `ConfigurationRule` | code generator (rule kinds) | Generated. Free: 10 rules |
| `feat/origin-rule-controller` | `OriginRule` | code generator (rule kinds) | Generated. Free: 10 rules, destination port override only |
| `feat/managed-ruleset-exception-controller` | `ManagedRulesetException` | managed-ruleset-controller | Not scheduled (decided 2026-10-14, see `ManagedRuleset`). Free: skip rules accepted |
| `feat/load-balancer-monitor-controller` | `LoadBalancerMonitor` | shared-client | ⏸ Deferred: needs Load Balancing add-on |
| `feat/load-balancer-controller` | `LoadBalancer` (+ pools) | load-balancer-monitor-controller | ⏸ Deferred: needs Load Balancing add-on |

Rows run top to bottom. The order decided on 2026-10-06 (WAF, then rate limiting, then Zero Trust) is kept. `IPList`
comes before Zero Trust because it is free and can be tested today, while Zero Trust waits on a dashboard step. When a
row is blocked, skip ahead: `ManagedTransform` depends on no other row. The five Rules rows are generated (decided
2026-10-13), so they wait for the code generator's rule kinds.

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
| `WAFCustomRule`, `RateLimitRule`, `ManagedRuleset` | Zone WAF Write | ✅ dry run accepted; enough on its own (2026-10-13 and 2026-10-14, see below) |
| `TransformRule` | Zone Transform Rules Write | ✅ written, updated and deleted by the e2e (2026-10-15, runs `7ed03a` and `9c869b`); enough on its own (2026-10-15, see below) |
| `RedirectRule` | Dynamic URL Redirects Write | ✅ added on `kflare.dev` 2026-10-15 (see below); written, updated and deleted by the e2e (run `5f9daf`); not yet checked on its own |
| `CacheRule` | Cache Settings Write | ✅ added on `kflare.dev` 2026-10-15; written, updated and deleted by the e2e (run `fd8de2`); not yet checked on its own |
| `ConfigurationRule` | Config Settings Write | ✅ added on `kflare.dev` 2026-10-15; dry run accepted |
| `OriginRule` | Origin Write | ✅ added on `kflare.dev` 2026-10-15; dry run accepted |
| `ManagedTransform` | Managed headers Write | ❌ Read only (not a Rulesets endpoint, so no dry run) |
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
- On 2026-10-15, with the user's OK for the four remaining Rules kinds, the dev token gained Dynamic URL Redirects
  Write, Cache Settings Write, Config Settings Write and Origin Write, in a new policy scoped to `kflare.dev` alone (its
  other policies are unchanged). A dry run in each of the four phases was accepted
- The dev token's account-level permissions cover more than deploy.md lists, so it cannot show that a permission is
  enough. Check a new one with a short-lived token holding only deploy.md's permissions, as above (ask first)

#### SDK decision
**Status:** Accepted 2026-10-09. Step 1 is done: kflare uses cloudflare-go **v0.119.0**. Step 3 is brought forward
for generated kinds (2026-10-13): **v7.12.0** sits next to v0.119 since `feat/cloudflare-go-v7`, and the rule kinds use
it since `feat/rulesets-v7`

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
   returns the same typed errors as every other call. v7's rulesets services replaced them in `feat/rulesets-v7`. Still
   open: R2 jurisdictions
3. **Move to the generated SDK later, controller by controller** (Phase 4, or sooner if the v0 line stops getting
   releases). Each controller already sits behind its own narrow interface, so `pkg/cloudflare.Client` can embed both
   SDKs during the move, with `errors.go` classifying both error types until it is done. Brought forward on 2026-10-13
   for the kinds the [code generator](#code-generator-kflare-gen) produces: they use v7 from the start, so
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
Last checked 2026-10-07; the Free-plan limits and Zero Trust again on 2026-10-09.

**Not built yet — needs something we don't have:**

| Item | Needs | Cost (2026-10) | What was verified | When we come back to it |
|------|-------|----------------|-------------------|-------------------------|
| `LoadBalancerMonitor`, `LoadBalancer` (+ pools) | Load Balancing add-on (account) | $5/month incl. 2 origins, +$5/origin/month, +$0.50 per 500K DNS queries | Live, 2026-10-06: every monitor create fails, `interval is not in range [1, 1]`. The token already has Load Balancing: Monitors and Pools Write | Subscribe, build the monitor then the load balancer; cancel after if it was only for testing |
| `HealthCheck` (standalone, zone-level Health Checks) | Pro plan or higher on the zone; zone-level Health Checks Write on the token | Pro plan ~$20+/month per zone | Docs: Free plan gets 0 checks (Pro 10, Business 50). Token has Health Checks Read only | Not in the Phase 3 table yet. Add a branch once a Pro zone exists; keep it separate from `LoadBalancerMonitor` |
| R2Bucket `storageClass` (a bucket's default storage class, `Standard` or `InfrequentAccess`) | Nothing to subscribe to, but R2's free tier covers Standard storage only, so testing Infrequent Access is billed usage | Usage-based; see R2 pricing | Not probed, to stay free. Every bucket on the account reports `Standard`. Also needs `Raw` calls: cloudflare-go v0.119 cannot send the `cf-r2-storage-class` header | Decide whether a few cents of test usage are acceptable, then add it as an optional, mutable field (create header + PATCH) |

**Built, but not verified live because it costs money:**

| Item | Why | What exists | When we come back to it |
|------|-----|-------------|-------------------------|
| Zone `spec.plan` | Changing a plan bills the account | Implemented (subscription POST/PUT, `plan_pending`), unit-tested against response shapes read from the live Free zone | Exercise once on a disposable zone (upgrade, check status, downgrade); expect a prorated charge |
| WAFCustomRule `blockResponse` | Custom block responses need a Pro plan; a Free zone refuses them ("not entitled to use a custom response", dry run 2026-10-09) | Implemented to Cloudflare's documented limits (status 400–499, four content types, 2 KB body), unit-tested. A dry run on 2026-10-13 confirmed the limits: Cloudflare validates a response before it checks the plan (codes 20048, 20052). The same response on a `RateLimitRule`, which Free allows, is verified live | On a Pro zone: create a block rule with a response, request the path, check the status and body |

**Worked around because the clean option is paid:**

| Item | Paid option | Current workaround | When we come back to it |
|------|-------------|--------------------|-------------------------|
| Ownership marker on the Cloudflare side (DNSRecord, and in general) | DNS record tags (Pro plan and up) | `kflare.dev/record-id` / `route-id` / `kv-namespace-id` / `d1-database-id` / `r2-bucket-name` / `ip-list-id` labels in the cluster; ARCHITECTURE.md lists "no ownership marker on the Cloudflare side" as a known limitation | With a Pro zone, tag records kflare creates; or a TXT ownership registry like external-dns (works on Free) |

**Free, but needs account setup first:**

| Item | Setup | Verified |
|------|-------|----------|
| Zero Trust (`AccessApplication`, `AccessPolicy`, `AccessGroup`) | Enable Zero Trust in the dashboard (pick a team name and the Free plan, up to 50 users). Not confirmed whether it asks for a payment method | Live, 2026-10-07 and 2026-10-09: the API returns `Access is not enabled`. The token already has the Access: Apps/Policies/Groups Write permissions |

**Free-plan limits to design around** (build for them; the e2e suite must stay within them). From Cloudflare's docs,
checked 2026-10-09; ✔ marks what a dry run on `kflare.dev` confirmed the same day:

| Item | Free | Pro | Business |
|------|------|-----|----------|
| WAF custom rules | 5 rules ✔, no regex ✔, no Log action ✔, no custom block response ✔, 1 zone-level custom ruleset | 20 rules | 100 rules, regex |
| Rate limiting rules | 1 rule ✔, period and mitigation timeout 10 s only ✔, counts by IP ✔, block only ✔, no counting expression ✔, no throttling (`mitigation_timeout: 0`) ✔, custom block response ✔ | 2 rules, periods up to 1 min, mitigation up to 1 h | 5 rules, periods up to 10 min, mitigation up to 1 day |
| WAF managed rulesets | Cloudflare Free Managed Ruleset only ✔, with overrides for the whole ruleset, a category or a rule ✔; no payload logging ✔ | + Cloudflare Managed Ruleset, OWASP Core Ruleset, Exposed Credentials Check | same as Pro |
| Custom lists | 1 IP list ✔, 10,000 items in the account ✔; the quota is per kind (a hostname list was accepted alongside) | 10 lists, 10,000 items in total | same as Pro |
| Transform, redirect, cache, configuration and origin rules | 10 rules each, transform rules counted across their three phases ✔, single redirects ✔, cache rules ✔; no regex in transform ✔ or redirect rules ✔ | 25 each | 50 each; regex in transform and redirect rules |
| Origin rule overrides | destination port only (Host header, SNI and DNS record need Enterprise) | same | same |

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
- Jurisdictions (`eu`, `fedramp`): cloudflare-go v0.119 cannot send `cf-r2-jurisdiction`, and its Worker R2 binding
  has no `jurisdiction` field. Needs `Raw` calls and a custom binding, or the generated SDK (v7 supports both)
- Default storage class: see Deferred (Infrequent Access testing is billed)
- Bucket configuration that Terraform models as separate resources: CORS, lifecycle rules, custom domains and the
  r2.dev public URL, event notifications, bucket locks, Sippy

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
**Status:** ✅ Complete

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

#### Branch: `feat/rate-limit-rule-controller`
**Depends on:** `feat/rulesets-foundation`
**Status:** ✅ Complete

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
**Status:** ✅ Complete

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
**Depend on:** the code generator's rule kinds (`feat/generator-rule-kinds`); one branch and PR each
**Status:** 🚧 `TransformRule` ✅, generated in `feat/generator-rule-kinds` (the code generator's PR 8); `RedirectRule`
✅ (`feat/redirect-rule-controller`); `CacheRule` ✅ (`feat/cache-rule-controller`); `ConfigurationRule` and
`OriginRule` not started

Each is the foundation plus typed action parameters. Together they replace Page Rules. They are generated (decided
2026-10-13): each is a `generator.yaml` entry plus hooks for what Cloudflare's schema leaves out (see
[Code generator](#code-generator-kflare-gen)).

| CRD | Phase | Action | Free | Dev token |
|-----|-------|--------|------|-----------|
| `TransformRule` (`spec.type`, immutable: `urlRewrite`, `requestHeaders` or `responseHeaders`) ✅ | `http_request_transform`, `http_request_late_transform`, `http_response_headers_transform` | `rewrite` | 10 rules across the three phases ✔, no regex ✔ | ✅ |
| `RedirectRule` (single redirects) ✅ | `http_request_dynamic_redirect` | `redirect` | 10 rules ✔, no regex ✔ | ✅ since 2026-10-15 |
| `CacheRule` ✅ | `http_request_cache_settings` | `set_cache_settings` | 10 rules ✔ | ✅ since 2026-10-15 |
| `ConfigurationRule` | `http_config_settings` | `set_config` | 10 rules | ✅ since 2026-10-15 |
| `OriginRule` | `http_request_origin` | `route` | 10 rules, destination port only | ✅ since 2026-10-15 |

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
- deploy.md documents Zone · Single Redirect · Edit (the API's Dynamic URL Redirects Write); not yet checked on its own
  with a short-lived token (see Token permissions)
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
  Write); not yet checked on its own with a short-lived token (see Token permissions)
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


#### Code generator (`kflare-gen`)
**Depends on:** `feat/shared-client`, `feat/rulesets-foundation`; eight PRs, in order
**Status:** ✅ Complete: PRs 1 to 8 (`feat/cloudflare-go-v7`, `feat/crud-reconciler`, `feat/generator-types`,
`feat/generator-openapi`, `feat/generator-controllers`, `feat/d1-database-controller`, `feat/rulesets-v7`,
`feat/generator-rule-kinds`). Planned and decided 2026-10-13

Most of a new kind is the same wiring every time. IPList (#26) changed 27 files (+2,596 lines). Besides its types and
its bulk-operation logic, it repeated `Get/SetConditions`, registration in `cmd/main.go`, the CRD kustomization, the
Helm `edit` role, the account watch and protection entry, the narrow API interface and its factory, and the lifecycle
specs every CRUD kind has (finalizer, account not ready, invalid token, create and claim, idle reconcile writes
nothing, update in place, adopt, conflict, recreate, retain, failed deletions retried). `kflare-gen` generates that
part from Cloudflare's own description of its API. What Cloudflare does not describe stays hand-written.

**Decided (2026-10-13):**
1. Generated kinds use cloudflare-go **v7** from the start, bringing [SDK decision](#sdk-decision) step 3 forward for
   them only. v0.119 is hand-written, with no machine-readable list of its resources and operations to generate from
2. Fields are **opt-out**: a generated kind has every field v7 exposes for it, minus those its entry ignores. The
   output is committed, so each field an SDK bump adds shows up in review
3. `ManagedRuleset` is built by hand first, as planned (✅); `feat/rulesets-v7` moved its conversion to v7 (✅)
4. The first new kind generated end to end is a free, account-level resource: a D1 database or a Turnstile widget,
   picked when that PR starts, after checking the Free-plan limits and the token permission. Picked 2026-10-15:
   `D1Database` (see [its section](#branch-featd1-database-controller))

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
- The OpenAPI spec runs ahead of the SDK (its KV create takes a `mode` that v7.12.0 lacks), and it is not always the
  live API: its list-name pattern allows uppercase, which Cloudflare refuses (10029, see IPList)

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
    a `SecretKeySelector`; the ID and computed fields → `status.cloudflareMetadata`
  - Unions → one struct with the fields of every variant and a CEL rule that a value matches a variant (built for rule
    kinds' action parameters; see the rule kinds' notes below). Maps of objects → a list keyed by the field the entry
    names (`listType=map`)
  - A field that Cloudflare's read cannot return is refused unless the entry says how to detect its drift (a hash in
    status, as WorkerScript does) or ignores it
- **Outputs**, marked `// Code generated by kflare-gen. DO NOT EDIT.` and committed. `make codegen` runs before
  controller-gen, and CI's `git diff --exit-code` catches stale output:
  - `api/v1alpha1/zz_generated.<kind>_types.go`: spec, status, list, markers, print columns, `Get/SetConditions`
  - `internal/controller/zz_generated.<kind>.go`: the narrow v7 interface, spec → create and update params, response →
    status, the drift comparison, RBAC markers and the kind's descriptor
  - `internal/controller/zz_generated_kinds.go`: registration (`cmd/main.go` calls `SetupGeneratedReconcilers` once)
    and the kinds' entries as users of their account; the kinds' lines in the CRD kustomization and the Helm `edit`
    role, between `+kflare-gen:begin` and `+kflare-gen:end`; and a sample in `config/samples`
  - `internal/controller/zz_generated.<kind>_test.go`: an envtest table per kind, for the lifecycle specs above and the
    immutability and validation cases (`crudresource_specs_test.go`)
- **Shared CRUD reconciler** (hand-written, `crudresource_controller.go`, the counterpart of
  `rulesetrule_controller.go`): ARCHITECTURE.md's reconcile loop for any kind a `crudResourceKind` describes, with
  adoption by key and claim labels (`claimedIDs`/`claimID`), recreation after a 404, writes only on drift, the retain
  policy and waiting for dependents. Deletion blockers are data: `{code: 10008, reason: BucketNotEmpty}`,
  `{code: 10041, reason: ListInUse}`. It carries the coverage target, which excludes generated files
- **Hooks:** optional Go functions in a hand-written `<kind>_hooks.go`, compiled and linted like any other code: adjust
  the desired params, normalize Cloudflare's copy before comparing, extra steps after a sync, extra watches. IPList's
  bulk operation and RateLimitRule's `cf.colo.id` are this kind of logic

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
      supports_url_encoding: Not observed by the controller yet.
```

**PRs**, one at a time against main:

| # | Branch | Change | Done when |
|---|--------|--------|-----------|
| 1 | `feat/cloudflare-go-v7` | ✅ v7.12.0 next to v0.119: `pkg/cloudflare.Client` carries v7's request options (`V7Options`), from which controllers build the services they call, and `errors.go` also classifies `*cloudflare.Error` (see the [v7 notes](#sdk-decision)) | Unit tests pass. Nothing uses v7 yet |
| 2 | `feat/crud-reconciler` | ✅ The shared CRUD reconciler, with KVNamespace hand-ported onto it and onto v7, whose `Get` ends the list-and-match workaround (see the notes below the table) | KVNamespace's specs (fake adapted) and the live e2e pass |
| 3 | `feat/generator-types` | ✅ `generator/`: SDK loader, model, `generator.yaml`, types emitter, golden tests, `make codegen` (see the notes below the table) | KVNamespace's regenerated CRD matches the hand-written one, or differs only on purpose |
| 4 | `feat/generator-openapi` | ✅ `kflare-gen sync-spec`, the committed subset, constraints and hints (see the notes below the table) | Golden tests: the constraints generated for KVNamespace, R2Bucket and IPList match their CRDs, or the entry says why not |
| 5 | `feat/generator-controllers` | ✅ The controller emitter: descriptor, conversions, drift, RBAC, registration, protection, sample, envtest table (see the notes below the table) | KVNamespace is fully generated and its hand port deleted; its specs and the live e2e pass |
| 6 | `feat/d1-database-controller` | ✅ `D1Database`, the first new kind, generated end to end (decision 4; see [its section](#branch-featd1-database-controller)) | Only its entry, any hooks and its live e2e spec are hand-written |
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
  (`feat/cache-rule-controller`), then ConfigurationRule and OriginRule. The dev token can write all four phases since
  2026-10-15
- **Later:** a runtime for settings-style singletons (the shape of TunnelConfiguration and ManagedTransform; the 90
  singletons include zone settings), cross-kind references such as `kvNamespaceRef`, generated live e2e specs, and
  deploy.md permission rows from `x-api-token-group` (API names, while deploy.md uses the dashboard's labels; the
  shared Rulesets endpoints list every phase's permission)
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
- The dev account holds a production database, `nhl-trade-tracker`: the e2e spec and its cleanup touch only databases
  named `kflare-e2e-<run>`
- Needs the account-level **D1: Edit** permission (the API's D1 Write), which is enough on its own (verified
  2026-10-15, see Token permissions)


#### Release and CI
**Status:** 🔲 Open, alongside Phase 3

- [ ] Run the live e2e suite in GitHub Actions. The repo has no Actions secrets or variables yet (2026-10-09). Use a
  dedicated CI token holding only what the suite needs, not the dev token, which also holds Account API Tokens Write
  and Registrar Domains Admin. What the suite needs is deploy.md's permissions, with Zone Read (verified 2026-10-13,
  see Token permissions). Set the secrets `CF_API_TOKEN` and `CF_ACCOUNT_ID`, and the variable
  `CF_E2E_ZONE=kflare.dev`. A Free zone holds one rate limiting rule and a Free account one IP list, so runs must
  not overlap: give the workflow a `concurrency` group, and avoid local runs while it runs
- [ ] Tag `v0.1.0`: `Chart.yaml` is already at 0.1.0 and no tag exists. The release workflow publishes the image and
  chart. (`docs/deploy.md`'s stale "the six kflare CRDs" was fixed with WAFCustomRule)
- [ ] `kflare.dev` expires on 2027-10-04 with auto-renew off (registered 2026-10-04). The e2e zone disappears if it
  lapses: decide on renewal before September 2027
- [ ] Open question: a Terraform `infra/` root for `kflare.dev` (zone, renewal, CI token, GitHub secrets)

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
# Generate CRDs and deepcopy functions (both run make codegen first: kflare-gen's kinds)
make generate manifests

# Generate only kflare-gen's kinds (API types, controllers, envtest tables, samples, registration), from
# generator/generator.yaml, cloudflare-go v7 and generator/openapi
make codegen

# Rewrite generator/openapi from Cloudflare's OpenAPI spec at API_SCHEMAS_COMMIT (after adding a kind or moving the SDK)
make sync-spec

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

> **Phases 1 and 2 complete. Phase 3 in progress: `KVNamespace`, `R2Bucket`, `WAFCustomRule`, `RateLimitRule`, `IPList`,
> `ManagedRuleset`, `D1Database`, `TransformRule`, `RedirectRule` and `CacheRule` done.** cloudflare-go is at v0.119,
> with v7.12.0 next to it for the generated kinds and the rule kinds; rule kinds share the Rulesets foundation and
> controller. The code generator, `kflare-gen`, is complete: KVNamespace and D1Database are generated in full on the
> shared CRUD reconciler, and TransformRule, RedirectRule and CacheRule on the shared rule controller. Next:
> ConfigurationRule and OriginRule, each a `generator.yaml` entry (see Phase 3).
> Zero Trust still waits on account setup. Items needing a paid plan, add-on or account setup are tracked under
> "Deferred: paid plan, add-on or account setup" in Phase 3.
> Last updated: October 2026
