"""Install schema35 browser evidence after an original contained reviewer update."""
import base64
import copy
import json
import secrets
import time

import bootstrap_database_forward
import bootstrap_prepare as bootstrap
import browser_platform_install
import browser_upgrade_chart
import browser_upgrade_data
import browser_upgrade_targets
import build_foundation_install as files
import build_runtime_install
from diagnostic_runtime_upgrade import advance
import initial_activation
import install
import install_images
import install_upgrade
from installer_process import capture
import konflux_runtime_install as platform
import prepared_configuration
import prepared_resources
from reviewer_image_upgrade import ReviewerImageUpgrade
from approval_window import ApprovedReleasePreflight
from runtime_upgrade_pause import pause_values, no_execution_resources
import upgrade_backup_data as data
from upgrade_fence_client import Fence
from upgrade_migrator import Migrator

VERSION='ai-sdlc.browser-runtime-upgrade/v1'
PHASES=('migrated','staged','warming','armed','active')


def paused_inventory(objects,description,identities,database_uid,previous=None):
    desired={key:obj for key,obj in prepared_resources.documents(description['documents']).items() if obj['kind']=='Deployment'}
    deployments={prepared_resources.key(obj):obj for obj in objects if obj['kind']=='Deployment'}
    pods=[obj for obj in objects if obj['kind']=='Pod']
    if (len(desired)!=10 or set(deployments)!=set(desired) or len(objects)!=len(deployments)+len(pods)
            or len(pods)!=1 or bootstrap.Preparation.identity(pods[0])!=database_uid):
        raise ValueError('Original paused producer inventory changed')
    generations={}
    for key,obj in deployments.items():
        generation=obj['metadata'].get('generation')
        if (bootstrap.Preparation.identity(obj)!=identities[key] or type(generation) is not int or generation<1
                or obj['spec'].get('replicas')!=0 or prepared_resources.content(obj)!=prepared_resources.content(desired[key])
                or any(obj.get('status',{}).get(k,0)!=0 for k in ('replicas','readyReplicas','availableReplicas','updatedReplicas'))):
            raise ValueError('Original paused producer changed')
        generations[key]=generation
    if previous is not None and previous!=generations:raise ValueError('Original paused producer generation changed')
    return generations


class BrowserPreflight(ApprovedReleasePreflight):
    def __init__(self,upgrade):
        super().__init__(upgrade,upgrade.record['targets']['active'])
        self.original['sources']['browser-original-profile']=upgrade.record['browserCompiled']


class BrowserUpgrade(ReviewerImageUpgrade):
    def __init__(self,config,run=None,revision=None,trust_path=None):
        self.config=config
        self.source_reviewer=ReviewerImageUpgrade(config)
        prior=self.source_reviewer.record
        if (prior is None or prior.get('completed')!='active' or prior.get('transition')
                or prior.get('recoveryPause') or prior.get('mode')!='reviewer-image'
                or (prior.get('backup') or {}).get('phase')!='verified'
                or prior['backup'].get('restore',{}).get('state')!='removed'):
            raise ValueError('Original completed reviewer image update required')
        self.parent,self.pause=self.source_reviewer.parent,self.source_reviewer.pause
        self.parent_hash=bootstrap.digest(prior)
        self.catalog=self.source_reviewer.catalog
        self.path=config.state_dir/'browser-runtime-upgrade.json'
        self.directory=config.state_dir/'browser-runtime-backup'
        self.record=files.bounded_json(self.path,32*1024*1024) if self.path.exists() or self.path.is_symlink() else None
        self.source_chart=browser_upgrade_chart.OriginalChart(install.ROOT,prior['selection']['revision'],self.pause.activation.record['chartSHA256'])
        self.target_chart=install.CHART
        self.approved_trust=files.bounded_json(trust_path,128*1024) if trust_path is not None else None
        self.selection=None
        if (run is None)!=(revision is None):raise ValueError('Both immutable browser publication inputs are required')
        if self.record is not None:
            if self.record.get('version')!=VERSION or self.record.get('parentSHA256')!=self.parent_hash:
                raise ValueError('Original browser operation custody changed')
            if run is not None and (run!=self.record['selection']['run'] or revision!=self.record['selection']['revision']):
                raise ValueError('Existing browser operation cannot select another publication')
            if browser_upgrade_chart.digest(self.target_chart)!=self.record['chartSHA256']:
                raise ValueError('Original target browser chart changed')
            self.selection=self.record['selection']
            if self.approved_trust is not None and self.approved_trust!=self.record['approvedTrust']:raise ValueError('Original reviewed trust changed')
            self.approved_trust=self.record['approvedTrust']
        elif run is not None:
            if type(run) is not int or not 0<run<=2**63-1 or not install_images.SHA.fullmatch(revision):
                raise ValueError('Explicit exact-main browser publication required')
            source=install_upgrade.Source(install.ROOT)
            source.git('merge-base','--is-ancestor',prior['selection']['revision'],revision)
            catalog=source.catalog(revision)
            local=source.catalog(source.git('rev-parse','HEAD').decode().strip())
            if len(catalog)!=35 or catalog[:34]!=self.catalog or catalog[-1]['filename']!=browser_upgrade_data.MIGRATION or catalog!=local:
                raise ValueError('Only the exact locally reviewed 34-to-35 browser bridge is supported')
            api=install_images.GitHub()
            # Verify signatures; revisions without the cutover sentinel predate signing.
            _,publication=install_images.resolve(api,run,revision,include_build=True,include_release=True,include_browser=True,
                                                 verify=install_images.signature_verifier(api,revision,allow_unsigned_legacy=True))
            self.selection=dict(publication,catalog=catalog)
        self.helper=(install_images.REGISTRY+'@'+self.selection['images']['migrator']['digest']) if self.selection else self.source_reviewer.helper

    def close(self):self.source_chart.close()

    def custody(self):
        with self.source_chart.selected():self.source_reviewer.custody()
        if bootstrap.digest(files.bounded_json(self.source_reviewer.path,32*1024*1024))!=self.parent_hash:
            raise ValueError('Original reviewer installation custody changed')
        if self.record and (self.record.get('backup') or {}).get('phase')=='verified':
            data.verify_archives(self.directory,self.record['backup']['archives'])
        if self.record and self.record.get('browserPlatformSHA256') and bootstrap.digest(files.bounded_json(self.config.state_dir/'browser-platform.json',1024*1024))!=self.record['browserPlatformSHA256']:
            raise ValueError('Original browser platform record changed')
        if self.record and browser_upgrade_chart.digest(self.target_chart)!=self.record['chartSHA256']:
            raise ValueError('Original browser target chart changed')

    def unchanged(self):
        self.custody()
        observed=files.bounded_json(self.config.state_dir/'installation.json',2*1024*1024)
        if self.record is None:
            reports=self.source_reviewer.record['installationReports']
            if not reports or observed!=reports[-1] or observed.get('configuration')!='active' or observed.get('installationReady') is not True:
                raise ValueError('Original completed reviewer installation report required')
        elif observed not in [self.record['installationSource']]+self.record.get('installationReports',[]):
            raise ValueError('Original installation report changed during browser update')

    def describe(self,phase):
        if self.record is None:return self.source_reviewer.describe('active')
        if phase in ('ready','prepared','migrated'):phase='paused'
        return self.record['targets'][phase]

    def observe(self,phase):
        self.unchanged();description=self.describe(phase)
        ids=self.pause.record['resourceUIDs'] if self.record is None else self.record['resourceUIDs']
        expected={key:value for key,value in ids.items() if key in prepared_resources.documents(description['documents'])}
        observed_ids=prepared_resources.observe(self.config,description,expected)
        observer=bootstrap.Preparation(self.config.base_run,self.config.kubectl,self.config.provider,self.config.cluster,self.config.state_dir,description['images'])
        paused=phase in ('paused','ready','prepared','migrated','staged')
        if paused:observer.use_configured_render(description['documents'])
        else:observer.use_activation_render(description['documents'],'active' if phase=='source' else phase)
        observed,connection=observer.observe(paused=paused)
        prepared_configuration.same_foundation(self.pause.record['sourceObserved'],observed)
        if phase in ('source','paused','ready','prepared','migrated'):no_execution_resources(observer)
        # New Helm IDs may be captured only after their one durable staging
        # intent and complete exact desired render; no unrelated objects adopted.
        if self.record and phase=='staged' and any(value is None for value in self.record['resourceUIDs'].values()):
            if self.record.get('transition',{}).get('to')!='staged':raise ValueError('Unowned browser Helm resource')
            self.record['resourceUIDs']=observed_ids;self.save(self.record)
        return observed,connection

    def snapshot(self,database=None):
        database=database or self.database()
        compatible=browser_upgrade_data.Compatibility(database)
        compatible.quiet()
        names=json.loads(database.sql('SELECT json_agg(filename ORDER BY filename) FROM schema_migrations;'))
        expected=self.selection['catalog'] if self.record and self.record.get('migrationVerified') else self.catalog
        if names!=[x['filename'] for x in expected]:raise ValueError('Original browser schema phase changed')
        return database.snapshot()

    def retained(self,database=None):
        database=database or self.database()
        if self.record.get('migrationVerified'):
            compatibility=browser_upgrade_data.Compatibility(database)
            original=dict(self.record['sourceCompatibility'],data=self.retained_proof())
            compatibility.verify(original,self.selection['catalog'])
            if self.snapshot(database)!=self.record['migratedProof']:raise ValueError('Post-migration history changed before activation')
        else:
            if self.snapshot(database)!=self.retained_proof():raise ValueError('Original history changed during browser preparation')
            if browser_upgrade_data.Compatibility(database).roles()!=self.record['sourceRoleIdentities']:
                raise ValueError('Original database role identities changed')

    def fence(self,connection,port):
        catalog=self.selection['catalog'] if self.record.get('migrationVerified') else self.catalog
        return Fence(self.config.provider,self.helper,connection,port,catalog,expires_at=self.record['expiresAt'],browser_deployment=True)

    def source_render(self,values):
        with self.source_chart.selected():return self.config.render(values)

    def begin(self):
        if self.record is not None:return
        if self.selection is None:raise ValueError('Explicit reviewed browser publication required')
        self.observe('source')
        compile_build=lambda request:build_runtime_install.component_call(self.config.provider,self.helper,'compile-build-installation',request)
        source=self.source_reviewer.describe('active')
        verifier=install_images.REGISTRY+'@'+self.selection['images']['verifier']['digest']
        browser_upgrade_targets.validate_trust(json.loads(source['values']['provenance']['trustJSON']),self.approved_trust,verifier)
        compiled=build_runtime_install.component_call(self.config.provider,self.helper,'compile-provenance-installation',
            {'trust':self.approved_trust,'profile':json.loads(source['values']['build']['configJSON'])['profile'],'verifierImage':verifier})
        if (set(compiled)!={'version','trust','trustSHA256','profileSHA256','verifierImage'}
                or compiled['version']!='ai-sdlc.provenance-installation/v1' or compiled['trust']!=self.approved_trust
                or compiled['verifierImage']!=verifier):raise ValueError('Reviewed browser trust compiler changed input')
        if self.source_render(source['values'])!=source['documents']:raise ValueError('Original source chart render differs')
        paused=pause_values(source['values'],compile_build)
        paused_description=dict(copy.deepcopy(source),values=paused,documents=self.source_render(paused))
        proof=browser_upgrade_data.Compatibility(self.database()).source(self.catalog)
        roles=self.original_database_authority(proof['data'])
        self.observe('source');now=time.time()
        self.save({'version':VERSION,'nonce':secrets.token_hex(16),'parentSHA256':self.parent_hash,
            'createdAt':now,'expiresAt':initial_activation.activation_expiry(source['values'],now),
            'chartSHA256':browser_upgrade_chart.digest(self.target_chart),'sourceChartSHA256':self.source_chart.expected,
            'mode':'browser-evidence','completed':'source','transition':None,
            'selection':copy.deepcopy(self.selection),'approvedTrust':copy.deepcopy(self.approved_trust),'approvedTrustSHA256':bootstrap.digest(self.approved_trust),'targets':{'source':copy.deepcopy(source),'paused':paused_description},
            'sourceProof':proof['data'],'sourceCompatibility':proof,'sourceRoleIdentities':roles,
            'resourceUIDs':copy.deepcopy(self.pause.record['resourceUIDs']),
            'pause':None,'backup':None,'recoveryPause':None,'migration':{},'migrationVerified':False,
            'installationSource':files.bounded_json(self.config.state_dir/'installation.json',2*1024*1024),'installationReports':[]})

    def new_helm_absent(self):
        self.deadline();self.unchanged()
        for key,obj in prepared_resources.documents(self.record['targets']['staged']['documents']).items():
            if self.record['resourceUIDs'][key] is None and self.config.read(obj['kind'],obj['metadata']['name'],obj['metadata'].get('namespace')) is not None:
                raise ValueError('New browser Helm resource appeared without its original creation acknowledgement')

    def apply(self,phase,command=capture):
        if phase=='paused':
            with self.source_chart.selected():return super().apply(phase,command)
        def guarded_command(argv,**kwargs):
            if phase=='staged':self.new_helm_absent()
            return command(argv,**kwargs)
        return super().apply(phase,guarded_command)

    def prepare(self):
        self.deadline();self.observe('ready');self.retained()
        self.browser_generations=None
        def infrastructure_guard():
            # Full installation checks bracket this infrastructure-only step.
            # Each nested read/effect retains custody and checks quiescence in
            # one API inventory, rather than re-reading every Helm object.
            self.deadline();self.unchanged()
            self.browser_generations=paused_inventory(self.config.inventory('Deployment,Pod'),
                self.record['targets']['paused'],self.record['resourceUIDs'],
                self.pause.record['sourceObserved']['resources']['pod'],self.browser_generations)
            self.deadline()
        infrastructure_guard()
        target=json.loads(self.record['targets']['source']['values']['release']['targetJSON'])
        infrastructure=browser_platform_install.Platform(self.config,target,
            install_images.REGISTRY+'@'+self.selection['images']['browser-verifier']['digest'],self.helper,
            infrastructure_guard,self.record['expiresAt'])
        profile=infrastructure.run()
        self.deadline();self.observe('ready');self.retained()
        compile_build=lambda request:build_runtime_install.component_call(self.config.provider,self.helper,'compile-build-installation',request)
        targets=browser_upgrade_targets.targets(self.record['targets']['source'],self.selection,profile,self.approved_trust,self.source_render,self.config.render,compile_build)
        if targets['paused']!=self.record['targets']['paused']:raise ValueError('Original pause changed during browser preparation')
        ids=copy.deepcopy(self.record['resourceUIDs'])
        for key,obj in prepared_resources.documents(targets['staged']['documents']).items():
            if key not in ids:
                if self.config.read(obj['kind'],obj['metadata']['name'],obj['metadata'].get('namespace')) is not None:
                    raise ValueError('New browser Helm resource exists before staging intent')
                ids[key]=None
        self.record.update(targets=targets,resourceUIDs=ids,browserCompiled=infrastructure.record['compiled'],
            browserPlatformSHA256=bootstrap.digest(infrastructure.record),completed='prepared')
        self.save(self.record)
        BrowserPreflight(self).preflight();self.retained()

    def migrate(self):
        self.deadline();observed,connection=self.observe('prepared');self.retained()
        intent=self.record['migration']
        if intent:
            raise ValueError('Original browser migration outcome cannot be replayed')
        secret=self.config.read('secret','ai-sdlc-database-roles')
        if set(secret.get('data',{}))!={'passwords.json'}:raise ValueError('Original role credentials required')
        passwords=json.loads(base64.b64decode(secret['data']['passwords.json'],validate=True))
        with bootstrap_database_forward.original_database(self.config.base_run,self.config.kubectl,connection,observed['resources']['pod'],timeout_seconds=240) as (forwarded,port):
            helper=Migrator(self.config.provider,self.helper,forwarded,port,passwords,intent,lambda _:self.save(self.record),
                expires_at=self.record['expiresAt'],guard=lambda:self.observe('prepared'))
            try:helper.run()
            finally:helper.close()
        self.observe('prepared')
        original=dict(self.record['sourceCompatibility'],data=self.retained_proof())
        self.record['compatibility']=browser_upgrade_data.Compatibility(self.database()).verify(original,self.selection['catalog'])
        self.record['migrationVerified']=True
        self.record['migratedProof']=self.snapshot()
        self.record['completed']='migrated';self.save(self.record)

    def run(self):
        self.begin();self.deadline()
        if self.record.get('mode')!='browser-evidence' or self.record.get('recoveryPause'):
            raise ValueError('Original browser operation required')
        if self.record['completed']=='source':
            self.pause_current();self.record['completed']='paused';self.save(self.record);self.publish_report(active=False)
        if self.record['completed']=='paused':
            self.backup();self.record['completed']='ready';self.save(self.record)
        if self.record['completed']=='ready':self.prepare()
        if self.record['completed']=='prepared':self.migrate()
        for phase in PHASES[PHASES.index(self.record['completed'])+1:]:
            if self.record.get('transition') is None:
                self.observe(self.record['completed']);self.retained();BrowserPreflight(self).preflight();self.retained()
                if phase=='staged':self.new_helm_absent()
            advance(self.record,phase,self.observe,self.apply,self.save,phases=PHASES)
        self.publish_report(active=True);return self.status()

    def publish_report(self,*,active):
        self.observe('active' if active else 'paused')
        result=copy.deepcopy(self.record['installationSource'])
        result.update(configuration='active' if active else 'browser-upgrade-paused',installationReady=active,
            runtimePaused=not active,innerFrozen=not active,outerFrozen=not active,deliveryEnabled=active,
            issueIntakeEnabled=active,runtimeReadinessValidated=active,browserUpgradeOperation=self.record['nonce'],
            browserUpgradeParentSHA256=self.parent_hash,browserUpgradeSelection=copy.deepcopy(self.selection),
            observedAt=int(time.time()*1000),workflowEvidence=False,autonomousEndToEndValidated=False)
        reports=self.record['installationReports']
        if reports and {k:v for k,v in reports[-1].items() if k!='observedAt'}=={k:v for k,v in result.items() if k!='observedAt'}:result=reports[-1]
        else:
            if len(reports)>=2:raise ValueError('Original browser report bound exceeded')
            reports.append(result);self.save(self.record)
        platform.write_record(self.config.state_dir/'installation.json',result)

    def status(self):
        if self.record is None or self.record.get('transition'):raise ValueError('Complete browser phase required')
        self.observe(self.record['completed'])
        return {'version':VERSION,'phase':self.record['completed'],'catalogSize':35 if self.record['migrationVerified'] else 34,
            'issueIntakeEnabled':self.record['completed']=='active','administrativeDeadline':int(self.record['expiresAt']*1000),
            'previousWorkflowAuthorityRenewed':False,'workflowEvidence':False,'autonomousEndToEndValidated':False}
