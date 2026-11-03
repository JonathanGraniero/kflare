#!/usr/bin/env bash
# Installs Argo CD on the cluster in the current kube context, with kflare's health check, so that you can watch
# kflare resources sync and turn Healthy, Progressing or Degraded in its UI. local/setup.sh runs it.
#
# Usage: local/argocd.sh [install|uninstall]
#
# ARGOCD_VERSION picks the release (default below). The health check is config/argocd/health.lua, put into
# argocd-cm as docs/deploy.md describes (TestDeployGuideHoldsTheHealthChecks keeps the two equal).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

ARGOCD_VERSION="${ARGOCD_VERSION:-v3.5.6}"
MANIFEST="https://raw.githubusercontent.com/argoproj/argo-cd/${ARGOCD_VERSION}/manifests/install.yaml"
NAMESPACE=argocd

install() {
  echo "==> Installing Argo CD ${ARGOCD_VERSION}..."
  kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
  # Server-side: the ApplicationSet CRD is too large for the annotation client-side apply writes.
  kubectl apply --server-side --force-conflicts -n "$NAMESPACE" -f "$MANIFEST"

  echo "==> Adding kflare's health check to argocd-cm..."
  # Patch, never apply: the file holds only what kflare adds (docs/deploy.md, "Argo CD"). The labels are what make
  # Argo CD read the ConfigMap at all.
  local dir
  dir="$(mktemp -d)"
  trap 'rm -rf "$dir"' RETURN
  {
    echo "kflare.dev/*:"
    echo "  health.lua: |"
    sed 's/^\(.\)/    \1/' "$ROOT_DIR/config/argocd/health.lua"
  } >"$dir/resource.customizations"
  kubectl create configmap argocd-cm -n "$NAMESPACE" --from-file="$dir/resource.customizations" \
    --dry-run=client -o yaml |
    kubectl label --local -f - -o yaml app.kubernetes.io/name=argocd-cm app.kubernetes.io/part-of=argocd \
      >"$dir/patch.yaml"
  kubectl -n "$NAMESPACE" patch configmap argocd-cm --patch-file "$dir/patch.yaml"

  echo "==> Waiting for Argo CD..."
  kubectl -n "$NAMESPACE" rollout status statefulset/argocd-application-controller --timeout=5m
  kubectl -n "$NAMESPACE" rollout status deployment --timeout=5m

  echo ""
  echo "==> Argo CD is ready. Open its UI with:"
  echo "    kubectl -n $NAMESPACE port-forward svc/argocd-server 8080:443"
  echo "    then https://localhost:8080, user admin, password:"
  echo "    kubectl -n $NAMESPACE get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d"
}

uninstall() {
  echo "==> Removing Argo CD..."
  kubectl delete -n "$NAMESPACE" -f "$MANIFEST" --ignore-not-found --timeout=3m
  kubectl delete namespace "$NAMESPACE" --ignore-not-found --timeout=3m
}

case "${1:-install}" in
install) install ;;
uninstall) uninstall ;;
*)
  echo "usage: $0 [install|uninstall]" >&2
  exit 2
  ;;
esac
