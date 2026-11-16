"""Reviewed isolated Saferus Go build; no operational workflow authority."""
import copy
import hashlib
import json
from pathlib import Path

import konflux_build_fixture as shared

INPUTS = json.loads((Path(__file__).resolve().parents[1] / 'runtime/k8s/deploy/konflux/saferus/inputs.json').read_text())
SECURITY = shared.SECURITY
PUBLIC_EGRESS = shared.PUBLIC_EGRESS
REGISTRY_EGRESS = shared.REGISTRY_EGRESS
pipeline_run = shared.pipeline_run


def policies(namespace):
    result = shared.policies(namespace)
    # A scratch publisher needs only DNS and the exact in-cluster registry.
    result[1]['spec']['egress'] = [copy.deepcopy(PUBLIC_EGRESS[0]), copy.deepcopy(REGISTRY_EGRESS)]
    return result


def build_pipeline(targets=None):
    script = '''#!/bin/bash
set -euo pipefail
umask 022
NETWORK_GATE
mkdir -p /work/cache /work/modcache /work/tmp /work/output
export GOCACHE=/work/cache GOMODCACHE=/work/modcache TMPDIR=/work/tmp
export GOTOOLCHAIN=local GOWORK=off GOFLAGS=-mod=readonly
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0
test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token
test ! -e /registry-auth
test "$(go env GOVERSION)" = EXPECTED_GO_VERSION
git -c core.hooksPath=/dev/null init /work/source
cd /work/source
git -c core.hooksPath=/dev/null -c protocol.file.allow=never fetch --no-tags --depth=1 SOURCE_URL SOURCE_SHA
git -c core.hooksPath=/dev/null checkout --detach FETCH_HEAD
test "$(git rev-parse HEAD)" = SOURCE_SHA
go vet ./...
go test -race ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /work/output/saferus ./cmd/console
chmod 0755 /work/output/saferus
'''.replace('NETWORK_GATE', shared.network_gate_script(targets)).replace('EXPECTED_GO_VERSION', INPUTS['toolchain']).replace('SOURCE_URL', INPUTS['sourceURL']).replace('SOURCE_SHA', INPUTS['sourceSHA'])
    return {'tasks': [{'name':'go', 'taskSpec': {'volumes': [
        {'name':'artifacts','persistentVolumeClaim':{'claimName':'artifacts'}},
        {'name':'tmp','emptyDir':{'medium':'Memory','sizeLimit':'64Mi'}}],
        'steps':[{'name':'verify','image':INPUTS['builderImage'],'script':script,
                  'securityContext':copy.deepcopy(SECURITY),
                  'computeResources':{'requests':{'cpu':'500m','memory':'256Mi'},'limits':{'cpu':INPUTS['buildCPU'],'memory':INPUTS['buildMemory']}},
                  'volumeMounts':[{'name':'artifacts','mountPath':'/work'},{'name':'tmp','mountPath':'/tmp'}]}]}}]}


def publish_pipeline(image, definition, output):
    value = shared.publish_pipeline(image, definition, output)
    step = value['tasks'][0]['taskSpec']['steps'][0]
    step['args'] = ['--recipe='+INPUTS['recipe'], '--input=/artifacts/output', '--output='+output,
                    '--source-sha='+INPUTS['sourceSHA'], '--definition-digest='+definition,
                    '--run-uid=$(params.run-uid)', '--timeout=8m']
    return value


def definition_digest(image, targets=None):
    definition = {'inputs':INPUTS,'build':build_pipeline(targets),'publish':publish_pipeline(image,'DEFINITION','OUTPUT')}
    return 'sha256:'+hashlib.sha256(json.dumps(definition,sort_keys=True,separators=(',',':')).encode()).hexdigest()
