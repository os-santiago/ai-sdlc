#!/usr/bin/env bash
# The legacy direct-endpoint probe is incompatible with component authority.
set -euo pipefail
echo 'Legacy direct-endpoint probe retired. Use ./install.sh for infrastructure and issue-only intake for autonomous delivery. See runtime/k8s/README.md.' >&2
exit 2
