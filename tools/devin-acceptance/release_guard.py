#!/usr/bin/env python3
"""Require source-bound user E2E and fresh review/security state before release.

The annotation is an auditable maintainer attestation, not a cryptographic proof.
This tool performs only Git/GitHub reads and emits a small verdict.
"""
import argparse
import base64
import json
import subprocess

from acceptance import PREFIX, validate
from github_snapshot import collect

def git(*arguments):
    return subprocess.check_output(['git', *arguments], stderr=subprocess.DEVNULL, timeout=30).decode().strip()


def requires_acceptance(commit):
    try:
        git('cat-file', '-e', commit + ':backend/internal/service/openai_gateway_devin_fusion.go')
    except subprocess.CalledProcessError:
        return False  # Preserve historical releases predating the Devin feature.
    # A failed release still leaves its tag behind. Never treat a nearby tag
    # (or a documentation-only diff from it) as evidence of prior acceptance.
    return True


def evaluate(ref, repository):
    commit = git('rev-parse', '--verify', '--end-of-options', ref + '^{commit}')
    tree = git('rev-parse', commit + '^{tree}')
    if not requires_acceptance(commit):
        return {'accepted': True, 'required': False, 'commit': commit, 'tree': tree}
    if git('cat-file', '-t', 'refs/tags/' + ref) != 'tag':
        raise ValueError('source-bound acceptance requires an annotated release tag')
    annotation = git('for-each-ref', '--format=%(contents)', 'refs/tags/' + ref)
    notes = [line[len(PREFIX):] for line in annotation.splitlines() if line.startswith(PREFIX)]
    if len(notes) != 1 or len(notes[0]) > 1024 * 1024:
        raise ValueError('one sanitized user E2E acceptance annotation is required')
    payload = json.loads(base64.b64decode(notes[0], validate=True))
    report, attestation = payload['report'], payload['prerequisites']
    # Never trust the tag's historical green-check list. Query all current
    # exact-SHA checks and review/security state afresh before publishing.
    state = collect(repository, attestation['pr'], commit, attestation['independent_review'])
    errors = validate(report, 'user_e2e', commit, tree, state)
    if errors:
        return {'accepted': False, 'required': True, 'commit': commit, 'reasons': sorted(set(errors))}
    return {'accepted': True, 'required': True, 'commit': commit, 'tree': tree}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--ref', required=True)
    parser.add_argument('--repository', required=True)
    parser.add_argument('--dry-run', action='store_true')
    args = parser.parse_args()
    try:
        verdict = evaluate(args.ref, args.repository)
    except (ValueError, KeyError, TypeError, OSError, subprocess.SubprocessError):
        verdict = {'accepted': False, 'reason': 'user_e2e_or_current_review_evidence_unavailable'}
    verdict['dry_run'] = args.dry_run
    print(json.dumps(verdict))
    return 0 if verdict['accepted'] or args.dry_run else 1


if __name__ == '__main__':
    raise SystemExit(main())
