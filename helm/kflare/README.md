# kflare Helm chart

Installs [kflare](https://github.com/JonathanGraniero/kflare), a Kubernetes operator that manages Cloudflare
zones, DNS records, tunnels and Workers as custom resources.

```sh
helm install kflare oci://ghcr.io/jonathangraniero/charts/kflare \
  --version <version> --namespace kflare-system --create-namespace
```

Then connect a Cloudflare account. The [deploy guide](../../docs/deploy.md) walks through tokens, accounts,
upgrades and uninstalling.

## Values

All values are documented in [`values.yaml`](values.yaml) and validated by
[`values.schema.json`](values.schema.json). The most commonly changed:

| Key | Default | Description |
|---|---|---|
| `image.repository` | `ghcr.io/jonathangraniero/kflare` | Controller image |
| `image.tag` | chart `appVersion` | Image tag |
| `replicaCount` | `1` | Replicas; leader election keeps one active |
| `crds.enabled` | `true` | Install and upgrade the CRDs with the chart |
| `crds.keep` | `true` | Keep the CRDs on uninstall |
| `rbac.aggregateToDefaultRoles` | `true` | Extend the built-in `view`/`edit`/`admin` roles |
| `leaderElection` | `true` | Run with leader election |
| `logging.level` | `info` | `debug`, `info` or `error` |
| `logging.format` | `json` | `json` or `console` |
| `metrics.enabled` | `true` | Serve Prometheus metrics on `metrics.port` |
| `metrics.serviceMonitor.enabled` | `false` | Create a Prometheus Operator ServiceMonitor |
| `resources` | 10m/64Mi requests, 256Mi limit | Controller resources |

## Maintaining the chart

`templates/crds/` and `files/manager-rules.yaml` are generated from `config/` by
`hack/sync-helm-chart.sh`. Run `make helm` after changing API types or RBAC markers; CI fails when they
are out of sync. Pushing a `vX.Y.Z` tag that matches `version`/`appVersion` in `Chart.yaml` publishes the
image and this chart to `ghcr.io`.
