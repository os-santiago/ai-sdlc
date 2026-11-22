"""Target an existing OpenShift cluster instead of creating a kind cluster.

The installer never creates, deletes or reconfigures the cluster or its nodes.
It records the cluster's identity on first use and refuses a different
cluster later. The administrator provides, beforehand, the namespaces' sandbox
runtime as the `ai-sdlc-offline` RuntimeClass and a default StorageClass. See
docs/operations/openshift.md.
"""
import json
import shutil
from pathlib import Path

MARKER = 'openshift-ownership.json'


def validate_options(args):
    if args.platform != 'openshift':
        if args.kubeconfig is not None or args.context is not None:
            raise ValueError('--kubeconfig and --context select an existing OpenShift cluster; use --platform openshift')
        return
    if args.kubeconfig is None or args.context is None:
        raise ValueError('OpenShift installation requires an explicit --kubeconfig and --context')
    if args.command not in ('plan', 'local', 'status', 'credentials'):
        raise ValueError('OpenShift installation supports plan, local, status and credentials')


def connect(run, binaries, state, args):
    """kubectl bound to the given context, the cluster UID and whether this is the first use."""
    source = Path(args.kubeconfig).expanduser()
    if not source.is_file():
        raise ValueError('The OpenShift kubeconfig does not exist')
    kubeconfig = state / 'kubeconfig'
    shutil.copyfile(source, kubeconfig)
    kubeconfig.chmod(0o600)
    kubectl = [binaries['kubectl'], '--kubeconfig', str(kubeconfig), '--context', args.context]
    cluster_uid = run(kubectl + ['get', 'namespace', 'kube-system', '-o', 'jsonpath={.metadata.uid}'], capture_output=True).stdout.strip()
    if not cluster_uid:
        raise ValueError('Cannot establish the OpenShift cluster identity')
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


def preflight(run, kubectl, sandbox):
    """Fail before any change when the cluster lacks what the runtime needs."""
    groups = run(kubectl + ['api-versions'], capture_output=True).stdout.split()
    if 'security.openshift.io/v1' not in groups:
        raise ValueError('The selected cluster is not OpenShift (no security.openshift.io/v1 API)')
    classes = json.loads(run(kubectl + ['get', 'storageclass', '-o', 'json'], capture_output=True).stdout).get('items', [])
    if not any(c.get('metadata', {}).get('annotations', {}).get('storageclass.kubernetes.io/is-default-class') == 'true' for c in classes):
        raise ValueError('OpenShift installation requires a default StorageClass')
    if sandbox:
        result = run(kubectl + ['get', 'runtimeclass', 'ai-sdlc-offline', '-o', 'json'], capture_output=True, check=False)
        if result.returncode != 0:
            raise ValueError('Provide the sandbox runtime as RuntimeClass ai-sdlc-offline before installing (docs/operations/openshift.md)')
        return json.loads(result.stdout).get('handler', '')
    return ''
