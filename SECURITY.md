# Security

Please report vulnerabilities privately through GitHub: **Security → Report a vulnerability** on this repository.
Don't open a public issue.

For how kflare handles credentials and what its controller can access, see the deploy guide's
[Security model](docs/deploy.md#security-model).

## Supported versions

- **The latest minor release** gets every fix, in its next patch release
- **The minor release before it** gets security fixes for three months after the latest came out, in patch releases
  of its own. This starts with v0.4, once v0.5.0 is out
- **Older releases** get no fixes

Within a minor release, upgrade to its newest patch: a patch release needs no change to upgrade. A fixed vulnerability
is announced in a GitHub security advisory, which names the fixed release of each supported minor.
[Versioning and support](docs/versioning.md) says what each release may change and which Kubernetes versions it
supports. The weekly scan below covers only the latest release for now: scanning the minor release before it too is
[#157](https://github.com/JonathanGraniero/kflare/issues/157).

## How kflare is checked

- **On every pull request:** [govulncheck](https://go.dev/doc/security/vuln/) fails on a known vulnerability in the
  modules or the Go standard library that kflare's code calls, and [Grype](https://github.com/anchore/grype) fails on
  a fixable vulnerability of high or critical severity in the controller image CI builds. CodeQL analyzes the code,
  and dependency review refuses a new dependency with a known vulnerability.
- **Fuzz tests** cover the code that reads input kflare does not control: the metrics endpoint's authentication,
  which serves any client that can reach its port, and the reading of the errors Cloudflare's API returns. `make test`
  runs their seed inputs and every input that once failed.
- **Every week:** Grype scans the latest release's image, sending its report to code scanning, and govulncheck checks
  the release's binary, so that a vulnerability published after a release shows up without a new commit. A fix goes
  into the next patch release.
- **Dependencies:** Dependabot proposes updates to the Go modules, the GitHub Actions and the Dockerfile's base images
  every week, once a release is a week old, and security updates as soon as an advisory is published. Every action is
  pinned to a commit. The base images stay on their tags, so that a release is built with the newest Go patch and base
  files rather than a week later; the weekly scan checks what each release was built with.
- **Releases** are signed with cosign, from v0.2.1, and the image comes with SBOMs and provenance:
  the deploy guide's [Verifying a release](docs/deploy.md#verifying-a-release) checks them.
- **The repository's practices:**
  [OpenSSF Scorecard](https://scorecard.dev/viewer/?uri=github.com/JonathanGraniero/kflare) checks them every week
  and on every push to `main`.
