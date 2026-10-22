'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { run, sourceWork, publication, publicImage, dispatch, readState, config, client } = require('./devin-upstream-repair.cjs');

const SHA = 'a'.repeat(40);
const HEAD = 'b'.repeat(40);
const TAG = 'v0.2.17-stx.1';
const env = { DEVIN_ORG_ID: 'org-test', DEVIN_API_KEY: 'cog_test', DEVIN_MAX_ACU: '10', REPAIR_MODE: 'repair', SOURCE_RUN_ID: '100' };
const settings = config(env);
const missing = () => Promise.reject(Object.assign(new Error('Not found'), { status: 404 }));

function fixture(options = {}) {
  const calls = [];
  const source = { id: 100, run_attempt: 1, run_number: 10, workflow_id: 7, event: 'schedule',
    path: '.github/workflows/sync-upstream.yml', name: 'Sync upstream release',
    status: 'completed', conclusion: 'failure', head_branch: 'main', head_sha: SHA,
    updated_at: '2026-10-22T01:00:00Z', repository: { full_name: 'SteinX/sub2api' },
    head_repository: { full_name: 'SteinX/sub2api' }, ...options.source };
  let issue = options.issue;
  const assets = ['checksums.txt', ...['linux_amd64.tar.gz', 'linux_arm64.tar.gz', 'darwin_amd64.tar.gz', 'darwin_arm64.tar.gz', 'windows_amd64.zip']
    .map(target => `sub2api_${TAG.slice(1)}_${target}`)].map(name => ({ name, size: 1024, state: 'uploaded' }));
  const pr = { number: 42, state: 'open', mergeable: false, head: { sha: HEAD, ref: 'sync/upstream-v0.2.17', repo: { full_name: 'SteinX/sub2api' } },
    base: { ref: 'main' }, ...options.pr };
  const jobs = [{ name: 'sync', conclusion: 'failure', steps: [{ name: 'Check upstream and prepare integration PR', conclusion: 'failure' }] }];
  const methods = {
    getWorkflowRun: async () => ({ data: source }),
    listWorkflowRuns: async () => ({ data: options.runs || [source] }),
    listJobsForWorkflowRun: async () => ({ data: options.jobs || jobs }),
    getLatestRelease: async () => ({ data: { tag_name: 'v0.2.17', draft: false, prerelease: false } }),
    getReleaseByTag: options.published ? async () => ({ data: { id: 5, draft: false, prerelease: !!options.prerelease } }) : missing,
    listReleaseAssets: async () => ({ data: options.assets || assets }),
    getBranch: async () => ({ data: { commit: { sha: options.main || SHA } } }),
    getCommit: async args => args.ref === TAG ? missing() : ({ data: { sha: HEAD,
      commit: { committer: { date: options.commitDate || '2026-10-22T00:30:00Z' } } } }),
    list: async () => ({ data: options.noPr ? [] : [pr] }),
    get: async () => ({ data: pr }),
    listForRepo: async () => ({ data: issue ? [issue] : [] }),
    create: async args => {
      calls.push({ name: 'createIssue', args });
      issue = { number: 9, user: { login: 'github-actions[bot]' }, body: args.body };
      return { data: issue };
    },
    update: async args => {
      calls.push({ name: 'updateIssue', args });
      if (options.failSave && args.body.includes('"phase":"active"')) throw new Error('GitHub write failed');
      issue.body = args.body;
      return { data: issue };
    },
  };
  const github = { rest: { actions: methods, repos: methods, pulls: methods, issues: methods },
    paginate: async (method, args) => (await method(args)).data };
  const core = { info: message => calls.push({ name: 'info', message }), setSecret() {} };
  const context = { repo: { owner: 'SteinX', repo: 'sub2api' }, payload: options.payload || {} };
  const api = async (method, path, body) => {
    calls.push({ name: 'api', method, path, body });
    if (options.apiError && method === 'POST') throw options.apiError;
    return { session_id: 'devin-test', status: 'running', status_detail: 'working', ...options.session };
  };
  return { github, core, context, calls, api, source, imageCheck: async () => options.image !== false,
    get issue() { return issue; } };
}

async function execute(f, values = {}) {
  await run(f, { env: { ...env, ...values }, api: f.api, imageCheck: f.imageCheck });
}

test('credential check is read-only and never starts a billable session or tracking issue', async () => {
  const f = fixture();
  await execute(f, { REPAIR_MODE: 'check', SOURCE_RUN_ID: '' });
  assert.deepEqual(f.calls.filter(call => call.name === 'api').map(call => [call.method, call.path]), [['GET', 'sessions?first=1']]);
  assert.equal(f.issue, undefined);
});

test('only the fork can dispatch; configuration rejects missing credentials and unbounded budgets', async () => {
  for (const values of [{ DEVIN_API_KEY: '' }, { DEVIN_ORG_ID: '../elsewhere' }, { DEVIN_MAX_ACU: '0' }, { DEVIN_MAX_ACU: '101' }, { DEVIN_MAX_ACU: '1.5' }]) {
    assert.throws(() => config({ ...env, ...values }));
  }
  const f = fixture();
  f.context.repo.owner = 'Wei-Shaw';
  await assert.rejects(execute(f), /restricted/);
  assert.equal(f.calls.length, 0);
});

test('current conflicts reserve first and create one bounded cloud session with fork instructions', async () => {
  const f = fixture();
  await execute(f);
  const post = f.calls.find(call => call.name === 'api');
  assert.equal(post.method, 'POST');
  assert.equal(post.path, 'sessions');
  assert.equal(post.body.max_acu_limit, 10);
  assert.deepEqual(post.body.repos, ['SteinX/sub2api']);
  assert.equal(post.body.bypass_approval, undefined);
  assert.match(post.body.prompt, /Preserve merge history/);
  assert.match(post.body.prompt, /Do not deploy servers/);
  assert.match(f.calls.find(call => call.name === 'updateIssue').args.body, /"phase":"reserved"/);
  assert.equal(readState(f.issue).phase, 'active');
  assert.equal(readState(f.issue).sessionId, 'devin-test');
  const posts = () => f.calls.filter(call => call.name === 'api' && call.method === 'POST');
  await execute(f);
  assert.equal(posts().length, 1);
});

test('repeated hourly failures have the same fingerprint even with different run IDs', async () => {
  const f = fixture();
  await execute(f);
  f.source.id = 101;
  f.source.run_number++;
  await execute(f, { SOURCE_RUN_ID: '101' });
  assert.equal(f.calls.filter(call => call.name === 'api' && call.method === 'POST').length, 1);
});

test('new failing code is sent to the existing session, not a replacement', async () => {
  const f = fixture();
  await execute(f);
  f.github.rest.pulls.get = async () => ({ data: { number: 42, mergeable: true, head: { sha: 'c'.repeat(40), ref: 'sync/upstream-v0.2.17', repo: { full_name: 'SteinX/sub2api' } }, base: { ref: 'main' } } });
  await execute(f);
  const posts = f.calls.filter(call => call.name === 'api' && call.method === 'POST');
  assert.deepEqual(posts.map(call => call.path), ['sessions', 'sessions/devin-test/messages']);
});

test('timeouts and a lost GitHub save keep the reservation and block duplicate sessions', async () => {
  for (const options of [{ apiError: new Error('timeout') }, { failSave: true }]) {
    const f = fixture(options);
    await assert.rejects(execute(f));
    assert.equal(readState(f.issue).phase, 'reserved');
    await assert.rejects(execute(f), /uncertain/);
    assert.equal(f.calls.filter(call => call.name === 'api' && call.method === 'POST').length, 1);
  }
});

test('definite API rejection releases the reservation so corrected configuration can retry', async () => {
  const options = { apiError: Object.assign(new Error('HTTP 403'), { definitelyRejected: true }) };
  const f = fixture(options);
  await assert.rejects(execute(f), /403/);
  assert.equal(readState(f.issue).phase, 'ready');
  delete options.apiError;
  await execute(f);
  assert.equal(readState(f.issue).phase, 'active');
});

test('quota, terminal and archived sessions stop rather than creating a new budget', async () => {
  for (const session of [{ status: 'error' }, { status: 'exit' }, { status: 'suspended', status_detail: 'usage_limit_exceeded' }, { is_archived: true }]) {
    const f = fixture({ session });
    await execute(f);
    const work = await sourceWork(f, '100', f.imageCheck);
    work.fingerprint = 'new-code';
    await assert.rejects(dispatch(f, work, settings, f.api), /needs attention/);
    assert.equal(f.calls.filter(call => call.name === 'api' && call.method === 'POST').length, 1);
  }
});

test('at most five distinct dispatches share the session budget', async () => {
  const f = fixture();
  const work = await sourceWork(f, '100', f.imageCheck);
  for (let i = 0; i < 5; i++) await dispatch(f, { ...work, fingerprint: String(i) }, settings, f.api);
  await assert.rejects(dispatch(f, { ...work, fingerprint: 'six' }, settings, f.api), /limit reached/);
  assert.equal(f.calls.filter(call => call.name === 'api' && call.path === 'sessions').length, 1);
});

test('unrelated, untrusted, canceled, healthy and pending runs never call Devin', async () => {
  for (const source of [{ path: '.github/workflows/backend-ci.yml' }, { repository: { full_name: 'other/repo' } },
    { head_repository: { full_name: 'other/sub2api' } }, { head_branch: 'feat/devin' }, { conclusion: 'cancelled' },
    { conclusion: 'success' }, { status: 'in_progress' }, { event: 'pull_request_target' }]) {
    const f = fixture({ source });
    await execute(f);
    assert.equal(f.calls.some(call => call.name === 'api'), false, JSON.stringify(source));
  }
});

test('newer runs, source attempts, moved main and newer candidate commits suppress stale work', async () => {
  for (const options of [
    { runs: [{ id: 101, run_number: 11, status: 'in_progress' }] },
    { payload: { workflow_run: { id: 100, run_attempt: 0 } } },
    { main: HEAD },
    { commitDate: '2026-10-22T02:00:00Z' },
    { source: { head_branch: 'sync/upstream-v0.2.17', event: 'pull_request', head_sha: SHA } },
  ]) {
    const f = fixture(options);
    await execute(f);
    assert.equal(f.calls.some(call => call.name === 'api'), false);
  }
});

test('unknown mergeability waits on planning failures but actual failed tests are repaired', async () => {
  const wait = fixture({ pr: { mergeable: null } });
  await execute(wait);
  assert.equal(wait.issue, undefined);
  const tests = fixture({ pr: { mergeable: null }, jobs: [{ name: 'sync', conclusion: 'failure', steps: [{ name: 'Validate backend', conclusion: 'failure' }] }] });
  await execute(tests);
  assert.equal(readState(tests.issue).phase, 'active');
});

test('failed fork publication can be repaired without an open PR; other versions are excluded', async () => {
  const source = { name: 'Release', path: '.github/workflows/release.yml', display_title: `Release ${TAG}` };
  const f = fixture({ source, noPr: true });
  await execute(f);
  assert.equal(readState(f.issue).phase, 'active');
  for (const title of ['Release v0.2.16-stx.1', 'Release main', 'Release v0.2.17']) {
    const other = fixture({ source: { ...source, display_title: title } });
    await execute(other);
    assert.equal(other.issue, undefined);
  }
});

test('successful workflow with missing artifacts still requires repair', async () => {
  const f = fixture({ source: { name: 'Release', path: '.github/workflows/release.yml', display_title: `Release ${TAG}`, conclusion: 'success' }, published: true, assets: [] });
  await execute(f);
  assert.match(f.calls.find(call => call.name === 'api').body.prompt, /Missing release assets/);
});

test('publication success requires all archives and the versioned multiarch image', async () => {
  for (const options of [{}, { published: true, assets: [] }, { published: true, prerelease: true }, { published: true, image: false }]) {
    const f = fixture(options);
    assert.equal((await publication(f.github, TAG, f.imageCheck)).complete, false);
  }
  const f = fixture({ published: true });
  assert.equal((await publication(f.github, TAG, f.imageCheck)).complete, true);
});

test('independently verified publication closes the tracking issue, not merely a finished session', async () => {
  const first = fixture();
  await execute(first);
  const f = fixture({ issue: first.issue, published: true, source: { name: 'Release', path: '.github/workflows/release.yml', display_title: `Release ${TAG}`, conclusion: 'success' } });
  await execute(f);
  assert.equal(readState(f.issue).phase, 'complete');
  assert.equal(f.calls.find(call => call.name === 'updateIssue').args.state, 'closed');
  assert.equal(f.calls.some(call => call.name === 'api'), false);
});

test('non-bot tracking issues are ignored and malformed machine state fails closed', async () => {
  const fake = { number: 1, user: { login: 'visitor' }, body: '<!-- devin-upstream-repair:v0.2.17 -->\n<!-- devin-upstream-state\n{}\n-->' };
  const f = fixture({ issue: fake });
  await execute(f);
  assert.equal(f.calls.some(call => call.name === 'createIssue'), true);
  assert.throws(() => readState({ body: '<!-- devin-upstream-state\nbad json\n-->' }), /Invalid/);
});

test('API client uses only the fixed Devin origin, rejects redirects and redacts remote errors', async () => {
  let request;
  const api = client(settings, async (url, options) => { request = { url, options }; return { ok: true, json: async () => ({ status: 'running' }) }; });
  await api('POST', 'sessions', { prompt: 'repair' });
  assert.equal(request.url, 'https://api.devin.ai/v3/organizations/org-test/sessions');
  assert.equal(request.options.redirect, 'error');
  for (const status of [401, 403, 422, 429, 500]) {
    const denied = client(settings, async () => ({ ok: false, status, text: async () => settings.key }));
    await assert.rejects(denied('POST', 'sessions'), error => !error.message.includes(settings.key)
      && !!error.definitelyRejected === (status < 500));
  }
});

test('GHCR check validates both Linux architectures and treats service errors as unknown', async () => {
  const response = platforms => async url => ({ ok: true, json: async () => url.includes('/token?')
    ? { token: 'registry-token' } : { manifests: platforms.map(architecture => ({ platform: { os: 'linux', architecture } })) } });
  assert.equal(await publicImage(TAG, response(['amd64', 'arm64'])), true);
  assert.equal(await publicImage(TAG, response(['amd64'])), false);
  await assert.rejects(publicImage(TAG, async () => ({ ok: false, status: 503 })), /unknown/);
});

test('hourly monitoring reads session status without creating or messaging a session', async () => {
  const first = fixture();
  await execute(first);
  const f = fixture({ issue: first.issue, session: { status: 'suspended', status_detail: 'usage_limit_exceeded' } });
  f.core.warning = message => f.calls.push({ name: 'warning', message });
  await execute(f, { REPAIR_MODE: 'monitor', SOURCE_RUN_ID: '' });
  assert.equal(f.calls.filter(call => call.name === 'api' && call.method === 'POST').length, 0);
  assert.equal(f.calls.filter(call => call.name === 'updateIssue').length, 1);
  await execute(f, { REPAIR_MODE: 'monitor', SOURCE_RUN_ID: '' });
  assert.equal(f.calls.filter(call => call.name === 'updateIssue').length, 1);
  assert.equal(f.calls.filter(call => call.name === 'warning').length, 1);
});

test('monitor independently closes a published repair without reading or waking Devin', async () => {
  const first = fixture();
  await execute(first);
  const f = fixture({ issue: first.issue, published: true });
  await execute(f, { REPAIR_MODE: 'monitor', SOURCE_RUN_ID: '' });
  assert.equal(readState(f.issue).phase, 'complete');
  assert.equal(f.calls.some(call => call.name === 'api'), false);
});

test('a finished session without verified publication raises attention only once', async () => {
  const first = fixture();
  await execute(first);
  const f = fixture({ issue: first.issue, session: { status: 'running', status_detail: 'finished' } });
  f.core.warning = message => f.calls.push({ name: 'warning', message });
  await execute(f, { REPAIR_MODE: 'monitor', SOURCE_RUN_ID: '' });
  assert.equal(readState(f.issue).phase, 'active');
  assert.equal(readState(f.issue).lastAttention, 'running/finished');
  assert.match(f.issue.body, /Publication is not verified/);
  assert.equal(f.calls.filter(call => call.name === 'warning').length, 1);
  await execute(f, { REPAIR_MODE: 'monitor', SOURCE_RUN_ID: '' });
  assert.equal(f.calls.filter(call => call.name === 'warning').length, 1);
  assert.equal(f.calls.some(call => call.name === 'api' && call.method === 'POST'), false);
});

test('an intentional release dry run cannot dispatch a missing-artifacts repair', async () => {
  const f = fixture({ source: { name: 'Release', path: '.github/workflows/release.yml',
    display_title: `Release ${TAG} (dry run)`, conclusion: 'success' } });
  await execute(f);
  assert.equal(f.issue, undefined);
});

test('simple releases are excluded even when their fork version matches the latest target', async () => {
  for (const conclusion of ['success', 'failure']) {
    const f = fixture({ source: { name: 'Release', path: '.github/workflows/release.yml',
      display_title: `Release ${TAG} (simple)`, conclusion } });
    await execute(f);
    assert.equal(f.issue, undefined);
  }
});

test('managed PR runs accept current PR snapshots even when run SHA is a synthetic merge', async () => {
  const source = { event: 'pull_request', head_branch: 'sync/upstream-v0.2.17', head_sha: 'c'.repeat(40),
    pull_requests: [{ number: 42, head: { sha: HEAD }, base: { sha: SHA } }] };
  const f = fixture({ source });
  await execute(f);
  assert.equal(readState(f.issue).phase, 'active');
  for (const snapshot of [{ number: 42, head: { sha: SHA }, base: { sha: SHA } },
    { number: 42, head: { sha: HEAD }, base: { sha: HEAD } }]) {
    const stale = fixture({ source: { ...source, pull_requests: [snapshot] } });
    await execute(stale);
    assert.equal(stale.issue, undefined);
  }
});

test('without PR metadata a merge run must have both current source parents', async () => {
  for (const current of [true, false]) {
    const f = fixture({ source: { event: 'pull_request', head_branch: 'sync/upstream-v0.2.17', head_sha: 'c'.repeat(40) } });
    const getCommit = f.github.rest.repos.getCommit;
    f.github.rest.repos.getCommit = async args => args.ref === 'c'.repeat(40)
      ? { data: { parents: [{ sha: SHA }, { sha: current ? HEAD : SHA }] } } : getCommit(args);
    await execute(f);
    assert.equal(!!f.issue, current);
  }
});
