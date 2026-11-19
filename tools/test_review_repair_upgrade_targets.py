"""The repair bridge changes a closed capability and chart delta."""
import copy
import json
import ssl
import time
import unittest

import install_images
import review_repair_upgrade_targets as repair
import test_review_feedback_upgrade
import install


def legacy_render(render, values):
    documents = copy.deepcopy(render(values))
    for obj in documents:
        if obj['kind'] == 'ConfigMap' and obj['metadata']['name'] == 'ai-sdlc-config':
            routes = json.loads(obj['data']['AI_SDLC_RELAY_ROUTES'])
            for grant in repair.GRANTS:
                routes.pop(grant['operation'], None)
            obj['data']['AI_SDLC_RELAY_ROUTES'] = json.dumps(routes,sort_keys=True,separators=(',',':'))
        if obj['kind'] == 'Deployment' and obj['metadata']['name'] == 'ai-sdlc-orchestrator':
            pod = obj['spec']['template']
            pod['metadata']['annotations'].pop('checksum/review-config',None)
            pod['spec']['volumes'] = [v for v in pod['spec']['volumes'] if v['name'] != 'review-config']
            pod['spec']['containers'][0]['volumeMounts'] = [v for v in pod['spec']['containers'][0]['volumeMounts'] if v['name'] != 'review-config']
    return documents


class RepairTargets(unittest.TestCase):
    def source(self):
        source, render, compiler, selection, trust = test_review_feedback_upgrade.FeedbackTargets().source()
        values=source['values']
        inner=json.loads((install.ROOT/'spec/fixtures/component-authority.v1.json').read_text());inner.update(json.loads(values['authority']['policyJSON']))
        inner['grants']=[g for g in inner['grants'] if g['operation'] not in {x['operation'] for x in repair.GRANTS}]
        values['authority']['policyJSON']=json.dumps(inner,sort_keys=True)
        build=json.loads(values['build']['configJSON']);build['inner']=inner
        values['build']['configJSON']=json.dumps(build,sort_keys=True)
        now=int(time.time()*1000)
        # The installed source carries an earlier, already expired window.
        for section,key in (('provenance','trustJSON'),('release','targetJSON')):
            document=json.loads(values[section][key])
            document.update(approvedAt=now-7200000,expiresAt=now-1000)
            values[section][key]=json.dumps(document,sort_keys=True)
        old=lambda v:legacy_render(render,v)
        source['documents']=old(values)
        release=json.loads(values['release']['targetJSON'])
        trust=dict(trust,approvedAt=now-10000,expiresAt=now+3600000)
        release.update(approvedAt=now-10000,expiresAt=now+3600000)
        return source,old,render,compiler,selection,{'provenance':trust,'release':release}

    def test_exact_capability_preserves_keys_limits_and_outer_policy(self):
        source,old,render,compiler,selection,trust=self.source()
        original=copy.deepcopy(source)
        result=repair.targets(source,selection,trust,old,render,compiler,time.time())
        active=result['active']['values'];before=source['values']
        candidate=json.loads(active['candidate']['configJSON']);prior=json.loads(before['candidate']['configJSON'])
        self.assertEqual(candidate['profile'],dict(prior['profile'],maxRepairs=1))
        self.assertEqual(candidate['model'],prior['model'])
        review=json.loads(active['review']['configJSON']);review['workerImage']=json.loads(before['review']['configJSON'])['workerImage']
        self.assertEqual(review,json.loads(before['review']['configJSON']))
        inner=json.loads(active['authority']['policyJSON']);previous=json.loads(before['authority']['policyJSON'])
        self.assertEqual(inner['keys'],previous['keys'])
        inner['grants']=[g for g in inner['grants'] if g not in repair.GRANTS]
        previous['grants'].sort(key=lambda x:(x['caller'],x['operation'],x['target']))
        self.assertEqual(inner,previous)
        self.assertEqual(active['build']['outerPolicyJSON'],before['build']['outerPolicyJSON'])
        for key in ('intake','contract','postgres','publication','merge'):
            self.assertEqual(active.get(key),before.get(key))
        for key,component in repair.COMPONENTS.items():
            self.assertEqual(active['components'][key]['digest'],selection['images'][component]['digest'])
        self.assertEqual(active['build']['digest'],selection['images']['build-controller']['digest'])
        self.assertEqual(active['release']['digest'],selection['images']['deployer']['digest'])
        self.assertEqual(source,original)

    def test_chart_cannot_add_permissions_or_weaken_security(self):
        source,old,render,compiler,selection,trust=self.source()
        def drift(values):
            docs=render(values)
            obj=next(x for x in docs if x['kind']=='Deployment' and x['metadata']['name']=='ai-sdlc-orchestrator')
            obj['spec']['template']['spec']['automountServiceAccountToken']=True
            return docs
        with self.assertRaisesRegex(ValueError,'outside reviewed'):
            repair.targets(source,selection,trust,old,drift,compiler,time.time())

    def test_existing_repair_grant_cannot_be_adopted(self):
        source,old,render,compiler,selection,trust=self.source()
        policy=json.loads(source['values']['authority']['policyJSON']);policy['grants']+=repair.GRANTS
        source['values']['authority']['policyJSON']=json.dumps(policy)
        build=json.loads(source['values']['build']['configJSON']);build['inner']=policy
        source['values']['build']['configJSON']=json.dumps(build)
        source['documents']=old(source['values'])
        with self.assertRaisesRegex(ValueError,'already grants'):
            repair.targets(source,selection,trust,old,render,compiler,time.time())

    def test_renews_both_approvals_and_nothing_else_in_them(self):
        source,old,render,compiler,selection,trust=self.source()
        result=repair.targets(source,selection,trust,old,render,compiler,time.time())
        active=result['active']['values']
        self.assertEqual(json.loads(active['provenance']['trustJSON']),trust['provenance'])
        self.assertEqual(json.loads(active['release']['targetJSON']),trust['release'])

    def test_approval_documents_cannot_change_scope_or_omit_renewal(self):
        cases={
            'missing-release':lambda a:a.pop('release'),
            'stale-provenance':lambda a:a['provenance'].update(expiresAt=json.loads(self._values['provenance']['trustJSON'])['expiresAt']),
            'stale-release':lambda a:a['release'].update(approvedAt=json.loads(self._values['release']['targetJSON'])['approvedAt']),
            'release-scope':lambda a:a['release'].update(namespace='unreviewed'),
            'provenance-keys':lambda a:a['provenance'].update(repository='unreviewed/repository'),
            'old-verifier':lambda a:a['provenance'].update(verifierImage=json.loads(self._values['provenance']['trustJSON'])['verifierImage']),
            'window-too-long':lambda a:a['release'].update(expiresAt=a['release']['approvedAt']+86400001)}
        for name,mutate in cases.items():
            with self.subTest(name):
                source,old,render,compiler,selection,approvals=self.source()
                self._values=source['values']
                mutate(approvals)
                with self.assertRaises(ValueError):
                    repair.targets(source,selection,approvals,old,render,compiler,time.time())



def make_ca(days=30):
    import subprocess,tempfile
    from pathlib import Path
    with tempfile.TemporaryDirectory() as d:
        subprocess.run(['openssl','req','-x509','-newkey','ec','-pkeyopt','ec_paramgen_curve:P-256','-nodes','-days',str(days),
            '-subj','/CN=Saferus local demo CA','-keyout',d+'/ca.key','-out',d+'/ca.crt'],check=True,capture_output=True)
        return Path(d+'/ca.crt').read_text()


class CARotation(unittest.TestCase):
    def setUp(self):
        import hashlib
        self.hashlib=hashlib
        self.original={'tlsCA':'old-ca','approvedAt':1,'expiresAt':2,'namespace':'n'}
        self.profile={'caSHA256':hashlib.sha256(b'old-ca').hexdigest(),'caConfigMapUID':'u','image':'i'}

    def test_profile_changes_only_ca_digest_and_recreated_object_when_rotated(self):
        raw=json.dumps(self.profile,sort_keys=True)
        new_uid='11111111-2222-4333-8444-555555555555'
        self.assertEqual(repair.rotated_profile(raw,self.original,dict(self.original),'u'),raw)
        rotated=json.loads(repair.rotated_profile(raw,self.original,dict(self.original,tlsCA='new-ca'),new_uid))
        self.assertEqual(rotated,dict(self.profile,caSHA256=self.hashlib.sha256(b'new-ca').hexdigest(),caConfigMapUID=new_uid))
        for approved,uid in ((dict(self.original),new_uid),(dict(self.original,tlsCA='new-ca'),'u'),
                             (dict(self.original,tlsCA='new-ca'),'not-a-uid'),(dict(self.original,tlsCA='new-ca'),None)):
            with self.subTest(uid=uid),self.assertRaises(ValueError):
                repair.rotated_profile(raw,self.original,approved,uid)
        with self.assertRaises(ValueError):
            repair.rotated_profile(json.dumps(dict(self.profile,caSHA256='0'*64)),self.original,self.original,'u')

    def test_compiled_profile_rejects_any_other_change(self):
        values=lambda profile,ca:{'release':{'browserProfileJSON':json.dumps(profile),'targetJSON':json.dumps({'tlsCA':ca})}}
        new_uid='11111111-2222-4333-8444-555555555555'
        rotated=dict(self.profile,caSHA256=self.hashlib.sha256(b'new-ca').hexdigest(),caConfigMapUID=new_uid)
        self.assertEqual(repair.expected_compiled_profile(self.profile,values(rotated,'new-ca')),rotated)
        self.assertEqual(repair.expected_compiled_profile(self.profile,values(self.profile,'old-ca')),self.profile)
        for profile,ca in ((dict(rotated,image='other'),'new-ca'),(rotated,'third-ca'),
                           (dict(rotated,caConfigMapUID='u'),'new-ca'),(dict(self.profile,caConfigMapUID=new_uid),'old-ca')):
            with self.subTest(profile=profile,ca=ca),self.assertRaises(ValueError):
                repair.expected_compiled_profile(self.profile,values(profile,ca))

    def test_rotated_ca_must_cover_the_approved_window(self):
        ca=make_ca(30)
        time.sleep(1)  # The approval necessarily follows the operator's CA rotation.
        now=time.time()
        original={'tlsCA':'old','approvedAt':int(now*1000)-7200000,'expiresAt':int(now*1000)-1000,'scope':'s'}
        approved=dict(original,tlsCA=ca,approvedAt=int(now*1000)-100,expiresAt=int(now*1000)+3600000)
        repair.validate_release(original,approved,now)
        short=make_ca(1)
        for bad in (dict(approved,tlsCA=short,expiresAt=int(now*1000)+86400000),
                    dict(approved,approvedAt=int(now*1000)-3600000),
                    dict(approved,tlsCA='not a certificate'),dict(approved,scope='other')):
            with self.subTest(bad=bad.get('scope')),self.assertRaises((ValueError,ssl.SSLError)):
                repair.validate_release(original,bad,now)


if __name__=='__main__': unittest.main()
