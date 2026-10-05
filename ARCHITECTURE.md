# kflare architecture

This document explains how kflare's controllers behave and why: how they find and own Cloudflare resources, when they
notice drift, how they handle errors, and what happens on deletion. It describes the code as it is today, including
its known limitations. The phased roadmap lives in [CLAUDE.md](./CLAUDE.md).

## Resource model

```
Secret (API token)
  └─ CloudflareAccount (cluster-scoped)
       ├─ Zone (namespaced)
       │    └─ DNSRecord (same namespace, via zoneRef)
       ├─ Tunnel (namespaced)
       │    └─ TunnelConfiguration (same namespace, via tunnelRef)
       └─ WorkerScript (namespaced)
```

Every namespaced resource resolves its credentials through this chain at reconcile time: a DNSRecord reads its Zone, the
Zone's CloudflareAccount, and the account's token Secret. Nothing caches a token between reconciles, so rotating the
Secret takes effect on the next reconcile.

Each controller declares a narrow interface over the Cloudflare SDK (for example `DNSRecordAPI`) holding only the calls it
makes. The production client (`pkg/cloudflare.Client`) embeds the SDK and satisfies all of them; tests inject fakes.

## The reconcile loop

Every controller follows the same shape:

1. Handle deletion first if the resource is being deleted (see [Deletion](#deletion)).
2. Add the `kflare.dev/finalizer` finalizer and return; the update re-triggers the reconcile.
3. Resolve the parent resource and credentials. If the parent is missing or not ready, report `Ready=False` and wait
   for the parent's watch event.
4. Observe: fetch the Cloudflare resource by the ID stored in status. If there is no ID, or Cloudflare says it no longer
   exists, find an existing resource to adopt or create a new one (see [Adoption and ownership](#adoption-and-ownership)).
5. Compare the spec with what Cloudflare reported, and write to Cloudflare only when they differ.
6. Record IDs and metadata in status and set `Ready=True`.

Step 5 is deliberate: an idle reconcile makes read calls only. The live e2e suite checks this for DNSRecord, and
TunnelConfiguration's comparison ignores fields Cloudflare adds on its own so an unchanged config does not bump its
version.

## When drift is detected

kflare corrects drift whenever a resource is reconciled, but **a change made in the Cloudflare dashboard or API does not
by itself trigger a reconcile**. Reconciles are triggered by:

- a change to the resource itself, including a metadata-only change such as an annotation;
- a change to a watched dependency (for example a Zone becoming ready re-triggers its DNSRecords; a ConfigMap or Secret
  change re-triggers WorkerScripts that use it);
- a scheduled retry after an error (see below);
- the informer resync, which re-reconciles every resource. kflare does not set a sync period, so it uses
  controller-runtime's default of **10 hours**.

A successful reconcile does not schedule another one. In practice, a record edited in the dashboard can stay changed for
up to about 10 hours before kflare reverts it.

To force a reconcile, change the resource's metadata:

```sh
kubectl annotate dnsrecord my-record kflare.dev/reconcile-at="$(date +%s)" --overwrite
```

What each controller compares when it does reconcile:

| Resource | Drift detected on |
|---|---|
| `CloudflareAccount` | Token validity (re-validated against the account on every reconcile) |
| `Zone` | Zone type and `spec.plan`; recreated if deleted externally |
| `DNSRecord` | Content, TTL, proxied, priority (when set), comment, tags, structured data; recreated if deleted externally |
| `Tunnel` | Existence (recreated if deleted externally); token Secret contents |
| `TunnelConfiguration` | The full ingress configuration |
| `WorkerScript` | Cloudflare's `modified_on` timestamp, because its etag does not change for binding-only edits |

## Adoption and ownership

When a resource has no Cloudflare ID yet, kflare looks for an existing Cloudflare object before creating one. This lets
you bring existing infrastructure under management, and it lets a reconcile recover if it created an object but failed
to record its ID in status.

Adoption means kflare takes full ownership: it will overwrite the object to match the spec, and **by default it will
delete the object when the Kubernetes resource is deleted**. If you adopt something kflare did not create, set the
retain policy first (see [Deletion](#deletion)).

| Resource | What gets adopted |
|---|---|
| `Zone` | A zone with `spec.name` in the referenced account. Zones in other accounts the token can reach are never adopted. |
| `DNSRecord` | A record with the same name and type that no other DNSRecord manages. A record whose content (or data) already matches is preferred. If none matches, an existing record is adopted and corrected only when it is the only record with that name and type; otherwise a new record is created alongside the others, so DNSRecords for record sets (several MX, TXT or A records) each find their own. |
| `Tunnel` | The first non-deleted tunnel named `spec.name` in the account. |
| `TunnelConfiguration` | The tunnel's entire ingress configuration. kflare replaces whatever is there. When several TunnelConfigurations point at one Tunnel, the oldest owns it and the others report `TunnelAlreadyConfigured`. |
| `WorkerScript` | No lookup. The first reconcile uploads the script, replacing any existing Worker with the same name. |

### DNSRecord ownership

DNSRecord stores the Cloudflare record ID it manages in the `kflare.dev/record-id` label and claims a record before doing
anything else with it. Adoption skips any record another DNSRecord already carries in that label, across all namespaces,
because two Zone resources in different namespaces can point at the same Cloudflare zone. Cloudflare record tags would be
a Cloudflare-side alternative, but they are only available on paid plans.

This prevents two DNSRecords from fighting over one record. It does **not** protect records that were created outside
kflare: a lone record with a matching name and type is adopted and overwritten. Tools such as external-dns avoid this
with a TXT ownership registry; kflare currently does not.

## Error handling

`pkg/cloudflare.IsTerminalError` sorts Cloudflare errors into two groups, and `handleCloudflareError` acts on them:

| Error | Classification | Behavior |
|---|---|---|
| 401, 403, other 4xx | Terminal | `Ready=False` with reason `TerminalError`; no retry. A spec or credential change re-triggers the reconcile. |
| 404 on a known resource ID | Handled first | The resource was deleted externally; kflare adopts or recreates it. |
| 404 elsewhere | Terminal | As above. |
| 429, 5xx, network errors | Retryable | `Ready=False` with reason `APIError`; the error is returned so controller-runtime retries with exponential back-off (5 ms doubling up to about 16 minutes per resource). |

cloudflare-go names its 401 error `AuthorizationError` and its 403 error `AuthenticationError`, the reverse of what the
names suggest; `pkg/cloudflare/errors.go` documents the mapping.

Failures caused by something a resource does not watch, such as a missing account or token Secret, retry every minute
with reason `AccountNotFound`, `AccountNotReady`, `SecretNotFound` or `TokenKeyMissing`. Terminal errors are also
retried by the 10-hour resync.

## Deletion

Each resource carries `kflare.dev/finalizer`. On deletion, kflare deletes the Cloudflare object and then removes the
finalizer. Set the `kflare.dev/deletion-policy: retain` annotation to keep the Cloudflare object instead:

```yaml
metadata:
  annotations:
    kflare.dev/deletion-policy: retain
```

Per-resource details:

- **Tunnel** removes the tunnel's connections first, which immediately disconnects any running `cloudflared`, then deletes
  the tunnel.
- **TunnelConfiguration** resets the tunnel to its catch-all rule rather than deleting anything.
- **WorkerScript** deletes the Worker only if kflare uploaded it.
- **DNSRecord** and **TunnelConfiguration** skip the Cloudflare call when their parent Zone or Tunnel resource is
  already gone. The parent was either deleted from Cloudflare (taking its children with it) or deliberately retained, and
  skipping keeps namespace deletion from hanging. If the parent was retained, its children's Cloudflare objects are left
  in place.

Deleting resources in any order must not strand one that still needs the API token for its own cleanup. Two finalizers
chain the deletions:

- A `CloudflareAccount` keeps its finalizer, reporting `Ready=False` with reason `InUse`, until no Zone, Tunnel or
  WorkerScript references it.
- The account's token Secret carries `kflare.dev/token-protection` until no `CloudflareAccount` references it.

## Known limitations

- **Drift is not corrected promptly.** Out-of-band changes wait for the next trigger, at worst the 10-hour resync. A
  configurable periodic requeue on successful reconciles would bound this; it needs to stay within Cloudflare's API
  rate limits when many resources are managed.
- **Adoption is implicit.** Zone, DNSRecord and Tunnel adopt matching objects automatically, WorkerScript overwrites a
  same-named Worker, and the default deletion policy applies to adopted objects. An explicit opt-in for adopting
  objects kflare did not create would make this safer.
- **No ownership marker on the Cloudflare side.** kflare tracks what it owns only in Kubernetes. If the cluster's
  resources are lost, kflare cannot tell its objects apart from anyone else's.
- **Pinned to the legacy SDK.** kflare uses cloudflare-go v0.89. Some limitations come from it: Tunnel renames are
  impossible because `UpdateTunnel` omits the tunnel ID from the request path, and `originRequest` timeouts are not
  exposed because `TunnelDuration` does not round-trip.
