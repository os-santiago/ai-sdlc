#!/usr/bin/env bash
# One entrypoint; implementation uses only Python's standard library.
set -euo pipefail
repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
if [[ ! -f "$repo_dir/tools/install.py" ]]; then
  command -v git >/dev/null || { echo 'Git is required to fetch AI-SDLC.' >&2; exit 1; }
  source_dir="${AI_SDLC_SOURCE_DIR:-${XDG_DATA_HOME:-$HOME/.local/share}/ai-sdlc/source}"
  source_url='https://github.com/os-santiago/ai-sdlc.git'
  if [[ -n "${AI_SDLC_INSTALL_REF:-}" ]] && [[ ! "$AI_SDLC_INSTALL_REF" =~ ^[a-zA-Z0-9][a-zA-Z0-9._/-]*$ ]]; then
    echo 'Invalid AI_SDLC_INSTALL_REF.' >&2
    exit 1
  fi
  if [[ -e "$source_dir" ]]; then
    [[ -f "$source_dir/tools/install.py" ]] &&
      [[ "$(git -C "$source_dir" remote get-url origin)" == "$source_url" ]] || {
        echo 'Source directory is not an AI-SDLC checkout; choose AI_SDLC_SOURCE_DIR.' >&2
        exit 1
      }
    if [[ -n "${AI_SDLC_INSTALL_REF:-}" ]] &&
      [[ "$(git -C "$source_dir" rev-parse HEAD)" != "$(git -C "$source_dir" rev-parse --verify "${AI_SDLC_INSTALL_REF}^{commit}")" ]]; then
      echo 'Existing checkout differs from AI_SDLC_INSTALL_REF; review and update that checkout explicitly.' >&2
      exit 1
    fi
  else
    mkdir -p -- "$(dirname -- "$source_dir")"
    git clone -- "$source_url" "$source_dir"
    if [[ -n "${AI_SDLC_INSTALL_REF:-}" ]]; then
      git -C "$source_dir" checkout --detach "$AI_SDLC_INSTALL_REF"
    fi
  fi
  repo_dir="$source_dir"
fi
exec python3 "$repo_dir/tools/install.py" "$@"
