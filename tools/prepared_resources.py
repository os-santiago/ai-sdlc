"""Compare current installation resources with persisted reviewed Helm renders."""
import copy

from bootstrap_prepare import normalized_template, Preparation, digest

KINDS={'ConfigMap','Deployment','StatefulSet','Service','ServiceAccount','Namespace',
    'Role','ClusterRole','RoleBinding','ClusterRoleBinding','NetworkPolicy','ResourceQuota','LimitRange','RuntimeClass'}


def key(obj):
    return '/'.join((obj['apiVersion'],obj['kind'],obj['metadata'].get('namespace',''),obj['metadata']['name']))


def content(obj):
    value=copy.deepcopy(obj)
    meta=value.pop('metadata',{})
    value.pop('status',None)
    kind=value['kind'];spec=value.get('spec',{})
    if kind in ('Deployment','StatefulSet'):
        spec['template']=normalized_template(spec['template'])
        if spec.get('revisionHistoryLimit')==10:spec.pop('revisionHistoryLimit')
    if kind=='Deployment':
        if spec.get('progressDeadlineSeconds')==600:spec.pop('progressDeadlineSeconds')
        if spec.get('strategy')=={'type':'RollingUpdate','rollingUpdate':{'maxSurge':'25%','maxUnavailable':'25%'}}:spec.pop('strategy')
    if kind=='StatefulSet':
        for name,default in {'podManagementPolicy':'OrderedReady','updateStrategy':{'type':'RollingUpdate','rollingUpdate':{'partition':0}},
                'persistentVolumeClaimRetentionPolicy':{'whenDeleted':'Retain','whenScaled':'Retain'},'ordinals':{'start':0}}.items():
            if spec.get(name)==default:spec.pop(name)
        for claim in spec.get('volumeClaimTemplates',[]):
            claim.pop('status',None)
            if claim.get('apiVersion')=='v1':claim.pop('apiVersion')
            if claim.get('kind')=='PersistentVolumeClaim':claim.pop('kind')
            if claim['metadata'].get('creationTimestamp') is None:claim['metadata'].pop('creationTimestamp',None)
            if claim['spec'].get('volumeMode')=='Filesystem':claim['spec'].pop('volumeMode')
    if kind=='Service':
        for name in ('clusterIP','clusterIPs','ipFamilies','ipFamilyPolicy'):
            spec.pop(name,None) # assigned endpoint identity checked separately by preparation
        for name,default in {'type':'ClusterIP','sessionAffinity':'None','internalTrafficPolicy':'Cluster'}.items():
            if spec.get(name)==default:spec.pop(name)
        for port in spec.get('ports',[]):
            if port.get('protocol')=='TCP':port.pop('protocol')
    if kind=='NetworkPolicy':
        for name in ('ingress','egress'):
            if spec.get(name)==[]:spec.pop(name)
    if kind=='Namespace':value.pop('spec',None)
    if kind=='ConfigMap' and value.get('data')=={}:value.pop('data')
    labels=meta.get('labels',{})
    if kind=='Namespace' and labels.get('kubernetes.io/metadata.name')==meta.get('name'):
        labels.pop('kubernetes.io/metadata.name')
    if labels.get('app.kubernetes.io/managed-by')=='Helm':labels.pop('app.kubernetes.io/managed-by')
    value['labels']=labels
    return value


def documents(rendered):
    found={}
    for obj in rendered:
        if obj['kind']=='Job':continue # completed bounded migration, checked by Preparation
        if obj['kind'] not in KINDS:raise ValueError('Unsupported prepared Helm resource kind')
        identity=key(obj)
        if identity in found:raise ValueError('Duplicate prepared Helm resource')
        found[identity]=obj
    return found


def observe(configuration,description,expected=None):
    desired_objects=documents(description['documents'])
    if hasattr(configuration,'inventory'):
        scoped={'ConfigMap','Service','ServiceAccount','Role','RoleBinding','NetworkPolicy','Deployment','StatefulSet','ResourceQuota','LimitRange'}
        expected_inventory={(obj['kind'],obj['metadata']['name']) for obj in desired_objects.values()
            if obj['kind'] in scoped and obj['metadata'].get('namespace')=='ai-sdlc-runtime'}
        ignored={('ConfigMap','kube-root-ca.crt'),('ConfigMap','trusted-ca'),('ServiceAccount','default')}
        inventory=configuration.inventory(','.join(sorted(scoped)))
        actual={(obj['kind'],obj['metadata']['name']) for obj in inventory}
        external={key(obj):{'uid':Preparation.identity(obj),'sha256':digest(content(obj))}
            for obj in inventory if (obj['kind'],obj['metadata']['name']) in ignored}
        if 'externalRuntime' not in description or description['externalRuntime']!=external:
            raise ValueError('Original platform runtime resources changed')
        if actual-ignored!=expected_inventory:
            raise ValueError('Unexpected or partial prepared runtime security inventory')
    observed={}
    for identity,desired in documents(description['documents']).items():
        current=configuration.read(desired['kind'],desired['metadata']['name'],desired['metadata'].get('namespace'))
        if current is None:raise ValueError('Prepared Helm resource is missing; partial state is excluded')
        uid=Preparation.identity(current)
        meta=current['metadata'];annotations=meta.get('annotations',{})
        if (meta.get('labels',{}).get('app.kubernetes.io/managed-by')!='Helm' or meta.get('ownerReferences') or annotations.get('meta.helm.sh/release-name')!='ai-sdlc'
                or annotations.get('meta.helm.sh/release-namespace')!='ai-sdlc-runtime'
                or any(annotations.get(k)!=v for k,v in desired['metadata'].get('annotations',{}).items())
                or content(current)!=content(desired)
                or expected is not None and expected.get(identity) is not None and expected[identity]!=uid):
            raise ValueError('Prepared Helm resource authority, ownership or original identity changed')
        observed[identity]=uid
    if expected is not None and set(expected)!=set(observed):
        raise ValueError('Prepared Helm resource inventory changed')
    return observed


def platform_resources(configuration):
    allowed={('ConfigMap','kube-root-ca.crt'),('ConfigMap','trusted-ca'),('ServiceAccount','default')}
    return {key(obj):{'uid':Preparation.identity(obj),'sha256':digest(content(obj))}
        for obj in configuration.inventory('ConfigMap,ServiceAccount')
        if (obj['kind'],obj['metadata']['name']) in allowed}
