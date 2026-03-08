# kflare — Cloudflare Kubernetes Operator

## Project Overview

kflare is a production-grade Kubernetes operator for Cloudflare, enabling fully declarative, GitOps-driven management of Cloudflare resources via Kubernetes CRDs.

The goal is to fill a gap in the ecosystem: existing community projects (adyanth/cloudflare-operator, containeroo/cloudflare-operator, replicatedhq/kubeflare) are narrowly scoped and inconsistently maintained. kflare targets production-grade quality with the intent to pursue official or CNCF-adjacent backing.

**Key differentiator:** Code-generate controllers from the [Cloudflare OpenAPI spec](https://github.com/cloudflare/api-schemas), driving broad API coverage with consistent patterns.

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
**Status:** 🔶 Partial — core done, deferred items moved to later phases

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
- [ ] `pkg/cloudflare/client.go` — shared CF client wrapper (deferred: will build alongside Phase 2 controllers)
- [ ] `pkg/reconciler/base.go` — shared reconciler interface (deferred: will extract once pattern is established across 2+ controllers)
- [ ] OpenAPI-to-CRD generator skeleton (`generator/` package) — deferred to later
- [ ] `AdoptedResource` and `FieldExport` CRDs — deferred to Phase 4
- [ ] GitHub Actions CI — deferred, will add before Phase 2 merge
- [ ] Base Helm chart — deferred to Phase 4

**Actual file layout (differs from original plan):**
- `api/v1alpha1/` (not `apis/`) — kubebuilder convention
- `internal/controller/` (not `pkg/reconciler/`) — kubebuilder convention
- API group resolved to `cloudflare.cloudflare.k8s.io` (group=`cloudflare` + domain=`cloudflare.k8s.io`)

---

### Phase 2 — Core Resource Controllers
**Status:** 🔲 Not started

Each feature below is developed on its own branch and merged independently once tested.
Branch naming convention: `feat/<name>` (e.g. `feat/shared-client`, `feat/zone-controller`)

---

#### Branch: `feat/shared-client`
**Depends on:** main (Phase 1)
**Merges into:** main (before any controller branch starts)

Shared infrastructure that all Phase 2 controllers will use. Build this first.

- [ ] `pkg/cloudflare/client.go` — thin wrapper around cloudflare-go SDK
  - Constructor that accepts an API token string
  - Exposes typed methods per resource (DNS, Zone, Tunnel, Workers)
  - Returns structured errors that controllers can classify as terminal vs retryable
- [ ] `pkg/reconciler/base.go` — shared reconciler helpers
  - `SetCondition()` helper (wraps `meta.SetStatusCondition`)
  - `IsTerminalError()` classifier
  - Finalizer add/remove helpers
- [ ] Unit tests for client error classification

**Test locally:** `make test` — no cluster needed

---

#### Branch: `feat/ci`
**Depends on:** main (can be done anytime)
**Merges into:** main

- [ ] `.github/workflows/ci.yml`
  - `go build ./...`
  - `go vet ./...`
  - `make manifests` + diff check (ensures generated files are committed)
  - `make test`
- [ ] `.github/workflows/e2e.yml` (manual trigger only, requires secrets)
  - `CF_API_TOKEN`, `CF_ACCOUNT_ID` as GitHub Actions secrets
  - Spins up kind cluster, runs `//go:build e2e` tests

**Test locally:** push branch and verify Actions run green

---

#### Branch: `feat/zone-controller`
**Depends on:** `feat/shared-client`
**Merges into:** main

`Zone` is the root resource — every other resource references it via `zoneRef`.

- [ ] `api/v1alpha1/zone_types.go`
  - `spec`: `name` (domain), `accountRef`, `plan` (free/pro/business), `settings`
  - `status.conditions`, `status.cloudflareMetadata` (zone ID, name servers, status)
- [ ] `internal/controller/zone_controller.go`
  - Create / read / update / delete lifecycle
  - Drift detection on zone settings
  - Finalizer: `cloudflare.k8s.io/finalizer`
  - Deletion policy annotation: `retain | delete`
- [ ] Unit tests (mocked CF client)
- [ ] `config/samples/zone.yaml`
- [ ] Local test: apply sample, verify zone appears in Cloudflare dashboard

**Test locally:**
```sh
kubectl apply -f config/samples/zone.yaml
kubectl get zone -o yaml   # check Ready condition + cloudflareMetadata.zoneID
```

---

#### Branch: `feat/dns-record-controller`
**Depends on:** `feat/zone-controller`
**Merges into:** main

- [ ] `api/v1alpha1/dnsrecord_types.go`
  - `spec`: `zoneRef`, `name`, `type` (A/AAAA/CNAME/MX/TXT/SRV/CAA), `content`, `ttl`, `proxied`
  - `status.conditions`, `status.cloudflareMetadata` (record ID)
- [ ] `internal/controller/dnsrecord_controller.go`
  - Create / update / delete lifecycle
  - Drift detection (content, TTL, proxied)
  - Watches parent `Zone` — requeues records if zone becomes unready
  - Finalizer + deletion policy
- [ ] Unit tests
- [ ] `config/samples/dnsrecord_a.yaml`, `dnsrecord_cname.yaml`
- [ ] Local test: apply A record, verify it appears in Cloudflare DNS dashboard

**Test locally:**
```sh
kubectl apply -f config/samples/dnsrecord_a.yaml
kubectl get dnsrecord -o yaml   # check Ready + record ID
```

---

#### Branch: `feat/tunnel-controller`
**Depends on:** `feat/shared-client`
**Merges into:** main (independent of zone controller)

- [ ] `api/v1alpha1/tunnel_types.go`
  - `spec`: `name`, `accountRef`, `credentialsSecretRef` (where to store the tunnel token)
  - `status.conditions`, `status.cloudflareMetadata` (tunnel ID, tunnel token secret name)
- [ ] `internal/controller/tunnel_controller.go`
  - Creates named tunnel via CF API
  - Stores tunnel credentials in a K8s Secret (referenced by `credentialsSecretRef`)
  - Finalizer: deletes tunnel on CR deletion (unless `retain`)
- [ ] Unit tests
- [ ] `config/samples/tunnel.yaml`

**Test locally:**
```sh
kubectl apply -f config/samples/tunnel.yaml
kubectl get tunnel -o yaml          # check tunnel ID in status
kubectl get secret -n kflare-system  # verify credentials secret was created
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

## Total Timeline

| Phase | Duration | Cumulative |
|-------|----------|------------|
| Phase 1: Foundation | 3–4 weeks | 4 weeks |
| Phase 2: Core Controllers | 6–8 weeks | 12 weeks |
| Phase 3: Expanded Surface | 6–8 weeks | 20 weeks |
| Phase 4: Ecosystem | 4–6 weeks | 26 weeks |
| **Total (solo)** | **~5–6 months** | |
| **Total (2–3 contributors)** | **~3–4 months** | |

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

> **Phase 1 core complete. Starting Phase 2.**
> Next branch: `feat/shared-client` — build `pkg/cloudflare/client.go` and `pkg/reconciler/base.go` before any controller work begins.
> Last updated: March 2026
