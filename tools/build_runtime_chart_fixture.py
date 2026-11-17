"""Server-side dry-run of the runtime chart using isolated, never installed policy."""
import json
from pathlib import Path
import subprocess
import tempfile


def verify(call, tools, root, observe, bootstrap, profile, slot, output):
    # Ephemeral public policies exercise compilation/rendering only. No receipt
    # is signed, no inbox or live database is accessed, no policy is installed.
    policy = bootstrap['policy']
    inner = dict(policy, autonomy='auto-merge-low', freeze=True,
                 keys={key:value for key,value in policy['keys'].items() if value['role']!='build-controller'},
                 grants=[g for g in policy['grants'] if g['operation'] in ('admit-issue','plan-admission','execute-work','verify-artifact','publish-candidate','merge-publication')])
    outer = dict(policy, autonomy='auto-deploy', freeze=True,
                 keys={key:value for key,value in policy['keys'].items() if value['role'] in ('release-manager','build-controller','verifier','deployer','relay','orchestrator')},
                 grants=[g for g in policy['grants'] if g['operation'] in ('build-merged','verify-built','promote-artifact','deploy-release','record-deployment','record-production')])
    intake={'version':1,'repository':'os-santiago/saferus','baseBranch':'main','triggerLabel':'ai-sdlc:shadow','authorizedActorIDs':[1],'pollSeconds':30,'maxPages':100}
    compiled=observe('compile-build-installation',{'inner':inner,'outer':outer,'profile':profile,'slots':[slot],'intake':intake,'issueMaxAgeSeconds':60})
    if compiled.get('inner')!=inner or compiled.get('outer')!=outer or compiled.get('profile')!=profile:
        raise ValueError('Production compiler changed chart fixture scope')
    values={'runtime':{'mode':'verification'},'verifier':{'digest':'sha256:'+'a'*64},
            'review':{'enabled':True,'configJSON':'{}'},'publication':{'enabled':True},'merge':{'enabled':True,'configJSON':'{}'},
            'authority':{'policyJSON':json.dumps(inner)},'build':{'enabled':True,'digest':'sha256:'+'b'*64,'configJSON':json.dumps(compiled),'outerPolicyJSON':json.dumps(outer)}}
    with tempfile.NamedTemporaryFile(mode='w',suffix='.json') as file:
        json.dump(values,file);file.flush()
        chart=root/'runtime/k8s/deploy/helm/ai-sdlc-k8s-runtime'
        rendered=subprocess.run([str(tools/'helm-4.3.2'),'template','ai-sdlc',str(chart),'--namespace','ai-sdlc-runtime',
                                 '-f',str(root/'runtime/k8s/deploy/local/values.yaml'),'-f',file.name,'--show-only','templates/build.yaml'],
                                capture_output=True,text=True,check=True,timeout=30).stdout
    # kubectl's server dry-run performs schema/admission validation and commits
    # nothing. It does not pull the illustrative digests or certify readiness.
    result=call(['apply','--server-side','--dry-run=server','--field-manager=ai-sdlc-chart-fixture','--force-conflicts','-f','-','-o','name'],input=rendered,capture_output=True,check=False)
    (output/'runtime-chart-dry-run.log').write_text((result.stdout+result.stderr)[-32768:])
    if result.returncode:
        raise ValueError('Runtime build chart rejected by API server; see bounded public dry-run log')
    names=result.stdout.splitlines()
    if not any(name=='deployment.apps/ai-sdlc-build-controller' for name in names):
        raise ValueError('Build Deployment not validated by API server')
    (output/'runtime-chart.yaml').write_text(rendered)
    proof={'status':'passed','serverDryRun':True,'persistedResources':False,'policyInstalled':False,
           'syntheticPublicPolicy':True,'illustrativeImageDigests':True,'workloadExecuted':False,'resources':names}
    (output/'runtime-chart-report.json').write_text(json.dumps(proof,indent=2)+'\n')
    return proof
