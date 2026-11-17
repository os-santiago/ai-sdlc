"""Closed initial phase transitions and rendered effects; not workflow evidence."""
import copy
import base64
import json
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import Mock,patch
import bootstrap_prepare
import initial_activation as activation
import test_release_helm
import test_bootstrap_prepare
import install


def source_values():
    v=test_release_helm.ReleaseChartTests().values();v.setdefault('migrator',{})['enabled']=True
    v['runtime']['paused']=True
    for section,key in (('authority','policyJSON'),('build','outerPolicyJSON')):
        p=json.loads(v[section][key]);p['freeze']=True;v[section][key]=json.dumps(p)
    cfg=json.loads(v['build']['configJSON']);cfg.update(inner=json.loads(v['authority']['policyJSON']),outer=json.loads(v['build']['outerPolicyJSON']))
    cfg.update(slots=[{'fixture':'original'}],issueMaxAgeSeconds=60)
    v.setdefault('intake',{})['configJSON']='{}'
    v['build']['configJSON']=json.dumps(cfg)
    return v


def compiler(source):
    def compile_value(request):return dict(json.loads(source['build']['configJSON']),inner=request['inner'],outer=request['outer'])
    return compile_value


class ActivationTests(unittest.TestCase):
    def test_live_observer_rejects_extra_intake_pods_template_drift_and_unready_armed(self):
        fixture=test_bootstrap_prepare.PreparationTests();fixture.setUp();self.addCleanup(fixture.doCleanups)
        source=source_values()
        for phase in ('warming','armed','active'):
            docs=test_release_helm.ReleaseChartTests().render(activation.phase_values(source,phase,compiler(source)))
            fixture.lists.update(deployments=[],replicasets=[],pods=[fixture.dbpod])
            for obj in docs:
                name=obj['metadata']['name']
                if obj['kind']=='ConfigMap':
                    actual=test_bootstrap_prepare.resource('ConfigMap',name,data=obj.get('data',{}))
                    actual['metadata']['annotations']={'ai-sdlc.io/cluster-uid':fixture.cluster_uid}
                    fixture.objects[('configmap',name)]=actual
                if obj['kind']!='Deployment':continue
                actual=test_bootstrap_prepare.resource('Deployment',name,spec=copy.deepcopy(obj['spec']),status={'observedGeneration':1})
                fixture.lists['deployments'].append(actual)
                if not obj['spec']['replicas']:continue
                template=copy.deepcopy(obj['spec']['template'])
                rs=test_bootstrap_prepare.resource('ReplicaSet','rs-'+name,spec={'replicas':1,'template':template})
                rs['metadata']['ownerReferences']=[{'uid':actual['metadata']['uid'],'name':name,'controller':True}]
                pod=test_bootstrap_prepare.resource('Pod','pod-'+name,spec=dict(copy.deepcopy(template['spec']),nodeName='fixture-control-plane'),status={'phase':'Running','containerStatuses':[
                    {'name':c['name'],'state':{'running':{}},'ready':phase!='warming'} for c in template['spec']['containers']]})
                pod['metadata'].update(copy.deepcopy(template.get('metadata',{})))
                pod['metadata']['ownerReferences']=[{'uid':rs['metadata']['uid'],'controller':True}]
                fixture.lists['replicasets'].append(rs);fixture.lists['pods'].append(pod)
            for role in ('verifier','build-controller','deployer'):
                password='synthetic_'+role.replace('-','_')+'x'*48
                secret=test_bootstrap_prepare.resource('Secret','ai-sdlc-database-'+role,data={k:base64.b64encode(v.encode()).decode() for k,v in {'uri':install.component_uri(role,password),'password':password}.items()})
                secret['metadata']['annotations']={'ai-sdlc.io/cluster-uid':fixture.cluster_uid}
                fixture.objects[('secret',secret['metadata']['name'])]=secret
            fixture.p.use_activation_render(docs,phase);fixture.p.observe()
            with self.assertRaises(ValueError):fixture.p.observe(paused=True)
            if phase=='active':
                with self.assertRaises(ValueError):fixture.p.inspect()
            pod=fixture.lists['pods'][-1];original=copy.deepcopy(pod)
            pod['spec']['containers'][0]['command']=['unreviewed']
            with self.assertRaises(ValueError):fixture.p.observe()
            fixture.lists['pods'][-1]=original
            fixture.lists['pods'].append(copy.deepcopy(original))
            with self.assertRaises(ValueError):fixture.p.observe()
            fixture.lists['pods'].pop()
            if phase!='warming':
                fixture.lists['pods'][-1]['status']['containerStatuses'][0]['ready']=False
                with self.assertRaises(ValueError):fixture.p.observe()
                if phase=='active':
                    fixture.p.use_expired_source_render(docs)
                    fixture.p.observe()  # Administrative source observation only.
                    fixture.lists['pods'][-1]['status']['phase']='Pending'
                    with self.assertRaises(ValueError):fixture.p.observe()
                    fixture.lists['pods'][-1]['status']['phase']='Running'
                    fixture.p.use_activation_render(docs,'active')
                    with self.assertRaises(ValueError):fixture.p.observe()

    def test_rendered_activation_is_closed_and_last_effect_only_intake(self):
        source=source_values();original=copy.deepcopy(source);renders={}
        for phase in ('warming','armed','active'):
            values=activation.phase_values(source,phase,compiler(source))
            docs=test_release_helm.ReleaseChartTests().render(values);renders[phase]=docs
            self.assertFalse(any(d['kind']=='Job' for d in docs))
            replicas={d['metadata']['name']:d['spec']['replicas'] for d in docs if d['kind']=='Deployment'}
            self.assertEqual(len(replicas),10);self.assertEqual(replicas.pop('ai-sdlc-intake'),int(phase=='active'))
            self.assertEqual(set(replicas.values()),{1})
            observer=bootstrap_prepare.Preparation(None,[],'podman','fixture',Path('/unused'),{})
            observer.use_activation_render(docs,phase)
            with self.assertRaises(ValueError):bootstrap_prepare.Preparation(None,[],'podman','fixture',Path('/unused'),{}).use_configured_render(docs)
            with self.assertRaises(ValueError):observer.use_activation_render(docs,'unknown')
        for d in renders['armed']:
            if d['kind']=='Deployment' and d['metadata']['name']=='ai-sdlc-intake':d['spec']['replicas']=1
        self.assertEqual(renders['armed'],renders['active']);self.assertEqual(source,original)
        for d in renders['warming']:
            if d['kind']=='Deployment' and d['metadata']['name']=='ai-sdlc-intake':d['spec']['replicas']=1
        with self.assertRaises(ValueError):observer.use_activation_render(renders['warming'],'warming')

    def test_global_pause_still_excludes_all_producers_and_default_migration_retained(self):
        source=source_values();source['runtime']['intakePaused']=True
        docs=test_release_helm.ReleaseChartTests().render(source)
        self.assertEqual({d['spec']['replicas'] for d in docs if d['kind']=='Deployment'},{0})
        self.assertEqual(sum(d['kind']=='Job' for d in docs),1)

    def test_unreviewed_policy_compiler_and_scope_changes_rejected(self):
        source=source_values()
        for mutate in (lambda v:v['release'].update(enabled=False),lambda v:v['runtime'].update(paused=False),
                lambda v:v['authority'].update(policyJSON=json.dumps(dict(json.loads(v['authority']['policyJSON']),freeze=False)))):
            value=copy.deepcopy(source);mutate(value)
            with self.assertRaises(ValueError):activation.phase_values(value,'armed',compiler(source))
        with self.assertRaises(ValueError):activation.phase_values(source,'armed',lambda _:dict(json.loads(source['build']['configJSON']),slots=[]))

    def fixture(self):
        a=object.__new__(activation.Activation);a.record={'completed':'armed','transition':{'from':'armed','to':'active'},'expiresAt':9999999999};a.phase=None
        a.save=Mock();a.observe=Mock();return a

    def test_lost_final_response_only_observes_target_no_empty_or_replay(self):
        a=self.fixture();a.resolve();a.observe.assert_called_once_with('active')
        self.assertEqual(a.record['completed'],'active');self.assertIsNone(a.record['transition']);a.save.assert_called_once()
        a.begin=Mock();a.apply=Mock();a.publish_report=Mock();a.activate();a.apply.assert_not_called();a.publish_report.assert_called_once()
        for call in a.observe.call_args_list:self.assertFalse(call.kwargs.get('empty',False))

    def test_partial_or_expired_transition_never_reaches_helm(self):
        a=self.fixture();a.observe=Mock(side_effect=ValueError('partial'));a.config=SimpleNamespace(base_run=Mock())
        with self.assertRaises(ValueError):a.resolve()
        a.save.assert_not_called();a.config.base_run.assert_not_called()
        a.record['expiresAt']=1
        with self.assertRaises(ValueError):a.apply('active')
        a.config.base_run.assert_not_called()

    def test_status_cannot_resolve_incomplete_transition_or_write(self):
        a=self.fixture();a.begin=Mock();a.resolve=Mock()
        with self.assertRaises(ValueError):a.activate(read_only=True)
        a.begin.assert_not_called();a.resolve.assert_not_called();a.save.assert_not_called()
        a.record.update(completed='active',transition=None)
        result=a.activate(read_only=True);self.assertTrue(result['issueIntakeEnabled']);self.assertFalse(result['workflowEvidence'])
        a.observe.assert_called_once_with('active');a.save.assert_not_called()

    def test_chart_drift_or_late_expiry_rejected_before_helm(self):
        for drift in (True,False):
            a=self.fixture();a.record.update(completed='armed',transition=None,chartSHA256='original',expiresAt=100)
            a.preflight=Mock();a.describe=Mock(return_value={'values':{},'documents':['reviewed']})
            a.config=SimpleNamespace(base_run=Mock(),render=Mock(return_value=['reviewed']))
            with self.subTest(chartDrift=drift),patch.object(activation,'chart_digest',return_value='changed' if drift else 'original'),patch.object(activation.time,'time',side_effect=[50,101]):
                with self.assertRaises(ValueError):a.apply('active')
            a.config.base_run.assert_not_called()

    def test_activation_cannot_extend_original_approval_windows(self):
        values={'provenance':{'trustJSON':json.dumps({'expiresAt':150000})},'release':{'targetJSON':json.dumps({'expiresAt':160000})}}
        self.assertEqual(activation.activation_expiry(values,100),150)
        with self.assertRaises(ValueError):activation.activation_expiry(values,150)
        for section,key in (('provenance','trustJSON'),('release','targetJSON')):
            values[section][key]=json.dumps({'expiresAt':9000000})
        self.assertEqual(activation.activation_expiry(values,100),1900)

if __name__=='__main__':unittest.main()
