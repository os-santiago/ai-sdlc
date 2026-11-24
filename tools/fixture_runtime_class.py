"""Grant disposable fixture controllers read access to exactly one RuntimeClass.

The chart grants the execution and verification controllers only `get` on the
configured sandbox RuntimeClass, by name (templates/sandbox.yaml). The kind
fixtures give their controller identities the same single-name permission, so
the controllers' real preflight reads the class the kind installer created
(`./install.sh sandbox`). The cluster-scoped objects carry a per-run nonce
label and are removed by that label, never by name alone.
"""
import secrets

RUNTIME_CLASS = 'ai-sdlc-offline'
LABEL = 'ai-sdlc.io/runtime-class-fixture'


def new_nonce():
    return secrets.token_hex(8)


def objects(nonce, subjects, runtime_class=RUNTIME_CLASS):
    """The ClusterRole and ClusterRoleBinding for (namespace, service account) subjects."""
    if not subjects:
        raise ValueError('RuntimeClass fixture grant needs a subject')
    name = 'ai-sdlc-fixture-runtime-class-' + nonce
    metadata = {'name': name, 'labels': {LABEL: nonce}}
    return {'apiVersion': 'v1', 'kind': 'List', 'items': [
        {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'ClusterRole', 'metadata': metadata,
         'rules': [{'apiGroups': ['node.k8s.io'], 'resources': ['runtimeclasses'],
                    'resourceNames': [runtime_class], 'verbs': ['get']}]},
        {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'ClusterRoleBinding', 'metadata': metadata,
         'roleRef': {'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'ClusterRole', 'name': name},
         'subjects': [{'kind': 'ServiceAccount', 'name': account, 'namespace': namespace}
                      for namespace, account in subjects]}]}


def grant(call, nonce, subjects, runtime_class=RUNTIME_CLASS):
    call(['create', '-f', '-'], objects(nonce, subjects, runtime_class))


def revoke(call, nonce):
    """Remove only this run's grant; safe when nothing was created."""
    call(['delete', 'clusterrolebinding,clusterrole', '-l', LABEL + '=' + nonce,
          '--ignore-not-found', '--wait=true'])
