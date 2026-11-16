"""Exercise frozen review installation only in a dedicated disposable kind cluster.

Fixture images are locally built/imported, never represented as published release
artifacts. No workflow rows, claims or candidates are created by this procedure.
"""
import argparse
import json, pathlib, sys, tempfile, subprocess, re
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))
import install, candidate_install, offline_runtime

def require(condition, message):
    if not condition:
        raise ValueError(message)

def require_review_disabled(spec):
    """Inspect both the desired template and every current verifier Pod."""
    require([c['name'] for c in spec.get('containers', [])] == ['verifier'], 'Review sidecar remains')
    names = {'review', 'review-model-client', 'model-auth', 'model-home'}
    paths = ('/etc/ai-sdlc/review', '/var/run/ai-sdlc/review-model', '/var/run/ai-sdlc/codex', '/model-home')
    secrets = {'ai-sdlc-review-model-client', 'ai-sdlc-codex-auth'}
    for volume in spec.get('volumes', []):
        require(volume.get('name') not in names, 'Review volume remains')
    for container in spec.get('containers', []) + spec.get('initContainers', []) + spec.get('ephemeralContainers', []):
        for mount in container.get('volumeMounts', []):
            path = mount.get('mountPath', '').rstrip('/')
            require(mount.get('name') not in names and not any(path == p or path.startswith(p + '/') for p in paths), 'Review mount remains')
    def inspect(value):
        if isinstance(value, dict):
            for key, child in value.items():
                if key in ('secretName', 'name') and isinstance(child, str):
                    require(child not in secrets, 'Review credential reference remains')
                inspect(child)
        elif isinstance(value, list):
            for child in value:
                inspect(child)
    inspect(spec)

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--images', type=pathlib.Path, required=True, help='Imported immutable fixture references')
    parser.add_argument('--lock', type=pathlib.Path, default=install.LOCAL / 'images.lock.json')
    parser.add_argument('--github-auth', choices=['none', 'gh-cli'], default='none')
    parser.add_argument('--codex-auth-file', type=pathlib.Path)
    parser.add_argument('--report', type=pathlib.Path, required=True)
    args = parser.parse_args()
    cluster = 'ai-sdlc-review-install-test'
    provider = 'podman'
    repository = 'os-santiago/ai-sdlc'
    state = pathlib.Path.home() / '.local/state/ai-sdlc' / cluster
    kubeconfig = state / 'kubeconfig'
    cache = pathlib.Path.home() / '.cache/ai-sdlc/tools'
    binaries = install.tools(cache)
    k = [str(binaries['kubectl']), '--kubeconfig', str(kubeconfig)]

    def run(args, value=None):
        r = subprocess.run([str(x) for x in args], input=json.dumps(value) if value is not None else None, text=True, capture_output=True, timeout=650)
        if r.returncode:
            raise ValueError('isolated installation command failed: ' + r.stderr[-1500:])
        return r.stdout
    nodes = json.loads(run(k + ['get', 'nodes', '-o', 'json']))['items']
    require([node['metadata']['name'] for node in nodes] == [cluster + '-control-plane'], 'Dedicated disposable cluster required')
    uid = json.loads(run(k + ['get', 'namespace', 'kube-system', '-o', 'json']))['metadata']['uid']
    lockpath = args.lock.resolve()
    lock = install.image_lock(lockpath)
    images = json.loads(args.images.read_text())
    if set(images) != {'worker', 'model-mailbox', 'verifier', 'migrator'} or any((not re.fullmatch('quay.io/os-santiago/ai-sdlc@sha256:[a-f0-9]{64}', v) for v in images.values())):
        raise ValueError('Expected exactly four immutable imported fixture images')
    # Bootstrap from the same migrator that will reconcile database roles.
    lock['migrator']['digest'] = images['migrator'].split('@')[1]
    public = install.configure_identities(k, provider, lock, uid, repository, 'ai-sdlc-production', freeze=True, candidate=True, verification=True)
    if args.github_auth == 'gh-cli':
        install.configure_github_client(k, uid, repository)
    public['intake'] = install.shadow_intake_config(k, uid, repository)
    intake = json.loads(public['intake']['configJSON'])
    intake['triggerLabel'] = 'ai-sdlc:review-install-fixture'
    public['intake']['configJSON'] = json.dumps(intake)
    profile = json.loads((install.LOCAL / 'profiles/verification.json').read_text())
    public['contract'] = {'inline': profile['contract']['inline'].replace('ai-sdlc:shadow', 'ai-sdlc:review-install-fixture')}
    values = candidate_install.desired_values(install.ROOT, lockpath, lock, verification=True, review=True)
    values['candidate']['credentialRevision'] = candidate_install.model_credentials(k, uid, args.codex_auth_file)
    candidate_install.review_credentials(k, uid)
    offline_runtime.install(k, provider, binaries['kind'], cluster, uid, cache / 'offline-runtime', images['worker'])
    values['candidate']['apiServerCIDRs'] = candidate_install.api_cidrs(k)
    c = json.loads(values['candidate']['configJSON'])
    c['placement']['WorkerImage'] = images['worker']
    c['placement']['MailboxImage'] = images['model-mailbox']
    values['candidate']['configJSON'] = json.dumps(c)
    c = json.loads(values['review']['configJSON'])
    c['workerImage'] = images['worker']
    c['mailboxImage'] = images['model-mailbox']
    values['review']['configJSON'] = json.dumps(c)
    values['verifier']['digest'] = images['verifier'].split('@')[1]
    values['components'] = {'worker': {'digest': images['worker'].split('@')[1]}}
    values['migrator'] = {'digest': images['migrator'].split('@')[1]}
    public.update(values)

    def apply(enabled):
        public['review']['enabled'] = enabled
        with tempfile.NamedTemporaryFile(mode='w', suffix='.json') as f:
            json.dump(public, f)
            f.flush()
            run([binaries['helm'], 'upgrade', '--install', 'ai-sdlc', install.CHART, '--kubeconfig', kubeconfig, '--namespace', 'ai-sdlc-runtime', '-f', install.LOCAL / 'values.yaml', '-f', install.LOCAL / 'profiles/verification.json', '-f', lockpath, '-f', f.name, '--wait=legacy', '--wait-for-jobs', '--timeout', '8m'])
        print('review enabled=' + str(enabled) + ' applied', flush=True)

    def db(query):
        return run(k + ['-n', 'ai-sdlc-runtime', 'exec', 'statefulset/ai-sdlc-postgres', '--', 'psql', '-U', 'ai_sdlc', '-d', 'ai_sdlc', '-Atc', query]).strip()

    def item(kind, name, namespace):
        return json.loads(run(k + ['get', kind, name, '-n', namespace, '-o', 'json']))
    apply(True)
    require(db("SELECT has_table_privilege('aisdlc_verifier','review_runs','SELECT')") == 't', 'Independent review installation invariant failed')
    require(db('SELECT count(*) FROM execution_requests') == '0', 'Independent review installation invariant failed')
    marker = {'apiVersion': 'v1', 'kind': 'ConfigMap', 'metadata': {'name': 'fixture-retention-marker', 'namespace': 'ai-sdlc-reviewers'}, 'data': {'purpose': 'test-owned non-workflow retention marker'}}
    run(k + ['create', '-f', '-'], marker)
    markerUID = item('configmap', 'fixture-retention-marker', 'ai-sdlc-reviewers')['metadata']['uid']
    namespaceUID = item('namespace', 'ai-sdlc-reviewers', 'ai-sdlc-reviewers')['metadata']['uid']
    apply(True)
    require(item('configmap', 'fixture-retention-marker', 'ai-sdlc-reviewers')['metadata']['uid'] == markerUID, 'Independent review installation invariant failed')
    apply(False)
    require(db("SELECT has_table_privilege('aisdlc_verifier','review_runs','SELECT')") == 'f', 'Independent review installation invariant failed')
    require(item('namespace', 'ai-sdlc-reviewers', 'ai-sdlc-reviewers')['metadata']['uid'] == namespaceUID, 'Independent review installation invariant failed')
    require(item('configmap', 'fixture-retention-marker', 'ai-sdlc-reviewers')['metadata']['uid'] == markerUID, 'Independent review installation invariant failed')
    policy = item('networkpolicy', 'deny-all', 'ai-sdlc-reviewers')['spec']
    require(policy['podSelector'] == {} and set(policy['policyTypes']) == {'Ingress', 'Egress'} and (not policy.get('egress')) and (not policy.get('ingress')), 'Independent review installation invariant failed')
    require(item('resourcequota', 'reviewer-bounds', 'ai-sdlc-reviewers')['spec']['hard']['pods'] == '2', 'Independent review installation invariant failed')
    require(run(k + ['get', 'rolebinding', 'review-controller', '-n', 'ai-sdlc-reviewers', '--ignore-not-found', '-o', 'name']).strip() == '', 'Independent review installation invariant failed')
    dep = item('deployment', 'ai-sdlc-verifier', 'ai-sdlc-runtime')
    require_review_disabled(dep['spec']['template']['spec'])
    selector = ','.join(key + '=' + value for key, value in sorted(dep['spec']['selector']['matchLabels'].items()))
    pods = json.loads(run(k + ['get', 'pods', '-n', 'ai-sdlc-runtime', '-l', selector, '-o', 'json']))['items']
    require(len(pods) == dep['spec']['replicas'] and all(p.get('status', {}).get('phase') == 'Running' for p in pods), 'Disabled verifier rollout incomplete')
    for pod in pods:
        require_review_disabled(pod['spec'])
    apply(True)
    require(item('namespace', 'ai-sdlc-reviewers', 'ai-sdlc-reviewers')['metadata']['uid'] == namespaceUID, 'Independent review installation invariant failed')
    require(item('configmap', 'fixture-retention-marker', 'ai-sdlc-reviewers')['metadata']['uid'] == markerUID, 'Independent review installation invariant failed')
    require(db("SELECT has_table_privilege('aisdlc_verifier','review_runs','SELECT')") == 't', 'Independent review installation invariant failed')
    require(db('SELECT count(*) FROM execution_requests') == '0', 'Independent review installation invariant failed')
    run(k + ['delete', '--raw', '/api/v1/namespaces/ai-sdlc-reviewers/configmaps/fixture-retention-marker', '-f', '-'], {'apiVersion': 'v1', 'kind': 'DeleteOptions', 'preconditions': {'uid': markerUID}})
    report = {'scope': 'isolated-review-installation-fixture', 'autonomousE2E': False, 'workflowRequests': 0, 'authorityFrozen': True, 'realKubernetes': True, 'realPostgres': True, 'locallyBuiltFixtureImages': True, 'publishedImages': False, 'enabledReady': True, 'reapplyReady': True, 'disabledPrivilegesRevoked': True, 'disabledModelMountsRemoved': True, 'disabledRBACRemoved': True, 'uncertainResourceBoundaryPreserved': True, 'retainedNamespaceUID': namespaceUID, 'retainedMarkerUID': markerUID, 'denyAllAndQuotaRetained': True, 'reenabledReady': True, 'homedirModified': False, 'ownedMarkerRemovedAfterValidation': True}
    args.report.write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report), flush=True)
if __name__ == '__main__':
    main()
