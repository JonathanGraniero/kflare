# Cloudflare Kubernetes Operator (cloudflare-operator)

## Project Overview

This project implements an **ACK-style (AWS Controllers for Kubernetes) Kubernetes operator for Cloudflare**, enabling fully declarative, GitOps-driven management of Cloudflare resources via Kubernetes CRDs.

The goal is to fill a gap in the ecosystem: existing community projects (adyanth/cloudflare-operator, containeroo/cloudflare-operator, replicatedhq/kubeflare) are narrowly scoped, inconsistently maintained, and none follow ACK-grade conventions. This operator targets production-grade quality with the intent to pursue official or CNCF-adjacent backing.

**Key differentiator:** Code-generate controllers from the [Cloudflare OpenAPI spec](https://github.com/cloudflare/api-schemas), similar to how ACK generates from AWS service models.

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

### Reconciler Pattern (ACK-style)
Every controller follows:
```
Desired State (CRD spec) → Observed State (Cloudflare API) → Delta → Reconcile
```
With:
- `status.conditions` using standard Kubernetes condition types
- `status.ackResourceMetadata` equivalent: `status.cloudflareMetadata` (ID, zone ID, timestamps)
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
│   └── cloudflare-operator/ # Helm chart
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
**Duration:** 3–4 weeks  
**Status:** 🔲 Not started

**Goals:**
- [ ] Initialize repo with kubebuilder scaffolding
- [ ] Set up API group `cloudflare.k8s.io` with versioning strategy (`v1alpha1` → `v1beta1` → `v1`)
- [ ] Implement `CloudflareAccount` cluster-scoped credential controller
- [ ] Build OpenAPI-to-CRD generator skeleton (`generator/` package)
  - Parse `cloudflare/api-schemas` OpenAPI spec
  - Emit Go structs with `+kubebuilder` markers
  - Emit reconciler skeletons
- [ ] Implement `AdoptedResource` and `FieldExport` CRDs (port ACK pattern)
- [ ] Set up CI/CD: GitHub Actions for lint, unit test, CRD validation, e2e
- [ ] Makefile targets: `generate`, `manifests`, `install`, `run`, `test`, `helm-package`
- [ ] Base Helm chart with cert-manager webhook support

**Key files to establish early:**
- `pkg/reconciler/base.go` — shared reconciler interface all controllers embed
- `pkg/cloudflare/client.go` — thin wrapper around `cloudflare-go` SDK
- `apis/v1alpha1/groupversion_info.go`

---

### Phase 2 — Core Resource Controllers
**Duration:** 6–8 weeks  
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
- [ ] `status.conditions`: `ACK.ResourceSynced`, `ACK.Terminal`
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
**Duration:** 6–8 weeks  
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
**Duration:** 4–6 weeks  
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

## ACK Conventions to Follow

This project mirrors ACK patterns closely so contributors from the ACK community find it familiar:

1. **Condition types** — use `ACK.ResourceSynced` and `ACK.Terminal` condition types verbatim
2. **Status fields** — every CRD status has `conditions []metav1.Condition` and a `cloudflareMetadata` block
3. **References** — foreign-key relationships use `*Ref` fields (e.g., `zoneRef`, `tunnelRef`) not raw IDs
4. **AdoptedResource** — allows `kubectl annotate` import of pre-existing CF resources
5. **FieldExport** — `FieldExport` CR pipes `.status.*` fields into ConfigMaps for cross-namespace consumption
6. **Deletion policy** — annotation `cloudflare.k8s.io/deletion-policy: retain | delete` controls whether CF resource is deleted on CR deletion
7. **Terminal errors** — unrecoverable API errors (4xx, invalid config) set `ACK.Terminal=True` and stop requeuing

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
make docker-build docker-push IMG=ghcr.io/your-org/cloudflare-operator:latest

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

## ACK Reference (for pattern guidance)

- **ACK Runtime:** https://github.com/aws-controllers-k8s/runtime
- **ACK Code Generator:** https://github.com/aws-controllers-k8s/code-generator
- **ACK Developer Guide:** https://aws-controllers-k8s.github.io/community/docs/contributor-docs/overview/

---

## Current Status

> **Phase 1 in planning.** Repository not yet initialized.
> Last updated: March 2026
