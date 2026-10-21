# Expose a Service through a Cloudflare Tunnel

[`expose-a-service.yaml`](expose-a-service.yaml) puts a Service in your cluster on the internet at
`https://hello.<your domain>`, through a Cloudflare Tunnel: no public IP, load balancer or open inbound port. It holds:

- a demo application, `whoami`, and its Service: replace them with your own;
- a `Zone` that adopts your domain (`kflare.dev/adoption-policy: adopt`), so kflare uses the zone you already have and
  keeps it when the example is deleted;
- a `Tunnel`, whose token kflare writes to the Secret `hello-tunnel-token`;
- a `TunnelConfiguration` that routes `hello.<your domain>` to the Service;
- a `DNSRecord` with a `tunnelRef`: kflare points the hostname at the tunnel, a proxied CNAME to
  `<tunnel ID>.cfargotunnel.com`, so no tunnel ID is ever copied by hand;
- a `cloudflared` Deployment that connects the tunnel. kflare manages Cloudflare's side; `cloudflared` runs in the
  cluster like any other workload.

## Before you start

- kflare installed, and a `CloudflareAccount` named `my-account` reporting `READY=True`, as in the
  [deploy guide](../../docs/deploy.md), sections 2 and 3
- A domain on that Cloudflare account
- The account's API token holding, at least: Zone · Zone · Read and Zone · DNS · Edit on the domain, and
  Account · Cloudflare Tunnel · Edit (see the deploy guide's
  [permissions](../../docs/deploy.md#1-create-a-cloudflare-api-token))
- `hello.<your domain>` free: kflare creates no record where one already exists, and Cloudflare allows only one CNAME
  per name

## Run it

Replace `example.com` with your domain as you apply:

```sh
sed 's/example\.com/your-domain.com/g' examples/expose-a-service/expose-a-service.yaml | kubectl apply -f -
```

Everything is ready within a minute or so:

```sh
kubectl -n kflare-example get zones.kflare.dev,tunnels.kflare.dev,tunnelconfigurations.kflare.dev,dnsrecords.kflare.dev
kubectl -n kflare-example get pods
curl https://hello.your-domain.com
```

`curl` prints what `whoami` received, request headers included: Cloudflare adds `Cf-Connecting-Ip`. Until the
`cloudflared` pods are running, the hostname answers with Cloudflare's error 1033 (HTTP 530). If a resource is not
ready, `kubectl describe` shows why in its `Ready` condition; the deploy guide's
[troubleshooting](../../docs/deploy.md#troubleshooting) lists the reasons.

If `curl` cannot resolve the hostname although the DNSRecord is ready, your resolver may have looked the name up
before the record existed and cached the answer that it does not, which a Cloudflare zone lets resolvers keep for 30
minutes by default. Ask one of the zone's name servers directly, or wait:

```sh
kubectl -n kflare-example get zones.kflare.dev my-zone -o jsonpath='{.status.nameServers}'
dig @<one of them> hello.your-domain.com
```

## Clean up

```sh
kubectl delete namespace kflare-example
```

kflare deletes the DNS record, the tunnel's ingress rules and the tunnel, and keeps your zone, which it adopted.
