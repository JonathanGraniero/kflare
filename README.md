# kflare

kflare is a Kubernetes operator for Cloudflare. It lets you manage Cloudflare resources — DNS zones, records, tunnels, Workers, and more — declaratively using Kubernetes custom resources, enabling full GitOps-driven infrastructure management.

Instead of clicking through the Cloudflare dashboard or scripting API calls, you define your desired Cloudflare state in YAML and apply it to your cluster. kflare reconciles the difference.

## What it does

- **Declarative Cloudflare management** — define DNS records, zones, tunnels, and Workers as Kubernetes CRDs
- **GitOps-ready** — store your Cloudflare config in git alongside your app manifests
- **Credential management** — reference Kubernetes Secrets for API tokens, with support for external-secrets
- **Drift detection** — compares desired state against the live Cloudflare API on every reconcile and corrects any difference. Changes made outside kflare are picked up on the next reconcile, not immediately; see [when drift is detected](./ARCHITECTURE.md#when-drift-is-detected)
- **Import existing resources** — adopt pre-existing Cloudflare resources into management without recreating them. Adopted resources are overwritten to match the spec and deleted with their Kubernetes resource unless you set `kflare.dev/deletion-policy: retain`; see [adoption and ownership](./ARCHITECTURE.md#adoption-and-ownership)

For how the controllers work, including known limitations, see [ARCHITECTURE.md](./ARCHITECTURE.md).

## Status

Early development. Phase 1 (foundation) and Phase 2 (core resources) are complete. See [CLAUDE.md](./CLAUDE.md) for the full phased implementation plan.

**Currently implemented:**
- `CloudflareAccount` — cluster-scoped credential store, validates your API token against the Cloudflare API on reconcile
- `Zone` — creates or adopts a DNS zone and reports its name servers and activation status
- `DNSRecord` — manages a record in a `Zone`, correcting drift in content, TTL, proxying, priority, comment, tags and structured data
- `Tunnel` — creates or adopts a remotely-managed Cloudflare Tunnel and writes its token to a Secret under `TUNNEL_TOKEN`, ready for `cloudflared tunnel run` (see [the sample](config/samples/cloudflare_v1alpha1_tunnel.yaml))
- `TunnelConfiguration` — owns a Tunnel's ingress rules (hostname → service), pushes them to Cloudflare and reverts changes made outside kflare (see [the sample](config/samples/cloudflare_v1alpha1_tunnelconfiguration.yaml))
- `WorkerScript` — uploads a Worker from inline code or a ConfigMap, with plain text, secret (from a Kubernetes Secret), KV and R2 bindings, and re-uploads it when it is changed outside kflare (see [the sample](config/samples/cloudflare_v1alpha1_workerscript.yaml))

**Planned next:**
- `WorkerRoute` — routes a zone URL pattern to a `WorkerScript`

## Prerequisites

- Go 1.22+
- kubectl
- kind (for local development)
- A Cloudflare account and API token with **Edit zone DNS** permissions
  - `Tunnel` also needs the account-level **Cloudflare Tunnel: Edit** permission
  - `WorkerScript` also needs the account-level **Workers Scripts: Edit** permission
  - Setting `Zone` `spec.plan` also needs **Billing: Edit**, and changes your bill

## Local Development

### 1. Set your Cloudflare credentials

```sh
export CF_API_TOKEN=<your-api-token>
export CF_ACCOUNT_ID=<your-account-id>
```

### 2. Spin up a local cluster

This creates a kind cluster, installs the CRDs, and applies a `CloudflareAccount` CR pointing at your real Cloudflare account:

```sh
./local/setup.sh
```

### 3. Run the controller

In a separate terminal, run the controller locally against the kind cluster:

```sh
make run
```

### 4. Verify it's working

```sh
kubectl get cloudflareaccount my-account -o yaml
```

You should see the `Ready` condition set to `True` and your account name populated in the status:

```yaml
status:
  accountName: your-account-name
  conditions:
  - type: Ready
    status: "True"
    reason: Validated
    message: Credentials are valid and account is reachable
```

### 5. Inspect with k9s

```sh
k9s
```

From k9s, type `:cloudflareaccounts` to browse CRs, or `:secrets` to inspect the credentials secret in `kflare-system`.

### Tear down

```sh
./local/teardown.sh
```

## Building

```sh
# Generate CRDs and deepcopy functions
make generate manifests

# Build the controller binary
make build

# Run unit tests
make test
```

## License

MIT License — Copyright (c) 2026 Jonathan Graniero. See [LICENSE](./LICENSE) for details.
