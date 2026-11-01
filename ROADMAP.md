# Roadmap

kflare is an alpha (API `v1alpha1`). This page says what works, what may come next, and what waits on something
outside the project. Nothing below "Done" is a promise or has a date: if you need one of these, say so in an issue,
since what people ask for decides the order.

## Done

| Area | Resource kinds |
|---|---|
| Accounts | `CloudflareAccount` |
| DNS | `Zone`, `DNSRecord` |
| Tunnels | `Tunnel`, `TunnelConfiguration` |
| Workers and storage | `WorkerScript`, `WorkerRoute`, `KVNamespace`, `R2Bucket`, `D1Database` |
| Security | `WAFCustomRule`, `RateLimitRule`, `ManagedRuleset`, `IPList` |
| Rules (the products that replace Page Rules) | `TransformRule`, `RedirectRule`, `CacheRule`, `ConfigurationRule`, `OriginRule` |
| Zone settings | `ManagedTransform` |

Each is tested live against a real Cloudflare account on the Free plan, in GitHub Actions, with an API token holding
only the permissions the [deploy guide](docs/deploy.md) lists. Existing Cloudflare objects are only taken over when a
resource asks for it (see [Adoption and ownership](ARCHITECTURE.md#adoption-and-ownership)). The Helm chart installs
the CRDs and the controller; the deploy guide shows it installed with Helm, Argo CD or Flux. Since v0.2.0 it serves
metrics over HTTPS to authorized scrapers only, adds a PodDisruptionBudget to a high-availability install, and has an
optional NetworkPolicy. A drift check interval (`driftCheckInterval`) corrects changes made outside kflare on a
schedule, each change of a resource's `Ready` condition is recorded as a Kubernetes Event, and `kubectl get kflare`
lists every kflare resource. A resource annotated `kflare.dev/paused: "true"` is left alone, its Cloudflare object
included, until the annotation is removed. From v0.2.1, the image and the chart are signed with
cosign, and the image comes with SBOMs and provenance (see [Verifying a release](docs/deploy.md#verifying-a-release)).
Since v0.3.0 every rule kind is generated from Cloudflare's API description, and its fields take the API's names
(the [changelog](CHANGELOG.md) shows each change, with a manifest before and after). `R2Bucket` gains a bucket's
jurisdiction and storage class, and `WorkerRoute` follows, its manifests unchanged. Since v0.4.0 every kind but
`CloudflareAccount` runs on a shared reconciler, so they all report conflicts, adopt objects and delete them the same
way: `IPList`, `Tunnel` and `DNSRecord` are generated too, and a Tunnel can be renamed. `Zone`, `TunnelConfiguration`
and `WorkerScript` keep hand-written fields on the older SDK.

## Next

Candidates that a Free account can use and test, roughly in the order the code generator can support them:

- **Zone settings**: settings such as the SSL mode, Always Use HTTPS, the minimum TLS version and HSTS
- **Queues** and **Vectorize indexes** for Workers
- **Turnstile widgets**, with the widget's secret written to a Kubernetes Secret
- **Workers custom domains**, next to `WorkerRoute`
- **Hyperdrive** connections, with the database password read from a Secret
- **Bulk redirects**: account-level redirect lists and the rules that use them
- **Managed ruleset exceptions**: skip rules for a managed ruleset kflare deploys

For running kflare, the [production-readiness issues](https://github.com/JonathanGraniero/kflare/issues/91), among
them:

- **One Cloudflare client and rate limiter per API token**
  ([#73](https://github.com/JonathanGraniero/kflare/issues/73))
- **A token's expiry** on its `CloudflareAccount`, and the token checked again on a schedule
  ([#74](https://github.com/JonathanGraniero/kflare/issues/74))
- **kflare's own metrics**: Cloudflare API calls, rate limiting, readiness and drift
  ([#75](https://github.com/JonathanGraniero/kflare/issues/75))

## Waiting on something

| What | Waits on |
|---|---|
| Zero Trust Access (applications, policies, groups) | Zero Trust has to be enabled on the test account before it can be built and tested |
| Load balancers, pools and monitors | Cloudflare's Load Balancing add-on (paid); without it, Cloudflare refuses every monitor |
| Standalone health checks | A Pro plan or higher |
| Zone plans (`Zone` `spec.plan`) | Built, but not tested live: changing a plan bills the account |
| Custom block responses on WAF custom rules | Built to Cloudflare's documented limits, but a Free zone cannot use them, so not tested live |

Later, beyond these: an OLM bundle, and a documentation site.
