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

Implement the highest-value resources that most users need immediately.

| CRD | Cloudflare Resource | API Coverage |
|-----|-------------------|--------------|
| `Zone` | DNS Zone management | Create, Read, Update, Delete + settings |
| `DNSRecord` | DNS record CRUD | A, AAAA, CNAME, MX, TXT, SRV, CAA |
| `Tunnel` | Cloudflare Tunnel lifecycle | Create named tunnel, manage credentials secret |
| `TunnelConfiguration` | Tunnel ingress routing | Map hostnames/services to tunnel |
| `WorkerScript` | Workers deployment | Upload, bind routes, env vars |

**Per-controller checklist (apply to each):**
- [ ] CRD type definition with full spec/status
- [ ] Reconciler: Create / Update / Delete lifecycle
- [ ] `status.conditions`: `kflare.ResourceSynced`, `kflare.Terminal`
- [ ] Drift detection (desired vs. observed diff)
- [ ] Finalizer registration and cleanup
- [ ] Unit tests (mocked Cloudflare client)
- [ ] e2e test with real API (behind `//go:build e2e` tag)
- [ ] Example manifests in `config/samples/`

**Notes:**
- `Zone` is the root resource — most others reference it via `zoneRef`
- `Tunnel` creates a credentials secret in Cloudflare; store locally in K8s Secret and reference via `tunnelCredentialsSecretRef`

---

### Phase 3 — Expanded API Surface
**Status:** 🔲 Not started

| CRD | Cloudflare Resource |
|-----|-------------------|
| `AccessApplication` | Zero Trust Access app |
| `AccessPolicy` | Zero Trust policy rules |
| `AccessGroup` | Zero Trust identity groups |
| `FirewallRule` | Firewall / WAF rules |
| `WAFPackage` | WAF package configuration |
| `PageRule` | Page rules (cache, redirects) |
| `R2Bucket` | R2 object storage bucket |
| `KVNamespace` | Workers KV namespace |
| `LoadBalancer` | Load balancer + pools |
| `HealthCheck` | Origin health checks |
| `RateLimit` | Rate limiting rules |
| `ManagedTransform` | Transform Rules (managed) |

**Notes:**
- Zero Trust resources require `Account`-scoped (not Zone-scoped) API tokens
- R2 and KV are account-level resources — no `zoneRef` needed
- `LoadBalancer` depends on `HealthCheck` — implement health checks first

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

> **Phase 1 core complete.** Kubebuilder scaffolded, `CloudflareAccount` CRD and controller live and validated against real Cloudflare API. Local kind cluster running. Moving to Phase 2 (Zone + DNSRecord controllers).
> Last updated: March 2026
