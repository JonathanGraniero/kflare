# Share a Zone with other namespaces

[`share-a-zone.yaml`](share-a-zone.yaml) keeps a Cloudflare zone with a platform team while an application team manages
its own DNS records in it, each in its own namespace. It holds:

- the namespaces `platform-dns` and `team-a`;
- a `Zone` in `platform-dns` that adopts your domain (`kflare.dev/adoption-policy: adopt`) and keeps it when the
  example is deleted. Its `spec.allowedReferences` lets `team-a` use it for `team-a.<your domain>` and every name below
  it;
- a `DNSRecord` in `team-a` for `api.team-a.<your domain>`, whose `zoneRef` names the Zone's namespace;
- a second `DNSRecord` in `team-a`, for `www.<your domain>`, which the Zone does not allow: it shows what a refused
  resource reports.

Without this, `team-a` would need a `Zone` of its own for the domain, and with it the whole zone: every hostname, its
rules and settings, and, with the `delete` deletion policy, the zone itself.

## Before you start

- kflare installed, and a `CloudflareAccount` named `my-account` reporting `READY=True`, as in the
  [deploy guide](../../docs/deploy.md), sections 2 and 3
- A domain on that Cloudflare account
- The account's API token holding, at least: Zone · Zone · Read and Zone · DNS · Edit on the domain (see the deploy
  guide's [permissions](../../docs/deploy.md#1-create-a-cloudflare-api-token))
- `api.team-a.<your domain>` free: kflare creates no record where one already exists

## Run it

Replace `example.com` with your domain as you apply:

```sh
sed 's/example\.com/your-domain.com/g' examples/share-a-zone/share-a-zone.yaml | kubectl apply -f -
```

The allowed record is ready within a minute or so, and the other one says why it is not:

```sh
kubectl -n team-a get dnsrecords.kflare.dev
kubectl -n team-a get dnsrecords.kflare.dev not-allowed \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}{"\n"}'
```

```text
Namespace "team-a" may not use Zone platform-dns/example-com for "www.your-domain.com": no entry of its
spec.allowedReferences that allows the namespace lists a hostname that covers it
```

## What to change

- **The hostnames.** A hostname allows itself alone, and one that starts with `*.` every name below the rest, but not
  the rest itself, so the example lists both.
- **The namespaces.** An entry's selector can match any labels, and `from: All` allows every namespace. Labels other
  than `kubernetes.io/metadata.name` trust whoever may label namespaces.
- **Worker routes.** A `WorkerRoute` takes `zoneRef.namespace` too: the hostname is what its `pattern` has before the
  path.
- **The account.** If your `CloudflareAccount` sets `spec.allowedNamespaces`, it needs to allow `platform-dns` only:
  `team-a` uses the account through the Zone.

[Sharing a Zone with other namespaces](../../docs/deploy.md#sharing-a-zone-with-other-namespaces) has the rules in
full.

## Clean up

```sh
kubectl delete namespace team-a
kubectl delete namespace platform-dns
```

The first deletes the record from Cloudflare. The second removes the `Zone` and keeps your zone, which it adopted. The
`Zone` waits for the records it allows in other namespaces, so delete `team-a` first, or the two together.
