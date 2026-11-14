#!/usr/bin/env bash
# Build and exercise the local cycle without installing a host Go/Node toolchain.
set -euo pipefail
if [ "$#" -lt 2 ]; then
  echo "Usage: bash $0 /path/to/adev-profile.yaml /path/to/report.json [runner options]" >&2
  exit 2
fi
for tool in podman python3 git codex; do
  command -v "$tool" >/dev/null || { echo "Missing prerequisite: $tool" >&2; exit 2; }
done
profile="$1"
report="$2"
shift 2
runtime="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
binary_dir="$(mktemp -d)"
trap 'rm -rf "$binary_dir"' EXIT
# Only compiled binaries live here; the non-root worker must be able to read them.
chmod 755 "$binary_dir"
podman run --rm -v "$runtime:/src:ro,Z" -v "$binary_dir:/out:Z" -w /src \
  docker.io/library/golang:1.25-alpine sh -c '
    for component in admission-controller orchestrator worker release-manager relay; do
      CGO_ENABLED=0 go build -o "/out/$component" "./components/$component/cmd" || exit
    done
  '
worker_image=localhost/ai-sdlc-worker:local-cycle
podman build -t "$worker_image" -f "$runtime/components/worker/Containerfile" "$runtime"
python3 "$runtime/tools/validate_local_cycle.py" \
  --binaries "$binary_dir" --worker-image "$worker_image" \
  --adev-profile "$profile" --report "$report" "$@"
