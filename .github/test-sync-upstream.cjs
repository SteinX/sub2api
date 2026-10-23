'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { plan, publish, publishOrWait, releaseTag } = require('./sync-upstream.cjs');

const SHA = 'a'.repeat(40);
const OTHER = 'b'.repeat(40);
const CANDIDATE = 'c'.repeat(40);
const MERGED = 'd'.repeat(40);
const upstream = { tag_name: 'v0.2.17', draft: false, prerelease: false,
  html_url: 'https://github.com/Wei-Shaw/sub2api/releases/tag/v0.2.17' };
const missing = () => Promise.reject(Object.assign(new Error('Not found'), { status: 404 }));

function fixture(options = {}) {
  const calls = [];
  const output = {};
  let head = options.head || SHA;
  const pr = { number: 42, html_url: 'https://github.com/SteinX/sub2api/pull/42', state: 'open',
    base: { ref: 'main' }, head: { sha: options.prHead || CANDIDATE,
      ref: 'sync/upstream-v0.2.17', repo: { full_name: 'SteinX/sub2api' } } };
  const record = (name, value) => async args => {
    calls.push({ name, args });
    if (value instanceof Error) throw value;
    return { data: value };
  };
  return {
    calls, output,
    context: { repo: { owner: 'SteinX', repo: 'sub2api' } },
    core: { info() {}, setOutput(key, value) { output[key] = value; }, setFailed(message) { output.error = message; } },
    github: {
      paginate: async () => options.runs || [],
      rest: {
        repos: {
          getLatestRelease: options.noRelease ? missing : record('latest', upstream),
          getReleaseByTag: async args => {
            calls.push({ name: 'release', args });
            if (args.owner === 'Wei-Shaw') return { data: upstream };
            return options.published ? { data: { draft: false } } : missing();
          },
          getBranch: async args => {
            calls.push({ name: 'branch', args });
            return { data: { commit: { sha: args.owner === 'Wei-Shaw' ? OTHER : args.branch === 'main' ? head : CANDIDATE } } };
          },
          compareCommitsWithBasehead: record('compare', { status: options.upstreamChanged ? 'diverged' : 'behind' }),
          getCommit: record('commit', { sha: options.tagSha || SHA }),
          merge: record('merge', options.conflict ? Object.assign(new Error('Merge conflict'), { status: 409 }) : { sha: CANDIDATE }),
        },
        git: {
          getRef: args => args.ref.startsWith('tags/') && options.tagExists
            ? record('ref', { object: { sha: OTHER } })(args) : missing(),
          getCommit: async args => ({ data: { tree: { sha: options.wrongTree && args.commit_sha === MERGED ? OTHER : 'e'.repeat(40) } } }),
          createTag: record('tag', { sha: OTHER }),
          createRef: record('createRef', {}),
        },
        pulls: {
          list: record('listPR', options.prExists ? [pr] : []),
          create: record('createPR', pr),
          get: record('getPR', pr),
          merge: async args => {
            calls.push({ name: 'mergePR', args });
            head = MERGED;
            return { data: { merged: true, sha: MERGED } };
          },
        },
        actions: {
          listWorkflowRuns() {},
          createWorkflowDispatch: record('dispatch', {}),
        },
      },
    },
  };
}

const request = { sha: SHA, tag: 'v0.2.17-stx.1', upstreamTag: 'v0.2.17' };

test('only stable upstream versions become fork tags', () => {
  assert.equal(releaseTag(upstream), request.tag);
  for (const bad of [{ draft: true }, { prerelease: true }, { tag_name: 'v0.2.17-rc.1' }, { tag_name: 'v1; echo unsafe' }]) {
    assert.throws(() => releaseTag({ ...upstream, ...bad }));
  }
});

test('missing or already published upstream releases do no work', async () => {
  for (const options of [{ noRelease: true }, { published: true }]) {
    const f = fixture(options);
    await plan(f);
    assert.deepEqual(f.output, {});
    assert.equal(f.calls.some(call => call.name === 'merge'), false);
  }
});

test('prepare a PR and validate its commit without changing main', async () => {
  const f = fixture({ upstreamChanged: true });
  await plan(f);
  assert.equal(f.output.sha, CANDIDATE);
  assert.equal(f.output.tag, request.tag);
  assert.equal(f.output.publish, 'true');
  const merge = f.calls.find(call => call.name === 'merge').args;
  assert.equal(merge.head, SHA);
  assert.equal(merge.base, 'sync/upstream-v0.2.17');
  assert.equal(merge.force, undefined);
  assert.equal(f.output.pr_number, '42');
  assert.equal(f.calls.some(call => call.name === 'mergePR'), false);
});

test('conflicts leave an actionable PR and stop before tagging or publication', async () => {
  const f = fixture({ conflict: true, upstreamChanged: true });
  await plan(f);
  assert.equal(f.output.agent_needed, 'true');
  assert.equal(f.output.pr_number, '42');
  assert.equal(f.output.publish, undefined);
  assert.equal(f.calls.some(call => call.name === 'tag' || call.name === 'dispatch'), false);
});

test('already integrated upstream needs no PR', async () => {
  const f = fixture();
  await plan(f);
  assert.equal(f.output.sha, SHA);
  assert.equal(f.calls.some(call => call.name === 'createPR' || call.name === 'merge'), false);
});

test('reuse an open sync PR instead of creating another', async () => {
  const f = fixture({ upstreamChanged: true, prExists: true });
  await plan(f);
  assert.equal(f.output.pr_number, '42');
  assert.equal(f.calls.some(call => call.name === 'createPR'), false);
});

test('in-progress release blocks another publication', async () => {
  const f = fixture({ runs: [{ status: 'in_progress' }] });
  await plan(f);
  assert.deepEqual(f.output, {});
});

test('previous failed publication needs inspection instead of repeated dispatch', async () => {
  for (const suffix of ['', ' (simple)']) {
    const f = fixture({ tagExists: true, runs: [{ status: 'completed', conclusion: 'failure',
      display_title: `Release ${request.tag}${suffix}`, html_url: 'https://github.com/run/1' }] });
    await assert.rejects(plan(f), /rerun its failed jobs/);
    assert.equal(f.calls.some(call => call.name === 'dispatch'), false);
  }
});

test('completed dry runs do not block a real publication', async () => {
  const f = fixture({ runs: [{ status: 'completed', conclusion: 'success',
    display_title: `Release ${request.tag} (dry run)` }] });
  await plan(f);
  assert.equal(f.output.publish, 'true');
});

test('interrupted dispatch resumes from frozen tag without merging newer main', async () => {
  const f = fixture({ tagExists: true, tagSha: OTHER });
  await plan(f);
  assert.equal(f.output.sha, OTHER);
  assert.equal(f.calls.some(call => call.name === 'merge'), false);
});

test('main moving during validation prevents publication', async () => {
  const f = fixture({ head: OTHER });
  await assert.rejects(publish(f, request), /main changed/);
  assert.equal(f.calls.some(call => call.name === 'tag' || call.name === 'dispatch'), false);
});

test('successful validation creates an annotated tag and explicitly dispatches full release', async () => {
  const f = fixture();
  await publish(f, request);
  assert.equal(f.calls.find(call => call.name === 'tag').args.object, SHA);
  assert.equal(f.calls.find(call => call.name === 'createRef').args.ref, `refs/tags/${request.tag}`);
  assert.deepEqual(f.calls.find(call => call.name === 'dispatch').args.inputs,
    { tag: request.tag, simple_release: 'false', dry_run: 'false' });
});

test('merge only the tested PR head and publish the matching merged tree', async () => {
  const f = fixture();
  await publish(f, { ...request, sha: CANDIDATE, prNumber: 42, baseSha: SHA });
  assert.equal(f.calls.find(call => call.name === 'mergePR').args.sha, CANDIDATE);
  assert.equal(f.calls.find(call => call.name === 'tag').args.object, MERGED);
});

test('PR head movement and merged tree mismatch both prevent publication', async () => {
  for (const options of [{ prHead: OTHER }, { wrongTree: true }]) {
    const f = fixture(options);
    await assert.rejects(publish(f, { ...request, sha: CANDIDATE, prNumber: 42, baseSha: SHA }));
    assert.equal(f.calls.some(call => call.name === 'tag' || call.name === 'dispatch'), false);
  }
});

test('existing tags are immutable and only the tested commit can be dispatched', async () => {
  const f = fixture({ tagExists: true });
  await publish(f, request);
  assert.equal(f.calls.some(call => call.name === 'tag'), false);
  const mismatch = fixture({ tagExists: true, tagSha: OTHER });
  await assert.rejects(publish(mismatch, request), /differs from the tested commit/);
});

test('another repository cannot run the writer', async () => {
  const f = fixture();
  f.context.repo.owner = 'Wei-Shaw';
  await assert.rejects(plan(f), /restricted/);
  await assert.rejects(publish(f, request), /restricted/);
  assert.deepEqual(f.calls, []);
});

test('publication races wait for fresh validation without becoming repair failures', async () => {
  const f = fixture({ head: OTHER });
  await publishOrWait(f, request);
  assert.equal(f.calls.some(call => call.name === 'tag' || call.name === 'dispatch'), false);
  const moved = fixture({ prHead: OTHER });
  await publishOrWait(moved, { ...request, sha: CANDIDATE, prNumber: 42, baseSha: SHA });
  const mismatch = fixture({ wrongTree: true });
  await assert.rejects(publishOrWait(mismatch, { ...request, sha: CANDIDATE, prNumber: 42, baseSha: SHA }), /differs/);
});
