#!/usr/bin/env bash
# Installs Prometheus and Grafana (kube-prometheus-stack) on the kind cluster in the current kube context, with a kflare
# dashboard, so that you can watch kflare's metrics while the e2e suite runs. local/setup.sh runs it. Prometheus
# scrapes both ways the suite runs the manager:
#   - make test-e2e installs the chart, and turns its ServiceMonitor on when the cluster has the ServiceMonitor CRD
#   - hack/e2e-live.sh runs the manager on this machine, and serves its metrics where the kflare-host-manager
#     EndpointSlice in the monitoring namespace points: the kind network's gateway, which is this machine
#
# Usage: local/prometheus.sh [install|uninstall]
#
# KUBE_PROMETHEUS_STACK_VERSION picks the chart's version (default below); local/prometheus/values.yaml says what of it
# runs.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

CHART=oci://ghcr.io/prometheus-community/charts/kube-prometheus-stack
VERSION="${KUBE_PROMETHEUS_STACK_VERSION:-92.11.0}"
RELEASE=prometheus
NAMESPACE=monitoring
# The port hack/e2e-live.sh's manager serves its metrics on.
HOST_MANAGER_PORT=18080

# kind_gateway prints the gateway of the network the current kind cluster's node is on, the address this machine has
# on that network, or nothing for a cluster that is not kind's.
kind_gateway() {
  local context
  context="$(kubectl config current-context)"
  [[ "$context" == kind-* ]] || return 0
  docker inspect "${context#kind-}-control-plane" \
    --format '{{range .NetworkSettings.Networks}}{{.Gateway}}{{end}}' 2>/dev/null || true
}

install() {
  echo "==> Installing kube-prometheus-stack ${VERSION}..."
  helm upgrade --install "$RELEASE" "$CHART" --version "$VERSION" --namespace "$NAMESPACE" --create-namespace \
    --values "$SCRIPT_DIR/prometheus/values.yaml" --wait --timeout 10m
  # The operator starts Prometheus itself, after Helm's wait.
  kubectl wait -n "$NAMESPACE" "prometheus/${RELEASE}-kube-prometheus-prometheus" --for=condition=Available \
    --timeout=5m

  echo "==> Adding kflare's dashboard to Grafana..."
  # Server-side: the dashboard is too large for the annotation client-side apply writes.
  kubectl create configmap kflare-dashboard -n "$NAMESPACE" --from-file="$SCRIPT_DIR/prometheus/kflare-dashboard.json" \
    --dry-run=client -o yaml |
    kubectl label --local -f - -o yaml grafana_dashboard=1 |
    kubectl apply --server-side --force-conflicts -f -

  local gateway
  gateway="$(kind_gateway)"
  if [[ "$gateway" =~ ^[0-9.]+$ ]]; then
    echo "==> Pointing Prometheus at a manager on this machine (${gateway}:${HOST_MANAGER_PORT}, hack/e2e-live.sh)..."
    # A Service without a selector, whose one endpoint is this machine. The manager serves its metrics as the chart
    # does: over HTTPS with a self-signed certificate, to a token allowed to get /metrics, as Prometheus' own is.
    kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: kflare-host-manager
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: kflare-host-manager
spec:
  ports:
    - name: metrics
      port: ${HOST_MANAGER_PORT}
---
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata:
  name: kflare-host-manager
  namespace: ${NAMESPACE}
  labels:
    kubernetes.io/service-name: kflare-host-manager
    endpointslice.kubernetes.io/managed-by: local-prometheus.kflare.dev
addressType: IPv4
ports:
  - name: metrics
    port: ${HOST_MANAGER_PORT}
    protocol: TCP
endpoints:
  - addresses: ["${gateway}"]
---
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: kflare-host-manager
  namespace: ${NAMESPACE}
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: kflare-host-manager
  endpoints:
    - port: metrics
      scheme: https
      bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
      tlsConfig:
        insecureSkipVerify: true
EOF
  else
    echo "==> Not a kind cluster on Docker: Prometheus will not scrape a manager on this machine (hack/e2e-live.sh)"
  fi

  echo ""
  echo "==> Prometheus and Grafana are ready. Open kflare's dashboard with:"
  echo "    kubectl -n $NAMESPACE port-forward svc/${RELEASE}-grafana 3000:80"
  echo "    then http://localhost:3000/d/kflare (no login needed to view; the admin password, to edit, is:"
  echo "    kubectl -n $NAMESPACE get secret ${RELEASE}-grafana -o jsonpath='{.data.admin-password}' | base64 -d)"
  echo "==> And Prometheus, to see its targets or run a query, with:"
  echo "    kubectl -n $NAMESPACE port-forward svc/${RELEASE}-kube-prometheus-prometheus 9090"
  echo "    then http://localhost:9090/targets"
}

uninstall() {
  echo "==> Removing Prometheus and Grafana..."
  helm uninstall "$RELEASE" --namespace "$NAMESPACE" --ignore-not-found --wait
  # With the dashboard and the objects that point at a manager on this machine.
  kubectl delete namespace "$NAMESPACE" --ignore-not-found --timeout=3m
  # Helm leaves the CRDs a chart installs. Without them, make test-e2e no longer turns the chart's ServiceMonitor on.
  kubectl get crd -o name | grep '\.monitoring\.coreos\.com$' | xargs -r kubectl delete --timeout=2m
}

case "${1:-install}" in
install) install ;;
uninstall) uninstall ;;
*)
  echo "usage: $0 [install|uninstall]" >&2
  exit 2
  ;;
esac
