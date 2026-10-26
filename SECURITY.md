# Security

Please report vulnerabilities privately through GitHub: **Security → Report a vulnerability** on this repository.
Don't open a public issue.

kflare is an alpha, so only the latest release is supported: fixes go into the next release, not into older ones.

For how kflare handles credentials and what its controller can access, see the deploy guide's
[Security model](docs/deploy.md#security-model).

## How kflare is checked

- **On every pull request:** [govulncheck](https://go.dev/doc/security/vuln/) fails on a known vulnerability in the
  modules or the Go standard library that kflare's code calls, and [Grype](https://github.com/anchore/grype) fails on
  a fixable vulnerability of high or critical severity in the controller image CI builds. CodeQL analyzes the code,
  and dependency review refuses a new dependency with a known vulnerability.
- **Every week:** Grype scans the latest release's image, sending its report to code scanning, and govulncheck checks
  the release's binary, so that a vulnerability published after a release shows up without a new commit. A fix goes
  into the next release.
- **Dependencies:** Dependabot proposes updates to the Go modules, the GitHub Actions and the Dockerfile's base images
  every week, once a release is a week old, and security updates as soon as an advisory is published. Every action is
  pinned to a commit. The base images stay on their tags, so that a release is built with the newest Go patch and base
  files rather than a week later; the weekly scan checks what each release was built with.
- **Releases** are signed with cosign, from v0.2.1, and the image comes with SBOMs and provenance:
  the deploy guide's [Verifying a release](docs/deploy.md#verifying-a-release) checks them.
- **The repository's practices:**
  [OpenSSF Scorecard](https://scorecard.dev/viewer/?uri=github.com/JonathanGraniero/kflare) checks them every week
  and on every push to `main`.
