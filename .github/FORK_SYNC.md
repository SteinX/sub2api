# Fork release synchronization

`Sync upstream release` checks `Wei-Shaw/sub2api` every hour at minute 17 (UTC),
and can also be run manually. GitHub may delay scheduled runs. It also runs after
updates to `SteinX/sub2api` main, including integration commits made by the agent.

When the latest published stable upstream release has no corresponding fork
release, it opens or reuses a `sync/upstream-vX.Y.Z` PR targeting fork **main**.
It merges fork main into that candidate branch to retain fork commits and runs
validation before it merges the PR. main is not changed by failed validation.
This intentionally includes upstream commits made after the stable release tag.
Conflicts leave an actionable PR and stop the mechanical merge without
overwriting fork changes. The maintenance agent then reads both sides, resolves
ordinary integration conflicts, preserves
fork behavior, validates the result and updates main through a reviewable commit
or pull request. A main update triggers this workflow again. Only unresolved
product decisions, missing access or persistent failures need user intervention.
Draft and upstream prereleases are excluded.

Frontend lint, type checking and critical tests, plus backend unit and integration
tests, must pass before the PR is merged. The PR head and main base must still
match the validated commits; the merged commit must have the same source tree as
the tested candidate. An immutable annotated tag such as `v0.2.17-stx.1` is then
created. When main already contains upstream, validation and publication can run
without an empty PR. The existing full `Release` workflow is explicitly dispatched to
build binaries, checksums and GHCR images. It does not depend on tag-push events
from `GITHUB_TOKEN`, which cannot trigger another workflow. The `-stx.N` suffix
identifies the fork build. The publisher marks these as stable GitHub Releases so
the fork's `/releases/latest` API, installer and updater can discover them. Other
prerelease suffixes retain the usual GoReleaser prerelease behavior. The image
publisher updates `latest` after a successful build.
The fork suffix is written into the built artifact, while main's tracked VERSION
stays aligned with upstream to avoid a version-file conflict on the next sync.

One fork release is made per upstream stable version. Existing completed releases
are skipped, in-progress releases are allowed to finish, and existing tags are
never moved. If main changes during validation, publication stops and the next
run validates the new commit. If dispatch was interrupted after tag creation, the
next run validates that frozen tag and resumes publication. Failed release runs
require inspection and rerunning the failed jobs in GitHub Actions; scheduled
checks do not repeatedly retry a known failed publication.

The maintenance agent owns upstream integration and publication follow-up; Actions
executes the checks, builds and publication. A conflict or code failure is a work
item for the agent to investigate and repair, not merely a notification to the
user. It must inspect existing integration work and active runs before starting
another attempt. It must never force-push main, move release tags or bypass tests.

The existing scheduled ChatGPT agent is an exception monitor. It first checks
managed sync PR mergeability and synchronization/release workflow results. Healthy
runs and pending checks do not cause code analysis, repeated tests or notifications.
Conflicts, failed checks and failed publication start active investigation and
repair. The exposed ChatGPT Automations API only accepts schedules, so this is
hourly exception polling rather than an instant GitHub webhook. No extra API key
or GitHub-hosted Codex execution is configured.

No personal access token is required. The workflow uses its repository-scoped
`GITHUB_TOKEN` with `contents: write` and `actions: write`; the existing Release
workflow requests `packages: write` for GHCR. Actions must be enabled on the fork,
and repository rules must allow the bot to update main. No deployed server is
upgraded by this workflow.

Fork release binaries have their update repository injected at build time as
`SteinX/sub2api`. Release installation instructions pin the installer to the same
tag and explicitly pass `GITHUB_REPO=SteinX/sub2api`; a fork installer can also be
run manually with that environment variable. GHCR uses the lowercased full GitHub
repository name, and registry authentication uses `github.actor` with the token.
DockerHub publication and Telegram announcements are off by default on forks;
enable them deliberately with `PUBLISH_DOCKERHUB=true` / `PUBLISH_TELEGRAM=true`
repository variables and the corresponding credentials, if wanted.

Monitor both [synchronization runs](https://github.com/SteinX/sub2api/actions/workflows/sync-upstream.yml)
and [release runs](https://github.com/SteinX/sub2api/actions/workflows/release.yml).
If GitHub disables scheduled workflows after inactivity, re-enable the workflow.
