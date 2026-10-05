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
- **Go** (1.22+)
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
    Zone, Tunnel or WorkerScript references it, and the token Secret (`kflare.dev/token-protection`, recorded
    in `status.protectedTokenSecret`) while an account references it — see `cloudflareaccount_protection.go`
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
  - `GOTOOLCHAIN=local`; `ENVTEST_VERSION` pinned to `release-0.19` (tagged setup-envtest releases need a newer Go)
- [x] `.github/workflows/e2e.yml` (manual trigger only)
  - Deployment smoke test: builds the image, deploys `config/default` to a kind cluster and checks the
    manager pod runs. It does not call Cloudflare; a live-API e2e suite is still to do
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
    `bindings[]` (exactly one of `plainText`, `secretKeyRef {name, key}`, `kvNamespaceID`, `r2BucketName`)
  - `status.conditions`, `status.cloudflareMetadata` (etag, modifiedOn at full precision), `status.appliedHash`
- [x] `internal/controller/workerscript_controller.go`
  - Uploads when the desired-state hash changes, when the Worker is missing, or when Cloudflare's `modified_on`
    differs from kflare's last upload
  - Watches ConfigMaps (script source), Secrets (secret bindings) and CloudflareAccounts
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
- Routes are not part of WorkerScript: they are zone-level resources with their own IDs. Planned as a separate
  `WorkerRoute` CRD (`zoneRef` + `pattern` + `workerScriptRef`) following the Zone → DNSRecord pattern

**Test locally:**
```sh
kubectl create secret generic example-worker-secrets --from-literal=api-key=...
kubectl apply -f config/samples/cloudflare_v1alpha1_workerscript.yaml
kubectl get workerscript example-worker -o yaml   # check Ready + cloudflareMetadata.modifiedOn
```

---

### Phase 3 — Expanded API Surface
**Status:** 🔲 Not started

Same branch-per-feature pattern as Phase 2. Planned branches:

| Branch | CRD(s) | Depends on |
|--------|--------|------------|
| `feat/health-check-controller` | `HealthCheck` | shared-client |
| `feat/load-balancer-controller` | `LoadBalancer` | health-check-controller |
| `feat/rate-limit-controller` | `RateLimit` | zone-controller |
| `feat/firewall-rule-controller` | `FirewallRule` | zone-controller |
| `feat/page-rule-controller` | `PageRule` | zone-controller |
| `feat/r2-bucket-controller` | `R2Bucket` | shared-client |
| `feat/kv-namespace-controller` | `KVNamespace` | shared-client |
| `feat/zero-trust-controllers` | `AccessApplication`, `AccessPolicy`, `AccessGroup` | shared-client |
| `feat/waf-controller` | `WAFPackage` | zone-controller |
| `feat/managed-transform-controller` | `ManagedTransform` | zone-controller |

**Notes:**
- Zero Trust resources require `Account`-scoped (not Zone-scoped) API tokens — separate CF token needed
- R2 and KV are account-level — no `zoneRef`
- `LoadBalancer` depends on `HealthCheck` — do health checks first

**Also in Phase 3:**
- [ ] OpenAPI-to-CRD generator skeleton (`generator/` package) — parses the [Cloudflare OpenAPI spec](https://github.com/cloudflare/api-schemas) to generate CRD type definitions and reconciler skeletons; intended to accelerate the long tail of resources beyond what is hand-written in Phase 2

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

# Deployment smoke test against a kind cluster named $KIND_CLUSTER (default "kind")
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
export CF_ZONE_ID=<zone-id-for-e2e-tests>   # a test zone, not production
```

---

## Cloudflare API Reference

- **OpenAPI Spec:** https://github.com/cloudflare/api-schemas
- **Go SDK:** https://github.com/cloudflare/cloudflare-go
- **Developer Docs:** https://developers.cloudflare.com/api/

---

## Current Status

> **Phase 1 and Phase 2 complete.**
> Next branch: a `WorkerRoute` CRD (`zoneRef` + `pattern` + `workerScriptRef`), then Phase 3.
> Last updated: October 2026
