# Changelog

kflare follows [semantic versioning](https://semver.org). While its API is `v1alpha1`, any release can change it:
read the notes before upgrading. The chart and the controller image share one version.

## Unreleased

Every kind written before kflare's code generator is moving onto it, to be released together as v0.3.0. A generated
kind's field names are Cloudflare's API's, as the generated kinds' already are, so manifests written for v0.2 change.
Each kind's changes are below.

### `ManagedRuleset`

- **`rulesetID` is now `id`**, the API's name for the ruleset an execute rule runs. A ManagedRuleset stored with
  `rulesetID` has no `id` after the upgrade, and cannot be updated until it gets one: apply your ManagedRuleset
  manifests with `id` right after upgrading
- `id` can change in place, which updates the deployment's rule to run the other ruleset; it was immutable
- The category and rule overrides are sent in the spec's order rather than sorted. Cloudflare keeps that order, so
  reordering them in the spec writes the rule again
- An empty `overrides.categories` or `overrides.rules` is refused: leave it out
- For Go clients: `ManagedRulesetCategoryOverride` and `ManagedRulesetRuleOverride` are now
  `ManagedRulesetOverridesCategory` and `ManagedRulesetOverridesRule`, and an override's `action` is a `string`

Before:

```yaml
spec:
  zoneRef:
    name: example-zone
  rulesetID: 77454fe2d30c4220b5701f6fdfb893ba
  expression: "true"
  overrides:
    categories:
      - category: wordpress
        action: log
```

After:

```yaml
spec:
  zoneRef:
    name: example-zone
  id: 77454fe2d30c4220b5701f6fdfb893ba
  expression: "true"
  overrides:
    categories:
      - category: wordpress
        action: log
```

## v0.2.1 (alpha)

The third alpha release of kflare: the API is still `v1alpha1`, and it needs Kubernetes 1.34 or newer. It is the first
signed release, with SBOMs and provenance for the image, and any resource can now be paused with an annotation.

```sh
helm upgrade kflare oci://ghcr.io/jonathangraniero/charts/kflare --version 0.2.1 -n kflare-system \
  --values kflare-values.yaml
```

The image is `ghcr.io/jonathangraniero/kflare:0.2.1`, for `linux/amd64` and `linux/arm64`. The deploy guide's
[Verifying a release](docs/deploy.md#verifying-a-release) checks both signatures before you install. To install kflare
for the first time, follow the [README's quickstart](README.md#quickstart-expose-a-service-through-a-cloudflare-tunnel).

### Upgrading from v0.2.0

Nothing to change: the CRDs, the controller's permissions and the chart's defaults are v0.2.0's.

### Releases

- **The controller image and the chart are signed** with cosign, keyless: Sigstore issues the signing certificate to
  kflare's release workflow, for the release's tag, and records the signature in its public transparency log. Each
  platform's image comes with an SPDX SBOM and SLSA provenance, stored in the signed image index, and the GitHub
  release has the SBOMs as files, each with a Sigstore bundle. The deploy guide's
  [Verifying a release](docs/deploy.md#verifying-a-release) has the commands, and a Kyverno policy that admits only
  signed kflare images ([#84](https://github.com/JonathanGraniero/kflare/issues/84))

### The Helm chart

- **`image.digest`** runs the controller image by digest instead of by tag: set it to the digest whose signature you
  verified

### The controller

- **Pausing a resource**: with the annotation `kflare.dev/paused: "true"`, kflare leaves the resource and its Cloudflare
  object alone, neither reverting changes made in Cloudflare nor applying changes to the spec, until the annotation is
  removed. A paused resource being deleted keeps its finalizer until then. `Ready` keeps its status and takes the
  reason `Paused` ([#81](https://github.com/JonathanGraniero/kflare/issues/81))

### Known limitations

v0.2.0's, unchanged: [ARCHITECTURE.md](ARCHITECTURE.md#known-limitations) explains each.

## v0.2.0 (alpha)

The second alpha release of kflare: the API is still `v1alpha1`, and it needs Kubernetes 1.34 or newer. It hardens
the Helm chart for production ([#67](https://github.com/JonathanGraniero/kflare/issues/67)), bounds every call to
Cloudflare with timeouts, can check every resource for drift on a schedule, records Kubernetes Events, and adds a
new kind, `ManagedTransform`. v0.1.0's notes announced the chart hardening as v0.1.1; it ships as a minor release
instead, because upgrading changes how Prometheus scrapes kflare (see below).

```sh
helm upgrade kflare oci://ghcr.io/jonathangraniero/charts/kflare --version 0.2.0 -n kflare-system \
  --values kflare-values.yaml
```

The image is `ghcr.io/jonathangraniero/kflare:0.2.0`, for `linux/amd64` and `linux/arm64`. To install kflare for
the first time, follow the [README's quickstart](README.md#quickstart-expose-a-service-through-a-cloudflare-tunnel).

### Upgrading from v0.1.0

**Metrics are served over HTTPS, and only to callers allowed to read them.** A scrape set up for v0.1.0 fails with
401 or 403 until it changes, on the same port (8080):

- **With the chart's ServiceMonitor**, nothing changes in your values: it scrapes over HTTPS with Prometheus' own
  ServiceAccount token. kube-prometheus-stack's Prometheus is already allowed; any other needs its ServiceAccount
  bound to the `kflare-metrics-reader` ClusterRole (the deploy guide's
  [Monitoring](docs/deploy.md#monitoring) has the binding)
- **Any other scraper** must use HTTPS, send a bearer token whose user is bound to `kflare-metrics-reader`, and either
  skip verifying the self-signed certificate or trust one from `metrics.certSecret`
- **To keep v0.1.0's plain HTTP**, set `metrics.secure: false`

**If you manage the CRDs yourself** (`crds.enabled=false`), apply `config/crd/bases` from v0.2.0 before upgrading
the chart: it adds the `ManagedTransform` CRD, which the controller watches, and puts every CRD in the `kflare`
category.

### The Helm chart

- **Metrics over HTTPS with authentication and authorization**, on by default (`metrics.secure`). A request needs a
  bearer token that the API server authenticates and authorizes for `get` on `/metrics`, which the new
  `kflare-metrics-reader` ClusterRole grants. The controller's ClusterRole may create TokenReviews and
  SubjectAccessReviews to check them. `metrics.certSecret` serves a certificate from a Secret, such as one
  cert-manager issues, instead of a self-signed one, and the ServiceMonitor then verifies it
- **A PodDisruptionBudget** with `maxUnavailable: 1` when `replicaCount` is above 1, so that a node drain evicts one
  controller pod at a time (`podDisruptionBudget`). A single replica gets none, so it never blocks a drain
- **An opt-in NetworkPolicy** (`networkPolicy`): scrapes of the metrics port in, from the sources you select; DNS, the
  Kubernetes API server and port 443 (Cloudflare) out

### The controller

- `--metrics-secure` also authenticates and authorizes each request now, with a TokenReview and a SubjectAccessReview;
  an allowed token is trusted for a minute. Before, it only turned on TLS
- `--metrics-cert-path` serves the metrics certificate and key from a directory (`tls.crt`, `tls.key`), reloaded
  when they change
- `--default-deletion-policy`, the chart's `defaultDeletionPolicy`: `retain` keeps the Cloudflare objects of deleted
  resources, unless a resource says `kflare.dev/deletion-policy: delete`. The default, `delete`, keeps v0.1.0's
  behavior ([#80](https://github.com/JonathanGraniero/kflare/issues/80))
- Every change of a resource's Ready condition to another status or reason is recorded as an Event, `Normal` when
  it turns True and `Warning` when it turns False, so `kubectl describe` shows the resource's history. The
  controller's ClusterRole may create and patch `events.k8s.io` Events
  ([#56](https://github.com/JonathanGraniero/kflare/issues/56))
- **Timeouts on every call to Cloudflare**, so that one that gets no answer cannot stall a controller: 10 seconds for
  a TLS handshake, a minute for a response to start, two for a whole request, and an HTTP/2 connection that goes 30
  seconds without a word is pinged and closed if the ping gets no answer. Before, a call could wait forever
  ([#72](https://github.com/JonathanGraniero/kflare/issues/72))
- `--reconcile-timeout`, the chart's `reconcileTimeout` (5 minutes by default): a reconcile that runs longer ends with
  reason `APIError` and is retried with back-off, and its controller goes on to the next resource
- `--drift-check-interval`, the chart's `driftCheckInterval`, off by default: every resource is checked against
  Cloudflare that long after each successful reconcile, give or take a tenth, so that a change made outside kflare is
  corrected within about the interval instead of at the 10-hour resync. The deploy guide says how to pick one for the
  number of resources ([#55](https://github.com/JonathanGraniero/kflare/issues/55))

### Resource kinds

- **`ManagedTransform`**: a zone's managed transforms, the request and response headers Cloudflare adds or removes,
  turned on and off by ID. One ManagedTransform configures a zone, and deleting it turns them all off unless it
  adopted them. It needs Zone · Managed Headers · Edit
- **Every kind is in the `kflare` category**, so `kubectl get kflare` lists the resources of all of them, in one
  namespace (`-n`) or in all (`-A`) ([#54](https://github.com/JonathanGraniero/kflare/issues/54))

### Known limitations

Those of v0.1.0, except the two this release fixes (no Kubernetes Events, and metrics over plain HTTP).
[ARCHITECTURE.md](ARCHITECTURE.md#known-limitations) explains each:

- **Drift is corrected promptly only with a drift check interval** (`driftCheckInterval`). Without one, the default,
  a change made outside kflare waits for the next reconcile, at worst the 10-hour resync
- **Adoption goes by key** (a name, a title, a pattern), never by ID. Tunnel names are not unique: a Tunnel adopts the
  first one listed with its name
- **Ownership is recorded in the cluster**, except for rules, which carry a ref in Cloudflare. A cluster rebuilt from
  git sees its existing objects as `AlreadyExists` until they are adopted; the deploy guide's
  [Moving to a new cluster](docs/deploy.md#moving-to-a-new-cluster) describes how
- **Rule order covers kflare's rules only.** Rules created outside kflare keep their place
- **Some limits of cloudflare-go v0.119**, which most controllers still use (the generated kinds and the rule kinds
  use v7): Tunnels cannot be renamed, `originRequest` timeouts are not exposed, and R2 buckets live in the default
  jurisdiction only

## v0.1.0 (alpha)

The first release of kflare, a Kubernetes operator that manages Cloudflare through custom resources. It is an alpha:
the API is `v1alpha1`, and fields can still change between releases. It needs Kubernetes 1.34 or newer.

```sh
helm install kflare oci://ghcr.io/jonathangraniero/charts/kflare \
  --version 0.1.0 --namespace kflare-system --create-namespace
```

The image is `ghcr.io/jonathangraniero/kflare:0.1.0`, for `linux/amd64` and `linux/arm64`. Start with the
[README's quickstart](README.md#quickstart-expose-a-service-through-a-cloudflare-tunnel), and the
[deploy guide](docs/deploy.md) for installing with Argo CD or Flux and running kflare in production.

### Resource kinds

| Area | Kinds |
|---|---|
| Accounts | `CloudflareAccount` (cluster-scoped; a token Secret per account) |
| DNS | `Zone`, `DNSRecord` (`tunnelRef` points a hostname at a `Tunnel`) |
| Tunnels | `Tunnel` (its token written to a Secret for `cloudflared`), `TunnelConfiguration` |
| Workers and storage | `WorkerScript`, `WorkerRoute`, `KVNamespace`, `R2Bucket`, `D1Database` |
| Security | `WAFCustomRule`, `RateLimitRule`, `ManagedRuleset`, `IPList` |
| Rules | `TransformRule`, `RedirectRule`, `CacheRule`, `ConfigurationRule`, `OriginRule` |

Every kind is tested live against a real Cloudflare account on the Free plan, with a token holding only the
permissions the deploy guide lists.

### Adoption and deletion

- kflare creates the Cloudflare objects its resources describe, and **never takes over an existing object** unless the
  resource is annotated `kflare.dev/adoption-policy: adopt` (adopt it, never create one) or `adopt-or-create`.
  Otherwise the resource reports `AlreadyExists` and changes nothing.
- Deleting a resource deletes the object kflare created, and **keeps an object it adopted**.
  `kflare.dev/deletion-policy: retain` keeps any object, and `delete` deletes even an adopted one.
- Deleting a `KVNamespace` or `D1Database` deletes its data. kflare never deletes objects in an R2 bucket.
- Resources can be deleted in any order, a whole namespace included: parents wait for the resources that need them.

See [Adoption and ownership](ARCHITECTURE.md#adoption-and-ownership) and [Deletion](ARCHITECTURE.md#deletion).

### The Helm chart

- Installs and upgrades the CRDs as templates, and keeps them on uninstall (`helm.sh/resource-policy: keep`)
- Runs the controller under the restricted Pod Security Standard, with leader election, so extra replicas stand by
- Adds kflare's namespaced kinds to the built-in `edit` and `admin` roles, and every kind to `view`;
  `CloudflareAccount` is left to cluster administrators
- Serves Prometheus metrics, with an optional ServiceMonitor; `values.schema.json` validates the values

### Known limitations

- **Drift is not corrected promptly.** A change made outside kflare is corrected on the next reconcile, which can be up
  to 10 hours away. Changing the resource, even an annotation, reconciles it straight away.
- **Adoption goes by key** (a name, a title, a pattern), never by ID. Tunnel names are not unique: a Tunnel adopts the
  first one listed with its name.
- **Ownership is recorded in the cluster**, except for rules, which carry a ref in Cloudflare. A cluster rebuilt from
  git sees its existing objects as `AlreadyExists` until they are adopted; the deploy guide's
  [Moving to a new cluster](docs/deploy.md#moving-to-a-new-cluster) describes how.
- **Rule order covers kflare's rules only.** Rules created outside kflare keep their place.
- **Some limits of cloudflare-go v0.119**, which most controllers still use: Tunnels cannot be renamed, `originRequest`
  timeouts are not exposed, and R2 buckets live in the default jurisdiction only.
- **No Kubernetes Events yet**: the `Ready` condition and the controller's log say what happened.
- **Metrics are served over plain HTTP** without authentication, inside the cluster.

### Coming in v0.1.1

The chart will serve metrics over HTTPS with authentication and authorization by default, which changes how
Prometheus scrapes kflare. It will also create a PodDisruptionBudget when `replicaCount` is above 1, and gain an opt-in
NetworkPolicy. The release notes will say what to change.
