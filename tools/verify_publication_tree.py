#!/usr/bin/env python3
"""Compare the bounded publication plan with actual Git objects, without a push."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile

from verify_workspace_bundle import source_bundle, wire

SOURCE_FILE = 'quarkus-app/src/main/resources/META-INF/resources/js/utils.js'
TEST_FILE = 'tests/js/utils-locale-fallback.test.js'


def verify(source_repository, source_sha, candidate_directory, test_binary):
    bundle = source_bundle(source_repository, source_sha)
    with tempfile.TemporaryDirectory(prefix='ai-sdlc-publication-tree-') as directory:
        root = Path(directory)
        source, control, report = root/'source.json', root/'input.json', root/'report.json'
        source.write_bytes(wire(bundle))
        control.write_text(json.dumps(dict(SourceFile=str(source), CandidateDirectory=str(candidate_directory.resolve()), ReportFile=str(report))))
        for path in (source, control):
            path.chmod(0o600)
        result = subprocess.run([str(test_binary.resolve()), '-test.run', '^TestRealHomedirPublicationTreeFixture$'],
                                env=dict(os.environ, AI_SDLC_PUBLICATION_TREE_FIXTURE=str(control)),
                                capture_output=True, timeout=60)
        if result.returncode:
            raise ValueError('Bounded publication fixture failed')
        proof = json.loads(report.read_bytes())
        # Shared local object storage is read-only through alternates. Every new
        # blob/tree and index belongs to this disposable bare repository.
        env = {key:value for key,value in os.environ.items() if not key.startswith('GIT_')}
        env.update(GIT_CONFIG_NOSYSTEM='1', GIT_CONFIG_GLOBAL=os.devnull,
                   GIT_TERMINAL_PROMPT='0', GIT_INDEX_FILE=str(root/'index'))
        repo = root/'objects.git'
        def git(args, value=None):
            return subprocess.run(['git', '-C', str(repo), *args], input=value, env=env,
                                  capture_output=True, check=True, timeout=20).stdout.strip()
        subprocess.run(['git', 'clone', '--bare', '--shared', '--no-hardlinks', '--', str(source_repository.resolve()), str(repo)],
                       env=env, capture_output=True, check=True, timeout=20)
        original = git(['rev-parse', source_sha+'^{tree}']).decode()
        if original != proof['sourceGitTreeSHA']:
            raise ValueError('Source Git tree differs from independent reconstruction')
        git(['read-tree', source_sha])
        for name in (SOURCE_FILE, TEST_FILE):
            with (candidate_directory/name).open('rb') as stream:
                data = stream.read(32769)
            if not 1 <= len(data) <= 32768:
                raise ValueError('Candidate file exceeded bound')
            blob = git(['hash-object', '-w', '--stdin'], data).decode()
            git(['update-index', '--add', '--cacheinfo', '100644', blob, name])
        actual = git(['write-tree']).decode()
        if actual != proof['candidateGitTreeSHA']:
            raise ValueError('Candidate Git tree differs from independently computed plan')
        proof.update(actualGitObjectsCompared=True, sourceCheckoutModified=False)
    proof['temporaryRepositoryRemoved'] = True
    return proof


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('source-repository', 'candidate-directory', 'test-binary'):
        parser.add_argument('--'+name, required=True, type=Path)
    parser.add_argument('--source-sha', required=True)
    args = parser.parse_args()
    print(json.dumps(verify(args.source_repository, args.source_sha, args.candidate_directory, args.test_binary), indent=2))


if __name__ == '__main__':
    main()
