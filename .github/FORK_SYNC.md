# Fork release synchronization

`Sync upstream release` checks `Wei-Shaw/sub2api` every hour at minute 17 (UTC),
and can also be run manually. GitHub may delay scheduled runs. It runs once when
its synchronization files are updated on `SteinX/sub2api` main.

When the latest published stable upstream release has no corresponding fork
release, it merges upstream **main** into fork **main**, preserving fork commits.
This intentionally includes upstream commits made after the stable release tag.
Conflicts stop the workflow without overwriting fork changes. Draft and upstream
prereleases are excluded.

Frontend lint, type checking and critical tests, plus backend unit and integration
tests, must pass before an immutable annotated tag such as `v0.2.17-stx.1` is
created. The existing full `Release` workflow is then explicitly dispatched to
build binaries, checksums and GHCR images. It does not depend on tag-push events
from `GITHUB_TOKEN`, which cannot trigger another workflow. The SemVer fork suffix
makes the GitHub Release a prerelease under the existing GoReleaser configuration;
the existing image publisher still updates `latest` after a successful build.

One fork release is made per upstream stable version. Existing completed releases
are skipped, in-progress releases are allowed to finish, and existing tags are
never moved. If main changes during validation, publication stops and the next
run validates the new commit. If dispatch was interrupted after tag creation, the
next run validates that frozen tag and resumes publication. Failed release runs
require inspection and rerunning the failed jobs in GitHub Actions; scheduled
checks do not repeatedly retry a known failed publication.

No personal access token is required. The workflow uses its repository-scoped
`GITHUB_TOKEN` with `contents: write` and `actions: write`; the existing Release
workflow requests `packages: write` for GHCR. Actions must be enabled on the fork,
and repository rules must allow the bot to update main. No deployed server is
upgraded by this workflow.

Monitor both [synchronization runs](https://github.com/SteinX/sub2api/actions/workflows/sync-upstream.yml)
and [release runs](https://github.com/SteinX/sub2api/actions/workflows/release.yml).
If GitHub disables scheduled workflows after inactivity, re-enable the workflow.
