# Devin cloud upstream repair

Normal upstream detection, integration checks, merging and publication remain in
GitHub Actions. `Devin upstream repair` starts a cloud Devin only for a current
failed synchronization/publication or a completed full release missing required
artifacts. It is **off until explicitly enabled**. It does not deploy servers.

## 1. Connect Devin and prepare its environment

In Devin **Settings → Connections → GitHub**, install/authorize the connection for
`SteinX/sub2api`. Give sessions access to that repository. The GitHub connection
is separate from the API credential below: a valid API key alone does not give
the session GitHub clone, push, PR or workflow access.

Configure a repository environment with Git, GitHub CLI, Node 20+, pnpm 9, Python
and PyYAML, plus the Go version declared in `backend/go.mod`. Install frontend
dependencies using the frozen lockfile. Check that the environment can run the
repo's unit/integration tests and read Actions logs. Any PostgreSQL/Redis or other
services required by those tests must also be available in Devin's environment.
Never copy `/workspace/.sub2api-env/activate.sh` from the Codex environment.

Instructions are versioned in `.github/DEVIN_UPSTREAM_REPAIR.md` and sent with
each dispatch. An existing Devin Playbook can optionally supplement them.

## 2. Generate the service-user key

In the target Devin organization:

1. Open **Settings → Devin API → Service users**.
2. Choose **Provision service user**, named for example `sub2api-upstream`.
3. Choose **Member**. Admin is unnecessary for this integration.
4. Copy the `cog_…` API key displayed once after provisioning.
5. Copy the **Organization ID** from the top of the Devin API settings page.

For enterprise custom roles, the dispatcher needs `UseDevinSessions`
(`org.devins.use`) to create sessions and `ManageOrgSessions` to send follow-up
messages. It reads sessions it created. The optional credential preflight lists
sessions and therefore also needs session-list access (`ViewOrgSessions` if the
role restricts that). No user impersonation is used.

Use the v3 service-user API, rather than legacy `apk_` keys or CLI/browser
credentials. This does not use sub2api's CLI/Cascade credential exchange.

Official references:
[Teams quick start](https://docs.devin.ai/api-reference/getting-started/teams-quickstart),
[Create Session](https://docs.devin.ai/api-reference/v3/sessions/post-organizations-sessions),
[Send a message](https://docs.devin.ai/api-reference/v3/sessions/post-organizations-sessions-messages),
[GitHub connection](https://docs.devin.ai/integrations/gh).

## 3. Store configuration directly in GitHub

Open **SteinX/sub2api → Settings → Secrets and variables → Actions**.
The repository must have Actions and Issues enabled.

| Kind | Name | Value |
| --- | --- | --- |
| Repository secret | `DEVIN_API_KEY` | Service-user `cog_…` key |
| Repository variable | `DEVIN_ORG_ID` | Organization ID, e.g. `org-…` |
| Repository variable | `DEVIN_MAX_ACU` | Optional integer 1–100; default **10** per session |
| Repository variable | `DEVIN_PLAYBOOK_ID` | Optional existing playbook ID |
| Repository variable | `DEVIN_UPSTREAM_REPAIR_ENABLED` | Set to `true` only after preflight/setup |

The key belongs in the **Secrets** tab. Store the organization ID and switches in
the **Variables** tab. `DEVIN_ORG_ID` is also accepted as a repository secret if
you already saved it there; the variable takes precedence. The assistant does not need the token in chat or source.
Tell the assistant the secret/variable names after saving them. If setting secrets
through a local authenticated `gh` CLI, use its interactive input:

```sh
gh secret set DEVIN_API_KEY --repo SteinX/sub2api
gh variable set DEVIN_ORG_ID --repo SteinX/sub2api
```

Avoid pasting the secret into a command argument or a tracked file. Rotating the
same named Actions secret requires no code change.

## 4. Preflight, enable, and validate a real repair

Merging dispatcher changes to `main` automatically runs the read-only credential
check, even while automatic repairs are disabled. To run it again, open
**Actions → Devin upstream repair →
Run workflow**, choose `main`, and leave **mode = check**. This performs one
read-only API request. It does not start a paid session, change GitHub state, or
verify whether the service user can create a session or push repository changes.

Once preflight and the Devin repository environment are ready, set
`DEVIN_UPSTREAM_REPAIR_ENABLED=true`. Automatic workflow completion events then
enable dispatch. To handle a failure that predates activation, run this workflow
with **mode = repair** and the numeric ID from its original synchronization or
fork Release run URL. Stale/unrelated/successful sync runs are ignored.

Monitor the created `Devin upstream repair: vX.Y.Z` tracking issue and its session
link. Run a real repair through push → existing Actions checks → merge → full
Release. Only then pause the old ChatGPT exception monitor, so two agents do not
repair the same branch. Neither this change nor preflight disables that task.

## Dispatch and completion behavior

- Only completed runs from the two named workflows in this same repository are
  eligible. Other repositories, other PR branches, older releases, superseded
  runs/attempts, moving main/candidate commits, cancellations and normal waiting
  do not start sessions. Dry-run and simple Releases have distinct run titles and
  are ignored. PR snapshots or verified merge parents establish the current head
  and base even when GitHub validates a synthetic merge commit.
- `workflow_run` executes only tooling checked out from trusted `main`, without
  persisted checkout credentials. It never downloads a source run's artifacts or
  executes its branch in the privileged dispatcher. The API token goes only to
  the dispatcher step, never to application/test jobs or the Devin prompt.
- A bot-authored GitHub issue stores a reservation **before** each API write.
  A fingerprint based on version, source heads and failing jobs deduplicates
  repeated hourly failures. New failing code is sent to the same active session,
  sharing its original ACU cap. At most five writes are allowed per version.
- No follow-up prompt or replacement session is sent automatically if the first
  is finished, waiting for user input/approval, terminal, archived, blocked by
  quota or stopped intentionally. Dispatch and monitoring use the same attention
  predicate. A timeout, canceled runner or lost
  state-save leaves a `reserved` state: inspect Devin before retrying. There is
  no documented client idempotency key used by this v3 integration, so ambiguous
  writes are never blindly retried.
- HTTP 400/401/403/404/422/429 rejections release the reservation. Correct the
  configuration and rerun the dispatcher for the same source run. Other HTTP
  failures require checking whether Devin accepted the request first.
- Every hour at minute 43 UTC, a lightweight monitor only reads open tracking
  issues, session status and release metadata. It creates no sessions and sends
  no agent prompts. A newly observed quota/input/approval/terminal blocker updates
  the issue once; unchanged blockers do not repeatedly update it.
- A successful full Release is independently checked for five nonempty uploaded
  archives, `checksums.txt`, and a versioned GHCR index containing Linux amd64
  and arm64. Only this verification closes the tracking issue. A service error
  while verifying GHCR is an unknown result, not proof of a missing image.

To recover a reserved/blocked record, inspect the source run, issue and Devin
sessions first. A repository maintainer may edit the issue's machine state block:
restore `phase` to `active` and set the verified `sessionId` if the write succeeded;
or remove only the unaccepted fingerprint and restore `phase` to `ready` if it
definitely did not. A replacement after a terminal session needs explicit review
of spent ACUs and removal of `sessionId`; it is never automatic. Keep the
`dispatches` history and five-write cap. Do not delete the issue to bypass limits.

Pause automatic dispatch and monitoring by setting
`DEVIN_UPSTREAM_REPAIR_ENABLED=false`. Normal Actions synchronization/publication
continues. Already running Devin sessions must be stopped separately in Devin.
