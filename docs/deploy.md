# Deploying kflare with Helm

This guide installs the kflare controller into a Kubernetes cluster, connects it to a Cloudflare
account, and covers running it in production, upgrades, uninstalling and troubleshooting.

- [Requirements](#requirements)
- [1. Create a Cloudflare API token](#1-create-a-cloudflare-api-token)
- [2. Install the chart](#2-install-the-chart)
  - [With Argo CD or Flux](#with-argo-cd-or-flux)
- [3. Connect your Cloudflare account](#3-connect-your-cloudflare-account)
- [4. Manage Cloudflare resources](#4-manage-cloudflare-resources)
- [Configuration](#configuration)
- [Security model](#security-model)
- [Running in production](#running-in-production)
  - [Monitoring](#monitoring)
  - [Network access](#network-access)
  - [Protecting objects that must outlive their manifests](#protecting-objects-that-must-outlive-their-manifests)
  - [Moving to a new cluster](#moving-to-a-new-cluster)
- [Upgrading](#upgrading)
- [Uninstalling](#uninstalling)
- [Troubleshooting](#troubleshooting)

## Requirements

- Kubernetes 1.34 or newer. The chart refuses to install on older clusters.
- Helm 3.8 or newer (OCI chart support), or a GitOps tool that installs Helm charts from an OCI registry, such as
  Argo CD or Flux.
- One kflare installation per cluster. A second release would reconcile the same resources.
- Outbound HTTPS from the controller to `api.cloudflare.com` (see [Network access](#network-access)).

## 1. Create a Cloudflare API token

Create an API token in the Cloudflare dashboard (**My Profile → API Tokens**, or an account-owned token under
**Manage Account → Account API Tokens**). Grant only what the resources you plan to use need:

| Resource | Permission |
|---|---|
| `CloudflareAccount` (always) | Account · Account Settings · Read |
| `Zone` | Zone · Zone · Edit (Read is enough to adopt existing zones) |
| `Zone` with `spec.plan` | Account · Billing · Edit, and changing a plan bills the account |
| `DNSRecord` | Zone · DNS · Edit |
| `Tunnel`, `TunnelConfiguration` | Account · Cloudflare Tunnel · Edit |
| `WorkerScript` | Account · Workers Scripts · Edit |
| `WorkerRoute` | Zone · Workers Routes · Edit |
| `KVNamespace` | Account · Workers KV Storage · Edit |
| `R2Bucket` | Account · Workers R2 Storage · Edit |
| `D1Database` | Account · D1 · Edit |
| `WAFCustomRule`, `RateLimitRule`, `ManagedRuleset` | Zone · Zone WAF · Edit |
| `TransformRule` | Zone · Transform Rules · Edit |
| `RedirectRule` | Zone · Single Redirect · Edit |
| `CacheRule` | Zone · Cache Rules · Edit |
| `ConfigurationRule` | Zone · Config Rules · Edit |
| `OriginRule` | Zone · Origin Rules · Edit |
| `ManagedTransform` | Zone · Managed Headers · Edit |
| `IPList` | Account · Account Rule Lists · Edit |

These are enough on their own: in October 2026 a token holding only them (with Zone · Zone · Read, and without Billing)
ran kflare's full live test suite against an existing zone. `ManagedTransform`'s row came later, and has been tested
live with a broader token only.
Scope zone permissions to the zones kflare should manage. Note the account ID, shown on the account's overview page.

## 2. Install the chart

Each release publishes the chart and the controller image to GitHub Container Registry. Pick a version from the
[releases page](https://github.com/JonathanGraniero/kflare/releases):

```sh
helm install kflare oci://ghcr.io/jonathangraniero/charts/kflare \
  --version <version> \
  --namespace kflare-system --create-namespace \
  --values kflare-values.yaml
```

Keep `kflare-values.yaml` in git (see [Configuration](#configuration)). The chart's version pins the image too:
`image.tag` defaults to the chart's `appVersion`. To pull from a registry mirror, set `image.repository` and, if it
needs credentials, `imagePullSecrets`.

From a checkout of this repository, for example to run unreleased changes:

```sh
helm install kflare ./helm/kflare \
  --namespace kflare-system --create-namespace \
  --set image.repository=<your-registry>/kflare,image.tag=<tag>
```

The chart installs:

- the kflare CRDs (kept on uninstall, see [Uninstalling](#uninstalling))
- the controller Deployment, ServiceAccount and a Service for metrics
- a ClusterRole for the controller, plus a Role for leader election in the release namespace
- a `kflare-metrics-reader` ClusterRole for whoever scrapes the metrics (see [Monitoring](#monitoring))
- a PodDisruptionBudget, when `replicaCount` is above 1
- `view`/`edit` aggregation roles (see [Security model](#security-model))
- optionally, a ServiceMonitor and a NetworkPolicy (see [Running in production](#running-in-production))

Wait for the controller:

```sh
kubectl -n kflare-system rollout status deployment/kflare
```

### With Argo CD or Flux

Install kflare from the platform repository your GitOps tool syncs, in its own Argo CD `Application` or Flux
`HelmRelease`, and keep kflare resources in the applications that use them. The `CloudflareAccount` and its token
Secret belong with the kflare installation, since only cluster administrators should create accounts (see
[Security model](#security-model)).

Argo CD, with the chart from GitHub Container Registry (Argo CD takes an OCI registry's address without `oci://`):

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: kflare
  namespace: argocd
spec:
  project: default
  source:
    repoURL: ghcr.io/jonathangraniero/charts
    chart: kflare
    targetRevision: <version>
    helm:
      valuesObject:
        replicaCount: 2
  destination:
    name: in-cluster
    namespace: kflare-system
  syncPolicy:
    automated:
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
```

Flux, with an `OCIRepository` for the chart:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: kflare
  namespace: flux-system
spec:
  interval: 1h
  url: oci://ghcr.io/jonathangraniero/charts/kflare
  ref:
    tag: <version>
  layerSelector:
    mediaType: application/vnd.cncf.helm.chart.content.v1.tar+gzip
    operation: copy
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: kflare
  namespace: flux-system
spec:
  interval: 1h
  chartRef:
    kind: OCIRepository
    name: kflare
  releaseName: kflare          # otherwise Flux names it kflare-system-kflare
  targetNamespace: kflare-system
  install:
    createNamespace: true
  values:
    replicaCount: 2
```

Things to know:

- **The CRDs come with the chart**, as templates rather than in Helm's `crds/` directory, so Argo CD and Flux upgrade
  them with the chart. They carry `helm.sh/resource-policy: keep`: Flux uninstalls with Helm, which keeps them, and
  Argo CD treats the annotation as its `Delete=false` sync option, so deleting the Application keeps them too.
- **Decide who owns the CRDs once, at install time.** Removing a CRD deletes every resource of its kind, and with them
  their Cloudflare objects. Switching `crds.enabled` to `false` on a running installation stops the chart from
  rendering the CRDs, and a GitOps tool that prunes could then delete them. This is why the Argo CD example above does
  not turn on `prune`. To manage the CRDs yourself, set `crds.enabled=false` from the start and apply
  `config/crd/bases` from the matching release, for example in an earlier sync wave.
- **On a new cluster, kflare resources need the CRDs first.** In Argo CD, an application holding kflare resources
  fails its first sync until the CRDs exist; `syncPolicy.retry` lets it try again. In Flux, put the `HelmRelease` in
  a `Kustomization` that the ones holding kflare resources list under `dependsOn`.
- **Upgrade by changing the version in git**, after reading the release notes (see [Upgrading](#upgrading)).

## 3. Connect your Cloudflare account

Store the token in a Secret. kflare reads the key `CF_API_TOKEN` unless `tokenSecretRef.key` says otherwise.
The Secret can live in any namespace; keeping it next to the controller is simplest.

```sh
kubectl -n kflare-system create secret generic cloudflare-token \
  --from-literal=CF_API_TOKEN=<your-api-token>
```

Point a cluster-scoped `CloudflareAccount` at it:

```yaml
apiVersion: kflare.dev/v1alpha1
kind: CloudflareAccount
metadata:
  name: my-account
spec:
  accountID: <your-account-id>
  tokenSecretRef:
    name: cloudflare-token
    namespace: kflare-system
```

```sh
kubectl apply -f account.yaml
kubectl get cloudflareaccounts.kflare.dev
# NAME         ACCOUNT ID   ACCOUNT NAME     READY
# my-account   0123abcd…    Example Account  True
```

`READY=True` means the token works and the account is reachable. kflare watches the Secret, so a
rotated token is re-validated straight away. Rotate it by updating the Secret in place: kflare holds a
finalizer on it (`kflare.dev/token-protection`) while an account references it, so a deleted Secret stays
`Terminating` until its accounts are gone.

Managing the Secret with [External Secrets Operator](https://external-secrets.io) or Sealed Secrets works the
same way; only the Secret's name and namespace matter.

## 4. Manage Cloudflare resources

Every other kind references the account (or a resource that does). Examples are in
[`config/samples`](../config/samples):

| Kind | Scope | References | Sample |
|---|---|---|---|
| `Zone` | Namespaced | `accountRef` | [zone](../config/samples/cloudflare_v1alpha1_zone.yaml) |
| `DNSRecord` | Namespaced | `zoneRef`, optional `tunnelRef` (same namespace) | [A](../config/samples/cloudflare_v1alpha1_dnsrecord_a.yaml), [MX](../config/samples/cloudflare_v1alpha1_dnsrecord_mx.yaml), [SRV](../config/samples/cloudflare_v1alpha1_dnsrecord_srv.yaml), [to a tunnel](../config/samples/cloudflare_v1alpha1_dnsrecord_tunnel.yaml) |
| `Tunnel` | Namespaced | `accountRef` | [tunnel](../config/samples/cloudflare_v1alpha1_tunnel.yaml) |
| `TunnelConfiguration` | Namespaced | `tunnelRef` (same namespace) | [tunnel configuration](../config/samples/cloudflare_v1alpha1_tunnelconfiguration.yaml) |
| `WorkerScript` | Namespaced | `accountRef` | [worker](../config/samples/cloudflare_v1alpha1_workerscript.yaml) |
| `WorkerRoute` | Namespaced | `zoneRef`, optional `workerScriptRef` (same namespace) | [route and exclusion](../config/samples/cloudflare_v1alpha1_workerroute.yaml) |
| `KVNamespace` | Namespaced | `accountRef`; WorkerScripts bind it with `kvNamespaceRef` | [KV namespace](../config/samples/cloudflare_v1alpha1_kvnamespace.yaml) |
| `R2Bucket` | Namespaced | `accountRef`; WorkerScripts bind it with `r2BucketRef` | [R2 bucket](../config/samples/cloudflare_v1alpha1_r2bucket.yaml) |
| `D1Database` | Namespaced | `accountRef` | [D1 database](../config/samples/cloudflare_v1alpha1_d1database.yaml) |
| `WAFCustomRule` | Namespaced | `zoneRef` (same namespace) | [block and skip rules](../config/samples/cloudflare_v1alpha1_wafcustomrule.yaml) |
| `RateLimitRule` | Namespaced | `zoneRef` (same namespace) | [API rate limit](../config/samples/cloudflare_v1alpha1_ratelimitrule.yaml) |
| `IPList` | Namespaced | `accountRef`; rule expressions use it as `$name` | [office addresses and a rule using them](../config/samples/cloudflare_v1alpha1_iplist.yaml) |
| `ManagedRuleset` | Namespaced | `zoneRef` (same namespace) | [Free Managed Ruleset with overrides](../config/samples/cloudflare_v1alpha1_managedruleset.yaml) |
| `TransformRule` | Namespaced | `zoneRef` (same namespace) | [URL rewrite, request and response headers](../config/samples/cloudflare_v1alpha1_transformrule.yaml) |
| `RedirectRule` | Namespaced | `zoneRef` (same namespace) | [www to the apex domain](../config/samples/cloudflare_v1alpha1_redirectrule.yaml) |
| `CacheRule` | Namespaced | `zoneRef` (same namespace) | [static assets for a day](../config/samples/cloudflare_v1alpha1_cacherule.yaml) |
| `ConfigurationRule` | Namespaced | `zoneRef` (same namespace) | [stricter settings for admin pages](../config/samples/cloudflare_v1alpha1_configurationrule.yaml) |
| `OriginRule` | Namespaced | `zoneRef` (same namespace) | [API requests to port 8443](../config/samples/cloudflare_v1alpha1_originrule.yaml) |
| `ManagedTransform` | Namespaced | `zoneRef` (same namespace); one per Cloudflare zone | [visitor location and security headers](../config/samples/cloudflare_v1alpha1_managedtransform.yaml) |

To expose a Service on your domain through a Cloudflare Tunnel, see
[`examples/expose-a-service`](../examples/expose-a-service). It puts the pieces together: a Zone, a Tunnel, its
TunnelConfiguration, a DNSRecord whose `tunnelRef` points the hostname at the tunnel, and a `cloudflared` Deployment
that connects it.

Things to know before pointing kflare at existing infrastructure:

- **Adoption.** kflare never takes over something you already have unless you ask it to. A resource whose
  object already exists in Cloudflare (a zone with its name, a bucket, a database, a Worker...) reports
  `AlreadyExists` and leaves it alone. To manage the existing object, annotate the resource
  `kflare.dev/adoption-policy: adopt` (adopt it, never create one) or `adopt-or-create` (adopt it, or create
  one if there is none). kflare then corrects drift toward your spec, and marks the resource
  `kflare.dev/adopted: "true"`.
- **Deletion policy.** Deleting a kflare resource deletes the Cloudflare object kflare created, and keeps one
  it adopted. The annotation `kflare.dev/deletion-policy` overrides that: `retain` keeps the object, and
  `delete` deletes it even if it was adopted.
- **R2 objects.** kflare never deletes objects. Deleting an `R2Bucket` deletes the bucket only once it is
  empty; until then it reports `BucketNotEmpty`. Use `retain` to keep a bucket and its objects.
- **KV data.** Deleting a `KVNamespace` deletes the namespace and everything stored in it. Use `retain` on
  namespaces holding data you need.
- **D1 data.** Deleting a `D1Database` deletes the database and everything stored in it. Use `retain` on
  databases holding data you need. A database's name, jurisdiction and location hint are set when it is created
  and cannot change. Free accounts can have 10 D1 databases.
- **WAF custom rules.** Each `WAFCustomRule` manages one rule in its zone, which kflare marks with a ref
  derived from the resource's namespace and name. kflare never adopts or changes rules it did not create,
  and keeps its own rules in `spec.priority` order (lower first) while other rules keep their place. Free
  zones allow 5 custom rules, without regular expressions.
- **Rate limiting rules.** Each `RateLimitRule` manages one rate limiting rule in its zone, marked and ordered
  like WAF custom rules. Free zones allow one, which counts per IP address over 10 seconds and can only block.
  Cloudflare also counts per data center, and requires rules to say so: kflare adds `cf.colo.id` to
  `spec.characteristics` itself.
- **IP lists.** An `IPList` manages a custom list in its account, created or adopted by its name, and
  replaces the list's items whenever they differ from `spec.items`. Rule expressions use it as
  `$<spec.name>`. Cloudflare refuses a rule that uses a list the account does not have yet, and refuses to
  delete a list that a rule uses: kflare writes such a rule once the list exists, and deletes the list once
  its rules are gone (reporting `ListInUse` meanwhile). Free accounts can have one IP list, with up to
  10,000 items.
- **Managed rulesets.** Cloudflare deploys its Free Managed Ruleset to Free zones by default, but only while the
  zone has no rules of its own in the managed phase. A `ManagedRuleset` deploys a ruleset explicitly, so the zone
  keeps it; to turn it off, set the ManagedRuleset's `enabled: false` rather than deleting it. Deleting the last
  ManagedRuleset in a zone hands it back to Cloudflare's default, which turns the Free Managed Ruleset on again even
  if it was off before kflare. A zone deploys each ruleset once: a second deployment, or one a rule created outside
  kflare already makes, reports `RulesetConflict`. Free zones can only deploy the Free Managed Ruleset.
- **Transform rules.** Each `TransformRule` manages one rule in its zone, marked and ordered like WAF custom
  rules. Its `spec.type`, which cannot change, picks what it transforms: `urlRewrite` rewrites the URL the origin
  receives (`uri`), `requestHeaders` sets or removes headers of the request to the origin, and `responseHeaders`
  adds, sets or removes headers of the response (`headers`). Free zones allow 10 transform rules across the three
  types, without regular expressions (`regex_replace`).
- **Redirect rules.** Each `RedirectRule` manages one single redirect in its zone, marked and ordered like WAF custom
  rules. `fromValue.targetURL` is the URL to redirect to, a static `value` or an `expression` that computes it;
  `statusCode` defaults to 301, and the query string is dropped unless `preserveQueryString` is set. Free zones allow
  10 single redirects, without regular expressions (`regex_replace`, or `matches` in the rule's expression);
  `wildcard_replace` works on every plan.
- **Cache rules.** Each `CacheRule` manages one cache rule in its zone, marked and ordered like WAF custom rules:
  whether responses are cached (`cache`), for how long at the edge and in browsers (`edgeTTL`, with TTLs by status code,
  and `browserTTL`), the cache key (`cacheKey`) and how the `Vary` header shapes it (`vary`), and more. Free zones allow
  10 cache rules, and of a custom cache key only leaving out the whole query string. kflare leaves out what Free zones
  cannot use or Cloudflare refuses in this phase: an extended read timeout, shared dictionaries, the rest of the custom
  cache key, and stripping ETags, Last-Modified or Set-Cookie headers.
- **Configuration rules.** Each `ConfigurationRule` manages one configuration rule in its zone, marked and ordered like
  WAF custom rules: settings changed for the requests its expression matches, such as the SSL mode, the security level,
  Rocket Loader, Polish or Browser Integrity Check. Free zones allow 10 configuration rules; some values need a higher
  plan (Polish other than `off`, `ssl: origin_pull`, `securityLevel: off`), and Cloudflare refuses them on Free.
  kflare leaves out the deprecated settings (Auto Minify, Cloudflare Apps, Mirage, Signed Exchanges) and Pay Per
  Crawl, which Free zones cannot set.
- **Origin rules.** Each `OriginRule` manages one origin rule in its zone, marked and ordered like WAF custom rules: the
  requests its expression matches reach the origin on another destination port (`origin.port`, 1 to 65535). The port is
  the only override Free, Pro and Business zones may use: Cloudflare's documentation lists the Host header, SNI and DNS
  record overrides for Enterprise plans, and Cloudflare refuses them on Free, so kflare leaves them out. Cloudflare's
  documentation gives Free zones 10 origin rules.
- **Managed transforms.** A `ManagedTransform` turns its zone's managed transforms on and off, by ID: those that add
  or remove request headers (`managedRequestHeaders`) and response headers (`managedResponseHeaders`). The transforms
  it does not list keep their state. One ManagedTransform configures a zone: another one for the same Cloudflare zone,
  in any namespace, reports `AlreadyConfigured` until the first is gone. A zone with any transform on counts as
  configured outside kflare, so a ManagedTransform takes it over only with an adoption policy. Deleting it turns all of
  the zone's managed transforms off, unless it adopted them or the retain policy is set. Free zones have six
  transforms; `GET /zones/{zone_id}/managed_headers` lists those a zone has.
- **Immutable fields.** Names and references (`spec.name`, `accountRef`, `zoneRef`, DNS `type`, ...) cannot be
  changed. Create a new resource instead.

## Configuration

Every value is documented in [`helm/kflare/values.yaml`](../helm/kflare/values.yaml), and
[`values.schema.json`](../helm/kflare/values.schema.json) rejects unknown or invalid values at install time.
A values file for a production cluster usually sets:

```yaml
# High availability: one replica is active, the others wait on the leader lease
# and take over within about 15 seconds (the lease's duration) if it goes. With
# more than one replica, a PodDisruptionBudget lets a node drain evict one pod
# at a time.
replicaCount: 2
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: kubernetes.io/hostname
    whenUnsatisfiable: ScheduleAnyway
    labelSelector:
      matchLabels:
        app.kubernetes.io/name: kflare

# Prometheus Operator scraping, over HTTPS with Prometheus' own ServiceAccount
# token (see Monitoring).
metrics:
  serviceMonitor:
    enabled: true
    labels:
      release: prometheus   # match your Prometheus' serviceMonitorSelector

logging:
  level: info               # debug, info or error
  format: json              # json or console

# The controller caches every Secret in the cluster (token and credential Secrets
# are looked up by reference). Raise the limit on clusters with many Secrets.
resources:
  limits:
    memory: 512Mi
```

The pod runs under the restricted Pod Security Standard: non-root, no privilege escalation, all
capabilities dropped and a read-only root filesystem.

## Security model

- **The controller can read every Secret in the cluster.** It needs to read token Secrets wherever
  `CloudflareAccount`s point, `WorkerScript` secret bindings, and to manage Tunnel credential Secrets.
- **CloudflareAccounts are cluster-wide credentials.** Any namespaced resource can reference any
  `CloudflareAccount` by name. Treat permission to create kflare resources as permission to use every
  account's token, and give tenants their own cluster or Cloudflare account if they must be isolated.
- **Aggregated roles.** With `rbac.aggregateToDefaultRoles` (default), the built-in `view` role can read all
  kflare kinds, and `edit`/`admin` can manage the namespaced ones. `CloudflareAccount` is never added to
  `edit`: an account can point the controller at a token Secret in any namespace, so only cluster
  administrators should create one.
- **Metrics** are served over HTTPS on port 8080, and only to callers the API server authenticates and authorizes: a
  request needs a bearer token, such as a ServiceAccount's, whose user may `get` the `/metrics` non-resource URL,
  which the `kflare-metrics-reader` ClusterRole grants. The controller checks a token with a TokenReview and a
  SubjectAccessReview, which its ClusterRole may create, and trusts an allowed token for a minute.
  `metrics.secure: false` serves plain HTTP to anyone who can reach the port instead.

## Running in production

### Monitoring

Every kflare resource reports a single `Ready` condition with a reason (see [Troubleshooting](#troubleshooting)), and
the `READY` column of `kubectl get` shows it. kflare does not record Kubernetes Events yet.

The controller serves controller-runtime's standard Prometheus metrics: `controller_runtime_reconcile_total` and
`controller_runtime_reconcile_errors_total`, labelled by `controller`, `controller_runtime_reconcile_time_seconds`,
`workqueue_depth`, and the Go runtime's. Turn on `metrics.serviceMonitor.enabled` to have the Prometheus Operator scrape
them.

The endpoint answers over HTTPS only, and to a bearer token allowed to `get` `/metrics`: a request gets 401 without a
token the API server accepts, and 403 with one that lacks the permission. The ServiceMonitor scrapes with Prometheus'
own ServiceAccount token. kube-prometheus-stack's Prometheus already has the permission; for another Prometheus, bind
its ServiceAccount to the chart's `kflare-metrics-reader` ClusterRole:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: prometheus-kflare-metrics-reader
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: kflare-metrics-reader
subjects:
  - kind: ServiceAccount
    name: prometheus          # your Prometheus' ServiceAccount
    namespace: monitoring
```

The ServiceMonitor reads that token with `bearerTokenFile`, which a Prometheus with
`arbitraryFSAccessThroughSMs.deny` refuses. To scrape by hand, with a token for a ServiceAccount bound to the role:

```sh
kubectl -n kflare-system port-forward svc/kflare-metrics 8080 &
curl -k -H "Authorization: Bearer $(kubectl -n monitoring create token prometheus)" https://localhost:8080/metrics
```

The controller's certificate is self-signed, and the ServiceMonitor skips verifying it, unless `metrics.certSecret`
names a Secret in the release namespace that holds one: `tls.crt` and `tls.key`, and `ca.crt` for the ServiceMonitor
to verify against. For example, from cert-manager, with an issuer whose Secrets carry `ca.crt` (a CA issuer does):

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: kflare-metrics
  namespace: kflare-system
spec:
  secretName: kflare-metrics-tls     # metrics.certSecret: kflare-metrics-tls
  dnsNames:
    - kflare-metrics.kflare-system.svc
  issuerRef:
    kind: ClusterIssuer
    name: my-ca-issuer
```

The controller reloads the certificate when it is renewed. `metrics.serviceMonitor.tlsConfig` replaces the
ServiceMonitor's TLS settings, for a CA kept elsewhere.

Two things to alert on:

- **Errors from Cloudflare that kflare retries** (5xx, rate limiting, network failures) count as reconcile errors.
  A rate that stays above zero means kflare cannot reach Cloudflare or is being rate limited:

  ```yaml
  - alert: KflareReconcileErrors
    expr: sum by (controller) (rate(controller_runtime_reconcile_errors_total{namespace="kflare-system"}[15m])) > 0
    for: 30m
  ```

- **Resources that are not `Ready`.** Errors kflare does not retry, such as a spec Cloudflare refuses or a missing
  token permission (`TerminalError`), and waits such as `AlreadyExists` or a parent that is not ready, do not count as
  reconcile errors: they show only in the condition. Check the `READY` column after each change, or export the
  conditions with kube-state-metrics' custom resource state metrics and alert on `Ready` being `False`.

The log is JSON by default, and each reconcile's lines name the resource's kind, namespace and name.

### Network access

The controller needs outbound HTTPS to `api.cloudflare.com` and access to the Kubernetes API server, and nothing
else; nothing connects to it but metrics scrapers and the kubelet's probes. Under a default-deny policy, turn on the
chart's NetworkPolicy, which allows just that:

```yaml
networkPolicy:
  enabled: true
  metricsFrom:                 # who may scrape the metrics port; empty allows any source
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: monitoring
```

It allows DNS to the cluster DNS (`networkPolicy.dns.to`: kube-system's pods labelled `k8s-app: kube-dns`), the API
server on ports 443 and 6443 at any address (`networkPolicy.apiServer.ports` and `.to`), and port 443 at any address
for Cloudflare, since a NetworkPolicy cannot select a hostname. Most CNIs apply a policy after translating the
`kubernetes` Service to the API server's own address and port, so `networkPolicy.apiServer.ports` must include the
port this lists, and `networkPolicy.apiServer.to`, if you narrow it, the addresses:

```sh
kubectl get endpointslices -n default -l kubernetes.io/service-name=kubernetes
```

The policy is off by default because one that misses the API server cuts the controller off, and that may not show
at once. The controller connects to the API server as it starts, sooner than a CNI applies a policy to a new pod, and
keeps that connection. To check the policy, connect from a pod under a copy of it, once the copy is in force:

```sh
kubectl -n kflare-system get networkpolicy kflare -o json \
  | jq '.metadata = {name: "kflare-probe"} | .spec.podSelector = {matchLabels: {run: "kflare-probe"}}' \
  | kubectl -n kflare-system apply -f -
kubectl -n kflare-system run kflare-probe --image=registry.k8s.io/e2e-test-images/agnhost:2.53 -- pause
kubectl -n kflare-system wait --for=condition=Ready pod/kflare-probe && sleep 10
kubectl -n kflare-system exec kflare-probe -- /agnhost connect kflare-metrics:8080 --timeout=5s  # TIMEOUT
kubectl -n kflare-system exec kflare-probe -- /agnhost connect kubernetes.default.svc:443 --timeout=5s
kubectl -n kflare-system exec kflare-probe -- /agnhost connect api.cloudflare.com:443 --timeout=5s
kubectl -n kflare-system delete pod/kflare-probe networkpolicy/kflare-probe
```

Each `connect` prints nothing when it gets through, and `TIMEOUT` when the policy blocks it. The first must time
out: the policy opens no egress to that port, so a connection there means the policy is not in force yet (wait and
retry) or the CNI does not enforce NetworkPolicies.

Behind an HTTP proxy, set the standard proxy variables with `extraEnv`. Both Cloudflare clients and the Kubernetes
client read them, so `NO_PROXY` must cover the API server, which the controller reaches at the `kubernetes` Service's
cluster IP:

```yaml
extraEnv:
  - name: HTTPS_PROXY
    value: http://proxy.internal.example:3128
  - name: NO_PROXY
    value: 10.96.0.0/12,.svc,.cluster.local   # your cluster's Service CIDR
```

With the NetworkPolicy on, add the proxy to `networkPolicy.extraEgress`, for example
`[{to: [{ipBlock: {cidr: 10.0.0.10/32}}], ports: [{port: 3128}]}]`.

### Protecting objects that must outlive their manifests

These all delete the Cloudflare objects kflare created, as long as the controller runs: deleting a resource or its
namespace, deleting a CRD, and a GitOps tool pruning a resource that was removed from git. For anything you cannot
lose or recreate, annotate the resource `kflare.dev/deletion-policy: retain`, so that deleting it leaves the object in
Cloudflare:

- a `Zone` kflare created, since deleting a zone deletes every record in it
- a `KVNamespace` or `D1Database` holding data, since deleting one deletes its data
- an `R2Bucket` you want to keep (kflare never deletes objects in a bucket, but deletes an empty bucket)

Objects kflare adopted (`kflare.dev/adoption-policy`) are already kept when their resource is deleted.

### Moving to a new cluster

kflare keeps its record of what it owns in the cluster: the object's ID in the resource's status, and for some kinds
a claim label (see [Adoption and ownership](../ARCHITECTURE.md#adoption-and-ownership)). Rules are the exception: kflare
finds a rule by a ref stored in Cloudflare and derived from the resource's kind, namespace and name. Applied in a new
cluster, the same manifests carry no such record, so each resource whose object exists reports `AlreadyExists` and
changes nothing, while rules find their rules and carry on. To move kflare's resources to a new cluster:

1. **Stop the old installation**: uninstall it, or scale `deployment/kflare` to zero replicas and keep your GitOps tool
   from scaling it back. Two installations must never manage the same objects; for rules, both would find the same
   rule by its ref and keep rewriting it.
2. **In the new cluster, annotate the resources whose objects exist** with
   `kflare.dev/adoption-policy: adopt-or-create` (rules need nothing) and apply them. Each adopts its object and
   corrects it to the spec, which is what it already holds.
3. **Adopted objects are kept when their resource is deleted.** To have kflare delete an object with its resource, as
   it would have before the move, annotate the resource `kflare.dev/deletion-policy: delete`.
4. **Retire the old cluster.** Deleting the cluster leaves the Cloudflare objects alone. To keep the cluster, remove
   the finalizers from the old kflare resources by hand (see [Uninstalling](#uninstalling)), which also leaves the
   objects in place; never delete them while the old controller still runs.

This procedure follows from the adoption rules above, but has not been run end to end yet: try it with a test zone
first.

## Upgrading

```sh
helm upgrade kflare oci://ghcr.io/jonathangraniero/charts/kflare --version <new-version> -n kflare-system \
  --values kflare-values.yaml
```

With Argo CD or Flux, change `targetRevision` or the `OCIRepository`'s tag in git instead.

The CRDs are part of the chart's templates, so `helm upgrade` upgrades them too. (Helm's `crds/` directory
would only install them once.) To manage CRDs yourself, for example through GitOps with a separate sync
wave, set `crds.enabled=false` and apply `config/crd/bases` from the matching release first.

kflare's API is `v1alpha1`: read the release notes before upgrading, as fields can still change between
releases.

## Uninstalling

Order matters, because kflare's finalizers need the controller running to clean up:

1. **Delete your kflare resources while the controller is still running.** Each one deletes the Cloudflare
   object it created, and keeps one it adopted or one annotated `retain`. The order does not matter: Zones
   wait for their records, routes, rules and managed transforms, Tunnels for their configurations and the
   records that point at them, KV namespaces and R2 buckets for the Workers bound to them, IP lists for the
   rules that use them, accounts until nothing uses them, and token Secrets until no account uses them. An R2
   bucket that still holds objects is not deleted: empty it, or annotate the R2Bucket `retain`.

   ```sh
   kubectl delete zones.kflare.dev,dnsrecords.kflare.dev,workerroutes.kflare.dev,wafcustomrules.kflare.dev,ratelimitrules.kflare.dev,managedrulesets.kflare.dev,transformrules.kflare.dev,redirectrules.kflare.dev,cacherules.kflare.dev,configurationrules.kflare.dev,originrules.kflare.dev,managedtransforms.kflare.dev,iplists.kflare.dev,tunnels.kflare.dev,tunnelconfigurations.kflare.dev,workerscripts.kflare.dev,kvnamespaces.kflare.dev,r2buckets.kflare.dev,d1databases.kflare.dev --all -A
   kubectl delete cloudflareaccounts.kflare.dev --all
   ```

   With Argo CD or Flux, remove the resources from git and let the tool delete them, or delete their applications
   with cascading deletion.

2. **Uninstall the release.**

   ```sh
   helm uninstall kflare -n kflare-system
   ```

   With Argo CD or Flux, delete the kflare `Application` or `HelmRelease` instead.

   The CRDs stay (`helm.sh/resource-policy: keep`). Deleting a CRD deletes every resource of that kind,
   which, while a controller runs, deletes the matching Cloudflare objects.

3. **Remove the CRDs**, once no kflare resources are left:

   ```sh
   kubectl get crd -o name | grep '\.kflare\.dev$' | xargs kubectl delete
   ```

If the controller is already gone and resources are stuck in `Terminating`, remove the finalizer by hand.
Their Cloudflare objects are then left in place.

```sh
kubectl patch <kind>.kflare.dev <name> -n <namespace> --type=merge -p '{"metadata":{"finalizers":null}}'
```

## Troubleshooting

Every kflare resource reports a single `Ready` condition. Start with:

```sh
kubectl get <kind>.kflare.dev -A                      # READY column
kubectl describe <kind>.kflare.dev <name> -n <ns>     # condition reason and message
kubectl -n kflare-system logs deploy/kflare           # controller log (JSON)
```

| Reason | Meaning | What to do |
|---|---|---|
| `Synced`, `Validated` | In sync with Cloudflare | Nothing |
| `TerminalError` | Cloudflare rejected the request (4xx): bad input or missing permission | Read the message, fix the spec or token; kflare retries on the next change |
| `APIError` | Transient failure (5xx, rate limit, network) | Retried automatically with back-off |
| `AccountNotFound`, `AccountNotReady` | The referenced `CloudflareAccount` is missing or not `Ready` | Check the account's own condition |
| `SecretNotFound`, `TokenKeyMissing`, `InvalidToken` | The token Secret is missing, lacks the key, or is empty | Fix the Secret; the account re-validates immediately |
| `ZoneNotFound`, `ZoneNotReady`, `TunnelNotFound`, `TunnelNotReady` | The referenced parent is missing or not ready | Check the parent resource |
| `TunnelAlreadyConfigured` | Another `TunnelConfiguration` owns this tunnel | Keep one configuration per tunnel |
| `CredentialsSecretConflict` | A Secret with the tunnel's `credentialsSecretRef` name exists and kflare does not own it | Delete it or pick another name |
| `AlreadyExists` | The Cloudflare object the resource names (a zone, record, tunnel, Worker, bucket, ...) exists and kflare did not create it | Annotate the resource `kflare.dev/adoption-policy: adopt` to manage it, or pick another name |
| `NotFound` | The resource's adoption policy is `adopt` and there is nothing to adopt | Check the name, or use `adopt-or-create` to create one |
| `InvalidAdoptionPolicy` | `kflare.dev/adoption-policy` holds a value other than `adopt` and `adopt-or-create` | Fix or remove the annotation |
| `PatternConflict` | Another `WorkerRoute` already manages a route with this pattern | Keep one WorkerRoute per pattern |
| `PatternOutsideZone` | The route pattern's hostname is not in the referenced zone | Fix `spec.pattern` or `spec.zoneRef` |
| `WorkerScriptNotFound`, `WorkerScriptNotReady`, `AccountMismatch` | The route's WorkerScript is missing, not uploaded yet, or in a different account than the zone | Check the WorkerScript; it must use the zone's account |
| `AccountMismatch` (DNSRecord) | The Tunnel the record's `tunnelRef` names uses another `CloudflareAccount` than the record's Zone | Use the zone's account for the Tunnel: Cloudflare routes a tunnel's traffic only for DNS records in the tunnel's account |
| `TitleConflict` | Another `KVNamespace` already manages a namespace with this title | Keep one KVNamespace per title |
| `KVNamespaceNotFound`, `KVNamespaceNotReady` | A WorkerScript's `kvNamespaceRef` names a missing or not-yet-ready KVNamespace | Check the KVNamespace; it must use the Worker's account |
| `NameConflict` | Another `R2Bucket` already manages a bucket with this name in the same Cloudflare account, another `IPList` a list with this name, another `D1Database` a database with this name, or the account has a list of another kind with this name | Keep one R2Bucket per bucket, one IPList per list and one D1Database per database; list names are unique across list kinds |
| `R2BucketNotFound`, `R2BucketNotReady` | A WorkerScript's `r2BucketRef` names a missing or not-yet-ready R2Bucket | Check the R2Bucket; it must use the Worker's account |
| `ReplacingItems` | Cloudflare is replacing an `IPList`'s items in the background | Nothing: kflare checks again every 2 seconds |
| `RulesetConflict` | Another rule in the zone already deploys the `ManagedRuleset`'s managed ruleset: another ManagedRuleset, or a rule made in the dashboard or the API | Keep one ManagedRuleset per ruleset and zone, or delete the other rule; kflare checks again every minute |
| `ListInUse` | A deleted `IPList`'s list is still used by a rule, and Cloudflare does not delete it until no rule does | Remove the `$name` from the rules that use it (a kflare rule's goes with its resource), or annotate the IPList `kflare.dev/deletion-policy: retain` to keep the list |
| `BucketNotEmpty` | A deleted `R2Bucket`'s bucket still holds objects; kflare does not delete them | Empty the bucket, or annotate the R2Bucket `kflare.dev/deletion-policy: retain` to keep it |
| `InUse` | A `CloudflareAccount`, `Zone`, `Tunnel`, `KVNamespace` or `R2Bucket` being deleted is still referenced | Delete the resources listed in the message |
