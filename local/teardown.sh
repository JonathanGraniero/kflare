#!/usr/bin/env bash
set -euo pipefail

echo "==> Deleting kind cluster kflare-dev..."
kind delete cluster --name kflare-dev
echo "==> Done."
