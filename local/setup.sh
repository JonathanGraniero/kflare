#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# Verify required env vars
if [[ -z "${CF_API_TOKEN:-}" || -z "${CF_ACCOUNT_ID:-}" ]]; then
  echo "ERROR: CF_API_TOKEN and CF_ACCOUNT_ID must be set."
  exit 1
fi

echo "==> Creating kind cluster..."
kind create cluster --config "$SCRIPT_DIR/kind-config.yaml" --wait 60s

echo "==> Installing CRDs..."
cd "$ROOT_DIR"
make manifests
kubectl apply -k config/crd/

echo "==> Creating kflare-system namespace..."
kubectl create namespace kflare-system --dry-run=client -o yaml | kubectl apply -f -

echo "==> Creating Cloudflare credentials secret..."
kubectl create secret generic cloudflare-credentials \
  --namespace kflare-system \
  --from-literal=CF_API_TOKEN="$CF_API_TOKEN" \
  --dry-run=client -o yaml | kubectl apply -f -

echo "==> Applying CloudflareAccount CR..."
kubectl apply -f - <<EOF
apiVersion: cloudflare.cloudflare.k8s.io/v1alpha1
kind: CloudflareAccount
metadata:
  name: my-account
spec:
  accountID: "${CF_ACCOUNT_ID}"
  tokenSecretRef:
    name: cloudflare-credentials
    namespace: kflare-system
    key: CF_API_TOKEN
EOF

echo ""
echo "==> Cluster ready. Run the controller with:"
echo "    make run"
echo ""
echo "==> Then watch the account status with:"
echo "    kubectl get cloudflareaccount my-account -o yaml"
