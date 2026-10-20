# Deploying kflare with Helm

This guide installs the kflare controller into a Kubernetes cluster, connects it to a Cloudflare
account, and covers upgrades, uninstalling and troubleshooting.

- [Requirements](#requirements)
- [1. Create a Cloudflare API token](#1-create-a-cloudflare-api-token)
- [2. Install the chart](#2-install-the-chart)
- [3. Connect your Cloudflare account](#3-connect-your-cloudflare-account)
- [4. Manage Cloudflare resources](#4-manage-cloudflare-resources)
- [Configuration](#configuration)
- [Security model](#security-model)
- [Upgrading](#upgrading)
- [Uninstalling](#uninstalling)
- [Local development with kind](#local-development-with-kind)
- [Troubleshooting](#troubleshooting)

## Requirements

- Kubernetes 1.34 or newer. The chart refuses to install on older clusters.
- Helm 3.8 or newer (OCI chart support).
- One kflare installation per cluster. A second release would reconcile the same resources.

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
| `IPList` | Account · Account Rule Lists · Edit |

These are enough on their own: in October 2026 a token holding only them (with Zone · Zone · Read, and without Billing)
ran kflare's full live test suite against an existing zone.
Scope zone permissions to the zones kflare should manage. Note the account ID, shown on the account's overview page.

## 2. Install the chart

From a release, the chart and image are published to GitHub Container Registry:

```sh
helm install kflare oci://ghcr.io/jonathangraniero/charts/kflare \
  --version <version> \
  --namespace kflare-system --create-namespace
```

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
- `view`/`edit` aggregation roles (see [Security model](#security-model))

Wait for the controller:

```sh
kubectl -n kflare-system rollout status deployment/kflare
```

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
| `DNSRecord` | Namespaced | `zoneRef` (same namespace) | [A](../config/samples/cloudflare_v1alpha1_dnsrecord_a.yaml), [MX](../config/samples/cloudflare_v1alpha1_dnsrecord_mx.yaml), [SRV](../config/samples/cloudflare_v1alpha1_dnsrecord_srv.yaml) |
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

Things to know before pointing kflare at existing infrastructure:

- **Adoption.** A resource that already exists in Cloudflare with the same name is adopted rather than
  recreated, and kflare then corrects drift toward your spec.
- **Deletion policy.** Deleting a kflare resource deletes the Cloudflare object, unless the resource carries
  the annotation `kflare.dev/deletion-policy: retain`. Use `retain` on anything you adopted and want to keep,
  such as a production zone.
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
- **IP lists.** An `IPList` manages a custom list in its account, adopting a list with the same name, and
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
- **Immutable fields.** Names and references (`spec.name`, `accountRef`, `zoneRef`, DNS `type`, ...) cannot be
  changed. Create a new resource instead.

## Configuration

Every value is documented in [`helm/kflare/values.yaml`](../helm/kflare/values.yaml), and
[`values.schema.json`](../helm/kflare/values.schema.json) rejects unknown or invalid values at install time.
Common settings:

```yaml
# High availability: one replica is active, the others wait on the leader lease.
replicaCount: 2
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: kubernetes.io/hostname
    whenUnsatisfiable: ScheduleAnyway
    labelSelector:
      matchLabels:
        app.kubernetes.io/name: kflare

# Prometheus Operator scraping.
metrics:
  serviceMonitor:
    enabled: true
    labels:
      release: prometheus   # match your Prometheus' serviceMonitorSelector

logging:
  level: debug              # debug, info or error
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
- **Metrics** are served over plain HTTP without authentication on port 8080, inside the cluster only.
  Restrict access with a NetworkPolicy if your metrics are sensitive.

## Upgrading

```sh
helm upgrade kflare oci://ghcr.io/jonathangraniero/charts/kflare --version <new-version> -n kflare-system
```

The CRDs are part of the chart's templates, so `helm upgrade` upgrades them too. (Helm's `crds/` directory
would only install them once.) To manage CRDs yourself, for example through GitOps with a separate sync
wave, set `crds.enabled=false` and apply `config/crd/bases` from the matching release first.

kflare's API is `v1alpha1`: read the release notes before upgrading, as fields can still change between
releases.

**From pre-release builds using `cloudflare.cloudflare.k8s.io`:** the API group moved to `kflare.dev`, and
objects are not migrated. Remove the old finalizers (`cloudflare.k8s.io/finalizer`) from existing objects,
delete the old CRDs, and re-apply your manifests with `apiVersion: kflare.dev/v1alpha1`. While both CRD sets
exist, short names such as `kubectl get cloudflareaccounts` can resolve to either group; use the full name
(`cloudflareaccounts.kflare.dev`).

## Uninstalling

Order matters, because kflare's finalizers need the controller running to clean up:

1. **Delete your kflare resources while the controller is still running.** Each one deletes its Cloudflare
   object, or keeps it if annotated `retain`. The order does not matter: Zones wait for their records,
   routes and rules, Tunnels for their configurations, KV namespaces and R2 buckets for the Workers bound
   to them, IP lists for the rules that use them, accounts until nothing uses them, and token Secrets until
   no account uses them. An R2 bucket that still holds objects is not deleted: empty it, or annotate the
   R2Bucket `retain`.

   ```sh
   kubectl delete zones.kflare.dev,dnsrecords.kflare.dev,workerroutes.kflare.dev,wafcustomrules.kflare.dev,ratelimitrules.kflare.dev,managedrulesets.kflare.dev,transformrules.kflare.dev,redirectrules.kflare.dev,cacherules.kflare.dev,configurationrules.kflare.dev,originrules.kflare.dev,iplists.kflare.dev,tunnels.kflare.dev,tunnelconfigurations.kflare.dev,workerscripts.kflare.dev,kvnamespaces.kflare.dev,r2buckets.kflare.dev,d1databases.kflare.dev --all -A
   kubectl delete cloudflareaccounts.kflare.dev --all
   ```

2. **Uninstall the release.**

   ```sh
   helm uninstall kflare -n kflare-system
   ```

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

## Local development with kind

Build the image from your checkout, load it into kind, and install the chart from the repository:

```sh
make docker-build IMG=kflare:dev
kind load docker-image kflare:dev --name kflare-dev
helm upgrade --install kflare ./helm/kflare -n kflare-system --create-namespace \
  --set image.repository=kflare,image.tag=dev
```

Run `make helm` after changing API types or RBAC markers. It regenerates the chart's CRDs and RBAC rules
from `config/`, and CI fails if they are out of sync. `make helm-lint` lints and renders the chart.

If pods cannot reach the API server (`dial tcp 10.96.0.1:443: i/o timeout`) and `kube-proxy` crash-loops with
`too many open files`, the host's inotify limits are too low for the number of kind clusters. Raise them:

```sh
sudo sysctl fs.inotify.max_user_instances=512 fs.inotify.max_user_watches=524288
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
| `PatternConflict` | Another `WorkerRoute` already manages a route with this pattern | Keep one WorkerRoute per pattern |
| `PatternOutsideZone` | The route pattern's hostname is not in the referenced zone | Fix `spec.pattern` or `spec.zoneRef` |
| `WorkerScriptNotFound`, `WorkerScriptNotReady`, `AccountMismatch` | The route's WorkerScript is missing, not uploaded yet, or in a different account than the zone | Check the WorkerScript; it must use the zone's account |
| `TitleConflict` | Another `KVNamespace` already manages a namespace with this title | Keep one KVNamespace per title |
| `KVNamespaceNotFound`, `KVNamespaceNotReady` | A WorkerScript's `kvNamespaceRef` names a missing or not-yet-ready KVNamespace | Check the KVNamespace; it must use the Worker's account |
| `NameConflict` | Another `R2Bucket` already manages a bucket with this name in the same Cloudflare account, another `IPList` a list with this name, another `D1Database` a database with this name, or the account has a list of another kind with this name | Keep one R2Bucket per bucket, one IPList per list and one D1Database per database; list names are unique across list kinds |
| `R2BucketNotFound`, `R2BucketNotReady` | A WorkerScript's `r2BucketRef` names a missing or not-yet-ready R2Bucket | Check the R2Bucket; it must use the Worker's account |
| `ReplacingItems` | Cloudflare is replacing an `IPList`'s items in the background | Nothing: kflare checks again every 2 seconds |
| `RulesetConflict` | Another rule in the zone already deploys the `ManagedRuleset`'s managed ruleset: another ManagedRuleset, or a rule made in the dashboard or the API | Keep one ManagedRuleset per ruleset and zone, or delete the other rule; kflare checks again every minute |
| `ListInUse` | A deleted `IPList`'s list is still used by a rule, and Cloudflare does not delete it until no rule does | Remove the `$name` from the rules that use it (a kflare rule's goes with its resource), or annotate the IPList `kflare.dev/deletion-policy: retain` to keep the list |
| `BucketNotEmpty` | A deleted `R2Bucket`'s bucket still holds objects; kflare does not delete them | Empty the bucket, or annotate the R2Bucket `kflare.dev/deletion-policy: retain` to keep it |
| `InUse` | A `CloudflareAccount`, `Zone`, `Tunnel`, `KVNamespace` or `R2Bucket` being deleted is still referenced | Delete the resources listed in the message |
