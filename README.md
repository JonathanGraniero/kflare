# kflare

A production-grade Kubernetes operator for Cloudflare. Manage Cloudflare resources declaratively via Kubernetes CRDs with full GitOps support.

## Status

Early development — Phase 1 complete. See [CLAUDE.md](./CLAUDE.md) for the full implementation plan.

## Prerequisites

- Go 1.22+
- kubectl
- kubebuilder
- kind (for local development)
- A Cloudflare account and API token

## Local Development

```sh
# Spin up a local kind cluster with CRDs installed
./local/setup.sh

# Run the controller locally
make run

# Tear down the cluster
./local/teardown.sh
```

## Building

```sh
# Generate CRDs and deepcopy functions
make generate manifests

# Build the controller binary
make build

# Run unit tests
make test
```

## License

MIT License — Copyright (c) 2026 Jonathan Graniero. See [LICENSE](./LICENSE) for details.
