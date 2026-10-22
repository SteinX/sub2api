'use strict';

const UPSTREAM = { owner: 'Wei-Shaw', repo: 'sub2api' };
const FORK = { owner: 'SteinX', repo: 'sub2api' };
const WORKFLOW = 'release.yml';

function assertFork(context) {
  if (context.repo.owner !== FORK.owner || context.repo.repo !== FORK.repo) {
    throw new Error('Upstream synchronization is restricted to SteinX/sub2api');
  }
}

async function optional(request) {
  try { return (await request()).data; }
  catch (error) { if (error.status === 404) return null; throw error; }
}

function releaseTag(release) {
  if (release.draft || release.prerelease || !/^v\d+\.\d+\.\d+$/.test(release.tag_name)) {
    throw new Error('Expected a published, stable upstream vMAJOR.MINOR.PATCH release');
  }
  return `${release.tag_name}-stx.1`;
}

function checkedSha(sha) {
  if (!/^[0-9a-f]{40}$/.test(sha)) throw new Error('Invalid source commit SHA');
  return sha;
}

async function integrationPullRequest({ github, core }, release, upstreamSha, baseSha) {
  const branch = `sync/upstream-${release.tag_name}`;
  let ref = await optional(() => github.rest.git.getRef({ ...FORK, ref: `heads/${branch}` }));
  if (!ref) {
    await github.rest.git.createRef({ ...FORK, ref: `refs/heads/${branch}`, sha: upstreamSha });
  }
  let conflicted = false;
  try {
    // Integrate main into the candidate branch, never into main before testing.
    // An existing branch may contain agent repairs; do not reset or force it.
    await github.rest.repos.merge({ ...FORK, base: branch, head: baseSha,
      commit_message: `chore: retain fork main while syncing ${release.tag_name}` });
  } catch (error) {
    if (error.status !== 409) throw error;
    conflicted = true;
  }
  const pulls = (await github.rest.pulls.list({ ...FORK, base: 'main',
    head: `${FORK.owner}:${branch}`, state: 'open' })).data;
  const pr = pulls[0] || (await github.rest.pulls.create({ ...FORK,
    base: 'main', head: branch, title: `chore: sync upstream ${release.tag_name}`,
    body: `Integrate upstream main for ${release.html_url}.\n\nPreserve fork changes. The synchronization workflow must validate this branch before merging and publishing.\n\n<!-- sub2api-upstream-sync:${release.tag_name} -->`,
  })).data;
  core.setOutput('pr_number', String(pr.number));
  core.setOutput('base_sha', baseSha);
  core.info(`Upstream integration PR: ${pr.html_url}`);
  if (conflicted) {
    core.setOutput('agent_needed', 'true');
    core.setFailed(`Resolve upstream integration conflicts in ${pr.html_url}`);
    return null;
  }
  return checkedSha((await github.rest.repos.getBranch({ ...FORK, branch })).data.commit.sha);
}

async function plan({ github, context, core }) {
  assertFork(context);
  const release = await optional(() => github.rest.repos.getLatestRelease(UPSTREAM));
  if (!release) { core.info('No stable upstream release yet'); return; }
  const tag = releaseTag(release);
  const published = await optional(() => github.rest.repos.getReleaseByTag({ ...FORK, tag }));
  if (published && !published.draft) { core.info(`${tag} is already published`); return; }

  // GITHUB_TOKEN tag pushes do not trigger another workflow. Dispatch explicitly,
  // and use the release run name to avoid retrying a failed publication blindly.
  const runs = await github.paginate(github.rest.actions.listWorkflowRuns, {
    ...FORK, workflow_id: WORKFLOW, per_page: 100,
  });
  if (runs.some(run => run.status !== 'completed')) {
    core.info('A release is still running; check again on the next scheduled run');
    return;
  }
  const previous = runs.find(run => run.display_title === `Release ${tag}`);
  if (previous) {
    throw new Error(`${tag} has an unfinished or failed publication; inspect ${previous.html_url} and rerun its failed jobs`);
  }

  const existingTag = await optional(() => github.rest.git.getRef({ ...FORK, ref: `tags/${tag}` }));
  let sha;
  if (existingTag) {
    // Resume after tag creation / dispatch interruption without moving the tag.
    sha = (await github.rest.repos.getCommit({ ...FORK, ref: tag })).data.sha;
  } else {
    const upstreamSha = checkedSha((await github.rest.repos.getBranch({ ...UPSTREAM, branch: 'main' })).data.commit.sha);
    const baseSha = checkedSha((await github.rest.repos.getBranch({ ...FORK, branch: 'main' })).data.commit.sha);
    const comparison = (await github.rest.repos.compareCommitsWithBasehead({
      ...FORK, basehead: `${baseSha}...${upstreamSha}`,
    })).data;
    sha = baseSha;
    if (!['identical', 'behind'].includes(comparison.status)) {
      sha = await integrationPullRequest({ github, core }, release, upstreamSha, baseSha);
      if (!sha) return;
    }
  }
  core.setOutput('sha', checkedSha(sha));
  core.setOutput('tag', tag);
  core.setOutput('upstream_tag', release.tag_name);
  core.setOutput('publish', 'true');
  core.info(`Validate ${tag} at ${sha} before publication`);
}

async function publish({ github, context, core }, { sha, tag, upstreamTag, prNumber, baseSha }) {
  assertFork(context);
  checkedSha(sha);
  if (!/^v\d+\.\d+\.\d+$/.test(upstreamTag) || tag !== `${upstreamTag}-stx.1`) {
    throw new Error('Invalid fork release tag');
  }
  const release = (await github.rest.repos.getReleaseByTag({ ...UPSTREAM, tag: upstreamTag })).data;
  if (releaseTag(release) !== tag) throw new Error('Upstream release changed');
  if (prNumber) {
    if (!Number.isSafeInteger(Number(prNumber)) || Number(prNumber) < 1) throw new Error('Invalid sync PR number');
    checkedSha(baseSha);
    const pr = (await github.rest.pulls.get({ ...FORK, pull_number: Number(prNumber) })).data;
    if (pr.head.repo.full_name !== 'SteinX/sub2api' || pr.base.ref !== 'main'
        || pr.head.ref !== `sync/upstream-${upstreamTag}` || pr.head.sha !== sha || pr.state !== 'open') {
      throw new Error('Sync PR changed during validation');
    }
    const main = (await github.rest.repos.getBranch({ ...FORK, branch: 'main' })).data.commit.sha;
    if (main !== baseSha) throw new Error('main changed during PR validation; retry integration');
    const testedTree = (await github.rest.git.getCommit({ ...FORK, commit_sha: sha })).data.tree.sha;
    const merged = (await github.rest.pulls.merge({ ...FORK, pull_number: Number(prNumber),
      sha, merge_method: 'merge' })).data;
    if (!merged.merged) throw new Error('Sync PR was not merged');
    const mergedSha = checkedSha(merged.sha);
    const mergedTree = (await github.rest.git.getCommit({ ...FORK, commit_sha: mergedSha })).data.tree.sha;
    if (mergedTree !== testedTree) throw new Error('Merged source differs from the tested PR; do not publish');
    sha = mergedSha;
  }
  const existingTag = await optional(() => github.rest.git.getRef({ ...FORK, ref: `tags/${tag}` }));
  if (existingTag) {
    const frozenSha = (await github.rest.repos.getCommit({ ...FORK, ref: tag })).data.sha;
    if (frozenSha !== sha) throw new Error('Existing release tag differs from the tested commit');
  } else {
    const head = (await github.rest.repos.getBranch({ ...FORK, branch: 'main' })).data.commit.sha;
    if (head !== sha) throw new Error('main changed during validation; retry with the current source');
    const annotated = await github.rest.git.createTag({
      ...FORK, tag, object: sha, type: 'commit',
      message: `${tag}\n\nFork release based on upstream ${upstreamTag}.\nUpstream release: ${release.html_url}\nIncludes upstream main and retained fork changes at ${sha}.\n`,
    });
    await github.rest.git.createRef({ ...FORK, ref: `refs/tags/${tag}`, sha: annotated.data.sha });
  }
  await github.rest.actions.createWorkflowDispatch({
    ...FORK, workflow_id: WORKFLOW, ref: 'main',
    inputs: { tag, simple_release: 'false', dry_run: 'false' },
  });
  core.info(`Dispatched Release ${tag}; follow the Release workflow for publication results`);
}

async function publishOrWait(dependencies, request) {
  try { await publish(dependencies, request); }
  catch (error) {
    if (/^(main changed during PR validation; retry integration|main changed during validation; retry with the current source|Sync PR changed during validation)$/.test(error.message)) {
      dependencies.core.info(`${error.message}; the next synchronization run will validate the new candidate.`);
      return;
    }
    throw error;
  }
}

module.exports = { plan, publish, publishOrWait, releaseTag };
