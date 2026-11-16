"""Disposable PostgreSQL composition for the explicit closed-workload fixture."""
import json
import os
from pathlib import Path
import re
import secrets
import tempfile
import time

import fixture_cleanup
import supply_chain_install

PUBLIC_FILES = ('profile.json','fixture-slot.json','fixture-command.json','build.json','publish.json','transport-report.json')


def validate_execution(stdout, report):
    name='TestRealJournalBackedClosedBuild'
    if (not re.search(r'^--- PASS: '+name+r' \(',stdout,re.MULTILINE)
            or re.search(r'^--- (SKIP|FAIL): '+name,stdout,re.MULTILINE)
            or report.get('status')!='passed' or report.get('durableBuiltRecordValidated') is not True
            or report.get('syntheticAuthority') is not True or report.get('releaseAuthorized') is not False):
        raise ValueError('Actual journal-backed test and durable built record required')


def runner_identity(provider):
    # Docker rootful must write as the host user. Rootless Podman also needs
    # keep-id so the selected in-container UID maps to that same host user.
    args = ['--user', str(os.getuid()) + ':' + str(os.getgid())]
    if provider == 'podman' and os.getuid() != 0:
        args += ['--userns=keep-id']
    return args


def execute(deadline, provider, node_id, binary, payload, profile, output, copy_back):
    lock = json.loads((Path(__file__).parent/'runtime-validation.lock.json').read_text())
    nonce = secrets.token_hex(12)
    label = 'ai-sdlc.io/journal-build-fixture='
    password = secrets.token_urlsafe(36)
    environment = dict(os.environ, POSTGRES_PASSWORD=password, POSTGRES_DB='ai_sdlc_test',
                       AI_SDLC_TEST_DATABASE_URL='postgres://postgres:'+password+'@127.0.0.1:5432/ai_sdlc_test?sslmode=disable',
                       AI_SDLC_JOURNAL_BUILD_FIXTURE='/fixture/private.json')
    if not re.fullmatch(r'[a-f0-9]{64}', node_id):
        raise ValueError('Original kind node ID required')
    def call(arguments, **kwargs):
        return deadline.run([provider]+arguments, env=environment, capture_output=True, **kwargs)
    report = {'scope':'disposable fixture database','cleanupComplete':False}
    def save():
        (output/'journal-database.json').write_text(json.dumps(report,indent=2)+'\n')
    save()
    with tempfile.TemporaryDirectory(prefix='ai-sdlc-journal-build-') as directory:
        root=Path(directory); evidence=root/'evidence';evidence.mkdir(mode=0o700)
        (evidence/'profile.json').write_text(json.dumps(profile))
        private=root/'private.json'
        with os.fdopen(os.open(private,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600),'w') as f:
            json.dump(dict(payload,Phase='journal-execute',OutputDirectory='/evidence'),f)
        try:
            db=call(['create','--name','ai-sdlc-journal-db-'+nonce,'--label',label+nonce,
                     '--network','container:'+node_id,'--tmpfs','/var/lib/postgresql/data:rw,size=512m',
                     '--env','POSTGRES_PASSWORD','--env','POSTGRES_DB',lock['postgres'],'postgres',
                     '-c','listen_addresses=127.0.0.1','-c','max_wal_size=128MB','-c','min_wal_size=32MB','-c','checkpoint_timeout=30s']).stdout.strip()
            report['databaseContainerID']=db;save()
            call(['start',db])
            until=time.monotonic()+30
            while call(['exec',db,'pg_isready','-h','127.0.0.1','-U','postgres'],check=False).returncode:
                if time.monotonic()>until:raise ValueError('Disposable journal database not ready')
                time.sleep(.5)
            runner=call(['create','--name','ai-sdlc-journal-runner-'+nonce,'--label',label+nonce,
                         '--network','container:'+node_id,'--security-opt','label=disable',*runner_identity(provider),
                         '--env','AI_SDLC_TEST_DATABASE_URL','--env','AI_SDLC_JOURNAL_BUILD_FIXTURE',
                         '-v',str(binary.resolve())+':/journal-test:ro','-v',str(private)+':/fixture/private.json:ro',
                         '-v',str(evidence)+':/evidence',lock['go'],'/journal-test','-test.run','^TestRealJournalBackedClosedBuild$','-test.v']).stdout.strip()
            report['runnerContainerID']=runner;save()
            result=call(['start','--attach',runner],check=False,limit=1100)
            (output/'journal-execute.log').write_text(result.stdout+result.stderr)
            if result.returncode:raise ValueError('Journal-backed workload fixture failed; see sanitized test log')
            validate_execution(result.stdout,json.loads((evidence/'transport-report.json').read_text()))
            report['executionPassed']=True;save()
        finally:
            deadline=supply_chain_install.Deadline(120)
            def cleanup(checkpoint):
                copy_errors=[]
                # Copy only explicit public evidence; never private API/DB inputs.
                for name in PUBLIC_FILES:
                    path=evidence/name
                    if path.is_file():
                        try:copy_back(path,name)
                        except BaseException as exc:copy_errors.append(exc)
                containers=call(['ps','-a','--filter','label='+label+nonce,'--format','{{.ID}}']).stdout.splitlines()
                for identifier in reversed(containers):
                    observed=json.loads(call(['inspect',identifier]).stdout)[0]
                    if observed['Config']['Labels'].get(label[:-1])!=nonce or not re.fullmatch(r'[a-f0-9]{64}',observed['Id']):
                        raise ValueError('Refusing unrelated fixture database cleanup')
                    call(['rm','-f','-v',observed['Id']])
                if call(['ps','-a','--filter','label='+label+nonce,'--format','{{.ID}}']).stdout.strip():
                    raise ValueError('Fixture database resources remain')
                report['cleanupComplete']=True
                checkpoint()
                if copy_errors:
                    report['evidenceCopyFailure']=type(copy_errors[0]).__name__
                    raise copy_errors[0]
            fixture_cleanup.finish_fixture(report,save,cleanup)
