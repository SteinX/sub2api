'use strict';

const { createHash } = require('node:crypto');
const { readFileSync } = require('node:fs');
const { releaseTag } = require('./sync-upstream.cjs');

const FORK = { owner: 'SteinX', repo: 'sub2api' };
const REPOSITORY = 'SteinX/sub2api';
const SOURCES = new Map([
  ['.github/workflows/sync-upstream.yml', 'Sync upstream release'],
  ['.github/workflows/release.yml', 'Release'],
]);
const BOT = 'github-actions[bot]';
const STATE = /<!-- devin-upstream-state\n([^]*?)\n-->/;
const MAX_DISPATCHES = 5;
const FAILURES = new Set(['failure', 'timed_out', 'startup_failure']);
const TERMINAL = new Set(['exit', 'error']);
const QUOTA = /limit|quota|credits|payment|contract/;

function needsAttention(session) {
  return session.is_archived || TERMINAL.has(session.status) || QUOTA.test(session.status_detail || '')
    || ['finished', 'waiting_for_user', 'waiting_for_approval'].includes(session.status_detail)
    || (session.status === 'suspended' && session.status_detail !== 'inactivity');
}

async function optional(request) {
  try { return (await request()).data; }
  catch (error) { if (error.status === 404) return null; throw error; }
}

function config(env) {
  const org = env.DEVIN_ORG_ID || '';
  const acu = Number(env.DEVIN_MAX_ACU || 10);
  if (!/^org-[A-Za-z0-9-]+$/.test(org)) throw new Error('Set the DEVIN_ORG_ID repository variable to the Devin organization ID.');
  if (!env.DEVIN_API_KEY?.startsWith('cog_')) throw new Error('Set the DEVIN_API_KEY Actions secret to a Devin service-user key.');
  if (!Number.isSafeInteger(acu) || acu < 1 || acu > 100) throw new Error('DEVIN_MAX_ACU must be an integer from 1 to 100.');
  return { org, acu, key: env.DEVIN_API_KEY, playbook: env.DEVIN_PLAYBOOK_ID || undefined };
}

function client(settings, fetchImpl = globalThis.fetch) {
  return async (method, path, body) => {
    let response;
    try {
      response = await fetchImpl(`https://api.devin.ai/v3/organizations/${settings.org}/${path}`, {
        method, redirect: 'error', signal: AbortSignal.timeout(30000),
        headers: { Authorization: `Bearer ${settings.key}`, 'Content-Type': 'application/json' },
        ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      });
    } catch {
      // Never log the request, headers, token, or untrusted error response body.
      throw new Error('Devin request failed or timed out; a write may already have been accepted.');
    }
    if (!response.ok) {
      const error = new Error(`Devin API HTTP ${response.status}. Check service-user permissions, organization, quota and Actions configuration.`);
      error.definitelyRejected = [400, 401, 403, 404, 422, 429].includes(response.status);
      throw error;
    }
    try { return await response.json(); }
    catch { throw new Error('Devin returned invalid JSON; a write may already have been accepted.'); }
  };
}

function sessionId(value) {
  if (!/^devin-[A-Za-z0-9-]+$/.test(value || '')) throw new Error('Invalid Devin session ID; inspect the reserved dispatch before retrying.');
  return value;
}

async function publicImage(tag, fetchImpl = globalThis.fetch) {
  const request = async (url, headers = {}) => {
    const response = await fetchImpl(url, { headers, redirect: 'error', signal: AbortSignal.timeout(30000) });
    if (response.status === 404) return null;
    if (!response.ok) throw new Error(`GHCR verification HTTP ${response.status}; publication status is unknown.`);
    return response.json();
  };
  const auth = await request('https://ghcr.io/token?service=ghcr.io&scope=repository:steinx/sub2api:pull');
  if (!auth?.token) throw new Error('GHCR verification could not obtain a public pull token.');
  const manifest = await request(`https://ghcr.io/v2/steinx/sub2api/manifests/${tag.slice(1)}`, {
    Authorization: `Bearer ${auth.token}`,
    Accept: 'application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json',
  });
  return ['amd64', 'arm64'].every(arch => manifest?.manifests?.some(item =>
    item.platform?.os === 'linux' && item.platform?.architecture === arch));
}

async function publication(github, tag, imageCheck) {
  const release = await optional(() => github.rest.repos.getReleaseByTag({ ...FORK, tag }));
  if (!release || release.draft || release.prerelease) return { complete: false, reason: 'Stable fork Release missing' };
  const targets = ['linux_amd64.tar.gz', 'linux_arm64.tar.gz', 'darwin_amd64.tar.gz', 'darwin_arm64.tar.gz', 'windows_amd64.zip'];
  const expected = ['checksums.txt', ...targets.map(target => `sub2api_${tag.slice(1)}_${target}`)];
  const assets = await github.paginate(github.rest.repos.listReleaseAssets, { ...FORK, release_id: release.id, per_page: 100 });
  const missing = expected.filter(name => !assets.some(asset => asset.name === name && asset.size > 0 && asset.state === 'uploaded'));
  if (missing.length) return { complete: false, reason: `Missing release assets: ${missing.join(', ')}` };
  if (!await imageCheck(tag)) return { complete: false, reason: 'Versioned GHCR linux/amd64 + linux/arm64 manifest missing' };
  return { complete: true, url: `https://github.com/${REPOSITORY}/releases/tag/${tag}` };
}

function managedRun(run, upstreamTag, isRelease) {
  if (run.head_repository?.full_name !== REPOSITORY) return false;
  return isRelease
    ? run.display_title === `Release ${upstreamTag}-stx.1`
    : ['main', `sync/upstream-${upstreamTag}`].includes(run.head_branch);
}

async function publicationPending(github, upstreamTag) {
  const releases = await github.paginate(github.rest.actions.listWorkflowRuns,
    { ...FORK, workflow_id: 'release.yml', per_page: 100 });
  if (releases.some(run => run.status !== 'completed' && managedRun(run, upstreamTag, true)
      && run.display_title === `Release ${upstreamTag}-stx.1`)) return true;
  const syncs = await github.paginate(github.rest.actions.listWorkflowRuns,
    { ...FORK, workflow_id: 'sync-upstream.yml', per_page: 100 });
  const upstream = await optional(() => github.rest.repos.getLatestRelease({ owner: 'Wei-Shaw', repo: 'sub2api' }));
  return syncs.some(run => run.status !== 'completed' && managedRun(run, upstreamTag, false)
    && (run.head_branch !== 'main' || upstream?.tag_name === upstreamTag));
}

async function sourceWork({ github, core, context }, id, imageCheck) {
  if (!/^[1-9]\d*$/.test(String(id || ''))) throw new Error('A valid source workflow run ID is required.');
  const run = (await github.rest.actions.getWorkflowRun({ ...FORK, run_id: Number(id) })).data;
  const path = run.path?.split('@')[0];
  if (!SOURCES.has(path) || run.name !== SOURCES.get(path) || run.repository?.full_name !== REPOSITORY
      || run.head_repository?.full_name !== REPOSITORY || run.status !== 'completed') return null;
  if (context.payload?.workflow_run?.id === run.id && context.payload.workflow_run.run_attempt !== run.run_attempt) return null;
  const upstream = await optional(() => github.rest.repos.getLatestRelease({ owner: 'Wei-Shaw', repo: 'sub2api' }));
  if (!upstream) return null;
  const tag = releaseTag(upstream);
  const isRelease = run.name === 'Release';
  if (isRelease && run.display_title !== `Release ${tag}`) return null;
  if (!isRelease && (!['main', `sync/upstream-${upstream.tag_name}`].includes(run.head_branch)
      || !['schedule', 'workflow_dispatch', 'push', 'pull_request'].includes(run.event))) return null;
  const runs = await github.paginate(github.rest.actions.listWorkflowRuns, { ...FORK, workflow_id: run.workflow_id, per_page: 100 });
  if (runs.some(other => other.id !== run.id && other.run_number > run.run_number
      && managedRun(other, upstream.tag_name, isRelease))) return null;
  if (!FAILURES.has(run.conclusion) && run.conclusion !== 'success') return null;
  if (!isRelease && run.conclusion === 'success') return null;

  const result = await publication(github, tag, imageCheck);
  if (result.complete) return { completed: true, tag, upstreamTag: upstream.tag_name, url: result.url };
  const main = (await github.rest.repos.getBranch({ ...FORK, branch: 'main' })).data.commit.sha;
  const prs = (await github.rest.pulls.list({ ...FORK, base: 'main', head: `SteinX:sync/upstream-${upstream.tag_name}`, state: 'open' })).data;
  let pr;
  if (prs[0]) {
    const candidate = (await github.rest.pulls.get({ ...FORK, pull_number: prs[0].number })).data;
    if (candidate.head.repo?.full_name !== REPOSITORY || candidate.base.ref !== 'main'
        || candidate.head.ref !== `sync/upstream-${upstream.tag_name}`) return null;
    pr = candidate;
  }
  if (!isRelease) {
    if (run.event === 'pull_request' && (!pr || run.head_branch !== pr.head.ref)) return null;
    if (run.head_branch === 'main' && run.head_sha !== main) return null;
    if (run.head_branch !== 'main') {
      if (!pr) return null;
      const snapshot = run.pull_requests?.find(item => item.number === pr.number);
      if (snapshot) {
        if (snapshot.head?.sha !== pr.head.sha || snapshot.base?.sha !== main) return null;
      } else if (run.head_sha !== pr.head.sha) {
        // Some PR runs expose a synthetic merge SHA instead of the branch SHA.
        // Its two parents must match the CURRENT base and managed PR head.
        if (run.event !== 'pull_request') return null;
        const merge = (await github.rest.repos.getCommit({ ...FORK, ref: run.head_sha })).data;
        if (merge.parents?.length !== 2 || merge.parents[0].sha !== main || merge.parents[1].sha !== pr.head.sha) return null;
      }
    }
    // A candidate pushed after this run finished will be validated by its own run.
    if (pr) {
      const commit = (await github.rest.repos.getCommit({ ...FORK, ref: pr.head.sha })).data;
      if (Date.parse(commit.commit.committer.date) > Date.parse(run.updated_at)) return null;
    }
  }
  const jobs = await github.paginate(github.rest.actions.listJobsForWorkflowRun, {
    ...FORK, run_id: run.id, filter: 'latest', per_page: 100,
  });
  const failures = jobs.filter(job => FAILURES.has(job.conclusion)).map(job => ({
    job: job.name, conclusion: job.conclusion,
    steps: (job.steps || []).filter(step => FAILURES.has(step.conclusion)).map(step => step.name).sort(),
  })).sort((a, b) => a.job.localeCompare(b.job));
  if (!failures.length && run.conclusion === 'startup_failure') {
    failures.push({ job: 'Workflow startup', conclusion: 'startup_failure', steps: [] });
  }
  if (run.conclusion !== 'success' && !failures.length) {
    core.info('No failed jobs in the latest attempt; wait for GitHub to settle.');
    return null;
  }
  // mergeability can be temporarily unknown; actual failed validation still needs work.
  if (!isRelease && pr?.mergeable === null && failures.every(job =>
    job.steps.length && job.steps.every(step => step === 'Check upstream and prepare integration PR'))) return null;
  const frozen = await optional(() => github.rest.repos.getCommit({ ...FORK, ref: tag }));
  const work = { tag, upstreamTag: upstream.tag_name, kind: isRelease ? 'release' : 'sync',
    runId: run.id, runAttempt: run.run_attempt, runUrl: `https://github.com/${REPOSITORY}/actions/runs/${run.id}`,
    mainSha: main, prNumber: pr?.number || null, prSha: pr?.head.sha || null,
    frozenSha: frozen?.sha || null, failures,
    ...(isRelease && run.conclusion === 'success' ? { publicationError: result.reason } : {}),
  };
  work.fingerprint = createHash('sha256').update(JSON.stringify({ tag, kind: work.kind, main, head: work.prSha,
    frozen: work.frozenSha, failures, publicationError: work.publicationError })).digest('hex');
  return work;
}

async function trackingIssue(github, upstreamTag, create) {
  const marker = `<!-- devin-upstream-repair:${upstreamTag} -->`;
  const issues = await github.paginate(github.rest.issues.listForRepo, { ...FORK, state: 'all', creator: BOT, per_page: 100 });
  const issue = issues.find(item => !item.pull_request && item.user?.login === BOT && item.body?.startsWith(marker));
  if (issue || !create) return issue;
  return (await github.rest.issues.create({ ...FORK, title: `Devin upstream repair: ${upstreamTag}`,
    body: `${marker}\n\nWaiting to reserve a Devin repair dispatch.`,
  })).data;
}

function readState(issue) {
  const match = issue.body?.match(STATE);
  if (!match) return { version: 1, dispatches: [], phase: 'ready' };
  let state;
  try { state = JSON.parse(match[1]); } catch { throw new Error('Invalid repair state; inspect the tracking issue.'); }
  if (state.version !== 1 || !Array.isArray(state.dispatches) || state.dispatches.length > MAX_DISPATCHES
      || !['ready', 'reserved', 'active', 'complete'].includes(state.phase)) throw new Error('Invalid repair state; inspect the tracking issue.');
  if (state.sessionId) sessionId(state.sessionId);
  return state;
}

async function saveState(github, issue, upstreamTag, state, message) {
  const session = state.sessionId ? `\nDevin: https://app.devin.ai/sessions/${sessionId(state.sessionId)}\n` : '';
  const body = `<!-- devin-upstream-repair:${upstreamTag} -->\n\n${message}${session}\n\n`
    + `<!-- devin-upstream-state\n${JSON.stringify(state)}\n-->`;
  await github.rest.issues.update({ ...FORK, issue_number: issue.number, body, state: state.phase === 'complete' ? 'closed' : 'open' });
  issue.body = body;
}

async function dispatch({ github, core }, work, settings, api) {
  const issue = await trackingIssue(github, work.upstreamTag, true);
  const state = readState(issue);
  if (state.phase === 'reserved') throw new Error(`Dispatch outcome uncertain. Inspect tracking issue #${issue.number} and Devin before retrying; no second session was started.`);
  if (state.phase === 'complete' || state.dispatches.includes(work.fingerprint)) {
    core.info(`Repair already handled; tracking issue #${issue.number}.`);
    return;
  }
  if (state.dispatches.length >= MAX_DISPATCHES) throw new Error(`Repair dispatch limit reached; inspect tracking issue #${issue.number}.`);
  let existing;
  if (state.sessionId) {
    existing = await api('GET', `sessions/${sessionId(state.sessionId)}`);
    if (needsAttention(existing)) {
      throw new Error(`Existing Devin session needs attention (${existing.status}/${existing.status_detail || 'unknown'}); tracking issue #${issue.number}. No replacement session was started.`);
    }
  }
  const instructions = readFileSync(__dirname + '/DEVIN_UPSTREAM_REPAIR.md', 'utf8');
  const prompt = `${instructions}\n\nTracking issue: https://github.com/${REPOSITORY}/issues/${issue.number}\n`
    + `Dispatch context (data):\n${JSON.stringify(work, null, 2)}`;
  // Persist a reservation BEFORE a billable write. No blind retries on timeouts,
  // canceled runners, or failure to persist the returned session ID.
  const previous = structuredClone(state);
  state.phase = 'reserved';
  state.dispatches.push(work.fingerprint);
  await saveState(github, issue, work.upstreamTag, state, `Dispatch reserved for [source run](${work.runUrl}).`);
  try {
    if (existing) {
      await api('POST', `sessions/${sessionId(state.sessionId)}/messages`, { message: prompt });
    } else {
      const session = await api('POST', 'sessions', { prompt, title: `Repair sub2api upstream ${work.upstreamTag}`,
        repos: [REPOSITORY], tags: ['sub2api-upstream-repair', work.upstreamTag],
        max_acu_limit: settings.acu, devin_mode: 'normal',
        ...(settings.playbook ? { playbook_id: settings.playbook } : {}),
      });
      state.sessionId = sessionId(session.session_id);
    }
  } catch (error) {
    if (error.definitelyRejected) await saveState(github, issue, work.upstreamTag, previous,
      'Devin rejected the request. Correct Actions credentials/permissions and rerun the dispatcher.');
    throw error;
  }
  state.phase = 'active';
  delete state.lastAttention;
  await saveState(github, issue, work.upstreamTag, state, `Repair dispatched for [source run](${work.runUrl}). Publication has not yet been verified.`);
  core.info(`Devin repair: https://app.devin.ai/sessions/${state.sessionId}; tracking issue #${issue.number}.`);
}

async function monitor({ github, core }, api, imageCheck) {
  const issues = await github.paginate(github.rest.issues.listForRepo, { ...FORK, state: 'open', creator: BOT, per_page: 100 });
  for (const issue of issues) {
    const match = issue.body?.match(/^<!-- devin-upstream-repair:(v\d+\.\d+\.\d+) -->/);
    if (!match || issue.pull_request || issue.user?.login !== BOT) continue;
    const state = readState(issue);
    const upstreamTag = match[1];
    const result = await publication(github, `${upstreamTag}-stx.1`, imageCheck);
    if (result.complete) {
      await saveState(github, issue, upstreamTag, { ...state, phase: 'complete' },
        `Publication verified: [${upstreamTag}-stx.1](${result.url}), five binary archives, checksums.txt and the versioned GHCR amd64/arm64 manifest.`);
      continue;
    }
    if (!state.sessionId || state.phase !== 'active') continue;
    const session = await api('GET', `sessions/${sessionId(state.sessionId)}`);
    const status = `${session.status}/${session.status_detail || 'unknown'}`;
    const waitingForChecks = session.status_detail === 'finished' && await publicationPending(github, upstreamTag);
    const attention = needsAttention(session) && !waitingForChecks;
    if (attention && state.lastAttention !== status) {
      state.lastAttention = status;
      await saveState(github, issue, upstreamTag, state,
        `Devin needs attention (${status}). Publication is not verified; inspect the session and source workflow before resuming.`);
      core.warning(`Devin repair needs attention (${status}); tracking issue #${issue.number}.`);
    } else if (!attention && state.lastAttention) {
      delete state.lastAttention;
      await saveState(github, issue, upstreamTag, state,
        waitingForChecks ? 'Publication checks are running. Publication has not yet been verified.'
          : 'Devin resumed work. Publication has not yet been verified.');
    }
  }
}

async function run({ github, context, core }, options = {}) {
  if (context.repo.owner !== FORK.owner || context.repo.repo !== FORK.repo) throw new Error('Devin repair is restricted to SteinX/sub2api.');
  const env = options.env || process.env;
  const settings = config(env);
  core.setSecret(settings.key);
  const api = options.api || client(settings);
  if (env.REPAIR_MODE === 'check') {
    // Read only: validate organization membership without starting a paid session.
    await api('GET', 'sessions?first=1');
    core.info('Devin API authentication and organization access verified; no session was created.');
    return;
  }
  const imageCheck = options.imageCheck || publicImage;
  if (env.REPAIR_MODE === 'monitor') {
    await monitor({ github, core }, api, imageCheck);
    return;
  }
  if (env.REPAIR_MODE !== 'repair') throw new Error('Unknown Devin repair mode.');
  const work = await sourceWork({ github, core, context }, env.SOURCE_RUN_ID, imageCheck);
  if (!work) { core.info('Healthy, pending, stale or unrelated source run; no Devin session needed.'); return; }
  if (work.completed) {
    const issue = await trackingIssue(github, work.upstreamTag, false);
    if (issue) await saveState(github, issue, work.upstreamTag, { ...readState(issue), phase: 'complete' },
      `Publication verified: [${work.tag}](${work.url}), five binary archives, checksums.txt and the versioned GHCR amd64/arm64 manifest.`);
    core.info(`Publication verified for ${work.tag}.`);
    return;
  }
  await dispatch({ github, core }, work, settings, api);
}

module.exports = { run, sourceWork, publication, publicImage, dispatch, monitor, readState, config, client };
