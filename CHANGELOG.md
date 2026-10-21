# Changelog

kflare follows [semantic versioning](https://semver.org). While its API is `v1alpha1`, any release can change it:
read the notes before upgrading. The chart and the controller image share one version.

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
