#!/usr/bin/env python3
"""Require declared source-bound validation and fresh review/security state.

The annotation is an auditable maintainer attestation, not a cryptographic proof.
This tool performs only Git/GitHub reads and emits a small verdict.
"""
import argparse
import base64
import json
import subprocess

from acceptance import PREFIX, validate, review_state_errors
from github_snapshot import collect

REVIEWED_PREFIX = 'Sub2API-Reviewed-CI-Release-v1: '


def reviewed_release_errors(release, commit, tree):
    expected = {'schema_version', 'commit', 'tree', 'validation', 'real_user_e2e_certified', 'limitations'}
    if (not isinstance(release, dict) or set(release) != expected
            or type(release.get('schema_version')) is not int or release['schema_version'] != 1
            or release.get('commit') != commit or release.get('tree') != tree
            or release.get('validation') != 'ci_and_independent_review'
            or release.get('real_user_e2e_certified') is not False
            or release.get('limitations') != ['real_upstream_acceptance_not_certified']):
        return ['invalid_reviewed_ci_release_declaration']
    return []

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
    notes = [(prefix, line[len(prefix):]) for line in annotation.splitlines()
             for prefix in [PREFIX, REVIEWED_PREFIX] if line.startswith(prefix)]
    if len(notes) != 1 or len(notes[0][1]) > 1024 * 1024:
        raise ValueError('one sanitized release validation annotation is required')
    prefix, encoded = notes[0]
    payload = json.loads(base64.b64decode(encoded, validate=True))
    attestation = payload['prerequisites']
    if prefix == REVIEWED_PREFIX:
        errors = reviewed_release_errors(payload.get('release'), commit, tree)
        if errors:
            return {'accepted': False, 'required': True, 'commit': commit, 'reasons': errors}
    # Never trust the tag's historical green-check list. Query all current
    # exact-SHA checks and review/security state afresh before publishing.
    state = collect(repository, attestation['pr'], commit, attestation['independent_review'])
    errors = (review_state_errors(state, commit) if prefix == REVIEWED_PREFIX
              else validate(payload['report'], 'user_e2e', commit, tree, state))
    if errors:
        return {'accepted': False, 'required': True, 'commit': commit, 'reasons': sorted(set(errors))}
    return {'accepted': True, 'required': True, 'commit': commit, 'tree': tree,
            'validation': 'ci_and_independent_review' if prefix == REVIEWED_PREFIX else 'user_e2e',
            'real_user_e2e_certified': prefix == PREFIX}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--ref', required=True)
    parser.add_argument('--repository', required=True)
    parser.add_argument('--dry-run', action='store_true')
    args = parser.parse_args()
    try:
        verdict = evaluate(args.ref, args.repository)
    except (ValueError, KeyError, TypeError, OSError, subprocess.SubprocessError):
        verdict = {'accepted': False, 'reason': 'declared_validation_or_current_review_evidence_unavailable'}
    verdict['dry_run'] = args.dry_run
    print(json.dumps(verdict))
    return 0 if verdict['accepted'] or args.dry_run else 1


if __name__ == '__main__':
    raise SystemExit(main())
