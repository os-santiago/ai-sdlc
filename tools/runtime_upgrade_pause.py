"""Pause and restore-verify an original runtime before a reviewed upgrade."""
import argparse
import copy
from contextlib import ExitStack
import json
import os
from pathlib import Path
import re
import tempfile
import time

import bootstrap_database_forward
import bootstrap_prepare as bootstrap
import build_foundation_install as files
import build_runtime_install
import initial_activation
import install
import install_images
import install_upgrade
import konflux_runtime_install as platform
import prepared_configuration
import prepared_resources
import release_target_install
from upgrade_fence_client import Fence

VERSION='ai-sdlc.runtime-upgrade-pause/v1'


def pause_values(source,compile_build):
    result=copy.deepcopy(source)
    if (source.get('runtime',{}).get('paused') is not False or source['runtime'].get('intakePaused') is not False
            or source.get('migrator',{}).get('enabled') is not False
            or not all(source.get(name,{}).get('enabled') is True for name in ('build','provenance','release','review','publication','merge'))):
        raise ValueError('Original complete active runtime required')
    inner=json.loads(source['authority']['policyJSON']);outer=json.loads(source['build']['outerPolicyJSON'])
    compiled=json.loads(source['build']['configJSON'])
    if inner.get('freeze') is not False or outer.get('freeze') is not False or compiled.get('inner')!=inner or compiled.get('outer')!=outer:
        raise ValueError('Original canonical active policies required')
    inner['freeze']=outer['freeze']=True
    request={key:copy.deepcopy(compiled[key]) for key in ('profile','slots','issueMaxAgeSeconds')}
    request.update(inner=inner,outer=outer,intake=json.loads(source['intake']['configJSON']))
    changed=compile_build(request)
    if changed!=dict(compiled,inner=inner,outer=outer):raise ValueError('Pause compiler changed original authority')
    result['authority']['policyJSON']=json.dumps(inner,sort_keys=True)
    result['build']['outerPolicyJSON']=json.dumps(outer,sort_keys=True)
    result['build']['configJSON']=json.dumps(changed,sort_keys=True)
    result['runtime'].update(paused=True,intakePaused=True)
    return result


def no_execution_resources(observer):
    namespaces={obj['metadata']['name'] for obj in observer.get('namespaces',namespace=None)['items']}
    for namespace in bootstrap.EXECUTION_NAMESPACES:
        if namespace in namespaces and observer.get('pods,jobs,cronjobs',namespace=namespace)['items']:
            raise ValueError('Original execution or cleanup resources remain')
    if 'ai-sdlc-builds' in namespaces and observer.get('pipelineruns,taskruns',namespace='ai-sdlc-builds')['items']:
        raise ValueError('Original build execution resources remain')


def pause_once(record,fence,observe_source,observe_target,apply_target,save):
    """One durable Kubernetes intent; never replay an ambiguous Helm send."""
    if record.get('phase')!='planned' or time.time()>=record['expiresAt']:
        raise ValueError('Original pause intent is unavailable or expired')
    fence.check();observe_source();fence.check()
    record['phase']='pausing';save(record)
    fence.check();apply_target();fence.check()
    while True:
        fence.check()
        try:observe_target();break
        except ValueError:
            if time.time()>=record['expiresAt']:raise ValueError('Original pause did not converge') from None
            fence.live();time.sleep(.5)
    fence.check();fence.release()
    # The caller must complete owned helper cleanup and reobserve the target
    # before it persists phase=paused. This is not a delivery-stage receipt.


class Pause:
    def __init__(self,config,plan=None):
        self.config=config;self.activation=initial_activation.Activation(config)
        if (self.activation.record is None or self.activation.record.get('completed')!='active'
                or self.activation.record.get('transition')):raise ValueError('Original completed activation required')
        self.activation_hash=bootstrap.digest(self.activation.record)
        self.path=config.state_dir/'runtime-upgrade-pause.json'
        self.record=files.bounded_json(self.path,16*1024*1024) if self.path.exists() or self.path.is_symlink() else None
        self.plan=plan
        if self.record is not None and (self.record.get('version')!=VERSION or self.record.get('activationSHA256')!=self.activation_hash
                or self.record.get('preparedSHA256')!=self.activation.original_hash):raise ValueError('Original pause custody changed')

    def unchanged(self):
        self.activation.unchanged()
        if bootstrap.digest(files.bounded_json(self.activation.path,16*1024*1024))!=self.activation_hash:
            raise ValueError('Original activation record changed during pause')
        if initial_activation.chart_digest()!=self.activation.record['chartSHA256']:
            raise ValueError('Pause requires the original reviewed chart')

    def source(self):
        self.unchanged()
        observed=self.activation.observe('active')
        description=self.activation.describe('active')
        observer=bootstrap.Preparation(self.config.base_run,self.config.kubectl,self.config.provider,self.config.cluster,self.config.state_dir,description['images'])
        observer.use_activation_render(description['documents'],'active')
        again,connection=observer.observe(paused=False)
        if observed!=again:raise ValueError('Original active runtime changed during pause observation')
        no_execution_resources(observer)
        return observed,connection

    def target(self):
        self.unchanged();target=self.record['target']
        prepared_resources.observe(self.config,target,self.record['resourceUIDs'])
        observer=bootstrap.Preparation(self.config.base_run,self.config.kubectl,self.config.provider,self.config.cluster,self.config.state_dir,target['images'])
        observer.use_configured_render(target['documents'])
        observed=observer.observe(paused=True)[0] # history is retained; never call inspect-empty-bootstrap
        prepared_configuration.same_foundation(self.record['sourceObserved'],observed)
        return observed

    def save(self,record):platform.write_record(self.path,record);self.record=record

    def installation(self):
        prior=files.bounded_json(self.config.state_dir/'installation.json',2*1024*1024)
        if prior.get('activationSHA256')!=self.activation_hash:
            raise ValueError('Original installation snapshot changed')
        return prior

    def publish_report(self):
        self.target()
        prior=self.installation()
        prior.update(configuration='upgrade-paused',installationReady=False,runtimePaused=True,innerFrozen=True,outerFrozen=True,
            deliveryEnabled=False,issueIntakeEnabled=False,runtimeReadinessValidated=False,observedAt=int(time.time()*1000),
            upgradePauseSHA256=bootstrap.digest(self.record),autonomousEndToEndValidated=False)
        platform.write_record(self.config.state_dir/'installation.json',prior)

    def status(self):
        if self.record is None:raise ValueError('No runtime pause recorded')
        if self.record.get('phase')!='paused':
            raise ValueError('Pause remains incomplete; preserve original intent and reconcile before further effects')
        self.target()
        return {'version':VERSION,'phase':'paused','runtimePaused':True,'issueIntakeEnabled':False,
                'upgradeAuthorized':False,'workflowEvidence':False,'autonomousEndToEndValidated':False}

    def run(self):
        if self.record is not None:
            # A failed/unknown send is deliberately not automatically repeated.
            result=self.status()
            self.publish_report()
            return result
        if self.plan is None:raise ValueError('Explicit reviewed upgrade plan required')
        self.unchanged();self.installation()
        source=self.activation.describe('active');images=source['images']
        published=self.plan['source']['images'];revision=self.plan['source']['revision']
        expected={'image':{'repository':install_images.REGISTRY,'tag':revision},
            'components':{key:{'digest':published[component]['digest']} for key,component in install_images.COMPONENTS.items()},
            'migrator':{'digest':published['migrator']['digest']}}
        if images!=expected:raise ValueError('Published source differs from the original installed image lock')
        helper=install_images.REGISTRY+'@'+self.plan['target']['images']['migrator']['digest']
        catalog=build_runtime_install.component_call(self.config.provider,helper,'describe-migrations',{})
        if catalog!=self.plan['target']['migrations']:raise ValueError('Reviewed helper migration catalog changed')
        def compile_build(request):
            return build_runtime_install.component_call(self.config.provider,images['image']['repository']+'@'+images['migrator']['digest'],'compile-build-installation',request)
        values=pause_values(source['values'],compile_build)
        target={'images':images,'values':values,'documents':self.config.render(values),'externalRuntime':source['externalRuntime']}
        if self.config.render(source['values'])!=source['documents']:raise ValueError('Original active chart render changed')
        if set(prepared_resources.documents(target['documents']))!=set(self.activation.original['target']['resourceUIDs']):
            raise ValueError('Pause cannot add or remove Helm resources')
        validator=bootstrap.Preparation(None,[],self.config.provider,self.config.cluster,self.config.state_dir,images)
        validator.use_configured_render(target['documents'])
        observed,connection=self.source()
        now=time.time();expiry=min(now+300,initial_activation.activation_expiry(source['values'],now))
        record={'version':VERSION,'activationSHA256':self.activation_hash,'preparedSHA256':self.activation.original_hash,
            'chartSHA256':initial_activation.chart_digest(),'planSHA256':bootstrap.digest(self.plan),'plan':self.plan,
            'sourceObserved':observed,'resourceUIDs':self.activation.original['target']['resourceUIDs'],
            'target':target,'createdAt':now,'expiresAt':expiry,'phase':'planned'}
        # DB write exclusion is ephemeral. Persist the original intent before
        # any Kubernetes mutation, only after the actual helper acquired custody.
        with bootstrap_database_forward.original_database(self.config.base_run,self.config.kubectl,connection,observed['resources']['pod'],timeout_seconds=240) as (forwarded,port):
            with Fence(self.config.provider,helper,forwarded,port,self.plan['source']['migrations'],expires_at=expiry) as fence:
                record['fence']={'nonce':fence.nonce,'expiresAt':fence.expiry,'containerID':fence.container_id}
                self.save(record)
                def observe_source():
                    current,_=self.source()
                    if current!=observed:raise ValueError('Original source changed after acquiring write exclusion')
                def apply_target():
                    self.unchanged()
                    if self.config.render(values)!=target['documents']:raise ValueError('Original pause target changed')
                    with tempfile.NamedTemporaryFile(mode='w',suffix='.json') as f:
                        json.dump(values,f);f.flush()
                        fence.command([self.config.binaries['helm'],'upgrade','ai-sdlc',str(install.CHART),
                            '--kubeconfig',str(self.config.state_dir/'kubeconfig'),'--kube-context','kind-'+self.config.cluster,
                            '-n',platform.NAMESPACE,'-f',f.name,'--timeout','60s'],timeout=60)
                pause_once(record,fence,observe_source,self.target,apply_target,self.save)
        self.target();record['phase']='paused';self.save(record)
        self.publish_report()
        return self.status()


def main(argv=None):
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command',choices=('pause','status','backup','backup-status','diagnostic','diagnostic-status','recover-original',
        'tool-budget','tool-budget-status','recover-tool-budget','reapprove','reapprove-status',
        'reviewer-image','reviewer-image-status','browser','browser-status',
        'review-feedback','review-feedback-status'));parser.add_argument('--cluster',required=True)
    parser.add_argument('--provider',choices=('auto','podman','docker'),default='auto')
    parser.add_argument('--provenance-trust',type=Path)
    parser.add_argument('--release-target',type=Path)
    for name in ('source','target'):
        parser.add_argument('--'+name+'-run',type=int);parser.add_argument('--'+name+'-revision')
    args=parser.parse_args(argv)
    if args.command == 'reapprove':
        if args.provenance_trust is None or args.release_target is None:
            raise ValueError('Reapproval requires both separately reviewed documents')
    elif args.command in ('browser','browser-status','review-feedback','review-feedback-status'):
        if args.release_target is not None or args.command in ('browser','review-feedback') and args.provenance_trust is None:
            raise ValueError('Selected upgrade requires reviewed verifier trust and preserves the release target')
    elif args.provenance_trust is not None or args.release_target is not None:
        raise ValueError('Approval files are accepted only by explicit reapprove')
    if not re.fullmatch('[a-z][a-z0-9-]{0,35}',args.cluster):raise ValueError('Selected original local cluster required')
    state=Path(os.environ.get('XDG_STATE_HOME',Path.home()/'.local/state'))/'ai-sdlc'/args.cluster
    release_target_install.private_directory(state)
    with ExitStack() as stack:
        platform.lock_state(state,stack)
        provider=platform.select_provider(args.provider,state,args.cluster)
        binaries=install.tools(Path(os.environ.get('XDG_CACHE_HOME',Path.home()/'.cache'))/'ai-sdlc/tools')
        kubectl=[binaries['kubectl'],'--kubeconfig',str(state/'kubeconfig'),'--context','kind-'+args.cluster,'--request-timeout=10s']
        config=prepared_configuration.Configuration(install.run,kubectl,binaries,provider,args.cluster,state,{})
        plan=None
        if args.command=='pause':
            plan=install_upgrade.plan(install_upgrade.Source(install.ROOT),install_images.GitHub(),args.source_run,args.source_revision,args.target_run,args.target_revision)
        if args.command in ('review-feedback','review-feedback-status'):
            if args.source_run is not None or args.source_revision is not None:
                raise ValueError('Feedback upgrade preserves the original recorded browser source')
            from review_feedback_runtime_upgrade import ReviewFeedbackUpgrade
            upgrade=ReviewFeedbackUpgrade(config,args.target_run,args.target_revision,args.provenance_trust)
            try:result=upgrade.run() if args.command=='review-feedback' else upgrade.status()
            finally:upgrade.close()
        elif args.command in ('browser','browser-status'):
            if args.source_run is not None or args.source_revision is not None:
                raise ValueError('Browser upgrade preserves the original recorded reviewer source')
            from browser_runtime_upgrade import BrowserUpgrade
            upgrade=BrowserUpgrade(config,args.target_run,args.target_revision,args.provenance_trust)
            try:result=upgrade.run() if args.command=='browser' else upgrade.status()
            finally:upgrade.close()
        elif args.command in ('reviewer-image','reviewer-image-status'):
            if args.source_run is not None or args.source_revision is not None:
                raise ValueError('Reviewer update preserves the original recorded source')
            from reviewer_image_upgrade import ReviewerImageUpgrade
            upgrade=ReviewerImageUpgrade(config,args.target_run,args.target_revision)
            result=upgrade.run() if args.command=='reviewer-image' else upgrade.status()
        elif args.command in ('reapprove','reapprove-status'):
            if any(getattr(args,name+'_'+field) is not None for name in ('source','target') for field in ('run','revision')):
                raise ValueError('Reapproval preserves the original recorded component selection')
            from approval_window import ApprovalWindow
            upgrade=ApprovalWindow(config,args.provenance_trust,args.release_target)
            result=upgrade.run() if args.command=='reapprove' else upgrade.status()
        elif args.command in ('tool-budget','tool-budget-status','recover-tool-budget'):
            if any(getattr(args,name+'_'+field) is not None for name in ('source','target') for field in ('run','revision')):
                raise ValueError('Tool-budget transition uses only the original recorded diagnostic source')
            from tool_budget_upgrade import ToolBudgetUpgrade
            upgrade=ToolBudgetUpgrade(config)
            result=upgrade.run() if args.command=='tool-budget' else upgrade.recover() if args.command=='recover-tool-budget' else upgrade.status()
        elif args.command in ('diagnostic','diagnostic-status','recover-original'):
            if any(getattr(args,name+'_'+field) is not None for name in ('source','target') for field in ('run','revision')):
                raise ValueError('Diagnostic transition uses only the original recorded pause plan')
            from diagnostic_runtime_upgrade import Upgrade
            upgrade=Upgrade(config)
            result=upgrade.run() if args.command=='diagnostic' else upgrade.recover() if args.command=='recover-original' else upgrade.status()
        elif args.command in ('backup','backup-status'):
            if any(getattr(args,name+'_'+field) is not None for name in ('source','target') for field in ('run','revision')):
                raise ValueError('Backup uses only the original recorded pause plan')
            from runtime_upgrade_backup import Backup
            backup=Backup(config)
            result=backup.run() if args.command=='backup' else backup.status()
        else:result=Pause(config,plan).run() if args.command=='pause' else Pause(config).status()
        print(json.dumps(result,indent=2));return 0
