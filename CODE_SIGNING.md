# Code signing policy

Free code signing provided by [SignPath.io](https://about.signpath.io/), certificate by
[SignPath Foundation](https://signpath.org/).

## What is signed

- The Windows installers (`ArtisanHubLink-<version>-x64.msi` and `-arm64.msi`) and the
  `link-site.exe` program inside them.
- Only files built from this repository are signed. They are built by the
  [Release workflow](.github/workflows/release.yml) on GitHub-hosted runners from a tag on `main`,
  and SignPath checks that each file came from that workflow before signing it.
- Every release is approved by hand before it is signed.
- Releases also carry a `SHA256SUMS` file listing every file in the release.

macOS installers are signed with the maintainer's Apple Developer ID and notarized by Apple.
Linux packages are signed with the project's repository key.

## Team

| Role | Members |
| --- | --- |
| Committers and reviewers | [James Norman](https://github.com/norjms) |
| Approvers | [James Norman](https://github.com/norjms) |

Committers may change the source. Changes from anyone else are reviewed by a reviewer before they are
merged. Approvers approve each signing request. Everyone in these roles uses multi-factor
authentication for GitHub and SignPath.

## Privacy

See [PRIVACY.md](PRIVACY.md). OpenPrintHQ Link sends nothing anywhere except to the hub its owner
pairs it with, and, when automatic updates are on, to GitHub to check for and download releases.

## Reporting a problem with a signed file

Report it privately as described in [SECURITY.md](SECURITY.md).
