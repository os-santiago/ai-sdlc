"""Target an existing OpenShift cluster instead of creating a kind cluster.

The installer never creates, deletes or reconfigures the cluster or its nodes.
It records the cluster's identity on first use and refuses a different
cluster later. The administrator provides, beforehand, the sandbox runtime as
a RuntimeClass (the platform's standard class, such as `kata` for OpenShift
sandboxed containers, selected with --runtime-class; `ai-sdlc-offline` by
default) and a default StorageClass. See docs/operations/openshift.md.
"""
import json
import math
import os
import re
from pathlib import Path

MARKER = 'openshift-ownership.json'
# Markers of the kind and Konflux installers, which own their own state.
OTHER_MARKERS = ('ownership.json', 'konflux-ownership.json')


def validate_options(args):
    runtime_class = getattr(args, 'runtime_class', None)
    if args.platform != 'openshift':
        if args.kubeconfig is not None or args.context is not None:
            raise ValueError('--kubeconfig and --context select an existing OpenShift cluster; use --platform openshift')
        # kind and Konflux install and use their own ai-sdlc-offline class.
        if runtime_class not in (None, 'ai-sdlc-offline'):
            raise ValueError('--runtime-class selects a platform sandbox RuntimeClass; use --platform openshift (kind installs ai-sdlc-offline)')
        return
    if args.kubeconfig is None or args.context is None:
        raise ValueError('OpenShift installation requires an explicit --kubeconfig and --context')
    if args.command not in ('plan', 'local', 'status', 'credentials'):
        raise ValueError('OpenShift installation supports plan, local, status and credentials')


def refuse_other_platform(state):
    """The kind and Konflux paths call this too: one state directory, one platform."""
    if (state / MARKER).exists():
        raise ValueError('This --cluster state belongs to an OpenShift installation; choose another --cluster')


def private_copy(source, target):
    """Write the kubeconfig as 0600 from the first byte, never following a link."""
    data = source.read_bytes()
    target.unlink(missing_ok=True)
    fd = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'wb') as output:
        output.write(data)


def connect(run, binaries, state, args):
    """kubectl bound to the given context, the cluster UID and whether this is the first use."""
    if any((state / name).exists() for name in OTHER_MARKERS):
        raise ValueError('This --cluster state belongs to a kind or Konflux installation; choose another --cluster')
    source = Path(args.kubeconfig).expanduser()
    if not source.is_file():
        raise ValueError('The OpenShift kubeconfig does not exist')
    state.chmod(0o700)
    kubeconfig = state / 'kubeconfig'
    private_copy(source, kubeconfig)
    kubectl = [binaries['kubectl'], '--kubeconfig', str(kubeconfig), '--context', args.context]
    cluster_uid = run(kubectl + ['get', 'namespace', 'kube-system', '-o', 'jsonpath={.metadata.uid}'], capture_output=True).stdout.strip()
    if not cluster_uid:
        raise ValueError('Cannot establish the OpenShift cluster identity')
    # Never record a cluster that is not OpenShift.
    if 'security.openshift.io/v1' not in run(kubectl + ['api-versions'], capture_output=True).stdout.split():
        raise ValueError('The selected cluster is not OpenShift (no security.openshift.io/v1 API)')
    marker = state / MARKER
    expected = {'cluster': args.cluster, 'platform': 'openshift', 'context': args.context, 'clusterUID': cluster_uid}
    if marker.exists():
        if json.loads(marker.read_text()) != expected:
            raise ValueError('This installation state belongs to a different OpenShift cluster or context')
        return kubeconfig, kubectl, cluster_uid, False
    if args.command != 'local':
        raise ValueError('No OpenShift installation is recorded for this cluster name')
    marker.write_text(json.dumps(expected) + '\n')
    return kubeconfig, kubectl, cluster_uid, True


def preflight(run, kubectl, sandbox, runtime_class='ai-sdlc-offline'):
    """Fail before any change when the cluster lacks what the runtime needs."""
    groups = run(kubectl + ['api-versions'], capture_output=True).stdout.split()
    if 'security.openshift.io/v1' not in groups:
        raise ValueError('The selected cluster is not OpenShift (no security.openshift.io/v1 API)')
    classes = json.loads(run(kubectl + ['get', 'storageclass', '-o', 'json'], capture_output=True).stdout).get('items', [])
    if not any(c.get('metadata', {}).get('annotations', {}).get('storageclass.kubernetes.io/is-default-class') == 'true' for c in classes):
        raise ValueError('OpenShift installation requires a default StorageClass')
    if sandbox:
        result = run(kubectl + ['get', 'runtimeclass', runtime_class, '-o', 'json'], capture_output=True, check=False)
        if result.returncode != 0:
            raise ValueError('Provide the sandbox runtime as RuntimeClass ' + runtime_class + ' before installing, or select the installed class with --runtime-class (docs/operations/openshift.md)')
        return json.loads(result.stdout).get('handler', '')
    return ''


_QUANTITY = re.compile(r'([0-9]+(?:\.[0-9]+)?)(m|Ki|Mi|Gi|Ti|k|M|G|T)?')
_FACTOR = {None: 1, 'm': 0.001, 'k': 10**3, 'M': 10**6, 'G': 10**9, 'T': 10**12,
           'Ki': 2**10, 'Mi': 2**20, 'Gi': 2**30, 'Ti': 2**40}


def pod_overhead(run, kubectl, runtime_class):
    """The class's overhead.podFixed as chart quota headroom, rounded up to
    whole millicores and Mi; empty when the class declares no overhead."""
    result = run(kubectl + ['get', 'runtimeclass', runtime_class, '-o', 'json'], capture_output=True)
    fixed = ((json.loads(result.stdout).get('overhead') or {}).get('podFixed')) or {}
    if not isinstance(fixed, dict) or set(fixed) - {'cpu', 'memory'}:
        raise ValueError('RuntimeClass ' + runtime_class + ' declares an unsupported pod overhead; only cpu and memory are supported')
    headroom = {}
    for name, value in fixed.items():
        match = _QUANTITY.fullmatch(str(value))
        if not match:
            raise ValueError('RuntimeClass ' + runtime_class + ' declares an unsupported ' + name + ' overhead quantity')
        amount = float(match.group(1)) * _FACTOR[match.group(2)]
        if name == 'cpu':
            headroom['cpu'] = str(math.ceil(round(amount * 1000, 6))) + 'm'
        else:
            headroom['memory'] = str(math.ceil(round(amount / 2**20, 6))) + 'Mi'
    if any(int(v[:-1] if v.endswith('m') else v[:-2]) > 999999 for v in headroom.values()):
        raise ValueError('RuntimeClass ' + runtime_class + ' declares a pod overhead above the supported bound')
    return {k: v for k, v in headroom.items() if v not in ('0m', '0Mi')}
