#!/usr/bin/env python3
"""Read a complete, paginated exact-SHA GitHub acceptance snapshot.

Uses gh's existing authentication. Never configures permissions, secrets, checks,
reviews or branch protection. An inaccessible source makes the gate fail closed.
"""
import hashlib
import argparse
import json
import os
import re
import subprocess
from pathlib import Path


class SnapshotError(ValueError):
    def __init__(self, stage, kind):
        super().__init__('complete GitHub snapshot unavailable')
        self.stage, self.kind = stage, kind


def api(route, *arguments):
    proc = subprocess.run(['gh', 'api', '--paginate', '--slurp', route, *arguments], capture_output=True, timeout=90)
    if proc.returncode:
        stage = next((name for name, marker in [('review_threads', 'graphql'), ('check_runs', '/check-runs?'), ('statuses', '/statuses?'),
                     ('reviews', '/reviews?'), ('workflow_jobs', '/jobs?'), ('workflows', '/actions/runs?'), ('pull_request', '/pulls/')] if marker in route), 'github')
        error = proc.stderr.decode(errors='replace').lower()
        kind = 'access_denied' if any(marker in error for marker in ['forbidden', '403', 'not accessible']) else 'unavailable'
        raise SnapshotError(stage, kind)
    return json.loads(proc.stdout)


def collect(repository, pr_number, commit, independent_review):
    if not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', repository) or not re.fullmatch(r'[a-f0-9]{40}', commit) or not isinstance(pr_number, int) or pr_number <= 0:
        raise ValueError('invalid acceptance source')
    root = f'repos/{repository}'
    base = independent_review.get('base', '')
    if not re.fullmatch(r'[a-f0-9]{40}', base):
        raise ValueError('independent review must identify its immutable base')
    patch = subprocess.run(['git', 'diff', '--binary', '--full-index', base, commit], capture_output=True, timeout=30)
    if patch.returncode or hashlib.sha256(patch.stdout).hexdigest() != independent_review.get('patch_sha256'):
        raise ValueError('independent review patch differs from selected source')
    pull = api(f'{root}/pulls/{pr_number}')[0]
    if commit not in {pull['head']['sha'], pull.get('merge_commit_sha')}:
        raise ValueError('pull request does not identify this source')
    runs = [run for page in api(f'{root}/actions/runs?head_sha={commit}&per_page=100') for run in page['workflow_runs']]
    job_metadata = {}
    self_check_ids = set()
    for run in runs:
        for page in api(f"{root}/actions/runs/{run['id']}/jobs?filter=all&per_page=100"):
            for job in page['jobs']:
                check_id = int(job['check_run_url'].rsplit('/', 1)[-1])
                job_metadata[check_id] = {'workflow': run['path'].rsplit('/', 1)[-1], 'event': run['event']}
                # The current release cannot await its own prepare/build jobs.
                # No CI/security/review run is exempted by this exclusion.
                if str(run['id']) == os.environ.get('GITHUB_RUN_ID') and run['path'] == '.github/workflows/release.yml':
                    self_check_ids.add(check_id)
    checks, pending, latest = [], [], {}
    for page in api(f'{root}/commits/{commit}/check-runs?filter=all&per_page=100'):
        for check in page['check_runs']:
            if check['head_sha'] != commit:
                raise ValueError('check source mismatch')
            if check['id'] in self_check_ids:
                continue
            metadata = job_metadata.get(check['id'], {'workflow': 'external', 'event': 'check_run'})
            record = {'name': check['name'], 'head_sha': commit, 'status': check['status'], 'conclusion': check['conclusion'],
                      **metadata, 'id': check['id'], 'app': check['app']['slug']}
            if check['status'] != 'completed':
                pending.append(check['id'])
            key = (record['workflow'], record['event'], record['app'], record['name'])
            if key not in latest or record['id'] > latest[key]['id']:
                latest[key] = record
    checks.extend(latest.values())
    # Commit statuses (including non-Actions review/security tools) are separate
    # from check runs. Retain the latest state of every context, not just success.
    statuses = {}
    for page in api(f'{root}/commits/{commit}/statuses?per_page=100'):
        for status in page:
            statuses.setdefault(status['context'], status)
    for name, status in statuses.items():
        checks.append({'name': name, 'workflow': 'commit-status', 'head_sha': commit,
                       'status': 'in_progress' if status['state'] == 'pending' else 'completed',
                       'conclusion': 'success' if status['state'] == 'success' else status['state']})
    reviews = []
    for page in api(f'{root}/pulls/{pr_number}/reviews?per_page=100'):
        for review in page:
            reviews.append({'reviewer': hashlib.sha256(review['user']['login'].encode()).hexdigest(),
                            'state': review['state'], 'commit': review['commit_id']})
    owner, name = repository.split('/')
    query = '''query($owner:String!,$name:String!,$number:Int!,$endCursor:String){repository(owner:$owner,name:$name){pullRequest(number:$number){reviewThreads(first:100,after:$endCursor){nodes{isResolved}pageInfo{hasNextPage endCursor}}}}}'''
    unresolved = 0
    for page in api('graphql', '-f', 'query=' + query, '-F', 'owner=' + owner, '-F', 'name=' + name, '-F', 'number=' + str(pr_number)):
        if page.get('errors'):
            raise ValueError('review thread state unavailable')
        threads = page['data']['repository']['pullRequest']['reviewThreads']['nodes']
        unresolved += sum(not thread['isResolved'] for thread in threads)
    return {'schema_version': 1, 'commit': commit, 'repository': repository, 'pr': pr_number,
            'snapshot_complete': True, 'checks': checks, 'outstanding_checks': pending,
            'reviews': reviews, 'unresolved_findings': unresolved, 'independent_review': independent_review,
            'collection': 'all paginated check runs, workflow jobs, statuses, PR reviews and review threads'}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--repository', required=True)
    parser.add_argument('--pr', required=True, type=int)
    parser.add_argument('--commit', required=True)
    parser.add_argument('--review', required=True, type=Path)
    parser.add_argument('--output', required=True, type=Path)
    args = parser.parse_args()
    try:
        state = collect(args.repository, args.pr, args.commit, json.loads(args.review.read_text()))
        args.output.write_text(json.dumps(state, indent=2) + '\n')
        args.output.chmod(0o600)
    except SnapshotError as error:
        print(json.dumps({'collected': False, 'stage': error.stage, 'reason': error.kind}))
        return 1
    except (ValueError, KeyError, TypeError, OSError, subprocess.SubprocessError):
        print(json.dumps({'collected': False, 'reason': 'complete_exact_sha_review_security_snapshot_unavailable'}))
        return 1
    print(json.dumps({'collected': True, 'commit': args.commit, 'checks': len(state['checks']), 'unresolved_findings': state['unresolved_findings']}))
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
