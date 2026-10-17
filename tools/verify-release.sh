#!/usr/bin/env bash
# tools/verify-release.sh — consumer-side provenance verification for
# ai-sdlc release artifacts (issues #100, #103).
#
# Every published release asset set carries, next to the
# `ai-sdlc-<tag>.tar.gz` git-archive source tarball:
#   <artifact>.sig             detached cosign signature (base64)
#   <artifact>.crt             Fulcio-issued certificate for the ephemeral key
#   <artifact>.sigstore.json   Sigstore bundle (sig + cert + Rekor inclusion)
#   <artifact>.attestation.jsonl  SLSA build-provenance attestation bundle
#   ai-sdlc-<tag>.cdx.json     CycloneDX SBOM (npm surface) when the
#                              security.sbom contract is on — attested
#                              alongside the tarball (issue #92)
#
# This script downloads the asset set with `gh release download` and
# verifies BOTH trust surfaces:
#   1. cosign signature — `cosign verify-blob` over .sig + .crt, asserting
#      the certificate identity is this repo's ai-sdlc-release.yml workflow
#      (GitHub OIDC) and the issuer is token.actions.githubusercontent.com.
#      When the .sigstore.json bundle is present it is verified too
#      (bundle carries the Rekor inclusion proof — the stronger path).
#   2. SLSA provenance — `gh attestation verify` (SLSA L3-class, GitHub
#      Artifact Attestations). With the downloaded .attestation.jsonl an
#      offline-style check runs as well (`--bundle` + trusted root).
#
# Usage:
#   tools/verify-release.sh <tag> [--repo OWNER/REPO] [--dir DIR]
#
# Defaults: --repo os-santiago/ai-sdlc, --dir <mktemp -d>.
# Requires: gh (authenticated — `gh auth login`), cosign ≥ 2.x.
# Exit 0 = all checks green · 1 = verification failed · 2 = usage/tooling.
set -euo pipefail

usage() { sed -n '2,33p' "$0" | sed 's/^# \{0,1\}//'; }

REPO="os-santiago/ai-sdlc"
DIR=""
TAG=""
while [ $# -gt 0 ]; do
  case "$1" in
    --repo) REPO="$2"; shift 2 ;;
    --dir)  DIR="$2";  shift 2 ;;
    -h|--help) usage; exit 0 ;;
    -*) echo "unknown flag: $1" >&2; usage >&2; exit 2 ;;
    *)  [ -z "$TAG" ] || { echo "unexpected extra argument: $1" >&2; exit 2; }
        TAG="$1"; shift ;;
  esac
done
[ -n "$TAG" ] || { echo "usage: verify-release.sh <tag> [--repo O/R] [--dir DIR]" >&2; exit 2; }

for tool in gh cosign; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing required tool: $tool" >&2; exit 2; }
done

if [ -z "$DIR" ]; then
  DIR="$(mktemp -d)"
  trap 'rm -rf "$DIR"' EXIT
fi
mkdir -p "$DIR"

# The Fulcio certificate SAN for keyless sign-blob is the workflow-ref URL;
# releases are maintainer-dispatched, so the ref is the dispatch ref
# (refs/heads/main) — tag refs are allowed for future tag-triggered runs.
REPO_RE="$(printf '%s' "$REPO" | sed 's/[][.^$*\\]/\\&/g')"
IDENT_RE="^https://github\\.com/${REPO_RE}/\\.github/workflows/ai-sdlc-release\\.yml@refs/(heads/main|tags/v.+)\$"
ISSUER="https://token.actions.githubusercontent.com"

ART_NAME="ai-sdlc-${TAG}.tar.gz"

echo "[i] downloading ${ART_NAME} (+ provenance assets) from ${REPO}@${TAG}"
# One glob covers the tarball and every provenance sidecar that exists;
# signing-disabled releases simply ship fewer files.
gh release download "$TAG" -R "$REPO" -D "$DIR" -p "${ART_NAME}*" --clobber

ART="${DIR}/${ART_NAME}"
[ -s "$ART" ] || { echo "[X] artifact not downloaded: $ART_NAME" >&2; exit 1; }
sha256sum "$ART"

ok=1

# --- 1. cosign signature (classic .sig/.crt pair) -----------------------------
if [ -s "${ART}.sig" ] && [ -s "${ART}.crt" ]; then
  if cosign verify-blob \
      --signature "${ART}.sig" \
      --certificate "${ART}.crt" \
      --certificate-identity-regexp "$IDENT_RE" \
      --certificate-oidc-issuer "$ISSUER" \
      "$ART"; then
    echo "[✓] cosign signature valid (cert identity: ai-sdlc-release.yml, issuer: $ISSUER)"
  else
    echo "[X] cosign verify-blob FAILED" >&2; ok=0
  fi
else
  echo "[i] no .sig/.crt pair — release published with signing: none (or pre-#103)"
fi

# --- 1b. cosign signature (Sigstore bundle — carries Rekor inclusion proof) ---
if [ -s "${ART}.sigstore.json" ]; then
  if cosign verify-blob \
      --bundle "${ART}.sigstore.json" \
      --certificate-identity-regexp "$IDENT_RE" \
      --certificate-oidc-issuer "$ISSUER" \
      "$ART"; then
    echo "[✓] cosign bundle valid (Rekor inclusion verified)"
  else
    echo "[X] cosign bundle verification FAILED" >&2; ok=0
  fi
fi

# --- 2. SLSA build provenance (GitHub Artifact Attestations) ------------------
if gh attestation verify "$ART" -R "$REPO"; then
  echo "[✓] SLSA build provenance verified (gh attestation verify)"
else
  echo "[X] gh attestation verify FAILED" >&2; ok=0
fi

# --- 2b. offline path, when the exported bundle asset is present ---------------
BUNDLE="${ART}.attestation.jsonl"
if [ -s "$BUNDLE" ]; then
  TRUSTED_ROOT="${DIR}/trusted_root.jsonl"
  if gh attestation trusted-root > "$TRUSTED_ROOT" \
      && gh attestation verify "$ART" -R "$REPO" \
           --bundle "$BUNDLE" --custom-trusted-root "$TRUSTED_ROOT"; then
    echo "[✓] attestation bundle verifies offline (trusted-root path)"
  else
    echo "[X] offline attestation verification FAILED" >&2; ok=0
  fi
fi

if [ "$ok" -eq 1 ]; then
  echo "[✓] ${ART_NAME} @ ${TAG}: provenance checks passed"
else
  echo "[X] ${ART_NAME} @ ${TAG}: verification FAILED" >&2
  exit 1
fi
