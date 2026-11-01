# Governance

This document says who decides what in kflare, how decisions are made, and how someone becomes a maintainer. kflare
is young and has one maintainer today, so these rules are written to work for a single maintainer and to open up as
more people join. They change through a pull request, as described in [Changing this document](#changing-this-document).

## Values

- **Users first.** kflare manages real Cloudflare accounts. A change that could delete or change something a user
  owns without them asking for it is a bug, however it is reached.
- **Verified, not assumed.** Behavior that depends on Cloudflare's API is checked against the live API and written
  down ([docs/design-notes.md](docs/design-notes.md)), and every controller is held to its tests by mutation testing.
- **Open by default.** Plans, decisions and their reasons are made in public issues, pull requests and Discussions.
  Only security reports and Code of Conduct reports are private.
- **Everyone is welcome.** Participation is governed by the [Code of Conduct](CODE_OF_CONDUCT.md).

## Roles

### Users

Anyone who runs kflare. Users help by reporting bugs, asking and answering questions in
[Discussions](https://github.com/JonathanGraniero/kflare/discussions), and saying which Cloudflare products they need
next.

### Contributors

Anyone who opens an issue, reviews a pull request or contributes code, docs or tests. [CONTRIBUTING.md](CONTRIBUTING.md)
says how. Contributors need no permission and sign no agreement: contributions are accepted under the project's
[MIT license](LICENSE).

### Reviewers

Contributors trusted to review pull requests in an area of the code (for example the generator, the Helm chart or a
group of kinds). A reviewer's approval counts toward the reviews `main` requires, but only a maintainer merges. Any
maintainer can nominate a contributor who has made several substantial contributions to an area and reviewed others'
pull requests well; the maintainers decide as described under [Decision making](#decision-making). Reviewers are
listed in [MAINTAINERS.md](MAINTAINERS.md) and added to [.github/CODEOWNERS](.github/CODEOWNERS) for their area.

### Maintainers

Maintainers are responsible for the project as a whole. They:

- review and merge pull requests, and keep `main` releasable
- triage issues and answer Discussions
- cut releases, and own the project's accounts: the GitHub repository, the `ghcr.io` packages, the Artifact Hub
  listing and the `kflare.dev` domain
- handle security reports ([SECURITY.md](SECURITY.md)) and Code of Conduct reports
- decide the project's direction and keep [ROADMAP.md](ROADMAP.md) current

Maintainers are listed in [MAINTAINERS.md](MAINTAINERS.md) and are code owners of the whole repository.

## Decision making

Most decisions are made in the pull request or issue they concern, by **lazy consensus**: a proposal goes ahead unless
a maintainer objects with a reason within a reasonable time (at least three working days for anything beyond a fix or
a small change). Disagreement is settled by discussion, aiming for consensus.

When consensus cannot be reached, the maintainers vote, a simple majority of all maintainers deciding. While there are
fewer than three maintainers, the project lead (see [MAINTAINERS.md](MAINTAINERS.md)) decides after hearing the
discussion, and writes down the reason in the issue or pull request.

These decisions need an issue open for at least a week, and the approval of a majority of maintainers (the project
lead's, while there are fewer than three):

- a breaking change to a released CRD, the Helm chart's values or the manager's flags, which is also listed in
  [CHANGELOG.md](CHANGELOG.md) with how to upgrade
- adding or removing a maintainer
- a change to this document, the license or the Code of Conduct
- moving the project to a foundation or another organization

## Becoming a maintainer

A contributor can become a maintainer after a sustained record of contributions, typically several months of:

- substantial pull requests merged, held to the standards in [CONTRIBUTING.md](CONTRIBUTING.md) (tests, live checks,
  mutation testing, docs)
- reviews of other contributors' pull requests
- help with issues and Discussions

A maintainer nominates the contributor in an issue, which the contributor accepts. The decision follows the rules
above. A new maintainer is added to [MAINTAINERS.md](MAINTAINERS.md), [.github/CODEOWNERS](.github/CODEOWNERS) and the
repository's maintainer role, and is given access to the project's accounts as their responsibilities need.

## Stepping down

Maintainers and reviewers can step down at any time by opening a pull request that moves them to the emeritus list in
[MAINTAINERS.md](MAINTAINERS.md). A maintainer or reviewer who has not taken part in the project for six months may be
moved there by the other maintainers, after asking them. Emeritus members can return through the same process as a new
one, which their earlier work counts toward.

A maintainer can be removed for a serious or repeated violation of the Code of Conduct, by a majority of the other
maintainers.

## Continuity

So that the project does not depend on one person's accounts:

- releases are signed keylessly, by the `Release` workflow's own identity, so there is no signing key to hand over
- the image and the chart are published from GitHub Actions, not from a person's machine
- each maintainer is given administrator access to the repository, the `ghcr.io` packages and the Artifact Hub
  listing once there is more than one

If every maintainer becomes unavailable, contributors are free to continue the project in a fork under the
[MIT license](LICENSE). A fork should make clear that it is not the original project.

## AI-assisted contributions

kflare is developed with AI assistance, as the README says, and contributions made with AI tools are welcome. The
contributor who opens a pull request is responsible for it as if they had written every line: they understand it, it
meets [CONTRIBUTING.md](CONTRIBUTING.md)'s standards, and they can answer reviews of it. Claims about Cloudflare's API
are checked against the live API, not taken from a model.

## Changing this document

This document changes through a pull request, decided as described under [Decision making](#decision-making).
