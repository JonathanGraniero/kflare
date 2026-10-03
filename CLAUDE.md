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
cloudflare.k8s.io
```

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
- Finalizer-based deletion protection: `cloudflare.k8s.io/finalizer`
- `AdoptedResource` CRD for importing pre-existing Cloudflare resources
- `FieldExport` CRD for piping resource fields into ConfigMaps/Secrets

### Repo Structure
```
.
├── apis/
│   └── v1alpha1/          # CRD type definitions (generated + hand-edited)
├── cmd/
│   └── controller/        # Main entrypoint
├── pkg/
│   ├── cloudflare/        # Cloudflare API client wrappers
│   ├── reconciler/        # Shared reconciler base logic
│   └── util/              # Shared utilities
├── config/
│   ├── crd/               # Generated CRD manifests
│   ├── rbac/              # RBAC manifests
│   └── default/           # Kustomize base
├── helm/
│   └── kflare/            # Helm chart
├── generator/             # OpenAPI → CRD/controller codegen tooling
├── test/
│   ├── unit/
│   └── e2e/               # Tests against live Cloudflare API (requires credentials)
├── docs/                  # Docusaurus site
├── CLAUDE.md              # This file
└── Makefile
```

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
- [x] Makefile targets: `generate`, `manifests`, `build`, `run`, `test` (kubebuilder-generated)
- [x] Local dev environment: `local/setup.sh`, `local/teardown.sh`, kind cluster config
- [x] `pkg/cloudflare/client.go` — shared CF client wrapper (completed in `feat/shared-client`)
- [x] `pkg/reconciler/base.go` — shared reconciler helpers (completed in `feat/shared-client`)

**Actual file layout (differs from original plan):**
- `api/v1alpha1/` (not `apis/`) — kubebuilder convention
- `internal/controller/` (not `pkg/reconciler/`) — kubebuilder convention
- API group resolved to `cloudflare.cloudflare.k8s.io` (group=`cloudflare` + domain=`cloudflare.k8s.io`)

---

### Phase 2 — Core Resource Controllers
**Status:** 🔶 In progress — `feat/shared-client`, `feat/ci`, `feat/zone-controller`, `feat/dns-record-controller` and `feat/tunnel-controller` complete

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
  - `Finalizer` constant — `"cloudflare.k8s.io/finalizer"`
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
- [x] `.github/workflows/e2e.yml` (manual trigger only, requires secrets)
  - `CF_API_TOKEN`, `CF_ACCOUNT_ID` as GitHub Actions secrets
  - Spins up kind cluster, runs `//go:build e2e` tests

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
  - Finalizer: `cloudflare.k8s.io/finalizer`
  - Deletion policy annotation: `retain | delete`
  - Watch on CloudflareAccount → re-triggers zones when account heals
- [x] Unit tests (mocked CF client, 96.2% coverage)
- [x] `config/samples/cloudflare_v1alpha1_zone.yaml`
- [x] Bug fix: `pkg/cloudflare/errors.go` — `errors.As` targets must use pointer types

**Zone-specific design notes:**
- `ZoneAPI` interface: `CreateZone/ZoneDetails/ListZones/DeleteZone/EditZone` (all on `*cf.API`)
- `*cfpkg.Client` satisfies `ZoneAPI` because it embeds `*cf.API`
- List→adopt pre-existing zones before creating new ones
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
  - Finalizer: cleans up connections and deletes the tunnel (unless `retain`)
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
- Cloudflare refuses to delete a tunnel with active connectors, so deletion retries until cloudflared stops
- Credential and Secret-conflict failures requeue after 1 minute (the API token Secret is not watched)
- Needs an API token with the account-level **Cloudflare Tunnel: Edit** permission
- Follow-up: move the zone and DNS record controllers onto `resolveAccountToken`

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

- [ ] `api/v1alpha1/tunnelconfiguration_types.go`
  - `spec`: `tunnelRef`, `ingress[]` (hostname → service mappings)
  - `status.conditions`
- [ ] `internal/controller/tunnelconfiguration_controller.go`
  - Pushes ingress config to CF API via tunnel configuration endpoint
  - Drift detection on ingress rules
- [ ] Unit tests
- [ ] `config/samples/tunnelconfiguration.yaml`

**Test locally:**
```sh
kubectl apply -f config/samples/tunnelconfiguration.yaml
# Verify routing config visible in Cloudflare Zero Trust dashboard → Tunnels
```

---

#### Branch: `feat/worker-script-controller`
**Depends on:** `feat/shared-client`
**Merges into:** main (independent of zone/tunnel)
**Note:** Requires a separate CF API token with Workers permissions

- [ ] `api/v1alpha1/workerscript_types.go`
  - `spec`: `accountRef`, `scriptName`, `scriptContent` or `scriptConfigMapRef`, `bindings[]`, `routes[]`
  - `status.conditions`, `status.cloudflareMetadata` (script etag, deployment ID)
- [ ] `internal/controller/workerscript_controller.go`
  - Upload script content via Workers API
  - Manage route bindings
  - Drift detection on script content (etag comparison)
- [ ] Unit tests
- [ ] `config/samples/workerscript.yaml`

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
- [ ] Helm chart published to ArtifactHub
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

1. **Condition types** — use `kflare.ResourceSynced` and `kflare.Terminal` condition types
2. **Status fields** — every CRD status has `conditions []metav1.Condition` and a `cloudflareMetadata` block
3. **References** — foreign-key relationships use `*Ref` fields (e.g., `zoneRef`, `tunnelRef`) not raw IDs
4. **AdoptedResource** — allows importing pre-existing Cloudflare resources into management
5. **FieldExport** — `FieldExport` CR pipes `.status.*` fields into ConfigMaps for cross-namespace consumption
6. **Deletion policy** — annotation `cloudflare.k8s.io/deletion-policy: retain | delete` controls whether the Cloudflare resource is deleted on CR deletion
7. **Terminal errors** — unrecoverable API errors (4xx, invalid config) set `kflare.Terminal=True` and stop requeuing

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

# Run e2e tests (requires CF_API_TOKEN and CF_ACCOUNT_ID env vars)
make test-e2e

# Build and push controller image
make docker-build docker-push IMG=ghcr.io/your-org/kflare:latest

# Package Helm chart
make helm-package
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

> **Phase 1 complete. Phase 2 `feat/shared-client`, `feat/ci`, `feat/zone-controller`, `feat/dns-record-controller`, and `feat/tunnel-controller` complete.**
> Next branch: `feat/tunnel-configuration-controller` (needs the Tunnel controller) or `feat/worker-script-controller` (independent).
> Last updated: October 2026
