# Versioning and support

kflare is an alpha: every kind is served as `kflare.dev/v1alpha1`. This page says what each release may change, which
Kubernetes versions a release supports, how long a release gets fixes, how kflare's API versions are deprecated, and
what a kind needs to become `v1beta1`. What each release changed is in its notes, in [CHANGELOG.md](../CHANGELOG.md).

This page changes through pull requests. A change that shortens a support period applies only to the releases made
after it.

## Release versions

The chart and the controller image are released together, under one version and its `vX.Y.Z` tag: the release
workflow refuses a tag that differs from the chart's version.

Until 1.0.0:

- **A patch release** (0.4.1 to 0.4.2) needs no change to upgrade: manifests, Helm values and the manager's flags
  written for the release before keep working, and keep their meaning. It fixes bugs and vulnerabilities, and can add
  to kflare: a kind, an optional field, a chart value, a flag, a metric or a condition reason
- **A minor release** (0.4.2 to 0.5.0) can also make breaking changes. Its notes list each one under "Breaking
  changes", with how to upgrade

A breaking change is one that a manifest, a values file or a flag written for the release before may not fit, one that
changes what they mean, or one that takes away something you may depend on:

- a kind, a field, or an annotation or label that kflare reads or writes, removed or renamed, and any change to a CRD
  that `make crd-compat` reports, such as a tighter validation or a new default
  ([Changing a CRD](../CONTRIBUTING.md#changing-a-crd) lists them)
- a condition reason, a metric or one of its labels, removed or renamed
- a chart value or a flag removed or renamed, and a default that changes what an installation does, such as drift
  checks turned on
- a Kubernetes minor version no longer supported (see [Kubernetes versions](#kubernetes-versions))

A fix can change behavior in any release: kflare stops doing what it did wrong. CI tests each release's upgrade from
the release before it, so to skip a minor release, read its notes too.

kflare stays below 1.0.0 while every kind is an alpha. From 1.0.0, a patch release only fixes, and a minor release adds
and deprecates. A minor release then makes only the breaking changes this page allows in one: to an alpha kind, the
removal of a beta version whose deprecation period is over, and the end of support for a Kubernetes minor. Any other
breaking change waits for a major release.

## Supported releases

- **The latest minor release gets every fix**, in its next patch release
- **The minor release before it gets security fixes for three months** after the latest came out, in patch releases
  of its own. A security fix is one for a vulnerability in kflare or in what it is built from (its Go modules, the Go
  standard library and the image's base files), reported as [SECURITY.md](../SECURITY.md) says or found by its checks
- **Older releases get no fixes**

Only two minor releases get fixes at a time: if another minor release comes out within the three months, the oldest
of the three gets no more. Within a minor release, use its newest patch: a patch needs no change to upgrade, so a fix
comes as the next patch, never as a change to an earlier one. A published release never changes.

This starts with v0.4: when v0.5.0 comes out, v0.4 gets security fixes for three months, as v0.4 patch releases, and
v0.5 gets every fix. v0.3 and older get none.

## Kubernetes versions

Each release supports the four newest Kubernetes minor versions when it comes out: the three that the Kubernetes
project supports, and the one before them. Managed services such as Amazon EKS and Azure AKS keep a minor in standard
support for some weeks after the Kubernetes project ends its support, and the fourth gives a cluster on one of them
time to upgrade. Releases today support **Kubernetes 1.34 to 1.37**.

- **The oldest and the newest are tested**: CI runs the envtest and end-to-end suites on both, and the upgrade from
  the last release on the newest. The chart refuses to install on an older Kubernetes (its `kubeVersion`)
- **A new Kubernetes minor is supported** from the first kflare release whose CI runs on it, a patch or a minor
- **The oldest is dropped in a minor release only**, the first one after a new Kubernetes minor comes out: once
  Kubernetes 1.38 is out, the next minor release of kflare needs 1.35. Its notes list the change under "Breaking
  changes"

Extended or long-term support, which keeps a Kubernetes minor for a year or more after the Kubernetes project ends its
support, is not covered. A cluster on it can stay on the last kflare release that supports its version, which gets
fixes only for as long as [Supported releases](#supported-releases) says.

## API versions

Every kind is served as `v1alpha1` today. Each kind has a version of its own: a kind graduates when it meets
[the criteria](#graduating-to-v1beta1), whatever the versions of the others, and a new kind starts as `v1alpha1`.

### Alpha

An alpha kind can change in a minor release: a field renamed or removed, a validation tightened, a default changed. It
changes in place, without a deprecation period, so a manifest in the old shape no longer applies once you upgrade. The
notes list each change under "Breaking changes", naming the kind, with the steps to upgrade, and a patch release makes
none. CI holds the notes to this: `make crd-compat` fails on a breaking change to a CRD that the notes do not name,
and the upgrade test installs the last release with a resource of each kind, upgrades it to the new chart, and checks
that every resource is kept and still applies.

### Beta and GA

A beta (`v1beta1`) or GA (`v1`) version never changes in a way that a manifest written for it may not fit:
`make crd-compat` fails on any such change. A change that would break goes into a new version of the kind, served next
to the old one, with a conversion between them that loses nothing. Versions are deprecated as Kubernetes deprecates
its own APIs ([its deprecation policy](https://kubernetes.io/docs/reference/using-api/deprecation-policy/)):

- **A beta version** is deprecated in a minor release, once its successor is served, and stays served for at least
  9 months or 3 minor releases after that, whichever is longer. It is then removed in a minor release
- **A GA version** can be deprecated, but stays served until the next major release
- **The storage version** changes only in a release after one that served both versions, so that rolling back one
  release still reads every stored resource
- **A deprecated version** is marked so in its CRD, with a warning that names its successor: the API server returns
  the warning to every request that uses the version, and `kubectl` prints it

## Graduating to v1beta1

A kind graduates from `v1alpha1` to `v1beta1` once it meets all of these:

1. **A stable shape**: no breaking change to its CRD for at least three months and the last three minor releases, and
   none planned
2. **Fields from Cloudflare's API**: kflare's code generator writes its fields from Cloudflare's API description, so
   that they take the API's names and constraints. That holds for every kind today but `Zone`, `TunnelConfiguration`
   and `WorkerScript`, whose fields are still written by hand. `CloudflareAccount` holds kflare's own settings rather
   than a Cloudflare object, so it has no API to follow
3. **Tested upgrades**: CI's upgrade from the last release covers the kind, as it covers every kind since
   [#86](https://github.com/JonathanGraniero/kflare/issues/86), and its live end-to-end spec passes
4. **Production use**: three organizations run kflare in production, each listed in [ADOPTERS.md](../ADOPTERS.md) at
   the `production` stage

### How a kind graduates

A kind's first `v1beta1` has its `v1alpha1` schema: a field that should change is changed while the kind is an alpha,
where that is allowed, so that the stable shape is the one that graduates. The API server can then serve both versions
with the `None` conversion strategy, which changes only `apiVersion`. Graduating needs no conversion webhook, so
reading and writing kflare's resources never waits on kflare's own pods.

1. **A minor release serves both versions**, `v1alpha1` stored and deprecated. The samples and docs move to
   `v1beta1`, and your manifests can follow at their own pace
2. **A later minor release stores `v1beta1`**: it rewrites each stored resource of the kind, which stores it in the new
   version, then removes `v1alpha1` from the CRD's `status.storedVersions`. Kubernetes' own storage version migration
   has been beta since 1.35, and is off by default, so kflare does this itself
3. **`v1alpha1` stays served** for at least three months and three minor releases after `v1beta1` is, and is then
   removed in a minor release

A later version whose schema differs, a `v1beta2` or a `v1`, needs a conversion webhook: one the manager serves, with a
certificate from cert-manager or one the manager rotates itself, and a CI test that converts a resource of each kind
both ways and finds nothing lost. That version ships only once both are in place.

### When the work starts

The work to serve two versions starts once the first kind has a stable shape, the first criterion above, so that it is
ready when a kind meets the rest: kflare's code generator writing a kind's `v1beta1` types and both versions into its
CRD, the rewrite of stored resources, and an upgrade test that runs the rewrite and then rolls back a release. The
conversion webhook's work starts when a beta kind first needs a change that it cannot take in place.

## How changes are announced

Each release has a section in [CHANGELOG.md](../CHANGELOG.md), which is also its GitHub release's notes and its
changelog on Artifact Hub. Its first paragraph says whether manifests written for the release before apply unchanged,
and its headings say what to read before upgrading:

- **Breaking changes**: each breaking change, with how to upgrade. One to a CRD names each kind it changes, which CI
  checks
- **Upgrading from** the release before: what to do, in order, and what to check
- **Deprecated**: what is deprecated, what replaces it, and the earliest release that may remove it
- **Known limitations**: what does not work yet

A breaking change to a CRD, a chart value or a flag is decided in an issue that stays open for at least a week first
([GOVERNANCE.md](../GOVERNANCE.md#decision-making)), so that you can see it coming and say how it would affect you. A
fixed vulnerability is announced in a GitHub security advisory, which names the fixed release of each supported minor.
