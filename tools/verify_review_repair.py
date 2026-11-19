#!/usr/bin/env python3
"""Run the connected repair controller in disposable kind and PostgreSQL.

Parent history, source and model replies are explicitly synthetic. Actual SCC,
gVisor, signed inboxes, child budget and cumulative capture are exercised. This
fixture never advances an installed application workflow or claims E2E delivery.
"""
import argparse
import json
from pathlib import Path
from verify_candidate_runtime import verify


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('kubectl', 'kubeconfig', 'worker-image', 'mailbox-image', 'test-binary'):
        parser.add_argument('--' + name, required=True)
    parser.add_argument('--report', type=Path, required=True)
    args = parser.parse_args()
    result = verify([args.kubectl, '--kubeconfig', args.kubeconfig], args.worker_image,
                    args.mailbox_image, args.test_binary, '', 'repair')
    args.report.write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps(result, indent=2))


if __name__ == '__main__':
    main()
