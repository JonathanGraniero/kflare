#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# Verify required env vars
if [[ -z "${CF_API_TOKEN:-}" || -z "${CF_ACCOUNT_ID:-}" ]]; then
  echo "ERROR: CF_API_TOKEN and CF_ACCOUNT_ID must be set."
  exit 1
fi

# The pinned kind (v0.30+ is needed for the Kubernetes 1.34 node image).
make -C "$ROOT_DIR" -s kind >/dev/null
KIND="$(make -C "$ROOT_DIR" -s kind-path)"

# With several kind clusters on one Linux host, kube-proxy fails with "too many
# open files" and pods cannot reach the API server unless this limit is raised.
if [[ "$(sysctl -n fs.inotify.max_user_instances 2>/dev/null || echo 512)" -lt 512 ]]; then
  echo "WARNING: fs.inotify.max_user_instances is below 512; pods may fail to reach the API server."
  echo "         Raise it with: sudo sysctl fs.inotify.max_user_instances=512"
fi

echo "==> Creating kind cluster..."
"$KIND" create cluster --config "$SCRIPT_DIR/kind-config.yaml" --wait 60s

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
apiVersion: kflare.dev/v1alpha1
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
