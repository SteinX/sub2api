'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { plan, publish, releaseTag } = require('./sync-upstream.cjs');

const SHA = 'a'.repeat(40);
const OTHER = 'b'.repeat(40);
const upstream = { tag_name: 'v0.2.17', draft: false, prerelease: false,
  html_url: 'https://github.com/Wei-Shaw/sub2api/releases/tag/v0.2.17' };
const missing = () => Promise.reject(Object.assign(new Error('Not found'), { status: 404 }));

function fixture(options = {}) {
  const calls = [];
  const output = {};
  const record = (name, value) => async args => {
    calls.push({ name, args });
    if (value instanceof Error) throw value;
    return { data: value };
  };
  return {
    calls, output,
    context: { repo: { owner: 'SteinX', repo: 'sub2api' } },
    core: { info() {}, setOutput(key, value) { output[key] = value; } },
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
          getBranch: record('branch', { commit: { sha: options.head || SHA } }),
          getCommit: record('commit', { sha: options.tagSha || SHA }),
          merge: record('merge', options.conflict ? new Error('Merge conflict') : { sha: SHA }),
        },
        git: {
          getRef: options.tagExists ? record('ref', { object: { sha: OTHER } }) : missing,
          createTag: record('tag', { sha: OTHER }),
          createRef: record('createRef', {}),
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

test('merge main by immutable SHA and validate resulting commit', async () => {
  const f = fixture();
  await plan(f);
  assert.equal(f.output.sha, SHA);
  assert.equal(f.output.tag, request.tag);
  assert.equal(f.output.publish, 'true');
  const merge = f.calls.find(call => call.name === 'merge').args;
  assert.equal(merge.head, SHA);
  assert.equal(merge.base, 'main');
  assert.equal(merge.force, undefined);
});

test('merge conflicts stop before tagging and dispatch', async () => {
  const f = fixture({ conflict: true });
  await assert.rejects(plan(f), /Merge conflict/);
  assert.deepEqual(f.output, {});
  assert.equal(f.calls.some(call => call.name === 'tag' || call.name === 'dispatch'), false);
});

test('in-progress release blocks another publication', async () => {
  const f = fixture({ runs: [{ status: 'in_progress' }] });
  await plan(f);
  assert.deepEqual(f.output, {});
});

test('previous failed publication needs inspection instead of repeated dispatch', async () => {
  const f = fixture({ runs: [{ status: 'completed', conclusion: 'failure',
    display_title: `Release ${request.tag}`, html_url: 'https://github.com/run/1' }] });
  await assert.rejects(plan(f), /rerun its failed jobs/);
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
