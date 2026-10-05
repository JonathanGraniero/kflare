#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KIND="$(make -C "$ROOT_DIR" -s kind-path)"
[[ -x "$KIND" ]] || KIND=kind

echo "==> Deleting kind cluster kflare-dev..."
"$KIND" delete cluster --name kflare-dev
echo "==> Done."
