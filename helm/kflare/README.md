# kflare Helm chart

Installs [kflare](https://github.com/JonathanGraniero/kflare), a Kubernetes operator that manages Cloudflare through
custom resources: zones, DNS records, tunnels, Workers and their storage (KV, R2, D1), WAF rules and the Rules
products. kflare is an alpha: its API is `v1alpha1`.

```sh
helm install kflare oci://ghcr.io/jonathangraniero/charts/kflare \
  --version <version> --namespace kflare-system --create-namespace
```

Requires Kubernetes 1.34 or newer and Helm 3.8 or newer. Install one release per cluster.

Then connect a Cloudflare account. The
[deploy guide](https://github.com/JonathanGraniero/kflare/blob/main/docs/deploy.md) covers API token permissions,
accounts, installing with Argo CD or Flux, running in production, upgrades and uninstalling.

## What the chart installs

- The kflare CRDs, as templates so that upgrades upgrade them, kept on uninstall (`crds.keep`)
- The controller Deployment and its ServiceAccount, under the restricted Pod Security Standard
- A ClusterRole for the controller, and a Role for leader election in the release namespace
- Optionally, ClusterRoles that add kflare's kinds to the built-in `view`, `edit` and `admin` roles
- A Service for metrics, served over HTTPS to callers allowed by the `<release>-metrics-reader` ClusterRole, and
  optionally a Prometheus Operator ServiceMonitor
- A PodDisruptionBudget when `replicaCount` is above 1
- Optionally, a NetworkPolicy

Before uninstalling, delete your kflare resources while the controller still runs, so it can clean up the Cloudflare
objects it created; see
[Uninstalling](https://github.com/JonathanGraniero/kflare/blob/main/docs/deploy.md#uninstalling).

## Values

All values are documented in [`values.yaml`](values.yaml) and validated by
[`values.schema.json`](values.schema.json). The most commonly changed:

| Key | Default | Description |
|---|---|---|
| `image.repository` | `ghcr.io/jonathangraniero/kflare` | Controller image |
| `image.tag` | chart `appVersion` | Image tag |
| `imagePullSecrets` | `[]` | Secrets for pulling the image from a private registry or mirror |
| `replicaCount` | `1` | Replicas; leader election keeps one active |
| `crds.enabled` | `true` | Install and upgrade the CRDs with the chart |
| `crds.keep` | `true` | Keep the CRDs on uninstall |
| `rbac.aggregateToDefaultRoles` | `true` | Extend the built-in `view`/`edit`/`admin` roles |
| `leaderElection` | `true` | Run with leader election |
| `logging.level` | `info` | `debug`, `info` or `error` |
| `logging.format` | `json` | `json` or `console` |
| `metrics.enabled` | `true` | Serve Prometheus metrics on `metrics.port` |
| `metrics.secure` | `true` | HTTPS, only to tokens allowed `get` on `/metrics`; `false`: plain HTTP |
| `metrics.certSecret` | unset | Secret with the metrics certificate (`tls.crt`, `tls.key`, `ca.crt`) |
| `metrics.serviceMonitor.enabled` | `false` | Create a Prometheus Operator ServiceMonitor |
| `podDisruptionBudget.enabled` | `true` | Create a PodDisruptionBudget (`maxUnavailable: 1`) when `replicaCount` > 1 |
| `networkPolicy.enabled` | `false` | Allow only metrics scrapes in, and DNS, the API server and port 443 out |
| `resources` | 10m/64Mi requests, 256Mi limit | Controller resources |
| `topologySpreadConstraints`, `affinity`, `nodeSelector`, `tolerations`, `priorityClassName` | unset | Pod scheduling |
| `extraEnv` | `[]` | Extra environment variables, such as `HTTPS_PROXY` and `NO_PROXY` |
| `extraArgs` | `[]` | Extra controller arguments |
