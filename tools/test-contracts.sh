#!/usr/bin/env bash
# Run from the repository root. Requires tools/quality-requirements.txt.
set -euo pipefail
python3 - <<'PY'
import json
from jsonschema import Draft202012Validator
with open("spec/component-authority.schema.json", encoding="utf-8") as source:
    schema = json.load(source)
Draft202012Validator.check_schema(schema)
with open("spec/fixtures/component-authority.v1.json", encoding="utf-8") as source:
    Draft202012Validator(schema).validate(json.load(source))
with open("spec/local-install.schema.json", encoding="utf-8") as source:
    schema = json.load(source)
Draft202012Validator.check_schema(schema)
with open("examples/homedir.local-install.json", encoding="utf-8") as source:
    Draft202012Validator(schema).validate(json.load(source))
with open("spec/flow-event.schema.json", encoding="utf-8") as source:
    schema = json.load(source)
Draft202012Validator.check_schema(schema)
validator = Draft202012Validator(schema)
with open("runtime/k8s/components/flow-console/fixtures/evaluation-batteries-29-30.jsonl", encoding="utf-8") as source:
    events = [json.loads(line) for line in source if line.strip() and not line.startswith("#")]
assert events
for event in events:
    validator.validate(event)
for invalid in ({**events[0], "prompt": "x"}, {**events[0], "kind": "succeeded"}, {**events[0], "attributes": {"token": "x"}}):
    assert not validator.is_valid(invalid)
PY
for contract in examples/*.ai-sdlc.yaml; do
  bash tools/validate-ai-sdlc-config.sh "$contract"
done
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT
printf 'version: invalid\n' > "$tmpdir/invalid.yaml"
if bash tools/validate-ai-sdlc-config.sh "$tmpdir/invalid.yaml"; then
  echo 'Invalid schema unexpectedly accepted' >&2
  exit 1
fi
printf 'token: synthetic-credential-fixture\n' > "$tmpdir/secret.yaml"
if bash tools/validate-ai-sdlc-config.sh "$tmpdir/secret.yaml"; then
  echo 'Inline credential unexpectedly accepted' >&2
  exit 1
fi
