#!/usr/bin/env bash
# Live e2e run with the manager on this machine: builds the manager, runs it against the current kube context, runs
# the e2e suite against it (KFLARE_E2E_MANAGER=external), and leaves the cluster as it found it. It skips the image
# build and kind load of `make test-e2e`, and works where a new kind cluster cannot start.
#
# Usage: hack/e2e-live.sh [go test flags...]
#
# The current context must be a kind cluster (KFLARE_E2E_ANY_CONTEXT=1 allows another). CF_API_TOKEN, CF_ACCOUNT_ID
# and CF_E2E_ZONE enable the Cloudflare specs; without them every live spec skips.
#
# In order:
#   1. Takes a lock, so two runs from this machine never overlap: a Free zone holds one rate limiting rule and a Free
#      account one IP list, so overlapping runs fail
#   2. Installs the CRDs (make install), builds bin/manager, starts it and waits until it is ready. The manager logs to
#      bin/e2e-live-manager.log; its health probe listens on $KFLARE_E2E_PROBE_ADDR (default 127.0.0.1:18081)
#   3. Runs the suite
#   4. Deletes the suite's namespace while the manager still runs, so finalizers can finish
#   5. Stops the manager, uninstalls the CRDs unless they were installed before the run, and prints the manager log's
#      errors
# Steps 4 and 5 also run after a failure or Ctrl-C. Exits with the suite's status.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PROBE_ADDR="${KFLARE_E2E_PROBE_ADDR:-127.0.0.1:18081}"
LOG="$ROOT/bin/e2e-live-manager.log"
LOCK="${XDG_RUNTIME_DIR:-/tmp}/kflare-e2e-live.lock"
# The namespace the suite's resources live in (e2eNamespace in test/e2e/cloudflare_test.go).
E2E_NAMESPACE=kflare-e2e

say() { echo "e2e-live: $*" >&2; }

context="$(kubectl config current-context)"
if [[ "$context" != kind-* && "${KFLARE_E2E_ANY_CONTEXT:-}" != 1 ]]; then
  say "the current context is $context, not a kind cluster (set KFLARE_E2E_ANY_CONTEXT=1 to run anyway)"
  exit 2
fi
for v in CF_API_TOKEN CF_ACCOUNT_ID CF_E2E_ZONE; do
  [[ -n "${!v:-}" ]] || say "$v is not set, so the specs that need it skip"
done

exec 9>"$LOCK"
if ! flock -n 9; then
  say "another live run holds $LOCK"
  exit 2
fi

kflare_crds() { kubectl get crd -o name | grep '\.kflare\.dev$' || true; }
crds_before="$(kflare_crds)"
manager_pid=""

stop_manager() {
  kill -TERM "$manager_pid" 2>/dev/null || return 0
  for _ in $(seq 60); do
    kill -0 "$manager_pid" 2>/dev/null || break
    sleep 0.5
  done
  kill -KILL "$manager_pid" 2>/dev/null || true
  wait "$manager_pid" 2>/dev/null || true
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if [[ -n "$manager_pid" ]] && kill -0 "$manager_pid" 2>/dev/null; then
    say "deleting namespace $E2E_NAMESPACE"
    kubectl delete namespace "$E2E_NAMESPACE" --ignore-not-found --timeout=5m >/dev/null ||
      say "namespace $E2E_NAMESPACE is still there after 5 minutes"
    say "stopping the manager"
    stop_manager
  fi
  if [[ -z "$crds_before" ]]; then
    say "uninstalling the CRDs this run installed"
    kflare_crds | xargs -r kubectl delete --timeout=2m >/dev/null ||
      say "the CRDs were not all deleted: check for resources left with finalizers"
  fi
  if [[ -f "$LOG" ]]; then
    local errors
    errors="$(grep -c 'ERROR' "$LOG" || true)"
    say "the manager logged $errors ERROR lines ($LOG)"
    grep 'ERROR' "$LOG" | cut -c1-200 | head -20 >&2 || true
  fi
  say "exit status $status"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

say "installing the CRDs into $context"
make -s install >/dev/null
say "building the manager"
make -s build
mkdir -p "$(dirname "$LOG")"
bin/manager --metrics-bind-address=0 --health-probe-bind-address="$PROBE_ADDR" >"$LOG" 2>&1 &
manager_pid=$!
say "started the manager ($manager_pid), waiting for it to be ready"
for _ in $(seq 120); do
  curl -sf "http://$PROBE_ADDR/readyz" >/dev/null && break
  if ! kill -0 "$manager_pid" 2>/dev/null; then
    say "the manager exited; the end of its log:"
    tail -20 "$LOG" >&2
    exit 1
  fi
  sleep 0.5
done
curl -sf "http://$PROBE_ADDR/readyz" >/dev/null || { say "the manager was not ready after 60 seconds"; exit 1; }

say "running the suite"
KFLARE_E2E_MANAGER=external go test ./test/e2e/ -count=1 -v -ginkgo.v -timeout 30m "$@"
