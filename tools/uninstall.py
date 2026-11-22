#!/usr/bin/env python3
"""Remove only installer-owned runtime resources; retain data and recovery state.

The default action is a read-only plan. `--apply` deletes the planned owned
resources; durable data and recovery metadata are never removed unless
`--delete-data <pvc>` explicitly names an owned claim.
"""
import argparse
from contextlib import ExitStack
import json
import os
from pathlib import Path
import re
import subprocess
import sys
from types import SimpleNamespace

import install
import konflux_runtime_install
import openshift_install
import supply_chain_install

RELEASE = 'ai-sdlc'
NAMESPACE = install.NAMESPACE
PART_OF = 'app.kubernetes.io/part-of'
UID_ANNOTATION = 'ai-sdlc.io/cluster-uid'
NAME = re.compile(r'[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?')
TIMEOUT = re.compile(r'[1-9][0-9]*(s|m)')
MARKERS = {'kind': 'ownership.json', 'konflux': 'konflux-ownership.json',
           'openshift': openshift_install.MARKER}
RECOVERY_CONFIGMAPS = frozenset({'ai-sdlc-authority'})
# Recorded release resource kinds the uninstaller may delete. Secrets,
# PersistentVolumeClaims and any other recorded kind are always retained.
REMOVABLE = frozenset({'Deployment', 'StatefulSet', 'Job', 'CronJob', 'ConfigMap',
                       'Service', 'ServiceAccount', 'Role', 'RoleBinding',
                       'ClusterRole', 'ClusterRoleBinding', 'NetworkPolicy',
                       'ResourceQuota', 'Route', 'Namespace', 'RuntimeClass'})
CLUSTER_SCOPED = frozenset({'ClusterRole', 'ClusterRoleBinding', 'Namespace', 'RuntimeClass'})
PLURAL = {'Deployment': 'deployments', 'StatefulSet': 'statefulsets', 'Job': 'jobs',
          'CronJob': 'cronjobs', 'ConfigMap': 'configmaps', 'Secret': 'secrets',
          'ServiceAccount': 'serviceaccounts', 'Service': 'services',
          'Role': 'roles', 'RoleBinding': 'rolebindings',
          'NetworkPolicy': 'networkpolicies', 'ResourceQuota': 'resourcequotas',
          'Route': 'routes', 'PersistentVolumeClaim': 'persistentvolumeclaims',
          'Namespace': 'namespaces', 'ClusterRole': 'clusterroles',
          'ClusterRoleBinding': 'clusterrolebindings', 'RuntimeClass': 'runtimeclasses'}
REVERSE = {plural: kind for kind, plural in PLURAL.items()}
# Namespaced kinds inventoried to classify ownership and unrelated resources.
SCAN_KINDS = ('deployments', 'statefulsets', 'jobs', 'cronjobs', 'configmaps',
              'serviceaccounts', 'roles', 'rolebindings', 'networkpolicies',
              'services', 'resourcequotas', 'persistentvolumeclaims', 'secrets')
# Cluster-scoped kinds inventoried for ownership evidence. `namespaces` also
# resolves recorded auxiliary namespaces such as ai-sdlc-executors.
SCAN_CLUSTER_KINDS = ('namespaces', 'clusterroles', 'clusterrolebindings', 'runtimeclasses')


def read_json(run, kubectl, args):
    raw = run(kubectl + [str(a) for a in args], capture_output=True).stdout
    return json.loads(raw) if raw.strip() else None


def listed(run, kubectl, kind, namespace=None):
    args = ['get', kind, '-o', 'json', '--ignore-not-found']
    if namespace is not None:
        args += ['-n', namespace]
    items = (read_json(run, kubectl, args) or {}).get('items', [])
    if not isinstance(items, list):
        raise ValueError('Invalid ' + kind + ' inventory')
    return items


def release_manifest(run, helm, kubeconfig, kube_context):
    """Resources recorded by the ai-sdlc Helm release; empty when absent."""
    result = run([helm, 'get', 'manifest', RELEASE, '--namespace', NAMESPACE,
                  '--kubeconfig', str(kubeconfig), '--kube-context', kube_context],
                 capture_output=True, check=False)
    if result.returncode != 0:
        if 'not found' in result.stderr.lower():
            return []
        raise ValueError('Cannot read the recorded release manifest: ' + result.stderr.strip())
    resources = []
    for document in re.split(r'(?m)^---\s*$', result.stdout):
        kind = re.search(r'(?m)^kind:[ \t]*(\S+)', document)
        metadata = re.search(r'(?m)^metadata:\s*\n((?: +[^\n]*(?:\n|$))*)', document)
        if kind is None:
            continue
        if metadata is None:
            raise ValueError('Recorded release resource lacks metadata')
        block = metadata.group(1)
        name = re.search(r'(?m)^  name:[ \t]*(\S+)', block)
        namespace = re.search(r'(?m)^  namespace:[ \t]*(\S+)', block)
        if name is None:
            raise ValueError('Recorded release resource lacks a name')
        resources.append({'kind': kind.group(1).strip('"\''), 'name': name.group(1).strip('"\''),
                          'namespace': (namespace.group(1).strip('"\'') if namespace else '')})
    return resources


def frozen(run, kubectl):
    """The maintenance control plane: only a frozen authority has stopped intake."""
    config = read_json(run, kubectl, ['get', 'configmap', 'ai-sdlc-config', '-n', NAMESPACE,
                                      '--ignore-not-found', '-o', 'json'])
    data = (config or {}).get('data', {})
    if data.get('AI_SDLC_BUILD', 'false') != 'false':
        raise ValueError('Build-enabled installations require a containment-preserving teardown; uninstall is not supported')
    authority = read_json(run, kubectl, ['get', 'configmap', 'ai-sdlc-authority', '-n', NAMESPACE,
                                         '--ignore-not-found', '-o', 'json'])
    if config is None and authority is None:
        return True
    try:
        freeze = json.loads((authority or {}).get('data', {}).get('policy.json', '{}')).get('freeze')
    except (ValueError, AttributeError):
        raise ValueError('Unreadable installed authority policy') from None
    # A removed runtime leaves only the retained frozen authority behind; the
    # mode ConfigMap is gone on an already-applied uninstall.
    return freeze is True and (config is None or data.get('AI_SDLC_RUNTIME_MODE') == 'maintenance')


def namespace_empty(run, kubectl, name):
    """An auxiliary namespace may be deleted only when it holds none of the
    namespaced resources the owned inventory never scanned; deleting a
    namespace cascades over its whole contents."""
    items = []
    for kind in ('all', 'persistentvolumeclaims', 'secrets', 'configmaps'):
        items += listed(run, kubectl, kind, name)
    return not items


def owned_claims(statefulsets):
    """PVC names created by owned StatefulSets; claim volumes carry no labels."""
    claims = set()
    for statefulset in statefulsets:
        spec = statefulset.get('spec', {})
        name = statefulset.get('metadata', {}).get('name', '')
        replicas = spec.get('replicas', 1)
        if type(replicas) is not int or not 0 <= replicas <= 100:
            raise ValueError('Invalid StatefulSet replica count')
        for template in spec.get('volumeClaimTemplates') or []:
            claim = template.get('metadata', {}).get('name', '')
            for ordinal in range(replicas):
                pvc = claim + '-' + name + '-' + str(ordinal)
                if not NAME.fullmatch(pvc):
                    raise ValueError('Invalid derived persistent volume claim name')
                claims.add(pvc)
    return claims


def plan(run, kubectl, manifest, cluster_uid, namespaced_kinds, delete_data):
    """Read-only owned-resource plan; nothing is deleted here."""
    inventory, cluster_inventory = {}, {}
    for kind in namespaced_kinds:
        for item in listed(run, kubectl, kind, NAMESPACE):
            inventory[(kind, item.get('metadata', {}).get('name', ''))] = item
    for kind in SCAN_CLUSTER_KINDS:
        for item in listed(run, kubectl, kind):
            cluster_inventory[(kind, item.get('metadata', {}).get('name', ''))] = item
    # A foreign installation's UID-bound resource anywhere in scope is
    # ambiguous: refuse rather than guess which installation owns it.
    ambiguous = [kind + '/' + name for scope in (inventory, cluster_inventory)
                 for (kind, name), item in scope.items()
                 if item.get('metadata', {}).get('annotations', {}).get(UID_ANNOTATION, cluster_uid) != cluster_uid]
    if ambiguous:
        raise ValueError('Ambiguous ownership; resources record a different installation: ' + ', '.join(sorted(ambiguous)))
    remove, purge, retained, untouched, seen = [], [], [], [], set()
    bound = False
    for recorded in manifest:
        kind, name, namespace = recorded['kind'], recorded['name'], recorded['namespace']
        plural = PLURAL.get(kind)
        if plural is None:
            retained.append({'kind': kind, 'name': name,
                             'reason': 'recorded kind is not handled; left in place'})
            continue
        namespaced = kind not in CLUSTER_SCOPED
        namespace = namespace or (NAMESPACE if namespaced else '')
        if namespaced and namespace != NAMESPACE:
            raise ValueError('Recorded release resource is outside the owned namespace: ' + name)
        live = (inventory if namespaced else cluster_inventory).get((plural, name))
        if live is None:
            continue
        if live.get('metadata', {}).get('annotations', {}).get(UID_ANNOTATION) == cluster_uid:
            bound = True
        seen.add((plural, name, namespaced))
        if kind == 'Namespace' and name == NAMESPACE:
            retained.append({'kind': kind, 'name': name,
                             'reason': 'installation boundary retains data and recovery metadata'})
        elif kind == 'Secret':
            retained.append({'kind': kind, 'name': name, 'reason': 'credentials are never removed'})
        elif kind == 'ConfigMap' and name in RECOVERY_CONFIGMAPS:
            retained.append({'kind': kind, 'name': name, 'reason': 'recovery metadata'})
        elif kind == 'PersistentVolumeClaim':
            retained.append({'kind': kind, 'name': name, 'reason': 'durable data'})
        elif kind == 'Namespace':
            if namespace_empty(run, kubectl, name):
                remove.append({'kind': kind, 'name': name, 'resource': plural, 'namespaced': False,
                               'uid': live.get('metadata', {}).get('uid')})
            else:
                retained.append({'kind': kind, 'name': name,
                                 'reason': 'auxiliary namespace holds resources outside the owned inventory; drain it first'})
        elif kind in REMOVABLE:
            remove.append({'kind': kind, 'name': name, 'resource': plural, 'namespaced': namespaced,
                           'uid': live.get('metadata', {}).get('uid')})
        else:
            retained.append({'kind': kind, 'name': name, 'reason': 'durable or unsupported kind; left in place'})
    if manifest and not bound:
        raise ValueError('The recorded release cannot be bound to this installation; refusing to infer ownership')
    owned = {item['name'] for item in remove if item['resource'] == 'statefulsets'}
    claims = owned_claims([item for (kind, name), item in inventory.items()
                           if kind == 'statefulsets' and name in owned])
    for claim in sorted(delete_data):
        if claim not in claims:
            raise ValueError('--delete-data does not identify installer-owned retained storage: ' + claim)
    for (kind, name), item in inventory.items():
        if (kind, name, True) in seen:
            continue
        kind_name = REVERSE.get(kind, kind)
        annotation = item.get('metadata', {}).get('annotations', {}).get(UID_ANNOTATION)
        if kind == 'persistentvolumeclaims':
            if name in delete_data:
                purge.append({'kind': kind_name, 'name': name, 'resource': kind,
                              'namespaced': True, 'uid': item.get('metadata', {}).get('uid')})
            elif name in claims:
                retained.append({'kind': kind_name, 'name': name, 'reason': 'durable data'})
            else:
                untouched.append({'kind': kind_name, 'name': name})
            continue
        if kind == 'secrets':
            if annotation == cluster_uid or item.get('metadata', {}).get('labels', {}).get(PART_OF) == 'ai-sdlc':
                retained.append({'kind': kind_name, 'name': name,
                                 'reason': 'credentials and recovery metadata are never removed'})
            else:
                untouched.append({'kind': kind_name, 'name': name})
            continue
        if annotation == cluster_uid:
            remove.append({'kind': kind_name, 'name': name, 'resource': kind, 'namespaced': True,
                           'uid': item.get('metadata', {}).get('uid')})
        else:
            untouched.append({'kind': kind_name, 'name': name})
    for (kind, name), item in cluster_inventory.items():
        if (kind, name, False) in seen or kind == 'namespaces':
            continue
        kind_name = REVERSE.get(kind, kind)
        if item.get('metadata', {}).get('annotations', {}).get(UID_ANNOTATION) == cluster_uid:
            remove.append({'kind': kind_name, 'name': name, 'resource': kind, 'namespaced': False,
                           'uid': item.get('metadata', {}).get('uid')})
        else:
            untouched.append({'kind': kind_name, 'name': name})
    order = lambda item: (item['kind'], item['name'])
    return {'version': 'ai-sdlc.uninstall-plan/v1', 'namespace': NAMESPACE,
            'remove': sorted(remove, key=order), 'purge': sorted(purge, key=order),
            'retained': sorted(retained, key=order), 'untouched': sorted(untouched, key=order)}


def delete_entry(run, kubectl, planned, timeout):
    args = ['get', planned['resource'], planned['name'], '-o', 'json', '--ignore-not-found']
    if planned['namespaced']:
        args += ['-n', NAMESPACE]
    current = read_json(run, kubectl, args)
    if current is None:
        return False
    if planned.get('uid') is not None and current.get('metadata', {}).get('uid') != planned['uid']:
        raise ValueError('Resource changed during uninstall; rerun the plan before retrying: ' + planned['name'])
    args = ['delete', planned['resource'], planned['name'], '--ignore-not-found', '--timeout', timeout]
    if planned['namespaced']:
        args += ['-n', NAMESPACE]
    run(kubectl + [str(a) for a in args], stdout=subprocess.DEVNULL)
    return True


def connect(run, binaries, state, args):
    """Resolve kubectl plus a proven cluster UID, or refuse before any read."""
    marker = state / MARKERS[args.platform]
    for name in set(MARKERS.values()) - {MARKERS[args.platform]}:
        if (state / name).exists():
            raise ValueError('This --cluster state belongs to a different platform installation')
    if marker.is_symlink() or not marker.is_file():
        raise ValueError('No ' + args.platform + ' ownership record for --cluster ' + args.cluster)
    if args.platform == 'openshift':
        request = SimpleNamespace(cluster=args.cluster, kubeconfig=args.kubeconfig,
                                  context=args.context, command='uninstall')
        kubeconfig, kubectl, cluster_uid, _ = openshift_install.connect(run, binaries, state, request)
        return kubeconfig, args.context, kubectl, cluster_uid
    kubeconfig = state / 'kubeconfig'
    context = 'kind-' + args.cluster
    if not kubeconfig.is_file():
        raise ValueError('The recorded kubeconfig is absent; the cluster cannot be bound to this installation')
    kubectl = [binaries['kubectl'], '--kubeconfig', str(kubeconfig), '--context', context]
    cluster_uid = run(kubectl + ['get', 'namespace', 'kube-system',
                                 '-o', 'jsonpath={.metadata.uid}'], capture_output=True).stdout.strip()
    if args.platform == 'konflux':
        provider = supply_chain_install.select_provider(args.provider, marker, args.cluster)
        konflux_runtime_install.verify_cluster(run, kubectl, state, args.cluster, provider, cluster_uid)
    else:
        record = json.loads(marker.read_text())
        provider = record.get('provider') if isinstance(record, dict) else None
        if args.provider != 'auto' and provider != args.provider:
            raise ValueError('Requested provider differs from the recorded cluster ownership')
        install.check_ownership(marker, args.cluster, provider, cluster_uid)
    return kubeconfig, context, kubectl, cluster_uid


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cluster', default='ai-sdlc')
    parser.add_argument('--platform', choices=sorted(MARKERS), default='kind')
    parser.add_argument('--provider', choices=['auto', 'docker', 'podman'], default='auto')
    parser.add_argument('--kubeconfig', type=Path, help='Existing OpenShift cluster kubeconfig; --platform openshift only')
    parser.add_argument('--context', help='Context of --kubeconfig; --platform openshift only')
    parser.add_argument('--timeout', default='10m', help='Bound each deletion wait; not the total duration')
    parser.add_argument('--delete-data', action='append', default=[], metavar='PVC',
                        help='Purge the named installer-owned retained PersistentVolumeClaim; repeatable')
    parser.add_argument('--apply', action='store_true', help='Perform the planned deletion; without it the plan is read-only')
    args = parser.parse_args(argv)
    if not re.fullmatch(r'[a-z][a-z0-9-]{0,35}', args.cluster):
        raise ValueError('Invalid cluster name')
    if not TIMEOUT.fullmatch(args.timeout):
        raise ValueError('Timeout must be positive seconds or minutes')
    for claim in args.delete_data:
        if not NAME.fullmatch(claim):
            raise ValueError('Invalid persistent volume claim name: ' + claim)
    if args.platform != 'openshift' and (args.kubeconfig is not None or args.context is not None):
        raise ValueError('--kubeconfig and --context select an existing OpenShift cluster; use --platform openshift')
    if args.platform == 'openshift' and (args.kubeconfig is None or args.context is None):
        raise ValueError('OpenShift uninstall requires an explicit --kubeconfig and --context')
    state = Path(os.environ.get('XDG_STATE_HOME', Path.home() / '.local/state')) / 'ai-sdlc' / args.cluster
    if state.is_symlink():
        raise ValueError('Installation state directory cannot use symlinks')
    cache = Path(os.environ.get('XDG_CACHE_HOME', Path.home() / '.cache')) / 'ai-sdlc/tools'
    with ExitStack() as stack:
        if state.is_dir():
            konflux_runtime_install.lock_state(state, stack)
        binaries = install.tools(cache)
        kubeconfig, context, kubectl, cluster_uid = connect(install.run, binaries, state, args)
        intake_stopped = frozen(install.run, kubectl)
        manifest = release_manifest(install.run, binaries['helm'], kubeconfig, context)
        kinds = list(SCAN_KINDS) + (['routes'] if args.platform == 'openshift' else [])
        result = plan(install.run, kubectl, manifest, cluster_uid, kinds, set(args.delete_data))
        result.update(cluster=args.cluster, platform=args.platform,
                      clusterUID=cluster_uid, intakeStopped=intake_stopped)
        if args.apply:
            if not intake_stopped:
                raise ValueError('New work intake is not stopped; freeze the installation first with ./install.sh local --profile maintenance')
            removed = []
            for planned in result['remove'] + result['purge']:
                if delete_entry(install.run, kubectl, planned, args.timeout):
                    removed.append(planned['kind'] + '/' + planned['name'])
            result['deleted'] = removed
            journal = {'version': 'ai-sdlc.uninstall/v1', 'cluster': args.cluster,
                       'platform': args.platform, 'clusterUID': cluster_uid,
                       'namespace': NAMESPACE, 'deleted': removed,
                       'purgedData': [pvc['name'] for pvc in result['purge']],
                       'retained': [item['kind'] + '/' + item['name'] for item in result['retained']],
                       'namespaceRetained': True, 'clusterRetained': True,
                       'helmHistoryRetained': True}
            konflux_runtime_install.write_record(state / 'uninstall.json', journal)
            result['journal'] = 'uninstall.json'
        result['applied'] = bool(args.apply)
    print(json.dumps(result, indent=2))
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        print('Uninstall failed: ' + str(error), file=sys.stderr)
        sys.exit(1)
