#!/usr/bin/env bash
# One entrypoint; implementation uses only Python's standard library.
set -euo pipefail
repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
exec python3 "$repo_dir/tools/install.py" "$@"
