# kflare

[![CI](https://github.com/JonathanGraniero/kflare/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/JonathanGraniero/kflare/actions/workflows/ci.yml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/JonathanGraniero/kflare/badge)](https://scorecard.dev/viewer/?uri=github.com/JonathanGraniero/kflare)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

kflare is a Kubernetes operator that manages Cloudflare through custom resources: zones, DNS records, tunnels, Workers
and their storage, WAF rules and the Rules products. You keep Cloudflare's configuration in git next to the workloads
it serves, apply it with Helm, Argo CD or Flux like everything else, and kflare keeps Cloudflare matching it.

**Status: alpha.** The API is `v1alpha1`, and fields can still change between releases. kflare needs Kubernetes 1.34
or newer, and installs with Helm 3.8 or newer.

## Quickstart: expose a Service through a Cloudflare Tunnel

This puts a demo Service on `https://hello.<your domain>` without a public IP, a load balancer or an open inbound port.
You need a domain on your Cloudflare account, and `hello.<your domain>` must not have a DNS record yet.

**1. Create an API token.** In the Cloudflare dashboard, under **My Profile → API Tokens** (or **Manage Account →
Account API Tokens** for a token the account owns), create a token with these permissions, with the zone permissions
limited to your domain:

- Account · Account Settings · Read
- Account · Cloudflare Tunnel · Edit
- Zone · Zone · Read
- Zone · DNS · Edit

Note your account ID, shown on the account's overview page.

**2. Install kflare.**

```sh
helm install kflare oci://ghcr.io/jonathangraniero/charts/kflare \
  --version 0.4.1 --namespace kflare-system --create-namespace
kubectl -n kflare-system rollout status deployment/kflare
```

**3. Connect your Cloudflare account.** The token goes in a Secret, and a cluster-scoped `CloudflareAccount` points
at it:

```sh
kubectl -n kflare-system create secret generic cloudflare-token --from-literal=CF_API_TOKEN=<your API token>

kubectl apply -f - <<'EOF'
apiVersion: kflare.dev/v1alpha1
kind: CloudflareAccount
metadata:
  name: my-account
spec:
  accountID: <your account ID>
  tokenSecretRef:
    name: cloudflare-token
    namespace: kflare-system
EOF

kubectl wait --for=condition=Ready cloudflareaccounts.kflare.dev/my-account --timeout=2m
```

**4. Expose a Service.** The [example](examples/expose-a-service) holds a demo app (`whoami`), a `Zone` that adopts
your existing domain, a `Tunnel`, its `TunnelConfiguration`, a `DNSRecord` pointing `hello.<your domain>` at the
tunnel, and a `cloudflared` Deployment that connects it. Apply it with your domain in place of `example.com`:

```sh
curl -sSL https://raw.githubusercontent.com/JonathanGraniero/kflare/v0.4.1/examples/expose-a-service/expose-a-service.yaml \
  | sed 's/example\.com/your-domain.com/g' | kubectl apply -f -

kubectl -n kflare-example wait --for=condition=Ready dnsrecords.kflare.dev/hello --timeout=5m
kubectl -n kflare-example rollout status deployment/cloudflared
curl https://hello.your-domain.com
```

`curl` prints the request `whoami` received. Until `cloudflared` connects, the hostname answers with Cloudflare's error
1033 (HTTP 530). If something stays not ready, `kubectl -n kflare-example describe` on it shows why in its `Ready`
condition.

**5. Clean up.**

```sh
kubectl delete namespace kflare-example
```

kflare deletes the DNS record, the tunnel's routes and the tunnel. It keeps your zone, because it adopted the zone
rather than creating it.

## Resource kinds

| Kind | What it manages | Token permission | Sample |
|---|---|---|---|
| `CloudflareAccount` | The credentials every other resource uses (cluster-scoped) | Account · Account Settings · Read | [account](config/samples/cloudflare_v1alpha1_cloudflareaccount.yaml) |
| `Zone` | A zone, created or adopted, with its name servers and status | Zone · Zone · Edit (Read to adopt) | [zone](config/samples/cloudflare_v1alpha1_zone.yaml) |
| `DNSRecord` | A DNS record, or with `tunnelRef` a hostname pointed at a `Tunnel` | Zone · DNS · Edit | [A](config/samples/cloudflare_v1alpha1_dnsrecord_a.yaml), [MX](config/samples/cloudflare_v1alpha1_dnsrecord_mx.yaml), [SRV](config/samples/cloudflare_v1alpha1_dnsrecord_srv.yaml), [tunnel](config/samples/cloudflare_v1alpha1_dnsrecord_tunnel.yaml) |
| `Tunnel` | A Cloudflare Tunnel, with its token written to a Secret for `cloudflared` | Account · Cloudflare Tunnel · Edit | [tunnel](config/samples/cloudflare_v1alpha1_tunnel.yaml) |
| `TunnelConfiguration` | A tunnel's routes, hostname to Service | Account · Cloudflare Tunnel · Edit | [routes](config/samples/cloudflare_v1alpha1_tunnelconfiguration.yaml) |
| `WorkerScript` | A Worker, from inline code or a ConfigMap, with its bindings | Account · Workers Scripts · Edit | [worker](config/samples/cloudflare_v1alpha1_workerscript.yaml) |
| `WorkerRoute` | A route from a URL pattern to a Worker | Zone · Workers Routes · Edit | [route](config/samples/cloudflare_v1alpha1_workerroute.yaml) |
| `KVNamespace` | A Workers KV namespace | Account · Workers KV Storage · Edit | [namespace](config/samples/cloudflare_v1alpha1_kvnamespace.yaml) |
| `R2Bucket` | An R2 bucket (kflare never deletes its objects) | Account · Workers R2 Storage · Edit | [bucket](config/samples/cloudflare_v1alpha1_r2bucket.yaml) |
| `D1Database` | A D1 database | Account · D1 · Edit | [database](config/samples/cloudflare_v1alpha1_d1database.yaml) |
| `WAFCustomRule` | One WAF custom rule in a zone | Zone · Zone WAF · Edit | [rules](config/samples/cloudflare_v1alpha1_wafcustomrule.yaml) |
| `RateLimitRule` | One rate limiting rule in a zone | Zone · Zone WAF · Edit | [rule](config/samples/cloudflare_v1alpha1_ratelimitrule.yaml) |
| `ManagedRuleset` | A Cloudflare managed ruleset deployed to a zone, with overrides | Zone · Zone WAF · Edit | [ruleset](config/samples/cloudflare_v1alpha1_managedruleset.yaml) |
| `IPList` | An IP list that rule expressions use as `$name` | Account · Account Rule Lists · Edit | [list](config/samples/cloudflare_v1alpha1_iplist.yaml) |
| `TransformRule` | One URL rewrite or header transform rule | Zone · Transform Rules · Edit | [rules](config/samples/cloudflare_v1alpha1_transformrule.yaml) |
| `RedirectRule` | One single redirect | Zone · Single Redirect · Edit | [rule](config/samples/cloudflare_v1alpha1_redirectrule.yaml) |
| `CacheRule` | One cache rule | Zone · Cache Rules · Edit | [rule](config/samples/cloudflare_v1alpha1_cacherule.yaml) |
| `ConfigurationRule` | One configuration rule (SSL mode, security level and other settings) | Zone · Config Rules · Edit | [rule](config/samples/cloudflare_v1alpha1_configurationrule.yaml) |
| `OriginRule` | One origin rule (the origin's destination port) | Zone · Origin Rules · Edit | [rule](config/samples/cloudflare_v1alpha1_originrule.yaml) |
| `ManagedTransform` | A zone's managed transforms, the request and response headers Cloudflare adds or removes (one per zone) | Zone · Managed Headers · Edit | [transforms](config/samples/cloudflare_v1alpha1_managedtransform.yaml) |

Grant a token only the permissions of the kinds you use. Setting a `Zone`'s `spec.plan` also needs Account · Billing ·
Edit, and changes your bill.

## Running kflare in your clusters

The [deploy guide](docs/deploy.md) covers installing and operating kflare. In short:

- **Install it like any other platform component.** Pin the chart version, keep your values in git, and let Argo CD or
  Flux install it; the guide has an `Application` and a `HelmRelease` to start from. The chart installs and upgrades
  the CRDs, keeps them on uninstall, and runs under the restricted Pod Security Standard. The guide also has health
  checks for Argo CD and Flux, so that an application turns Degraded when Cloudflare refuses a resource, and sync
  waves wait for one to be ready.
- **Releases are signed** (from v0.2.1): the image and the chart with cosign, keyless, with SBOMs
  and provenance, so that cosign, an admission policy or Flux can check them; the guide has a Kyverno policy.
- **One installation per cluster**, with leader election. A HorizontalPodAutoscaler adds standby replicas while the
  leader is busy, and `autoscaling.minReplicas: 2` keeps one at all times for a quick failover.
- **kflare never takes over what it did not create**, unless a resource asks with the
  `kflare.dev/adoption-policy` annotation. Objects it adopted are kept when their resource is deleted, and
  `kflare.dev/deletion-policy: retain` keeps any object, for zones and storage that must outlive a manifest.
- **Any resource can be paused** with `kflare.dev/paused: "true"`, during an incident or a migration done by hand:
  kflare leaves its Cloudflare object alone until the annotation is removed (from v0.2.1).
- **Credentials stay in Secrets** that you can manage with External Secrets Operator or Sealed Secrets. A rotated token
  is picked up straight away. Only cluster administrators can create a `CloudflareAccount`, and each can
  [limit the namespaces that may use it](docs/deploy.md#limiting-which-namespaces-may-use-an-account); the namespaced
  kinds are added to the built-in `edit` and `admin` roles, so teams manage their own records and rules.
- **One installation can serve many Cloudflare accounts.** Each API token stays within Cloudflare's rate limit, and an
  account past its limit holds up no other
  ([deploy guide](docs/deploy.md#several-cloudflare-accounts)).
- **Every resource reports a single `Ready` condition** with a reason, and the controller serves Prometheus metrics.

[ARCHITECTURE.md](ARCHITECTURE.md) explains how kflare finds, owns, corrects and deletes Cloudflare objects, and its
known limitations. One to know up front: a change made in the Cloudflare dashboard is corrected on the next
reconcile, which can be up to 10 hours away, unless you set a drift check interval (the chart's `driftCheckInterval`,
from v0.2.0).

## Why kflare

Other tools cover parts of this, and may suit you better. [external-dns](https://github.com/kubernetes-sigs/external-dns)
is mature and widely used for DNS records that follow your Services and Ingresses.
[adyanth/cloudflare-operator](https://github.com/adyanth/cloudflare-operator) exposes Services through tunnels.
Terraform's Cloudflare provider, or Crossplane, covers far more of Cloudflare's API than kflare does. kflare is for
teams who want Cloudflare configured the Kubernetes way: custom resources reviewed and applied like the rest of the
cluster, references between them instead of copied IDs, and one operator for DNS, tunnels, Workers and their storage,
the WAF and the Rules products.

## How it's built

kflare is developed with AI assistance (Claude Code). What keeps it honest:

- **A live end-to-end suite** runs every resource kind against a real Cloudflare account on the Free plan, in GitHub
  Actions, and checks Cloudflare's copy after each step. Its token holds nothing beyond the permissions above.
- **Mutation testing** on the controllers: deliberate bugs, which the tests must catch.
- **Fuzz tests** on the code that reads input kflare does not control: the metrics endpoint's authentication and the
  reading of Cloudflare's errors.
- **Cloudflare's behavior is checked live before code relies on it**: what the API accepts, refuses and stores.
  [ARCHITECTURE.md](ARCHITECTURE.md) records what was verified.
- **Known vulnerabilities are checked for** in the code and the image on every pull request, and in the latest
  release's image every week; [SECURITY.md](SECURITY.md) lists the checks.

## More

- [Deploy guide](docs/deploy.md): install, configure, operate, upgrade, uninstall, troubleshoot
- [Architecture](ARCHITECTURE.md): how the controllers behave, and known limitations
- [Roadmap](ROADMAP.md): what may come next
- [Discussions](https://github.com/JonathanGraniero/kflare/discussions): questions, ideas and feedback
- [Contributing](CONTRIBUTING.md): building, testing and the live suite
- [Security](SECURITY.md): reporting a vulnerability

## License

MIT. Copyright (c) 2026 Jonathan Graniero. See [LICENSE](LICENSE).
