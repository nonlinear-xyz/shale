#!/usr/bin/env python3
"""Make a private, consistent snapshot and real-task drafts. No network calls.

Usage: python3 scripts/prepare_jev_benchmark.py --output /private/tmp/shale-jev-benchmark
Then: go run ./cmd/shale-bench prepare --dir /private/tmp/shale-jev-benchmark
"""
import argparse
import collections
import hashlib
import json
import pathlib
import re
import shutil
import sqlite3


def prepare(source, output):
    source = source.expanduser().resolve()
    output = output.expanduser().resolve()
    if output.exists():
        raise SystemExit('Output already exists; choose a new directory to preserve prior labels/results.')
    output.mkdir(mode=0o700, parents=True)
    snapshot = output / 'store'
    snapshot.mkdir(mode=0o700)
    src = sqlite3.connect((source / 'shale.db').as_uri() + '?mode=ro', uri=True)
    dst = sqlite3.connect(snapshot / 'shale.db')
    src.backup(dst)  # Includes committed WAL data; copying shale.db alone does not.
    src.close()
    for name in ('blobs', 'artifact-blobs'):
        if (source / name).exists():
            shutil.copytree(source / name, snapshot / name)
    fingerprint = hashlib.sha256((snapshot / 'shale.db').read_bytes()).hexdigest()
    groups = collections.defaultdict(list)
    seen = set()
    for seq, scope, payload in dst.execute("SELECT seq, scope, payload FROM events WHERE kind='session.captured' ORDER BY occurred_at DESC, seq DESC"):
        p = json.loads(payload)
        source_key = p.get('sourceKey')
        started = p.get('startedAt')
        if not scope or not source_key or not started or started.startswith('0001') or (scope, source_key) in seen:
            continue
        seen.add((scope, source_key))
        row = dst.execute('SELECT body FROM chunks_fts WHERE event_seq=? AND chunk_index=0', (seq,)).fetchone()
        if not row:
            continue
        # Use an actual user message, not a model-authored session title.
        match = re.search(r'(?:^|\n)user: (.*?)(?=\n(?:tool_use|tool_result|tool_error|assistant|user|system):|\Z)', row[0], re.S)
        if not match:
            continue
        task = match.group(1).strip()
        if len(task) < 30 or len(task.encode()) > 8192:
            continue
        groups[scope].append(dict(id=f'session-{seq}', task=task, repo=scope,
                                 taskSourceRef=f'chunk:{seq}:0', sourceKey=source_key,
                                 at=started, taskReviewed=False))
    # Round-robin repositories for diversity; split whole repositories to avoid
    # related sessions leaking between prompt tuning and held-out evaluation.
    repos = sorted(groups, key=lambda r: hashlib.sha256(r.encode()).hexdigest())
    tuning_repos = set(repos[:max(1, len(repos) // 4)])
    chosen = []
    for split, eligible, cap in [('tuning', tuning_repos, 10), ('heldout', set(repos)-tuning_repos, 30)]:
        queues = {r: list(groups[r]) for r in repos if r in eligible}
        count = 0
        while count < cap and any(queues.values()):
            for repo in queues:
                if queues[repo] and count < cap:
                    chosen.append(dict(queues[repo].pop(0), split=split))
                    count += 1
    tables = {r[0] for r in dst.execute("SELECT name FROM sqlite_master WHERE type='table'")}
    artifact_counts = list(dst.execute('SELECT kind,status,count(*) FROM artifacts GROUP BY kind,status')) if 'artifacts' in tables else []
    manifest = dict(version=1, corpusFingerprint=fingerprint, mode='historical_transcript_only',
                    artifactCounts=artifact_counts, cases=chosen,
                    note='Unreviewed real user-message excerpts. Historical artifact versions are not reconstructed; memory/runbook accuracy is not measured by this sample.')
    (output / 'tasks.json').write_text(json.dumps(manifest, indent=2)+'\n')
    dst.close()
    print(json.dumps(dict(directory=str(output), tasks=len(chosen), artifactCounts=artifact_counts,
                         corpusFingerprint=fingerprint)))


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source', type=pathlib.Path, default=pathlib.Path.home()/'.shale')
    parser.add_argument('--output', type=pathlib.Path, required=True)
    args = parser.parse_args()
    prepare(args.source, args.output)
