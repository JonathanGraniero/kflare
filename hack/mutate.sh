#!/usr/bin/env bash
# Mutation check: applies each hand-written mutation to the source, runs the tests that should catch it, restores
# the file and reports whether the tests failed. Every mutation must be caught before a controller PR is opened.
#
# Usage: hack/mutate.sh MUTATIONS.json
#
# MUTATIONS.json is a list of mutations, each:
#   {"name": "...", "file": "internal/controller/foo_controller.go", "old": "...", "new": "..."}
# with optional:
#   "pkg"   the package to test (default ./internal/controller/)
#   "focus" a Ginkgo focus for ./internal/controller/ (default $MUTATE_FOCUS, else every spec)
#   "run"   a go test -run pattern instead, for packages without Ginkgo
# "old" must occur exactly once in "file". Paths are relative to the repo root.
#
# Before mutating, each distinct test command runs once unmutated and must pass, so a failing suite cannot pass
# for catching mutations. Verdicts: caught, SURVIVED (tests passed), BAD (does not compile, or "old" is not found
# exactly once). Exits non-zero unless every mutation is caught.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ $# -ne 1 ]]; then
  echo "usage: $0 MUTATIONS.json" >&2
  exit 2
fi
MUTATIONS="$(cd "$(dirname "$1")" && pwd)/$(basename "$1")"

if [[ -z "${KUBEBUILDER_ASSETS:-}" ]]; then
  make -C "$ROOT" -s envtest
  ENVTEST_K8S_VERSION="${ENVTEST_K8S_VERSION:-$(sed -n 's/^ENVTEST_K8S_VERSION ?= //p' "$ROOT/Makefile")}"
  ENVTEST="$ROOT/bin/setup-envtest-$(sed -n 's/^ENVTEST_VERSION ?= //p' "$ROOT/Makefile")"
  KUBEBUILDER_ASSETS="$("$ENVTEST" use "$ENVTEST_K8S_VERSION" --bin-dir "$ROOT/bin" -p path)"
  # Absolute, so the tests find the assets whatever directory they run from.
  case "$KUBEBUILDER_ASSETS" in /*) ;; *) KUBEBUILDER_ASSETS="$ROOT/$KUBEBUILDER_ASSETS" ;; esac
  export KUBEBUILDER_ASSETS
fi

cd "$ROOT"
exec python3 -I - "$MUTATIONS" <<'EOF'
import json, os, subprocess, sys

DEFAULT_PKG = "./internal/controller/"
DEFAULT_FOCUS = os.environ.get("MUTATE_FOCUS", "")


def command(m):
    pkg = m.get("pkg", DEFAULT_PKG)
    args = ["go", "test", pkg, "-count=1"]
    if "run" in m:
        args.append("-run=" + m["run"])
    elif pkg == DEFAULT_PKG and m.get("focus", DEFAULT_FOCUS):
        args.append("-ginkgo.focus=" + m.get("focus", DEFAULT_FOCUS))
    return args


def run(args):
    p = subprocess.run(args, capture_output=True, text=True, timeout=1800)
    return p.returncode, p.stdout + p.stderr


mutations = json.load(open(sys.argv[1]))

for args in sorted({tuple(command(m)) for m in mutations}):
    print("baseline:", " ".join(args), flush=True)
    code, out = run(list(args))
    if code != 0:
        print(out[-4000:])
        sys.exit("baseline failed: fix the tests before checking mutations")

results = []
for m in mutations:
    src = open(m["file"]).read() if os.path.isfile(m["file"]) else None
    if src is None:
        verdict = "BAD: file not found"
    elif src.count(m["old"]) != 1:
        verdict = "BAD: old found %d times" % src.count(m["old"])
    else:
        open(m["file"], "w").write(src.replace(m["old"], m["new"]))
        try:
            code, out = run(command(m))
        finally:
            open(m["file"], "w").write(src)
        if code == 0:
            verdict = "SURVIVED"
        elif "build failed" in out or "[setup failed]" in out:
            verdict = "BAD: does not compile"
        else:
            verdict = "caught"
    results.append((m["name"], verdict))
    print(m["name"], verdict, flush=True)

print("---")
for name, verdict in results:
    print(f"{verdict:24} {name}")
caught = sum(v == "caught" for _, v in results)
print(f"{caught}/{len(results)} caught")
sys.exit(0 if caught == len(results) else 1)
EOF
