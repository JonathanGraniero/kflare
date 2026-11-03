## What changed

<!-- What this pull request changes, and why. Link the issue it fixes: "Fixes #123". -->

## How it was checked

<!-- The tests you added or ran. For behavior that depends on Cloudflare: what you ran against the live API, what it
accepted, refused and stored. A fork gets no CI secrets, so describe a run against your own account. -->

## Checklist

<!-- Tick what applies; delete what does not. CONTRIBUTING.md explains each. -->

- [ ] Unit or envtest specs for the new behavior and its failure cases
- [ ] `go fix -diff ./...` prints nothing, and `make lint` passes
- [ ] `make generate manifests helm` leaves no diff
- [ ] A controller change: a mutation check with `hack/mutate.sh`, every mutation caught
- [ ] A new controller or Cloudflare call: a live e2e spec, and the behavior checked live written down in
      `docs/design-notes.md`
- [ ] Docs: ARCHITECTURE.md (drift, adoption, errors, deletion), the deploy guide (permissions, troubleshooting),
      samples, and Markdown lines of 120 characters or fewer
- [ ] A change users see: an entry under CHANGELOG.md's Unreleased, with upgrade steps if it breaks a manifest, a
      chart value or a flag
