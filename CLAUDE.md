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
Decided 2026-10-13. kflare-gen's design and as-built notes are in
[generator/CLAUDE.md](generator/CLAUDE.md#code-generator-kflare-gen); [Adding a kind](#adding-a-kind) is the recipe.
- `kflare-gen` reads a per-kind `generator.yaml`, the Go types of cloudflare-go v7 (the SDK Cloudflare generates from
  its OpenAPI spec) and a pinned subset of that spec (`api-schemas` repo). It generates CRD types and each kind's
  controller glue for a shared reconciler. It generates no client code: v7 is the client
- What the API description cannot express, such as behavior verified live, goes into the kind's entry: validations,
  CEL rules, ignores with their reasons, and test specs. Go hooks, for logic an entry cannot express, are planned but
  no kind has needed one yet
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
├── generator/             # kflare-gen: generator.yaml, SDK and OpenAPI readers, emitters (make codegen);
│                          #   its CLAUDE.md holds the generator's design
├── hack/                  # sync-helm-chart.sh, mutate.sh (mutations), e2e-live.sh (live e2e), license header
├── docs/deploy.md         # Deploy guide (Helm install, accounts, upgrade, uninstall, troubleshooting)
├── docs/design-notes.md   # What was decided and verified live, per phase and kind
├── ARCHITECTURE.md        # How the controllers behave: drift, adoption, errors, deletion, known limitations
├── CHANGELOG.md           # Release notes; a release's section is its GitHub release's notes
├── CLAUDE.md              # This file
└── Makefile
```
Planned, not yet present: the Docusaurus site under `docs/` (Phase 4).

---

## Operator Conventions

1. **Conditions** — every resource has a single `Ready` condition. Reasons: `Synced`/`Validated` (True);
   `TerminalError` (Cloudflare 4xx, not requeued until something changes); `APIError` (retryable, the error
   is returned so controller-runtime backs off); credential reasons from `credentials.go`
   (`AccountNotFound`, `AccountNotReady`, `SecretNotFound`, `TokenKeyMissing`, requeued after a minute)
2. **Status fields** — every CRD status has `conditions []metav1.Condition` and a `cloudflareMetadata` block
3. **References** — foreign-key relationships use `*Ref` fields (e.g., `zoneRef`, `tunnelRef`) not raw IDs
4. **Adoption** — a resource creates its Cloudflare object, and takes over an existing one with its key only when its
   `kflare.dev/adoption-policy` annotation says `adopt` or `adopt-or-create` (`internal/controller/adoption.go`:
   `mayAdopt`, `mayCreate`; otherwise `AlreadyExists` or `NotFound`). kflare marks an adopting resource
   `kflare.dev/adopted`, and records a key in `kflare.dev/creating` before each create (`markCreating`), so a reconcile
   that lost the record of a create finds the object as its own
5. **FieldExport** (planned, Phase 4) — `FieldExport` CR pipes `.status.*` fields into ConfigMaps for cross-namespace consumption
6. **Deletion policy** — annotation `kflare.dev/deletion-policy: retain | delete` controls whether the
   Cloudflare resource is deleted on CR deletion. Without it, a created object is deleted and an adopted one kept;
   check it with `reconciler.RetainOnDelete`
7. **Immutability** — fields that identify the Cloudflare resource (names, refs) are immutable via CEL
   `self == oldSelf`, with an envtest case per field
8. **Status helpers** — set conditions and report errors through `internal/controller/status.go`
   (`updateReady`, `updateNotReady`, `handleCloudflareError`, `notReadyRetryAfter`); API types implement
   `GetConditions/SetConditions`
9. **Prefix** — every finalizer, annotation and label kflare owns starts with `kflare.dev/`
10. **ARCHITECTURE.md** — a new controller, or a change to drift detection, adoption, error handling or deletion,
    updates the matching section and tables in `ARCHITECTURE.md`

---

## Development workflow

- Every new controller needs: unit tests, a live e2e spec, docs, and a mutation-testing run where all mutations are
  caught.
- Keep doc and markdown lines at 120 characters or fewer.
- Before pushing, run tests, the linter and codegen checks. Merge main and resolve conflicts if the branch is behind.
- Keep tooling such as the mutation runner in the repo (e.g. `hack/`), not in the scratchpad.
- Verify token-permission claims against live specs, not old notes.
- A new kind follows [Adding a kind](#adding-a-kind).

---

## Adding a kind

New kinds are generated by kflare-gen ([generator/CLAUDE.md](generator/CLAUDE.md)), unless they are mostly behavior
an entry cannot express (the kinds that stay hand-written are listed there). One kind per PR, off `origin/main`, in
its own worktree. Before changing an existing kind, read its notes in [docs/design-notes.md](docs/design-notes.md):
what was decided and verified live, and why.

1. **Check the shape.** kflare-gen generates two:
   - CRUD kinds (`kinds:` in `generator/generator.yaml`, run by `crudresource_controller.go`): an account-level v7
     service with `New`, `Get`, a `List` that pages with `V4PagePaginationArray`, `Delete`, and optionally `Update` or
     `Edit`, its objects identified by an ID. Closest examples: `D1Database`, `KVNamespace`
   - Rule kinds (`ruleKinds:`, run by `rulesetrule_controller.go`): one rule in a zone's Rulesets phase, typed by v7's
     `rulesets.<Action>RuleParam`. Closest examples: `OriginRule` (small), `CacheRule` (CEL rules and `tests`)

   Anything else needs a kflare-gen capability first ([Next wave](#next-wave-proposed-2026-10-17) lists them). A
   draft entry is the quickest check: `make codegen` refuses what it cannot generate, and says why.
2. **A new capability is its own PR, merged before the kind's.** Build it on a test kind in
   `generator/testdata/generator.yaml`, with golden tests. The kind's PR is then its entry, the generated output, its
   live e2e spec and docs. A kflare-gen fix the kind turns up can go in it; new mapping or runtime code cannot.
3. **Check the token permission first.** The OpenAPI operation's `x-api-token-group` names it, but is not always right
   (for managed transforms it names one that is refused). Try it with `CF_API_TOKEN`: a `?dry_run=true` write for a
   rule kind, a throwaway `kflare-probe-*` object otherwise. Adding a permission to the token needs the user's OK.
4. **Write the entry** from the closest example, then run `make sync-spec` (copies the kind's operations and schemas
   into `generator/openapi`; downloads 27 MB) and `make generate manifests helm`. Fields are opt-out: the kind gets
   every field v7 has unless the entry ignores it, with a reason. The five Rules products' phases all refuse
   `logging` (20018), `ratelimit` (20131) and `exposed_credential_check` (20189), so a new rule kind can ignore those
   up front and confirm it with one dry run.
5. **Probe the live API, then narrow the entry.** Try each field, and each variant of a union, on the Free zone in
   `CF_E2E_ZONE`: rule kinds with `?dry_run=true` (the same authorization, plan and quota checks as a write, but
   nothing is stored), CRUD kinds with `kflare-probe-*` objects, deleted afterwards. Only create or delete objects
   named `kflare-probe-*` or `kflare-e2e-*`: the account holds others.
   - A field Free refuses ("not entitled") or the phase refuses: ignore it, with the error code and date as reason
   - A constraint of the spec that the live API contradicts: replace it (`fields.<path>.validation`), with the reason
   - A refusal the schema does not describe: a CEL rule (`rules`), with its reason and an `invalid` spec
   - Dry-run every expression a sample or the e2e spec uses: one naming no real field fails the whole live run
     (`cf.colo.name`, 20087)
6. **Tests.** kflare-gen writes the kind's envtest table, which the shared specs run (`crudresource_specs_test.go`,
   `rulesetrule_specs_test.go`). Written by hand:
   - The live e2e spec, in `test/e2e/cloudflare_test.go`. A rule kind's follows OriginRule's: a phase constant, a key,
     a `cleanupRules` line in the suite's `DeferCleanup`, and a spec that writes the sample's rule, checks the JSON
     Cloudflare stores, changes a field in place and expects an idle reconcile (`expectIdle`). Keep the suite within
     the Free limits ([docs/design-notes.md](docs/design-notes.md#deferred-paid-plan-add-on-or-account-setup)), and
     never run two live runs at once
   - The mutation check: `MUTATE_FOCUS=<Kind> hack/mutate.sh <mutations.json>`, until every mutation is caught. The
     rule kinds so far took 21 to 40, of: the generated conversion (each field), the phase and action, the kind's name
     and registration, the CRD's bounds, patterns, enums, required fields and CEL rules, and any kflare-gen code the
     PR changed. A survivor is a gap in the generated specs: close it in kflare-gen, for every kind
   - The live run: `hack/e2e-live.sh` runs the manager on this machine against the current kind cluster, runs the
     suite against it and leaves the cluster as it found it (its header says how). Afterwards, check that Cloudflare
     holds nothing the run made, and delete the empty entry points rule specs leave in the zone's phases (issue #57).
     Once the repo has its CI secrets (Release and CI below), a `/live-e2e` review comment on the pull request
     (`gh pr review <number> --comment -b /live-e2e`) runs the same suite in GitHub Actions instead. Never run both at
     once: the script's lock does not see a CI run
7. **Docs.** Every place that lists the kinds names the last one of the same shape, so
   `git grep -n OriginRule -- '*.md'` (or `D1Database`) finds them: README's kinds, deploy.md's permission row (the
   label Cloudflare's docs give it), kinds table and uninstall command, ARCHITECTURE.md's ownership tree, drift and
   deletion lists, plus a section on the kind's behavior (convention 10), and Current Status below. In
   [docs/design-notes.md](docs/design-notes.md), add the kind's section (what was verified live: refusals and their
   codes, quota, what Cloudflare stores, the permission) and its rows in Phase 3's table and Token permissions.
8. **Before the PR:** `go fix -diff ./...` prints nothing, `make lint` passes, and `make generate manifests helm`
   leaves no diff (CI checks all three).

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

# go fix (make test runs it too)
make fix

# Lint (also run in CI)
make lint

# Pinned kind (v0.30+ needed for 1.34 node images); local/setup.sh uses it
make kind

# e2e: deploys to the kind cluster in the current context ($KIND_CLUSTER, default "kind");
# runs the live Cloudflare specs when CF_API_TOKEN/CF_ACCOUNT_ID (and CF_E2E_ZONE) are set
make test-e2e

# The same suite with the manager on this machine, against the current kind cluster (see the script's header)
hack/e2e-live.sh

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

## Plans in progress

### Launch readiness
**Status:** 🚧 In progress: L1 (but for one rerun) to L4 are done; L5 is under way in `docs/release-v0.1.0`.
Goal: announce kflare on Reddit (r/kubernetes first, then r/CloudFlare) as a v0.1 alpha, to find its first users. The
audience, as the user put it on 2026-10-21: companies running Kubernetes, so the docs lead with Helm and GitOps
installs and with keeping kflare easy to maintain. kflare is a standalone project: the user docs do not compare it
with ACK, and ARCHITECTURE.md credits ACK once, for the adoption and deletion annotations

**Where it stands** (checked 2026-10-19 against main, `7005841`):
- No tag and no GitHub release, so the release workflow has never run: no image or chart is published, and the
  README's `helm install ... --version <version>` cannot work yet
- Adoption is implicit and adopted objects are deleted with their resource by default (ARCHITECTURE.md, "Adoption and
  ownership"). Someone trying kflare on their real account can delete a zone, a record or a database they already had
- Exposing a Service through a Tunnel, the first thing most people will try, needs the tunnel's ID copied by hand from
  status into a DNSRecord (the TunnelConfiguration sample says so), against convention 3, and a cloudflared Deployment
  that nothing in the repo provides
- The README is written for contributors (kind and `make run`), opens with a 15-item feature list, and links this
  file as the roadmap. This file also states operational details about the maintainer's own Cloudflare account
- The repo page: 1 star, no topics, no website, no CONTRIBUTING.md. A stray `.golangci.bck.yml` sits at the root
- The engineering is not the gap: 19 resource kinds, live e2e against a real account, mutation testing, a Helm chart
  with a deploy guide and least-privilege token permissions verified live

**Order:** L1 to L5 come before the announcement, in order; L6 and L7 go with it. L2, L3 and L4 can be built side by
side but merge one at a time (no stacked PRs). L5 waits for all of them.

**Decisions for the user** (answered 2026-10-19, each as proposed):
1. L1: OK to create a short-lived token with deploy.md's permissions for one live run: yes
2. L2: whether adopted objects are kept on deletion by default (proposed) or deleted, as ACK does: kept
3. L3: before the announcement (proposed), or after it, with the quickstart showing the copy step meanwhile: before
4. L4: where the operational details removed from this file are kept (private notes): the maintainer's private notes

#### L1. Finish the live checks
**Status:** ✅ 2026-10-19 but for a rerun of the least-privilege run. No branch unless a run fails: one did, so
`fix/zone-watches-generated-rule-kinds` carries the fix and these notes.
- [x] Run OriginRule's live e2e spec against main, its last open item: run `2748f8`, 20 of 20 with the dev token, no
  error in the manager's log but optimistic-concurrency conflicts, which are retried
- [x] Run the whole suite with a short-lived token holding only deploy.md's permissions and Zone Read, as in
  [Token permissions](docs/design-notes.md#token-permissions-for-phase-3), so the `ConfigurationRule` and
  `OriginRule` rows are checked on their own too.
  The token needs the user's OK, and is deleted after the run: run `e79693` (OK given 2026-10-19,
  token deleted), 19 of 20, with no refusal. The all-at-once deletion spec failed: the Zone stayed, reporting `InUse`
  for a TransformRule that was already gone. The Zone controller watched only the hand-written kinds a Zone waits for,
  so nothing re-triggered it when a generated rule kind went last; run `2748f8` passed only because a hand-written
  dependent happened to go last. `SetupWithManager` now watches every kind in `zoneDependents`, and the generated rule
  kinds' shared specs run the Zone controller in a manager and check that deleting the rule releases its Zone (they
  fail without the fix). With the fix, run `f1f17f` passed, 20 of 20, with the dev token
- [x] Record the run IDs here and drop deploy.md's caveat ("before `ConfigurationRule` and `OriginRule` were added")
- [ ] Optional, needs the user's OK for another short-lived token: rerun the least-privilege suite with the fix, so
  that its deletion spec passes too
- Done when: both runs pass with no refusal in the manager's log

#### L2. Adoption on opt-in, ACK style (`feat/adoption-policy`)
**Status:** ✅ Merged 2026-10-20 (#53). Conventions 4 and 6 state the rules; the design and as-built notes are in
[docs/design-notes.md](docs/design-notes.md#l2-adoption-on-opt-in-ack-style-featadoption-policy).

#### L3. A DNSRecord points at a Tunnel by reference (`feat/dnsrecord-tunnel-ref`)
**Status:** ✅ Merged 2026-10-21 (#60), and the first live CI run passed (GitHub run 39530820711, e2e run `f7dccb`,
24 of 24). The design and as-built notes are in
[docs/design-notes.md](docs/design-notes.md#l3-a-dnsrecord-points-at-a-tunnel-by-reference-featdnsrecord-tunnel-ref).

#### L4. Launch docs (`docs/launch`)
**Status:** ✅ Merged 2026-10-21 (#61). Its done-when is checked by L5's quickstart run, since the README installs the
released chart (`--version 0.1.0`) and applies the example from the `v0.1.0` tag.
- [x] README, rewritten for someone arriving from Reddit:
  - What kflare is, in two sentences, and its status: alpha, API `v1alpha1`, Kubernetes 1.34+
  - A quickstart: install the released chart, connect an account, run `examples/expose-a-service/`
  - The resource kinds as a short table, each linking its sample and naming its token permission
  - "Why kflare", one paragraph, fair to the alternatives: adyanth/cloudflare-operator for tunnels and DNS,
    external-dns for DNS, Terraform or Crossplane for everything. kflare is Kubernetes-native and also covers Workers,
    storage, WAF and the Rules products
  - "How it's built", stated plainly rather than left for commenters to find: developed with AI assistance (Claude
    Code), and kept honest by the live e2e suite against a real Cloudflare account, mutation testing on every
    controller, and a record of what was verified live
  - Links to ROADMAP.md, ARCHITECTURE.md and docs/deploy.md; contributor setup moves to CONTRIBUTING.md
- [x] ROADMAP.md, short and written for users: done, next (the [Next wave](#next-wave-proposed-2026-10-17)), and
  deferred with the reason (paid plans, Zero Trust setup). The README links it instead of this file
- [x] CONTRIBUTING.md (build, test, `make codegen`, the live e2e and the token it needs, branch and PR conventions),
  a bug report template (kflare and Kubernetes versions, the resource's status and conditions), and 3 to 5 good first
  issues from the Next wave and the known limitations (#54 to #57, created 2026-10-20)
- [x] Take out of this file the three operational details about the maintainer's own account: the dev token's extra
  permissions (Release and CI), a database on the dev account that is not kflare's (D1Database notes) and the
  domain's renewal setting (Release and CI). None is a secret, so git history stays as it is
- [x] Delete `.golangci.bck.yml` (nothing references it) and add a CI badge
- [x] Added for the audience above: deploy.md's "With Argo CD or Flux" (an `Application` and an
  `OCIRepository`/`HelmRelease`, CRD ownership, sync order) and "Running in production" (monitoring with the
  controller-runtime metrics and the `Ready` condition, egress and proxies, `retain` for objects that must outlive
  their manifests, moving to a new cluster). deploy.md's kind section and the chart README's maintenance notes moved to
  CONTRIBUTING.md; the chart README, written for ArtifactHub, links with absolute URLs. Not run: the Argo CD and Flux
  manifests, the proxy settings and the cluster move; deploy.md says the move is untested
- Done when: someone who has never seen the repo gets from the README to a working tunnel without opening another
  file, and the README links no file written for agents

#### L5. Release v0.1.0 (`docs/release-v0.1.0`)
**Status:** 🚧 Started 2026-10-21. The branch adds CHANGELOG.md, whose v0.1.0 section is the release's notes. Checked
before tagging: the Dockerfile cross-compiles (`--platform=$BUILDPLATFORM`, no `RUN` in the final stage), so the
release workflow's `linux/arm64` build needs no QEMU (a local arm64 build passed); `helm package` gives
`kflare-0.1.0.tgz` with the 19 CRDs and `ghcr.io/jonathangraniero/kflare:0.1.0`, the tag the workflow pushes. Pushing
the tag, making the packages public (GitHub cannot make a public package private again) and the live quickstart run
each need the user's OK.
- [ ] Once L2, L3 and L4 are merged: tag `v0.1.0` (Chart.yaml is at 0.1.0) and check the release workflow is green
- [ ] Both packages on GHCR (`ghcr.io/jonathangraniero/kflare` and `charts/kflare`) are public: the image pull and
  `helm install` work from a machine not logged in to ghcr.io
- [ ] Run the README's quickstart verbatim on a new kind cluster against a real account, then upgrade and uninstall as
  deploy.md describes
- [ ] A GitHub release with notes: the resource kinds, the alpha status, adoption and deletion (L2), and the known
  limitations from ARCHITECTURE.md
- Done when: the quickstart works on a clean machine with nothing but the README

#### L6. Repository settings
In GitHub's settings, no branch.
- [ ] About: a description that says what kflare manages, topics (`kubernetes`, `kubernetes-operator`, `cloudflare`,
  `gitops`, `crd`, `cloudflare-tunnel`, `dns`, `waf`), and a website once `kflare.dev` serves one
- [ ] Discussions on, so questions from the announcement do not land in issues (optional)
- [ ] The chart listed on ArtifactHub (a Phase 4 item, cheap once v0.1.0 exists)

#### L7. Announcement
- [ ] r/kubernetes first, r/CloudFlare a few days later. r/selfhosted asks that promoted projects be production-ready
  with docs (its rule 2, checked 2026-10-19), so wait for a later release there
- [ ] Frame it as a v0.1 alpha looking for early users, not as production-grade. Ask for specific feedback: which
  resources people need next, and how adoption and deletion should behave
- [ ] Lead with the quickstart and what kflare covers that the alternatives do not; mention the AI assistance and the
  verification behind it in the post itself
- [ ] Be around for the first hours to answer comments, and file issues for the bugs people report

**After the announcement**, not blocking it: the drift correction period (Known limitations). The live e2e suite has
run in GitHub Actions since 2026-10-21 (Release and CI below).

#### Chart hardening for v0.1.1 (`feat/chart-hardening`)
**Status:** 🔲 Decided 2026-10-21, after v0.1.0 is tagged: the first release after it. One PR off main. Today the chart
has no PodDisruptionBudget or NetworkPolicy, and `--metrics-secure` turns on TLS only: the manager sets no
`FilterProvider`, so nothing authenticates or authorizes a scrape.
- [ ] Metrics over HTTPS with authentication and authorization **on by default** (the user's choice): the manager's
  `FilterProvider` is controller-runtime's `filters.WithAuthenticationAndAuthorization`, the controller's ClusterRole
  gains `tokenreviews` and `subjectaccessreviews` create, the chart adds a `metrics-reader` ClusterRole
  (`nonResourceURLs: /metrics`, get) for Prometheus to bind, and the ServiceMonitor scrapes over HTTPS with its
  ServiceAccount token. A value turns it back to plain HTTP. It changes how v0.1.0 is scraped, so the release notes
  say so
- [ ] PodDisruptionBudget, `maxUnavailable: 1`, created by default only when `replicaCount` > 1 (the user's choice), so
  a single replica never blocks a node drain; a value turns it off
- [ ] NetworkPolicy, opt-in (default off: the API server's address varies by cluster): ingress to the metrics port from
  selected namespaces, egress to DNS, the API server and port 443
- [ ] values.yaml, values.schema.json, the chart README and deploy.md (Security model, Monitoring, Network access);
  `make helm-lint`, and a kind install that scrapes `/metrics` with and without a bound token
- Done when: a default install answers `/metrics` only over HTTPS to a token bound to the metrics-reader role, and a
  two-replica install gets a PDB that a node drain respects

### Next wave (proposed 2026-10-17)
**Status:** 🔲 Proposed, not decided: the user picks what comes after OriginRule, and in what order

Phase 3's table ([docs/design-notes.md](docs/design-notes.md#phase-3--expanded-api-surface)) ends with the Rules
kinds. Below is what else this account can build and live-test, checked on
2026-10-17. The probes created a `kflare-probe-*` queue, Vectorize index and Turnstile widget and deleted each. Each
row also says what kflare-gen needs before it can generate the kind.

| Candidate | What it manages | Free, on this account | Dev token | What kflare-gen needs first |
|---|---|---|---|---|
| `ManagedTransform` (planned above) | the zone's managed request and response headers | ✔ all six, read live | ❌ needs Managed headers Write (ask) | a runtime for settings-style singletons |
| Zone settings (one kind, or one per group of settings) | SSL mode, Always Use HTTPS, minimum TLS version, HSTS, and the other settings a zone has | most settings (to check one by one) | ❌ needs Zone Settings Write (ask) | the singleton runtime |
| `Queue` | a Workers queue (consumers later) | ✔ probe queue created and deleted | ✔ Queues Write | `SinglePage` lists: the shared CRUD reconciler pages only `V4PagePaginationArray` |
| `VectorizeIndex` | a Vectorize index | ✔ probe index (dimensions 32 to 1,536) | ✔ Vectorize Write | objects identified by name (no ID, like R2 buckets), `SinglePage` lists |
| `TurnstileWidget` | a Turnstile widget | ✔ probe widget | ✔ Turnstile Sites Write | lists of strings in CRUD kinds (domains); the widget's secret written to a Kubernetes Secret; adoption without unique names |
| `WorkerDomain` | a Worker on a custom domain, next to `WorkerRoute` | not probed: needs a hostname in the zone | ✔ Workers Scripts Write | a service without `New` (a PUT attaches), `SinglePage` lists |
| `HyperdriveConfig` | a Hyperdrive connection to a database | not probed: a create checks that the database answers | ✔ Hyperdrive Write | an input secret (the password, from a Secret) |
| Bulk Redirects (`RedirectList` and a rule kind) | account-level redirects from lists | per the docs: 15 rules, 5 lists, 10,000 redirects | ✔ Mass URL Redirects Write; the list permission to check | account-level phases in the Rulesets foundation; a redirect list like `IPList` |
| `ManagedRulesetException` (planned above) | skip rules in the managed phase | ✔ skip rules accepted (2026-10-14) | ✔ Zone WAF Write | a rank per kind in the foundation's rule ordering |

Not on this table: Zero Trust (waits on enabling Access, still off on 2026-10-17), load balancing (a paid add-on),
and products Free zones lack (custom error pages, Page Shield, Logpush).

kflare-gen capabilities, in the order the candidates above need them:
1. **A runtime for settings-style singletons** (v7 services with `Get` and `Update` or `Edit`, without `New` or
   `Delete`, 90 of them): one resource owns the configuration, the oldest per zone, as with TunnelConfiguration; the
   others report `AlreadyConfigured`; deletion resets the settings unless retain. ManagedTransform, zone settings, URL
   normalization and DNSSEC have this shape (DNSSEC only on a zone that is not the e2e zone)
2. **`SinglePage` lists and objects identified by name** in the shared CRUD reconciler: queues, Vectorize indexes,
   Workers custom domains
3. **Secrets:** an `x-sensitive` field written to a Kubernetes Secret (the Turnstile secret), or read from one (the
   Hyperdrive password)
4. **Services without `New`** (a PUT that creates or replaces), and **zone-scoped CRUD kinds** (the shared CRUD
   reconciler handles account-scoped kinds only)
5. For rule kinds, when a plan that can use them can also check them: maps of lists (a cache key's `header.contains`)
   and union variants told apart by a number or a bool

Cross-cutting, from ARCHITECTURE.md's known limitations:
- **Drift correction period:** out-of-band changes wait for the next trigger, at worst the 10-hour resync. A
  configurable requeue after a successful sync would bound it, within Cloudflare's rate limits
- **R2Bucket and Tunnel onto v7:** jurisdictions and storage class for buckets, renames for tunnels (both blocked on
  v0.119), as SDK decision step 3 foresees

### Release and CI
**Status:** 🔲 Open, alongside Phase 3

- [x] Run the live e2e suite in GitHub Actions. The workflow is ready since L3 (`feat/dnsrecord-tunnel-ref`): a
  review comment starting with `/live-e2e`, by the owner, a member or a collaborator, runs it on the pull request as
  reviewed (branches of this repo only: a fork gets no secrets), one run at a time and never cancelled halfway. The
  secrets `CF_API_TOKEN` (a dedicated CI token holding deploy.md's permissions with Zone Read, see
  [Token permissions](docs/design-notes.md#token-permissions-for-phase-3)) and `CF_ACCOUNT_ID`, and the variable
  `CF_E2E_ZONE=kflare.dev`, were set on 2026-10-21. First run, by hand on main: GitHub run 39530820711, e2e run
  `f7dccb`, 24 of 24 passed. A Free zone holds one rate limiting rule and a Free account one IP list, so local runs
  must not overlap with a CI run
- [ ] Tag `v0.1.0`: L5 of [Launch readiness](#launch-readiness). The release workflow publishes the image and chart
- [ ] Open question: a Terraform `infra/` root for `kflare.dev` (zone, CI token, GitHub secrets)

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

## Current Status

> **Phases 1 and 2 complete. Phase 3 in progress: `KVNamespace`, `R2Bucket`, `WAFCustomRule`, `RateLimitRule`, `IPList`,
> `ManagedRuleset`, `D1Database`, `TransformRule`, `RedirectRule`, `CacheRule`, `ConfigurationRule` and `OriginRule`
> done.**
> cloudflare-go is at v0.119, with v7.12.0 next to it for the generated kinds and the rule kinds; rule kinds share the
> Rulesets foundation and controller. The code generator, `kflare-gen`, is complete: KVNamespace and D1Database are
> generated in full on the shared CRUD reconciler, and TransformRule, RedirectRule, CacheRule, ConfigurationRule and
> OriginRule on the shared rule controller. Next: [Launch readiness](#launch-readiness) (Phase 3), L1 to L7 in order;
> L1 is done but for one rerun, L2 to L4 are merged, L5 is under way. The proposed
> [Next wave](#next-wave-proposed-2026-10-17) comes after the announcement.
> deploy.md's permissions are verified on their own for every kind. Zero Trust still waits on account setup. Items
> needing a paid plan, add-on or account setup are tracked under
> [Deferred](docs/design-notes.md#deferred-paid-plan-add-on-or-account-setup) in docs/design-notes.md.
> Last updated: October 2026
