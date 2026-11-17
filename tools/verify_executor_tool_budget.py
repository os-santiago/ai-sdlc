"""Exercise SCC tool budgets using an offline, disposable published executor."""
import argparse
import json
from pathlib import Path
import re

from verify_generative_pilot import bounded_container


def verify(provider, image):
    if provider not in ('podman', 'docker') or not re.fullmatch(r'(?:[a-zA-Z0-9./:_-]+@)?sha256:[a-f0-9]{64}', image):
        raise ValueError('Explicit immutable executor image required')
    script = Path(__file__).parent / 'fixtures/scc-tool-budget.mjs'
    code, raw, _ = bounded_container(provider, [
        '--rm', '--pull', 'never', '--network', 'none', '--read-only',
        '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges',
        '--user', '65532:65532', '--pids-limit', '64', '--memory', '512m', '--cpus', '1',
        '--tmpfs', '/tmp:rw,nosuid,nodev,size=128m',
        '-v', str(script.resolve()) + ':/fixture.mjs:ro,z',
        '--entrypoint', 'node', image, '/fixture.mjs'], timeout=180)
    if code:
        raise ValueError('Isolated SCC budget fixture failed')
    report = json.loads(raw)
    cases = report.get('results', [])
    if len(cases) != 2:
        raise ValueError('Both budget cases required')
    for case, steps, exit_code, calls, tools, reason in zip(
            cases, (16, 24), (22, 0), (6, 8), (18, 19), ('budget_exceeded', 'success')):
        if (case.get('maxSteps') != steps or case.get('exitCode') != exit_code
                or case.get('modelCalls') != calls or case.get('toolCalls') != tools
                or case.get('exitReason') != reason or case.get('outputBoundExceeded') is not False
                or case.get('budgetMarkerSteps') is not (exit_code == 22)
                or case.get('candidateFileExists') is not (exit_code == 0)
                or case.get('candidateContentMatches') is not (exit_code == 0)
                or not 0 < case.get('inputBytes', 0) <= 2097152
                or not 0 < case.get('outputBytes', 0) <= 4194304):
            raise ValueError('SCC tool budget or independent call/byte bounds differ')
    report.update(status='passed', executorImage=image, cleanupComplete=True,
                  realModelUsed=False, workflowEvidence=False, autonomousEndToEndValidated=False)
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--provider', required=True, choices=('podman', 'docker'))
    parser.add_argument('--image', required=True)
    args = parser.parse_args()
    print(json.dumps(verify(args.provider, args.image), indent=2))


if __name__ == '__main__':
    main()
